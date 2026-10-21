package review

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/eideroliveira/aspect/internal/gate"
)

// Reports must decode under `aspect gate check`, which decodes strictly
// (types, unknown fields, enums, locations) and treats an invalid report as
// missing. Models get the shape wrong: in run local-20261007T095818Z
// observability-reviewer and i18n-reviewer wrote "summary" as an object and
// both reports were rejected; in run local-20261007T111658Z
// test-adequacy-reviewer wrote category "quality".

const testBase, testHead = "52b0cbb58fb25dea8828091fad06f317f1173829", "8e52423be982067cbda2f39a2f39e0904374ecfb"

func mustObject(t *testing.T, s string) *Object {
	t.Helper()
	v, err := parseValue([]byte(s))
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	o, ok := v.(*Object)
	if !ok {
		t.Fatalf("%s is not an object", s)
	}
	return o
}

// messyReports are what models actually wrote, keyed by agent and mode.
var messyReports = []struct{ agent, mode, raw string }{
	{"observability-reviewer", "advisory", `{"summary": {"total_files_changed": 5, "total_lines_added": 923, "total_lines_removed": 0}, "findings": [], "handoffs": [], "changes": []}`},
	{"i18n-reviewer", "gate", `{"summary": {"overall_score": 4.2, "comment": "The new features are well-implemented."}, "findings": [], "handoffs": [], "changes": []}`},
	{"quality-reviewer", "gate", `{
		"summary": ["Mostly fine.", {"risk": "low"}],
		"verdict": "pass",
		"findings": [
			{"id": "q-1", "severity": "HIGH", "category": "quality", "title": 42,
			 "evidence": ["line one", "line two"], "recommendation": {"do": "fix it"},
			 "confidence": 85, "location": "models/a.go:12-20", "extra": true},
			{"id": "q-1", "severity": "medium", "category": "architecture", "title": "Duplicate id",
			 "evidence": "x", "recommendation": "y", "confidence": "high", "file": "models/b.go", "line": "7"},
			{"severity": "low", "category": "Error Handling", "title": "Zero line",
			 "evidence": "x", "recommendation": "y", "location": {"file": "c.go", "line": 0, "end_line": 9}},
			{"severity": "nit", "category": null, "title": "", "evidence": null, "recommendation": null},
			"not a finding"
		],
		"handoffs": [{"to": "quality-reviewer", "reason": "self"}, {"to": "red-team", "reason": "check auth", "priority": 1}, {"reason": "no target"}],
		"changes": [{"file": "a.go", "reason": "edited"}],
		"adrs": [
			{"slug": "Bad Slug", "title": "t", "body": "b"},
			{"slug": "use-queue", "title": "Use a queue", "body": "Because.", "finding": "q-1", "status": "proposed"},
			{"slug": "orphan-ref", "title": "Orphan", "body": "Body.", "finding": "nope"}
		]
	}`},
	{"test-adequacy-reviewer", "gate", `{"summary": "One coverage gap.", "findings": [{
		"id": "test-adequacy-reviewer-1", "severity": "medium", "category": "quality",
		"title": "Missing unit tests for SetReviewCommentRoute state transitions and validation",
		"evidence": "Observed during review", "recommendation": "Review and address the issue",
		"confidence": 0.9, "location": {"file": "modules/translation/review_comments.go", "line": 1279}}]}`},
}

func normalized(t *testing.T, raw, agent, mode string) *Report {
	t.Helper()
	r, err := Normalize(mustObject(t, raw), AgentRun{Agent: agent, Mode: mode, Base: testBase, Head: testHead})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return r
}

func TestAsTextFlattensJSONValues(t *testing.T) {
	o := mustObject(t, `{"s": {"score": 3.6, "comments": ["The implementation is well-structured.", "Input validation could be tightened."]},
		"n": null, "l": ["a", {"b": 1}], "t": "  kept  ", "f": 85.0, "e": 1e-05, "b": true}`)
	cases := map[string]string{
		"s": "score: 3.6; comments: The implementation is well-structured.; Input validation could be tightened.",
		"n": "", "l": "a; b: 1", "t": "kept", "f": "85.0", "e": "1e-05", "b": "True",
	}
	for k, want := range cases {
		if got := asText(o.Get(k)); got != want {
			t.Errorf("asText(%s) = %q, want %q", k, got, want)
		}
	}
}

func TestObjectSummariesBecomeText(t *testing.T) {
	if got := normalized(t, messyReports[0].raw, "observability-reviewer", "advisory").Summary; got != "total_files_changed: 5; total_lines_added: 923; total_lines_removed: 0" {
		t.Errorf("summary = %q", got)
	}
	if got := normalized(t, messyReports[1].raw, "i18n-reviewer", "gate").Summary; got != "overall_score: 4.2; comment: The new features are well-implemented." {
		t.Errorf("summary = %q", got)
	}
	if got := normalized(t, `{"summary": {}}`, "quality-reviewer", "gate").Summary; got != "Review completed." {
		t.Errorf("summary = %q", got)
	}
}

func TestCategoriesAreAspects(t *testing.T) {
	r := normalized(t, messyReports[2].raw, "quality-reviewer", "gate")
	var got []string
	for _, f := range r.Findings {
		got = append(got, f.Category)
	}
	if want := []string{"correctness", "design", "correctness", "correctness"}; !slices.Equal(got, want) {
		t.Errorf("categories = %v, want %v", got, want)
	}
	// An unknown category falls back to the agent's own concern.
	if c := normalized(t, `{"findings": [{"severity": "info", "category": "mystery", "title": "t"}]}`, "i18n-reviewer", "gate").Findings[0].Category; c != "i18n" {
		t.Errorf("unknown category = %q", c)
	}
}

func TestVagueCategoryTakesTheAgentsConcern(t *testing.T) {
	want := map[string]string{
		"test-adequacy-reviewer": "test-gap", "api-contract-reviewer": "contract",
		"database-reviewer": "data", "spec-keeper": "spec-drift", "red-team": "security",
		"security-red-team": "security", "architect": "design", "quality-reviewer": "correctness",
		"adversarial-reviewer": "correctness", "performance-reviewer": "performance",
		"observability-reviewer": "observability", "unknown-agent": "correctness",
	}
	for agent, cat := range want {
		for _, vague := range []string{`"quality"`, `"General"`, `""`, `null`} {
			raw := `{"findings": [{"severity": "info", "category": ` + vague + `, "title": "t"}]}`
			if got := normalized(t, raw, agent, "gate").Findings[0].Category; got != cat {
				t.Errorf("%s / %s: category %q, want %q", agent, vague, got, cat)
			}
		}
	}
}

func TestSpecificCategoryBeatsTheAgentsConcern(t *testing.T) {
	if c := normalized(t, `{"findings": [{"severity": "info", "category": "SQL", "title": "t"}]}`, "test-adequacy-reviewer", "gate").Findings[0].Category; c != "data" {
		t.Errorf("SQL = %q", c)
	}
	if c := normalized(t, `{"findings": [{"severity": "info", "category": "security", "title": "t"}]}`, "i18n-reviewer", "gate").Findings[0].Category; c != "security" {
		t.Errorf("security = %q", c)
	}
}

func TestFindingsAreCoercedWithoutLosingSeverity(t *testing.T) {
	fs := normalized(t, messyReports[2].raw, "quality-reviewer", "gate").Findings
	if len(fs) != 4 {
		t.Fatalf("findings = %d, want 4 (the string is not a finding)", len(fs))
	}
	q1, dup, zero, weird := fs[0], fs[1], fs[2], fs[3]
	if q1.Severity != "high" || q1.Title != "42" || q1.Confidence != 0.85 || q1.Evidence != "line one; line two" {
		t.Errorf("q1 = %+v", q1)
	}
	if *q1.Location != (gate.Location{File: "models/a.go", Line: 12, EndLine: 20}) {
		t.Errorf("q1 location = %+v", *q1.Location)
	}
	if dup.ID == "q-1" || dup.Confidence != 0.9 || *dup.Location != (gate.Location{File: "models/b.go", Line: 7}) {
		t.Errorf("dup = %+v %+v", dup, *dup.Location)
	}
	if *zero.Location != (gate.Location{File: "c.go"}) {
		t.Errorf("a whole-file finding has no line, so no end_line: %+v", *zero.Location)
	}
	if weird.Severity != "info" || weird.Title != "Code finding" || weird.Location != nil {
		t.Errorf("weird = %+v", weird)
	}
}

func TestNonInfoFindingWithoutLocationFailsTheAgent(t *testing.T) {
	_, err := Normalize(mustObject(t, `{"findings": [{"severity": "critical", "title": "SQL injection", "location": {"line": 3}}]}`),
		AgentRun{Agent: "quality-reviewer", Mode: "gate", Base: testBase, Head: testHead})
	if !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("err = %v", err)
	}
}

func TestHandoffsChangesAndADRsKeepOnlyValidEntries(t *testing.T) {
	r := normalized(t, messyReports[2].raw, "quality-reviewer", "gate")
	if want := []gate.Handoff{{To: "red-team", Reason: "check auth"}}; !slices.Equal(r.Handoffs, want) {
		t.Errorf("handoffs = %+v", r.Handoffs)
	}
	if len(r.Changes) != 0 {
		t.Errorf("a gate report lists changes: %+v", r.Changes)
	}
	want := []gate.ADR{
		{Slug: "use-queue", Title: "Use a queue", Body: "Because.", Finding: "q-1"},
		{Slug: "orphan-ref", Title: "Orphan", Body: "Body."},
	}
	if !slices.Equal(r.ADRs, want) {
		t.Errorf("adrs = %+v", r.ADRs)
	}
	author := normalized(t, `{"summary": "Wrote docs.", "findings": [], "changes": [{"file": "README.md", "reason": "docs", "diff": "+x"}, {"reason": "no file"}]}`, "doc-writer", "author")
	if want := []gate.Written{{File: "README.md", Reason: "docs"}}; !slices.Equal(author.Changes, want) {
		t.Errorf("author changes = %+v", author.Changes)
	}
}

// The real decoder is the oracle: every normalized report must check "ok".
func TestGateCheckAcceptsEveryNormalizedReport(t *testing.T) {
	cfgYAML := "gates:\n"
	plan := &gate.Plan{Schema: gate.PlanSchema, Spec: gate.NoSpec, Base: testBase, Head: testHead}
	inputs := map[string]gate.Input{}
	for _, m := range messyReports {
		cfgYAML += "  - agent: " + m.agent + "\n    paths: [\"**\"]\n    mode: " + m.mode + "\n"
		plan.Agents = append(plan.Agents, gate.Planned{Agent: m.agent, Mode: m.mode, Reason: "test", Files: []string{"a.go"}})
		b, err := json.Marshal(normalized(t, m.raw, m.agent, m.mode))
		if err != nil {
			t.Fatal(err)
		}
		inputs[m.agent] = gate.Input{Data: b}
	}
	cfg, err := gate.ParseConfig([]byte(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	res := gate.Check(cfg, plan, inputs, false)
	for _, a := range res.Agents {
		if a.Status != gate.StatusOK {
			t.Errorf("%s: %s %s", a.Agent, a.Status, a.Error)
		}
	}
}

func TestExtractJSON(t *testing.T) {
	for _, text := range []string{
		`{"summary": "ok"}`,
		"Here is the report:\n```json\n{\"summary\": \"ok\"}\n```\nDone.",
		`Sure! {"summary": "ok"} hope it helps`,
	} {
		o, err := extractJSON(text)
		if err != nil || asText(o.Get("summary")) != "ok" {
			t.Errorf("extractJSON(%q) = %v, %v", text, o, err)
		}
	}
	for _, text := range []string{"no json here", `["a list"]`, `{broken`} {
		if _, err := extractJSON(text); err == nil {
			t.Errorf("extractJSON(%q) accepted it", text)
		}
	}
}

// A severity decides the verdict: a known alias maps to the severity it
// means, anything else makes the report invalid instead of becoming info,
// which could turn a blocker into a pass.
func TestSeverityAliasesAndUnknownSeverities(t *testing.T) {
	for raw, want := range map[string]string{"Major": "high", "blocker": "high", "warning": "medium", "minor": "low", "nit": "info", "CRITICAL": "critical"} {
		r := normalized(t, `{"findings": [{"severity": "`+raw+`", "title": "t", "location": {"file": "a.go", "line": 1}}]}`, "quality-reviewer", "gate")
		if got := r.Findings[0].Severity; got != want {
			t.Errorf("%s -> %s, want %s", raw, got, want)
		}
	}
	for _, raw := range []string{`"weird"`, `""`, `null`} {
		_, err := Normalize(mustObject(t, `{"findings": [{"severity": `+raw+`, "title": "SQL injection", "location": {"file": "a.go", "line": 3}}]}`),
			AgentRun{Agent: "security-reviewer", Mode: "gate", Base: testBase, Head: testHead})
		if !errors.Is(err, ErrInvalidReport) {
			t.Errorf("severity %s: want ErrInvalidReport, got %v", raw, err)
		}
	}
}

// NormalizeJSON stamps the run on a report that leaves it out or
// abbreviates a commit, and refuses one that names another agent, mode or
// revision rather than relabelling it, and anything that is not a report.
func TestNormalizeJSON(t *testing.T) {
	run := AgentRun{Agent: "quality-reviewer", Mode: "gate", Base: testBase, Head: testHead}
	decode := func(b []byte) gate.Report {
		t.Helper()
		var r gate.Report
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	b, err := NormalizeJSON([]byte(`{"schema": "`+gate.ReportSchema+`", "summary": "ok", "head": "`+testHead[:7]+`", "findings": [{"severity": "high", "confidence": 0.8, "title": "t", "location": {"file": "a.go", "line": 1}}]}`), run)
	if err != nil {
		t.Fatal(err)
	}
	if r := decode(b); r.Agent != run.Agent || r.Mode != "gate" || r.Base != testBase || r.Head != testHead {
		t.Errorf("stamped = %+v", r)
	}
	for name, raw := range map[string]string{
		"not JSON":        `{"findings": [`,
		"array":           `[{"findings": []}]`,
		"other head":      `{"head": "0123456789abcdef", "findings": []}`,
		"other base":      `{"base": "0123456789abcdef", "findings": []}`,
		"short head":      `{"head": "` + testHead[:6] + `", "findings": []}`,
		"string finding":  `{"findings": ["HIGH: SQL injection in a.go"]}`,
		"no findings":     `{"summary": "Found a critical SQL injection in a.go:3"}`,
		"null findings":   `{"findings": null}`,
		"confidence 9":    `{"findings": [{"severity": "critical", "confidence": 9, "title": "t", "location": {"file": "a.go", "line": 1}}]}`,
		"confidence 1.5":  `{"findings": [{"severity": "high", "confidence": 1.5, "title": "t", "location": {"file": "a.go", "line": 1}}]}`,
		"confidence -0.9": `{"findings": [{"severity": "critical", "confidence": -0.9, "title": "t", "location": {"file": "a.go", "line": 1}}]}`,
		"confidence 95%":  `{"findings": [{"severity": "critical", "confidence": "95%", "title": "t", "location": {"file": "a.go", "line": 1}}]}`,
		"other schema":    `{"schema": "aspect-review/v2", "findings": []}`,
		"findings map":    `{"findings": {"a": {"severity": "high"}}}`,
		"other agent":     `{"agent": "red-team", "findings": []}`,
		"other mode":      `{"mode": "advisory", "findings": []}`,
		"no location":     `{"findings": [{"severity": "high", "title": "t"}]}`,
		"weird severity":  `{"findings": [{"severity": "weird", "title": "t", "location": {"file": "a.go", "line": 1}}]}`,
	} {
		if _, err := NormalizeJSON([]byte(raw), run); !errors.Is(err, ErrInvalidReport) {
			t.Errorf("%s: want ErrInvalidReport, got %v", name, err)
		}
	}
	// The run abbreviated, the report's commit in full: the same commit.
	short := AgentRun{Agent: run.Agent, Mode: run.Mode, Base: testBase[:7], Head: testHead[:7]}
	if _, err := NormalizeJSON([]byte(`{"base": "`+testBase+`", "head": "`+testHead+`", "findings": []}`), short); err != nil {
		t.Errorf("an abbreviated run matches the full commit: %v", err)
	}
	for _, bad := range []AgentRun{{}, {Agent: "a", Mode: "gates", Base: "b", Head: "h"}, {Agent: "a", Mode: "gate", Head: "h"}} {
		if _, err := NormalizeJSON([]byte(`{"findings": []}`), bad); err == nil || errors.Is(err, ErrInvalidReport) {
			t.Errorf("run %+v: want a refusal of the run, got %v", bad, err)
		}
		if _, err := Normalize(mustObject(t, `{"findings": []}`), bad); err == nil {
			t.Errorf("Normalize, run %+v: want a refusal of the run", bad)
		}
	}
	var syntax *json.SyntaxError
	if _, err := NormalizeJSON([]byte(`{"findings": [}`), run); !errors.As(err, &syntax) {
		t.Errorf("the parse error stays inspectable: %v", err)
	}
}
