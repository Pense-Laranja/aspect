// Package pipeline runs the agents over a plan: for each module in dependency
// order it asks the Coder for code, the Tester for tests, runs them for real,
// loops repairs through the Coder, and finally asks the Validator for a
// verdict per goal. The output is a Report the user can read and diff.
package pipeline

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/eideroliveira/aspect/internal/agents"
	"github.com/eideroliveira/aspect/internal/lang"
	"github.com/eideroliveira/aspect/internal/llm"
	"github.com/eideroliveira/aspect/internal/plan"
	"github.com/eideroliveira/aspect/internal/spec"
	"github.com/eideroliveira/aspect/internal/workspace"
)

// Options tune a run.
type Options struct {
	// OutDir is where the generated project and the report are written.
	OutDir string
	// MaxRepairs bounds the Coder repair loop per module.
	MaxRepairs int
	// Log receives progress lines; nil silences them.
	Log io.Writer
	// Model is recorded in the report for provenance.
	Model string
	// Parallel is how many independent modules of a tier are generated at
	// once. Model calls overlap; tool runs in one workspace stay serialised.
	// Zero or one means sequential.
	Parallel int
}

// ModuleReport is the outcome for one module.
type ModuleReport struct {
	Tier       string         `json:"tier,omitempty"`
	Module     string         `json:"module"`
	Iterations int            `json:"iterations"`
	TestsOK    bool           `json:"tests_ok"`
	TestOutput string         `json:"test_output"`
	CoderNotes string         `json:"coder_notes"`
	Concerns   []string       `json:"concerns"`
	Verdict    agents.Verdict `json:"verdict"`
	Files      []string       `json:"files"`
	Usage      llm.Usage      `json:"usage"`
	Duration   time.Duration  `json:"duration"`
	Error      string         `json:"error,omitempty"`

	code []workspace.File
}

// Report is the outcome of a whole run.
type Report struct {
	System   string        `json:"system"`
	Topology spec.Topology `json:"topology"`
	Language string        `json:"language"`
	// Tiers lists tier names in build order with their output directories.
	Tiers    []TierOutput   `json:"tiers,omitempty"`
	Model    string         `json:"model"`
	Started  time.Time      `json:"started"`
	Finished time.Time      `json:"finished"`
	Modules  []ModuleReport `json:"modules"`
	// SystemVerdict is the cross-module verdict, produced once every tier
	// is built.
	SystemVerdict      agents.SystemVerdict `json:"system_verdict"`
	SystemVerdictError string               `json:"system_verdict_error,omitempty"`
	Usage              llm.Usage            `json:"usage"`
	// Goals summarises every system goal across modules and the system pass.
	Goals []GoalSummary `json:"goals"`
}

// TierOutput records where a tier was generated.
type TierOutput struct {
	Name     string `json:"name"`
	Language string `json:"language"`
	Dir      string `json:"dir"`
}

// GoalSummary rolls a goal's verdicts up across the modules that own it.
type GoalSummary struct {
	ID        string            `json:"id"`
	Statement string            `json:"statement"`
	Verify    spec.VerifyMethod `json:"verify"`
	// Status is the weakest of the module roll-up and the system verdict.
	Status   agents.GoalStatus            `json:"status"`
	ByModule map[string]agents.GoalStatus `json:"by_module"`
	// System is the System Validator's verdict on the goal.
	System   agents.GoalStatus `json:"system"`
	Evidence string            `json:"evidence,omitempty"`
}

// Runner wires agents to a workspace.
type Runner struct {
	Coder     *agents.Coder
	Tester    *agents.Tester
	Validator *agents.Validator
	System    *agents.SystemValidator
	Opts      Options
}

// New builds a Runner where all three agents share one client.
func New(c llm.Client, opts Options) *Runner {
	if opts.MaxRepairs == 0 {
		opts.MaxRepairs = 3
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	return &Runner{
		Coder:     &agents.Coder{LLM: c},
		Tester:    &agents.Tester{LLM: c},
		Validator: &agents.Validator{LLM: c},
		System:    &agents.SystemValidator{LLM: c},
		Opts:      opts,
	}
}

var logMu sync.Mutex

func (r *Runner) logf(format string, args ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	fmt.Fprintf(r.Opts.Log, format+"\n", args...)
}

// Toolchain adapts a language profile and a tier to what the workspace needs.
func Toolchain(p *lang.Profile, s *spec.Spec, t *spec.Tier) workspace.Toolchain {
	return workspace.Toolchain{
		SourceExt: p.SourceExt,
		SkipDir:   p.SkipDir,
		Protected: []string{"go.mod", "go.sum", "Package.swift", "Package.resolved"},
		Init:      func(root string) error { return p.Init(root, s, t) },
		Sync:      func(root string, mods []string) error { return p.Sync(root, s, t, mods) },
		Steps: func(module string) [][]string {
			return append(append([][]string{}, p.Prepare...), p.Steps(t.Module(module))...)
		},
		Env: []string{"GOFLAGS=-mod=mod", "GOWORK=off"},
	}
}

// InPlaceToolchain is Toolchain for a repository Aspect did not generate and
// must not change: no manifest is written or synced, no Prepare step runs,
// and the steps see the caller's own environment (its GOFLAGS, its go.work).
func InPlaceToolchain(p *lang.Profile, t *spec.Tier) workspace.Toolchain {
	return workspace.Toolchain{
		SourceExt: p.SourceExt,
		SkipDir:   p.SkipDir,
		Steps:     func(module string) [][]string { return p.Steps(t.Module(module)) },
	}
}

// TierModules returns pointers to a tier's modules, the form lang.Profile's
// Owner takes.
func TierModules(t *spec.Tier) []*spec.Module {
	out := make([]*spec.Module, len(t.Modules))
	for i := range t.Modules {
		out[i] = &t.Modules[i]
	}
	return out
}

// TierDir is where a tier's code goes: out/<system>/ for a single-tier
// spec, out/<system>/<tier>/ otherwise.
func TierDir(outDir, system, tier string) string {
	if tier == "" {
		return filepath.Join(outDir, system)
	}
	return filepath.Join(outDir, system, tier)
}

// Run executes the plan tier by tier. It keeps going after a module fails
// so the report shows every module's state; the error returned summarises
// failures.
func (r *Runner) Run(ctx context.Context, s *spec.Spec, p *plan.Plan) (*Report, error) {
	rep := &Report{System: s.System.Name, Topology: p.Topology, Model: r.Opts.Model, Started: time.Now()}
	if len(p.Tiers) > 0 {
		rep.Language = p.Tiers[0].Language
	}
	var failed []string

	for _, tp := range p.Tiers {
		tier := s.Tier(tp.Name)
		profile, err := lang.For(tier.Language)
		if err != nil {
			return rep, err
		}
		root := TierDir(r.Opts.OutDir, s.System.Name, tier.Name)
		ws, err := workspace.New(root, Toolchain(profile, s, tier))
		if err != nil {
			return rep, err
		}
		rep.Tiers = append(rep.Tiers, TierOutput{Name: tier.Name, Language: tier.Language, Dir: root})
		if tier.Name != "" {
			r.logf("#### tier %s (%s) -> %s", tier.Name, tier.Language, root)
		}
		// Every module of the tier is named to Sync; manifests only declare
		// what exists on disk.
		var all []string
		for _, step := range tp.Steps {
			all = append(all, step.Module)
		}
		results := make([]ModuleReport, len(tp.Steps))
		for _, wave := range waves(tp.Steps) {
			if ctx.Err() != nil {
				return rep, ctx.Err()
			}
			r.runWave(ctx, s, tier, profile, tp.Steps, wave, ws, all, results)
		}
		for _, mr := range results {
			rep.Modules = append(rep.Modules, mr)
			rep.Usage.Calls += mr.Usage.Calls
			rep.Usage.InputTokens += mr.Usage.InputTokens
			rep.Usage.OutputTokens += mr.Usage.OutputTokens
			rep.Usage.CacheReadTokens += mr.Usage.CacheReadTokens
			if mr.Error != "" {
				failed = append(failed, mr.Module)
			}
		}
	}
	// Cross-module pass: the parts have been judged; now the whole.
	r.logf("#### system validator: judging goals across modules")
	var evidence []agents.ModuleEvidence
	for _, m := range rep.Modules {
		evidence = append(evidence, agents.ModuleEvidence{Tier: m.Tier, Module: m.Module, TestsOK: m.TestsOK, Verdict: m.Verdict, Code: m.code, Error: m.Error})
	}
	sysVerdict, resp, err := r.System.Judge(ctx, agents.SystemInput{Spec: s, Modules: evidence})
	rep.Usage.Add(resp)
	if err != nil {
		rep.SystemVerdictError = err.Error()
		r.logf("   error: %s", err)
	} else {
		rep.SystemVerdict = sysVerdict
	}

	rep.Finished = time.Now()
	rep.Goals = Summarise(s, rep.Modules, rep.SystemVerdict, rep.SystemVerdictError != "")
	if rep.SystemVerdictError != "" {
		failed = append(failed, "system")
	}
	if len(failed) > 0 {
		return rep, fmt.Errorf("pipeline: %d module(s) failed: %s", len(failed), strings.Join(failed, ", "))
	}
	return rep, nil
}

// waves groups step indexes into rounds: a step joins the first round after
// every dependency's round. Steps in one round are independent of each
// other and may be generated concurrently. Within a round, plan order is
// kept.
func waves(steps []plan.Step) [][]int {
	round := map[string]int{}
	var out [][]int
	for i, st := range steps {
		w := 0
		for _, d := range st.DependsOn {
			if dw, ok := round[d]; ok && dw+1 > w {
				w = dw + 1
			}
		}
		round[st.Module] = w
		for len(out) <= w {
			out = append(out, nil)
		}
		out[w] = append(out[w], i)
	}
	return out
}

// runWave generates the steps of one round, at most Opts.Parallel at a time.
func (r *Runner) runWave(ctx context.Context, s *spec.Spec, tier *spec.Tier, profile *lang.Profile, steps []plan.Step, wave []int, ws *workspace.Workspace, all []string, results []ModuleReport) {
	parallel := r.Opts.Parallel
	if parallel < 1 {
		parallel = 1
	}
	if len(wave) > 1 && parallel > 1 {
		var names []string
		for _, i := range wave {
			names = append(names, steps[i].Module)
		}
		r.logf("== wave of %d independent modules: %s", len(wave), strings.Join(names, ", "))
	}
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for _, i := range wave {
		step := steps[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				results[i] = ModuleReport{Tier: tier.Name, Module: step.Module, Error: ctx.Err().Error()}
				return
			}
			r.logf("== module %s (goals %s)", step.Module, strings.Join(step.Goals, ","))
			mr := r.runModule(ctx, s, tier, profile, step, ws, all)
			if mr.Error != "" {
				r.logf("   %s: error: %s", step.Module, mr.Error)
			}
			results[i] = mr
		}()
	}
	wg.Wait()
}

func (r *Runner) runModule(ctx context.Context, s *spec.Spec, tier *spec.Tier, profile *lang.Profile, step plan.Step, ws *workspace.Workspace, generated []string) ModuleReport {
	start := time.Now()
	mr := ModuleReport{Tier: tier.Name, Module: step.Module}
	fail := func(err error) ModuleReport {
		mr.Error = err.Error()
		mr.Duration = time.Since(start)
		return mr
	}
	task := agents.Task{Spec: s, Tier: tier, Module: tier.Module(step.Module), Lang: profile}
	allowed := tier.AllowedImports()
	deps, err := dependencyFiles(ws, profile, tier, step.DependsOn)
	if err != nil {
		return fail(err)
	}

	// 1. Code.
	r.logf("   %s: coder writing implementation", step.Module)
	code, resp, err := r.Coder.Generate(ctx, agents.CodeInput{Task: task, Dependencies: deps})
	mr.Usage.Add(resp)
	if err != nil {
		return fail(err)
	}
	if err := ws.WriteFiles(code.Files); err != nil {
		return fail(err)
	}
	mr.CoderNotes = code.Notes
	mr.Concerns = append(mr.Concerns, code.Concerns...)

	// 2. Tests, derived from the spec with the code visible for identifiers.
	// A proposal that imports a disallowed library gets one rewrite.
	r.logf("   %s: tester writing tests", step.Module)
	tests, resp, err := r.Tester.Generate(ctx, agents.TestInput{Task: task, Code: code.Files, Dependencies: deps})
	mr.Usage.Add(resp)
	if err != nil {
		return fail(err)
	}
	if vs := checkImports(profile, tests.Files, allowed); len(vs) > 0 {
		r.logf("   %s: tester rewriting (disallowed imports)", step.Module)
		feedback := workspace.FormatViolations(vs, allowed)
		tests, resp, err = r.Tester.Generate(ctx, agents.TestInput{Task: task, Code: code.Files, Dependencies: deps, Feedback: feedback, Existing: tests.Files})
		mr.Usage.Add(resp)
		if err != nil {
			return fail(err)
		}
		if vs := checkImports(profile, tests.Files, allowed); len(vs) > 0 {
			return fail(fmt.Errorf("tester: %s", strings.TrimSpace(workspace.FormatViolations(vs, allowed))))
		}
	}
	if err := ws.WriteFiles(tests.Files); err != nil {
		return fail(err)
	}
	mr.Concerns = append(mr.Concerns, tests.Concerns...)
	if err := ws.Sync(generated); err != nil {
		return fail(err)
	}

	// 3. Run, and repair the implementation while it fails. Disallowed
	// imports count as a failed run without spending a build.
	var result workspace.Result
	var attempts []agents.RepairAttempt
	var lastOutput string
	for attempt := 0; ; attempt++ {
		mr.Iterations = attempt + 1
		if vs := checkImports(profile, code.Files, allowed); len(vs) > 0 {
			result = workspace.Result{OK: false, Output: workspace.FormatViolations(vs, allowed)}
		} else {
			r.logf("   %s: test run %d", step.Module, mr.Iterations)
			result, err = ws.Test(ctx, step.Module)
			if err != nil {
				return fail(err)
			}
		}
		if result.OK || attempt >= r.Opts.MaxRepairs {
			break
		}
		if attempt > 0 {
			attempts[len(attempts)-1].Output = result.Output
			// A repair that leaves the failure exactly as it was has not
			// engaged with it; the remaining rounds would most likely be
			// spent the same way.
			if sameFailure(lastOutput, result.Output) {
				r.logf("   %s: repair %d left the failure unchanged; stopping early", step.Module, attempt)
				mr.Concerns = append(mr.Concerns, fmt.Sprintf("[aspect] repair %d left the failing output unchanged; the remaining %d repair rounds were skipped", attempt, r.Opts.MaxRepairs-attempt))
				break
			}
		}
		lastOutput = result.Output
		r.logf("   %s: coder repairing (%d of %d)", step.Module, attempt+1, r.Opts.MaxRepairs)
		previous := code.Files
		code, resp, err = r.Coder.Generate(ctx, agents.CodeInput{
			Task: task, Dependencies: deps,
			Existing: previous, Tests: tests.Files, Feedback: result.Output,
			Attempts: attempts,
		})
		mr.Usage.Add(resp)
		if err != nil {
			return fail(err)
		}
		attempts = append(attempts, agents.RepairAttempt{Changed: changed(previous, code.Files)})
		// A repair that renames a file must not leave the old one behind.
		if err := ws.DeleteFiles(removed(previous, code.Files)); err != nil {
			return fail(err)
		}
		if err := ws.WriteFiles(code.Files); err != nil {
			return fail(err)
		}
		mr.Concerns = append(mr.Concerns, code.Concerns...)
	}
	mr.TestsOK = result.OK
	mr.TestOutput = tail(result.Output, 8000)
	mr.code = code.Files
	for _, f := range append(append([]workspace.File{}, code.Files...), tests.Files...) {
		mr.Files = append(mr.Files, f.Path)
	}

	// 4. Judge.
	r.logf("   %s: validator judging goals", step.Module)
	verdict, resp, err := r.Validator.Judge(ctx, agents.ValidateInput{
		Task: task, GoalIDs: step.Goals,
		Code: code.Files, Tests: tests.Files, TestResult: result,
		Coverage: tests.Coverage, Concerns: mr.Concerns,
	})
	mr.Usage.Add(resp)
	if err != nil {
		return fail(err)
	}
	mr.Verdict = verdict
	mr.Duration = time.Since(start)
	return mr
}

func checkImports(p *lang.Profile, files []workspace.File, allowed []string) []workspace.ImportViolation {
	if p.CheckImports == nil {
		return nil
	}
	return p.CheckImports(files, allowed)
}

// dependencyFiles collects the source (not tests) of the named modules.
func dependencyFiles(ws *workspace.Workspace, p *lang.Profile, t *spec.Tier, deps []string) ([]workspace.File, error) {
	if len(deps) == 0 {
		return nil, nil
	}
	tree, err := ws.ReadTree()
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, d := range deps {
		want[d] = true
	}
	mods := TierModules(t)
	var out []workspace.File
	for _, f := range tree {
		if p.IsTestFile(f.Path) {
			continue
		}
		if owner := p.Owner(mods, f.Path); owner != nil && want[owner.Name] {
			out = append(out, f)
		}
	}
	return out, nil
}

// changed lists the files a repair touched: new paths, rewritten content,
// and removed paths.
func changed(before, after []workspace.File) []string {
	prev := map[string]string{}
	for _, f := range before {
		prev[f.Path] = f.Content
	}
	var out []string
	for _, f := range after {
		if c, ok := prev[f.Path]; !ok || c != f.Content {
			out = append(out, f.Path)
		}
	}
	for _, p := range removed(before, after) {
		out = append(out, p+" (removed)")
	}
	return out
}

// volatile matches the parts of a test run that change between identical
// failures: durations, addresses, goroutine ids, temp paths.
var volatile = regexp.MustCompile(`\(\d+(\.\d+)?m?s\)|\b\d+(\.\d+)?m?s\b|0x[0-9a-fA-F]+|goroutine \d+|/tmp/[^\s:]+|/var/folders/[^\s:]+`)

// sameFailure reports whether two failing outputs are the same failure once
// the volatile parts are ignored.
func sameFailure(a, b string) bool {
	norm := func(s string) string { return strings.TrimSpace(volatile.ReplaceAllString(s, "")) }
	return norm(a) == norm(b)
}

func removed(before, after []workspace.File) []string {
	keep := map[string]bool{}
	for _, f := range after {
		keep[f.Path] = true
	}
	var gone []string
	for _, f := range before {
		if !keep[f.Path] {
			gone = append(gone, f.Path)
		}
	}
	return gone
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…\n" + s[len(s)-n:]
}

var statusRank = map[agents.GoalStatus]int{agents.Achieved: 0, agents.Partial: 1, agents.Unverifiable: 2, agents.NotAchieved: 3}

// Summarise rolls goal verdicts up: a goal is only as achieved as its weakest
// owning module, and never better than the system pass judged it.
func Summarise(s *spec.Spec, mods []ModuleReport, system agents.SystemVerdict, systemFailed bool) []GoalSummary {
	rank := statusRank
	bySystem := map[string]agents.GoalVerdict{}
	for _, g := range system.Goals {
		bySystem[g.ID] = g
	}
	var out []GoalSummary
	for _, g := range s.System.Goals {
		gs := GoalSummary{ID: g.ID, Statement: g.Statement, Verify: g.Verify, ByModule: map[string]agents.GoalStatus{}}
		worst := agents.GoalStatus("")
		for _, m := range mods {
			for _, v := range m.Verdict.Goals {
				if v.ID != g.ID {
					continue
				}
				gs.ByModule[m.Module] = v.Status
				if worst == "" || rank[v.Status] > rank[worst] {
					worst = v.Status
				}
			}
			if m.Error != "" && owns(s.Module(m.Module), g.ID) {
				gs.ByModule[m.Module] = agents.Unverifiable
				if rank[agents.Unverifiable] > rank[worst] || worst == "" {
					worst = agents.Unverifiable
				}
			}
		}
		if worst == "" {
			worst = agents.Unverifiable
		}
		if sv, ok := bySystem[g.ID]; ok && !systemFailed {
			gs.System = sv.Status
			gs.Evidence = sv.Evidence
		} else {
			gs.System = agents.Unverifiable
		}
		if rank[gs.System] > rank[worst] {
			worst = gs.System
		}
		gs.Status = worst
		out = append(out, gs)
	}
	return out
}

func owns(m *spec.Module, goal string) bool {
	if m == nil {
		return false
	}
	for _, g := range m.Goals {
		if g == goal {
			return true
		}
	}
	for _, sc := range m.Scenarios {
		for _, g := range sc.Goals {
			if g == goal {
				return true
			}
		}
	}
	return false
}
