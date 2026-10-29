package plan

import (
	"reflect"
	"testing"

	"github.com/Pense-Laranja/aspect/internal/spec"
)

func parse(t *testing.T, src string) *spec.Spec {
	t.Helper()
	s, err := spec.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBuildOrdersByDependencyThenName(t *testing.T) {
	s := parse(t, `
aspect: 1
system: {name: s, intent: i, module_path: m, goals: [{id: G1, statement: x}]}
modules:
  - {name: web, intent: i, goals: [G1], depends_on: [stock, ledger]}
  - {name: stock, intent: i, depends_on: [ledger]}
  - {name: audit, intent: i, depends_on: [ledger]}
  - {name: ledger, intent: i}
`)
	p, err := Build(s)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ledger", "audit", "stock", "web"}
	if got := p.Order(); !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestBuildCollectsGoalsFromScenarios(t *testing.T) {
	s := parse(t, `
aspect: 1
system: {name: s, intent: i, module_path: m, goals: [{id: G1, statement: x}, {id: G2, statement: y}]}
modules:
  - name: core
    intent: i
    goals: [G2]
    invariants: [always]
    scenarios:
      - {id: S1, when: w, then: t, goals: [G1, G2]}
`)
	p, err := Build(s)
	if err != nil {
		t.Fatal(err)
	}
	st := p.Steps()[0]
	if !reflect.DeepEqual(st.Goals, []string{"G1", "G2"}) || st.Scenarios != 1 || st.Invariants != 1 {
		t.Fatalf("step = %+v", st)
	}
}

func TestBuildRejectsCycle(t *testing.T) {
	s := parse(t, `
aspect: 1
system: {name: s, intent: i, module_path: m, goals: [{id: G1, statement: x}]}
modules:
  - {name: a, intent: i, goals: [G1], depends_on: [b]}
  - {name: b, intent: i, depends_on: [a]}
`)
	if _, err := Build(s); err == nil {
		t.Fatal("want cycle error")
	}
}

func TestBuildOrdersTiersByDependency(t *testing.T) {
	s := parse(t, `
aspect: 1
system:
  name: s
  intent: i
  goals: [{id: G1, statement: x}]
  interfaces:
    - {name: api, kind: http, intent: i, provider: backend, surfaces: [{name: a, route: /a, method: GET}]}
tiers:
  - name: mobile
    intent: i
    language: swift
    module_path: com.example.s
    depends_on: [backend]
    modules:
      - {name: client, intent: i, goals: [G1], consumes: [api.a]}
  - name: backend
    intent: i
    language: go
    module_path: example.com/s
    modules:
      - {name: web, intent: i, goals: [G1], depends_on: [core], surfaces: [api]}
      - {name: core, intent: i}
`)
	p, err := Build(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Tiers) != 2 || p.Tiers[0].Name != "backend" || p.Tiers[0].Language != "go" || p.Tiers[1].Name != "mobile" {
		t.Fatalf("tiers = %+v", p.Tiers)
	}
	if got := p.Order(); !reflect.DeepEqual(got, []string{"core", "web", "client"}) {
		t.Fatalf("order = %v", got)
	}
	if st := p.Tiers[1].Steps[0]; st.Tier != "mobile" || !reflect.DeepEqual(st.Consumes, []string{"api.a"}) {
		t.Fatalf("client step = %+v", st)
	}
}

// The Tester covers a module's invariants and its topics' alike.
func TestStepCountsTopicInvariants(t *testing.T) {
	s := parse(t, `
aspect: 1
system: {name: s, intent: i, module_path: m, goals: [{id: G1, statement: x}]}
modules:
  - name: core
    intent: i
    goals: [G1]
    invariants: [always]
    topics:
      - {name: a, files: [a/*.go], invariants: [x, y]}
      - {name: b, files: [b/*.go], constraints: [c]}
`)
	p, err := Build(s)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Steps()[0].Invariants; got != 3 {
		t.Fatalf("invariants = %d, want 3", got)
	}
}
