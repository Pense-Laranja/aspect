package analyze

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/eideroliveira/aspect/internal/spec"
)

func TestVerifyModuleDependencies(t *testing.T) {
	s := &spec.Spec{
		Modules: []spec.Module{
			{Name: "cart", DependsOn: []string{"models"}},
			{Name: "checkout", DependsOn: []string{"cart"}},
			{Name: "models"},
		},
	}
	inv := &Inventory{
		Language:   "go",
		ModulePath: "example.com/app",
		Packages: []Package{
			{
				Dir:     "checkout",
				Imports: []string{"example.com/app/models"}, // checkout imports models without declaring DependsOn!
			},
		},
	}

	violations := VerifyModuleDependencies(inv, s)
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d: %v", len(violations), violations)
	}
	if violations[0].Rule != "module-depends-on" || violations[0].Category != "boundary" {
		t.Errorf("unexpected violation properties: %+v", violations[0])
	}
}

func TestCheckCodeInvariantsAST(t *testing.T) {
	dir := t.TempDir()

	code := `package sample

import "net/http"

type DB struct{}
func (d *DB) Find(out any) *DB { return d }

func Process(items []string, db *DB) {
	// Violation 1: HTTP Client without Timeout
	client := &http.Client{}
	_ = client

	// Violation 2: Unbounded Find without Limit
	db.Find(&items)

	// Violation 3: N+1 query in loop
	for _, it := range items {
		db.Find(it)
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}

	violations, err := CheckCodeInvariants(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	rules := map[string]bool{}
	for _, v := range violations {
		rules[v.Rule] = true
	}

	for _, want := range []string{"http-client-timeout", "unbounded-query-limit", "n-plus-one-query-in-loop"} {
		if !rules[want] {
			t.Errorf("expected rule %q to be violated, got violations: %v", want, violations)
		}
	}
}
