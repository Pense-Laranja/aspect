package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Pense-Laranja/aspect/internal/llm"
	"github.com/Pense-Laranja/aspect/internal/review"
)

// mcpModel answers every agent with a clean report, after gate closes when
// one is set.
type mcpModel struct {
	mu    sync.Mutex
	calls int
	gate  chan struct{}
}

func (m *mcpModel) Complete(ctx context.Context, _ llm.Request) (llm.Response, error) {
	m.mu.Lock()
	m.calls++
	g := m.gate
	m.mu.Unlock()
	if g != nil {
		select {
		case <-g:
		case <-ctx.Done():
			return llm.Response{}, ctx.Err()
		}
	}
	return llm.Response{Text: `{"summary": "checked", "findings": []}`, Model: "flash", InputTokens: 10, OutputTokens: 5}, nil
}

func mcpGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func mcpWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mcpRepo is a repository whose branch changes a.go, with two gate agents.
func mcpRepo(t *testing.T) string {
	t.Helper()
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
	} {
		t.Setenv(k, v)
	}
	repo := t.TempDir()
	mcpGit(t, repo, "init", "-q", "-b", "main")
	mcpWrite(t, filepath.Join(repo, ".claude/review/gates.yaml"), "gates:\n  - agent: security-reviewer\n    paths: [\"**\"]\n    mode: gate\n  - agent: red-team\n    paths: [\"**\"]\n    mode: gate\n")
	for _, a := range []string{"security-reviewer", "red-team"} {
		mcpWrite(t, filepath.Join(repo, ".claude/agents", a+".md"), "---\nname: "+a+"\n---\n"+a+"\n")
	}
	mcpWrite(t, filepath.Join(repo, ".gitignore"), ".review/\n")
	mcpWrite(t, filepath.Join(repo, "a.go"), "package a\n")
	mcpGit(t, repo, "add", "-A")
	mcpGit(t, repo, "commit", "-q", "-m", "base")
	mcpGit(t, repo, "checkout", "-q", "-b", "feature")
	mcpWrite(t, filepath.Join(repo, "a.go"), "package a\n\nfunc A() {}\n")
	mcpGit(t, repo, "commit", "-q", "-am", "head")
	return repo
}

type mcpHarness struct {
	srv     *mcpServer
	model   *mcpModel
	session *mcp.ClientSession
	repo    string
	prs     []string // the pull request numbers the engine was given
	mu      sync.Mutex
}

func newMCPHarness(t *testing.T, key string) *mcpHarness {
	t.Helper()
	h := &mcpHarness{model: &mcpModel{}, repo: mcpRepo(t)}
	cfg := review.Config{DefaultRepo: h.repo, GeminiAPIKey: key, GeminiModel: "flash", PromptBudget: 800_000, MaxConcurrency: 4}
	h.srv = newMCPServer(context.Background(), cfg)
	h.srv.engine = func() *review.Engine {
		return &review.Engine{Config: cfg, Client: h.model, FindPR: func(_ context.Context, _, pr string) (review.PullRequest, bool) {
			h.mu.Lock()
			h.prs = append(h.prs, pr)
			h.mu.Unlock()
			return review.PullRequest{}, false
		}}
	}
	h.srv.check = func(context.Context) (llm.GeminiModelInfo, error) {
		return llm.GeminiModelInfo{Name: "models/flash", InputTokenLimit: 1000}, nil
	}
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := h.srv.server().Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.session = cs
	t.Cleanup(func() {
		cs.Close()
		ss.Close()
		h.srv.stop()
	})
	return h
}

// call calls a tool and returns its text and whether it is an error result.
func (h *mcpHarness) call(t *testing.T, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := h.session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

// The server offers the Python CodeReviewer's tools under their names, so
// the desktop app's configuration only changes the command it starts.
func TestMCPServesCodeReviewersTools(t *testing.T) {
	h := newMCPHarness(t, "k")
	res, err := h.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	for _, want := range []string{"review_with_aspect_gates", "run_single_aspect_agent", "get_review_status",
		"get_review_findings", "compare_review_runs", "list_review_runs", "check_review_engine_status"} {
		if !slices.Contains(names, want) {
			t.Errorf("tool %s missing; have %v", want, names)
		}
	}
}

// pr_id is accepted as the number the desktop app sends, or as a string;
// the Python server refused the number.
func TestMCPReviewTakesPRIDAsNumberOrString(t *testing.T) {
	h := newMCPHarness(t, "k")
	out, isErr := h.call(t, "review_with_aspect_gates", map[string]any{"base_ref": "main", "pr_id": 1744, "publish_to_pr": false})
	if isErr || !strings.Contains(out, "Aspect gate outcome: PASS") || !strings.Contains(out, "**Gates passed.**") {
		t.Fatalf("number pr_id: error=%v\n%s", isErr, out)
	}
	if _, isErr := h.call(t, "review_with_aspect_gates", map[string]any{"base_ref": "main", "pr_id": "#1745", "publish_to_pr": false, "force": true}); isErr {
		t.Fatal("string pr_id was refused")
	}
	h.mu.Lock()
	got := slices.Clone(h.prs)
	h.mu.Unlock()
	if !slices.Equal(got, []string{"1744", "1745"}) {
		t.Errorf("the engine was given pull requests %v, want [1744 1745]", got)
	}
	if out, isErr := h.call(t, "review_with_aspect_gates", map[string]any{"pr_id": "abc"}); !isErr || !strings.Contains(out, "not a pull request number") {
		t.Errorf("pr_id abc: error=%v %q", isErr, out)
	}
}

// A review longer than the wait returns its run id, keeps running after the
// call has returned, and is then read back by status and findings.
func TestMCPReviewOutlivesTheCallThatStartedIt(t *testing.T) {
	h := newMCPHarness(t, "k")
	h.srv.wait = 2 * time.Second // the plan is made in well under that; the model then waits for the gate
	h.model.gate = make(chan struct{})

	out, isErr := h.call(t, "review_with_aspect_gates", map[string]any{"base_ref": "main", "publish_to_pr": false})
	if isErr || !strings.Contains(out, "**IN PROGRESS**") || !strings.Contains(out, "not** a verdict") {
		t.Fatalf("error=%v\n%s", isErr, out)
	}
	runID := between(out, "**Run ID:** `", "`")
	if !strings.HasPrefix(runID, "local-") {
		t.Fatalf("no run id in %q", out)
	}
	if st, _ := h.call(t, "get_review_status", map[string]any{}); !strings.Contains(st, "in flight: 1") || !strings.Contains(st, runID) {
		t.Errorf("status while running:\n%s", st)
	}

	close(h.model.gate)
	deadline := time.Now().Add(10 * time.Second)
	for len(h.srv.inFlight()) > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	r, err := review.LoadRun(h.repo, runID)
	if err != nil || r.Outcome != review.OutcomePass {
		t.Fatalf("after the call returned, the run %s: %+v, %v; want it finished with PASS", runID, r, err)
	}
	if out, isErr := h.call(t, "get_review_findings", map[string]any{"run_id": runID}); isErr || !strings.Contains(out, "No findings in run "+runID) {
		t.Errorf("findings: error=%v %q", isErr, out)
	}
	if out, _ := h.call(t, "list_review_runs", map[string]any{}); !strings.Contains(out, runID) {
		t.Errorf("list_review_runs does not list %s:\n%s", runID, out)
	}
}

// One agent's run says it is not the gates' verdict and does not become
// the repository's current run.
func TestMCPSingleAgentIsNotTheGatesVerdict(t *testing.T) {
	h := newMCPHarness(t, "k")
	out, isErr := h.call(t, "run_single_aspect_agent", map[string]any{"agent_name": "red-team", "base_ref": "main"})
	if isErr || !strings.Contains(out, "not the gates' verdict") || !strings.Contains(out, "**Run ID:** `agent-") {
		t.Fatalf("error=%v\n%s", isErr, out)
	}
	if h.model.calls != 1 {
		t.Errorf("%d model calls, want red-team's alone", h.model.calls)
	}
	if _, err := os.Stat(filepath.Join(h.repo, ".review", "current_run.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("current_run.json written by an agent run (err %v)", err)
	}
	if out, isErr := h.call(t, "run_single_aspect_agent", map[string]any{"agent_name": "architect", "base_ref": "main"}); !isErr || !strings.Contains(out, "not in this change's gate plan") {
		t.Errorf("an agent outside the plan: error=%v %q", isErr, out)
	}
}

func TestMCPRefusalsAreToolErrors(t *testing.T) {
	h := newMCPHarness(t, "")
	if out, isErr := h.call(t, "review_with_aspect_gates", map[string]any{}); !isErr || !strings.Contains(out, "No Gemini API key") {
		t.Errorf("no key: error=%v %q", isErr, out)
	}
	if out, isErr := h.call(t, "compare_review_runs", map[string]any{"run_a": "local-1", "run_b": ""}); !isErr || !strings.Contains(out, "needs both") {
		t.Errorf("one run id: error=%v %q", isErr, out)
	}
	if _, err := h.session.CallTool(context.Background(), &mcp.CallToolParams{Name: "compare_review_runs", Arguments: map[string]any{"run_a": "local-1"}}); err == nil {
		if out, isErr := h.call(t, "compare_review_runs", map[string]any{"run_a": "local-1"}); !isErr || !strings.Contains(out, "run_b") {
			t.Errorf("run_b missing: error=%v %q", isErr, out)
		}
	}
	if out, isErr := h.call(t, "get_review_findings", map[string]any{"severity": "urgent"}); !isErr || !strings.Contains(out, "severity must be one of") {
		t.Errorf("bad severity: error=%v %q", isErr, out)
	}
	if out, _ := h.call(t, "check_review_engine_status", map[string]any{}); !strings.Contains(out, "missing (set GEMINI_API_KEY") || !strings.Contains(out, "not checked: no key") {
		t.Errorf("engine status without a key:\n%s", out)
	}
}

func between(s, start, end string) string {
	_, rest, ok := strings.Cut(s, start)
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(rest, end)
	return v
}
