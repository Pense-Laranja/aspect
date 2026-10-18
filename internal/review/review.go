// Package review runs the review gates' agents on Gemini: it plans which
// agents a change needs (internal/gate), sends each its role prompt and the
// diff, normalizes the reports they answer with, and lets gate.Check decide
// the outcome. Every run leaves .review/<run-id>/ with plan.json, one
// <agent>.json report per agent that reported, <agent>.error.txt for one
// that failed, status.json and diff.hash; .review/current_run.json names the
// latest run.
//
// It replaces CodeReviewer, the Python engine that ran the same agents
// (eideroliveira/CodeReviewer), and keeps its files, environment and exit
// codes.
package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/eideroliveira/aspect/internal/gate"
	"github.com/eideroliveira/aspect/internal/gitx"
	"github.com/eideroliveira/aspect/internal/llm"
)

// maxDescriptionChars bounds the PR description given as context: past
// this it would crowd the diff out of the prompt.
const maxDescriptionChars = 6000

// noAgents is the rendered result of a change no agent applies to.
const noAgents = "No review agents required for this change."

// reportFormat is the report shape, spelled out in the prompt: the model
// does not know aspect-review/v1 by name.
var reportFormat = strings.TrimSpace(fmt.Sprintf(`
Output ONLY one JSON object, exactly these keys and types:
{
  "summary": "<plain-text string, 1-3 sentences; never an object or list>",
  "findings": [
    {
      "id": "<unique string>",
      "severity": "<%s>",
      "category": "<%s>",
      "title": "<string>",
      "location": {"file": "<path in the head version>", "line": <1-based integer>},
      "evidence": "<string>",
      "recommendation": "<string>",
      "confidence": <number from 0 to 1>
    }
  ],
  "handoffs": []
}
Every finding with a severity other than info MUST have a location with a file.
If there are no issues, return "findings": [].
`, strings.Join(gate.Severities, "|"), strings.Join(gate.Categories, "|")))

// PullRequest is the open pull request of the branch under review.
type PullRequest struct {
	Number      string
	URL         string
	Title, Body string
}

// Engine runs reviews.
type Engine struct {
	Config Config
	// Client answers the agents. Engine label and cost are Gemini's.
	Client llm.Client
	// FindPR looks up the branch's open pull request, or the one numbered pr
	// when given. Defaults to `gh pr view`.
	FindPR func(ctx context.Context, repo, pr string) (PullRequest, bool)
	// Now is the clock (tests).
	Now func() time.Time
}

// Options are one review's inputs.
type Options struct {
	Repo    string // repository root
	Base    string // default origin/main; the diff starts at its merge base with Head
	Head    string // default HEAD
	PR      string // pull request number, when known
	Publish bool   // run .claude/review/publish.sh on the result
	Force   bool   // review again even if a run of the same diff exists
}

// Result is a review's outcome.
type Result struct {
	RunID     string
	RunDir    string
	Cached    bool // an earlier run of the same diff was reused
	PR, PRURL string
	Plan      *gate.Plan
	// Outcome is PASS, WARN, OVERRIDDEN, BLOCK or ERROR.
	Outcome   string
	Rendered  string // the Markdown result
	Published string // what publish.sh said, when it ran
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) engineLabel() string {
	return fmt.Sprintf("gemini (%s)", e.Config.GeminiModel)
}

// Run reviews the change from opt.Base to opt.Head in opt.Repo.
func (e *Engine) Run(ctx context.Context, opt Options) (*Result, error) {
	repo, err := filepath.Abs(firstNonEmpty(opt.Repo, e.Config.DefaultRepo, "."))
	if err != nil {
		return nil, err
	}
	base, head := firstNonEmpty(opt.Base, "origin/main"), firstNonEmpty(opt.Head, "HEAD")
	branch, _ := gitx.Run(ctx, repo, "branch", "--show-current")
	pr := e.detectPR(ctx, repo, branch, opt.PR)

	cfg, err := gate.LoadConfig(e.gatesPath(repo))
	if err != nil {
		return nil, err
	}
	plan, err := gitx.Plan(ctx, repo, cfg, base, head)
	if err != nil {
		return nil, err
	}
	diff, err := gitx.Run(ctx, repo, "diff", plan.Base+"..."+plan.Head)
	if err != nil {
		return nil, err
	}
	diff = strings.TrimSpace(diff)
	sum := sha256.Sum256([]byte(diff))
	diffHash := hex.EncodeToString(sum[:])

	if !opt.Force {
		if dir := e.findCachedRun(ctx, repo, plan, diff, diffHash); dir != "" {
			return e.reuse(ctx, repo, dir, cfg, plan, pr, opt.Publish)
		}
	}

	runID, dir, err := e.newRunDir(repo)
	if err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(dir, "plan.json"), plan); err != nil {
		return nil, err
	}
	_ = os.WriteFile(filepath.Join(dir, "diff.hash"), []byte(diffHash), 0o644)

	st := &runStatus{
		dir: dir, runID: runID, baseRef: base, headRef: head, baseSHA: plan.Base, headSHA: plan.Head,
		branch: branch, diffHash: diffHash, pr: pr.Number, prURL: pr.URL, state: stateRunning,
		agents: map[string]*agentStatus{}, startedAt: e.now(), now: e.now,
	}
	st.update(func() {
		for _, a := range plan.Agents {
			s := st.agent(a.Agent)
			s.mode, s.files = a.Mode, a.Files
		}
	})
	res := &Result{RunID: runID, RunDir: dir, PR: pr.Number, PRURL: pr.URL, Plan: plan}
	if len(plan.Agents) == 0 {
		res.Outcome, res.Rendered = st.finish(OutcomePass, ""), noAgents
		return res, nil
	}

	shared := ""
	if pr.Title != "" || pr.Body != "" {
		body := pr.Body
		if len(body) > maxDescriptionChars {
			body = body[:maxDescriptionChars] + "\n[description truncated]"
		}
		shared = strings.TrimRight("### Pull request\nTitle: "+pr.Title+"\n\n"+body, " \n\t")
	}
	rv := &reviewer{client: e.Client, engine: e.engineLabel(), model: e.Config.GeminiModel, promptBudget: e.Config.PromptBudget}
	if e.Config.MaxConcurrency > 0 {
		rv.slots = make(chan struct{}, e.Config.MaxConcurrency)
	}
	var wg sync.WaitGroup
	for _, a := range plan.Agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.runAgent(ctx, repo, dir, plan, a, diff, shared, rv, st)
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		st.fail("review pipeline stopped: cancelled")
		return nil, err
	}

	check, rendered := e.check(cfg, plan, dir)
	published := ""
	if opt.Publish {
		published = e.publish(ctx, repo, dir, pr.Number)
	}
	outcome := strings.ToUpper(check.Outcome)
	if outcome == "" {
		outcome = OutcomeError // no verdict is not a pass
	}
	res.Outcome = st.finish(outcome, published)
	res.Rendered, res.Published = rendered, published
	return res, nil
}

func (e *Engine) gatesPath(repo string) string {
	p := filepath.Join(repo, filepath.FromSlash(gate.DefaultConfigPath))
	if _, err := os.Stat(p); err != nil && e.Config.AspectHome != "" {
		return filepath.Join(e.Config.AspectHome, filepath.FromSlash(gate.DefaultConfigPath))
	}
	return p
}

// newRunDir creates .review/local-<UTC timestamp>, numbered when taken.
func (e *Engine) newRunDir(repo string) (string, string, error) {
	base := "local-" + e.now().UTC().Format("20060102T150405Z")
	if err := os.MkdirAll(filepath.Join(repo, ".review"), 0o755); err != nil {
		return "", "", err
	}
	id := base
	for n := 1; ; n++ {
		dir := filepath.Join(repo, ".review", id)
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return id, dir, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", "", err
		}
		id = fmt.Sprintf("%s.%d", base, n)
	}
}

// runAgent runs one planned agent and records its report, or its failure.
func (e *Engine) runAgent(ctx context.Context, repo, dir string, plan *gate.Plan, a gate.Planned,
	diff, shared string, rv *reviewer, st *runStatus) {
	out := filepath.Join(dir, a.Agent+".json")
	fail := func(msg string) {
		// No <agent>.json: `aspect gate check` blocks a gate whose report
		// is missing, while a well-formed report with no findings would
		// pass it. The reason goes in a side file.
		_ = os.Remove(out)
		_ = os.WriteFile(filepath.Join(dir, a.Agent+".error.txt"), []byte(msg+"\n"), 0o644)
		st.failed(a.Agent, msg)
	}
	write := func(rep *Report, u usage) {
		if err := writeJSON(out, rep); err != nil {
			fail("writing the report: " + err.Error())
			return
		}
		st.completed(a.Agent, rep, u)
	}
	st.queued(a.Agent)

	agentDiff := diff
	if len(a.Files) > 0 {
		args := append([]string{"--literal-pathspecs", "diff", plan.Base + "..." + plan.Head, "--"}, a.Files...)
		d, err := gitx.Run(ctx, repo, args...)
		if err != nil {
			fail(err.Error())
			return
		}
		agentDiff = strings.TrimSpace(d)
	}
	if agentDiff == "" {
		write(emptyReport(a.Agent, a.Mode, plan.Base, plan.Head, "No changes matching this agent's scoped paths."), usage{})
		return
	}
	def, ok := e.agentDefinition(repo, a.Agent)
	if !ok {
		if a.Mode == "gate" {
			// A gate nobody can run has not passed: leave no report.
			fail(fmt.Sprintf("agent definition %s.md not found", a.Agent))
			return
		}
		write(emptyReport(a.Agent, a.Mode, plan.Base, plan.Head, fmt.Sprintf("Agent %s definition missing; skipped.", a.Agent)), usage{})
		return
	}

	// The change block of PROTOCOL.md. The diff is already scoped to the
	// agent's files, so no file list is repeated.
	assignment := strings.Join([]string{
		"### Your review assignment", "```",
		"agent: " + a.Agent, "base: " + plan.Base, "head: " + plan.Head,
		"spec: " + firstNonEmpty(plan.Spec, gate.NoSpec), "mode: " + a.Mode, "```",
	}, "\n")
	req := request{
		agent:         a.Agent,
		instructions:  promptBody(def),
		outputFormat:  "Produce your report strictly conforming to the `aspect-review/v1` schema.\n" + reportFormat,
		diff:          agentDiff,
		sharedContext: shared,
		agentContext:  assignment,
	}
	raw, u, err := rv.review(ctx, req, hooks{
		planned:  func(parts int) { st.planned(a.Agent, rv.engine, parts) },
		waiting:  func() { st.waiting(a.Agent) },
		started:  func() { st.started(a.Agent) },
		partDone: func(done, total int) { st.partDone(a.Agent, done, total) },
	})
	if err != nil {
		fail(describe(err))
		return
	}
	rep, err := Normalize(raw, a.Agent, a.Mode, plan.Base, plan.Head)
	if err != nil {
		fail(err.Error())
		return
	}
	if u.CostUSD > 0 || strings.Contains(strings.ToLower(u.Engine), "gemini") {
		side := NewObject()
		side.Set("engine", u.Engine)
		side.Set("cost_usd", roundTo(u.CostUSD, 6))
		side.Set("prompt_tokens", u.PromptTokens)
		side.Set("completion_tokens", u.CompletionTokens)
		side.Set("total_tokens", u.TotalTokens)
		_ = writeJSON(filepath.Join(dir, a.Agent+".usage.json"), side)
	}
	write(rep, u)
}

// describe is a never-empty one-line reason for err.
func describe(err error) string {
	if s := strings.TrimSpace(err.Error()); s != "" {
		return s
	}
	return fmt.Sprintf("%T (no message)", err)
}

// agentDefinition reads .claude/agents/<agent>.md from the repository, else
// from ASPECT_HOME.
func (e *Engine) agentDefinition(repo, agent string) (string, bool) {
	if agent == "" || filepath.Base(agent) != agent || strings.HasPrefix(agent, ".") {
		return "", false
	}
	dirs := []string{repo}
	if e.Config.AspectHome != "" {
		dirs = append(dirs, e.Config.AspectHome)
	}
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(d, ".claude", "agents", agent+".md"))
		if err == nil {
			return string(b), true
		}
	}
	return "", false
}

// promptBody is an agent definition without its front matter.
func promptBody(md string) string {
	if !strings.HasPrefix(md, "---") {
		return md
	}
	parts := strings.SplitN(md, "---", 3)
	if len(parts) < 3 {
		return md
	}
	return strings.TrimSpace(parts[2])
}

// check validates the run's reports against its plan and renders the
// result.
func (e *Engine) check(cfg *gate.Config, plan *gate.Plan, dir string) (*gate.Result, string) {
	inputs := map[string]gate.Input{}
	for _, a := range plan.Agents {
		b, err := os.ReadFile(filepath.Join(dir, a.Agent+".json"))
		if os.IsNotExist(err) {
			continue
		}
		inputs[a.Agent] = gate.Input{Data: b, Err: err}
	}
	res := gate.Check(cfg, plan, inputs, false)
	return res, gate.Render(cfg, plan, res, gate.RenderOptions{})
}

// publish runs .claude/review/publish.sh on the run, when the repository
// has one, and returns what it said.
func (e *Engine) publish(ctx context.Context, repo, dir, pr string) string {
	script := filepath.Join(repo, ".claude", "review", "publish.sh")
	if _, err := os.Stat(script); err != nil {
		return ""
	}
	args := []string{script, dir}
	if pr != "" {
		args = append(args, "--pr", pr)
	}
	cmd := exec.CommandContext(ctx, "bash", args...)
	cmd.Dir = repo
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if s := strings.TrimSpace(stdout.String()); s != "" {
			return s
		}
		return strings.TrimSpace(stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

var prInBranch = regexp.MustCompile(`(?i)(?:PR\s*#?|pr-?|-pr)(\d+)`)

// detectPR finds the pull request under review: the one numbered pr or the
// branch's open one, else a number in the branch name.
func (e *Engine) detectPR(ctx context.Context, repo, branch, pr string) PullRequest {
	find := e.FindPR
	if find == nil {
		find = ghFindPR
	}
	found, ok := find(ctx, repo, pr)
	if !ok {
		found = PullRequest{Number: pr}
	}
	if found.Number == "" && branch != "" {
		if m := prInBranch.FindStringSubmatch(branch); m != nil {
			found.Number = m[1]
		}
	}
	if found.Number != "" && found.URL == "" {
		if slug := githubSlug(ctx, repo); slug != "" {
			found.URL = fmt.Sprintf("https://github.com/%s/pull/%s", slug, found.Number)
		}
	}
	return found
}

// ghPR is a pull request as `gh pr view --json` (url) or the REST API
// (html_url) gives it.
type ghPR struct {
	Number  json.Number `json:"number"`
	URL     string      `json:"url"`
	HTMLURL string      `json:"html_url"`
	Title   string      `json:"title"`
	Body    string      `json:"body"`
}

// ghFindPR asks the GitHub CLI: `gh pr view`, else the REST API, which a
// proxy that refuses GraphQL (a cloud session's) still answers.
func ghFindPR(ctx context.Context, repo, pr string) (PullRequest, bool) {
	args := []string{"pr", "view"}
	if pr != "" {
		args = append(args, pr)
	}
	var v ghPR
	if out, err := ghOutput(ctx, repo, append(args, "--json", "number,url,title,body")...); err == nil && json.Unmarshal(out, &v) == nil && v.Number != "" {
		return PullRequest{Number: v.Number.String(), URL: v.URL, Title: strings.TrimSpace(v.Title), Body: strings.TrimSpace(v.Body)}, true
	}
	slug := remoteSlug(ctx, repo)
	if slug == "" {
		return PullRequest{}, false
	}
	path := "repos/" + slug + "/pulls/" + pr
	if pr == "" {
		branch, err := gitx.Run(ctx, repo, "branch", "--show-current")
		if err != nil || branch == "" {
			return PullRequest{}, false
		}
		owner, _, _ := strings.Cut(slug, "/")
		path = "repos/" + slug + "/pulls?state=open&head=" + owner + ":" + branch
	}
	out, err := ghOutput(ctx, repo, "api", path)
	if err != nil {
		return PullRequest{}, false
	}
	if pr == "" {
		var list []json.RawMessage
		if json.Unmarshal(out, &list) != nil || len(list) == 0 {
			return PullRequest{}, false
		}
		out = list[0]
	}
	v = ghPR{}
	if json.Unmarshal(out, &v) != nil || v.Number == "" {
		return PullRequest{}, false
	}
	return PullRequest{Number: v.Number.String(), URL: v.HTMLURL, Title: strings.TrimSpace(v.Title), Body: strings.TrimSpace(v.Body)}, true
}

func ghOutput(ctx context.Context, repo string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = repo
	return cmd.Output()
}

// remoteSlug is owner/name of the origin remote, read off its URL's last two
// path segments: a GitHub URL, or a proxy's URL that ends the same way.
func remoteSlug(ctx context.Context, repo string) string {
	url, err := gitx.Run(ctx, repo, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	if m := remoteTail.FindStringSubmatch(strings.TrimSpace(url)); m != nil {
		return m[1]
	}
	return ""
}

var remoteTail = regexp.MustCompile(`[/:]([^/:\s]+/[^/:\s]+?)(?:\.git)?/?$`)

var githubRemote = regexp.MustCompile(`github\.com[:/]([^/\s]+/[^/\s]+?)(?:\.git)?/?$`)

// githubSlug is owner/name of the repository's GitHub origin, or "".
func githubSlug(ctx context.Context, repo string) string {
	url, err := gitx.Run(ctx, repo, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	if m := githubRemote.FindStringSubmatch(strings.TrimSpace(url)); m != nil {
		return m[1]
	}
	return ""
}
