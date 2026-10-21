package main

import (
	"os"
	"path/filepath"
	"slices"
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
	if len(unowned) != 3 {
		t.Errorf("unowned = %v (want the subpackage, the unknown name and the outside directory)", unowned)
	}
}
