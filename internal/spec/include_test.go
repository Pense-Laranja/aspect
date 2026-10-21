package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSplitSpecWithDependency(t *testing.T) {
	s, err := Load("testdata/split/aspect.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(moduleNames(s), ","); got != "catalog,orders,audit" {
		t.Fatalf("modules = %s (dir includes sorted by file name, lists spliced)", got)
	}
	if len(s.System.Interfaces) != 1 || s.System.Interfaces[0].Name != "api" {
		t.Fatalf("interfaces = %+v", s.System.Interfaces)
	}
	if b := s.Module("catalog").Brief; b.Path != "modules/catalog.md" || !strings.Contains(b.Text, "immutable") {
		t.Fatalf("brief in an included file must be rebased to the root and loaded: %+v", b)
	}
	if s.System.Brief.Text != "The shop brief.\n" {
		t.Fatalf("root brief = %q", s.System.Brief.Text)
	}
	dep, ok := s.Deps["identity"]
	if !ok || dep.System.Name != "identity" || dep.Path == "" {
		t.Fatalf("dependency not loaded: %+v", s.Deps)
	}
	issues := Validate(s)
	if issues.HasErrors() {
		t.Fatalf("split spec must validate:\n%s", join(issues))
	}
	refs, errs := s.ResolveSurfaces([]string{"identity/api.whoami", "identity/api", "api.reserve"})
	if len(errs) != 0 || len(refs) != 4 || refs[0].ID() != "identity/api.whoami" || refs[0].System != "identity" || refs[3].ID() != "api.reserve" {
		t.Fatalf("refs = %+v errs = %v", refs, errs)
	}
	_, errs = s.ResolveSurfaces([]string{"ghost/api.x", "identity/api.nope"})
	if len(errs) != 2 || !strings.Contains(errs[0].Error(), "unknown system") || !strings.Contains(errs[1].Error(), "unknown surface") {
		t.Fatalf("errs = %v", errs)
	}

	expanded, err := Expand("testdata/split/aspect.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(expanded), "file:") || !strings.Contains(string(expanded), "name: orders") {
		t.Fatalf("Expand must inline every include:\n%s", expanded)
	}
}

func TestValidateRejectsImplementingDependencySurfaces(t *testing.T) {
	s, err := Load("testdata/split/aspect.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s.Module("orders").Surfaces = append(s.Module("orders").Surfaces, "identity/api.login")
	text := join(Validate(s))
	if !strings.Contains(text, `surface "identity/api.login" belongs to system "identity"; consume it`) {
		t.Fatalf("want error, got:\n%s", text)
	}
}

func TestParseWithoutLoadingWarnsAboutDependencies(t *testing.T) {
	src := `
aspect: 1
system:
  name: s
  intent: i
  module_path: m
  dependencies: [{name: identity, spec: ../identity/aspect.yaml}, {name: external, spec: x.yaml}]
  goals: [{id: G1, statement: x}]
modules:
  - {name: m, intent: i, goals: [G1], consumes: [identity/api.login], scenarios: [{id: S1, when: a, then: b}]}
`
	s, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	text := join(Validate(s))
	for _, want := range []string{`dependency "identity" was not loaded`, `"external" is reserved`, `dependency "identity" is not loaded, cannot resolve`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}

func TestIncludeErrors(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.yaml", "aspect: 1\nsystem: {file: b.yaml}\n")
	write("b.yaml", "name: s\nintent: {file: a.yaml}\n")
	if _, err := Load(filepath.Join(dir, "a.yaml")); err == nil || !strings.Contains(err.Error(), "include cycle") {
		t.Fatalf("want cycle error, got %v", err)
	}
	write("c.yaml", "aspect: 1\nsystem: {dir: mods}\n")
	if _, err := Load(filepath.Join(dir, "c.yaml")); err == nil || !strings.Contains(err.Error(), "only valid inside lists") {
		t.Fatalf("want dir-in-mapping error, got %v", err)
	}
	write("d.yaml", "aspect: 1\nmodules:\n  - file: missing.yaml\n")
	if _, err := Load(filepath.Join(dir, "d.yaml")); err == nil || !strings.Contains(err.Error(), "missing.yaml") {
		t.Fatalf("want missing include error, got %v", err)
	}
	write("e.yaml", "aspect: 1\nsystem:\n  name: e\n  intent: i\n  module_path: m\n  dependencies: [{name: dep, spec: broken.yaml}]\n  goals: [{id: G1, statement: x}]\nmodules: [{name: m, intent: i, goals: [G1]}]\n")
	write("broken.yaml", "aspect: 1\nsystem: {name: Bad Name, intent: i}\n")
	if _, err := Load(filepath.Join(dir, "e.yaml")); err == nil || !strings.Contains(err.Error(), "dependency dep") || !strings.Contains(err.Error(), "has errors") {
		t.Fatalf("a dependency with errors must fail the load, got %v", err)
	}
	write("f.yaml", "aspect: 1\nsystem:\n  name: f\n  intent: i\n  module_path: m\n  dependencies: [{name: g, spec: g.yaml}]\n  goals: [{id: G1, statement: x}]\nmodules: [{name: m, intent: i, goals: [G1]}]\n")
	write("g.yaml", "aspect: 1\nsystem:\n  name: g\n  intent: i\n  module_path: m\n  dependencies: [{name: f, spec: f.yaml}]\n  goals: [{id: G1, statement: x}]\nmodules: [{name: m, intent: i, goals: [G1]}]\n")
	if _, err := Load(filepath.Join(dir, "f.yaml")); err == nil || !strings.Contains(err.Error(), "dependency cycle") {
		t.Fatalf("want dependency cycle error, got %v", err)
	}
}

func moduleNames(s *Spec) []string {
	var out []string
	for _, m := range s.AllModules() {
		out = append(out, m.Name)
	}
	return out
}

// What a spec includes is sent to a model by aspect review, so an include
// may not leave the repository, directly, through a dir: include or a
// symbolic link, nor read anything under .git.
func TestIncludesStayInsideTheRepository(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "token.yaml"), []byte("name: m\nintent: secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	for _, d := range []string{".git", "_aspect/modules"} {
		if err := os.MkdirAll(filepath.Join(repo, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "m.yaml"), []byte("name: m\nintent: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	specPath := filepath.Join(repo, "_aspect", "aspect.yaml")
	head := "aspect: 1\nsystem:\n  name: g\n  intent: x\n  module_path: example.com/g\n  goals: [{id: G1, statement: x}]\nmodules:\n"
	rel := func(target string) string {
		r, err := filepath.Rel(filepath.Join(repo, "_aspect"), target)
		if err != nil {
			t.Fatal(err)
		}
		return filepath.ToSlash(r)
	}
	link := filepath.Join(repo, "_aspect", "modules", "link.yaml")
	canLink := os.Symlink(filepath.Join(outside, "token.yaml"), link) == nil
	for name, tc := range map[string]struct{ include, want string }{
		"outside":     {"  - file: " + rel(filepath.Join(outside, "token.yaml")) + "\n", "must stay inside the repository"},
		"absolute":    {"  - file: " + filepath.ToSlash(filepath.Join(outside, "token.yaml")) + "\n", "must stay inside the repository"},
		"outside dir": {"  - dir: " + rel(outside) + "\n", "must stay inside the repository"},
		"git":         {"  - file: ../.git/m.yaml\n", "under .git"},
		"link":        {"  - file: modules/link.yaml\n", "through a symbolic link"},
	} {
		if name == "link" && !canLink {
			continue
		}
		if err := os.WriteFile(specPath, []byte(head+tc.include), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(specPath); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
	}
}
