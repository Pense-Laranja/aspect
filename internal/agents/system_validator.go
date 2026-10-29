package agents

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Pense-Laranja/aspect/internal/llm"
	"github.com/Pense-Laranja/aspect/internal/spec"
	"github.com/Pense-Laranja/aspect/internal/workspace"
)

// ModuleEvidence is what the System Validator knows about one built module.
type ModuleEvidence struct {
	Tier    string
	Module  string
	TestsOK bool
	Verdict Verdict
	// Code is the module's implementation (not tests). It may be omitted for
	// modules that neither provide nor consume surfaces when the whole
	// system would not fit the prompt budget.
	Code []workspace.File
	// Error is set when the module could not be built at all.
	Error string
}

// SystemInput is the evidence for the cross-module pass.
type SystemInput struct {
	Spec    *spec.Spec
	Modules []ModuleEvidence
}

// IntegrationFinding is the System Validator's judgement of one surface
// contract between the module that provides it and the modules that
// consume it.
type IntegrationFinding struct {
	Surface  string     `json:"surface"`
	Provider string     `json:"provider"`
	Consumer string     `json:"consumer"`
	Status   GoalStatus `json:"status"`
	Note     string     `json:"note"`
}

// SystemVerdict is the whole-system judgement.
type SystemVerdict struct {
	Goals           []GoalVerdict        `json:"goals"`
	IntentStatus    IntentStatus         `json:"intent_status"`
	IntentRationale string               `json:"intent_rationale"`
	Integration     []IntegrationFinding `json:"integration"`
	Recommendations []string             `json:"recommendations"`
}

// SystemValidator judges the system as a whole once every module exists.
// Module verdicts answer "does this module do its part?"; this pass
// answers "do the parts add up to the system the spec describes?".
type SystemValidator struct {
	LLM llm.Client
}

const systemValidatorSystem = `You are the System Validator agent in Aspect, a pipeline that builds software from a formal specification.

Every module has been built and judged on its own. You now judge the system as a whole. You receive the spec, each module's test outcome and verdict, and the implementation of the modules that matter for integration.

How to judge:
- Each system goal gets one verdict considering every module accountable for it together. A goal is achieved only if the modules' contributions compose: the API module serves what the client calls, the owner of an entity exposes what a scenario in another module needs, and so on. Module verdicts are evidence, not conclusions; they can each be "achieved" while the goal still fails because the modules disagree.
- Integration: for every surface that one module provides and others consume (including across tiers and languages), compare routes, methods, request and response shapes, error cases and auth between provider and consumer code. Report each pairing with a status and a precise note (file and identifier on both sides).
- Intent: does the system as built do what the system intent says, or has it drifted into something narrower or different? Cite what convinced you.
- A module that failed to build makes every goal it owns at best "partial".
- Confidence is your honest probability that the status is right. Prefer "partial" with concrete gaps over an optimistic "achieved".
- Answer with JSON matching the schema you were given. Evidence names files and identifiers, not impressions.`

var systemVerdictSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"goals", "intent_status", "intent_rationale", "integration", "recommendations"},
	"properties": map[string]any{
		"goals":            verdictSchema["properties"].(map[string]any)["goals"],
		"intent_status":    map[string]any{"type": "string", "enum": []string{"aligned", "drifted", "violated"}},
		"intent_rationale": map[string]any{"type": "string"},
		"integration": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"surface", "provider", "consumer", "status", "note"},
				"properties": map[string]any{
					"surface":  map[string]any{"type": "string"},
					"provider": map[string]any{"type": "string"},
					"consumer": map[string]any{"type": "string"},
					"status":   map[string]any{"type": "string", "enum": []string{"achieved", "partial", "not_achieved", "unverifiable"}},
					"note":     map[string]any{"type": "string"},
				},
			},
		},
		"recommendations": stringList(),
	},
}

// codeBudget bounds the implementation shown to the System Validator.
const codeBudget = 400000

// Judge produces the system verdict.
func (v *SystemValidator) Judge(ctx context.Context, in SystemInput) (SystemVerdict, llm.Response, error) {
	s := in.Spec
	var b strings.Builder
	view := struct {
		Name        string        `yaml:"name"`
		Intent      string        `yaml:"intent"`
		Topology    spec.Topology `yaml:"topology"`
		Goals       []spec.Goal   `yaml:"goals"`
		Constraints []string      `yaml:"constraints,omitempty"`
	}{s.System.Name, s.System.Intent, s.EffectiveTopology(), s.System.Goals, s.System.Constraints}
	fmt.Fprintf(&b, "# System\n\n```yaml\n%s```\n\n", renderYAML(view))
	if text := strings.TrimSpace(s.System.Brief.Text); text != "" {
		fmt.Fprintf(&b, "## System brief (%s)\n\n%s\n\n", s.System.Brief.Path, text)
	}
	if len(s.Tiers) > 0 {
		fmt.Fprintf(&b, "# Tiers\n\n```yaml\n%s```\n\n", renderYAML(tierOutline(s)))
	}
	if len(s.System.Interfaces) > 0 {
		fmt.Fprintf(&b, "# Interfaces (full contracts)\n\n```yaml\n%s```\n\n", renderYAML(s.System.Interfaces))
	}
	for _, d := range s.System.Dependencies {
		if dep, ok := s.Deps[d.Name]; ok && len(dep.System.Interfaces) > 0 {
			fmt.Fprintf(&b, "# Interfaces of system dependency %s (consumed only)\n\n```yaml\n%s```\n\n", d.Name, renderYAML(dep.System.Interfaces))
		}
	}
	for _, db := range s.Databases() {
		fmt.Fprintf(&b, "# Database (%s)\n\n```yaml\n%s```\n\n", dbOwner(db), renderYAML(db))
	}

	b.WriteString("# Modules\n\n")
	type moduleView struct {
		Tier     string   `yaml:"tier,omitempty"`
		Name     string   `yaml:"name"`
		Goals    []string `yaml:"goals,omitempty"`
		Entities []string `yaml:"entities,omitempty"`
		Surfaces []string `yaml:"surfaces,omitempty"`
		Consumes []string `yaml:"consumes,omitempty"`
		TestsOK  bool     `yaml:"tests_ok"`
		Error    string   `yaml:"error,omitempty"`
	}
	for _, m := range in.Modules {
		mod := s.Module(m.Module)
		mv := moduleView{Tier: m.Tier, Name: m.Module, TestsOK: m.TestsOK, Error: m.Error}
		if mod != nil {
			mv.Goals, mv.Entities, mv.Surfaces, mv.Consumes = mod.Goals, mod.Entities, mod.Surfaces, mod.Consumes
		}
		fmt.Fprintf(&b, "## %s\n\n```yaml\n%s```\n\nModule verdict:\n\n```yaml\n%s```\n\n", m.Module, renderYAML(mv), renderYAML(m.Verdict))
	}

	b.WriteString("# Implementation\n\n")
	for _, m := range selectCode(s, in.Modules) {
		b.WriteString(renderFiles("Module "+m.Module, m.Code))
	}
	b.WriteString("Judge the system.\n")

	var out SystemVerdict
	resp, err := complete(ctx, v.LLM, systemValidatorSystem, b.String(), systemVerdictSchema, &out)
	if err != nil {
		return out, resp, fmt.Errorf("system validator: %w", err)
	}
	var ids []string
	for _, g := range s.System.Goals {
		ids = append(ids, g.ID)
	}
	out.Goals = reconcileGoals(ids, out.Goals)
	normalizeGoals(out.Goals)
	capGoals(out.Goals, func(id string) (bool, string) {
		v := verifyOf(s, id)
		for _, m := range in.Modules {
			mod := s.Module(m.Module)
			if mod == nil || !contains(mod.Goals, id) {
				continue
			}
			if m.Error != "" {
				return true, fmt.Sprintf("module %s failed to build; a goal it is accountable for is at best partial", m.Module)
			}
			if !m.TestsOK && needsPassingTests(v) {
				return true, fmt.Sprintf("module %s's tests failed; a goal verified by %s cannot be achieved", m.Module, v)
			}
		}
		return false, ""
	})
	if st, ok := normalizeIntentStatus(string(out.IntentStatus)); !ok {
		out.IntentRationale = fmt.Sprintf("[aspect] unknown intent status %q read as %s. %s", out.IntentStatus, st, out.IntentRationale)
		out.IntentStatus = st
	} else {
		out.IntentStatus = st
	}
	for i := range out.Integration {
		f := &out.Integration[i]
		if st, ok := normalizeGoalStatus(string(f.Status)); !ok {
			f.Note = fmt.Sprintf("[aspect] unknown status %q read as %s. %s", f.Status, st, f.Note)
			f.Status = st
		} else {
			f.Status = st
		}
	}
	return out, resp, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// selectCode returns the modules whose implementation is shown. Modules
// that provide or consume surfaces always are; the rest are included while
// the budget allows, largest last so small helpers survive.
func selectCode(s *spec.Spec, mods []ModuleEvidence) []ModuleEvidence {
	size := func(m ModuleEvidence) int {
		n := 0
		for _, f := range m.Code {
			n += len(f.Content)
		}
		return n
	}
	var must, rest []ModuleEvidence
	for _, m := range mods {
		mod := s.Module(m.Module)
		if mod != nil && (len(mod.Surfaces) > 0 || len(mod.Consumes) > 0) {
			must = append(must, m)
		} else {
			rest = append(rest, m)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return size(rest[i]) < size(rest[j]) })
	used := 0
	for _, m := range must {
		used += size(m)
	}
	out := must
	for _, m := range rest {
		if used+size(m) > codeBudget {
			break
		}
		used += size(m)
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Module < out[j].Module })
	return out
}

func tierOutline(s *spec.Spec) any {
	type tierView struct {
		Name      string   `yaml:"name"`
		Intent    string   `yaml:"intent"`
		Language  string   `yaml:"language"`
		DependsOn []string `yaml:"depends_on,omitempty"`
		Modules   []string `yaml:"modules"`
	}
	var views []tierView
	for _, tr := range s.Tiers {
		v := tierView{tr.Name, tr.Intent, tr.Language, tr.DependsOn, nil}
		for _, m := range tr.Modules {
			v.Modules = append(v.Modules, m.Name)
		}
		views = append(views, v)
	}
	return views
}

func dbOwner(db *spec.Database) string {
	switch db.Tier {
	case "":
		return "system"
	case spec.External:
		return "external"
	default:
		return "tier " + db.Tier
	}
}
