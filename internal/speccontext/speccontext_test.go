package speccontext

import (
	"strings"
	"testing"

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
	out := Render(load(t), []string{"payments", "unknown", "payments"}, Options{System: true, Briefs: true})
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
	full := Render(s, []string{"payments", "store"}, Options{System: true, Briefs: true})
	out := Render(s, []string{"payments", "store"}, Options{System: true, Briefs: true, Budget: 600})
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

	tiny := Render(s, []string{"payments"}, Options{Budget: 120})
	if !strings.Contains(tiny, "[cut to fit the size limit]") || !strings.Contains(tiny, "the rest of the rules of payments") {
		t.Errorf("rules that do not fit are cut, not dropped:\n%s", tiny)
	}
}

func TestRenderNothing(t *testing.T) {
	if out := Render(load(t), []string{"unknown"}, Options{}); out != "" {
		t.Errorf("want nothing, got %q", out)
	}
}
