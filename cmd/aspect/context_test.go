package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eideroliveira/aspect/internal/spec"
)

func TestModulesFor(t *testing.T) {
	repo := t.TempDir()
	for rel, content := range map[string]string{
		"_aspect/aspect.yaml":     "aspect: 1\nsystem:\n  name: g\n  intent: x\n  module_path: example.com/g\n  goals: [{id: G1, statement: x}]\nmodules:\n  - {name: root, path: ., intent: a, goals: [G1]}\n  - {name: orders, path: pkg/orders, intent: a, goals: [G1]}\n",
		"main.go":                 "package main\n",
		"pkg/orders/order.go":     "package orders\n",
		"pkg/orders/sub/thing.go": "package sub\n",
	} {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := spec.Load(filepath.Join(repo, "_aspect", "aspect.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	in := func(rel string) string { return filepath.Join(repo, filepath.FromSlash(rel)) }
	names, unowned := modulesFor(s, repo, []string{in("pkg/orders/order.go"), in("pkg/orders"), in("."), "orders", in("pkg/orders/sub"), "nosuch", t.TempDir()})
	if !slices.Equal(names, []string{"orders", "root"}) {
		t.Errorf("names = %v", names)
	}
	if len(unowned) != 3 || unowned[0] != in("pkg/orders/sub") || unowned[1] != "nosuch" || !strings.Contains(unowned[2], "(outside the repository") {
		t.Errorf("unowned = %v (want the subpackage, the unknown name and the outside directory)", unowned)
	}
}

// captureStdout runs f and returns what it printed.
func captureStdout(t *testing.T, f func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = old })
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	ferr := f()
	w.Close()
	os.Stdout = old
	return <-done, ferr
}

// aspect briefs proposes without -w, writes with it (the spec still loads,
// now reading the package's ASPECT.md), names the copy nothing references
// any more, and has nothing left to do on a second run.
func TestRunBriefs(t *testing.T) {
	repo := t.TempDir()
	for rel, content := range map[string]string{
		".git/HEAD":                "ref: refs/heads/main\n",
		"_aspect/aspect.yaml":      "aspect: 1\nsystem:\n  name: g\n  intent: x\n  module_path: example.com/g\n  goals: [{id: G1, statement: x}]\nmodules:\n  - dir: modules/\n",
		"_aspect/modules/o.yaml":   "name: orders\npath: pkg/orders\nintent: a\ngoals: [G1]\nscenarios: [{id: S1, when: a, then: b}]\nbrief: ../briefs/orders.md\n",
		"_aspect/briefs/orders.md": "<!-- Recovered by aspect import from example.com/g/pkg/orders. Edit freely; this is the module's brief. -->\n\n<!-- CLAUDE.md -->\n\nOld rules.",
		"pkg/orders/ASPECT.md":     "Orders are never deleted.\n",
	} {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	specPath := filepath.Join(repo, "_aspect", "aspect.yaml")
	fragment := filepath.Join(repo, "_aspect", "modules", "o.yaml")
	before, _ := os.ReadFile(fragment)

	out, err := captureStdout(t, func() error { return runBriefs(specPath, nil) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "briefs/orders.md -> ../pkg/orders/ASPECT.md") || !strings.Contains(out, "the copy also held CLAUDE.md") || !strings.Contains(out, "rerun with -w") {
		t.Errorf("dry run:\n%s", out)
	}
	if after, _ := os.ReadFile(fragment); string(after) != string(before) {
		t.Error("a dry run writes nothing")
	}

	out, err = captureStdout(t, func() error { return runBriefs(specPath, []string{"-w"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "moved 1 brief(s) in 1 file(s)") || !strings.Contains(out, "no longer referenced: "+filepath.Join(repo, "_aspect", "briefs", "orders.md")) {
		t.Errorf("write:\n%s", out)
	}
	s, err := spec.Load(specPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Module("orders").Brief.Text; got != "Orders are never deleted.\n" {
		t.Errorf("brief now reads %q", got)
	}

	out, err = captureStdout(t, func() error { return runBriefs(specPath, nil) })
	if err != nil || !strings.Contains(out, "no brief to move") {
		t.Errorf("second run: %v\n%s", err, out)
	}
}

// aspect context prints the module's rules, leaves out what its flags say,
// and fails when nothing it names belongs to a module.
func TestRunContext(t *testing.T) {
	repo := t.TempDir()
	for rel, content := range map[string]string{
		".git/HEAD":            "ref: refs/heads/main\n",
		"_aspect/aspect.yaml":  "aspect: 1\nsystem:\n  name: g\n  intent: x\n  module_path: example.com/g\n  constraints: [Money is cents.]\n  goals: [{id: G1, statement: x}]\nmodules:\n  - {name: orders, path: pkg/orders, intent: a, goals: [G1], brief: ../pkg/orders/ASPECT.md, invariants: [One live charge.]}\n",
		"pkg/orders/ASPECT.md": "Orders are never deleted.\n",
		"pkg/orders/order.go":  "package orders\n",
	} {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	specPath := filepath.Join(repo, "_aspect", "aspect.yaml")
	file := filepath.Join(repo, "pkg", "orders", "order.go")
	out, err := captureStdout(t, func() error { return runContext(specPath, []string{file}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Money is cents.", "## Module orders (pkg/orders)", "One live charge.", "Orders are never deleted."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	out, err = captureStdout(t, func() error { return runContext(specPath, []string{"-system=false", "-briefs=false", "orders"}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Money is cents.") || strings.Contains(out, "never deleted") || !strings.Contains(out, "One live charge.") {
		t.Errorf("-system=false -briefs=false:\n%s", out)
	}
	var ec exitCode
	if _, err := captureStdout(t, func() error { return runContext(specPath, []string{"nosuch"}) }); !errors.As(err, &ec) || ec.code != 1 {
		t.Errorf("nothing to show must exit 1, got %v", err)
	}
}
