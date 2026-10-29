package agents

import (
	"context"
	"fmt"
	"strings"

	"github.com/Pense-Laranja/aspect/internal/llm"
	"github.com/Pense-Laranja/aspect/internal/spec"
	"github.com/Pense-Laranja/aspect/internal/workspace"
)

// GoalStatus is the Validator's conclusion about one goal.
type GoalStatus string

const (
	Achieved     GoalStatus = "achieved"
	Partial      GoalStatus = "partial"
	NotAchieved  GoalStatus = "not_achieved"
	Unverifiable GoalStatus = "unverifiable"
)

// IntentStatus is the Validator's conclusion about a module's intent.
type IntentStatus string

const (
	Aligned  IntentStatus = "aligned"
	Drifted  IntentStatus = "drifted"
	Violated IntentStatus = "violated"
)

// ValidateInput is the evidence the Validator judges.
type ValidateInput struct {
	Task
	// GoalIDs the module is accountable for.
	GoalIDs []string
	Code    []workspace.File
	Tests   []workspace.File
	// TestResult is the real outcome of running the tests; the Validator must
	// not assume tests pass.
	TestResult workspace.Result
	// Coverage is the Tester's claimed scenario mapping, which the Validator
	// checks against the test files.
	Coverage []ScenarioCoverage
	// Concerns raised by the Coder and Tester.
	Concerns []string
}

// GoalVerdict is one goal's judgement with the evidence behind it.
type GoalVerdict struct {
	ID         string     `json:"id"`
	Status     GoalStatus `json:"status"`
	Evidence   string     `json:"evidence"`
	Gaps       []string   `json:"gaps"`
	Confidence float64    `json:"confidence"`
}

// ScenarioVerdict says whether a scenario is genuinely covered by a test.
type ScenarioVerdict struct {
	Scenario string `json:"scenario"`
	Covered  bool   `json:"covered"`
	Test     string `json:"test"`
	Note     string `json:"note"`
}

// Verdict is the Validator's full report for one module.
type Verdict struct {
	Module          string            `json:"module"`
	Goals           []GoalVerdict     `json:"goals"`
	IntentStatus    IntentStatus      `json:"intent_status"`
	IntentRationale string            `json:"intent_rationale"`
	Scenarios       []ScenarioVerdict `json:"scenarios"`
	Recommendations []string          `json:"recommendations"`
}

// Validator judges whether the module achieves its goals and honours its
// intent. It is deliberately separate from the Coder and Tester: the agent
// that wrote the code must not be the one that grades it.
type Validator struct {
	LLM llm.Client
}

const validatorSystem = `You are the Validator agent in Aspect, a pipeline that builds software from a formal specification.

You did not write this code or these tests. Your job is to decide, goal by goal, whether the module achieves what the specification states, and whether the implementation stays true to the module's intent. You are the sceptical reviewer the Coder and Tester were told to convince.

How to judge:
- A goal verified by "test" is achieved only if a test exists that genuinely exercises it and the real test run passed. Passing tests that do not exercise the goal are not evidence.
- A goal verified by "invariant" needs both a property-style test and your own reading of the code for paths the test cannot reach (error paths, concurrency, integer overflow).
- A goal verified by "review" is judged from the code alone; cite the lines that support your conclusion.
- Check the Tester's coverage claims: open each named test and confirm it does what the scenario says. Mark uncovered scenarios.
- Intent drift means the code works but solves a different or narrower problem than the stated intent, or leaks responsibilities that belong to another module.
- For owned entities, check the persistent type matches the declared fields, keys, uniqueness and relations, and that no other write path exists outside this module. For surfaces, check routes, methods, request and response shapes, error cases and auth against the spec, and that the named framework is used the way its guidance says.
- If the test run failed, no goal verified by test or invariant can be "achieved".
- Confidence is your honest probability that the status is right. Prefer "partial" with concrete gaps over an optimistic "achieved".
- Answer with JSON matching the schema you were given. Evidence must reference file names and identifiers, not vague impressions.`

var verdictSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"module", "goals", "intent_status", "intent_rationale", "scenarios", "recommendations"},
	"properties": map[string]any{
		"module": map[string]any{"type": "string"},
		"goals": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"id", "status", "evidence", "gaps", "confidence"},
				"properties": map[string]any{
					"id":         map[string]any{"type": "string"},
					"status":     map[string]any{"type": "string", "enum": []string{"achieved", "partial", "not_achieved", "unverifiable"}},
					"evidence":   map[string]any{"type": "string"},
					"gaps":       stringList(),
					"confidence": map[string]any{"type": "number"},
				},
			},
		},
		"intent_status":    map[string]any{"type": "string", "enum": []string{"aligned", "drifted", "violated"}},
		"intent_rationale": map[string]any{"type": "string"},
		"scenarios": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"scenario", "covered", "test", "note"},
				"properties": map[string]any{
					"scenario": map[string]any{"type": "string"},
					"covered":  map[string]any{"type": "boolean"},
					"test":     map[string]any{"type": "string"},
					"note":     map[string]any{"type": "string"},
				},
			},
		},
		"recommendations": stringList(),
	},
}

// Judge produces the verdict for one module.
func (v *Validator) Judge(ctx context.Context, in ValidateInput) (Verdict, llm.Response, error) {
	var b strings.Builder
	b.WriteString(renderContext(in.Task))
	b.WriteString(renderModule(in.Task, "Module under judgement"))
	fmt.Fprintf(&b, "Goals to reach a verdict on: %s\n\n", strings.Join(in.GoalIDs, ", "))
	b.WriteString(renderFiles("Implementation", in.Code))
	b.WriteString(renderFiles("Tests", in.Tests))
	status := "PASSED"
	if !in.TestResult.OK {
		status = "FAILED"
	}
	fmt.Fprintf(&b, "## Test run: %s (%s)\n\n```\n%s\n```\n\n", status, in.TestResult.Duration.Round(1e6), truncate(in.TestResult.Output, 12000))
	scenarioIDs := scenarioIDsOf(in.Module)
	claims, rejected := checkCoverage(scenarioIDs, in.Coverage, in.Tests)
	if len(claims) > 0 {
		fmt.Fprintf(&b, "## Tester's coverage claims\n\nEvery test named here is defined in the test files (checked mechanically); whether it genuinely exercises the scenario is for you to judge.\n\n```yaml\n%s```\n\n", renderYAML(claims))
	}
	if len(rejected) > 0 {
		fmt.Fprintf(&b, "## Coverage claims rejected before judgement\n\nThe Tester claimed these, and the claims do not hold up mechanically:\n\n```yaml\n%s```\n\n", renderYAML(rejected))
	}
	if len(in.Concerns) > 0 {
		fmt.Fprintf(&b, "## Concerns raised by Coder and Tester\n\n```yaml\n%s```\n\n", renderYAML(in.Concerns))
	}
	b.WriteString("Judge the module.\n")

	var out Verdict
	resp, err := complete(ctx, v.LLM, validatorSystem, b.String(), verdictSchema, &out)
	if err != nil {
		return out, resp, fmt.Errorf("validator: %w", err)
	}
	out.Module = in.Module.Name
	out.Goals = reconcileGoals(in.GoalIDs, out.Goals)
	normalizeGoals(out.Goals)
	if !in.TestResult.OK {
		capGoals(out.Goals, func(id string) (bool, string) {
			v := verifyOf(in.Spec, id)
			if !needsPassingTests(v) {
				return false, ""
			}
			return true, fmt.Sprintf("the test run failed; a goal verified by %s cannot be achieved", v)
		})
	}
	if st, ok := normalizeIntentStatus(string(out.IntentStatus)); !ok {
		out.IntentRationale = fmt.Sprintf("[aspect] unknown intent status %q read as %s. %s", out.IntentStatus, st, out.IntentRationale)
		out.IntentStatus = st
	} else {
		out.IntentStatus = st
	}
	out.Scenarios = reconcileScenarios(scenarioIDs, out.Scenarios)
	for _, r := range rejected {
		out.Recommendations = append(out.Recommendations, "[aspect] coverage "+r)
	}
	return out, resp, nil
}

func scenarioIDsOf(m *spec.Module) []string {
	ids := make([]string, 0, len(m.Scenarios))
	for _, s := range m.Scenarios {
		ids = append(ids, s.ID)
	}
	return ids
}

// reconcileGoals guarantees one verdict per accountable goal: goals the model
// forgot become "unverifiable" so silence never reads as success, and goals
// it invented are dropped.
func reconcileGoals(want []string, got []GoalVerdict) []GoalVerdict {
	byID := map[string]GoalVerdict{}
	for _, g := range got {
		byID[g.ID] = g
	}
	out := make([]GoalVerdict, 0, len(want))
	for _, id := range want {
		if g, ok := byID[id]; ok {
			out = append(out, g)
			continue
		}
		out = append(out, GoalVerdict{ID: id, Status: Unverifiable, Evidence: "the Validator returned no verdict for this goal", Confidence: 0})
	}
	return out
}
