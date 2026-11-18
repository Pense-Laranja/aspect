package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pense-Laranja/aspect/internal/gate"
)

func newPrepareEngine(t *testing.T, repo string) *Engine {
	t.Helper()
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("ASPECT_BIN", "")
	t.Setenv("ASPECT_HOME", "")
	t.Setenv("ASPECT_ALLOW_REPO_EXEC", "")
	t.Setenv("HOME", t.TempDir())
	clock := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	return &Engine{
		Config: Config{DefaultRepo: repo, PromptBudget: 800_000, SpecBudget: 30_000},
		Client: nil, // Prepare and Finalize must never touch Client
		FindPR: func(_ context.Context, _, pr string) (PullRequest, bool) {
			if pr != "" {
				return PullRequest{Number: pr, URL: "https://github.com/example/repo/pull/" + pr, Title: "PR " + pr}, true
			}
			return PullRequest{}, false
		},
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			clock = clock.Add(time.Second)
			return clock
		},
	}
}

func writeTaskOutputs(t *testing.T, prep *PreparedRun, outputs map[string]string) {
	t.Helper()
	for _, task := range prep.Tasks {
		ans, ok := outputs[task.TaskID]
		if !ok {
			ans, ok = outputs[task.Agent]
		}
		if !ok {
			t.Fatalf("no test output scripted for task %q (agent %q)", task.TaskID, task.Agent)
		}
		writeFile(t, filepath.Join(prep.RunDir, filepath.FromSlash(task.OutputFile)), ans)
	}
}

func TestPrepareAndFinalize_FreshPassAndBlockWithoutAPIKey(t *testing.T) {
	t.Run("fresh pass without Gemini API key or client", func(t *testing.T) {
		repo := testRepo(t)
		eng := newPrepareEngine(t, repo)

		var startedID string
		prep, err := eng.Prepare(context.Background(), Options{
			Repo:    repo,
			Base:    "main",
			PR:      "77",
			Started: func(id string) { startedID = id },
		})
		if err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		if prep.Status != "pending" {
			t.Fatalf("Prepare().Status = %q, want pending", prep.Status)
		}
		if startedID != prep.RunID {
			t.Errorf("Started callback got %q, want %q", startedID, prep.RunID)
		}
		if prep.Engine != DefaultAntigravityEngine {
			t.Errorf("Prepare().Engine = %q, want %q", prep.Engine, DefaultAntigravityEngine)
		}
		if len(prep.Tasks) != 2 {
			t.Fatalf("len(Tasks) = %d, want 2", len(prep.Tasks))
		}

		// Verify prompt payload contents and L<n>: annotated diff.
		for _, task := range prep.Tasks {
			b, err := os.ReadFile(filepath.Join(prep.RunDir, filepath.FromSlash(task.PromptFile)))
			if err != nil {
				t.Fatalf("reading prompt file %s: %v", task.PromptFile, err)
			}
			var payload PromptPayload
			if err := json.Unmarshal(b, &payload); err != nil {
				t.Fatalf("decoding prompt file %s: %v", task.PromptFile, err)
			}
			if payload.SystemPrompt != sharedSystemPrompt {
				t.Errorf("task %s SystemPrompt mismatch", task.TaskID)
			}
			if !strings.Contains(payload.UserPrompt, "L3: +func A() {}") {
				t.Errorf("task %s UserPrompt missing L3 annotated diff:\n%s", task.TaskID, payload.UserPrompt)
			}
			if !strings.Contains(payload.UserPrompt, "### Pull request\nTitle: PR 77") {
				t.Errorf("task %s UserPrompt missing PR context:\n%s", task.TaskID, payload.UserPrompt)
			}
		}

		// Verify running status.json omits fabricated Gemini token/cost fields.
		runningStatus := readStatus(t, prep.RunDir)
		if runningStatus["state"] != "running" || runningStatus["engine"] != DefaultAntigravityEngine {
			t.Errorf("running status = %+v", runningStatus)
		}
		for _, forbidden := range []string{"total_cost_usd", "total_tokens", "usage"} {
			if _, has := runningStatus[forbidden]; has {
				t.Errorf("running status.json should omit %q for antigravity engine, got %v", forbidden, runningStatus[forbidden])
			}
		}

		writeTaskOutputs(t, prep, map[string]string{
			"security-reviewer": `{"summary": "Security checked.", "findings": [], "handoffs": []}`,
			"red-team":          `{"summary": "Red-team checked.", "findings": [], "handoffs": []}`,
		})

		res, err := eng.Finalize(context.Background(), FinalizeOptions{Repo: repo})
		if err != nil {
			t.Fatalf("Finalize() error = %v", err)
		}
		if res.Outcome != OutcomePass {
			t.Fatalf("Finalize().Outcome = %q, want PASS", res.Outcome)
		}
		if res.RunID != prep.RunID || res.RunDir != prep.RunDir {
			t.Errorf("Finalize() run = (%q, %q), want (%q, %q)", res.RunID, res.RunDir, prep.RunID, prep.RunDir)
		}
		if res.Usage.Engine != DefaultAntigravityEngine || res.Usage.CostUSD != 0 || res.Usage.TotalTokens != 0 {
			t.Errorf("Finalize().Usage = %+v", res.Usage)
		}
		if strings.Contains(res.Rendered, "Cost: $") {
			t.Errorf("Rendered markdown must not fabricate Cost: $0.0000:\n%s", res.Rendered)
		}
		if !exists(filepath.Join(prep.RunDir, "rendered.md")) {
			t.Errorf("rendered.md was not written")
		}
		if matches, _ := filepath.Glob(filepath.Join(prep.RunDir, "*.usage.json")); len(matches) != 0 {
			t.Errorf("unexpected *.usage.json sidecars for antigravity run: %v", matches)
		}

		doneStatus := readStatus(t, prep.RunDir)
		if doneStatus["state"] != "completed" || doneStatus["outcome"] != OutcomePass || doneStatus["engine"] != DefaultAntigravityEngine {
			t.Errorf("completed status = %+v", doneStatus)
		}
		for _, forbidden := range []string{"total_cost_usd", "total_tokens", "usage"} {
			if _, has := doneStatus[forbidden]; has {
				t.Errorf("completed status.json should omit %q for antigravity engine, got %v", forbidden, doneStatus[forbidden])
			}
		}

		// Verify read commands work seamlessly on finalized Antigravity runs.
		summary, err := LoadRun(repo, "")
		if err != nil {
			t.Fatalf("LoadRun: %v", err)
		}
		var buf bytes.Buffer
		PrintStatus(&buf, summary)
		if !strings.Contains(buf.String(), DefaultAntigravityEngine) {
			t.Errorf("PrintStatus missing engine:\n%s", buf.String())
		}
	})

	t.Run("blocking finding produces BLOCK outcome", func(t *testing.T) {
		repo := testRepo(t)
		eng := newPrepareEngine(t, repo)

		prep, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main"})
		if err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		writeTaskOutputs(t, prep, map[string]string{
			"security-reviewer": `{
				"summary": "Found SQL injection.",
				"findings": [{
					"id": "sec-1",
					"severity": "high",
					"category": "security",
					"title": "Unsanitized query input",
					"location": {"file": "a.go", "line": 3},
					"evidence": "func A()",
					"recommendation": "(1) Code Fix: parameterize at a.go:3. (2) Contract/Stamp/Lock Check: none. (3) Regression Test Anchor: TestA_SQLi.",
					"confidence": 0.95
				}],
				"handoffs": []
			}`,
			"red-team": clean,
		})

		res, err := eng.Finalize(context.Background(), FinalizeOptions{Run: prep.RunDir})
		if err != nil {
			t.Fatalf("Finalize() error = %v", err)
		}
		if res.Outcome != OutcomeBlock {
			t.Fatalf("Outcome = %q, want BLOCK", res.Outcome)
		}
		summary, err := LoadRun(repo, prep.RunID)
		if err != nil {
			t.Fatalf("LoadRun: %v", err)
		}
		if summary.TotalBlockers != 1 {
			t.Errorf("TotalBlockers = %d, want 1", summary.TotalBlockers)
		}
	})
}

func TestPrepare_FullRunReuseAndBaseMergeCarrySkipManifestAndSubdirs(t *testing.T) {
	repo := testRepo(t)
	eng := newPrepareEngine(t, repo)

	prep1, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	writeTaskOutputs(t, prep1, map[string]string{
		"security-reviewer": clean,
		"red-team":          clean,
	})
	res1, err := eng.Finalize(context.Background(), FinalizeOptions{Repo: repo, Run: prep1.RunID})
	if err != nil || res1.Outcome != OutcomePass {
		t.Fatalf("Finalize 1 = %+v, err = %v", res1, err)
	}

	// 1. Unchanged diff -> reused with non-nil empty Tasks ([] in JSON).
	prepReused, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if prepReused.Status != "reused" || prepReused.RunID != res1.RunID || prepReused.Result == nil || !prepReused.Result.Cached {
		t.Fatalf("prepReused = %+v", prepReused)
	}
	if prepReused.Result.Usage.Engine != DefaultAntigravityEngine {
		t.Errorf("prepReused.Result.Usage.Engine = %q, want %q", prepReused.Result.Usage.Engine, DefaultAntigravityEngine)
	}
	if prepReused.Tasks == nil || len(prepReused.Tasks) != 0 {
		t.Fatalf("Tasks = %#v, want non-nil empty slice", prepReused.Tasks)
	}
	jb, err := json.Marshal(prepReused)
	if err != nil || !strings.Contains(string(jb), `"tasks":[]`) {
		t.Errorf("marshaled PreparedRun should contain \"tasks\":[], got %s", jb)
	}

	// 2. Clean merge of main -> carried to new head without copying manifest.json, prompts/, or raw/.
	mergeMain(t, repo, "other.txt", "unrelated\n")
	prepCarried, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if prepCarried.Status != "reused" || prepCarried.CarriedFrom != res1.RunID || prepCarried.RunID == res1.RunID {
		t.Fatalf("prepCarried = %+v, want carried from %s", prepCarried, res1.RunID)
	}
	for _, skipped := range []string{"manifest.json", "prompts", "raw"} {
		if exists(filepath.Join(prepCarried.RunDir, skipped)) {
			t.Errorf("carried run directory %s unexpectedly copied %s", prepCarried.RunDir, skipped)
		}
	}
	carriedStatus := readStatus(t, prepCarried.RunDir)
	for _, forbidden := range []string{"total_cost_usd", "total_tokens", "usage"} {
		if _, has := carriedStatus[forbidden]; has {
			t.Errorf("carried antigravity status should not fabricate %q: %+v", forbidden, carriedStatus)
		}
	}
	if agentsMap, ok := carriedStatus["agents"].(map[string]any); ok {
		for agentName, rawAgent := range agentsMap {
			am, _ := rawAgent.(map[string]any)
			for _, forbidden := range []string{"cost_usd", "tokens"} {
				if _, has := am[forbidden]; has {
					t.Errorf("carried antigravity agent %q should not fabricate %q: %+v", agentName, forbidden, am)
				}
			}
		}
	}
}

func TestPrepare_SelectiveCarryTier1Tier2AndCrossEngineInteroperability(t *testing.T) {
	repo := testRepo(t)
	gatesYAML := `gates:
  - agent: security-reviewer
    paths: ["auth/**"]
    mode: gate
  - agent: quality-reviewer
    paths: ["pkg/**"]
    mode: gate
  - agent: red-team
    paths: ["**"]
    mode: gate
`
	writeFile(t, filepath.Join(repo, ".claude/review/gates.yaml"), gatesYAML)
	writeFile(t, filepath.Join(repo, ".claude/agents/quality-reviewer.md"), "---\nname: quality-reviewer\n---\nquality-reviewer\n")
	writeFile(t, filepath.Join(repo, "auth/login.go"), "package auth\nfunc Login() {}\n")
	writeFile(t, filepath.Join(repo, "pkg/svc.go"), "package pkg\nfunc Svc() {}\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "scoped files")

	eng := newPrepareEngine(t, repo)

	// Run 1 (Antigravity Prepare + Finalize): quality-reviewer emits a medium warning; security-reviewer & red-team pass cleanly.
	prep1, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if len(prep1.Tasks) != 3 {
		t.Fatalf("len(prep1.Tasks) = %d, want 3", len(prep1.Tasks))
	}
	writeTaskOutputs(t, prep1, map[string]string{
		"security-reviewer": clean,
		"red-team":          clean,
		"quality-reviewer": `{
			"summary": "minor warning",
			"findings": [{
				"id": "q-1",
				"severity": "medium",
				"category": "correctness",
				"title": "Add doc comment",
				"location": {"file": "pkg/svc.go", "line": 2},
				"evidence": "func Svc()",
				"recommendation": "Document Svc",
				"confidence": 0.8
			}],
			"handoffs": []
		}`,
	})
	res1, err := eng.Finalize(context.Background(), FinalizeOptions{Repo: repo, Run: prep1.RunID})
	if err != nil || res1.Outcome != OutcomeWarn {
		t.Fatalf("res1 = %+v, err = %v", res1, err)
	}

	// Commit 2 modifies ONLY pkg/svc.go to address the warning:
	// - security-reviewer (auth/**): carried via Tier 1 / Tier 2a!
	// - red-team (**): passed cleanly on run 1 while run 1 had warnings, no new file added -> carried via Tier 2b!
	// - quality-reviewer (pkg/**): had a warning and pkg/svc.go changed -> MUST re-run!
	writeFile(t, filepath.Join(repo, "pkg/svc.go"), "package pkg\n// Svc runs service.\nfunc Svc() {}\n")
	gitIn(t, repo, "commit", "-q", "-am", "fix warning in pkg/svc.go")

	prep2, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if prep2.Status != "pending" || len(prep2.Tasks) != 1 || prep2.Tasks[0].Agent != "quality-reviewer" {
		t.Fatalf("prep2 tasks = %+v, want only quality-reviewer pending", prep2.Tasks)
	}
	writeTaskOutputs(t, prep2, map[string]string{"quality-reviewer": clean})
	res2, err := eng.Finalize(context.Background(), FinalizeOptions{Repo: repo, Run: prep2.RunID})
	if err != nil || res2.Outcome != OutcomePass {
		t.Fatalf("res2 = %+v, err = %v", res2, err)
	}
	if !strings.Contains(res2.Rendered, "Carried from earlier review") {
		t.Errorf("res2.Rendered missing carried note:\n%s", res2.Rendered)
	}

	// Commit 3 modifies auth/login.go and runs Engine.Run (Gemini fake client) on top of Antigravity res2:
	// quality-reviewer (pkg/**) must carry from Antigravity into Gemini Engine.Run!
	writeFile(t, filepath.Join(repo, "auth/login.go"), "package auth\nfunc Login() bool { return true }\n")
	gitIn(t, repo, "commit", "-q", "-am", "update auth")

	te := newTestEngine(t, repo, map[string]any{
		"security-reviewer": clean,
		"red-team":          clean,
	})
	res3 := te.run(t, Options{})
	if res3.Outcome != OutcomePass {
		t.Fatalf("Engine.Run after Antigravity run = %+v", res3)
	}
	if slices.Contains(te.model.calls, "quality-reviewer") {
		t.Errorf("Engine.Run re-ran quality-reviewer instead of carrying from Antigravity run: calls = %v", te.model.calls)
	}

	// Commit 4 modifies pkg/svc.go again and runs Prepare on top of Gemini res3:
	// security-reviewer (auth/**) must carry from Gemini into Antigravity Prepare!
	writeFile(t, filepath.Join(repo, "pkg/svc.go"), "package pkg\n// Svc v2.\nfunc Svc() int { return 1 }\n")
	gitIn(t, repo, "commit", "-q", "-am", "update pkg again")

	prep4, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range prep4.Tasks {
		if task.Agent == "security-reviewer" {
			t.Errorf("Prepare after Gemini run should have carried security-reviewer, got tasks %+v", prep4.Tasks)
		}
	}
}

func TestPrepare_ZeroPendingTasksCompletesImmediatelyAndFinalizeIsIdempotent(t *testing.T) {
	t.Run("no applicable agents completes immediately with PASS", func(t *testing.T) {
		repo := testRepo(t)
		writeFile(t, filepath.Join(repo, ".claude/review/gates.yaml"), "gates:\n  - agent: red-team\n    paths: [\"docs/**\"]\n    mode: gate\n")
		eng := newPrepareEngine(t, repo)

		prep, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main"})
		if err != nil {
			t.Fatal(err)
		}
		if prep.Status != "completed" || len(prep.Tasks) != 0 || prep.Result == nil {
			t.Fatalf("prep = %+v", prep)
		}
		if prep.Result.Outcome != OutcomePass || prep.Result.Rendered != noAgents {
			t.Fatalf("Result = %+v", prep.Result)
		}
		if !exists(filepath.Join(prep.RunDir, "rendered.md")) {
			t.Errorf("rendered.md missing for zero-agent completed run")
		}
	})

	t.Run("missing gate definition fails closed while missing advisory definition skips", func(t *testing.T) {
		repo := testRepo(t)
		writeFile(t, filepath.Join(repo, ".claude/review/gates.yaml"), `gates:
  - agent: missing-gate
    paths: ["**"]
    mode: gate
  - agent: missing-advisory
    paths: ["**"]
    mode: advisory
`)
		eng := newPrepareEngine(t, repo)

		prep, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main"})
		if err != nil {
			t.Fatal(err)
		}
		if prep.Status != "completed" || len(prep.Tasks) != 0 || prep.Result == nil {
			t.Fatalf("prep = %+v", prep)
		}
		if prep.Result.Outcome != OutcomeBlock {
			t.Fatalf("missing gate definition must BLOCK, got %q", prep.Result.Outcome)
		}
		if !exists(filepath.Join(prep.RunDir, "missing-gate.error.txt")) {
			t.Errorf("missing-gate.error.txt not written")
		}
		if !exists(filepath.Join(prep.RunDir, "missing-advisory.json")) {
			t.Errorf("missing-advisory.json skip report not written")
		}
	})

	t.Run("repeated Finalize with Publish and known PR publishes only once", func(t *testing.T) {
		repo := testRepo(t)
		pubCalls := filepath.Join(t.TempDir(), "publish_calls.txt")
		writeFile(t, filepath.Join(repo, ".claude/review/publish.sh"),
			"#!/bin/sh\necho \"$*\" >> "+pubCalls+"\necho \"published $*\"\n")
		eng := newPrepareEngine(t, repo)

		prep, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main", PR: "42"})
		if err != nil {
			t.Fatal(err)
		}
		writeTaskOutputs(t, prep, map[string]string{
			"security-reviewer": clean,
			"red-team":          clean,
		})

		resA, err := eng.Finalize(context.Background(), FinalizeOptions{Repo: repo, Run: prep.RunID, PR: "42", Publish: true})
		if err != nil || resA.Outcome != OutcomePass || !strings.Contains(resA.Published, "--pr 42") {
			t.Fatalf("first Finalize = %+v, err = %v", resA, err)
		}
		resB, err := eng.Finalize(context.Background(), FinalizeOptions{Repo: repo, Run: prep.RunID, PR: "42", Publish: true})
		if err != nil || resB.Outcome != OutcomePass || resB.Published != resA.Published {
			t.Fatalf("second Finalize = %+v, err = %v", resB, err)
		}
		b, err := os.ReadFile(pubCalls)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(lines) != 1 {
			t.Errorf("publish.sh ran %d times on idempotent Finalize, want 1:\n%s", len(lines), b)
		}
	})
}

func TestPrepareAndFinalize_MultiPartSplitAndAllPartFailureModesFailClosed(t *testing.T) {
	setupMultiPartRepo := func(t *testing.T) (string, *Engine, *PreparedRun) {
		t.Helper()
		repo := testRepo(t)
		writeFile(t, filepath.Join(repo, ".claude/review/gates.yaml"), "gates:\n  - agent: security-reviewer\n    paths: [\"**\"]\n    mode: gate\n")
		for i := 0; i < 8; i++ {
			var sb strings.Builder
			sb.WriteString("package a\n")
			for j := 0; j < 80; j++ {
				fmt.Fprintf(&sb, "func F_%d_%d() int { return %d }\n", i, j, i+j)
			}
			writeFile(t, filepath.Join(repo, fmt.Sprintf("f%d.go", i)), sb.String())
		}
		gitIn(t, repo, "add", "-A")
		gitIn(t, repo, "commit", "-q", "-m", "large multi-file change")

		eng := newPrepareEngine(t, repo)
		eng.Config.PromptBudget = 2500
		prep, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main", Force: true})
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if len(prep.Tasks) < 2 {
			t.Fatalf("expected >= 2 parts under tight PromptBudget, got %d", len(prep.Tasks))
		}
		return repo, eng, prep
	}

	t.Run("multi-part merge suffixes duplicate finding IDs and passes gate validation", func(t *testing.T) {
		repo, eng, prep := setupMultiPartRepo(t)
		for _, task := range prep.Tasks {
			partJSON := fmt.Sprintf(`{
				"summary": "Checked part %d.",
				"findings": [{
					"id": "sec-dup",
					"severity": "low",
					"category": "security",
					"title": "Observation in part %d",
					"location": {"file": "f0.go", "line": 2},
					"evidence": "func F_0_0",
					"recommendation": "Keep an eye on it",
					"confidence": 0.7
				}],
				"handoffs": []
			}`, task.Part, task.Part)
			writeFile(t, filepath.Join(prep.RunDir, filepath.FromSlash(task.OutputFile)), partJSON)
		}

		res, err := eng.Finalize(context.Background(), FinalizeOptions{Repo: repo, Run: prep.RunID})
		if err != nil {
			t.Fatalf("Finalize: %v", err)
		}
		if res.Outcome != OutcomeWarn {
			t.Fatalf("Outcome = %q, want WARN", res.Outcome)
		}
		summary, err := LoadRun(repo, prep.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if summary.TotalFindings != len(prep.Tasks) {
			t.Errorf("TotalFindings = %d, want %d", summary.TotalFindings, len(prep.Tasks))
		}
		if summary.Agents["security-reviewer"].PartsDone != len(prep.Tasks) {
			t.Errorf("PartsDone = %d, want %d", summary.Agents["security-reviewer"].PartsDone, len(prep.Tasks))
		}
	})

	failureCases := []struct {
		name  string
		apply func(t *testing.T, prep *PreparedRun)
	}{
		{
			name: "missing part 2 output file",
			apply: func(t *testing.T, prep *PreparedRun) {
				// Leave part 2 output absent; write all other parts clean.
				for _, task := range prep.Tasks {
					if task.Part == 2 {
						continue
					}
					writeFile(t, filepath.Join(prep.RunDir, filepath.FromSlash(task.OutputFile)), clean)
				}
			},
		},
		{
			name: "non-empty part 2 error file",
			apply: func(t *testing.T, prep *PreparedRun) {
				for _, task := range prep.Tasks {
					writeFile(t, filepath.Join(prep.RunDir, filepath.FromSlash(task.OutputFile)), clean)
					if task.Part == 2 {
						writeFile(t, filepath.Join(prep.RunDir, filepath.FromSlash(task.ErrorFile)), "flash quota exceeded\n")
					}
				}
			},
		},
		{
			name: "malformed JSON in part 2",
			apply: func(t *testing.T, prep *PreparedRun) {
				for _, task := range prep.Tasks {
					out := clean
					if task.Part == 2 {
						out = "{broken json"
					}
					writeFile(t, filepath.Join(prep.RunDir, filepath.FromSlash(task.OutputFile)), out)
				}
			},
		},
		{
			name: "unknown severity in part 2",
			apply: func(t *testing.T, prep *PreparedRun) {
				for _, task := range prep.Tasks {
					out := clean
					if task.Part == 2 {
						out = `{"summary":"x","findings":[{"id":"1","severity":"apocalyptic","category":"security","title":"t","location":{"file":"f0.go","line":2},"evidence":"e","recommendation":"r","confidence":0.9}],"handoffs":[]}`
					}
					writeFile(t, filepath.Join(prep.RunDir, filepath.FromSlash(task.OutputFile)), out)
				}
			},
		},
		{
			name: "non-info finding missing file location in part 2",
			apply: func(t *testing.T, prep *PreparedRun) {
				for _, task := range prep.Tasks {
					out := clean
					if task.Part == 2 {
						out = `{"summary":"x","findings":[{"id":"1","severity":"high","category":"security","title":"t","location":{"file":""},"evidence":"e","recommendation":"r","confidence":0.9}],"handoffs":[]}`
					}
					writeFile(t, filepath.Join(prep.RunDir, filepath.FromSlash(task.OutputFile)), out)
				}
			},
		},
	}

	for _, tc := range failureCases {
		t.Run(tc.name, func(t *testing.T) {
			repo, eng, prep := setupMultiPartRepo(t)
			tc.apply(t, prep)

			res, err := eng.Finalize(context.Background(), FinalizeOptions{Repo: repo, Run: prep.RunID})
			if err != nil {
				t.Fatalf("Finalize returned unexpected error: %v", err)
			}
			if res.Outcome != OutcomeBlock {
				t.Errorf("Outcome = %q, want BLOCK (partial/failed gate must fail closed)", res.Outcome)
			}
			if exists(filepath.Join(prep.RunDir, "security-reviewer.json")) {
				t.Errorf("security-reviewer.json must not exist when any part fails")
			}
			if !exists(filepath.Join(prep.RunDir, "security-reviewer.error.txt")) {
				t.Errorf("security-reviewer.error.txt must be written when a part fails")
			}
			if state := agentState(t, prep.RunDir, "security-reviewer"); state != stateFailed {
				t.Errorf("agent state = %q, want failed", state)
			}
		})
	}
}

func TestFinalize_AdvisoryOnlyFailureProducesOutcomeError(t *testing.T) {
	repo := testRepo(t)
	writeFile(t, filepath.Join(repo, ".claude/review/gates.yaml"), "gates:\n  - agent: architect\n    paths: [\"**\"]\n    mode: advisory\n")
	writeFile(t, filepath.Join(repo, ".claude/agents/architect.md"), "---\nname: architect\n---\narchitect\n")
	eng := newPrepareEngine(t, repo)

	prep, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(prep.RunDir, filepath.FromSlash(prep.Tasks[0].ErrorFile)), "subagent timeout\n")

	res, err := eng.Finalize(context.Background(), FinalizeOptions{Repo: repo, Run: prep.RunID})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if res.Outcome != OutcomeError {
		t.Fatalf("Outcome = %q, want ERROR when only advisory agents were planned and none completed", res.Outcome)
	}
}

func TestPrepareAndFinalize_SingleAgentIsolation(t *testing.T) {
	repo := testRepo(t)
	pubCalls := filepath.Join(t.TempDir(), "pub.txt")
	writeFile(t, filepath.Join(repo, ".claude/review/publish.sh"), "#!/bin/sh\necho \"$*\" >> "+pubCalls+"\n")
	eng := newPrepareEngine(t, repo)

	// Unknown agent is rejected with planned list.
	if _, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main", Agent: "architect"}); err == nil ||
		!strings.Contains(err.Error(), `"architect" is not in this change's gate plan`) {
		t.Fatalf("expected unknown agent error, got %v", err)
	}

	prep, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main", Agent: "red-team", Publish: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(prep.RunID, agentRunPrefix) {
		t.Fatalf("RunID = %q, want %q prefix", prep.RunID, agentRunPrefix)
	}
	if len(prep.Tasks) != 1 || prep.Tasks[0].Agent != "red-team" {
		t.Fatalf("Tasks = %+v, want red-team alone", prep.Tasks)
	}
	if exists(filepath.Join(repo, ".review", "current_run.json")) {
		t.Errorf("single-agent Prepare must not write current_run.json")
	}

	writeTaskOutputs(t, prep, map[string]string{"red-team": clean})
	res, err := eng.Finalize(context.Background(), FinalizeOptions{Repo: repo, Run: prep.RunID, Publish: true})
	if err != nil || res.Outcome != OutcomePass {
		t.Fatalf("Finalize = %+v, err = %v", res, err)
	}
	if exists(filepath.Join(repo, ".review", "current_run.json")) {
		t.Errorf("single-agent Finalize must not write current_run.json")
	}
	if exists(pubCalls) {
		t.Errorf("single-agent Finalize must never invoke publish.sh")
	}
	if runs, err := LoadRuns(repo, 0); err != nil || len(runs) != 0 {
		t.Errorf("LoadRuns = %d runs, want 0", len(runs))
	}

	// Subsequent full Prepare must NOT reuse the single-agent run.
	fullPrep, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if fullPrep.Status != "pending" || len(fullPrep.Tasks) != 2 {
		t.Errorf("full Prepare reused single-agent run: %+v", fullPrep)
	}
}

func TestReviewerReportJSONSchema_ClosedInlineContract(t *testing.T) {
	schema := reviewerReportJSONSchema()
	forbidden := map[string]bool{
		"$ref": true, "$defs": true, "definitions": true,
		"if": true, "then": true, "else": true,
		"allOf": true, "anyOf": true, "oneOf": true,
	}

	var walk func(path string, node any)
	walk = func(path string, node any) {
		m, ok := node.(map[string]any)
		if !ok {
			return
		}
		for k, v := range m {
			if forbidden[k] {
				t.Errorf("%s uses forbidden JSON Schema keyword %q", path, k)
			}
			walk(path+"."+k, v)
		}
		if m["type"] == "object" {
			if ap, ok := m["additionalProperties"].(bool); !ok || ap {
				t.Errorf("%s: object schema must set additionalProperties: false, got %v", path, m["additionalProperties"])
			}
			if props, ok := m["properties"].(map[string]any); !ok || len(props) == 0 {
				t.Errorf("%s: object schema must have non-empty properties", path)
			}
			if req, ok := m["required"].([]string); !ok || len(req) == 0 {
				t.Errorf("%s: object schema must declare required keys", path)
			}
		}
	}
	walk("root", schema)

	props := schema["properties"].(map[string]any)
	findingProps := props["findings"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	if got := findingProps["severity"].(map[string]any)["enum"].([]string); !slices.Equal(got, gate.Severities) {
		t.Errorf("severity enum = %v, want %v", got, gate.Severities)
	}
	if got := findingProps["category"].(map[string]any)["enum"].([]string); !slices.Equal(got, gate.Categories) {
		t.Errorf("category enum = %v, want %v", got, gate.Categories)
	}

	// Verify info findings with empty location.file pass Normalize while non-info findings fail closed.
	run := AgentRun{Agent: "security-reviewer", Mode: "gate", Base: "1111111", Head: "2222222"}
	infoObj, err := extractJSON(`{"summary":"note","findings":[{"id":"i1","severity":"info","category":"security","title":"Note","location":{"file":""},"evidence":"e","recommendation":"r","confidence":0.5}],"handoffs":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if rep, err := Normalize(infoObj, run); err != nil || len(rep.Findings) != 1 || rep.Findings[0].Location != nil {
		t.Errorf("Normalize(info with empty file) = %+v, err = %v", rep, err)
	}

	highObj, err := extractJSON(`{"summary":"bug","findings":[{"id":"h1","severity":"high","category":"security","title":"Bug","location":{"file":""},"evidence":"e","recommendation":"r","confidence":0.9}],"handoffs":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Normalize(highObj, run); !errors.Is(err, ErrInvalidReport) {
		t.Errorf("Normalize(high with empty file) err = %v, want ErrInvalidReport", err)
	}
}

func TestPythonReviewWorkflowUnitSuite(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found on PATH")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ASPECT_BIN", "")
	t.Setenv("ASPECT_HOME", "")
	t.Setenv("ASPECT_ALLOW_REPO_EXEC", "")
	scriptsDir, err := filepath.Abs(filepath.Join("..", "..", ".agents", "skills", "aspect-review", "scripts"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(py, "-B", "-m", "unittest", "discover", "-s", scriptsDir, "-p", "test_*.py")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python unittest suite failed: %v\n%s", err, out)
	}
}

func TestFinalize_MixedManifestStatusesAndMalformedReportGuards(t *testing.T) {
	t.Run("mixed pending, skipped fallback, empty, and missing_def statuses plus Case 3 Repo+Run", func(t *testing.T) {
		repo := testRepo(t)
		writeFile(t, filepath.Join(repo, ".claude/review/gates.yaml"), `gates:
  - agent: security-reviewer
    paths: ["a.go"]
    mode: gate
  - agent: skipped-advisory
    paths: ["a.go"]
    mode: advisory
  - agent: missing-gate
    paths: ["a.go"]
    mode: gate
`)
		eng := newPrepareEngine(t, repo)
		prep, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main", EngineLabel: "antigravity (custom-flash)"})
		if err != nil {
			t.Fatal(err)
		}
		if prep.Engine != "antigravity (custom-flash)" {
			t.Errorf("prep.Engine = %q", prep.Engine)
		}
		if len(prep.Tasks) != 1 || prep.Tasks[0].Agent != "security-reviewer" {
			t.Fatalf("prep.Tasks = %+v", prep.Tasks)
		}

		// Delete skipped-advisory.json and leave a stale skipped-advisory.error.txt so Finalize exercises fallback rewrite + stale .error.txt removal.
		_ = os.Remove(filepath.Join(prep.RunDir, "skipped-advisory.json"))
		writeFile(t, filepath.Join(prep.RunDir, "skipped-advisory.error.txt"), "stale error\n")
		writeTaskOutputs(t, prep, map[string]string{"security-reviewer": clean})

		// Exercise Case 3 with BOTH Repo and Run directory path provided.
		res, err := eng.Finalize(context.Background(), FinalizeOptions{Repo: repo, Run: prep.RunDir})
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutcomeBlock {
			t.Fatalf("Outcome = %q, want BLOCK (missing-gate failed)", res.Outcome)
		}
		if res.Usage.Engine != "antigravity (custom-flash)" {
			t.Errorf("Usage.Engine = %q, want antigravity (custom-flash)", res.Usage.Engine)
		}
		if !exists(filepath.Join(prep.RunDir, "skipped-advisory.json")) {
			t.Errorf("skipped-advisory.json fallback was not recreated")
		}
		if exists(filepath.Join(prep.RunDir, "skipped-advisory.error.txt")) {
			t.Errorf("stale skipped-advisory.error.txt was not removed")
		}
	})

	t.Run("empty object, non-array findings, and directory output_file fail closed with accurate errors", func(t *testing.T) {
		cases := []struct {
			name    string
			setup   func(t *testing.T, prep *PreparedRun)
			wantSub string
		}{
			{
				name: "empty JSON object missing summary and findings",
				setup: func(t *testing.T, prep *PreparedRun) {
					writeTaskOutputs(t, prep, map[string]string{"security-reviewer": `{}`, "red-team": clean})
				},
				wantSub: "missing summary and findings",
			},
			{
				name: "findings is not an array",
				setup: func(t *testing.T, prep *PreparedRun) {
					writeTaskOutputs(t, prep, map[string]string{"security-reviewer": `{"summary":"s","findings":"none"}`, "red-team": clean})
				},
				wantSub: "findings must be an array",
			},
			{
				name: "output_file is a directory (non-ErrNotExist read error)",
				setup: func(t *testing.T, prep *PreparedRun) {
					writeTaskOutputs(t, prep, map[string]string{"security-reviewer": clean, "red-team": clean})
					outPath := filepath.Join(prep.RunDir, filepath.FromSlash(prep.Tasks[0].OutputFile))
					_ = os.Remove(outPath)
					if err := os.MkdirAll(outPath, 0o755); err != nil {
						t.Fatal(err)
					}
				},
				wantSub: "reading output file",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				repo := testRepo(t)
				eng := newPrepareEngine(t, repo)
				prep, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main"})
				if err != nil {
					t.Fatal(err)
				}
				tc.setup(t, prep)
				res, err := eng.Finalize(context.Background(), FinalizeOptions{Repo: repo, Run: prep.RunID})
				if err != nil {
					t.Fatal(err)
				}
				if res.Outcome != OutcomeBlock {
					t.Fatalf("Outcome = %q, want BLOCK", res.Outcome)
				}
				errBytes, err := os.ReadFile(filepath.Join(prep.RunDir, "security-reviewer.error.txt"))
				if err != nil || !strings.Contains(string(errBytes), tc.wantSub) {
					t.Errorf("error.txt = %q (err %v), want substring %q", errBytes, err, tc.wantSub)
				}
			})
		}
	})
}

func TestPrepare_AllCarriedCompletionAndCancellationMarksStatusFailed(t *testing.T) {
	t.Run("all planned agents carried completes immediately with reused footer", func(t *testing.T) {
		repo := testRepo(t)
		writeFile(t, filepath.Join(repo, ".claude/review/gates.yaml"), `gates:
  - agent: security-reviewer
    paths: ["a.go"]
    mode: gate
`)
		eng := newPrepareEngine(t, repo)
		prep1, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main"})
		if err != nil {
			t.Fatal(err)
		}
		writeTaskOutputs(t, prep1, map[string]string{"security-reviewer": clean})
		if _, err := eng.Finalize(context.Background(), FinalizeOptions{Repo: repo, Run: prep1.RunID}); err != nil {
			t.Fatal(err)
		}

		// Commit touching an unwatched file -> full diffHash changes, so full reuse misses,
		// while security-reviewer (scoped to a.go) carries via Tier 1 -> 0 tasks, immediate completion!
		writeFile(t, filepath.Join(repo, "README.txt"), "hello\n")
		gitIn(t, repo, "add", "README.txt")
		gitIn(t, repo, "commit", "-q", "-m", "unwatched file")

		prep2, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main"})
		if err != nil {
			t.Fatal(err)
		}
		if prep2.Status != "completed" || len(prep2.Tasks) != 0 || prep2.Result == nil || prep2.Result.Outcome != OutcomePass {
			t.Fatalf("prep2 = %+v", prep2)
		}
		if !strings.Contains(prep2.Result.Rendered, "Cost: $0.0000 (reused)") {
			t.Errorf("expected reused footer when all agents carried:\n%s", prep2.Result.Rendered)
		}
	})

	t.Run("cancelled context after initRun marks status.json failed instead of leaving running", func(t *testing.T) {
		repo := testRepo(t)
		eng := newPrepareEngine(t, repo)
		ctx, cancel := context.WithCancel(context.Background())

		var runID string
		_, err := eng.Prepare(ctx, Options{
			Repo: repo,
			Base: "main",
			Started: func(id string) {
				runID = id
				cancel()
			},
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Prepare err = %v, want context.Canceled", err)
		}
		if runID == "" {
			t.Fatal("Started was not invoked before cancellation")
		}
		st := readStatus(t, filepath.Join(repo, ".review", runID))
		if st["state"] != stateFailed {
			t.Errorf("cancelled Prepare left status.json state = %v, want %q", st["state"], stateFailed)
		}
	})
}

func TestPrepare_CapsPromptBytesBelowAntigravityUserInputLimit(t *testing.T) {
	repo := testRepo(t)
	writeFile(t, filepath.Join(repo, ".claude/review/gates.yaml"), "gates:\n  - agent: security-reviewer\n    paths: [\"**\"]\n    mode: gate\n")

	// Commit a valid _aspect/aspect.yaml + large 180 KB module brief so SpecBudget: 80_000 would exceed 192 KB if unclamped.
	specYAML := `aspect: "1"
system:
  name: demo
  intent: Demo system for prompt size bounding.
  language: go
  module_path: example.com/demo
  goals:
    - id: G1
      description: Keep prompts bounded.
      verify: test
modules:
  - name: core
    path: core
    brief: briefs/core.md
    intent: Core logic.
    goals: [G1]
`
	var brief strings.Builder
	brief.WriteString("# Core Module Brief\n\n")
	for i := 0; i < 2400; i++ {
		fmt.Fprintf(&brief, "- Rule %04d: Every state transition in core must validate preconditions and preserve invariants.\n", i)
	}
	writeFile(t, filepath.Join(repo, "_aspect/aspect.yaml"), specYAML)
	writeFile(t, filepath.Join(repo, "_aspect/briefs/core.md"), brief.String())

	// Generate >260 KB of reviewable Go source diff under core/.
	var totalDiffBytes int
	for fIdx := 0; fIdx < 6; fIdx++ {
		var code strings.Builder
		code.WriteString("package core\n\n")
		for lineIdx := 0; lineIdx < 750; lineIdx++ {
			line := fmt.Sprintf("func ComputeValue_%d_%03d(input int) int { return input*%d + %d }\n", fIdx, lineIdx, fIdx+2, lineIdx)
			code.WriteString(line)
			totalDiffBytes += len(line)
		}
		writeFile(t, filepath.Join(repo, "core", fmt.Sprintf("part_%d.go", fIdx)), code.String())
	}
	if totalDiffBytes < 250_000 {
		t.Fatalf("expected >250KB generated code, got %d bytes", totalDiffBytes)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "large diff with large spec brief")

	eng := newPrepareEngine(t, repo)
	eng.Config.SpecBudget = 80_000 // Explicitly exceeds DefaultAntigravityPromptBudget unless clamped by Prepare.

	prep, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main", Force: true})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if len(prep.Tasks) < 2 {
		t.Fatalf("len(prep.Tasks) = %d, want >= 2 parts for >250KB diff", len(prep.Tasks))
	}
	if eng.Config.SpecBudget != 80_000 {
		t.Errorf("Prepare mutated Engine.Config.SpecBudget in place: got %d, want 80000", eng.Config.SpecBudget)
	}

	const maxSafeUserPromptBytes = 185_000 // Strictly below Antigravity's 191,956-byte USER_INPUT truncation limit.
	for _, task := range prep.Tasks {
		raw, err := os.ReadFile(filepath.Join(prep.RunDir, filepath.FromSlash(task.PromptFile)))
		if err != nil {
			t.Fatalf("reading %s: %v", task.PromptFile, err)
		}
		var payload PromptPayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decoding %s: %v", task.PromptFile, err)
		}
		if len(payload.UserPrompt) >= maxSafeUserPromptBytes {
			t.Errorf("task %s UserPrompt length = %d bytes, want < %d bytes", task.TaskID, len(payload.UserPrompt), maxSafeUserPromptBytes)
		}
	}
}
