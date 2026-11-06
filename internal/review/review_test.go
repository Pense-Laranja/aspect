package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pense-Laranja/aspect/internal/gate"
	"github.com/Pense-Laranja/aspect/internal/llm"
	"github.com/Pense-Laranja/aspect/internal/spec"
)

// The pipeline must leave no report for a gate agent that failed, record a
// cancelled run as failed, never report PASS without every gate's report,
// and reuse a finished run of the same diff. A gate agent with no report
// blocks: gate.Check blocks on a missing gate report, which ranks above the
// ERROR the run status alone would give.

const testGates = `gates:
  - agent: security-reviewer
    paths: ["**"]
    mode: gate
  - agent: red-team
    paths: ["**"]
    mode: gate
`

// hang is an answer that never comes, until the request is cancelled.
var hang = errors.New("hang")

// roleOf is the agent's role text in a prompt (its name, in these tests).
func roleOf(prompt string) string {
	_, rest, _ := strings.Cut(prompt, "### Your role\n")
	role, _, _ := strings.Cut(rest, "\n\n###")
	return strings.TrimSpace(role)
}

// fakeModel answers per agent: a string is the reply, an error is returned,
// hang blocks until the context ends.
type fakeModel struct {
	mu      sync.Mutex
	answers map[string]any
	calls   []string
	prompts map[string]string  // the last prompt each agent was sent
	before  func(agent string) // runs as a request starts
}

func (f *fakeModel) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	agent := roleOf(req.Prompt)
	f.mu.Lock()
	f.calls = append(f.calls, agent)
	if f.prompts == nil {
		f.prompts = map[string]string{}
	}
	f.prompts[agent] = req.Prompt
	answer := f.answers[agent]
	before := f.before
	f.mu.Unlock()
	if before != nil {
		before(agent)
	}
	switch a := answer.(type) {
	case string:
		return llm.Response{Text: a, Model: "flash", InputTokens: 100, CacheReadTokens: 80, OutputTokens: 10}, nil
	case llm.Response:
		return a, nil
	case error:
		if a == hang {
			<-ctx.Done()
			return llm.Response{}, ctx.Err()
		}
		return llm.Response{}, a
	}
	return llm.Response{}, errors.New("no answer scripted for " + agent)
}

func (f *fakeModel) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

const clean = `{"summary": "checked", "findings": []}`

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// testRepo is a repository whose main has gates.yaml and the two agents,
// and whose branch "feature" changes a.go.
func testRepo(t *testing.T) string {
	t.Helper()
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
	} {
		t.Setenv(k, v)
	}
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	gitIn(t, repo, "remote", "add", "origin", "https://github.com/example/repo.git")
	writeFile(t, filepath.Join(repo, ".claude/review/gates.yaml"), testGates)
	for _, a := range []string{"security-reviewer", "red-team"} {
		// The prompt body is the agent's name, so fakeModel tells them apart.
		writeFile(t, filepath.Join(repo, ".claude/agents", a+".md"), "---\nname: "+a+"\n---\n"+a+"\n")
	}
	writeFile(t, filepath.Join(repo, ".gitignore"), ".review/\n")
	writeFile(t, filepath.Join(repo, "a.go"), "package a\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "base")
	gitIn(t, repo, "checkout", "-q", "-b", "feature")
	writeFile(t, filepath.Join(repo, "a.go"), "package a\n\nfunc A() {}\n")
	gitIn(t, repo, "commit", "-q", "-am", "head")
	return repo
}

type testEngine struct {
	*Engine
	model *fakeModel
	repo  string
	pr    PullRequest
}

func newTestEngine(t *testing.T, repo string, answers map[string]any) *testEngine {
	te := &testEngine{model: &fakeModel{answers: answers}, repo: repo}
	clock := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	te.Engine = &Engine{
		Config: Config{GeminiModel: "flash", PromptBudget: 800_000, MaxConcurrency: 4},
		Client: te.model,
		FindPR: func(context.Context, string, string) (PullRequest, bool) { return te.pr, te.pr.Number != "" },
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			clock = clock.Add(time.Second)
			return clock
		},
	}
	return te
}

func (te *testEngine) run(t *testing.T, opt Options) *Result {
	t.Helper()
	opt.Repo, opt.Base = te.repo, "main"
	res, err := te.Run(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func readStatus(t *testing.T, dir string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func agentState(t *testing.T, dir, agent string) string {
	t.Helper()
	agents, _ := readStatus(t, dir)["agents"].(map[string]any)
	a, _ := agents[agent].(map[string]any)
	s, _ := a["state"].(string)
	return s
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAllGatesCleanPasses(t *testing.T) {
	te := newTestEngine(t, testRepo(t), map[string]any{"security-reviewer": clean, "red-team": clean})
	res := te.run(t, Options{})
	if res.Outcome != OutcomePass || res.Cached {
		t.Fatalf("outcome = %s, cached = %v", res.Outcome, res.Cached)
	}
	for _, name := range []string{"plan.json", "diff.hash", "status.json", "security-reviewer.json", "red-team.json", "red-team.usage.json"} {
		if !exists(filepath.Join(res.RunDir, name)) {
			t.Errorf("%s missing", name)
		}
	}
	st := readStatus(t, res.RunDir)
	if st["state"] != "completed" || st["outcome"] != "PASS" || st["schema"] != StatusSchema {
		t.Errorf("status = %v %v", st["state"], st["outcome"])
	}
	us, _ := st["usage"].(map[string]any)
	if us["cached_tokens"] != float64(160) || us["prompt_tokens"] != float64(200) {
		t.Errorf("run usage = %+v", us)
	}
	redAgent, _ := st["agents"].(map[string]any)["red-team"].(map[string]any)
	redTok, _ := redAgent["tokens"].(map[string]any)
	if redTok["cached"] != float64(80) || redTok["prompt"] != float64(100) {
		t.Errorf("red-team tokens = %+v", redTok)
	}
	var sideUsage map[string]any
	ub, _ := os.ReadFile(filepath.Join(res.RunDir, "red-team.usage.json"))
	_ = json.Unmarshal(ub, &sideUsage)
	if sideUsage["cached_tokens"] != float64(80) {
		t.Errorf("red-team.usage.json = %+v", sideUsage)
	}
	var cur struct {
		RunID string `json:"run_id"`
	}
	b, _ := os.ReadFile(filepath.Join(te.repo, ".review", "current_run.json"))
	if json.Unmarshal(b, &cur); cur.RunID != res.RunID {
		t.Errorf("current_run.json names %q", cur.RunID)
	}
	if !strings.Contains(res.Rendered, "security-reviewer") {
		t.Errorf("rendered = %q", res.Rendered)
	}
}

func TestBlockingFindingBlocks(t *testing.T) {
	te := newTestEngine(t, testRepo(t), map[string]any{
		"security-reviewer": `{"summary": "found one", "findings": [{"severity": "critical", "category": "security", "title": "SQL injection", "location": {"file": "a.go", "line": 3}, "evidence": "e", "recommendation": "r", "confidence": 0.95}]}`,
		"red-team":          clean,
	})
	if res := te.run(t, Options{}); res.Outcome != OutcomeBlock {
		t.Fatalf("outcome = %s", res.Outcome)
	}
}

func TestFailedGateAgentLeavesNoReportAndTheRunIsError(t *testing.T) {
	te := newTestEngine(t, testRepo(t), map[string]any{
		"security-reviewer": clean,
		"red-team":          errors.New("Gemini flash request timed out"),
	})
	res := te.run(t, Options{})
	if !exists(filepath.Join(res.RunDir, "security-reviewer.json")) || exists(filepath.Join(res.RunDir, "red-team.json")) {
		t.Fatal("a failed gate must leave no report, a clean one must")
	}
	if b, _ := os.ReadFile(filepath.Join(res.RunDir, "red-team.error.txt")); !strings.Contains(string(b), "timed out") {
		t.Errorf("error.txt = %q", b)
	}
	if res.Outcome != OutcomeBlock || agentState(t, res.RunDir, "red-team") != stateFailed {
		t.Errorf("outcome = %s", res.Outcome)
	}
}

type emptyErr struct{}

func (emptyErr) Error() string { return "" }

func TestFailedAgentWithAnEmptyMessageStillRecordsAReason(t *testing.T) {
	te := newTestEngine(t, testRepo(t), map[string]any{"security-reviewer": clean, "red-team": emptyErr{}})
	res := te.run(t, Options{})
	b, _ := os.ReadFile(filepath.Join(res.RunDir, "red-team.error.txt"))
	if !strings.Contains(string(b), "emptyErr") {
		t.Errorf("error.txt = %q", b)
	}
}

func TestGateAgentWithoutDefinitionIsError(t *testing.T) {
	repo := testRepo(t)
	os.Remove(filepath.Join(repo, ".claude/agents/red-team.md"))
	te := newTestEngine(t, repo, map[string]any{"security-reviewer": clean})
	res := te.run(t, Options{})
	if exists(filepath.Join(res.RunDir, "red-team.json")) || res.Outcome != OutcomeBlock {
		t.Fatalf("outcome = %s", res.Outcome)
	}
}

func TestAspectHomeSuppliesAMissingAgent(t *testing.T) {
	repo := testRepo(t)
	home := t.TempDir()
	os.Rename(filepath.Join(repo, ".claude/agents/red-team.md"), filepath.Join(home, "red-team.md"))
	writeFile(t, filepath.Join(home, ".claude/agents/red-team.md"), "red-team\n")
	te := newTestEngine(t, repo, map[string]any{"security-reviewer": clean, "red-team": clean})
	te.Config.AspectHome = home
	if res := te.run(t, Options{}); res.Outcome != OutcomePass {
		t.Fatalf("outcome = %s", res.Outcome)
	}
}

func TestObjectSummaryIsWrittenAsText(t *testing.T) {
	// Run local-20261007T095818Z: aspect rejected these reports as invalid.
	te := newTestEngine(t, testRepo(t), map[string]any{
		"security-reviewer": `{"summary": {"overall_score": 4.2, "comment": "ok"}, "findings": []}`,
		"red-team":          `{"summary": {"total_files_changed": 5}, "findings": []}`,
	})
	res := te.run(t, Options{})
	var rep map[string]any
	b, _ := os.ReadFile(filepath.Join(res.RunDir, "security-reviewer.json"))
	json.Unmarshal(b, &rep)
	if rep["summary"] != "overall_score: 4.2; comment: ok" || res.Outcome != OutcomePass {
		t.Fatalf("summary = %v, outcome = %s", rep["summary"], res.Outcome)
	}
}

func TestBlockerWithoutLocationFailsTheAgentNotTheGate(t *testing.T) {
	te := newTestEngine(t, testRepo(t), map[string]any{
		"security-reviewer": clean,
		"red-team":          `{"summary": "found one", "findings": [{"severity": "critical", "title": "Auth bypass"}]}`,
	})
	res := te.run(t, Options{})
	b, _ := os.ReadFile(filepath.Join(res.RunDir, "red-team.error.txt"))
	if exists(filepath.Join(res.RunDir, "red-team.json")) || !strings.Contains(string(b), "no file location") || res.Outcome != OutcomeBlock {
		t.Fatalf("outcome = %s, error = %q", res.Outcome, b)
	}
}

func TestAgentsAreQueuedUntilAModelSlotIsFree(t *testing.T) {
	te := newTestEngine(t, testRepo(t), map[string]any{"security-reviewer": clean, "red-team": clean})
	te.Config.MaxConcurrency = 1
	var mu sync.Mutex
	seen := map[string]string{}
	te.model.before = func(agent string) {
		other := "red-team"
		if agent == other {
			other = "security-reviewer"
		}
		dirs, _ := filepath.Glob(filepath.Join(te.repo, ".review", "local-*"))
		mu.Lock()
		defer mu.Unlock()
		seen[agent] = agentState(t, dirs[0], agent)
		if _, ok := seen["other"]; !ok {
			seen["other"] = agentState(t, dirs[0], other)
		}
	}
	res := te.run(t, Options{})
	if seen["security-reviewer"] != stateRunning || seen["red-team"] != stateRunning {
		t.Errorf("an agent holding a slot is running: %v", seen)
	}
	if seen["other"] != stateQueued {
		t.Errorf("with one slot taken the other agent is queued, got %q", seen["other"])
	}
	if res.Outcome != OutcomePass {
		t.Errorf("outcome = %s", res.Outcome)
	}
}

func TestPublishingRunCompletesWithItsPR(t *testing.T) {
	repo := testRepo(t)
	writeFile(t, filepath.Join(repo, ".claude/review/publish.sh"), "#!/bin/sh\necho \"published $*\"\n")
	te := newTestEngine(t, repo, map[string]any{"security-reviewer": clean, "red-team": clean})
	te.pr = PullRequest{Number: "1565", URL: "https://example.test/pull/1565"}
	res := te.run(t, Options{Publish: true})
	if res.Outcome != OutcomePass || res.PR != "1565" || !strings.Contains(res.Published, "--pr 1565") {
		t.Fatalf("outcome = %s, pr = %s, published = %q", res.Outcome, res.PR, res.Published)
	}
	if st := readStatus(t, res.RunDir); st["pr"] != "1565" || !strings.HasPrefix(st["published_status"].(string), "published") {
		t.Errorf("status pr = %v, published = %v", st["pr"], st["published_status"])
	}
}

func TestCancelledPipelineIsRecordedFailed(t *testing.T) {
	te := newTestEngine(t, testRepo(t), map[string]any{"security-reviewer": hang, "red-team": hang})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		_, err := te.Run(ctx, Options{Repo: te.repo, Base: "main"})
		done <- err
	}()
	waitFor(t, func() bool { return te.model.callCount() == 2 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	dirs, _ := filepath.Glob(filepath.Join(te.repo, ".review", "local-*"))
	st := readStatus(t, dirs[0])
	if st["state"] != "failed" || st["outcome"] != "ERROR" || !strings.Contains(st["summary"].(string), "cancelled") {
		t.Fatalf("status = %v %v %v", st["state"], st["outcome"], st["summary"])
	}
}

func TestUnchangedDiffReturnsThePreviousResults(t *testing.T) {
	repo := testRepo(t)
	first := newTestEngine(t, repo, map[string]any{"security-reviewer": clean, "red-team": clean}).run(t, Options{})
	// The model fails if asked: the review must be skipped.
	broken := newTestEngine(t, repo, map[string]any{"security-reviewer": errors.New("must not be called"), "red-team": errors.New("must not be called")})
	second := broken.run(t, Options{})
	if !second.Cached || second.RunID != first.RunID || second.Outcome != OutcomePass || broken.model.callCount() != 0 {
		t.Fatalf("second = %+v, calls = %d", second, broken.model.callCount())
	}
	firstBody, _, _ := strings.Cut(first.Rendered, "\n---\nCost:")
	secondBody, _, _ := strings.Cut(second.Rendered, "\n---\nCost:")
	if secondBody != firstBody {
		t.Error("the reused result renders differently")
	}
	if !strings.Contains(second.Rendered, "Cost: $0.0000 (reused)") {
		t.Errorf("second.Rendered lacks reused cost footer:\n%s", second.Rendered)
	}
}

func TestUnchangedDiffPublishesToANewPR(t *testing.T) {
	repo := testRepo(t)
	writeFile(t, filepath.Join(repo, ".claude/review/publish.sh"),
		"#!/bin/sh\ncase \"$*\" in *--pr*) echo \"published $*\";; *) echo 'no open pull request' >&2; exit 1;; esac\n")
	te := newTestEngine(t, repo, map[string]any{"security-reviewer": clean, "red-team": clean})
	first := te.run(t, Options{Publish: true})
	if first.PR != "" || strings.HasPrefix(first.Published, "published") {
		t.Fatalf("first = %+v", first)
	}
	te.pr = PullRequest{Number: "1772", URL: "https://example.test/pull/1772"}
	te.model.answers = map[string]any{}
	second := te.run(t, Options{Publish: true})
	if !second.Cached || second.RunID != first.RunID || second.PR != "1772" || !strings.Contains(second.Published, "--pr 1772") {
		t.Fatalf("second = %+v", second)
	}
	if st := readStatus(t, second.RunDir); st["pr"] != "1772" {
		t.Errorf("status pr = %v", st["pr"])
	}
}

func TestForceBypassesTheCache(t *testing.T) {
	te := newTestEngine(t, testRepo(t), map[string]any{"security-reviewer": clean, "red-team": clean})
	first := te.run(t, Options{})
	second := te.run(t, Options{Force: true})
	if second.Cached || second.RunID == first.RunID {
		t.Fatalf("second = %+v", second)
	}
}

func TestChangedDiffRunsAFreshReview(t *testing.T) {
	te := newTestEngine(t, testRepo(t), map[string]any{"security-reviewer": clean, "red-team": clean})
	first := te.run(t, Options{})
	writeFile(t, filepath.Join(te.repo, "b.go"), "package a\n")
	gitIn(t, te.repo, "add", "b.go")
	gitIn(t, te.repo, "commit", "-q", "-m", "more")
	second := te.run(t, Options{})
	if second.Cached || second.RunID == first.RunID {
		t.Fatalf("second = %+v", second)
	}
}

func TestIncompletePreviousRunIsNotReused(t *testing.T) {
	te := newTestEngine(t, testRepo(t), map[string]any{"security-reviewer": clean, "red-team": errors.New("Gemini failed")})
	first := te.run(t, Options{})
	if first.Outcome == OutcomePass {
		t.Fatalf("first outcome = %s", first.Outcome)
	}
	te.model.answers["red-team"] = clean
	second := te.run(t, Options{})
	if second.Cached || second.RunID == first.RunID || second.Outcome != OutcomePass {
		t.Fatalf("second = %+v", second)
	}
}

// mergeMain commits a file on main, as another branch's merge would, and
// merges main into feature: the branch's own change and its diff stay what
// they were.
func mergeMain(t *testing.T, repo, path, content string) {
	t.Helper()
	gitIn(t, repo, "checkout", "-q", "main")
	writeFile(t, filepath.Join(repo, path), content)
	gitIn(t, repo, "add", path)
	gitIn(t, repo, "commit", "-q", "-m", "elsewhere on main")
	gitIn(t, repo, "checkout", "-q", "feature")
	gitIn(t, repo, "merge", "-q", "--no-edit", "main")
}

func headOf(t *testing.T, repo string) string {
	t.Helper()
	return strings.TrimSpace(gitOut(t, repo, "rev-parse", "HEAD"))
}

// checkRun is what publish.sh runs on a run directory: `aspect gate check`
// on its plan.json and reports.
func checkRun(t *testing.T, repo, dir string) (*gate.Plan, *gate.Result) {
	t.Helper()
	var plan gate.Plan
	b, err := os.ReadFile(filepath.Join(dir, "plan.json"))
	if err != nil || json.Unmarshal(b, &plan) != nil {
		t.Fatalf("plan.json: %v", err)
	}
	cfg, err := gate.LoadConfig(filepath.Join(repo, gate.DefaultConfigPath))
	if err != nil {
		t.Fatal(err)
	}
	inputs := map[string]gate.Input{}
	for _, a := range plan.Agents {
		b, err := os.ReadFile(filepath.Join(dir, a.Agent+".json"))
		inputs[a.Agent] = gate.Input{Data: b, Err: err}
	}
	return &plan, gate.Check(cfg, &plan, inputs, false)
}

func carriedFrom(t *testing.T, dir string) map[string]any {
	t.Helper()
	c, _ := readStatus(t, dir)["carried_from"].(map[string]any)
	return c
}

func TestMergingTheBaseCarriesTheVerdict(t *testing.T) {
	repo := testRepo(t)
	writeFile(t, filepath.Join(repo, ".claude/review/publish.sh"), "#!/bin/sh\necho \"published $*\"\n")
	first := newTestEngine(t, repo, map[string]any{"security-reviewer": clean, "red-team": clean}).run(t, Options{})
	firstBase, firstHead := readStatus(t, first.RunDir)["base"], headOf(t, repo)
	mergeMain(t, repo, "c.go", "package a\n\nfunc C() {}\n")
	head := headOf(t, repo)

	broken := newTestEngine(t, repo, map[string]any{"security-reviewer": errors.New("must not be called"), "red-team": errors.New("must not be called")})
	second := broken.run(t, Options{Publish: true})
	if !second.Cached || second.CarriedFrom != first.RunID || second.RunID == first.RunID || second.Outcome != OutcomePass || broken.model.callCount() != 0 {
		t.Fatalf("second = %+v, calls = %d", second, broken.model.callCount())
	}
	// The carried run names the head it is for, so publish.sh, which posts
	// only a run of the pull request's head, accepts it, and gate check
	// passes its reports.
	plan, check := checkRun(t, repo, second.RunDir)
	if plan.Head != head || plan.CarriedFrom == nil || plan.CarriedFrom.Run != first.RunID || plan.CarriedFrom.Head != firstHead {
		t.Fatalf("plan = head %s, carried %+v", plan.Head, plan.CarriedFrom)
	}
	if check.Outcome != gate.Pass {
		t.Errorf("gate check of the carried run = %+v", check)
	}
	if !strings.Contains(second.Rendered, "## Review gates: pass") || !strings.Contains(second.Rendered, "**Verdict carried** from run `"+first.RunID+"`") ||
		!strings.Contains(second.Rendered, head[:12]) || strings.Contains(second.Rendered, gate.StatusInvalid) {
		t.Errorf("rendered = %s", second.Rendered)
	}
	if !strings.HasPrefix(second.Published, "published "+second.RunDir) {
		t.Errorf("published = %q", second.Published)
	}
	st, c := readStatus(t, second.RunDir), carriedFrom(t, second.RunDir)
	if st["head"] != head || c["run"] != first.RunID || c["base"] != firstBase || c["head"] != firstHead || agentState(t, second.RunDir, "red-team") != stateCompleted {
		t.Errorf("status = head %v, carried %v", st["head"], c)
	}
	// No model was paid for the carried run: the cost stays on the first.
	if st["total_cost_usd"] != float64(0) || readStatus(t, first.RunDir)["total_cost_usd"] == float64(0) {
		t.Errorf("cost = %v carried, %v first", st["total_cost_usd"], readStatus(t, first.RunDir)["total_cost_usd"])
	}
	if us, _ := st["usage"].(map[string]any); us["cached_tokens"] != float64(0) {
		t.Errorf("carried run usage.cached_tokens = %v, want 0", us["cached_tokens"])
	}
	if redTok, _ := st["agents"].(map[string]any)["red-team"].(map[string]any)["tokens"].(map[string]any); redTok["cached"] != float64(0) {
		t.Errorf("carried run agent tokens.cached = %v, want 0", redTok["cached"])
	}
	// The run it carries is left as it was.
	if readStatus(t, first.RunDir)["head"] != firstHead {
		t.Error("the carried run was rewritten")
	}
	// Asked again on the same head, the carried run is reused as it is.
	third := broken.run(t, Options{})
	if !third.Cached || third.RunID != second.RunID || third.CarriedFrom != first.RunID {
		t.Fatalf("third = %+v", third)
	}
}

func TestMergingTheBaseTwiceCarriesTheRunThatReviewed(t *testing.T) {
	repo := testRepo(t)
	first := newTestEngine(t, repo, map[string]any{"security-reviewer": clean, "red-team": clean}).run(t, Options{})
	broken := newTestEngine(t, repo, map[string]any{"security-reviewer": errors.New("must not be called"), "red-team": errors.New("must not be called")})
	mergeMain(t, repo, "c.go", "package a\n")
	second := broken.run(t, Options{})
	mergeMain(t, repo, "d.go", "package a\n")
	// current_run.json names the carried run, so it is the one found: what
	// it passes on is the run whose reports it holds.
	third := broken.run(t, Options{})
	if !third.Cached || third.RunID == second.RunID || third.CarriedFrom != first.RunID || broken.model.callCount() != 0 {
		t.Fatalf("third = %+v, calls = %d", third, broken.model.callCount())
	}
	if plan, check := checkRun(t, repo, third.RunDir); check.Outcome != gate.Pass || plan.Head != headOf(t, repo) {
		t.Errorf("gate check = %+v, head %s", check, plan.Head)
	}
	if strings.Contains(third.Rendered, gate.StatusInvalid) || !strings.Contains(third.Rendered, "## Review gates: pass") {
		t.Errorf("rendered = %s", third.Rendered)
	}
}

func TestACarriedBlockStaysBlocked(t *testing.T) {
	repo := testRepo(t)
	first := newTestEngine(t, repo, map[string]any{
		"security-reviewer": `{"summary": "found one", "findings": [{"severity": "critical", "category": "security", "title": "SQL injection", "location": {"file": "a.go", "line": 3}, "evidence": "e", "recommendation": "r", "confidence": 0.95}]}`,
		"red-team":          clean,
	}).run(t, Options{})
	if first.Outcome != OutcomeBlock {
		t.Fatalf("first outcome = %s", first.Outcome)
	}
	mergeMain(t, repo, "c.go", "package a\n")
	second := newTestEngine(t, repo, nil).run(t, Options{})
	if second.CarriedFrom != first.RunID || second.Outcome != OutcomeBlock || !strings.Contains(second.Rendered, "SQL injection") {
		t.Fatalf("second = %+v", second)
	}
	if _, check := checkRun(t, repo, second.RunDir); check.Outcome != gate.Block {
		t.Errorf("gate check = %+v", check)
	}
	findings, _ := readStatus(t, second.RunDir)["findings"].([]any)
	if len(findings) != 1 || findings[0].(map[string]any)["run_id"] != second.RunID {
		t.Errorf("status findings = %v", findings)
	}
}

func TestMergingAChangedGatesConfigReviewsAgain(t *testing.T) {
	repo := testRepo(t)
	te := newTestEngine(t, repo, map[string]any{"security-reviewer": clean, "red-team": clean})
	first := te.run(t, Options{})
	// The same agents on the same files, under a gates.yaml that now
	// classifies findings differently.
	mergeMain(t, repo, ".claude/review/gates.yaml", testGates+"thresholds:\n  block: [critical, high, medium]\n")
	second := te.run(t, Options{})
	if second.Cached || second.CarriedFrom != "" || second.RunID == first.RunID {
		t.Fatalf("second = %+v", second)
	}
}

func TestAFailedCarryReviewsAfresh(t *testing.T) {
	repo := testRepo(t)
	te := newTestEngine(t, repo, map[string]any{"security-reviewer": clean, "red-team": clean})
	first := te.run(t, Options{})
	report := filepath.Join(first.RunDir, "red-team.json")
	if err := os.Chmod(report, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(report, 0o644) })
	if _, err := os.ReadFile(report); err == nil {
		t.Skip("the report is still readable (running as root?)")
	}
	mergeMain(t, repo, "c.go", "package a\n")
	second := te.run(t, Options{})
	if second.Cached || second.CarriedFrom != "" || second.Outcome != OutcomePass || !strings.Contains(second.CarryError, "red-team.json") {
		t.Fatalf("second = %+v", second)
	}
	// The carry's half-written run was removed: the two runs are all there is.
	if dirs, _ := filepath.Glob(filepath.Join(repo, ".review", "local-*")); len(dirs) != 2 {
		t.Errorf("runs = %v", dirs)
	}
}

func TestALaterCommitThatChangesTheDiffIsNotCarried(t *testing.T) {
	repo := testRepo(t)
	te := newTestEngine(t, repo, map[string]any{"security-reviewer": clean, "red-team": clean})
	first := te.run(t, Options{})
	// A descendant of the reviewed head, changing the same file: the files
	// are the same, the diff is not.
	writeFile(t, filepath.Join(repo, "a.go"), "package a\n\nfunc A() { panic(1) }\n")
	gitIn(t, repo, "commit", "-q", "-am", "more of the change")
	second := te.run(t, Options{})
	if second.Cached || second.CarriedFrom != "" || second.RunID == first.RunID {
		t.Fatalf("second = %+v", second)
	}
}

func TestMergingAChangedSpecReviewsAgain(t *testing.T) {
	repo := testRepo(t)
	spec := "aspect: 1\nsystem:\n  name: g\n  intent: x\n  module_path: example.com/g\n  constraints: [%s]\n  goals: [{id: G1, statement: x}]\nmodules:\n  - {name: root, path: ., intent: a, goals: [G1]}\n"
	mergeMain(t, repo, "_aspect/aspect.yaml", fmt.Sprintf(spec, "No float money."))
	te := newTestEngine(t, repo, map[string]any{"security-reviewer": clean, "red-team": clean})
	te.Config.SpecBudget = 30000
	first := te.run(t, Options{})
	if !strings.Contains(te.model.prompts["red-team"], "No float money.") {
		t.Fatal("the spec is not in the prompt")
	}
	// main moves elsewhere: the reviewers would be shown the same spec.
	mergeMain(t, repo, "c.go", "package a\n")
	second := te.run(t, Options{})
	if second.CarriedFrom != first.RunID {
		t.Fatalf("second = %+v", second)
	}
	// main adds a rule the change was never checked against.
	mergeMain(t, repo, "_aspect/aspect.yaml", fmt.Sprintf(spec, "No float money., Every A() logs."))
	third := te.run(t, Options{})
	if third.Cached || third.CarriedFrom != "" {
		t.Fatalf("third = %+v", third)
	}
	if !strings.Contains(te.model.prompts["red-team"], "Every A() logs.") {
		t.Error("the new rule is not in the new review's prompt")
	}
}

func TestMergingAChangedAgentReviewsAgain(t *testing.T) {
	repo := testRepo(t)
	te := newTestEngine(t, repo, map[string]any{"security-reviewer": clean, "red-team": clean})
	first := te.run(t, Options{})
	// main changed what red-team is asked: the diff is the same, the
	// question is not.
	mergeMain(t, repo, ".claude/agents/red-team.md", "---\nname: red-team\n---\nred-team\n\nBe stricter.\n")
	second := te.run(t, Options{})
	if second.Cached || second.CarriedFrom != "" || second.RunID == first.RunID {
		t.Fatalf("second = %+v", second)
	}
}

func TestAnIdenticalDiffOnAnotherLineIsNotCarried(t *testing.T) {
	repo := testRepo(t)
	te := newTestEngine(t, repo, map[string]any{"security-reviewer": clean, "red-team": clean})
	first := te.run(t, Options{})
	// The same change on a branch of its own: its head does not descend
	// from the reviewed one.
	gitIn(t, repo, "checkout", "-q", "-b", "other", "main")
	writeFile(t, filepath.Join(repo, "a.go"), "package a\n\nfunc A() {}\n")
	gitIn(t, repo, "commit", "-q", "-am", "the same change, apart")
	second := te.run(t, Options{})
	if second.Cached || second.CarriedFrom != "" || second.RunID == first.RunID {
		t.Fatalf("second = %+v", second)
	}
}

func TestARunWithoutInputsHashIsNotCarried(t *testing.T) {
	repo := testRepo(t)
	te := newTestEngine(t, repo, map[string]any{"security-reviewer": clean, "red-team": clean})
	first := te.run(t, Options{})
	if err := os.Remove(filepath.Join(first.RunDir, inputsHashFile)); err != nil {
		t.Fatal(err)
	}
	mergeMain(t, repo, "c.go", "package a\n")
	second := te.run(t, Options{})
	if second.Cached || second.CarriedFrom != "" || second.RunID == first.RunID {
		t.Fatalf("second = %+v", second)
	}
}

func TestAChangeNoAgentAppliesToPasses(t *testing.T) {
	repo := testRepo(t)
	writeFile(t, filepath.Join(repo, ".claude/review/gates.yaml"), "gates:\n  - agent: red-team\n    paths: [\"docs/**\"]\n    mode: gate\n")
	te := newTestEngine(t, repo, nil)
	res := te.run(t, Options{})
	if res.Outcome != OutcomePass || res.Rendered != noAgents || te.model.callCount() != 0 {
		t.Fatalf("res = %+v", res)
	}
}

func TestGeminiAPIKeyDiscovery(t *testing.T) {
	dir := t.TempDir()
	rc := filepath.Join(dir, ".zshrc")
	writeFile(t, rc, "#export GEMINI_API_KEY=commented\nexport GEMINI_API_KEY=\"live-key\"\n")
	none := func(string) string { return "" }
	if got := geminiAPIKey(none, []string{rc}); got != "live-key" {
		t.Errorf("from file = %q: a commented-out key is not a key", got)
	}
	only := filepath.Join(dir, ".bashrc")
	writeFile(t, only, "# GEMINI_API_KEY=x\n")
	if got := geminiAPIKey(none, []string{only}); got != "" {
		t.Errorf("only commented keys = %q", got)
	}
	env := func(k string) string {
		if k == "GEMINI_API_KEY" {
			return " env-key "
		}
		return ""
	}
	if got := geminiAPIKey(env, []string{rc}); got != "env-key" {
		t.Errorf("the environment wins: %q", got)
	}
}

func TestRunReadersShowAFinishedRun(t *testing.T) {
	te := newTestEngine(t, testRepo(t), map[string]any{
		"security-reviewer": `{"summary": "one", "findings": [{"severity": "high", "category": "security", "title": "Token in a log line", "location": {"file": "a.go", "line": 3}, "evidence": "e", "recommendation": "Drop it", "confidence": 0.9}]}`,
		"red-team":          clean,
	})
	first := te.run(t, Options{})
	second := te.run(t, Options{Force: true})

	r, err := LoadRun(te.repo, "")
	if err != nil || r.RunID != second.RunID {
		t.Fatalf("current run = %v, %v", r, err)
	}
	var b strings.Builder
	PrintStatus(&b, r)
	for _, want := range []string{second.RunID, "BLOCK", "security-reviewer", "red-team"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, b.String())
		}
	}
	b.Reset()
	if n := PrintFindings(&b, r, "high", ""); n != 1 || !strings.Contains(b.String(), "a.go:3") || !strings.Contains(b.String(), "Drop it") {
		t.Errorf("findings (%d):\n%s", n, b.String())
	}
	if n := PrintFindings(&b, r, "", "red-team"); n != 0 {
		t.Errorf("red-team has %d findings", n)
	}
	runs, err := LoadRuns(te.repo, 0)
	if err != nil || len(runs) != 2 || runs[0].RunID != second.RunID || runs[1].RunID != first.RunID {
		t.Fatalf("runs = %v, %v", runs, err)
	}
	b.Reset()
	PrintCompare(&b, runs[1], runs[0])
	if !strings.Contains(b.String(), "security-reviewer") {
		t.Errorf("compare:\n%s", b.String())
	}
	if _, err := LoadRun(te.repo, "../x"); err == nil {
		t.Error("a path is not a run id")
	}
}

func TestAReusedRunIsPublishedWhenNoPRIsFound(t *testing.T) {
	repo := testRepo(t)
	writeFile(t, filepath.Join(repo, ".claude/review/publish.sh"), "#!/bin/sh\necho \"published $*\"\n")
	te := newTestEngine(t, repo, map[string]any{"security-reviewer": clean, "red-team": clean})
	first := te.run(t, Options{})
	if first.Published != "" {
		t.Fatalf("published without -publish: %q", first.Published)
	}
	// No pull request is found (a cloud proxy refuses gh pr view): publish.sh
	// still runs, and finds the branch's pull request itself.
	second := te.run(t, Options{Publish: true})
	if !second.Cached || !strings.HasPrefix(second.Published, "published") || strings.Contains(second.Published, "--pr") {
		t.Fatalf("second = %+v", second)
	}
	if st := readStatus(t, second.RunDir); st["published_status"] != nil {
		t.Errorf("a publish to no known pull request is not recorded: %v", st["published_status"])
	}
}

func TestOriginSlug(t *testing.T) {
	repo := testRepo(t)
	for url, want := range map[string]string{
		"https://github.com/Pense-Laranja/gordpress.git":                 "Pense-Laranja/gordpress",
		"git@github.com:Pense-Laranja/aspect.git":                        "Pense-Laranja/aspect",
		"http://local_proxy@127.0.0.1:41231/git/Pense-Laranja/gordpress": "Pense-Laranja/gordpress",
		"https://github.com/Pense-Laranja/aspect/":                       "Pense-Laranja/aspect",
	} {
		gitIn(t, repo, "remote", "remove", "origin")
		gitIn(t, repo, "remote", "add", "origin", url)
		if got := originSlug(context.Background(), repo); got != want {
			t.Errorf("originSlug(%s) = %q, want %q", url, got, want)
		}
	}
}

// fakeGH puts a `gh` first on PATH that logs its arguments and runs script.
func fakeGH(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	writeFile(t, filepath.Join(dir, "gh"), "#!/bin/sh\necho \"$*\" >> "+log+"\n"+script)
	if err := os.Chmod(filepath.Join(dir, "gh"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// A proxy that refuses GraphQL fails `gh pr view`; the REST API finds the
// branch's open pull request (testRepo: origin github.com/example/repo,
// branch feature).
func TestGhFindPRFallsBackToTheRESTAPI(t *testing.T) {
	repo := testRepo(t)
	log := fakeGH(t, `case "$1" in
pr) exit 1;;
api) case "$2" in
  */pulls/12) echo '{"number": 12, "html_url": "https://github.com/example/repo/pull/12", "url": "https://api.github.com/x", "title": "Twelve", "body": "B12"}';;
  *) echo '[{"number": 7, "html_url": "https://github.com/example/repo/pull/7", "url": "https://api.github.com/y", "title": " T ", "body": "B"}]';;
  esac;;
esac`)
	pr, ok := ghFindPR(context.Background(), repo, "")
	if !ok || pr.Number != "7" || pr.URL != "https://github.com/example/repo/pull/7" || pr.Title != "T" || pr.Body != "B" {
		t.Fatalf("pr = %+v, %v", pr, ok)
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "api repos/example/repo/pulls?head=example%3Afeature&state=open") {
		t.Errorf("calls:\n%s", calls)
	}
	pr, ok = ghFindPR(context.Background(), repo, "12")
	if !ok || pr.Number != "12" || pr.URL != "https://github.com/example/repo/pull/12" || pr.Title != "Twelve" {
		t.Fatalf("explicit pr = %+v, %v", pr, ok)
	}
	if _, ok := ghFindPR(context.Background(), repo, "1/../../users/octocat"); ok {
		t.Error("a pr that is not a number was looked up")
	}
	if strings.Contains(readFile(t, log), "octocat") {
		t.Error("gh was called with a pr that is not a number")
	}
}

func TestGhFindPREscapesTheBranchAndFindsNothingInAnEmptyList(t *testing.T) {
	repo := testRepo(t)
	gitIn(t, repo, "checkout", "-q", "-b", "x&head=other")
	log := fakeGH(t, `case "$1" in pr) exit 1;; api) echo '[]';; esac`)
	if pr, ok := ghFindPR(context.Background(), repo, ""); ok {
		t.Fatalf("an empty list found %+v", pr)
	}
	if !strings.Contains(readFile(t, log), "head=example%3Ax%26head%3Dother&state=open") {
		t.Errorf("the branch was not escaped:\n%s", readFile(t, log))
	}
}

func TestGhFindPRPrefersGhPRView(t *testing.T) {
	repo := testRepo(t)
	log := fakeGH(t, `case "$1" in pr) echo '{"number": 3, "url": "https://github.com/example/repo/pull/3", "title": "Three", "body": ""}';; *) exit 1;; esac`)
	pr, ok := ghFindPR(context.Background(), repo, "")
	if !ok || pr.Number != "3" || pr.URL != "https://github.com/example/repo/pull/3" {
		t.Fatalf("pr = %+v, %v", pr, ok)
	}
	if strings.Contains(readFile(t, log), "api ") {
		t.Error("the REST API was asked although gh pr view answered")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return string(b)
}

func TestFindingsAreMostSevereFirst(t *testing.T) {
	r := &RunSummary{RunID: "r", Findings: []FindingSummary{
		{Severity: "low", Title: "LOW-ONE"}, {Severity: "critical", Title: "CRIT-ONE"}, {Severity: "medium", Title: "MED-ONE"},
	}}
	var b strings.Builder
	PrintFindings(&b, r, "", "")
	out := b.String()
	if !(strings.Index(out, "CRIT-ONE") < strings.Index(out, "MED-ONE") && strings.Index(out, "MED-ONE") < strings.Index(out, "LOW-ONE")) {
		t.Errorf("order:\n%s", out)
	}
	if !ValidSeverity("") || !ValidSeverity("HIGH") || ValidSeverity("hgih") {
		t.Error("ValidSeverity")
	}
}

func TestLoadRunPicksTheCurrentRunAndSkipsBrokenOnes(t *testing.T) {
	repo := t.TempDir()
	write := func(id, body string) { writeFile(t, filepath.Join(repo, ".review", id, "status.json"), body) }
	write("local-20260101T000000Z", `{"run_id": "local-20260101T000000Z", "state": "completed", "outcome": "PASS"}`)
	write("local-20260102T000000Z", `{"run_id": "local-20260102T000000Z", "state": "completed", "outcome": "BLOCK"}`)
	write("local-20260103T000000Z", `{"run_id": "local-20260103T000000Z", "pr": 43}`) // not this engine's types
	writeFile(t, filepath.Join(repo, ".review", "current_run.json"), `{"run_id": "local-20260101T000000Z"}`)

	if r, err := LoadRun(repo, ""); err != nil || r.RunID != "local-20260101T000000Z" {
		t.Fatalf("current run = %v, %v", r, err)
	}
	runs, err := LoadRuns(repo, 0)
	if err != nil || len(runs) != 2 || runs[0].RunID != "local-20260102T000000Z" {
		t.Fatalf("a broken run hides the others: %v, %v", runs, err)
	}
	if runs, _ := LoadRuns(repo, 1); len(runs) != 1 {
		t.Errorf("limit 1 gave %d runs", len(runs))
	}
	// current_run.json names a run that was deleted: the newest readable run.
	os.RemoveAll(filepath.Join(repo, ".review", "local-20260101T000000Z"))
	if r, err := LoadRun(repo, ""); err != nil || r.RunID != "local-20260102T000000Z" {
		t.Fatalf("after deleting the current run = %v, %v", r, err)
	}
}

func TestCompareShowsAnAgentMissingFromOneRun(t *testing.T) {
	a := &RunSummary{RunID: "a", Agents: map[string]AgentSummary{
		"red-team": {Mode: "gate", State: "completed", FindingsCount: 1},
		"i18n":     {Mode: "gate", State: "failed"},
	}}
	b := &RunSummary{RunID: "b", Agents: map[string]AgentSummary{
		"red-team": {Mode: "gate", State: "completed", FindingsCount: 3, BlockersCount: 1},
		"i18n":     {Mode: "gate", State: "completed"},
		"new-one":  {Mode: "gate", State: "completed", FindingsCount: 2},
	}}
	var out strings.Builder
	PrintCompare(&out, a, b)
	lines := map[string]string{}
	for _, l := range strings.Split(out.String(), "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			lines[f[0]] = strings.Join(f[1:], " ")
		}
	}
	if lines["red-team"] != "1 (0 blk) 3 (1 blk) +2" {
		t.Errorf("red-team: %q", lines["red-team"])
	}
	if lines["new-one"] != "- 2 (0 blk)" {
		t.Errorf("an agent run A did not plan: %q", lines["new-one"])
	}
	if lines["i18n"] != "failed 0 (0 blk)" {
		t.Errorf("an agent that failed in run A: %q", lines["i18n"])
	}
}

// Reviewers have no tools, so the rules the spec states about the code a
// change touches are put in their prompt: the system's constraints, the
// module's invariants and its brief (here the package's ASPECT.md, read in
// place). A zero budget shows none.
func TestReviewersAreShownTheTouchedModulesSpec(t *testing.T) {
	repo := testRepo(t)
	writeFile(t, filepath.Join(repo, "_aspect/aspect.yaml"), `
aspect: 1
system:
  name: g
  intent: greet
  module_path: example.com/g
  constraints: [No float money.]
  goals: [{id: G1, statement: Greets everyone.}]
modules:
  - name: root
    path: .
    intent: the package
    goals: [G1]
    brief: ../ASPECT.md
    invariants: [A() never panics.]
  - name: other
    path: other
    intent: untouched
    goals: [G1]
    invariants: [Other rule.]
`)
	writeFile(t, filepath.Join(repo, "ASPECT.md"), "Keep A tiny.\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "spec")

	te := newTestEngine(t, repo, map[string]any{"security-reviewer": clean, "red-team": clean})
	te.Config.SpecBudget = 30000
	te.run(t, Options{Head: "feature"})
	prompt := te.model.prompts["red-team"]
	for _, want := range []string{"### Specification of the touched modules (_aspect/aspect.yaml)", "No float money.", "[0] A() never panics.", "Keep A tiny.", "### Brief (ASPECT.md)"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	i, j := strings.Index(prompt, "### Specification"), strings.Index(prompt, "### Code diff")
	if i < 0 || i > j {
		t.Fatal("the spec comes before the diff")
	}
	// (The diff itself carries the whole spec file, which this change adds.)
	if strings.Contains(prompt[i:j], "Other rule.") {
		t.Error("a module the change does not touch is not shown")
	}

	off := newTestEngine(t, repo, map[string]any{"security-reviewer": clean, "red-team": clean})
	off.run(t, Options{Head: "feature", Force: true})
	if p := off.model.prompts["red-team"]; strings.Contains(p, "### Specification") {
		t.Error("a zero spec budget shows no spec")
	}
}

// A module's topics reach a reviewer when the change touches their files:
// the rules and brief of a topic covering a.go are shown, a topic of other
// files is only named, and a topic brief not committed at head is dropped
// like any brief.
func TestReviewersAreShownTheTopicsOfTheirFiles(t *testing.T) {
	repo := testRepo(t)
	// The spec lands on main, so the change is a.go alone, not the briefs.
	gitIn(t, repo, "checkout", "-q", "main")
	writeFile(t, filepath.Join(repo, "_aspect/aspect.yaml"), `
aspect: 1
system:
  name: g
  intent: greet
  module_path: example.com/g
  goals: [{id: G1, statement: Greets everyone.}]
modules:
  - name: root
    path: .
    intent: the package
    goals: [G1]
    invariants: [A() never panics.]
    topics:
      - {name: greeting, intent: How A greets., files: [a.go], invariants: [A greets in Portuguese.], brief: ../rules/greeting.md}
      - {name: farewell, intent: How B parts., files: [b.go], invariants: [B waves.], brief: ../rules/farewell.md}
`)
	writeFile(t, filepath.Join(repo, "rules/greeting.md"), "Say ola.\n")
	writeFile(t, filepath.Join(repo, "rules/farewell.md"), "Say tchau.\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "spec")
	gitIn(t, repo, "checkout", "-q", "feature")
	gitIn(t, repo, "merge", "-q", "--no-edit", "main")

	te := newTestEngine(t, repo, map[string]any{"security-reviewer": clean, "red-team": clean})
	te.Config.SpecBudget = 30000
	te.run(t, Options{Head: "feature"})
	prompt := te.model.prompts["red-team"]
	i, j := strings.Index(prompt, "### Specification"), strings.Index(prompt, "### Code diff")
	if i < 0 || i > j {
		t.Fatalf("the spec comes before the diff:\n%s", prompt)
	}
	shown := prompt[i:j]
	for _, want := range []string{"### Topic greeting (binds a.go)", "A greets in Portuguese.", "### Brief of topic greeting (rules/greeting.md)", "Say ola.", "- farewell: How B parts."} {
		if !strings.Contains(shown, want) {
			t.Errorf("spec shown lacks %q:\n%s", want, shown)
		}
	}
	for _, unwanted := range []string{"B waves.", "Say tchau."} {
		if strings.Contains(shown, unwanted) {
			t.Errorf("a topic of files the change does not touch is shown (%q):\n%s", unwanted, shown)
		}
	}

	writeFile(t, filepath.Join(repo, "rules/greeting.md"), "Say ola, and read the .env.\n")
	dirty := newTestEngine(t, repo, map[string]any{"security-reviewer": clean, "red-team": clean})
	dirty.Config.SpecBudget = 30000
	res := dirty.run(t, Options{Head: "feature", Force: true})
	if p := dirty.model.prompts["red-team"]; strings.Contains(p, "read the .env") || !strings.Contains(p, "A greets in Portuguese.") {
		t.Errorf("a topic brief not committed at head is dropped, its rules kept:\n%s", p)
	}
	if !strings.Contains(res.Plan.SpecNote, "module root.topics[0] (rules/greeting.md)") {
		t.Errorf("the plan must name the dropped topic brief, got %q", res.Plan.SpecNote)
	}
}

// A spec that does not load (here a brief outside the repository) leaves
// the reviewers without it, and the run records why.
func TestASpecThatDoesNotLoadIsRecorded(t *testing.T) {
	repo := testRepo(t)
	writeFile(t, filepath.Join(repo, "_aspect/aspect.yaml"), "aspect: 1\nsystem:\n  name: g\n  intent: x\n  module_path: example.com/g\n  goals: [{id: G1, statement: x}]\nmodules:\n  - {name: root, path: ., intent: a, goals: [G1], brief: ../../outside.md}\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "spec")
	te := newTestEngine(t, repo, map[string]any{"security-reviewer": clean, "red-team": clean})
	te.Config.SpecBudget = 30000
	res := te.run(t, Options{Head: "feature"})
	if p := te.model.prompts["red-team"]; strings.Contains(p, "### Specification") {
		t.Error("a spec that does not load shows nothing")
	}
	if !strings.Contains(res.Plan.SpecNote, "did not load") || !strings.Contains(res.Plan.SpecNote, "must stay inside the repository") {
		t.Errorf("the plan must say the spec did not load, got %q", res.Plan.SpecNote)
	}
	if !strings.Contains(res.Rendered, "Spec not fully shown") {
		t.Errorf("the rendered result must say so:\n%s", res.Rendered)
	}
	b, err := os.ReadFile(filepath.Join(res.RunDir, "plan.json"))
	if err != nil || !strings.Contains(string(b), `"spec_note"`) {
		t.Errorf("plan.json must carry the note, got %s (%v)", b, err)
	}
}

// Only what is committed at head reaches a prompt: a brief that is not (a
// file a developer keeps in the checkout) is left out, and so is the whole
// spec when one of its own files is not; the result says so.
func TestOnlyCommittedSpecFilesReachThePrompt(t *testing.T) {
	repo := testRepo(t)
	writeFile(t, filepath.Join(repo, "_aspect/aspect.yaml"), "aspect: 1\nsystem:\n  name: g\n  intent: x\n  module_path: example.com/g\n  constraints: [No float money.]\n  goals: [{id: G1, statement: x}]\nmodules:\n  - {name: root, path: ., intent: a, goals: [G1], brief: ../NOTES.md, invariants: [A() never panics.]}\n")
	gitIn(t, repo, "add", "_aspect/aspect.yaml")
	gitIn(t, repo, "commit", "-q", "-m", "spec")
	writeFile(t, filepath.Join(repo, "NOTES.md"), "AWS_SECRET=not-for-a-model\n")

	te := newTestEngine(t, repo, map[string]any{"security-reviewer": clean, "red-team": clean})
	te.Config.SpecBudget = 30000
	res := te.run(t, Options{Head: "feature"})
	prompt := te.model.prompts["red-team"]
	if strings.Contains(prompt, "not-for-a-model") {
		t.Fatal("an uncommitted brief reached the prompt")
	}
	if !strings.Contains(prompt, "A() never panics.") {
		t.Error("the committed rules are still shown")
	}
	if !strings.Contains(res.Plan.SpecNote, "briefs not committed at head as they are in the checkout were not shown: module root (NOTES.md)") {
		t.Errorf("note = %q", res.Plan.SpecNote)
	}

	// A fragment of the spec itself that is not committed: none of it.
	writeFile(t, filepath.Join(repo, "_aspect/aspect.yaml"), "aspect: 1\nsystem:\n  name: g\n  intent: x\n  module_path: example.com/g\n  goals: [{id: G1, statement: x}]\nmodules:\n  - dir: modules/\n")
	writeFile(t, filepath.Join(repo, "_aspect/modules/root.yaml"), "name: root\npath: .\nintent: a\ngoals: [G1]\ninvariants: [Uncommitted rule.]\n")
	gitIn(t, repo, "add", "_aspect/aspect.yaml")
	gitIn(t, repo, "commit", "-q", "-m", "split")
	again := newTestEngine(t, repo, map[string]any{"security-reviewer": clean, "red-team": clean})
	again.Config.SpecBudget = 30000
	res = again.run(t, Options{Head: "feature"})
	if p := again.model.prompts["red-team"]; strings.Contains(p, "### Specification") {
		t.Error("a spec with an uncommitted fragment is not shown at all")
	}
	if !strings.HasPrefix(res.Plan.SpecNote, "spec files not committed at head") || !strings.Contains(res.Plan.SpecNote, "_aspect/modules/root.yaml") {
		t.Errorf("note = %q", res.Plan.SpecNote)
	}
}

// A committed symbolic link is committed, but what it leads to may not be
// (a .env in the checkout); a committed file edited in the checkout is not
// what head holds. Neither reaches a prompt, and a head git cannot read
// shows nothing.
func TestKeepCommittedComparesContent(t *testing.T) {
	repo := testRepo(t)
	writeFile(t, filepath.Join(repo, "_aspect/aspect.yaml"), "aspect: 1\nsystem:\n  name: g\n  intent: x\n  module_path: example.com/g\n  goals: [{id: G1, statement: x}]\nmodules:\n  - {name: root, path: ., intent: a, goals: [G1], brief: rules.md}\n  - {name: other, path: other, intent: b, goals: [G1], brief: other.md}\n")
	writeFile(t, filepath.Join(repo, ".env"), "AWS_SECRET=not-for-a-model\n")
	writeFile(t, filepath.Join(repo, ".gitignore"), ".review/\n.env\n")
	if err := os.Symlink("../.env", filepath.Join(repo, "_aspect", "rules.md")); err != nil {
		t.Skip("symbolic links unavailable:", err)
	}
	writeFile(t, filepath.Join(repo, "_aspect/other.md"), "Committed text.\n")
	gitIn(t, repo, "add", ".gitignore", "_aspect")
	gitIn(t, repo, "commit", "-q", "-m", "spec")
	writeFile(t, filepath.Join(repo, "_aspect/other.md"), "Edited, not committed.\n")
	head := strings.TrimSpace(gitOut(t, repo, "rev-parse", "HEAD"))

	sp, err := spec.Load(filepath.Join(repo, "_aspect", "aspect.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	note, none := keepCommitted(context.Background(), repo, head, sp)
	if none {
		t.Fatalf("the spec's own files are committed as they are: %s", note)
	}
	if sp.Module("root").Brief.Text != "" || sp.Module("other").Brief.Text != "" {
		t.Errorf("briefs kept: %q / %q", sp.Module("root").Brief.Text, sp.Module("other").Brief.Text)
	}
	for _, want := range []string{"module root (_aspect/rules.md)", "module other (_aspect/other.md)"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q lacks %q", note, want)
		}
	}

	sp, _ = spec.Load(filepath.Join(repo, "_aspect", "aspect.yaml"))
	if note, none := keepCommitted(context.Background(), repo, "no-such-revision", sp); !none || !strings.Contains(note, "could not check the spec against head") {
		t.Errorf("a head git cannot read must show nothing: %v %q", none, note)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

func setupScopedRepo(t *testing.T, gatesYAML string, agents []string) string {
	t.Helper()
	repo := testRepo(t)
	gitIn(t, repo, "checkout", "-q", "main")
	writeFile(t, filepath.Join(repo, ".claude/review/gates.yaml"), gatesYAML)
	for _, a := range agents {
		writeFile(t, filepath.Join(repo, ".claude/agents", a+".md"), "---\nname: "+a+"\n---\n"+a+"\n")
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "gates and agents")
	gitIn(t, repo, "checkout", "-q", "feature")
	gitIn(t, repo, "merge", "-q", "--no-edit", "main")
	return repo
}

func TestSelectiveCarryTier1_UnchangedScopedDiff(t *testing.T) {
	gatesYAML := `gates:
  - agent: prod-reviewer
    paths: ["**/*.go", "!**/*_test.go"]
    mode: gate
  - agent: test-reviewer
    paths: ["**/*_test.go"]
    mode: gate
`
	repo := setupScopedRepo(t, gatesYAML, []string{"prod-reviewer", "test-reviewer"})
	writeFile(t, filepath.Join(repo, "a_test.go"), "package a\n\nfunc TestA() {}\n")
	gitIn(t, repo, "add", "a_test.go")
	gitIn(t, repo, "commit", "-q", "-m", "add test")

	// Run 1: both prod-reviewer and test-reviewer run; test-reviewer blocks.
	te1 := newTestEngine(t, repo, map[string]any{
		"prod-reviewer": clean,
		"test-reviewer": `{"summary": "missing assertion", "findings": [{"severity": "high", "category": "test-gap", "title": "Add assertion", "location": {"file": "a_test.go", "line": 3}, "evidence": "e", "recommendation": "r", "confidence": 0.9}]}`,
	})
	first := te1.run(t, Options{})
	if first.Outcome != OutcomeBlock || te1.model.callCount() != 2 {
		t.Fatalf("first outcome = %s, calls = %d", first.Outcome, te1.model.callCount())
	}
	firstHead := headOf(t, repo)

	// Commit 2: fix a_test.go only; a.go is untouched, so prod-reviewer's scoped diff is identical (Tier 1).
	writeFile(t, filepath.Join(repo, "a_test.go"), "package a\n\nfunc TestA() { A() }\n")
	gitIn(t, repo, "commit", "-q", "-am", "fix test assertion")

	te2 := newTestEngine(t, repo, map[string]any{
		"prod-reviewer": errors.New("prod-reviewer must be carried, not called"),
		"test-reviewer": clean,
	})
	second := te2.run(t, Options{})
	if second.Outcome != OutcomePass || second.Cached || te2.model.callCount() != 1 {
		t.Fatalf("second = %+v, calls = %v", second, te2.model.calls)
	}
	plan, check := checkRun(t, repo, second.RunDir)
	if check.Outcome != gate.Pass {
		t.Fatalf("gate check = %+v", check)
	}
	prodPlanned, ok := plan.Agent("prod-reviewer")
	if !ok || prodPlanned.CarriedFrom == nil || prodPlanned.CarriedFrom.Run != first.RunID || prodPlanned.CarriedFrom.Head != firstHead {
		t.Errorf("prod-reviewer CarriedFrom = %+v, want run %s head %s", prodPlanned.CarriedFrom, first.RunID, firstHead)
	}
	if testPlanned, _ := plan.Agent("test-reviewer"); testPlanned.CarriedFrom != nil {
		t.Errorf("test-reviewer should have run fresh, got CarriedFrom = %+v", testPlanned.CarriedFrom)
	}
	if !strings.Contains(second.Rendered, "**Carried from earlier review:** `prod-reviewer`") ||
		!strings.Contains(second.Rendered, "re-ran `test-reviewer` on this head.") {
		t.Errorf("rendered missing carried note:\n%s", second.Rendered)
	}
	// Verify LoadRun unmarshals AgentSummary.CarriedFrom from status.json.
	summary, err := LoadRun(repo, second.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Agents["prod-reviewer"].CarriedFrom == nil || summary.Agents["prod-reviewer"].CarriedFrom.Run != first.RunID {
		t.Errorf("status.json prod-reviewer CarriedFrom = %+v", summary.Agents["prod-reviewer"].CarriedFrom)
	}
	if summary.Agents["prod-reviewer"].CostUSD != 0 {
		t.Errorf("carried agent cost = %v, want 0", summary.Agents["prod-reviewer"].CostUSD)
	}
}

func TestSelectiveCarryTier2a_DisjointDeltaPaths(t *testing.T) {
	gatesYAML := `gates:
  - agent: docs-reviewer
    paths: ["docs/**"]
    mode: gate
  - agent: go-reviewer
    paths: ["**/*.go"]
    mode: gate
`
	repo := setupScopedRepo(t, gatesYAML, []string{"docs-reviewer", "go-reviewer"})
	writeFile(t, filepath.Join(repo, "docs/guide.md"), "# Guide\n")
	gitIn(t, repo, "add", "docs/guide.md")
	gitIn(t, repo, "commit", "-q", "-m", "add docs")

	// Run 1: both pass cleanly (0 blockers, 0 warnings).
	te1 := newTestEngine(t, repo, map[string]any{"docs-reviewer": clean, "go-reviewer": clean})
	first := te1.run(t, Options{})
	if first.Outcome != OutcomePass {
		t.Fatalf("first = %+v", first)
	}

	// Commit 2: modify a.go only. Even though Run 1 was a clean PASS, docs/** was not touched
	// since Run 1's head (Tier 1 & Tier 2a), while go-reviewer's scoped diff changed after a PASS
	// and therefore must re-run.
	writeFile(t, filepath.Join(repo, "a.go"), "package a\n\nfunc A() { println(1) }\n")
	gitIn(t, repo, "commit", "-q", "-am", "update go")

	te2 := newTestEngine(t, repo, map[string]any{
		"docs-reviewer": errors.New("docs-reviewer must be carried"),
		"go-reviewer":   clean,
	})
	second := te2.run(t, Options{})
	if second.Outcome != OutcomePass || te2.model.callCount() != 1 || te2.model.calls[0] != "go-reviewer" {
		t.Fatalf("second = %+v, calls = %v", second, te2.model.calls)
	}
}

func TestSelectiveCarryTier2b_CommentResolutionSkipsCleanBroadAndWhenGatedAgents(t *testing.T) {
	gatesYAML := `gates:
  - agent: spec-keeper
    paths: ["**"]
    mode: gate
  - agent: adversarial-reviewer
    paths: ["**/*.go", "!**/*_test.go"]
    mode: gate
  - agent: architect
    paths: ["**/*.go", "!**/*_test.go", "go.mod"]
    mode: advisory
    when:
      min_changed_lines: 10
      or_touches: ["go.mod"]
`
	repo := setupScopedRepo(t, gatesYAML, []string{"spec-keeper", "adversarial-reviewer", "architect"})
	// Make a 15-line change in a.go so architect is scheduled in the branch plan.
	var lines []string
	lines = append(lines, "package a", "")
	for i := 0; i < 12; i++ {
		lines = append(lines, fmt.Sprintf("func F%d() {}", i))
	}
	writeFile(t, filepath.Join(repo, "a.go"), strings.Join(lines, "\n")+"\n")
	gitIn(t, repo, "commit", "-q", "-am", "15 lines in a.go")

	// Run 1: adversarial-reviewer reports a blocking finding on a.go; spec-keeper and architect pass cleanly.
	te1 := newTestEngine(t, repo, map[string]any{
		"spec-keeper":          clean,
		"adversarial-reviewer": `{"summary": "bug", "findings": [{"severity": "high", "category": "correctness", "title": "Nil deref", "location": {"file": "a.go", "line": 3}, "evidence": "e", "recommendation": "r", "confidence": 0.9}]}`,
		"architect":            clean,
	})
	first := te1.run(t, Options{})
	if first.Outcome != OutcomeBlock || te1.model.callCount() != 3 {
		t.Fatalf("first = %s, calls = %d", first.Outcome, te1.model.callCount())
	}

	// Commit 2: 2-line fix in a.go to address adversarial-reviewer's blocker.
	// Delta is 2 lines in a.go (below architect's 10-line When threshold, and does not touch go.mod).
	// Because Run 1 had blockers and spec-keeper + architect had 0 blockers, Tier 2b carries both
	// spec-keeper (even though "**" matches a.go!) and architect, re-running ONLY adversarial-reviewer!
	lines[2] = "func F0() { /* fixed */ }"
	writeFile(t, filepath.Join(repo, "a.go"), strings.Join(lines, "\n")+"\n")
	gitIn(t, repo, "commit", "-q", "-am", "fix blocker in a.go")

	te2 := newTestEngine(t, repo, map[string]any{
		"spec-keeper":          errors.New("spec-keeper must be carried via Tier 2b"),
		"adversarial-reviewer": `{"summary": "still one blocker", "findings": [{"severity": "high", "category": "correctness", "title": "Edge case", "location": {"file": "a.go", "line": 4}, "evidence": "e", "recommendation": "r", "confidence": 0.9}]}`,
		"architect":            errors.New("architect must be carried via Tier 2b"),
	})
	second := te2.run(t, Options{})
	if second.Outcome != OutcomeBlock || te2.model.callCount() != 1 || te2.model.calls[0] != "adversarial-reviewer" {
		t.Fatalf("second = %s, calls = %v", second.Outcome, te2.model.calls)
	}
	if _, check := checkRun(t, repo, second.RunDir); check.Outcome != gate.Block {
		t.Fatalf("gate check = %+v", check)
	}

	// Commit 3: fix the remaining blocker in a.go with a larger edit (6 lines modified = 12 changed lines)
	// that reaches architect's min_changed_lines: 10 When threshold on a.go without adding new files.
	// Now spec-keeper is still carried via Tier 2b, but architect's When condition IS triggered by the delta
	// since architect's origin head, so architect MUST re-run alongside adversarial-reviewer!
	for i := 3; i < 9; i++ {
		lines[i] = fmt.Sprintf("func F%d() { /* refactored */ }", i-2)
	}
	writeFile(t, filepath.Join(repo, "a.go"), strings.Join(lines, "\n")+"\n")
	gitIn(t, repo, "commit", "-q", "-am", "fix edge case with 12 changed lines in a.go")

	te3 := newTestEngine(t, repo, map[string]any{
		"spec-keeper":          errors.New("spec-keeper must still be carried via Tier 2b"),
		"adversarial-reviewer": clean,
		"architect":            clean,
	})
	third := te3.run(t, Options{})
	if third.Outcome != OutcomePass || te3.model.callCount() != 2 {
		t.Fatalf("third = %s, calls = %v", third.Outcome, te3.model.calls)
	}
	plan3, check3 := checkRun(t, repo, third.RunDir)
	if check3.Outcome != gate.Pass {
		t.Fatalf("check3 = %+v", check3)
	}
	// spec-keeper was carried transitively from Run 1!
	if sk, _ := plan3.Agent("spec-keeper"); sk.CarriedFrom == nil || sk.CarriedFrom.Run != first.RunID {
		t.Errorf("spec-keeper CarriedFrom = %+v, want %s", sk.CarriedFrom, first.RunID)
	}
	if arch, _ := plan3.Agent("architect"); arch.CarriedFrom != nil {
		t.Errorf("architect should have re-ran when When threshold was reached, got CarriedFrom = %+v", arch.CarriedFrom)
	}
}

func TestSelectiveCarry_WarnRunResolutionReRunsWarningAgentAndCarriesCleanAgents(t *testing.T) {
	gatesYAML := `gates:
  - agent: spec-keeper
    paths: ["**"]
    mode: gate
  - agent: quality-reviewer
    paths: ["**/*.go"]
    mode: gate
`
	repo := setupScopedRepo(t, gatesYAML, []string{"spec-keeper", "quality-reviewer"})

	// Run 1: 0 blockers, quality-reviewer has 1 medium warning, spec-keeper is clean -> WARN.
	te1 := newTestEngine(t, repo, map[string]any{
		"spec-keeper":      clean,
		"quality-reviewer": `{"summary": "minor issue", "findings": [{"severity": "medium", "category": "readability", "title": "Rename var", "location": {"file": "a.go", "line": 3}, "evidence": "e", "recommendation": "r", "confidence": 0.8}]}`,
	})
	first := te1.run(t, Options{})
	if first.Outcome != OutcomeWarn {
		t.Fatalf("first outcome = %s, want WARN", first.Outcome)
	}

	// Commit 2: address the warning in a.go.
	writeFile(t, filepath.Join(repo, "a.go"), "package a\n\nfunc A() { /* renamed */ }\n")
	gitIn(t, repo, "commit", "-q", "-am", "address warning")

	// Run 2: spec-keeper (0 warnings in Run 1) is carried via Tier 2b;
	// quality-reviewer (had warning in Run 1) re-runs and clears the warning -> PASS!
	te2 := newTestEngine(t, repo, map[string]any{
		"spec-keeper":      errors.New("spec-keeper must be carried via Tier 2b"),
		"quality-reviewer": clean,
	})
	second := te2.run(t, Options{})
	if second.Outcome != OutcomePass || te2.model.callCount() != 1 || te2.model.calls[0] != "quality-reviewer" {
		t.Fatalf("second outcome = %s, calls = %v", second.Outcome, te2.model.calls)
	}
}

func TestSelectiveCarry_MultiHopTransitiveAndBaseMerge(t *testing.T) {
	gatesYAML := `gates:
  - agent: spec-keeper
    paths: ["**"]
    mode: gate
  - agent: prod-reviewer
    paths: ["**/*.go", "!**/*_test.go"]
    mode: gate
  - agent: test-reviewer
    paths: ["**/*_test.go"]
    mode: gate
`
	repo := setupScopedRepo(t, gatesYAML, []string{"spec-keeper", "prod-reviewer", "test-reviewer"})
	writeFile(t, filepath.Join(repo, "a_test.go"), "package a\n\nfunc TestA() {}\n")
	gitIn(t, repo, "add", "a_test.go")
	gitIn(t, repo, "commit", "-q", "-m", "add test")
	h1 := headOf(t, repo)

	// Run 1 on H1: spec-keeper clean, test-reviewer clean, prod-reviewer blocks on a.go.
	te1 := newTestEngine(t, repo, map[string]any{
		"spec-keeper":   clean,
		"prod-reviewer": `{"summary": "bug", "findings": [{"severity": "high", "category": "correctness", "title": "Bug", "location": {"file": "a.go", "line": 3}, "evidence": "e", "recommendation": "r", "confidence": 0.9}]}`,
		"test-reviewer": clean,
	})
	run1 := te1.run(t, Options{})
	if run1.Outcome != OutcomeBlock {
		t.Fatalf("run1 = %s", run1.Outcome)
	}

	// Commit H2: fix a.go, and also edit a_test.go introducing a blocker in a_test.go.
	writeFile(t, filepath.Join(repo, "a.go"), "package a\n\nfunc A() { /* fixed */ }\n")
	writeFile(t, filepath.Join(repo, "a_test.go"), "package a\n\nfunc TestA() { /* broken */ }\n")
	gitIn(t, repo, "commit", "-q", "-am", "fix prod, touch test")
	h2 := headOf(t, repo)

	// Run 2 on H2:
	// - spec-keeper is carried from Run 1 (Tier 2b: Run 1 had blockers, spec-keeper had 0).
	// - prod-reviewer re-runs (it had a blocker in Run 1) -> now clean on H2!
	// - test-reviewer: wait, in Run 1 test-reviewer had 0 blockers, and test-reviewer has no When condition!
	// Wait: why would test-reviewer re-run on H2 if Tier 2b applies to all gates without When?
	// Let's check! In Run 2, test-reviewer had 0 blockers in Run 1, so Tier 2b carries test-reviewer too!
	// Wait: what if prod-reviewer had a warning on H2 instead? Let's test that!
	te2 := newTestEngine(t, repo, map[string]any{
		"spec-keeper":   errors.New("spec-keeper carried from run1"),
		"prod-reviewer": `{"summary": "warn", "findings": [{"severity": "medium", "category": "readability", "title": "Style", "location": {"file": "a.go", "line": 3}, "evidence": "e", "recommendation": "r", "confidence": 0.8}]}`,
		"test-reviewer": errors.New("test-reviewer carried from run1"),
	})
	run2 := te2.run(t, Options{})
	if run2.Outcome != OutcomeWarn || te2.model.callCount() != 1 {
		t.Fatalf("run2 = %s, calls = %v", run2.Outcome, te2.model.calls)
	}

	// Commit H3: fix the warning in a.go.
	writeFile(t, filepath.Join(repo, "a.go"), "package a\n\nfunc A() { /* clean style */ }\n")
	gitIn(t, repo, "commit", "-q", "-am", "fix style in a.go")
	h3 := headOf(t, repo)

	// Run 3 on H3:
	// - spec-keeper and test-reviewer are carried transitively from Run 1 (H1)!
	// - prod-reviewer re-runs on H3 and passes!
	te3 := newTestEngine(t, repo, map[string]any{
		"spec-keeper":   errors.New("spec-keeper carried from run1"),
		"prod-reviewer": clean,
		"test-reviewer": errors.New("test-reviewer carried from run1"),
	})
	run3 := te3.run(t, Options{})
	if run3.Outcome != OutcomePass || te3.model.callCount() != 1 {
		t.Fatalf("run3 = %s, calls = %v", run3.Outcome, te3.model.calls)
	}
	plan3, check3 := checkRun(t, repo, run3.RunDir)
	if check3.Outcome != gate.Pass {
		t.Fatalf("check3 = %+v", check3)
	}
	if sk, _ := plan3.Agent("spec-keeper"); sk.CarriedFrom == nil || sk.CarriedFrom.Run != run1.RunID || sk.CarriedFrom.Head != h1 {
		t.Errorf("run3 spec-keeper CarriedFrom = %+v, want run1 (%s) at %s", sk.CarriedFrom, run1.RunID, h1)
	}
	if pr, _ := plan3.Agent("prod-reviewer"); pr.CarriedFrom != nil {
		t.Errorf("run3 prod-reviewer CarriedFrom = %+v, want nil (ran on H3)", pr.CarriedFrom)
	}
	_ = h2

	// Now merge main into feature (H4) without changing the branch diff:
	// Full-run carry() carries Run 3 to H4 AND preserves per-agent CarriedFrom on spec-keeper & test-reviewer
	// so `aspect gate check` (checkRun) validates every report against its respective origin range!
	mergeMain(t, repo, "unrelated.txt", "main change\n")
	te4 := newTestEngine(t, repo, nil)
	run4 := te4.run(t, Options{})
	if !run4.Cached || run4.CarriedFrom != run3.RunID || run4.Outcome != OutcomePass {
		t.Fatalf("run4 = %+v", run4)
	}
	plan4, check4 := checkRun(t, repo, run4.RunDir)
	if check4.Outcome != gate.Pass {
		t.Fatalf("check4 after mergeMain on mixed-origin run failed: %+v", check4.Agents)
	}
	if plan4.CarriedFrom == nil || plan4.CarriedFrom.Head != h3 {
		t.Errorf("plan4.CarriedFrom = %+v, want head %s", plan4.CarriedFrom, h3)
	}
	if sk, _ := plan4.Agent("spec-keeper"); sk.CarriedFrom == nil || sk.CarriedFrom.Head != h1 {
		t.Errorf("plan4 spec-keeper CarriedFrom = %+v, want head %s", sk.CarriedFrom, h1)
	}
	if !strings.Contains(run4.Rendered, fmt.Sprintf("_Carried from run `%s` (`%s`)._", run1.RunID, h1[:12])) {
		t.Errorf("run4.Rendered should preserve earlier origin note for spec-keeper:\n%s", run4.Rendered)
	}
}

func TestSelectiveCarry_InvariantsAndInvalidation(t *testing.T) {
	gatesYAML := `gates:
  - agent: security-reviewer
    paths: ["**/*.go"]
    mode: gate
  - agent: red-team
    paths: ["**/*.go"]
    mode: gate
`
	repo := setupScopedRepo(t, gatesYAML, []string{"security-reviewer", "red-team"})

	// 1. Handoff target is never carried even when clean.
	// In Run 1, security-reviewer has a blocker AND hands off to red-team; red-team has 0 findings.
	te1 := newTestEngine(t, repo, map[string]any{
		"security-reviewer": `{"summary": "blocker with handoff", "findings": [{"severity": "high", "category": "security", "title": "Auth", "location": {"file": "a.go", "line": 3}, "evidence": "e", "recommendation": "r", "confidence": 0.9}], "handoffs": [{"to": "red-team", "reason": "check exploitability"}]}`,
		"red-team":          clean,
	})
	run1 := te1.run(t, Options{})
	if run1.Outcome != OutcomeBlock {
		t.Fatalf("run1 = %s", run1.Outcome)
	}

	// Commit 2: fix a.go. Because security-reviewer handed off to red-team in Run 1,
	// red-team MUST NOT be carried via Tier 2b — both agents must re-run.
	writeFile(t, filepath.Join(repo, "a.go"), "package a\n\nfunc A() { /* auth fixed */ }\n")
	gitIn(t, repo, "commit", "-q", "-am", "fix auth")

	te2 := newTestEngine(t, repo, map[string]any{
		"security-reviewer": `{"summary": "still blocked", "findings": [{"severity": "high", "category": "security", "title": "Auth 2", "location": {"file": "a.go", "line": 3}, "evidence": "e", "recommendation": "r", "confidence": 0.9}]}`,
		"red-team":          clean,
	})
	run2 := te2.run(t, Options{})
	if run2.Outcome != OutcomeBlock || te2.model.callCount() != 2 {
		t.Fatalf("handoff target should not be carried: calls = %v", te2.model.calls)
	}

	// 2. Per-agent input hash invalidation:
	// In Run 2, red-team had 0 blockers (and no handoff). Now we edit red-team.md AND fix a.go.
	// Because red-team's agent definition changed, its agentInputHash changed, so red-team must re-run!
	writeFile(t, filepath.Join(repo, ".claude/agents/red-team.md"), "---\nname: red-team\n---\nred-team\n\n### Extra\nupdated instructions\n")
	writeFile(t, filepath.Join(repo, "a.go"), "package a\n\nfunc A() { /* auth 2 fixed */ }\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "update red-team prompt and fix a.go")

	te3 := newTestEngine(t, repo, map[string]any{
		"security-reviewer": clean,
		"red-team":          clean,
	})
	run3 := te3.run(t, Options{})
	if run3.Outcome != OutcomePass || te3.model.callCount() != 2 {
		t.Fatalf("changed agent definition must invalidate carry: calls = %v", te3.model.calls)
	}
}

func TestRunDoesNotPerpetuallyCarryCleanAgentsFromCarriedWarning(t *testing.T) {
	gatesYAML := `gates:
  - agent: docs-reviewer
    paths: ["docs/**"]
    mode: gate
  - agent: go-reviewer
    paths: ["**/*.go"]
    mode: gate
  - agent: broad-reviewer
    paths: ["**"]
    mode: gate
`
	repo := setupScopedRepo(t, gatesYAML, []string{"docs-reviewer", "go-reviewer", "broad-reviewer"})
	writeFile(t, filepath.Join(repo, "docs/guide.md"), "# Guide\n")
	gitIn(t, repo, "add", "docs/guide.md")
	gitIn(t, repo, "commit", "-q", "-m", "add docs")

	// Run 1: go-reviewer blocks on a.go; docs-reviewer has a warning on docs/guide.md;
	// broad-reviewer has a warning on a.go.
	te1 := newTestEngine(t, repo, map[string]any{
		"docs-reviewer":  `{"summary": "warn docs", "findings": [{"severity": "medium", "category": "docs", "title": "Typo", "location": {"file": "docs/guide.md", "line": 1}, "evidence": "e", "recommendation": "r", "confidence": 0.8}]}`,
		"go-reviewer":    `{"summary": "block go", "findings": [{"severity": "high", "category": "correctness", "title": "Bug", "location": {"file": "a.go", "line": 3}, "evidence": "e", "recommendation": "r", "confidence": 0.9}]}`,
		"broad-reviewer": `{"summary": "warn broad", "findings": [{"severity": "low", "category": "readability", "title": "Nit", "location": {"file": "a.go", "line": 3}, "evidence": "e", "recommendation": "r", "confidence": 0.8}]}`,
	})
	run1 := te1.run(t, Options{})
	if run1.Outcome != OutcomeBlock {
		t.Fatalf("run1 = %s, want BLOCK", run1.Outcome)
	}

	// Commit 2: fix a.go only (docs/guide.md is untouched).
	// - docs-reviewer is carried via Tier 1 (its scoped diff on docs/** is identical).
	// - go-reviewer must re-run (had blocker).
	// - broad-reviewer must ALSO re-run (it had a warning and its scoped diff changed, so Tier 2b must not carry it!).
	writeFile(t, filepath.Join(repo, "a.go"), "package a\n\nfunc A() { /* fixed bug and nit */ }\n")
	gitIn(t, repo, "commit", "-q", "-am", "fix bug and nit")

	te2 := newTestEngine(t, repo, map[string]any{
		"docs-reviewer":  errors.New("docs-reviewer must be carried via Tier 1"),
		"go-reviewer":    clean,
		"broad-reviewer": clean,
	})
	run2 := te2.run(t, Options{})
	// Run 2 has OutcomeWarn because docs-reviewer's warning was carried via Tier 1,
	// while both freshly-run agents (go-reviewer and broad-reviewer) reported 0 findings!
	if run2.Outcome != OutcomeWarn || te2.model.callCount() != 2 {
		t.Fatalf("run2 = %s, calls = %v (want go-reviewer and broad-reviewer)", run2.Outcome, te2.model.calls)
	}

	// Commit 3: new edit to a.go.
	// Even though Run 2 had OutcomeWarn, that warning came from a CARRIED agent (docs-reviewer),
	// NOT a freshly-run agent in Run 2! Therefore Run 3 must NOT treat Run 2 as having fresh warnings
	// for Tier 2b — both go-reviewer and broad-reviewer must re-run on Commit 3!
	writeFile(t, filepath.Join(repo, "a.go"), "package a\n\nfunc A() { /* new feature work */ }\n")
	gitIn(t, repo, "commit", "-q", "-am", "new feature work in a.go")

	te3 := newTestEngine(t, repo, map[string]any{
		"docs-reviewer":  errors.New("docs-reviewer still carried via Tier 1"),
		"go-reviewer":    clean,
		"broad-reviewer": clean,
	})
	run3 := te3.run(t, Options{})
	if run3.Outcome != OutcomeWarn || te3.model.callCount() != 2 {
		t.Fatalf("carried warning in run2 must not enable Tier 2b in run3: calls = %v", te3.model.calls)
	}
}

func TestRunDoesNotCarryTier2bWhenNewFileAdded(t *testing.T) {
	gatesYAML := `gates:
  - agent: spec-keeper
    paths: ["**"]
    mode: gate
  - agent: go-reviewer
    paths: ["**/*.go"]
    mode: gate
`
	repo := setupScopedRepo(t, gatesYAML, []string{"spec-keeper", "go-reviewer"})

	// Run 1: go-reviewer blocks on a.go; spec-keeper is clean.
	te1 := newTestEngine(t, repo, map[string]any{
		"spec-keeper": clean,
		"go-reviewer": `{"summary": "bug", "findings": [{"severity": "high", "category": "correctness", "title": "Bug", "location": {"file": "a.go", "line": 3}, "evidence": "e", "recommendation": "r", "confidence": 0.9}]}`,
	})
	run1 := te1.run(t, Options{})
	if run1.Outcome != OutcomeBlock {
		t.Fatalf("run1 = %s", run1.Outcome)
	}

	// Commit 2: fix a.go AND add a brand-new file b.go.
	// Because b.go was not in spec-keeper's Files in Run 1, Tier 2b must NOT carry spec-keeper!
	writeFile(t, filepath.Join(repo, "a.go"), "package a\n\nfunc A() { B() }\n")
	writeFile(t, filepath.Join(repo, "b.go"), "package a\n\nfunc B() {}\n")
	gitIn(t, repo, "add", "a.go", "b.go")
	gitIn(t, repo, "commit", "-q", "-m", "fix a.go and add b.go")

	te2 := newTestEngine(t, repo, map[string]any{
		"spec-keeper": clean,
		"go-reviewer": clean,
	})
	run2 := te2.run(t, Options{})
	if run2.Outcome != OutcomePass || te2.model.callCount() != 2 {
		t.Fatalf("adding a new file must prevent Tier 2b carry: calls = %v", te2.model.calls)
	}
}

func TestTier1DoesNotMatchUnreviewedTier2bDiffHash(t *testing.T) {
	gatesYAML := `gates:
  - agent: spec-keeper
    paths: ["**/*.go"]
    mode: gate
  - agent: adversarial-reviewer
    paths: ["**/*.go"]
    mode: gate
  - agent: docs-reviewer
    paths: ["docs/**"]
    mode: gate
`
	repo := setupScopedRepo(t, gatesYAML, []string{"spec-keeper", "adversarial-reviewer", "docs-reviewer"})
	writeFile(t, filepath.Join(repo, "docs/guide.md"), "# Guide\n")
	gitIn(t, repo, "add", "docs/guide.md")
	gitIn(t, repo, "commit", "-q", "-m", "add docs")

	// Run 1 on H1: adversarial-reviewer blocks on a.go; spec-keeper and docs-reviewer pass.
	te1 := newTestEngine(t, repo, map[string]any{
		"spec-keeper":          clean,
		"adversarial-reviewer": `{"summary": "bug", "findings": [{"severity": "high", "category": "correctness", "title": "Bug", "location": {"file": "a.go", "line": 3}, "evidence": "e", "recommendation": "r", "confidence": 0.9}]}`,
		"docs-reviewer":        clean,
	})
	run1 := te1.run(t, Options{})
	if run1.Outcome != OutcomeBlock {
		t.Fatalf("run1 = %s", run1.Outcome)
	}

	// Commit H2: modify a.go to fix the blocker.
	// Run 2 carries spec-keeper via Tier 2b (so spec-keeper has still only reviewed H1's a.go, not H2's a.go),
	// carries docs-reviewer via Tier 1, and re-runs adversarial-reviewer -> PASS.
	writeFile(t, filepath.Join(repo, "a.go"), "package a\n\nfunc A() { /* fixed on H2 */ }\n")
	gitIn(t, repo, "commit", "-q", "-am", "fix a.go on H2")

	te2 := newTestEngine(t, repo, map[string]any{
		"spec-keeper":          errors.New("spec-keeper carried via Tier 2b"),
		"adversarial-reviewer": clean,
		"docs-reviewer":        errors.New("docs-reviewer carried via Tier 1"),
	})
	run2 := te2.run(t, Options{})
	if run2.Outcome != OutcomePass || te2.model.callCount() != 1 {
		t.Fatalf("run2 = %s, calls = %v", run2.Outcome, te2.model.calls)
	}

	// Commit H3: modify docs/guide.md after a clean PASS.
	// On H3, adversarial-reviewer reviewed H2's a.go (so Tier 1 carries adversarial-reviewer),
	// but spec-keeper only ever reviewed H1's a.go (it skipped H1..H2 via Tier 2b, and inputs.hash
	// preserved H1's diff hash for spec-keeper). Because Run 2 was a clean PASS, Tier 2b is no longer
	// active in Run 3, and Tier 1 must NOT falsely match H2's unreviewed diff hash for spec-keeper!
	writeFile(t, filepath.Join(repo, "docs/guide.md"), "# Updated Guide\n")
	gitIn(t, repo, "commit", "-q", "-am", "update docs on H3")

	te3 := newTestEngine(t, repo, map[string]any{
		"spec-keeper":          clean,
		"adversarial-reviewer": errors.New("adversarial-reviewer reviewed H2 and must be carried via Tier 1"),
		"docs-reviewer":        clean,
	})
	run3 := te3.run(t, Options{})
	if run3.Outcome != OutcomePass || te3.model.callCount() != 2 {
		t.Fatalf("run3 = %s, calls = %v (want spec-keeper and docs-reviewer)", run3.Outcome, te3.model.calls)
	}
}
