package gate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testConfig = `
spec: aspect.yaml
gates:
  - agent: spec-keeper
    paths: ["**"]
    mode: gate
  - agent: adversarial-reviewer
    paths: ["**/*.go"]
    mode: gate
  - agent: security-red-team
    paths: ["**/*.go", ".github/**"]
    mode: gate
  - agent: architect
    paths: ["internal/**", "go.mod"]
    mode: advisory
    when: {min_changed_lines: 100, or_touches: ["go.mod"]}
thresholds:
  block: [critical, high]
  warn: [medium, low]
  min_confidence_to_block: 0.5
public_redaction: [critical, high]
`

func mustConfig(t *testing.T) *Config {
	t.Helper()
	c, err := ParseConfig([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"**", "a/b/c.go", true},
		{"**/*.go", "main.go", true},
		{"**/*.go", "internal/gate/plan.go", true},
		{"**/*.go", "internal/gate/plan.md", false},
		{".github/**", ".github/workflows/ci.yml", true},
		{".github/**", "github/x", false},
		{"internal/**", "internal", true},
		{"go.mod", "go.mod", true},
		{"go.mod", "sub/go.mod", false},
		{"**/aspect.yaml", "aspect.yaml", true},
		{"**/aspect.yaml", "examples/shop/aspect.yaml", true},
		{"cmd/*/main.go", "cmd/aspect/main.go", true},
		{"cmd/*/main.go", "cmd/a/b/main.go", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.name); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestParseConfigRejects(t *testing.T) {
	cases := map[string]string{
		"unknown key":      "gates: [{agent: a, paths: ['**'], mode: gate, extra: 1}]",
		"no gates":         "spec: x.yaml",
		"bad mode":         "gates: [{agent: a, paths: ['**'], mode: author}]",
		"duplicate agent":  "gates: [{agent: a, paths: ['**'], mode: gate}, {agent: a, paths: ['x'], mode: gate}]",
		"no paths":         "gates: [{agent: a, mode: gate}]",
		"bad pattern":      "gates: [{agent: a, paths: ['[x'], mode: gate}]",
		"unknown severity": "gates: [{agent: a, paths: ['**'], mode: gate}]\nthresholds: {block: [severe]}",
		"block and warn":   "gates: [{agent: a, paths: ['**'], mode: gate}]\nthresholds: {block: [high], warn: [high]}",
		"confidence range": "gates: [{agent: a, paths: ['**'], mode: gate}]\nthresholds: {block: [high], min_confidence_to_block: 2}",
	}
	for name, y := range cases {
		if _, err := ParseConfig([]byte(y)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseConfigDefaults(t *testing.T) {
	c, err := ParseConfig([]byte("gates: [{agent: a, paths: ['**'], mode: gate}]"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Spec != DefaultSpec || !slices.Equal(c.Thresholds.Block, []string{"critical", "high"}) {
		t.Errorf("defaults not applied: %+v", c)
	}
}

func TestRepositoryGatesYAML(t *testing.T) {
	c, err := LoadConfig(filepath.Join("..", "..", DefaultConfigPath))
	if err != nil {
		t.Fatal(err)
	}
	// Advisory agents may ship separately; a missing one only warns at run
	// time. A gate-mode agent without a file would block every PR.
	for _, g := range c.Gates {
		if g.Mode != "gate" {
			continue
		}
		if _, err := os.Stat(filepath.Join("..", "..", ".claude", "agents", g.Agent+".md")); err != nil {
			t.Errorf("gates.yaml names %s, which has no agent file: %v", g.Agent, err)
		}
	}
}

func agents(p *Plan) []string {
	var out []string
	for _, a := range p.Agents {
		out = append(out, a.Agent)
	}
	return out
}

func TestBuildPlan(t *testing.T) {
	c := mustConfig(t)
	cases := []struct {
		name    string
		changes []Change
		want    []string
	}{
		{"docs only", []Change{{Path: "README.md", Added: 3}}, []string{"spec-keeper"}},
		{"small go change", []Change{{Path: "internal/gate/plan.go", Added: 10, Deleted: 2}}, []string{"spec-keeper", "adversarial-reviewer", "security-red-team"}},
		{"large internal change", []Change{{Path: "internal/gate/plan.go", Added: 90, Deleted: 20}}, []string{"spec-keeper", "adversarial-reviewer", "security-red-team", "architect"}},
		{"go.mod touch", []Change{{Path: "go.mod", Added: 1}}, []string{"spec-keeper", "architect"}},
		{"ci change", []Change{{Path: ".github/workflows/ci.yml", Added: 1}}, []string{"spec-keeper", "security-red-team"}},
		{"nothing", nil, nil},
	}
	for _, tc := range cases {
		p := BuildPlan(c, "b", "h", tc.changes, "", c.Spec)
		if got := agents(p); !slices.Equal(got, tc.want) {
			t.Errorf("%s: agents = %v, want %v", tc.name, got, tc.want)
		}
	}
	p := BuildPlan(c, "b", "h", []Change{{Path: "a.go", Added: 1}, {Path: "README.md", Added: 4, Deleted: 1}}, "", c.Spec)
	if p.ChangedFiles != 2 || p.ChangedLines != 6 {
		t.Errorf("counts = %d files, %d lines", p.ChangedFiles, p.ChangedLines)
	}
	if ar, _ := p.Agent("adversarial-reviewer"); !slices.Equal(ar.Files, []string{"a.go"}) {
		t.Errorf("adversarial-reviewer files = %v", ar.Files)
	}
}

func TestBuildPlanWithModules(t *testing.T) {
	c, err := LoadConfig(filepath.Join("..", "..", DefaultConfigPath))
	if err != nil {
		t.Fatal(err)
	}
	specPath := filepath.Join("..", "..", "examples", "inventory", "aspect.yaml")
	changes := []Change{
		{Path: "ledger/ledger.go", Added: 10, Deleted: 2},
		{Path: "README.md", Added: 5},
	}
	p := BuildPlan(c, "base123", "head456", changes, "", specPath)
	if len(p.Modules) == 0 {
		t.Fatal("expected modules to be populated in plan")
	}
	var ledgerMod *ModuleContext
	for i := range p.Modules {
		if p.Modules[i].Name == "ledger" {
			ledgerMod = &p.Modules[i]
			break
		}
	}
	if ledgerMod == nil {
		t.Fatal("expected ledger module in plan.Modules")
	}
	if len(ledgerMod.Invariants) == 0 {
		t.Errorf("expected invariants on ledger module, got none")
	}
	if len(ledgerMod.Files) != 1 || ledgerMod.Files[0] != "ledger/ledger.go" {
		t.Errorf("expected ledger module to have ledger/ledger.go, got %v", ledgerMod.Files)
	}
	ar, ok := p.Agent("adversarial-reviewer")
	if !ok {
		t.Fatal("expected adversarial-reviewer in plan")
	}
	if !slices.Contains(ar.Modules, "ledger") {
		t.Errorf("expected adversarial-reviewer to reference ledger module, got %v", ar.Modules)
	}
}

// report builds a valid report JSON for an agent, with the given findings.
func report(agent, mode string, findings ...Finding) []byte {
	r := Report{Schema: ReportSchema, Agent: agent, Mode: mode, Base: "aaaaaaa1", Head: "bbbbbbb2", Summary: "checked", Findings: findings, Handoffs: []Handoff{}, Changes: []Written{}}
	if r.Findings == nil {
		r.Findings = []Finding{}
	}
	b, _ := json.Marshal(r)
	return b
}

func finding(id, severity string, confidence float64) Finding {
	return Finding{ID: id, Severity: severity, Category: "correctness", Title: "t", Location: &Location{File: "a.go", Line: 3}, Evidence: "e", Recommendation: "r", Confidence: confidence}
}

func TestDecodeReport(t *testing.T) {
	if _, err := DecodeReport(report("a", "gate", finding("a/1", "high", 0.9))); err != nil {
		t.Fatalf("valid report rejected: %v", err)
	}
	valid := string(report("a", "gate", finding("a/1", "high", 0.9)))
	cases := map[string]string{
		"unknown field":       strings.Replace(valid, `"summary"`, `"verdict":"pass","summary"`, 1),
		"missing handoffs":    strings.Replace(valid, `"handoffs":[],`, ``, 1),
		"missing confidence":  strings.Replace(valid, `,"confidence":0.9`, ``, 1),
		"wrong schema":        strings.Replace(valid, ReportSchema, "aspect-review/v0", 1),
		"bad severity":        strings.Replace(valid, `"high"`, `"severe"`, 1),
		"bad category":        strings.Replace(valid, `"correctness"`, `"style"`, 1),
		"confidence too high": strings.Replace(valid, `0.9`, `1.5`, 1),
		"trailing data":       valid + `{}`,
		"empty summary":       strings.Replace(valid, `"checked"`, `""`, 1),
		"zero line":           strings.Replace(valid, `"line":3`, `"line":0`, 1),
		"no location":         strings.Replace(valid, `"location":{"file":"a.go","line":3},`, ``, 1),
	}
	for name, doc := range cases {
		if _, err := DecodeReport([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	info := finding("a/1", "info", 1)
	info.Location = nil
	if _, err := DecodeReport(report("a", "gate", info)); err != nil {
		t.Errorf("info finding without location rejected: %v", err)
	}
	dup := report("a", "gate", finding("a/1", "low", 1), finding("a/1", "low", 1))
	if _, err := DecodeReport(dup); err == nil {
		t.Error("duplicate finding ids accepted")
	}
	var r Report
	json.Unmarshal(report("a", "gate"), &r)
	r.Changes = []Written{{File: "x", Reason: "y"}}
	if r.Validate() == nil {
		t.Error("gate report with changes accepted")
	}
}

func TestDecodeReportADRs(t *testing.T) {
	var r Report
	json.Unmarshal(report("architect", "advisory", finding("architect/1", "medium", 0.8)), &r)
	r.ADRs = []ADR{{Slug: "split-gate-package", Title: "Split", Finding: "architect/1", Body: "## Context"}}
	b, _ := json.Marshal(r)
	if _, err := DecodeReport(b); err != nil {
		t.Fatalf("valid ADR rejected: %v", err)
	}
	for name, a := range map[string]ADR{
		"bad slug":        {Slug: "Split Gate", Title: "t", Body: "b"},
		"no body":         {Slug: "s", Title: "t"},
		"unknown finding": {Slug: "s", Title: "t", Body: "b", Finding: "architect/9"},
	} {
		r.ADRs = []ADR{a}
		if r.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func plan(c *Config, files ...string) *Plan {
	var ch []Change
	for _, f := range files {
		ch = append(ch, Change{Path: f, Added: 1})
	}
	return BuildPlan(c, "aaaaaaa1", "bbbbbbb2", ch, "", c.Spec)
}

func TestCheck(t *testing.T) {
	c := mustConfig(t)
	p := plan(c, "a.go") // spec-keeper, adversarial-reviewer, security-red-team
	clean := map[string]Input{
		"spec-keeper":          {Data: report("spec-keeper", "gate")},
		"adversarial-reviewer": {Data: report("adversarial-reviewer", "gate")},
		"security-red-team":    {Data: report("security-red-team", "gate")},
	}
	with := func(agent string, in Input) map[string]Input {
		m := map[string]Input{}
		for k, v := range clean {
			m[k] = v
		}
		m[agent] = in
		return m
	}
	without := func(agent string) map[string]Input {
		m := with(agent, Input{})
		delete(m, agent)
		return m
	}
	cases := []struct {
		name    string
		reports map[string]Input
		want    string
	}{
		{"all clean", clean, Pass},
		{"high finding blocks", with("adversarial-reviewer", Input{Data: report("adversarial-reviewer", "gate", finding("x/1", "high", 0.9))}), Block},
		{"low confidence high only warns", with("adversarial-reviewer", Input{Data: report("adversarial-reviewer", "gate", finding("x/1", "high", 0.4))}), Warn},
		{"medium warns", with("spec-keeper", Input{Data: report("spec-keeper", "gate", finding("x/1", "medium", 1))}), Warn},
		{"info passes", with("spec-keeper", Input{Data: report("spec-keeper", "gate", Finding{ID: "x/1", Severity: "info", Category: "docs", Title: "t", Confidence: 1})}), Pass},
		{"missing report blocks", without("security-red-team"), Block},
		{"unreadable report blocks", with("security-red-team", Input{Err: errors.New("boom")}), Block},
		{"malformed report blocks", with("security-red-team", Input{Data: []byte(`{"schema":"aspect-review/v1"}`)}), Block},
		{"report from another agent blocks", with("security-red-team", Input{Data: report("spec-keeper", "gate")}), Block},
		{"report in wrong mode blocks", with("security-red-team", Input{Data: report("security-red-team", "advisory")}), Block},
	}
	for _, tc := range cases {
		r := Check(c, p, tc.reports, false)
		if r.Outcome != tc.want {
			t.Errorf("%s: outcome = %s, want %s (%+v)", tc.name, r.Outcome, tc.want, r.Agents)
		}
		if r.Failed() != (tc.want == Block) {
			t.Errorf("%s: Failed() = %v", tc.name, r.Failed())
		}
	}

	stale := report("spec-keeper", "gate")
	stale = []byte(strings.Replace(string(stale), "bbbbbbb2", "ccccccc3", 1))
	if r := Check(c, p, with("spec-keeper", Input{Data: stale}), false); r.Outcome != Block {
		t.Errorf("stale report: outcome = %s", r.Outcome)
	}

	blocked := with("adversarial-reviewer", Input{Data: report("adversarial-reviewer", "gate", finding("x/1", "critical", 1))})
	r := Check(c, p, blocked, true)
	if r.Outcome != Overridden || r.Failed() {
		t.Errorf("override: outcome = %s, failed = %v", r.Outcome, r.Failed())
	}
}

func TestCheckAdvisoryNeverBlocks(t *testing.T) {
	c := mustConfig(t)
	p := plan(c, "go.mod") // spec-keeper (gate), architect (advisory)
	reports := map[string]Input{
		"spec-keeper": {Data: report("spec-keeper", "gate")},
		"architect":   {Data: report("architect", "advisory", finding("architect/1", "critical", 1))},
	}
	if r := Check(c, p, reports, false); r.Outcome != Warn {
		t.Errorf("advisory critical: outcome = %s, want warn", r.Outcome)
	}
	delete(reports, "architect")
	if r := Check(c, p, reports, false); r.Outcome != Warn {
		t.Errorf("advisory missing: outcome = %s, want warn", r.Outcome)
	}
}

func TestCheckSortsBySeverity(t *testing.T) {
	c := mustConfig(t)
	p := plan(c, "README.md")
	r := Check(c, p, map[string]Input{"spec-keeper": {Data: report("spec-keeper", "gate",
		finding("s/1", "low", 1), finding("s/2", "high", 1), finding("s/3", "medium", 1), finding("s/4", "critical", 1))}}, false)
	a := r.Agents[0]
	if a.Blocking[0].ID != "s/4" || a.Blocking[1].ID != "s/2" || a.Warnings[0].ID != "s/3" || a.Warnings[1].ID != "s/1" {
		t.Errorf("not sorted: blocking %v warnings %v", a.Blocking, a.Warnings)
	}
}

func TestRender(t *testing.T) {
	c := mustConfig(t)
	p := plan(c, "a.go")
	sec := finding("security-red-team/1", "high", 0.9)
	sec.Category = "security"
	sec.Title = "SQL injection in <b>search</b>"
	sec.Evidence = "secret exploit path"
	reports := map[string]Input{
		"spec-keeper":          {Data: report("spec-keeper", "gate")},
		"adversarial-reviewer": {Data: report("adversarial-reviewer", "gate", finding("adversarial-reviewer/1", "medium", 0.8))},
		"security-red-team":    {Data: report("security-red-team", "gate", sec)},
	}
	r := Check(c, p, reports, false)

	private := Render(c, p, r, RenderOptions{})
	for _, want := range []string{CommentMarker, "Review gates: blocked", "| security-red-team | gate | ok | 1 | 0 |", "secret exploit path", "&lt;b&gt;search&lt;/b&gt;"} {
		if !strings.Contains(private, want) {
			t.Errorf("private render lacks %q:\n%s", want, private)
		}
	}
	if strings.Contains(private, "<b>") {
		t.Error("model text was not escaped")
	}

	public := Render(c, p, r, RenderOptions{Public: true})
	if strings.Contains(public, "secret exploit path") || !strings.Contains(public, "details withheld") {
		t.Errorf("public render did not redact:\n%s", public)
	}
	if !strings.Contains(public, "a.go:3") {
		t.Error("public render dropped the file")
	}
	if strings.Count(public, CommentMarker) != 1 {
		t.Error("marker must appear exactly once")
	}

	empty := Render(c, plan(c), Check(c, plan(c), nil, false), RenderOptions{})
	if !strings.Contains(empty, "No gate applies") {
		t.Errorf("empty plan render:\n%s", empty)
	}
	if strings.Contains(private, UpdateSpecLabel) {
		t.Error("offered the spec update with no spec drift")
	}

	drift := func(agent, severity string) Finding {
		f := finding(agent+"/1", severity, 0.9)
		f.Category = "spec-drift"
		return f
	}
	for _, tc := range []struct {
		name  string
		agent string
		f     Finding
		spec  string
		offer bool
	}{
		{"blocking drift", "spec-keeper", drift("spec-keeper", "high"), "_aspect/aspect.yaml", true},
		{"warning drift", "spec-keeper", drift("spec-keeper", "medium"), "_aspect/aspect.yaml", true},
		{"info drift", "spec-keeper", drift("spec-keeper", "info"), "_aspect/aspect.yaml", false},
		{"drift from another agent", "adversarial-reviewer", drift("adversarial-reviewer", "high"), "_aspect/aspect.yaml", false},
		{"root spec the workflow cannot edit", "spec-keeper", drift("spec-keeper", "high"), LegacySpec, false},
		{"spec named with ./", "spec-keeper", drift("spec-keeper", "high"), "./_aspect/aspect.yaml", true},
	} {
		rs := map[string]Input{}
		for k, v := range reports {
			rs[k] = v
		}
		rs["spec-keeper"] = Input{Data: report("spec-keeper", "gate")}
		rs[tc.agent] = Input{Data: report(tc.agent, "gate", tc.f)}
		pl := *p
		pl.Spec = tc.spec
		got := Render(c, &pl, Check(c, &pl, rs, false), RenderOptions{})
		if strings.Contains(got, "`"+UpdateSpecLabel+"` label") != tc.offer {
			t.Errorf("%s: offer = %v, want %v:\n%s", tc.name, !tc.offer, tc.offer, got)
		}
	}
}

// TestSchemaMatchesValidator keeps report.schema.json and the Go validator
// describing the same document.
func TestSchemaMatchesValidator(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".claude", "review", "report.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		AdditionalProperties *bool               `json:"additionalProperties"`
		Required             []string            `json:"required"`
		Properties           map[string]struct{} `json:"properties"`
		Defs                 map[string]struct {
			AdditionalProperties *bool `json:"additionalProperties"`
			Required             []string
			Properties           map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if s.AdditionalProperties == nil || *s.AdditionalProperties {
		t.Error("report must have additionalProperties: false")
	}
	for name, d := range s.Defs {
		if d.AdditionalProperties == nil || *d.AdditionalProperties {
			t.Errorf("$defs.%s must have additionalProperties: false", name)
		}
	}
	f := s.Defs["finding"].Properties
	if !slices.Equal(f["severity"].Enum, Severities) {
		t.Errorf("severity enum %v, Go has %v", f["severity"].Enum, Severities)
	}
	if !slices.Equal(f["category"].Enum, Categories) {
		t.Errorf("category enum %v, Go has %v", f["category"].Enum, Categories)
	}
	// Every Go field is a schema property and vice versa.
	jsonKeys := func(v any) []string {
		b, _ := json.Marshal(v)
		var m map[string]any
		json.Unmarshal(b, &m)
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		return keys
	}
	schemaKeys := func(m map[string]struct{}) []string {
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		return keys
	}
	full := Report{ADRs: []ADR{{}}}
	if got, want := jsonKeys(full), schemaKeys(s.Properties); !slices.Equal(got, want) {
		t.Errorf("report keys: Go %v, schema %v", got, want)
	}
	fullFinding := Finding{Location: &Location{EndLine: 1}}
	fp := map[string]struct{}{}
	for k := range f {
		fp[k] = struct{}{}
	}
	if got, want := jsonKeys(fullFinding), schemaKeys(fp); !slices.Equal(got, want) {
		t.Errorf("finding keys: Go %v, schema %v", got, want)
	}
}

func TestLastJSONBlock(t *testing.T) {
	text := "Summary.\n\n```json\n{\"first\": 1}\n```\n\nMore.\n\n```json\n{\"last\": 2}\n```\n"
	b, err := LastJSONBlock(text)
	if err != nil || strings.TrimSpace(string(b)) != `{"last": 2}` {
		t.Errorf("got %q, %v", b, err)
	}
	if _, err := LastJSONBlock("no block here"); err == nil {
		t.Error("missing block accepted")
	}
	b, err = LastJSONBlock("```json\n{\"a\": 1}\n```\n```json\n{\"unterminated\"")
	if err != nil || strings.TrimSpace(string(b)) != `{"a": 1}` {
		t.Errorf("unterminated trailing block: got %q, %v", b, err)
	}
}

func TestAgentText(t *testing.T) {
	cases := map[string]string{
		`{"type":"result","result":"claude text"}`:         "claude text",
		`{"session_id":"x","response":"gemini text"}`:      "gemini text",
		"plain ```json\n{}\n```":                           "plain ```json\n{}\n```",
		`{"session_id":"x","error":{"message":"bad key"}}`: `{"session_id":"x","error":{"message":"bad key"}}`,
	}
	for in, want := range cases {
		if got := AgentText([]byte(in)); got != want {
			t.Errorf("AgentText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWholeFileLocation(t *testing.T) {
	f := finding("spec-keeper/1", "high", 0.9)
	f.Location = &Location{File: "aspect.yaml"}
	b := report("spec-keeper", "gate", f)
	if strings.Contains(string(b), `"line"`) {
		t.Fatalf("a whole-file location should omit line: %s", b)
	}
	if _, err := DecodeReport(b); err != nil {
		t.Fatalf("whole-file finding rejected: %v", err)
	}
	f.Location.EndLine = 4
	if _, err := DecodeReport(report("spec-keeper", "gate", f)); err == nil {
		t.Error("end_line without line accepted")
	}

	c := mustConfig(t)
	p := plan(c, "README.md")
	f.Location.EndLine = 0
	out := Render(c, p, Check(c, p, map[string]Input{"spec-keeper": {Data: report("spec-keeper", "gate", f)}}, false), RenderOptions{})
	if !strings.Contains(out, "`aspect.yaml`") || strings.Contains(out, "aspect.yaml:0") {
		t.Errorf("whole-file location rendered wrongly:\n%s", out)
	}
}

func TestNoSpec(t *testing.T) {
	c := mustConfig(t)
	p := BuildPlan(c, "aaaaaaa1", "bbbbbbb2", []Change{{Path: "README.md", Added: 1}}, "", NoSpec)
	if p.Spec != NoSpec {
		t.Fatalf("spec = %q, want %q", p.Spec, NoSpec)
	}
	out := Render(c, p, Check(c, p, map[string]Input{"spec-keeper": {Data: report("spec-keeper", "gate")}}, false), RenderOptions{})
	if strings.Count(out, "No spec:") != 1 || !strings.Contains(out, "`aspect.yaml` does not exist") {
		t.Errorf("render should state the missing spec once:\n%s", out)
	}
}

func TestResolveSpec(t *testing.T) {
	defaulted, err := ParseConfig([]byte("gates: [{agent: a, paths: ['**'], mode: gate}]"))
	if err != nil {
		t.Fatal(err)
	}
	explicit := mustConfig(t) // spec: aspect.yaml
	in := func(paths ...string) func(string) bool {
		return func(p string) bool { return slices.Contains(paths, p) }
	}
	for _, tc := range []struct {
		name   string
		c      *Config
		exists func(string) bool
		want   string
	}{
		{"default present", defaulted, in(DefaultSpec, LegacySpec), DefaultSpec},
		{"default falls back to the root file", defaulted, in(LegacySpec), LegacySpec},
		{"default with neither", defaulted, in(), NoSpec},
		{"explicit present", explicit, in("aspect.yaml"), "aspect.yaml"},
		{"explicit never falls back", mustParse(t, "spec: specs/app.yaml\ngates: [{agent: a, paths: ['**'], mode: gate}]"), in(LegacySpec), NoSpec},
	} {
		if got := tc.c.ResolveSpec(tc.exists); got != tc.want {
			t.Errorf("%s: ResolveSpec = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func mustParse(t *testing.T, src string) *Config {
	t.Helper()
	c, err := ParseConfig([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A placed module owns its package's files and nothing by name; a change to
// its brief (here the package's own ASPECT.md) or to the spec file it is
// written in is a change to the module too. The spec is read from the
// repository given, not from the working directory.
func TestBuildPlanModulesFollowPathsBriefsAndSpecFiles(t *testing.T) {
	c, err := LoadConfig(filepath.Join("..", "..", DefaultConfigPath))
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	files := map[string]string{
		"_aspect/aspect.yaml": `
aspect: 1
system:
  name: g
  intent: shop
  module_path: example.com/g
  goals: [{id: G1, statement: x}]
modules:
  - dir: modules/
`,
		"_aspect/modules/orders.yaml": "name: modules_orders\npath: modules/orders\nintent: orders\ngoals: [G1]\nbrief: ../../modules/orders/ASPECT.md\nconstraints: [Orders are never deleted.]\ninvariants: [one live charge]\n",
		"_aspect/modules/admin.yaml":  "name: admin\npath: admin\nintent: back office\ngoals: [G1]\ninvariants: [x]\n",
		"modules/orders/ASPECT.md":    "rules",
		".git/HEAD":                   "ref: refs/heads/main\n",
	}
	for rel, content := range files {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plan := func(paths ...string) map[string][]string {
		var ch []Change
		for _, p := range paths {
			ch = append(ch, Change{Path: p, Added: 1})
		}
		out := map[string][]string{}
		for _, m := range BuildPlan(c, "b", "h", ch, repo, "_aspect/aspect.yaml").Modules {
			out[m.Name] = m.Files
		}
		return out
	}
	got := plan("modules/orders/order.go", "modules/orders/sub/x.go", "store/admin.go", "modules/orders/ASPECT.md", "_aspect/modules/admin.yaml")
	if want := []string{"modules/orders/order.go", "modules/orders/ASPECT.md"}; !slices.Equal(got["modules_orders"], want) {
		t.Errorf("modules_orders files = %v, want %v", got["modules_orders"], want)
	}
	// store/admin.go is not admin's (the old stem guess said it was); the
	// module's spec file is.
	if want := []string{"_aspect/modules/admin.yaml"}; !slices.Equal(got["admin"], want) {
		t.Errorf("admin files = %v, want %v", got["admin"], want)
	}
	p := BuildPlan(c, "b", "h", []Change{{Path: "modules/orders/order.go", Added: 1}}, repo, "_aspect/aspect.yaml")
	if len(p.Modules) != 1 || p.Modules[0].Brief != "modules/orders/ASPECT.md" || !slices.Equal(p.Modules[0].Constraints, []string{"Orders are never deleted."}) {
		t.Errorf("module context = %+v", p.Modules)
	}
}

func TestBuildPlanTopics(t *testing.T) {
	c, err := LoadConfig(filepath.Join("..", "..", DefaultConfigPath))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for p, body := range map[string]string{
		"go.mod": "module example.com/shop\n",
		"aspect.yaml": `aspect: 1
system:
  name: shop
  intent: sell
  module_path: example.com/shop
  goals: [{id: G1, statement: Charge once.}]
modules:
  - name: pay
    path: pay
    intent: Charge money.
    goals: [G1]
    topics:
      - {name: refunds, files: [admin/refund_*.go], brief: pay/rules/refunds.md, invariants: [Capped.]}
      - {name: slips, files: [pay/slip*.go], invariants: [One slip.]}
  - name: admin
    path: admin
    intent: Back office.
    goals: [G1]
`,
		"pay/rules/refunds.md": "Refunds.\n",
	} {
		abs := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	modules := func(changes ...string) map[string]ModuleContext {
		var ch []Change
		for _, p := range changes {
			ch = append(ch, Change{Path: p, Added: 1})
		}
		out := map[string]ModuleContext{}
		for _, m := range BuildPlan(c, "b", "h", ch, dir, "aspect.yaml").Modules {
			out[m.Name] = m
		}
		return out
	}
	// A file another module owns, which a topic covers, is a change to both.
	got := modules("admin/refund_page.go")
	if pay, ok := got["pay"]; !ok || !slices.Equal(pay.Topics, []string{"refunds"}) || !slices.Equal(pay.Files, []string{"admin/refund_page.go"}) {
		t.Errorf("pay = %+v, want the refunds topic through admin/refund_page.go", got["pay"])
	}
	if _, ok := got["admin"]; !ok {
		t.Errorf("admin, which owns the file, must stay in the plan: %v", got)
	}
	// A change to a topic's brief is a change to its module and that topic.
	if pay := modules("pay/rules/refunds.md")["pay"]; !slices.Equal(pay.Topics, []string{"refunds"}) {
		t.Errorf("pay = %+v, want the refunds topic through its brief", pay)
	}
	// A module's own file touches none of its topics unless one covers it.
	if pay := modules("pay/charge.go")["pay"]; len(pay.Topics) != 0 || len(pay.Files) != 1 {
		t.Errorf("pay = %+v, want its file and no topic", pay)
	}
}
