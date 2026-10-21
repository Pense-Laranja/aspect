package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pathsSpec = `aspect: 1
system:
  name: shop
  intent: sell things
  module_path: example.com/shop
  goals:
    - {id: G1, statement: things sell}
modules:
  # The root package.
  - name: root
    intent: wire the shop
    goals: [G1]
  - name: modules_api
    intent: serve the API # recovered by import
    goals: [G1]
  - name: models
    path: models
    intent: hold the data
    goals: [G1]
`

func TestValidateModulePaths(t *testing.T) {
	src := strings.NewReplacer(
		"  - name: root\n", "  - name: root\n    path: ./\n",
		"  - name: modules_api\n", "  - name: modules_api\n    path: ../api\n",
	).Replace(pathsSpec) + "  - name: models_again\n    path: models\n    intent: duplicate\n    goals: [G1]\n"
	s, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	text := join(Validate(s))
	for _, want := range []string{
		`modules[0].path: module "root": path "./" must be a clean relative slash path`,
		`modules[1].path: module "modules_api": path "../api" leaves the code root`,
		`modules[3].path: module "models_again": path "models" is already module "models"'s`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing issue containing %q\nall issues:\n%s", want, text)
		}
	}

	// A path outside the root, written with backslashes, or nested inside a
	// module laid out by name (which owns everything below <name>/).
	for p, want := range map[string]string{
		"/abs/api":             `path "/abs/api" must be a clean relative slash path`,
		"modules\\api":         `path "modules\\api" must be a clean relative slash path`,
		"modules_api/internal": `path "modules_api/internal" lies inside modules_api/, which module "modules_api" owns entirely`,
		"modules_api":          `path "modules_api" lies inside modules_api/`,
	} {
		src := pathsSpec + "  - name: extra\n    path: '" + p + "'\n    intent: x\n    goals: [G1]\n"
		s, err := Parse([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		if text := join(Validate(s)); !strings.Contains(text, want) {
			t.Errorf("path %q: missing issue containing %q\nall issues:\n%s", p, want, text)
		}
	}

	s, err = Parse([]byte(strings.Replace(pathsSpec, "  module_path: example.com/shop\n", "  language: swift\n  module_path: example.com/shop\n", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if text := join(Validate(s)); !strings.Contains(text, `path is supported only in Go tiers (this one is swift)`) {
		t.Errorf("a path outside Go must be refused:\n%s", text)
	}
}

func TestSetModulePathsSingleFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "aspect.yaml")
	if err := os.WriteFile(file, []byte(pathsSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := SetModulePaths(file, map[string]string{"root": ".", "modules_api": "modules/api", "models": "elsewhere", "ghost": "ghost"})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0] != file {
		t.Fatalf("changed = %v", changed)
	}
	got, _ := os.ReadFile(file)
	want := strings.NewReplacer(
		"  - name: root\n", "  - name: root\n    path: .\n",
		"  - name: modules_api\n", "  - name: modules_api\n    path: modules/api\n",
	).Replace(pathsSpec)
	if string(got) != want {
		t.Fatalf("comments and layout must be kept, an existing path left alone:\n%s", got)
	}
	s, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if s.Module("root").Path != "." || s.Module("modules_api").Path != "modules/api" || s.Module("models").Path != "models" {
		t.Fatalf("paths = %q %q %q", s.Module("root").Path, s.Module("modules_api").Path, s.Module("models").Path)
	}
}

// A split spec gets each path in the module's own fragment, and split and
// expand carry the field through.
func TestSetModulePathsInFragmentsAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	splitInto(t, dir, pathsSpec, nil)
	root := filepath.Join(dir, "aspect.yaml")
	changed, err := SetModulePaths(root, map[string]string{"root": ".", "modules_api": "modules/api"})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 2 || filepath.Base(changed[0]) != "modules_api.yaml" || filepath.Base(changed[1]) != "root.yaml" {
		t.Fatalf("changed = %v", changed)
	}
	frag, _ := os.ReadFile(filepath.Join(dir, "modules", "modules_api.yaml"))
	if !strings.HasPrefix(string(frag), "name: modules_api\npath: modules/api\n") {
		t.Fatalf("fragment:\n%s", frag)
	}
	expanded, err := Expand(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"path: .\n", "path: modules/api\n", "path: models\n"} {
		if !strings.Contains(string(expanded), want) {
			t.Errorf("expand lost %q:\n%s", want, expanded)
		}
	}
	// Splitting the expanded document again keeps every path.
	_, s := splitInto(t, t.TempDir(), string(expanded), nil)
	if issues := Validate(s); issues.HasErrors() {
		t.Fatal(join(issues))
	}
	if s.Module("root").Path != "." || s.Module("modules_api").Path != "modules/api" || s.Module("models").Path != "models" {
		t.Fatalf("paths after split = %q %q %q", s.Module("root").Path, s.Module("modules_api").Path, s.Module("models").Path)
	}
}

func TestSetModulePathsRefusesFlowMappings(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "aspect.yaml")
	src := strings.Replace(pathsSpec, "  - name: root\n    intent: wire the shop\n    goals: [G1]\n", "  - {name: root, intent: wire the shop, goals: [G1]}\n", 1)
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SetModulePaths(file, map[string]string{"root": "."}); err == nil || !strings.Contains(err.Error(), "flow mapping") {
		t.Fatalf("err = %v", err)
	}
}

// SetModuleBriefs rewrites a brief where it is written, relative to that
// file, keeping the line's comment, and adds one where there was none.
func TestSetModuleBriefs(t *testing.T) {
	repo := t.TempDir()
	for _, d := range []string{".git", "_aspect/modules", "_aspect/briefs", "pkg/orders", "pkg/store"} {
		if err := os.MkdirAll(filepath.Join(repo, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"_aspect/aspect.yaml":         "aspect: 1\nsystem:\n  name: g\n  intent: shop\n  module_path: example.com/g\n  goals: [{id: G1, statement: x}]\nmodules:\n  - name: store\n    path: pkg/store\n    intent: store\n    goals: [G1]\n  - dir: modules/\n",
		"_aspect/modules/orders.yaml": "name: orders\npath: pkg/orders\nintent: orders\ngoals: [G1]\nbrief: ../briefs/orders.md # recovered by import\n",
		"_aspect/briefs/orders.md":    "copy",
		"pkg/orders/ASPECT.md":        "orders rules",
		"pkg/store/ASPECT.md":         "store rules",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	specPath := filepath.Join(repo, "_aspect", "aspect.yaml")
	changed, err := SetModuleBriefs(specPath, map[string]string{"orders": "../pkg/orders/ASPECT.md", "store": "../pkg/store/ASPECT.md", "missing": "x.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 2 {
		t.Fatalf("changed %v", changed)
	}
	frag, _ := os.ReadFile(filepath.Join(repo, "_aspect", "modules", "orders.yaml"))
	if !strings.Contains(string(frag), "brief: ../../pkg/orders/ASPECT.md # recovered by import\n") {
		t.Errorf("fragment:\n%s", frag)
	}
	rootFile, _ := os.ReadFile(specPath)
	if !strings.Contains(string(rootFile), "  - name: store\n    brief: ../pkg/store/ASPECT.md\n    path: pkg/store\n") {
		t.Errorf("root:\n%s", rootFile)
	}
	s, err := Load(specPath)
	if err != nil {
		t.Fatal(err)
	}
	if s.Module("orders").Brief.Text != "orders rules" || s.Module("store").Brief.Text != "store rules" {
		t.Errorf("briefs: %q %q", s.Module("orders").Brief.Text, s.Module("store").Brief.Text)
	}
}

// What SetModuleBriefs cannot edit as one line it refuses, leaving the file
// as it was.
func TestSetModuleBriefsRefusesWhatItCannotEditSafely(t *testing.T) {
	for name, mod := range map[string]string{
		"flow":  "modules:\n  - {name: m, intent: a, goals: [G1]}\n",
		"block": "modules:\n  - name: m\n    intent: a\n    goals: [G1]\n    brief: |\n      x.md\n",
	} {
		p := filepath.Join(t.TempDir(), "aspect.yaml")
		src := "aspect: 1\nsystem: {name: g, intent: x, module_path: example.com/g, goals: [{id: G1, statement: x}]}\n" + mod
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := SetModuleBriefs(p, map[string]string{"m": "b.md"}); err == nil || !strings.Contains(err.Error(), "by hand") {
			t.Errorf("%s: err = %v", name, err)
		}
		if got, _ := os.ReadFile(p); string(got) != src {
			t.Errorf("%s: file changed on refusal:\n%s", name, got)
		}
	}
}

// Several briefs in one file: each line edit lands where it belongs, whether
// it replaces a brief or inserts one, above or below the other.
func TestSetModuleBriefsSeveralInOneFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "aspect.yaml")
	src := "aspect: 1\nsystem: {name: g, intent: x, module_path: example.com/g, goals: [{id: G1, statement: x}]}\nmodules:\n" +
		"  - name: a\n    intent: a\n    brief: old-a.md\n" +
		"  - name: b\n    intent: b\n" +
		"  - name: c\n    intent: c\n    brief: old-c.md # keep\n"
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SetModuleBriefs(p, map[string]string{"a": "new-a.md", "b": "new-b.md", "c": "new-c.md"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	want := "aspect: 1\nsystem: {name: g, intent: x, module_path: example.com/g, goals: [{id: G1, statement: x}]}\nmodules:\n" +
		"  - name: a\n    intent: a\n    brief: new-a.md\n" +
		"  - name: b\n    brief: new-b.md\n    intent: b\n" +
		"  - name: c\n    intent: c\n    brief: new-c.md # keep\n"
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
