package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eideroliveira/aspect/internal/agents"
	"github.com/eideroliveira/aspect/internal/lang"
	"github.com/eideroliveira/aspect/internal/llm"
	"github.com/eideroliveira/aspect/internal/plan"
	"github.com/eideroliveira/aspect/internal/spec"
	"github.com/eideroliveira/aspect/internal/workspace"
)

// scriptedLLM plays back answers in order and records every prompt, so the
// test can assert what each agent was shown.
type scriptedLLM struct {
	answers []string
	prompts []string
}

func (s *scriptedLLM) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	s.prompts = append(s.prompts, req.Prompt)
	if len(s.answers) == 0 {
		return llm.Response{}, context.Canceled
	}
	a := s.answers[0]
	s.answers = s.answers[1:]
	return llm.Response{Text: a, InputTokens: 100, OutputTokens: 50}, nil
}

func systemVerdict(goal string, status agents.GoalStatus) agents.SystemVerdict {
	return agents.SystemVerdict{
		Goals:           []agents.GoalVerdict{{ID: goal, Status: status, Evidence: "system pass", Gaps: []string{}, Confidence: 0.8}},
		IntentStatus:    agents.Aligned,
		IntentRationale: "the parts add up",
		Integration:     []agents.IntegrationFinding{},
		Recommendations: []string{},
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const counterSpec = `
aspect: 1
system:
  name: counter
  intent: count events
  module_path: example.com/counter
  goals:
    - {id: G1, statement: the count never decreases, verify: invariant}
modules:
  - name: counter
    intent: hold a monotonically increasing count
    goals: [G1]
    interface:
      - {name: Inc, signature: "func (c *Counter) Inc() int"}
    scenarios:
      - {id: S1, given: a new counter, when: Inc twice, then: the result is 2, goals: [G1]}
`

func TestRunRepairsUntilTestsPassThenValidates(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the go toolchain")
	}
	s, err := spec.Parse([]byte(counterSpec))
	if err != nil {
		t.Fatal(err)
	}
	if issues := spec.Validate(s); issues.HasErrors() {
		t.Fatal(issues)
	}
	p, err := plan.Build(s)
	if err != nil {
		t.Fatal(err)
	}

	buggy := agents.CodeOutput{Files: []workspace.File{{Path: "counter/counter.go",
		Content: "package counter\n\ntype Counter struct{ n int }\n\nfunc (c *Counter) Inc() int { return c.n }\n"}}, Notes: "first try", Concerns: []string{}}
	fixed := agents.CodeOutput{Files: []workspace.File{{Path: "counter/counter.go",
		Content: "package counter\n\ntype Counter struct{ n int }\n\nfunc (c *Counter) Inc() int { c.n++; return c.n }\n"}}, Notes: "fixed", Concerns: []string{}}
	tests := agents.TestOutput{Files: []workspace.File{{Path: "counter/counter_test.go",
		Content: "package counter\n\nimport \"testing\"\n\nfunc TestInc_S1(t *testing.T) {\n\tvar c Counter\n\tc.Inc()\n\tif got := c.Inc(); got != 2 {\n\t\tt.Fatalf(\"got %d\", got)\n\t}\n}\n"}},
		Coverage: []agents.ScenarioCoverage{{Scenario: "S1", Tests: []string{"TestInc_S1"}}}, Concerns: []string{}}
	verdict := agents.Verdict{Module: "counter", Goals: []agents.GoalVerdict{{ID: "G1", Status: agents.Achieved, Evidence: "TestInc_S1", Gaps: []string{}, Confidence: 0.9}},
		IntentStatus: agents.Aligned, IntentRationale: "does what it says", Scenarios: []agents.ScenarioVerdict{{Scenario: "S1", Covered: true, Test: "TestInc_S1"}}, Recommendations: []string{}}

	fake := &scriptedLLM{answers: []string{mustJSON(t, buggy), mustJSON(t, tests), mustJSON(t, fixed), mustJSON(t, verdict), mustJSON(t, systemVerdict("G1", agents.Partial))}}
	out := t.TempDir()
	r := New(fake, Options{OutDir: out, MaxRepairs: 2, Model: "fake"})
	rep, err := r.Run(context.Background(), s, p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(fake.answers) != 0 {
		t.Fatalf("%d scripted answers unused", len(fake.answers))
	}
	m := rep.Modules[0]
	if !m.TestsOK || m.Iterations != 2 {
		t.Fatalf("tests_ok=%v iterations=%d output:\n%s", m.TestsOK, m.Iterations, m.TestOutput)
	}
	if m.Usage.Calls != 4 || rep.Usage.Calls != 5 || rep.Usage.InputTokens != 500 {
		t.Fatalf("usage = %+v / %+v", m.Usage, rep.Usage)
	}
	if !strings.Contains(fake.prompts[2], "Failing output") || !strings.Contains(fake.prompts[2], "got 0") {
		t.Fatalf("repair prompt must carry the failing test output, got:\n%s", fake.prompts[2])
	}
	if !strings.Contains(fake.prompts[3], "Test run: PASSED") {
		t.Fatal("validator must see the final, passing test run")
	}
	if g := rep.Goals[0]; g.ByModule["counter"] != agents.Achieved || g.System != agents.Partial || g.Status != agents.Partial {
		t.Fatalf("goal summary must take the weakest of module roll-up and system pass: %+v", g)
	}
	sysPrompt := fake.prompts[4]
	for _, want := range []string{"Module verdict:", "tests_ok: true", "## Module counter", "func (c *Counter) Inc()"} {
		if !strings.Contains(sysPrompt, want) {
			t.Errorf("system validator prompt lacks %q", want)
		}
	}

	if err := rep.Write(out); err != nil {
		t.Fatal(err)
	}
	md, err := os.ReadFile(filepath.Join(out, "counter", "REPORT.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"🟡 partial", "Tests passed after 2 run(s)", "TestInc_S1", "## System", "the parts add up"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("REPORT.md lacks %q", want)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "counter", "counter", "counter.go")); err != nil {
		t.Fatal("generated code missing from workspace")
	}
}

func TestRunReportsFailedModuleAsUnverifiable(t *testing.T) {
	s, _ := spec.Parse([]byte(counterSpec))
	p, _ := plan.Build(s)
	fake := &scriptedLLM{answers: []string{"not json at all"}}
	r := New(fake, Options{OutDir: t.TempDir()})
	rep, err := r.Run(context.Background(), s, p)
	if err == nil || rep == nil {
		t.Fatalf("want error and partial report, got err=%v rep=%v", err, rep)
	}
	if rep.Modules[0].Error == "" || rep.Goals[0].Status != agents.Unverifiable {
		t.Fatalf("report = %+v", rep)
	}
}

const swiftCounterSpec = `
aspect: 1
system:
  name: counter
  intent: count events
  language: swift
  module_path: com.example.counter
  goals:
    - {id: G1, statement: the count never decreases, verify: invariant}
modules:
  - name: counter
    intent: hold a monotonically increasing count
    goals: [G1]
    scenarios:
      - {id: S1, given: a new counter, when: increment twice, then: the result is 2, goals: [G1]}
`

func TestRunSwiftModuleThroughRealToolchain(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the swift toolchain")
	}
	if _, err := exec.LookPath("swift"); err != nil {
		t.Skip("swift toolchain not installed")
	}
	s, err := spec.Parse([]byte(swiftCounterSpec))
	if err != nil {
		t.Fatal(err)
	}
	if issues := spec.Validate(s); issues.HasErrors() {
		t.Fatal(issues)
	}
	p, _ := plan.Build(s)

	code := agents.CodeOutput{Files: []workspace.File{{Path: "Sources/Counter/Counter.swift",
		Content: "public struct Counter {\n    public private(set) var value = 0\n    public init() {}\n    public mutating func increment() -> Int { value += 1; return value }\n}\n"}}, Notes: "n", Concerns: []string{}}
	tests := agents.TestOutput{Files: []workspace.File{{Path: "Tests/CounterTests/CounterTests.swift",
		Content: "import XCTest\n@testable import Counter\n\nfinal class CounterTests: XCTestCase {\n    func testIncrement_S1() {\n        var c = Counter()\n        _ = c.increment()\n        XCTAssertEqual(c.increment(), 2)\n    }\n}\n"}},
		Coverage: []agents.ScenarioCoverage{{Scenario: "S1", Tests: []string{"testIncrement_S1"}}}, Concerns: []string{}}
	verdict := agents.Verdict{Module: "counter", Goals: []agents.GoalVerdict{{ID: "G1", Status: agents.Achieved, Evidence: "testIncrement_S1", Gaps: []string{}, Confidence: 0.9}},
		IntentStatus: agents.Aligned, IntentRationale: "ok", Scenarios: []agents.ScenarioVerdict{}, Recommendations: []string{}}

	fake := &scriptedLLM{answers: []string{mustJSON(t, code), mustJSON(t, tests), mustJSON(t, verdict), mustJSON(t, systemVerdict("G1", agents.Achieved))}}
	out := t.TempDir()
	rep, err := New(fake, Options{OutDir: out, MaxRepairs: 1, Model: "fake"}).Run(context.Background(), s, p)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, rep.Modules[0].TestOutput)
	}
	m := rep.Modules[0]
	if !m.TestsOK || m.Iterations != 1 {
		t.Fatalf("tests_ok=%v iterations=%d output:\n%s", m.TestsOK, m.Iterations, m.TestOutput)
	}
	if _, err := os.Stat(filepath.Join(out, "counter", "Package.swift")); err != nil {
		t.Fatal("Package.swift was not generated")
	}
	if rep.Language != "swift" {
		t.Fatalf("report language = %q", rep.Language)
	}
}

func TestRunFeedsDisallowedImportsBackToCoder(t *testing.T) {
	s, _ := spec.Parse([]byte(counterSpec))
	p, _ := plan.Build(s)
	bad := agents.CodeOutput{Files: []workspace.File{{Path: "counter/counter.go",
		Content: "package counter\n\nimport _ \"github.com/evil/dep\"\n\ntype Counter struct{ n int }\n\nfunc (c *Counter) Inc() int { c.n++; return c.n }\n"}}, Notes: "n", Concerns: []string{}}
	tests := agents.TestOutput{Files: []workspace.File{{Path: "counter/counter_test.go", Content: "package counter\n"}}, Coverage: []agents.ScenarioCoverage{}, Concerns: []string{}}
	fake := &scriptedLLM{answers: []string{mustJSON(t, bad), mustJSON(t, tests), mustJSON(t, bad)}}
	rep, err := New(fake, Options{OutDir: t.TempDir(), MaxRepairs: 1}).Run(context.Background(), s, p)
	if err == nil {
		t.Fatal("run should fail: the validator answer was never scripted")
	}
	m := rep.Modules[0]
	if m.TestsOK || m.Iterations != 2 || !strings.Contains(m.TestOutput, `imports "github.com/evil/dep"`) {
		t.Fatalf("import violation must be the recorded failure, got ok=%v iterations=%d output=%q", m.TestsOK, m.Iterations, m.TestOutput)
	}
	if !strings.Contains(fake.prompts[2], "disallowed imports") {
		t.Fatal("the repair prompt must carry the import violation")
	}
}

const twoTierSpec = `
aspect: 1
system:
  name: shop
  intent: sell
  topology: api_backend
  goals:
    - {id: G1, statement: the app can read the count, verify: test}
  interfaces:
    - name: api
      kind: http
      intent: what the app calls
      provider: backend
      surfaces: [{name: count, route: /count, method: GET}]
tiers:
  - name: backend
    intent: serve
    language: go
    module_path: example.com/shop
    modules:
      - name: counter
        intent: count
        goals: [G1]
        surfaces: [api.count]
        scenarios: [{id: S1, when: GET /count, then: "0"}]
  - name: mobile
    intent: app
    language: swift
    module_path: com.example.shop
    depends_on: [backend]
    modules:
      - name: client
        intent: read the count
        goals: [G1]
        consumes: [api.count]
        scenarios: [{id: S1, when: parse, then: int}]
`

func TestRunTwoTiersEachInItsOwnWorkspace(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the go and swift toolchains")
	}
	if _, err := exec.LookPath("swift"); err != nil {
		t.Skip("swift toolchain not installed")
	}
	s, err := spec.Parse([]byte(twoTierSpec))
	if err != nil {
		t.Fatal(err)
	}
	if issues := spec.Validate(s); issues.HasErrors() {
		t.Fatal(issues)
	}
	p, err := plan.Build(s)
	if err != nil {
		t.Fatal(err)
	}
	goCode := agents.CodeOutput{Files: []workspace.File{{Path: "counter/counter.go", Content: "package counter\n\nimport \"net/http\"\n\n// Handler serves the count.\nfunc Handler() http.Handler {\n\treturn http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(\"0\")) })\n}\n"}}, Concerns: []string{}}
	goTests := agents.TestOutput{Files: []workspace.File{{Path: "counter/counter_test.go", Content: "package counter\n\nimport (\n\t\"net/http/httptest\"\n\t\"testing\"\n)\n\nfunc TestCount_S1(t *testing.T) {\n\trec := httptest.NewRecorder()\n\tHandler().ServeHTTP(rec, httptest.NewRequest(\"GET\", \"/count\", nil))\n\tif rec.Body.String() != \"0\" {\n\t\tt.Fatal(rec.Body.String())\n\t}\n}\n"}}, Coverage: []agents.ScenarioCoverage{}, Concerns: []string{}}
	verdict := func(module string) agents.Verdict {
		return agents.Verdict{Module: module, Goals: []agents.GoalVerdict{{ID: "G1", Status: agents.Achieved, Evidence: "test", Gaps: []string{}, Confidence: 0.9}}, IntentStatus: agents.Aligned, Scenarios: []agents.ScenarioVerdict{}, Recommendations: []string{}}
	}
	swiftCode := agents.CodeOutput{Files: []workspace.File{{Path: "Sources/Client/Client.swift", Content: "public enum Client {\n    public static func parse(_ body: String) -> Int? { Int(body) }\n}\n"}}, Concerns: []string{}}
	swiftTests := agents.TestOutput{Files: []workspace.File{{Path: "Tests/ClientTests/ClientTests.swift", Content: "import XCTest\n@testable import Client\n\nfinal class ClientTests: XCTestCase {\n    func testParse_S1() { XCTAssertEqual(Client.parse(\"0\"), 0) }\n}\n"}}, Coverage: []agents.ScenarioCoverage{}, Concerns: []string{}}

	fake := &scriptedLLM{answers: []string{
		mustJSON(t, goCode), mustJSON(t, goTests), mustJSON(t, verdict("counter")),
		mustJSON(t, swiftCode), mustJSON(t, swiftTests), mustJSON(t, verdict("client")),
		mustJSON(t, agents.SystemVerdict{Goals: []agents.GoalVerdict{{ID: "G1", Status: agents.Achieved, Evidence: "both sides agree", Gaps: []string{}, Confidence: 0.9}},
			IntentStatus: agents.Aligned, Integration: []agents.IntegrationFinding{{Surface: "api.count", Provider: "counter", Consumer: "client", Status: agents.Achieved, Note: "GET /count on both sides"}}, Recommendations: []string{}}),
	}}
	out := t.TempDir()
	rep, err := New(fake, Options{OutDir: out, MaxRepairs: 1, Model: "fake"}).Run(context.Background(), s, p)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, dumpOutputs(rep))
	}
	if len(rep.Tiers) != 2 || rep.Tiers[0].Name != "backend" || rep.Tiers[1].Name != "mobile" {
		t.Fatalf("tiers = %+v", rep.Tiers)
	}
	for _, m := range rep.Modules {
		if !m.TestsOK {
			t.Fatalf("module %s (tier %s) failed:\n%s", m.Module, m.Tier, m.TestOutput)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "shop", "backend", "go.mod")); err != nil {
		t.Fatal("backend tier must have its own go.mod")
	}
	if _, err := os.Stat(filepath.Join(out, "shop", "mobile", "Package.swift")); err != nil {
		t.Fatal("mobile tier must have its own Package.swift")
	}
	if !strings.Contains(fake.prompts[3], "Surfaces this module consumes") || !strings.Contains(fake.prompts[3], "route: /count") {
		t.Fatal("the swift client's coder prompt must carry the consumed api surface")
	}
	if !strings.Contains(fake.prompts[3], "This tier\n\nLanguage: Swift\nModule path: com.example.shop") {
		t.Fatal("the swift client's coder prompt must describe its own tier")
	}
	if err := rep.Write(out); err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(filepath.Join(out, "shop", "REPORT.md"))
	if !strings.Contains(string(md), "## Tiers") || !strings.Contains(string(md), "(tier mobile)") || !strings.Contains(string(md), "GET /count on both sides") {
		t.Fatalf("report must show tiers and integration findings:\n%s", md)
	}
	sysPrompt := fake.prompts[6]
	if !strings.Contains(sysPrompt, "Interfaces (full contracts)") || !strings.Contains(sysPrompt, "Module client") || !strings.Contains(sysPrompt, "Module counter") {
		t.Fatal("system validator must see the contracts and both sides' code")
	}
}

func dumpOutputs(rep *Report) string {
	if rep == nil {
		return ""
	}
	var b strings.Builder
	for _, m := range rep.Modules {
		b.WriteString(m.Module + ": " + m.Error + "\n" + m.TestOutput + "\n")
	}
	return b.String()
}

func TestWavesGroupIndependentModules(t *testing.T) {
	steps := []plan.Step{
		{Module: "a"}, {Module: "b"}, {Module: "c", DependsOn: []string{"a"}},
		{Module: "d", DependsOn: []string{"b", "c"}}, {Module: "e"},
	}
	got := waves(steps)
	want := [][]int{{0, 1, 4}, {2}, {3}}
	if len(got) != len(want) {
		t.Fatalf("waves = %v", got)
	}
	for i := range want {
		if strings.Trim(strings.Join(strings.Fields(fmt.Sprint(got[i])), ","), "[]") != strings.Trim(strings.Join(strings.Fields(fmt.Sprint(want[i])), ","), "[]") {
			t.Fatalf("wave %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// concurrentLLM answers by agent role and module name, and records how many
// calls overlapped, so a test can prove modules were generated in parallel.
type concurrentLLM struct {
	t       *testing.T
	answers map[string]string // "coder:a", "tester:a", "validator:a", "system"
	mu      sync.Mutex
	active  int
	maxSeen int
}

func (c *concurrentLLM) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	c.mu.Lock()
	c.active++
	if c.active > c.maxSeen {
		c.maxSeen = c.active
	}
	c.mu.Unlock()
	time.Sleep(150 * time.Millisecond) // long enough for two calls to overlap
	defer func() {
		c.mu.Lock()
		c.active--
		c.mu.Unlock()
	}()

	role := ""
	switch {
	case strings.HasPrefix(req.System, "You are the Coder"):
		role = "coder"
	case strings.HasPrefix(req.System, "You are the Tester"):
		role = "tester"
	case strings.HasPrefix(req.System, "You are the Validator"):
		role = "validator"
	case strings.HasPrefix(req.System, "You are the System Validator"):
		return llm.Response{Text: c.answers["system"]}, nil
	}
	for key, ans := range c.answers {
		r, mod, _ := strings.Cut(key, ":")
		if r == role && strings.Contains(req.Prompt, "\nModule: "+mod+"\n") {
			return llm.Response{Text: ans}, nil
		}
	}
	c.t.Errorf("no answer for role %s; prompt head: %.120s", role, req.Prompt)
	return llm.Response{}, context.Canceled
}

const twoIndependentSpec = `
aspect: 1
system:
  name: pair
  intent: two things
  module_path: example.com/pair
  goals: [{id: G1, statement: both work, verify: test}]
modules:
  - {name: alpha, intent: a, goals: [G1], scenarios: [{id: S1, when: A, then: 1}]}
  - {name: beta, intent: b, goals: [G1], scenarios: [{id: S1, when: B, then: 2}]}
`

func TestRunGeneratesIndependentModulesInParallel(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the go toolchain")
	}
	s, err := spec.Parse([]byte(twoIndependentSpec))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := plan.Build(s)
	code := func(name string, n int) string {
		return mustJSON(t, agents.CodeOutput{Files: []workspace.File{{Path: name + "/" + name + ".go", Content: fmt.Sprintf("package %s\n\n// N is the answer.\nfunc N() int { return %d }\n", name, n)}}, Concerns: []string{}})
	}
	tests := func(name string, n int) string {
		return mustJSON(t, agents.TestOutput{Files: []workspace.File{{Path: name + "/" + name + "_test.go", Content: fmt.Sprintf("package %s\n\nimport \"testing\"\n\nfunc TestN_S1(t *testing.T) {\n\tif N() != %d {\n\t\tt.Fatal()\n\t}\n}\n", name, n)}}, Coverage: []agents.ScenarioCoverage{}, Concerns: []string{}})
	}
	verdict := func(name string) string {
		return mustJSON(t, agents.Verdict{Module: name, Goals: []agents.GoalVerdict{{ID: "G1", Status: agents.Achieved, Evidence: "TestN_S1", Gaps: []string{}, Confidence: 0.9}}, IntentStatus: agents.Aligned, Scenarios: []agents.ScenarioVerdict{}, Recommendations: []string{}})
	}
	fake := &concurrentLLM{t: t, answers: map[string]string{
		"coder:alpha": code("alpha", 1), "tester:alpha": tests("alpha", 1), "validator:alpha": verdict("alpha"),
		"coder:beta": code("beta", 2), "tester:beta": tests("beta", 2), "validator:beta": verdict("beta"),
		"system": mustJSON(t, systemVerdict("G1", agents.Achieved)),
	}}
	rep, err := New(fake, Options{OutDir: t.TempDir(), MaxRepairs: 1, Parallel: 2, Model: "fake"}).Run(context.Background(), s, p)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, dumpOutputs(rep))
	}
	if len(rep.Modules) != 2 || rep.Modules[0].Module != "alpha" || rep.Modules[1].Module != "beta" {
		t.Fatalf("report order must follow the plan: %+v", rep.Modules)
	}
	for _, m := range rep.Modules {
		if !m.TestsOK {
			t.Fatalf("%s failed:\n%s", m.Module, m.TestOutput)
		}
	}
	if fake.maxSeen < 2 {
		t.Fatalf("expected overlapping model calls, max concurrent = %d", fake.maxSeen)
	}
}

func TestRunStopsRepairingWhenTheFailureDoesNotChange(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the go toolchain")
	}
	s, err := spec.Parse([]byte(counterSpec))
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(s)
	if err != nil {
		t.Fatal(err)
	}
	buggy := agents.CodeOutput{Files: []workspace.File{{Path: "counter/counter.go",
		Content: "package counter\n\ntype Counter struct{ n int }\n\nfunc (c *Counter) Inc() int { return c.n }\n"}}, Notes: "first try", Concerns: []string{}}
	tests := agents.TestOutput{Files: []workspace.File{{Path: "counter/counter_test.go",
		Content: "package counter\n\nimport \"testing\"\n\nfunc TestInc_S1(t *testing.T) {\n\tvar c Counter\n\tc.Inc()\n\tif got := c.Inc(); got != 2 {\n\t\tt.Fatalf(\"got %d\", got)\n\t}\n}\n"}},
		Coverage: []agents.ScenarioCoverage{{Scenario: "S1", Tests: []string{"TestInc_S1"}}}, Concerns: []string{}}
	verdict := agents.Verdict{Module: "counter", Goals: []agents.GoalVerdict{{ID: "G1", Status: agents.Achieved, Evidence: "optimistic", Gaps: []string{}, Confidence: 0.9}},
		IntentStatus: agents.Aligned, IntentRationale: "r", Scenarios: []agents.ScenarioVerdict{}, Recommendations: []string{}}

	// The repair answers with the very same code, so the second run fails
	// the same way and the third repair round is never requested.
	fake := &scriptedLLM{answers: []string{mustJSON(t, buggy), mustJSON(t, tests), mustJSON(t, buggy), mustJSON(t, verdict), mustJSON(t, systemVerdict("G1", agents.Partial))}}
	rep, err := New(fake, Options{OutDir: t.TempDir(), MaxRepairs: 3, Model: "fake"}).Run(context.Background(), s, p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(fake.answers) != 0 {
		t.Fatalf("%d scripted answers unused", len(fake.answers))
	}
	m := rep.Modules[0]
	if m.TestsOK || m.Iterations != 2 || m.Usage.Calls != 4 {
		t.Fatalf("tests_ok=%v iterations=%d calls=%d", m.TestsOK, m.Iterations, m.Usage.Calls)
	}
	if !strings.Contains(strings.Join(m.Concerns, "\n"), "left the failing output unchanged") {
		t.Fatalf("concerns = %v", m.Concerns)
	}
	// The Validator said achieved; the pipeline knows the tests failed.
	if g := rep.Goals[0]; g.ByModule["counter"] != agents.Partial {
		t.Fatalf("goal = %+v", g)
	}
}

func TestSameFailureIgnoresVolatileOutput(t *testing.T) {
	a := "--- FAIL: TestInc_S1 (0.00s)\n    counter_test.go:9: got 0\nFAIL\nFAIL\texample.com/counter\t0.312s\ngoroutine 7 [running]: 0xc000012345"
	b := "--- FAIL: TestInc_S1 (0.01s)\n    counter_test.go:9: got 0\nFAIL\nFAIL\texample.com/counter\t0.298s\ngoroutine 19 [running]: 0xc0000abcde"
	if !sameFailure(a, b) {
		t.Fatal("timings, goroutine ids and addresses must not count as a change")
	}
	if sameFailure(a, strings.Replace(b, "got 0", "got 1", 1)) {
		t.Fatal("a different assertion message is a different failure")
	}
	before := []workspace.File{{Path: "a.go", Content: "1"}, {Path: "b.go", Content: "2"}}
	after := []workspace.File{{Path: "a.go", Content: "1"}, {Path: "b.go", Content: "3"}, {Path: "c.go", Content: "4"}}
	if got := changed(before, after); strings.Join(got, ",") != "b.go,c.go" {
		t.Fatalf("changed = %v", got)
	}
	if got := changed(before, before[:1]); strings.Join(got, ",") != "b.go (removed)" {
		t.Fatalf("changed = %v", got)
	}
}

// dependencyFiles hands the Coder its dependencies' implementation files by
// ownership: a placed dependency gives its one package, never a sibling
// sharing its prefix, a subpackage, or its tests.
func TestDependencyFilesByOwnership(t *testing.T) {
	root := t.TempDir()
	for p, c := range map[string]string{
		"modules/api/api.go":      "package api\n",
		"modules/api/api_test.go": "package api\n",
		"modules/api/v2/v2.go":    "package v2\n",
		"modules/apikeys/keys.go": "package apikeys\n",
		"store/store.go":          "package store\n",
		"store/sub/sub.go":        "package sub\n",
		"shop.go":                 "package shop\n",
	} {
		abs := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := workspace.New(root, workspace.Toolchain{SourceExt: ".go"})
	if err != nil {
		t.Fatal(err)
	}
	tier := &spec.Tier{Language: "go", Modules: []spec.Module{
		{Name: "root", Path: "."},
		{Name: "modules_api", Path: "modules/api"},
		{Name: "modules_apikeys", Path: "modules/apikeys"},
		{Name: "store"}, // laid out by name: store/ and below
	}}
	p, _ := lang.For("go")
	for deps, want := range map[string]string{
		"modules_api": "modules/api/api.go",
		"store":       "store/store.go,store/sub/sub.go",
		"root":        "shop.go",
	} {
		files, err := dependencyFiles(ws, p, tier, strings.Split(deps, ","))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, f := range files {
			got = append(got, f.Path)
		}
		if strings.Join(got, ",") != want {
			t.Errorf("dependencyFiles(%s) = %v, want %s", deps, got, want)
		}
	}
}
