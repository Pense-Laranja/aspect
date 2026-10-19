package drift

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eideroliveira/aspect/internal/agents"
	"github.com/eideroliveira/aspect/internal/llm"
	"github.com/eideroliveira/aspect/internal/plan"
	"github.com/eideroliveira/aspect/internal/spec"
	"github.com/eideroliveira/aspect/internal/workspace"
)

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
    scenarios:
      - {id: S1, given: a new counter, when: Inc twice, then: the result is 2, goals: [G1]}
`

// layout writes a generated-looking workspace by hand: what `aspect run`
// would have left behind.
func layout(t *testing.T, out string, files map[string]string) {
	t.Helper()
	root := filepath.Join(out, "counter")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	files["go.mod"] = "module example.com/counter\n\ngo 1.24\n"
	for p, c := range files {
		abs := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func good() map[string]string {
	return map[string]string{
		"counter/counter.go":      "package counter\n\ntype Counter struct{ n int }\n\nfunc (c *Counter) Inc() int { c.n++; return c.n }\n",
		"counter/counter_test.go": "package counter\n\nimport \"testing\"\n\nfunc TestInc_S1(t *testing.T) {\n\tvar c Counter\n\tc.Inc()\n\tif c.Inc() != 2 {\n\t\tt.Fatal()\n\t}\n}\n",
	}
}

func parse(t *testing.T, src string) (*spec.Spec, *plan.Plan) {
	t.Helper()
	s, err := spec.Parse([]byte(src))
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
	return s, p
}

func TestDriftNoLLMDetectsMissingOrphansAndFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the go toolchain")
	}
	out := t.TempDir()
	layout(t, out, good())
	s, p := parse(t, counterSpec)
	rep, err := Run(context.Background(), nil, s, p, Options{OutDir: out, NoLLM: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Drifted() || !rep.Modules[0].Present || !rep.Modules[0].TestsOK || len(rep.Orphans) != 0 {
		t.Fatalf("clean workspace must not drift: %+v orphans=%v", rep.Modules[0], rep.Orphans)
	}
	if rep.Goals[0].Status != agents.Unverifiable || rep.Goals[0].Change != NoBaseline {
		t.Fatalf("without verdicts goals are unverifiable and without baseline new: %+v", rep.Goals[0])
	}

	// The spec grows a module the code does not have; the code grows a
	// directory the spec does not name; the tests start failing.
	changed := strings.Replace(counterSpec, "modules:\n", "modules:\n  - {name: audit, intent: log every change, goals: [G1], scenarios: [{id: S1, when: Inc, then: logged}]}\n", 1)
	s2, p2 := parse(t, changed)
	files := good()
	files["legacy/legacy.go"] = "package legacy\n"
	files["counter/counter.go"] = "package counter\n\ntype Counter struct{ n int }\n\nfunc (c *Counter) Inc() int { return c.n }\n"
	layout(t, out, files)
	rep, err = Run(context.Background(), nil, s2, p2, Options{OutDir: out, NoLLM: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Drifted() {
		t.Fatal("drift expected")
	}
	if got := strings.Join(rep.Missing(), ","); got != "audit" {
		t.Fatalf("missing = %s", got)
	}
	if got := strings.Join(rep.Orphans, ","); got != "legacy" {
		t.Fatalf("orphans = %s", got)
	}
	if got := strings.Join(rep.Failing(), ","); got != "counter" {
		t.Fatalf("failing = %s", got)
	}
	if rep.Goals[0].Status != agents.NotAchieved {
		t.Fatalf("a missing module makes its goal not achieved: %+v", rep.Goals[0])
	}
	if err := rep.Write(filepath.Join(out, "counter")); err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(filepath.Join(out, "counter", "DRIFT.md"))
	for _, want := range []string{"## Attention", "no code: audit", "no module claims: legacy", "tests fail: counter", "**Missing:**"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("DRIFT.md lacks %q:\n%s", want, md)
		}
	}
}

// verdictLLM answers the Validator and System Validator with fixed statuses.
type verdictLLM struct {
	module agents.GoalStatus
	system agents.GoalStatus
}

func (v *verdictLLM) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	var out any
	if strings.HasPrefix(req.System, "You are the System Validator") {
		out = agents.SystemVerdict{Goals: []agents.GoalVerdict{{ID: "G1", Status: v.system, Evidence: "sys", Gaps: []string{}, Confidence: 0.8}}, IntentStatus: agents.Aligned, Integration: []agents.IntegrationFinding{}, Recommendations: []string{}}
	} else {
		out = agents.Verdict{Module: "counter", Goals: []agents.GoalVerdict{{ID: "G1", Status: v.module, Evidence: "TestInc_S1", Gaps: []string{}, Confidence: 0.9}}, IntentStatus: agents.Aligned, IntentRationale: "ok", Scenarios: []agents.ScenarioVerdict{}, Recommendations: []string{}}
	}
	b, _ := json.Marshal(out)
	return llm.Response{Text: string(b), InputTokens: 10}, nil
}

func TestDriftComparesVerdictsAgainstBaseline(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the go toolchain")
	}
	out := t.TempDir()
	layout(t, out, good())
	s, p := parse(t, counterSpec)

	first, err := Run(context.Background(), &verdictLLM{module: agents.Achieved, system: agents.Achieved}, s, p, Options{OutDir: out})
	if err != nil {
		t.Fatal(err)
	}
	if first.Goals[0].Status != agents.Achieved || first.Usage.Calls != 2 {
		t.Fatalf("first = %+v calls=%d", first.Goals[0], first.Usage.Calls)
	}
	if err := first.Write(filepath.Join(out, "counter")); err != nil {
		t.Fatal(err)
	}
	baseline := filepath.Join(out, "counter", "drift.json")

	second, err := Run(context.Background(), &verdictLLM{module: agents.Achieved, system: agents.Partial}, s, p, Options{OutDir: out, Baseline: baseline})
	if err != nil {
		t.Fatal(err)
	}
	g := second.Goals[0]
	if g.Status != agents.Partial || g.Previous != agents.Achieved || g.Change != Regressed || !second.Drifted() {
		t.Fatalf("expected a regression against the baseline: %+v", g)
	}
	if strings.Join(second.Regressions(), ",") != "G1" {
		t.Fatalf("regressions = %v", second.Regressions())
	}
	if _, err := workspace.New(filepath.Join(out, "counter"), workspace.Toolchain{}); err != nil {
		t.Fatal(err)
	}
}

// importedSpec is what `aspect import` writes for a repository whose module
// names do not spell their directories: every module records its package
// directory as its path, and the import's -exclude list is kept.
const importedSpec = `
aspect: 1
system:
  name: shop
  intent: sell things
  module_path: example.com/shop
  source: {language: go, exclude: [cmd]}
  goals:
    - {id: G1, statement: the shop builds, verify: invariant}
modules:
  - {name: root, path: ., intent: wire the shop, goals: [G1]}
  - {name: modules_api, path: modules/api, intent: serve the API, goals: [G1]}
  - {name: modules_apikeys, path: modules/apikeys, intent: issue API keys, goals: [G1]}
  - {name: modules_jobs_jobtypes, path: modules/jobs/jobtypes, intent: name the jobs, goals: [G1]}
  - {name: store_product_render, path: store/product_render, intent: render products, goals: [G1]}
`

// importedRepo writes a repository laid out like an imported one: nested
// packages, a sibling sharing a prefix (modules/api, modules/apikeys), an
// underscore in a directory name, a root package, plus code no module
// claims (tools/gen), code the import excluded (cmd/), and trees Go itself
// ignores (testdata, a nested module, a hidden worktree copy).
func importedRepo(t *testing.T, root string) {
	t.Helper()
	pkg := func(name string) string { return "package " + name + "\n\nfunc OK() bool { return true }\n" }
	test := func(name string) string {
		return "package " + name + "\n\nimport \"testing\"\n\nfunc TestOK(t *testing.T) {\n\tif !OK() {\n\t\tt.Fatal()\n\t}\n}\n"
	}
	files := map[string]string{
		"go.mod":                                 "module example.com/shop\n\ngo 1.24\n",
		"shop.go":                                pkg("shop"),
		"shop_test.go":                           test("shop"),
		"modules/api/api.go":                     pkg("api"),
		"modules/api/api_test.go":                test("api"),
		"modules/api/testdata/fixture.go":        "package fixture\n",
		"modules/apikeys/keys.go":                pkg("apikeys"),
		"modules/jobs/jobtypes/types.go":         pkg("jobtypes"),
		"modules/jobs/jobtypes/types_test.go":    test("jobtypes"),
		"store/product_render/render.go":         pkg("product_render"),
		"tools/gen/gen.go":                       pkg("gen"),
		"cmd/migrate/main.go":                    "package main\n\nfunc main() {}\n",
		"external/lib/go.mod":                    "module example.com/lib\n\ngo 1.24\n",
		"external/lib/lib.go":                    pkg("lib"),
		".claude/worktrees/x/modules/api/api.go": pkg("api"),
	}
	for p, c := range files {
		abs := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDriftFindsImportedModulesByPath(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the go toolchain")
	}
	repo := t.TempDir()
	importedRepo(t, repo)
	// The same repository reached two ways: in place, and through an -out
	// directory whose <system> entry is a symlink to the checkout.
	out := t.TempDir()
	if err := os.Symlink(repo, filepath.Join(out, "shop")); err != nil {
		t.Fatal(err)
	}
	for name, opts := range map[string]Options{
		"repo":    {Repo: repo, NoLLM: true},
		"symlink": {OutDir: out, NoLLM: true},
	} {
		t.Run(name, func(t *testing.T) {
			s, p := parse(t, importedSpec)
			rep, err := Run(context.Background(), nil, s, p, opts)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string][]string{
				"root":                  {"shop.go", "shop_test.go"},
				"modules_api":           {"modules/api/api.go", "modules/api/api_test.go"},
				"modules_apikeys":       {"modules/apikeys/keys.go"},
				"modules_jobs_jobtypes": {"modules/jobs/jobtypes/types.go", "modules/jobs/jobtypes/types_test.go"},
				"store_product_render":  {"store/product_render/render.go"},
			}
			for _, m := range rep.Modules {
				if !m.Present || !m.TestsOK {
					t.Errorf("%s: present=%v tests_ok=%v\n%s", m.Module, m.Present, m.TestsOK, m.TestOutput)
				}
				if got := strings.Join(m.Files, ","); got != strings.Join(want[m.Module], ",") {
					t.Errorf("%s files = %s, want %s", m.Module, got, strings.Join(want[m.Module], ","))
				}
			}
			if strings.Join(rep.Orphans, ",") != "tools/gen" {
				t.Errorf("orphans = %v, want only tools/gen (cmd is excluded, testdata, nested modules and hidden trees ignored)", rep.Orphans)
			}
			// The root module tests only the root package, and a placed
			// module only its own package.
			for _, m := range rep.Modules {
				switch m.Module {
				case "root":
					if !strings.Contains(m.TestOutput, "go test -count=1 -race .\n") {
						t.Errorf("root test command:\n%s", m.TestOutput)
					}
				case "modules_api":
					if !strings.Contains(m.TestOutput, "go test -count=1 -race ./modules/api\n") {
						t.Errorf("modules_api test command:\n%s", m.TestOutput)
					}
				}
			}
		})
	}
}

// A repository checked in place is never written: no go mod tidy, no
// manifest, nothing left behind.
func TestDriftInPlaceWritesNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the go toolchain")
	}
	repo := t.TempDir()
	importedRepo(t, repo)
	before := snapshot(t, repo)
	s, p := parse(t, importedSpec)
	rep, err := Run(context.Background(), nil, s, p, Options{Repo: repo, NoLLM: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range rep.Modules {
		if strings.Contains(m.TestOutput, "go mod tidy") {
			t.Fatalf("%s ran go mod tidy in place:\n%s", m.Module, m.TestOutput)
		}
	}
	if after := snapshot(t, repo); after != before {
		t.Fatalf("drift changed the repository:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// An imported spec without paths, checked with the legacy name layout,
// finds nothing; that is the failure `aspect paths` exists to repair.
func TestDriftWithoutPathsReportsMissing(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the go toolchain")
	}
	repo := t.TempDir()
	importedRepo(t, repo)
	src := strings.NewReplacer("path: ., ", "", "path: modules/api, ", "", "path: modules/apikeys, ", "",
		"path: modules/jobs/jobtypes, ", "", "path: store/product_render, ", "").Replace(importedSpec)
	s, p := parse(t, src)
	rep, err := Run(context.Background(), nil, s, p, Options{Repo: repo, NoLLM: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(rep.Missing()); got != 5 {
		t.Fatalf("missing = %v", rep.Missing())
	}
}

func snapshot(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			b.WriteString(rel + "/\n")
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		b.WriteString(rel + " " + string(data) + "\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}
