package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Pense-Laranja/aspect/internal/gate"
	"github.com/Pense-Laranja/aspect/internal/llm"
	"github.com/Pense-Laranja/aspect/internal/review"
)

// mcpReviewWait is how long a review call waits for the verdict before it
// returns the run id to poll: an MCP client cancels a request after about
// a minute, and a full review takes several.
const mcpReviewWait = 40 * time.Second

// mcpStopWait bounds how long the server waits, at shutdown, for the
// reviews it cancels to record themselves as stopped.
const mcpStopWait = 10 * time.Second

// runMCP serves the review engine over MCP on stdin and stdout: `aspect mcp`.
// It replaces the Python CodeReviewer's MCP server (the desktop app's
// aspect-code-reviewer) with the same tool names and arguments, run by the
// engine of this binary, so a review started from a chat is judged exactly
// as `aspect review` would judge it.
func runMCP(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("mcp: takes no arguments, got %v (configure it with the environment, as aspect review)", args)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s := newMCPServer(ctx, review.ConfigFromEnv())
	defer s.stop()
	return s.server().Run(ctx, &mcp.StdioTransport{})
}

// mcpServer holds what the tools share: the configuration, the reviews in
// flight, and the server's own context, which a review runs under so that a
// client cancelling a call ends the call, never the review.
type mcpServer struct {
	cfg    review.Config
	engine func() *review.Engine
	check  func(context.Context) (llm.GeminiModelInfo, error)
	wait   time.Duration

	ctx     context.Context
	cancel  context.CancelFunc
	reviews sync.WaitGroup

	mu     sync.Mutex
	active map[string]string // run id → repository, for the reviews in flight
}

func newMCPServer(parent context.Context, cfg review.Config) *mcpServer {
	ctx, cancel := context.WithCancel(parent)
	return &mcpServer{
		cfg:    cfg,
		engine: func() *review.Engine { return &review.Engine{Config: cfg, Client: cfg.Gemini()} },
		check:  func(ctx context.Context) (llm.GeminiModelInfo, error) { return cfg.Gemini().CheckModel(ctx) },
		wait:   mcpReviewWait,
		ctx:    ctx,
		cancel: cancel,
		active: map[string]string{},
	}
}

// stop cancels the reviews still running, which records each as stopped,
// and waits a little for them to do so.
func (s *mcpServer) stop() {
	s.cancel()
	done := make(chan struct{})
	go func() { s.reviews.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(mcpStopWait):
	}
}

func (s *mcpServer) server() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "aspect", Version: binaryRevision()}, &mcp.ServerOptions{
		Instructions: "Aspect's review engine: runs the repository's review gates (gates.yaml, .claude/agents) on " +
			"Google Gemini, records each run in the repository's .review/, and reads the runs back. " +
			"A review takes minutes: review_with_aspect_gates returns the run id when it is not done within " +
			"about 40 s; poll get_review_status, then read get_review_findings. Pass repo_path as the absolute " +
			"path of the worktree to review.",
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "review_with_aspect_gates", Description: "Run the repository's Aspect review gates on Gemini: every applicable agent of .claude/agents, the reports in .review/, the gates of gates.yaml, and by default the result published to the branch's open pull request. Waits about 40 s for the verdict; otherwise returns the run id, and the review keeps running: poll get_review_status until the run is no longer running, then read get_review_findings(run_id=...). Not a verdict until it says PASS, WARN or OVERRIDDEN."}, s.review)
	mcp.AddTool(srv, &mcp.Tool{Name: "run_single_aspect_agent", Description: "Run one agent of the change's gate plan alone, in a run of its own (.review/agent-<time>): never published, never reused, never the current run, so it is not the gates' verdict. Waits about 40 s like review_with_aspect_gates."}, s.singleAgent)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_review_status", Description: "A review run's agents, state and findings tally, live while it runs: the run named, else the repository's current run, plus its recent runs and the reviews this server has in flight."}, s.status)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_review_findings", Description: "A run's findings (title, severity, file, line, recommendation), optionally of one severity or one agent: the run named, else the repository's current run."}, s.findings)
	mcp.AddTool(srv, &mcp.Tool{Name: "compare_review_runs", Description: "Two runs of the repository side by side: outcome, cost, and each agent's findings."}, s.compare)
	mcp.AddTool(srv, &mcp.Tool{Name: "list_review_runs", Description: "The repository's review runs, newest first, with their outcome, cost and findings."}, s.runs)
	mcp.AddTool(srv, &mcp.Tool{Name: "check_review_engine_status", Description: "Whether Gemini answers with the configured key and model, the engine's settings, the reviews in flight, and the aspect commit this server was built at."}, s.engineStatus)
	return srv
}

type reviewIn struct {
	RepoPath    string `json:"repo_path,omitempty" jsonschema:"absolute path of the repository or worktree to review (default: CODE_REVIEWER_DEFAULT_REPO, else the server's working directory)"`
	BaseRef     string `json:"base_ref,omitempty" jsonschema:"base ref; the diff starts at its merge base with head (default: origin/main)"`
	HeadRef     string `json:"head_ref,omitempty" jsonschema:"head ref (default: HEAD)"`
	PublishToPR *bool  `json:"publish_to_pr,omitempty" jsonschema:"publish the result to the open pull request: the sticky comment and the plan and gate statuses (default: true)"`
	PRID        any    `json:"pr_id,omitempty" jsonschema:"pull request number, as a number or a string (default: the branch's open pull request)"`
	Force       bool   `json:"force,omitempty" jsonschema:"review again even if a run of the same diff exists"`
}

type agentIn struct {
	AgentName string `json:"agent_name" jsonschema:"the agent, as named in .claude/agents without .md (e.g. adversarial-reviewer); it must be in the change's gate plan"`
	RepoPath  string `json:"repo_path,omitempty" jsonschema:"absolute path of the repository or worktree (default: CODE_REVIEWER_DEFAULT_REPO, else the server's working directory)"`
	BaseRef   string `json:"base_ref,omitempty" jsonschema:"base ref (default: origin/main)"`
	HeadRef   string `json:"head_ref,omitempty" jsonschema:"head ref (default: HEAD)"`
}

type runIn struct {
	RepoPath string `json:"repo_path,omitempty" jsonschema:"absolute path of the repository whose .review/ to read (default: CODE_REVIEWER_DEFAULT_REPO, else the server's working directory)"`
	RunID    string `json:"run_id,omitempty" jsonschema:"run id, e.g. local-20261029T101335Z (default: the repository's current run)"`
}

type findingsIn struct {
	RepoPath string `json:"repo_path,omitempty" jsonschema:"absolute path of the repository whose .review/ to read"`
	RunID    string `json:"run_id,omitempty" jsonschema:"run id (default: the repository's current run)"`
	Severity string `json:"severity,omitempty" jsonschema:"only this severity: critical, high, medium, low or info"`
	Agent    string `json:"agent,omitempty" jsonschema:"only this agent's findings, e.g. security-red-team"`
}

type compareIn struct {
	RepoPath string `json:"repo_path,omitempty" jsonschema:"absolute path of the repository whose .review/ to read"`
	RunA     string `json:"run_a" jsonschema:"the first run id"`
	RunB     string `json:"run_b" jsonschema:"the second run id"`
}

type runsIn struct {
	RepoPath string `json:"repo_path,omitempty" jsonschema:"absolute path of the repository whose .review/ to read"`
	Limit    int    `json:"limit,omitempty" jsonschema:"how many runs, newest first (default: 15)"`
}

type noIn struct{}

// text is a tool result of Markdown text.
func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// failed is a tool result that reports an error to the model, which can act
// on it, instead of failing the call.
func failed(format string, a ...any) *mcp.CallToolResult {
	r := text(fmt.Sprintf(format, a...))
	r.IsError = true
	return r
}

func (s *mcpServer) repo(path string) string {
	if path != "" {
		return path
	}
	return s.cfg.DefaultRepo
}

// prNumber reads pr_id, which clients send as a number or a string (the
// Python server took only a string and refused the desktop app's number).
func prNumber(v any) (string, error) {
	switch p := v.(type) {
	case nil:
		return "", nil
	case string:
		p = strings.TrimPrefix(strings.TrimSpace(p), "#")
		if p == "" {
			return "", nil
		}
		if n, err := strconv.Atoi(p); err != nil || n <= 0 {
			return "", fmt.Errorf("pr_id %q is not a pull request number", p)
		}
		return p, nil
	case float64:
		if p <= 0 || p != float64(int64(p)) {
			return "", fmt.Errorf("pr_id %v is not a pull request number", p)
		}
		return strconv.FormatInt(int64(p), 10), nil
	case json.Number:
		return prNumber(p.String())
	}
	return "", fmt.Errorf("pr_id must be a number or a string, got %T", v)
}

func (s *mcpServer) review(ctx context.Context, _ *mcp.CallToolRequest, in reviewIn) (*mcp.CallToolResult, any, error) {
	pr, err := prNumber(in.PRID)
	if err != nil {
		return failed("%v", err), nil, nil
	}
	if s.cfg.GeminiAPIKey == "" {
		return failed("No Gemini API key: set GEMINI_API_KEY or GOOGLE_API_KEY for the server (or export it in ~/.zshrc), then restart it."), nil, nil
	}
	opt := review.Options{
		Repo: s.repo(in.RepoPath), Base: in.BaseRef, Head: in.HeadRef, PR: pr,
		Publish: in.PublishToPR == nil || *in.PublishToPR, Force: in.Force,
	}
	return s.start(ctx, opt, formatGatesResult)
}

func (s *mcpServer) singleAgent(ctx context.Context, _ *mcp.CallToolRequest, in agentIn) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.AgentName) == "" {
		return failed("agent_name is required"), nil, nil
	}
	if s.cfg.GeminiAPIKey == "" {
		return failed("No Gemini API key: set GEMINI_API_KEY or GOOGLE_API_KEY for the server, then restart it."), nil, nil
	}
	opt := review.Options{Repo: s.repo(in.RepoPath), Base: in.BaseRef, Head: in.HeadRef, Agent: in.AgentName}
	return s.start(ctx, opt, formatAgentResult)
}

type runOutcome struct {
	res *review.Result
	err error
}

// start runs a review under the server's context and waits up to s.wait
// for it: the result, else the run id to poll. The call ending (a client
// timeout or cancel) ends only the wait.
func (s *mcpServer) start(ctx context.Context, opt review.Options, format func(*review.Result) string) (*mcp.CallToolResult, any, error) {
	var mu sync.Mutex
	runID := ""
	opt.Started = func(id string) {
		mu.Lock()
		runID = id
		mu.Unlock()
		s.track(id, opt.Repo)
	}
	done := make(chan runOutcome, 1)
	s.reviews.Add(1)
	go func() {
		defer s.reviews.Done()
		res, err := s.engine().Run(s.ctx, opt)
		mu.Lock()
		id := runID
		mu.Unlock()
		s.untrack(id)
		done <- runOutcome{res, err}
	}()
	timer := time.NewTimer(s.wait)
	defer timer.Stop()
	select {
	case o := <-done:
		if o.err != nil {
			return failed("The review did not run: %v", o.err), nil, nil
		}
		return text(format(o.res)), nil, nil
	case <-timer.C:
		mu.Lock()
		id := runID
		mu.Unlock()
		return text(formatInProgress(id, s.cfg.GeminiModel, s.wait)), nil, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

func (s *mcpServer) track(id, repo string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active[id] = repo
}

func (s *mcpServer) untrack(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, id)
}

func (s *mcpServer) inFlight() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := make(map[string]string, len(s.active))
	for k, v := range s.active {
		m[k] = v
	}
	return m
}

func formatInProgress(runID, model string, waited time.Duration) string {
	ref, findings := "(not assigned yet: the gate plan is still being computed)", "get_review_findings(run_id=...)"
	if runID != "" {
		ref, findings = "`"+runID+"`", "get_review_findings(run_id='"+runID+"')"
	}
	return strings.Join([]string{
		"# Aspect review: **IN PROGRESS**",
		fmt.Sprintf("**Run ID:** %s | **Engine:** Gemini `%s`", ref, model),
		"",
		fmt.Sprintf("The review is still running (it did not finish within %s; a full review takes several minutes). This is **not** a verdict.", waited),
		"",
		"1. Poll `get_review_status(run_id=...)` every minute or two until the run is no longer running.",
		"2. Call `" + findings + "` for the outcome and the findings.",
		"",
		"> [!CAUTION]",
		"> Do not merge until the run has finished with a passing outcome.",
	}, "\n")
}

func outcomeHeader(res *review.Result) []string {
	reused := res.Cached || res.CarriedFrom != ""
	lines := []string{"# " + review.FormatOutcomeLine(res.Outcome, res.Usage, reused), fmt.Sprintf("**Run ID:** `%s`", res.RunID)}
	switch {
	case res.CarriedFrom != "":
		lines = append(lines, fmt.Sprintf("Only the base moved since run `%s` (same diff, agents and spec): its verdict is carried to this head.", res.CarriedFrom))
	case res.Cached:
		lines = append(lines, "Diff unchanged since this run: returning its results.")
	}
	if res.CarryError != "" {
		lines = append(lines, "The earlier verdict could not be carried, so the change was reviewed again: "+res.CarryError)
	}
	return lines
}

func formatGatesResult(res *review.Result) string {
	lines := outcomeHeader(res)
	if res.Published != "" {
		lines = append(lines, fmt.Sprintf("**Published:** `%s`", res.Published))
	}
	lines = append(lines, "", res.Rendered, "")
	switch res.Outcome {
	case review.OutcomePass:
		lines = append(lines, "> [!TIP]", "> **Gates passed.** The review clears the change for merge (with the local CI).")
	case review.OutcomeWarn, review.OutcomeOverridden:
		lines = append(lines, "> [!NOTE]", "> **Gates passed with warnings.** Read them before merging.")
	case review.OutcomeBlock:
		lines = append(lines, "> [!CAUTION]", "> **Blocked:** resolve the blocking findings before merging.")
	default:
		lines = append(lines, "> [!CAUTION]", "> **No verdict:** a gate agent produced no report, so the gates did not pass. Run the review again; do not merge on this run.")
	}
	return strings.Join(lines, "\n")
}

func formatAgentResult(res *review.Result) string {
	lines := outcomeHeader(res)
	lines = append(lines, "", res.Rendered, "",
		"> [!NOTE]",
		"> One agent's run: not the gates' verdict, not published, and not the repository's current run. Run review_with_aspect_gates for the verdict.")
	return strings.Join(lines, "\n")
}

func (s *mcpServer) status(_ context.Context, _ *mcp.CallToolRequest, in runIn) (*mcp.CallToolResult, any, error) {
	repo := s.repo(in.RepoPath)
	var b bytes.Buffer
	if flight := s.inFlight(); len(flight) > 0 {
		fmt.Fprintf(&b, "Reviews this server has in flight: %d\n", len(flight))
		for id, r := range flight {
			fmt.Fprintf(&b, "- `%s` in %s\n", id, r)
		}
		b.WriteString("\n")
	}
	r, err := review.LoadRun(repo, in.RunID)
	if err != nil {
		if b.Len() > 0 {
			fmt.Fprintf(&b, "No run to show in %s: %v\n", repo, err)
			return text(b.String()), nil, nil
		}
		return failed("No run to show in %s: %v", repo, err), nil, nil
	}
	review.PrintStatus(&b, r)
	if in.RunID == "" {
		if runs, err := review.LoadRuns(repo, 5); err == nil && len(runs) > 1 {
			b.WriteString("\nRecent runs:\n")
			review.PrintRuns(&b, runs)
		}
	}
	return text(b.String()), nil, nil
}

func (s *mcpServer) findings(_ context.Context, _ *mcp.CallToolRequest, in findingsIn) (*mcp.CallToolResult, any, error) {
	if !review.ValidSeverity(in.Severity) {
		return failed("severity must be one of %v, got %q", gate.Severities, in.Severity), nil, nil
	}
	repo := s.repo(in.RepoPath)
	r, err := review.LoadRun(repo, in.RunID)
	if err != nil {
		return failed("No run to read in %s: %v", repo, err), nil, nil
	}
	var b bytes.Buffer
	if review.PrintFindings(&b, r, in.Severity, in.Agent) == 0 {
		fmt.Fprintf(&b, "No findings in run %s match.\n", r.RunID)
	}
	return text(b.String()), nil, nil
}

func (s *mcpServer) compare(_ context.Context, _ *mcp.CallToolRequest, in compareIn) (*mcp.CallToolResult, any, error) {
	if in.RunA == "" || in.RunB == "" {
		return failed("compare_review_runs needs both run_a and run_b"), nil, nil
	}
	repo := s.repo(in.RepoPath)
	a, err := review.LoadRun(repo, in.RunA)
	if err != nil {
		return failed("run_a: %v", err), nil, nil
	}
	b2, err := review.LoadRun(repo, in.RunB)
	if err != nil {
		return failed("run_b: %v", err), nil, nil
	}
	var b bytes.Buffer
	review.PrintCompare(&b, a, b2)
	return text(b.String()), nil, nil
}

func (s *mcpServer) runs(_ context.Context, _ *mcp.CallToolRequest, in runsIn) (*mcp.CallToolResult, any, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 15
	}
	repo := s.repo(in.RepoPath)
	runs, err := review.LoadRuns(repo, limit)
	if err != nil {
		return failed("Reading the runs of %s: %v", repo, err), nil, nil
	}
	if len(runs) == 0 {
		return text(fmt.Sprintf("No review runs in %s/.review.", repo)), nil, nil
	}
	var b bytes.Buffer
	review.PrintRuns(&b, runs)
	return text(b.String()), nil, nil
}

func (s *mcpServer) engineStatus(ctx context.Context, _ *mcp.CallToolRequest, _ noIn) (*mcp.CallToolResult, any, error) {
	c := s.cfg
	api := ""
	switch info, err := s.check(ctx); {
	case c.GeminiAPIKey == "":
		api = "not checked: no key"
	case err != nil:
		api = "unreachable: " + err.Error()
	case info.InputTokenLimit > 0:
		api = fmt.Sprintf("reachable (%s, %s-token input limit)", firstNonEmptyStr(info.Name, c.GeminiModel), strconv.Itoa(info.InputTokenLimit))
	default:
		api = "reachable (" + firstNonEmptyStr(info.Name, c.GeminiModel) + ")"
	}
	key := "configured"
	if c.GeminiAPIKey == "" {
		key = "missing (set GEMINI_API_KEY or GOOGLE_API_KEY)"
	}
	concurrency := "no cap"
	if c.MaxConcurrency > 0 {
		concurrency = strconv.Itoa(c.MaxConcurrency)
	}
	lines := []string{
		"# Aspect review engine",
		"- **Engine:** Gemini `" + c.GeminiModel + "`",
		"- **Gemini API:** " + api,
		"- **API key:** " + key,
		"- **Requests in flight per run:** " + concurrency,
		fmt.Sprintf("- **Request timeout:** %s", c.Timeout),
		fmt.Sprintf("- **Retries:** %d on HTTP 429/5xx (backoff from %s); %d on a blocked prompt", c.MaxRetries, c.RetryBase, c.BlockRetries),
		fmt.Sprintf("- **Prompt budget:** %d tokens per request; spec budget %d", c.PromptBudget, c.SpecBudget),
		"- **Default repository:** `" + c.DefaultRepo + "`",
		fmt.Sprintf("- **Reviews in flight:** %d", len(s.inFlight())),
		"- **Built at aspect commit:** `" + binaryRevision() + "` (a repository's .claude/review/aspect.lock names the commit its reviews must run on)",
	}
	return text(strings.Join(lines, "\n")), nil, nil
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// binaryRevision is the commit Go stamped in this binary, with "+dirty"
// when it was built from a changed tree; "unknown" without VCS stamps.
func binaryRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	if dirty {
		rev += "+dirty"
	}
	return rev
}
