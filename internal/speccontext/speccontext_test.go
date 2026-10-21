package speccontext

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/eideroliveira/aspect/internal/spec"
)

const src = `
aspect: 1
system:
  name: shop
  intent: sell
  module_path: example.com/shop
  constraints: [Money is integer cents.]
  goals:
    - {id: G1, statement: An order carries at most one live charge.}
modules:
  - name: payments
    path: modules/payments
    intent: >
      Charge money for an order
      through one gateway.
    goals: [G1]
    entities: [Order]
    constraints: [Never branch on the provider.]
    invariants:
      - A cancel goes to the provider that issued the charge.
      - {id: INV-LEASE, statement: One writer at a time.}
    interface:
      - {name: Resolve, signature: "Resolve(method: string) -> Gateway", intent: For a new charge., pre: ["method != \"\""]}
    scenarios:
      - {id: S1, given: a boleto order, when: the method changes, then: the old slip is cancelled first}
  - name: store
    intent: Show products.
    goals: [G1]
    invariants: [Only sellable products are listed.]
`

func load(t *testing.T) *spec.Spec {
	t.Helper()
	s, err := spec.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	s.Module("payments").Brief = spec.Brief{Path: "briefs/payments.md", Text: strings.Repeat("Detach before you issue. ", 200)}
	return s
}

func TestRenderPutsTheRulesFirst(t *testing.T) {
	out := Render(load(t), []string{"payments", "unknown", "payments"}, Options{})
	for _, want := range []string{
		"## System constraints", "- Money is integer cents.",
		"## Module payments (modules/payments)", "Intent: Charge money for an order through one gateway.",
		"- G1: An order carries at most one live charge.", "Entities it owns", "Order",
		"- Never branch on the provider.", "- [0] A cancel goes to the provider", "- [INV-LEASE] One writer at a time.",
		"`Resolve(method: string) -> Gateway`: For a new charge.", "pre: method != \"\"",
		"- S1: given a boleto order; when the method changes; then the old slip is cancelled first",
		"### Brief (briefs/payments.md)", "Detach before you issue.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Count(out, "## Module payments") != 1 {
		t.Error("a module named twice is rendered once")
	}
	if strings.Contains(out, "Left out") {
		t.Error("nothing is left out without a budget")
	}
	if i, j := strings.Index(out, "Invariants"), strings.Index(out, "### Brief"); i > j {
		t.Error("the rules come before the brief")
	}
}

func TestRenderFitsTheBudgetAndSaysWhatItLeftOut(t *testing.T) {
	s := load(t)
	full := Render(s, []string{"payments", "store"}, Options{})
	out := Render(s, []string{"payments", "store"}, Options{Budget: 600})
	if len(out) > 600*CharsPerToken {
		t.Fatalf("rendering of %d chars exceeds the budget", len(out))
	}
	if len(out) >= len(full) {
		t.Fatal("the budget must have cut something")
	}
	// Both modules' rules survive; the brief, last in line, does not.
	for _, want := range []string{"One writer at a time.", "Only sellable products are listed.", "Left out to fit the size limit: brief of payments (briefs/payments.md)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Grouped by module even though the parts were chosen in budget order.
	if i, j := strings.Index(out, "### Scenarios"), strings.Index(out, "## Module store"); i < 0 || i > j {
		t.Errorf("payments' scenarios belong under payments:\n%s", out)
	}

	tiny := Render(s, []string{"payments"}, Options{Budget: 140, OmitSystem: true})
	if !strings.Contains(tiny, "[cut to fit the size limit]") || !strings.Contains(tiny, "the rest of the rules of payments") {
		t.Errorf("rules that do not fit are cut, not dropped:\n%s", tiny)
	}
}

func TestRenderNothing(t *testing.T) {
	if out := Render(load(t), []string{"unknown"}, Options{OmitSystem: true}); out != "" {
		t.Errorf("want nothing, got %q", out)
	}
}

// A module whose rules did not fit shows nothing else, or its scenarios
// would print under the previous module's heading.
func TestRenderDropsAllOfAModuleWhoseRulesDoNotFit(t *testing.T) {
	s := load(t)
	s.Module("store").Scenarios = []spec.Scenario{{ID: "S9", When: "w", Then: "t"}}
	pay := len(core(s, s.Module("payments")))
	// Room for payments' rules, then less than minCutRoom left.
	budget := (pay + omittedNoteReserve + minCutRoom/2) / CharsPerToken
	out := Render(s, []string{"payments", "store"}, Options{Budget: budget, OmitSystem: true})
	if !strings.Contains(out, "One writer at a time.") {
		t.Fatalf("payments' rules fit:\n%s", out)
	}
	if strings.Contains(out, "S9") {
		t.Errorf("store's scenarios shown without its rules:\n%s", out)
	}
	for _, want := range []string{"the rules of store", "scenarios of store"} {
		if !strings.Contains(out, want) {
			t.Errorf("the note must name %q:\n%s", want, out)
		}
	}
}

// A cut never splits a UTF-8 character, and the note on what was left out
// stays within its reserve however much was.
func TestRenderCutsAtCharactersAndShortensTheNote(t *testing.T) {
	s := load(t)
	m := s.Module("payments")
	m.Invariants = append(m.Invariants, spec.Invariant{Statement: strings.Repeat("Cobrança é única — ", 200)})
	for budget := 150; budget < 260; budget++ {
		out := Render(s, []string{"payments"}, Options{Budget: budget, OmitSystem: true})
		if !utf8.ValidString(out) {
			t.Fatalf("budget %d: cut inside a character", budget)
		}
		if len(out) > budget*CharsPerToken {
			t.Fatalf("budget %d: %d chars", budget, len(out))
		}
	}
	var many []string
	for i := 0; i < 40; i++ {
		many = append(many, fmt.Sprintf("scenarios of module_number_%d", i))
	}
	note := omittedNote(many)
	if len(note) > omittedNoteReserve+60 || !strings.Contains(note, "more") || !strings.Contains(note, "module_number_0") {
		t.Errorf("note (%d chars): %s", len(note), note)
	}
}
