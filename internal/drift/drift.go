// Package drift re-validates an existing codebase against a spec without
// regenerating anything. It answers three questions the build pipeline
// cannot: does the code on disk still match the spec after either changed,
// which modules exist only in one of them, and which goal verdicts moved
// since the last report.
//
// The deterministic part (presence, orphans, the real test run) needs no
// model; verdicts do.
package drift

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/eideroliveira/aspect/internal/agents"
	"github.com/eideroliveira/aspect/internal/lang"
	"github.com/eideroliveira/aspect/internal/llm"
	"github.com/eideroliveira/aspect/internal/pipeline"
	"github.com/eideroliveira/aspect/internal/plan"
	"github.com/eideroliveira/aspect/internal/spec"
	"github.com/eideroliveira/aspect/internal/workspace"
)

// Options configure a drift check.
type Options struct {
	// OutDir is the directory that holds out/<system>[/<tier>], as given to
	// `aspect run -out`.
	OutDir string
	// Repo, when set, is a repository checked in place instead of generated
	// output: a single-tier spec's code is Repo itself, a tier's is
	// Repo/<tier>. Drift then never writes there: no manifest is created or
	// synced and no Prepare step (go mod tidy) runs.
	Repo string
	// Baseline is a previous report.json (from run or drift) to compare
	// goal verdicts against. Optional.
	Baseline string
	// NoLLM skips the Validator and System Validator: presence, orphans and
	// tests only.
	NoLLM bool
	Log   io.Writer
	Model string
}

// Change of a goal's status against the baseline.
type Change string

const (
	Same       Change = "same"
	Regressed  Change = "regressed"
	Improved   Change = "improved"
	NoBaseline Change = "new"
)

// ModuleDrift is one module's state on disk against the spec.
type ModuleDrift struct {
	Tier   string `json:"tier,omitempty"`
	Module string `json:"module"`
	// Present is false when the spec names a module that has no code.
	Present    bool           `json:"present"`
	Files      []string       `json:"files,omitempty"`
	TestsOK    bool           `json:"tests_ok"`
	TestOutput string         `json:"test_output,omitempty"`
	Verdict    agents.Verdict `json:"verdict"`
	Error      string         `json:"error,omitempty"`
}

// GoalDrift is a goal's status now and before.
type GoalDrift struct {
	pipeline.GoalSummary
	Previous agents.GoalStatus `json:"previous,omitempty"`
	Change   Change            `json:"change"`
}

// Report is the outcome of a drift check.
type Report struct {
	System   string        `json:"system"`
	Topology spec.Topology `json:"topology"`
	Model    string        `json:"model,omitempty"`
	Checked  time.Time     `json:"checked"`
	Baseline string        `json:"baseline,omitempty"`
	Modules  []ModuleDrift `json:"modules"`
	// Orphans are code directories in a tier that no module of the spec
	// claims, as "tier/dir" (or "dir" for a single-tier spec).
	Orphans            []string             `json:"orphans,omitempty"`
	SystemVerdict      agents.SystemVerdict `json:"system_verdict"`
	SystemVerdictError string               `json:"system_verdict_error,omitempty"`
	Goals              []GoalDrift          `json:"goals"`
	Usage              llm.Usage            `json:"usage"`
}

// Missing lists modules the spec names that have no code.
func (r *Report) Missing() []string {
	var out []string
	for _, m := range r.Modules {
		if !m.Present {
			out = append(out, m.Module)
		}
	}
	return out
}

// Failing lists present modules whose tests fail.
func (r *Report) Failing() []string {
	var out []string
	for _, m := range r.Modules {
		if m.Present && !m.TestsOK {
			out = append(out, m.Module)
		}
	}
	return out
}

// Regressions lists goals whose status is worse than the baseline's.
func (r *Report) Regressions() []string {
	var out []string
	for _, g := range r.Goals {
		if g.Change == Regressed {
			out = append(out, g.ID)
		}
	}
	return out
}

// Drifted reports whether anything needs attention: missing modules,
// orphans, failing tests or regressed goals.
func (r *Report) Drifted() bool {
	return len(r.Missing()) > 0 || len(r.Orphans) > 0 || len(r.Failing()) > 0 || len(r.Regressions()) > 0
}

// Run checks the codebase under opts.OutDir against the spec.
func Run(ctx context.Context, c llm.Client, s *spec.Spec, p *plan.Plan, opts Options) (*Report, error) {
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	logf := func(format string, args ...any) { fmt.Fprintf(opts.Log, format+"\n", args...) }
	rep := &Report{System: s.System.Name, Topology: p.Topology, Model: opts.Model, Checked: time.Now(), Baseline: opts.Baseline}
	validator := &agents.Validator{LLM: c}
	system := &agents.SystemValidator{LLM: c}
	var evidence []agents.ModuleEvidence
	var reports []pipeline.ModuleReport

	for _, tp := range p.Tiers {
		tier := s.Tier(tp.Name)
		profile, err := lang.For(tier.Language)
		if err != nil {
			return rep, err
		}
		root, tc := pipeline.TierDir(opts.OutDir, s.System.Name, tier.Name), pipeline.Toolchain(profile, s, tier)
		if opts.Repo != "" {
			root, tc = filepath.Join(opts.Repo, tier.Name), pipeline.InPlaceToolchain(profile, tier)
		}
		if _, err := os.Stat(root); err != nil {
			hint := ""
			if opts.Repo == "" {
				hint = " (for code that is not aspect run output, such as the repository the spec describes, use -repo)"
			}
			return rep, fmt.Errorf("drift: no code for %s at %s%s", tierLabel(tier), root, hint)
		}
		ws, err := workspace.New(root, tc)
		if err != nil {
			return rep, err
		}
		tree, err := ws.ReadTree()
		if err != nil {
			return rep, err
		}
		var all []string
		for _, st := range tp.Steps {
			all = append(all, st.Module)
		}
		owned, unclaimed := assign(profile, tier.ModuleRefs(), tree)
		var exclude []string
		if s.System.Source != nil {
			exclude = s.System.Source.Exclude
		}
		rep.Orphans = append(rep.Orphans, orphans(tier, unclaimed, exclude)...)
		if err := ws.Sync(all); err != nil {
			return rep, err
		}

		for _, step := range tp.Steps {
			if ctx.Err() != nil {
				return rep, ctx.Err()
			}
			md := ModuleDrift{Tier: tier.Name, Module: step.Module}
			code, tests := split(profile, owned[step.Module])
			for _, f := range append(append([]workspace.File{}, code...), tests...) {
				md.Files = append(md.Files, f.Path)
			}
			md.Present = len(code) > 0
			if !md.Present {
				logf("== %s: missing (no files in %s)", step.Module, lang.DirLabel(profile.CodeDir(tier.Module(step.Module))))
				md.Verdict = agents.Verdict{Module: step.Module, IntentStatus: agents.Violated, IntentRationale: "the module has no code"}
				for _, g := range step.Goals {
					md.Verdict.Goals = append(md.Verdict.Goals, agents.GoalVerdict{ID: g, Status: agents.NotAchieved, Evidence: "no code on disk"})
				}
				rep.Modules = append(rep.Modules, md)
				reports = append(reports, pipeline.ModuleReport{Tier: tier.Name, Module: step.Module, Verdict: md.Verdict, Error: "missing"})
				continue
			}
			logf("== %s: %d file(s), running tests", step.Module, len(md.Files))
			result, err := ws.Test(ctx, step.Module)
			if err != nil {
				md.Error = err.Error()
				rep.Modules = append(rep.Modules, md)
				reports = append(reports, pipeline.ModuleReport{Tier: tier.Name, Module: step.Module, Error: md.Error})
				continue
			}
			md.TestsOK = result.OK
			md.TestOutput = tail(result.Output, 8000)
			if opts.NoLLM {
				md.Verdict = agents.Verdict{Module: step.Module}
				for _, g := range step.Goals {
					md.Verdict.Goals = append(md.Verdict.Goals, agents.GoalVerdict{ID: g, Status: agents.Unverifiable, Evidence: "verdicts skipped (-no-llm)"})
				}
			} else {
				logf("   validator: judging %s", step.Module)
				task := agents.Task{Spec: s, Tier: tier, Module: tier.Module(step.Module), Lang: profile}
				verdict, resp, err := validator.Judge(ctx, agents.ValidateInput{Task: task, GoalIDs: step.Goals, Code: code, Tests: tests, TestResult: result})
				rep.Usage.Add(resp)
				if err != nil {
					md.Error = err.Error()
				} else {
					md.Verdict = verdict
				}
			}
			rep.Modules = append(rep.Modules, md)
			reports = append(reports, pipeline.ModuleReport{Tier: tier.Name, Module: step.Module, TestsOK: md.TestsOK, Verdict: md.Verdict, Error: md.Error})
			evidence = append(evidence, agents.ModuleEvidence{Tier: tier.Name, Module: step.Module, TestsOK: md.TestsOK, Verdict: md.Verdict, Code: code, Error: md.Error})
		}
	}

	systemFailed := true
	if !opts.NoLLM && ctx.Err() == nil {
		logf("== system validator: judging goals across modules")
		sv, resp, err := system.Judge(ctx, agents.SystemInput{Spec: s, Modules: evidence})
		rep.Usage.Add(resp)
		if err != nil {
			rep.SystemVerdictError = err.Error()
		} else {
			rep.SystemVerdict = sv
			systemFailed = false
		}
	}
	summary := pipeline.Summarise(s, reports, rep.SystemVerdict, systemFailed)
	previous := loadBaseline(opts.Baseline)
	for _, g := range summary {
		gd := GoalDrift{GoalSummary: g, Change: NoBaseline}
		if prev, ok := previous[g.ID]; ok {
			gd.Previous = prev
			gd.Change = compare(prev, g.Status)
		}
		rep.Goals = append(rep.Goals, gd)
	}
	sort.Strings(rep.Orphans)
	return rep, nil
}

func tierLabel(t *spec.Tier) string {
	if t.Name == "" {
		return "the system"
	}
	return "tier " + t.Name
}

// assign gives every file of the tree to the module that owns it (see
// lang.Profile.Owner); the rest are unclaimed.
func assign(p *lang.Profile, mods []*spec.Module, tree []workspace.File) (owned map[string][]workspace.File, unclaimed []workspace.File) {
	owned = map[string][]workspace.File{}
	for _, f := range tree {
		if m := p.Owner(mods, f.Path); m != nil {
			owned[m.Name] = append(owned[m.Name], f)
		} else {
			unclaimed = append(unclaimed, f)
		}
	}
	return owned, unclaimed
}

// split separates a module's files into implementation and tests.
func split(p *lang.Profile, files []workspace.File) (code, tests []workspace.File) {
	for _, f := range files {
		if p.IsTestFile(f.Path) {
			tests = append(tests, f)
		} else {
			code = append(code, f)
		}
	}
	return code, tests
}

// orphans lists the directories of unclaimed files. Loose files in the root
// are not reported (a generated project keeps its entry point there), nor
// directories the import excluded.
func orphans(t *spec.Tier, unclaimed []workspace.File, exclude []string) []string {
	seen := map[string]bool{}
	for _, f := range unclaimed {
		if !strings.Contains(f.Path, "/") {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(f.Path))
		if excluded(dir, exclude) {
			continue
		}
		label := dir
		if t.Name != "" {
			label = t.Name + "/" + dir
		}
		seen[label] = true
	}
	var out []string
	for k := range seen {
		out = append(out, k)
	}
	return out
}

// excluded matches a directory against `aspect import -exclude` entries the
// way the inventory does: an entry names a directory anywhere in the tree,
// or a relative path and everything below it.
func excluded(dir string, exclude []string) bool {
	segments := strings.Split(dir, "/")
	for _, ex := range exclude {
		ex = strings.TrimSuffix(strings.TrimPrefix(ex, "./"), "/")
		if ex == "" {
			continue
		}
		if dir == ex || strings.HasPrefix(dir, ex+"/") {
			return true
		}
		for _, seg := range segments {
			if seg == ex {
				return true
			}
		}
	}
	return false
}

func compare(prev, now agents.GoalStatus) Change {
	rank := map[agents.GoalStatus]int{agents.Achieved: 0, agents.Partial: 1, agents.Unverifiable: 2, agents.NotAchieved: 3}
	switch {
	case rank[now] > rank[prev]:
		return Regressed
	case rank[now] < rank[prev]:
		return Improved
	default:
		return Same
	}
}

// loadBaseline reads goal statuses from a run or drift report.json.
func loadBaseline(path string) map[string]agents.GoalStatus {
	out := map[string]agents.GoalStatus{}
	if path == "" {
		return out
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var doc struct {
		Goals []struct {
			ID     string            `json:"id"`
			Status agents.GoalStatus `json:"status"`
		} `json:"goals"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return out
	}
	for _, g := range doc.Goals {
		out[g.ID] = g.Status
	}
	return out
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…\n" + s[len(s)-n:]
}
