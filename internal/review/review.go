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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Pense-Laranja/aspect/internal/gate"
	"github.com/Pense-Laranja/aspect/internal/gitx"
	"github.com/Pense-Laranja/aspect/internal/llm"
	"github.com/Pense-Laranja/aspect/internal/spec"
	"github.com/Pense-Laranja/aspect/internal/speccontext"
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
      "location": {"file": "<path in the head version>", "line": <1-based integer from the L<n>: prefix>},
      "evidence": "<string>",
      "recommendation": "<(1) Code Fix: ... (2) Contract/Stamp/Lock Check: ... (3) Regression Test Anchor: ...>",
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
	// Agent, when set, runs that one planned agent alone, in a run of its
	// own (.review/agent-<UTC timestamp>) that is never reused, never
	// published and never the current run: a single reviewer's verdict must
	// not read as the gates'.
	Agent string
	// Started, when set, is called with the run id once the run's directory
	// exists; a reused run returns at once and does not call it.
	Started func(runID string)
}

// Result is a review's outcome.
type Result struct {
	RunID  string
	RunDir string
	Cached bool // an earlier run of the same diff was reused
	// CarriedFrom is the run whose verdict was carried to this head, when
	// the reused run judged an earlier one (findCachedRun); Plan.CarriedFrom
	// has its range.
	CarriedFrom string
	// CarryError says why a verdict that could have been carried was not,
	// and the change was reviewed afresh instead.
	CarryError string
	PR, PRURL  string
	Plan       *gate.Plan
	// Outcome is PASS, WARN, OVERRIDDEN, BLOCK or ERROR.
	Outcome   string
	Rendered  string // the Markdown result
	Published string // what publish.sh said, when it ran
	Usage     Usage
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

	specOf := e.specContexts(ctx, repo, plan) // before plan.json: it records plan.SpecNote
	inputsHash := e.inputsHash(repo, plan, specOf)

	carryErr := ""
	prefix := runDirPrefix
	if opt.Agent != "" {
		var only []gate.Planned
		var planned []string
		for _, a := range plan.Agents {
			planned = append(planned, a.Agent)
			if a.Agent == opt.Agent {
				only = append(only, a)
			}
		}
		if len(only) == 0 {
			return nil, fmt.Errorf("agent %q is not in this change's gate plan (planned: %s)", opt.Agent, strings.Join(planned, ", "))
		}
		plan.Agents, prefix = only, agentRunPrefix
	}
	if !opt.Force && opt.Agent == "" {
		if dir, carried := e.findCachedRun(ctx, repo, plan, diff, diffHash, inputsHash); dir != "" {
			if carried {
				// A carry is a saving, never a requirement: when it cannot be
				// written, the change is reviewed afresh, and the result says why.
				var err error
				if dir, err = e.carry(repo, dir, plan, diffHash, inputsHash); err != nil {
					carryErr = describe(err)
				}
			}
			if dir != "" {
				return e.reuse(ctx, repo, dir, cfg, plan, pr, opt.Publish)
			}
		}
	}

	runID, dir, err := e.newRunDir(repo, prefix)
	if err != nil {
		return nil, err
	}
	if opt.Started != nil {
		opt.Started(runID)
	}
	if err := writeJSON(filepath.Join(dir, "plan.json"), plan); err != nil {
		return nil, err
	}
	_ = os.WriteFile(filepath.Join(dir, "diff.hash"), []byte(diffHash), 0o644)
	_ = os.WriteFile(filepath.Join(dir, inputsHashFile), []byte(inputsHash), 0o644)

	st := &runStatus{
		dir: dir, runID: runID, baseRef: base, headRef: head, baseSHA: plan.Base, headSHA: plan.Head,
		branch: branch, diffHash: diffHash, pr: pr.Number, prURL: pr.URL, state: stateRunning,
		agents: map[string]*agentStatus{}, startedAt: e.now(), now: e.now, notCurrent: opt.Agent != "",
	}
	st.update(func() {
		for _, a := range plan.Agents {
			s := st.agent(a.Agent)
			s.mode, s.files = a.Mode, a.Files
		}
	})
	res := &Result{RunID: runID, RunDir: dir, PR: pr.Number, PRURL: pr.URL, Plan: plan, CarryError: carryErr}
	if len(plan.Agents) == 0 {
		res.Outcome, res.Rendered = st.finish(OutcomePass, ""), noAgents
		res.Usage = st.usage
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
			e.runAgent(ctx, repo, dir, plan, a, diff, shared, specOf(a), rv, st)
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		st.fail("review pipeline stopped: cancelled")
		return nil, err
	}

	_, _, _, _, u := st.counts()
	duration := e.now().Sub(st.startedAt)
	usageSummary := &gate.UsageSummary{
		CostUSD:          u.CostUSD,
		PromptTokens:     u.PromptTokens,
		CachedTokens:     u.CachedTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		Duration:         duration,
	}

	check, rendered := e.check(cfg, plan, dir, usageSummary)
	published := ""
	if opt.Publish && opt.Agent == "" {
		published = e.publish(ctx, repo, dir, pr.Number)
	}
	outcome := strings.ToUpper(check.Outcome)
	if outcome == "" {
		outcome = OutcomeError // no verdict is not a pass
	}
	res.Outcome = st.finish(outcome, published)
	res.Usage = st.usage
	res.Rendered, res.Published = rendered, published
	return res, nil
}

// specContexts returns what each planned agent is shown of the spec: the
// system's constraints and the rules of the modules its files belong to
// (plan.Agents[i].Modules), with the topics of theirs that cover its files
// (plan.Agents[i].Files), within Config.SpecBudget. Reviewers have no
// tools; this is how a rule written in the spec reaches them. The spec is
// read from the working tree, as the plan's modules were, but only what is
// committed at the change's head, as it is there, reaches a prompt (see
// keepCommitted): a brief or an included file that is not (a .env beside
// the code, or a committed link to one) is never sent. A spec fragment that is not
// committed leaves the reviewers without any of the spec; a brief that is
// not, without that brief. With no spec or a zero budget, every agent is
// shown nothing. Whatever the reviewers were not shown, and why, goes in
// plan.SpecNote, which the rendered result repeats: a review that never saw
// the rules must not look like one that did.
func (e *Engine) specContexts(ctx context.Context, repo string, plan *gate.Plan) func(gate.Planned) string {
	none := func(gate.Planned) string { return "" }
	if e.Config.SpecBudget <= 0 || plan.Spec == "" || plan.Spec == gate.NoSpec {
		return none
	}
	sp, err := spec.Load(filepath.Join(repo, filepath.FromSlash(plan.Spec)))
	if err != nil {
		plan.SpecNote = fmt.Sprintf("spec %s did not load, so the reviewers were shown none of it: %v", plan.Spec, err)
		return none
	}
	if note, showNone := keepCommitted(ctx, repo, plan.Head, sp); note != "" {
		plan.SpecNote = note
		if showNone {
			return none
		}
	}
	return func(a gate.Planned) string {
		text := speccontext.Render(sp, a.Modules, speccontext.Options{Budget: e.Config.SpecBudget, Files: a.Files})
		if text == "" {
			return ""
		}
		return "### Specification of the touched modules (" + plan.Spec + ")\n" +
			"What the project's spec says about the code this change touches. A change that breaks a constraint or " +
			"invariant below is a finding: cite it in spec_ref. This is the spec as it stands in the checkout under review " +
			"(the change's head when that is what is checked out); where the diff edits the spec itself, judge that edit " +
			"too: a rule weakened only so that the code passes is a finding.\n\n" + text
	}
}

// keepCommitted makes sure what the reviewers are shown of sp is what is
// committed at head, byte for byte: each file the spec is assembled from,
// and each brief, must be a regular file at head (not a symbolic link, which
// could lead to an untracked .env in the checkout) whose committed content is
// the content the loader read. A brief that is not is dropped from sp; a file
// of the spec itself that is not means none of the spec may be shown
// (showNone). note says what was not shown and why, "" when everything was.
// When git cannot answer, nothing is shown: the check fails closed.
func keepCommitted(ctx context.Context, repo, head string, sp *spec.Spec) (note string, showNone bool) {
	repoAbs, err := filepath.Abs(repo)
	if err != nil {
		return "could not check the spec against head (" + err.Error() + "), so the reviewers were shown none of it", true
	}
	rel := func(file string) (string, bool) {
		abs, err := filepath.Abs(file)
		if err != nil || !spec.Inside(repoAbs, abs) {
			return "", false
		}
		r, _ := filepath.Rel(repoAbs, abs)
		return filepath.ToSlash(r), true
	}
	var specFiles []string
	for _, f := range sp.Sources.Files() {
		r, ok := rel(f)
		if !ok {
			return "spec file " + f + " is outside the repository under review, so the reviewers were shown none of the spec", true
		}
		specFiles = append(specFiles, r)
	}
	briefs := map[string]string{} // owner -> repository-relative brief file
	var paths []string
	paths = append(paths, specFiles...)
	for owner, b := range sp.Briefs() {
		if b.Path == "" || b.Text == "" {
			continue
		}
		if r, ok := rel(filepath.Join(sp.Dir, filepath.FromSlash(b.Path))); ok {
			briefs[owner] = r
			paths = append(paths, r)
		} else {
			briefs[owner] = ""
		}
	}
	committed, err := committedAsIs(ctx, repo, head, paths)
	if err != nil {
		return "could not check the spec against head (" + err.Error() + "), so the reviewers were shown none of it", true
	}
	var stale []string
	for _, f := range specFiles {
		if !committed[f] {
			stale = append(stale, f)
		}
	}
	if len(stale) > 0 {
		return "spec files not committed at head as they are in the checkout (" + strings.Join(stale, ", ") + "), so the reviewers were shown none of the spec", true
	}
	var dropped []string
	for owner, b := range sp.Briefs() {
		r, has := briefs[owner]
		if !has || committed[r] {
			continue
		}
		b.Text = ""
		dropped = append(dropped, owner+" ("+firstNonEmpty(r, b.Path)+")")
	}
	if len(dropped) == 0 {
		return "", false
	}
	sort.Strings(dropped)
	return "briefs not committed at head as they are in the checkout were not shown: " + strings.Join(dropped, ", "), false
}

// committedAsIs reports, for each repository-relative path, whether head
// holds it as a regular file whose content is the working tree's: asked of
// git for those paths only (ls-tree for the mode and blob, hash-object for
// the file as it is).
func committedAsIs(ctx context.Context, repo, head string, paths []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(paths) == 0 {
		return out, nil
	}
	listing, err := gitx.Run(ctx, repo, append([]string{"ls-tree", "-z", "--full-tree", head, "--"}, paths...)...)
	if err != nil {
		return nil, err
	}
	blob := map[string]string{}
	for _, rec := range strings.Split(listing, "\x00") {
		meta, path, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta) // mode, type, object
		if len(f) == 3 && f[1] == "blob" && (f[0] == "100644" || f[0] == "100755") {
			blob[path] = f[2]
		}
	}
	var present []string
	for _, p := range paths {
		if blob[p] != "" {
			present = append(present, p)
		}
	}
	if len(present) == 0 {
		return out, nil
	}
	hashes, err := gitx.Run(ctx, repo, append([]string{"hash-object", "--no-filters", "--"}, present...)...)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(hashes, "\n")
	if len(lines) != len(present) {
		return nil, fmt.Errorf("git hash-object gave %d hashes for %d files", len(lines), len(present))
	}
	for i, p := range present {
		out[p] = strings.TrimSpace(lines[i]) == blob[p]
	}
	return out, nil
}

func (e *Engine) gatesPath(repo string) string {
	p := filepath.Join(repo, filepath.FromSlash(gate.DefaultConfigPath))
	if _, err := os.Stat(p); err != nil && e.Config.AspectHome != "" {
		return filepath.Join(e.Config.AspectHome, filepath.FromSlash(gate.DefaultConfigPath))
	}
	return p
}

// runDirPrefix names the engine's runs: .review/local-<UTC timestamp>, which
// sort by name in the order they were made (LoadRuns, findCachedRun).
const runDirPrefix = "local-"

// agentRunPrefix names the runs of one agent alone (Options.Agent): kept
// apart from runDirPrefix's, so the cache, the run list and the newest-run
// lookups never take one for the gates' run.
const agentRunPrefix = "agent-"

// newRunDir creates .review/local-<UTC timestamp>, numbered when taken.
func (e *Engine) newRunDir(repo, prefix string) (string, string, error) {
	base := prefix + e.now().UTC().Format("20060102T150405Z")
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
	diff, shared, specContext string, rv *reviewer, st *runStatus) {
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
		specContext:   specContext,
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
	rep, err := Normalize(raw, AgentRun{Agent: a.Agent, Mode: a.Mode, Base: plan.Base, Head: plan.Head})
	if err != nil {
		fail(err.Error())
		return
	}
	if u.CostUSD > 0 || strings.Contains(strings.ToLower(u.Engine), "gemini") {
		side := NewObject()
		side.Set("engine", u.Engine)
		side.Set("cost_usd", roundTo(u.CostUSD, 6))
		side.Set("prompt_tokens", u.PromptTokens)
		side.Set("cached_tokens", u.CachedTokens)
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
func (e *Engine) check(cfg *gate.Config, plan *gate.Plan, dir string, usage *gate.UsageSummary) (*gate.Result, string) {
	inputs := map[string]gate.Input{}
	for _, a := range plan.Agents {
		b, err := os.ReadFile(filepath.Join(dir, a.Agent+".json"))
		if os.IsNotExist(err) {
			continue
		}
		inputs[a.Agent] = gate.Input{Data: b, Err: err}
	}
	res := gate.Check(cfg, plan, inputs, false)
	return res, gate.Render(cfg, plan, res, gate.RenderOptions{Usage: usage})
}

// FormatOutcomeLine formats the outcome banner with token and cost breakdown.
func FormatOutcomeLine(outcome string, u Usage, reused bool) string {
	if reused {
		return fmt.Sprintf("Aspect gate outcome: %s — Cost: $0.0000 (reused)", outcome)
	}
	if u.TotalTokens == 0 && u.CostUSD == 0 {
		return fmt.Sprintf("Aspect gate outcome: %s", outcome)
	}
	var parts []string
	parts = append(parts, fmt.Sprintf("%s prompt", gate.FormatTokens(u.PromptTokens)))
	parts = append(parts, fmt.Sprintf("%s completion", gate.FormatTokens(u.CompletionTokens)))
	if u.CachedTokens > 0 {
		parts = append(parts, fmt.Sprintf("%s cached [75%% discount]", gate.FormatTokens(u.CachedTokens)))
	}
	if u.Duration > 0 {
		parts = append(parts, gate.FormatDuration(u.Duration))
	}
	return fmt.Sprintf("Aspect gate outcome: %s — Cost: %s (%s)", outcome, gate.FormatCost(u.CostUSD), strings.Join(parts, ", "))
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

// prLookupTimeout bounds finding the pull request: a stalled GitHub falls
// back to the branch name instead of holding the review.
const prLookupTimeout = 20 * time.Second

// detectPR finds the pull request under review: the one numbered pr or the
// branch's open one, else a number in the branch name.
func (e *Engine) detectPR(ctx context.Context, repo, branch, pr string) PullRequest {
	find := e.FindPR
	if find == nil {
		find = ghFindPR
	}
	lctx, cancel := context.WithTimeout(ctx, prLookupTimeout)
	defer cancel()
	found, ok := find(lctx, repo, pr)
	if !ok {
		found = PullRequest{Number: pr}
	}
	if found.Number == "" && branch != "" {
		if m := prInBranch.FindStringSubmatch(branch); m != nil {
			found.Number = m[1]
		}
	}
	if found.Number != "" && found.URL == "" {
		if slug := originSlug(ctx, repo); slug != "" {
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

func (v ghPR) pullRequest(url string) PullRequest {
	return PullRequest{Number: v.Number.String(), URL: url, Title: strings.TrimSpace(v.Title), Body: strings.TrimSpace(v.Body)}
}

var prNumber = regexp.MustCompile(`^[0-9]+$`)

// ghFindPR asks the GitHub CLI: `gh pr view`, else the REST API, which a
// proxy that refuses GraphQL (a cloud session's) still answers. A pr that is
// not a number is never looked up.
func ghFindPR(ctx context.Context, repo, pr string) (PullRequest, bool) {
	if pr != "" && !prNumber.MatchString(pr) {
		return PullRequest{}, false
	}
	if p, ok := ghViewPR(ctx, repo, pr); ok {
		return p, true
	}
	return ghAPIPR(ctx, repo, pr)
}

func ghViewPR(ctx context.Context, repo, pr string) (PullRequest, bool) {
	args := []string{"pr", "view"}
	if pr != "" {
		args = append(args, pr)
	}
	out, err := ghOutput(ctx, repo, append(args, "--json", "number,url,title,body")...)
	var v ghPR
	if err != nil || json.Unmarshal(out, &v) != nil || v.Number == "" {
		return PullRequest{}, false
	}
	return v.pullRequest(v.URL), true
}

// ghAPIPR reads the pull request numbered pr, or the open one whose head is
// the current branch, from the REST API.
func ghAPIPR(ctx context.Context, repo, pr string) (PullRequest, bool) {
	slug := originSlug(ctx, repo)
	if slug == "" {
		return PullRequest{}, false
	}
	if pr != "" {
		out, err := ghOutput(ctx, repo, "api", "repos/"+slug+"/pulls/"+pr)
		var v ghPR
		if err != nil || json.Unmarshal(out, &v) != nil || v.Number == "" {
			return PullRequest{}, false
		}
		return v.pullRequest(v.HTMLURL), true
	}
	branch, err := gitx.Run(ctx, repo, "branch", "--show-current")
	if err != nil || branch == "" {
		return PullRequest{}, false
	}
	owner, _, _ := strings.Cut(slug, "/")
	query := url.Values{"state": {"open"}, "head": {owner + ":" + branch}}.Encode()
	out, err := ghOutput(ctx, repo, "api", "repos/"+slug+"/pulls?"+query)
	var list []ghPR
	if err != nil || json.Unmarshal(out, &list) != nil || len(list) == 0 || list[0].Number == "" {
		return PullRequest{}, false
	}
	return list[0].pullRequest(list[0].HTMLURL), true
}

func ghOutput(ctx context.Context, repo string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = repo
	return cmd.Output()
}

// originSlug is owner/name of the origin remote, read off its URL's last two
// path segments: a GitHub URL, or a proxy's URL that ends the same way.
func originSlug(ctx context.Context, repo string) string {
	u, err := gitx.Run(ctx, repo, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	if m := remoteTail.FindStringSubmatch(strings.TrimSpace(u)); m != nil {
		return m[1]
	}
	return ""
}

var remoteTail = regexp.MustCompile(`[/:]([^/:\s]+/[^/:\s]+?)(?:\.git)?/?$`)
