package review

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eideroliveira/aspect/internal/llm"
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
	before  func(agent string) // runs as a request starts
}

func (f *fakeModel) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	agent := roleOf(req.Prompt)
	f.mu.Lock()
	f.calls = append(f.calls, agent)
	answer := f.answers[agent]
	before := f.before
	f.mu.Unlock()
	if before != nil {
		before(agent)
	}
	switch a := answer.(type) {
	case string:
		return llm.Response{Text: a, Model: "flash", InputTokens: 100, OutputTokens: 10}, nil
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
	if second.Rendered != first.Rendered {
		t.Error("the reused result renders differently")
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
