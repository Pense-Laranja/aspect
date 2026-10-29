package agents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Pense-Laranja/aspect/internal/lang"
	"github.com/Pense-Laranja/aspect/internal/llm"
	"github.com/Pense-Laranja/aspect/internal/spec"
	"github.com/Pense-Laranja/aspect/internal/workspace"
)

// fakeLLM answers with a canned JSON document and records the prompt.
type fakeLLM struct {
	answer string
	prompt string
	system string
	schema map[string]any
}

func (f *fakeLLM) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	f.prompt, f.system, f.schema = req.Prompt, req.System, req.Schema
	return llm.Response{Text: f.answer, InputTokens: 10, OutputTokens: 5}, nil
}

const fixture = `
aspect: 1
system:
  name: inv
  intent: count things
  module_path: example.com/inv
  goals: [{id: G1, statement: counts are right}]
  stack:
    go:
      frameworks:
        - {name: qor5, module: github.com/qor5/admin/v3, guidance: "Register models with presets.New().Model(&T{})."}
      orm: {name: gorm, module: gorm.io/gorm}
  database:
    engine: postgres
    entities:
      - name: Item
        intent: a thing on a shelf
        fields: [{name: ID, type: uint, key: primary}, {name: SKU, type: string, unique: true}]
  interfaces:
    - name: admin
      kind: web
      intent: back office
      framework: qor5
      surfaces: [{name: items, route: /admin/items, entity: Item, operations: [list, create]}]
modules:
  - name: stock
    intent: count things
    goals: [G1]
    entities: [Item]
    surfaces: [admin.items]
    scenarios:
      - {id: S1, when: Receive, then: Available == 5, goals: [G1]}
`

func task(t *testing.T, language string) Task {
	t.Helper()
	s, err := spec.Parse([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	s.System.Language = language
	p, err := lang.For(language)
	if err != nil {
		t.Fatal(err)
	}
	s.Module("stock").Brief = spec.Brief{Path: "briefs/stock.md", Text: "Stock is counted in whole units; never fractional."}
	return Task{Spec: s, Module: s.Module("stock"), Lang: p}
}

func TestCoderKeepsOnlyModuleSourceFiles(t *testing.T) {
	out := CodeOutput{
		Files: []workspace.File{
			{Path: "stock/stock.go", Content: "package stock"},
			{Path: "./stock/extra.go", Content: "package stock"},
			{Path: "stock/stock_test.go", Content: "package stock"},
			{Path: "ledger/ledger.go", Content: "package ledger"},
			{Path: "stock/README.md", Content: "nope"},
		},
		Notes: "n",
	}
	answer, _ := json.Marshal(out)
	f := &fakeLLM{answer: "```json\n" + string(answer) + "\n```"}
	got, _, err := (&Coder{LLM: f}).Generate(context.Background(), CodeInput{Task: task(t, "go")})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 2 || got.Files[0].Path != "stock/stock.go" || got.Files[1].Path != "stock/extra.go" {
		t.Fatalf("files = %+v", got.Files)
	}
	for _, want := range []string{
		"Import path: example.com/inv/stock",
		"Code directory: stock/",
		"name: stock",
		"S1",
		"# Stack (Go)",
		"gorm.io/gorm",
		"Entities owned by this module",
		"name: SKU",
		"Surfaces this module implements",
		"route: /admin/items",
		"How to use qor5 here",
		"presets.New()",
		"## Module brief (briefs/stock.md)",
		"never fractional",
	} {
		if !strings.Contains(f.prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if f.schema == nil || !strings.HasPrefix(f.system, coderSystem) || !strings.Contains(f.system, "Go rules:") {
		t.Error("coder must send its system prompt with language rules, and a schema")
	}
}

func TestCoderSwiftLayout(t *testing.T) {
	f := &fakeLLM{answer: `{"files":[{"path":"Sources/Stock/Stock.swift","content":"public struct Stock {}"},{"path":"stock/stock.go","content":"x"},{"path":"Tests/StockTests/StockTests.swift","content":"x"}],"notes":"","concerns":[]}`}
	got, _, err := (&Coder{LLM: f}).Generate(context.Background(), CodeInput{Task: task(t, "swift")})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 1 || got.Files[0].Path != "Sources/Stock/Stock.swift" {
		t.Fatalf("files = %+v", got.Files)
	}
	if !strings.Contains(f.prompt, "Code directory: Sources/Stock/") || !strings.Contains(f.system, "Swift rules:") {
		t.Error("swift prompt must carry the swift layout and rules")
	}
}

func TestCoderRejectsEmptyProposal(t *testing.T) {
	f := &fakeLLM{answer: `{"files":[{"path":"other/x.go","content":"x"}],"notes":"","concerns":[]}`}
	if _, _, err := (&Coder{LLM: f}).Generate(context.Background(), CodeInput{Task: task(t, "go")}); err == nil {
		t.Fatal("want error when no file lands in the module directory")
	}
}

func TestTesterKeepsOnlyTestFiles(t *testing.T) {
	f := &fakeLLM{answer: `{"files":[{"path":"stock/stock.go","content":"x"},{"path":"stock/stock_test.go","content":"package stock"}],"coverage":[{"scenario":"S1","tests":["TestReceive_S1"]}],"notes":"","concerns":[]}`}
	got, _, err := (&Tester{LLM: f}).Generate(context.Background(), TestInput{Task: task(t, "go")})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 1 || got.Files[0].Path != "stock/stock_test.go" {
		t.Fatalf("files = %+v", got.Files)
	}
	if len(got.Coverage) != 1 || got.Coverage[0].Scenario != "S1" {
		t.Fatalf("coverage = %+v", got.Coverage)
	}
	f.answer = `{"files":[{"path":"stock/stock_test.go","content":"package stock"}],"coverage":[],"notes":"","concerns":[]}`
	_, _, err = (&Tester{LLM: f}).Generate(context.Background(), TestInput{Task: task(t, "go"), Feedback: "testify is not allowed"})
	if err != nil || !strings.Contains(f.prompt, "Why they were rejected") {
		t.Fatalf("retry prompt must explain the rejection; err=%v", err)
	}
}

func TestValidatorFillsMissingGoalsAsUnverifiable(t *testing.T) {
	f := &fakeLLM{answer: `{"module":"stock","goals":[{"id":"G1","status":"achieved","evidence":"TestReceive_S1","gaps":[],"confidence":0.9},{"id":"G9","status":"achieved","evidence":"invented","gaps":[],"confidence":1}],"intent_status":"aligned","intent_rationale":"ok","scenarios":[],"recommendations":[]}`}
	v, _, err := (&Validator{LLM: f}).Judge(context.Background(), ValidateInput{
		Task:       task(t, "go"),
		GoalIDs:    []string{"G1", "G2"},
		TestResult: workspace.Result{OK: false, Output: "FAIL"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The run failed, so the model's "achieved" for G1 is capped to partial
	// by the pipeline, and the goal it forgot is unverifiable.
	if len(v.Goals) != 2 || v.Goals[0].Status != Partial || len(v.Goals[0].Gaps) != 1 || v.Goals[1].ID != "G2" || v.Goals[1].Status != Unverifiable {
		t.Fatalf("goals = %+v", v.Goals)
	}
	if len(v.Scenarios) != 1 || v.Scenarios[0].Scenario != "S1" || v.Scenarios[0].Covered {
		t.Fatalf("a scenario without a verdict must be reported uncovered: %+v", v.Scenarios)
	}
	if !strings.Contains(f.prompt, "Test run: FAILED") || !strings.Contains(f.prompt, "Entities owned by this module") {
		t.Error("validator prompt must state the real test outcome and the module's ownership")
	}
}

func TestRenderShowsSystemDependencies(t *testing.T) {
	s, err := spec.Load("../spec/testdata/split/aspect.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := lang.For("go")
	prompt := renderContext(Task{Spec: s, Module: s.Module("orders"), Lang: p}) + renderModule(Task{Spec: s, Module: s.Module("orders"), Lang: p}, "Module")
	for _, want := range []string{"# System dependencies", "name: identity", "api (http)", "Surfaces this module consumes", "system: identity", "route: /me"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
}

// grammarLLM refuses schema-bearing requests the way the API does for a
// grammar that is too large, and answers schema-less ones.
type grammarLLM struct {
	calls   int
	prompts []string
}

func (g *grammarLLM) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	g.calls++
	g.prompts = append(g.prompts, req.Prompt)
	if req.Schema != nil {
		return llm.Response{}, errors.New(`llm: API error 400: {"type":"error","error":{"type":"invalid_request_error","message":"The compiled grammar is too large, which would cause performance issues. Simplify your tool schemas or reduce the number of strict tools."}}`)
	}
	return llm.Response{Text: "```json\n{\"files\":[{\"path\":\"stock/stock.go\",\"content\":\"package stock\"}],\"notes\":\"n\",\"concerns\":[]}\n```"}, nil
}

func TestCompleteFallsBackWhenGrammarIsTooLarge(t *testing.T) {
	g := &grammarLLM{}
	got, _, err := (&Coder{LLM: g}).Generate(context.Background(), CodeInput{Task: task(t, "go")})
	if err != nil {
		t.Fatal(err)
	}
	if g.calls != 2 || len(got.Files) != 1 {
		t.Fatalf("calls=%d files=%d", g.calls, len(got.Files))
	}
	if !strings.Contains(g.prompts[1], "conform to this JSON schema") || !strings.Contains(g.prompts[1], `"additionalProperties":false`) {
		t.Fatal("the retry must carry the schema in the prompt")
	}
	if !IsGrammarTooLarge(errors.New("x: The compiled grammar is too large, y")) || IsGrammarTooLarge(errors.New("rate limited")) {
		t.Fatal("IsGrammarTooLarge misclassifies")
	}
}

// The pipeline's agents build the whole module, so they see every topic's
// brief; a blank one adds no heading.
func TestRenderModuleShowsTopicBriefs(t *testing.T) {
	tk := task(t, "go")
	tk.Module.Topics = []spec.Topic{
		{Name: "counts", Files: []string{"stock/count*.go"}, Invariants: []spec.Invariant{{Statement: "Whole units."}}, Brief: spec.Brief{Path: "briefs/counts.md", Text: "Counts are taken at close."}},
		{Name: "empty", Files: []string{"stock/e.go"}, Brief: spec.Brief{Path: "briefs/e.md", Text: " "}},
	}
	out := renderModule(tk, "Module")
	for _, want := range []string{"## Brief of topic counts (briefs/counts.md)", "stock/count*.go", "Counts are taken at close.", "Whole units."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Brief of topic empty") {
		t.Errorf("a blank topic brief must not render:\n%s", out)
	}
}
