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
