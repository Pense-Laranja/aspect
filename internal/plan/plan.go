// Package plan turns a validated spec into an ordered build plan.
//
// The planner is deliberately deterministic: tier order comes from tier
// dependencies and module order from the module dependency graph, not from a
// model. Determinism here means two runs of the same spec produce the same
// plan, which makes reports comparable across runs.
package plan

import (
	"fmt"
	"sort"

	"github.com/eideroliveira/aspect/internal/spec"
)

// Step is the work the pipeline performs for one module.
type Step struct {
	Tier      string
	Module    string
	DependsOn []string
	// Goals the Validator must reach a verdict on for this module: the module's
	// own goals plus those named by its scenarios.
	Goals []string
	// Scenarios the Tester must cover.
	Scenarios int
	// Invariants the Tester must probe and the Validator must judge.
	Invariants int
	// Entities the module owns, Surfaces it implements, Consumes it calls.
	Entities []string
	Surfaces []string
	Consumes []string
}

// TierPlan is the ordered steps of one tier.
type TierPlan struct {
	// Name is empty for the implicit tier of a single-tier spec.
	Name     string
	Language string
	Steps    []Step
}

// Plan is the ordered list of tiers, each with its ordered steps.
type Plan struct {
	System   string
	Topology spec.Topology
	Tiers    []TierPlan
}

// Steps flattens every tier's steps in build order.
func (p *Plan) Steps() []Step {
	var out []Step
	for _, t := range p.Tiers {
		out = append(out, t.Steps...)
	}
	return out
}

// Order returns module names in build order.
func (p *Plan) Order() []string {
	var out []string
	for _, s := range p.Steps() {
		out = append(out, s.Module)
	}
	return out
}

// Build computes the plan. It assumes the spec passed Validate; on a cycle
// it returns an error rather than a partial plan.
func Build(s *spec.Spec) (*Plan, error) {
	p := &Plan{System: s.System.Name, Topology: s.EffectiveTopology()}
	tiers := s.EffectiveTiers()
	byName := map[string]*spec.Tier{}
	var names []string
	deps := map[string][]string{}
	for _, t := range tiers {
		byName[t.Name] = t
		names = append(names, t.Name)
		deps[t.Name] = t.DependsOn
	}
	tierOrder, err := topo(names, deps)
	if err != nil {
		return nil, fmt.Errorf("plan: tiers: %w", err)
	}
	for _, name := range tierOrder {
		t := byName[name]
		var modNames []string
		modDeps := map[string][]string{}
		for _, m := range t.Modules {
			modNames = append(modNames, m.Name)
			modDeps[m.Name] = m.DependsOn
		}
		order, err := topo(modNames, modDeps)
		if err != nil {
			return nil, fmt.Errorf("plan: tier %q: %w", name, err)
		}
		tp := TierPlan{Name: t.Name, Language: t.Language}
		for _, mn := range order {
			tp.Steps = append(tp.Steps, stepFor(s, t, t.Module(mn)))
		}
		p.Tiers = append(p.Tiers, tp)
	}
	return p, nil
}

// topo is Kahn's algorithm with alphabetical tie-breaking.
func topo(names []string, deps map[string][]string) ([]string, error) {
	known := map[string]bool{}
	for _, n := range names {
		known[n] = true
	}
	indeg := map[string]int{}
	dependents := map[string][]string{}
	for _, n := range names {
		indeg[n] = 0
	}
	for _, n := range names {
		for _, d := range deps[n] {
			if !known[d] {
				continue
			}
			indeg[n]++
			dependents[d] = append(dependents[d], n)
		}
	}
	var ready []string
	for n, k := range indeg {
		if k == 0 {
			ready = append(ready, n)
		}
	}
	sort.Strings(ready)
	var out []string
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		out = append(out, n)
		next := append([]string{}, dependents[n]...)
		sort.Strings(next)
		for _, d := range next {
			indeg[d]--
			if indeg[d] == 0 {
				ready = append(ready, d)
				sort.Strings(ready)
			}
		}
	}
	if len(out) != len(names) {
		return nil, fmt.Errorf("dependency cycle (ordered %d of %d)", len(out), len(names))
	}
	return out, nil
}

func stepFor(s *spec.Spec, t *spec.Tier, m *spec.Module) Step {
	seen := map[string]bool{}
	var goals []string
	addGoal := func(g string) {
		if !seen[g] {
			seen[g] = true
			goals = append(goals, g)
		}
	}
	for _, g := range m.Goals {
		addGoal(g)
	}
	for _, sc := range m.Scenarios {
		for _, g := range sc.Goals {
			addGoal(g)
		}
	}
	sort.Strings(goals)
	ids := func(refs []string) []string {
		var out []string
		if resolved, _ := s.ResolveSurfaces(refs); len(resolved) > 0 {
			for _, r := range resolved {
				out = append(out, r.ID())
			}
		}
		return out
	}
	return Step{
		Tier:       t.Name,
		Module:     m.Name,
		DependsOn:  append([]string{}, m.DependsOn...),
		Goals:      goals,
		Scenarios:  len(m.Scenarios),
		Invariants: invariantCount(m),
		Entities:   append([]string{}, m.Entities...),
		Surfaces:   ids(m.Surfaces),
		Consumes:   ids(m.Consumes),
	}
}

// invariantCount counts a module's invariants, its topics' included: the
// Tester covers them all.
func invariantCount(m *spec.Module) int {
	n := len(m.Invariants)
	for _, t := range m.Topics {
		n += len(t.Invariants)
	}
	return n
}
