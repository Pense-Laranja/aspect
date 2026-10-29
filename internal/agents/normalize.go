package agents

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Pense-Laranja/aspect/internal/spec"
	"github.com/Pense-Laranja/aspect/internal/workspace"
)

// The checks in this file enforce in Go what the Validator prompts ask for
// in prose. Structured outputs keep the enums honest on the main path, but
// the schema-less fallback does not, and a rule such as "a failed test run
// achieves nothing" should not depend on the model remembering it.

// normalizeGoalStatus maps what the model wrote onto a GoalStatus. Anything
// unrecognised becomes Unverifiable, never a success.
func normalizeGoalStatus(s string) (GoalStatus, bool) {
	switch key(s) {
	case "achieved", "pass", "passed", "met", "done":
		return Achieved, true
	case "partial", "partially_achieved", "incomplete":
		return Partial, true
	case "not_achieved", "failed", "fail", "unmet", "missing":
		return NotAchieved, true
	case "unverifiable", "unknown", "unverified":
		return Unverifiable, true
	}
	return Unverifiable, false
}

// normalizeIntentStatus maps what the model wrote onto an IntentStatus.
// Anything unrecognised becomes Drifted, the non-success that still asks
// for a human look rather than a block.
func normalizeIntentStatus(s string) (IntentStatus, bool) {
	switch key(s) {
	case "aligned", "ok", "true":
		return Aligned, true
	case "drifted", "drift", "partial":
		return Drifted, true
	case "violated", "violation", "broken":
		return Violated, true
	}
	return Drifted, false
}

func key(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.NewReplacer(" ", "_", "-", "_").Replace(s)
}

// clampConfidence brings a probability into [0, 1]. A value between 1 and
// 100 is read as a percentage, the mistake models actually make.
func clampConfidence(c float64) float64 {
	switch {
	case c != c: // NaN
		return 0
	case c > 1 && c <= 100:
		return c / 100
	case c > 1:
		return 1
	case c < 0:
		return 0
	}
	return c
}

// normalizeGoals applies the enum and range rules to every goal verdict.
func normalizeGoals(goals []GoalVerdict) {
	for i := range goals {
		g := &goals[i]
		if st, ok := normalizeGoalStatus(string(g.Status)); !ok {
			g.Gaps = append(g.Gaps, fmt.Sprintf("the Validator answered with an unknown status %q; read as %s", g.Status, st))
			g.Status = st
		} else {
			g.Status = st
		}
		g.Confidence = clampConfidence(g.Confidence)
	}
}

// capGoals downgrades "achieved" to "partial" for goals whose evidence is
// compromised: failed says whether that is so for a goal, and why. The gap
// is recorded so the report shows the verdict was capped by the pipeline,
// not by the model.
func capGoals(goals []GoalVerdict, failed func(id string) (bool, string)) {
	for i := range goals {
		g := &goals[i]
		if g.Status != Achieved {
			continue
		}
		if bad, why := failed(g.ID); bad {
			g.Status = Partial
			g.Gaps = append(g.Gaps, "[aspect] "+why)
		}
	}
}

// verifyOf returns a goal's verification method, test when the spec does
// not know the goal (the strict reading).
func verifyOf(s *spec.Spec, id string) spec.VerifyMethod {
	if g := s.Goal(id); g != nil && g.Verify != "" {
		return g.Verify
	}
	return spec.VerifyTest
}

// needsPassingTests reports whether a goal's verification method depends on
// the test run. Review-only goals are judged from the code.
func needsPassingTests(v spec.VerifyMethod) bool { return v != spec.VerifyReview }

// reconcileScenarios guarantees one verdict per spec scenario: the ones the
// model skipped are reported uncovered, the ones it invented are dropped.
func reconcileScenarios(want []string, got []ScenarioVerdict) []ScenarioVerdict {
	byID := map[string]ScenarioVerdict{}
	for _, s := range got {
		byID[strings.TrimSpace(s.Scenario)] = s
	}
	out := make([]ScenarioVerdict, 0, len(want))
	for _, id := range want {
		if s, ok := byID[id]; ok {
			s.Scenario = id
			out = append(out, s)
			continue
		}
		out = append(out, ScenarioVerdict{Scenario: id, Covered: false, Note: "the Validator returned no verdict for this scenario"})
	}
	return out
}

// checkCoverage verifies the Tester's claims mechanically before the
// Validator sees them: a claim must name a spec scenario, and every test it
// names must appear as an identifier in the test files. The cleaned claims
// go to the Validator; the rejected ones are listed so it, and the report,
// know the Tester overstated.
func checkCoverage(scenarios []string, claims []ScenarioCoverage, tests []workspace.File) (valid []ScenarioCoverage, rejected []string) {
	known := map[string]bool{}
	for _, id := range scenarios {
		known[id] = true
	}
	var all strings.Builder
	for _, f := range tests {
		all.WriteString(f.Content)
		all.WriteByte('\n')
	}
	corpus := all.String()
	for _, c := range claims {
		id := strings.TrimSpace(c.Scenario)
		if !known[id] {
			rejected = append(rejected, fmt.Sprintf("claim for %q: no such scenario in the module spec", c.Scenario))
			continue
		}
		var found []string
		for _, name := range c.Tests {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if identifierPresent(corpus, name) {
				found = append(found, name)
			} else {
				rejected = append(rejected, fmt.Sprintf("claim for %s names %s, which no test file defines", id, name))
			}
		}
		if len(found) > 0 {
			valid = append(valid, ScenarioCoverage{Scenario: id, Tests: found})
		} else if len(c.Tests) == 0 {
			rejected = append(rejected, fmt.Sprintf("claim for %s names no test", id))
		}
	}
	return valid, rejected
}

// identifierPresent reports whether name occurs in text as a whole word. A
// claim may be written as "TestFoo_S1" or "TestFoo/S1" (a subtest); the part
// before the slash is what the file must define.
func identifierPresent(text, name string) bool {
	if i := strings.Index(name, "/"); i > 0 {
		name = name[:i]
	}
	if i := strings.Index(name, "("); i > 0 {
		name = name[:i]
	}
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
	return re.MatchString(text)
}
