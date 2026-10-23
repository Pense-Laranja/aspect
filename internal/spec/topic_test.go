package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for p, body := range files {
		abs := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestTopicBriefsAreRebasedLoadedAndLocated(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"go.mod": "module example.com/shop\n",
		"_aspect/aspect.yaml": `aspect: 1
system:
  name: shop
  intent: sell
  module_path: example.com/shop
  goals: [{id: G1, statement: Charge once.}]
modules:
  - dir: modules/
`,
		"_aspect/modules/pay.yaml": `name: pay
path: pay
intent: Charge money.
goals: [G1]
invariants: [{id: ONE, statement: One live charge per order.}]
topics:
  - name: refunds
    intent: Giving money back.
    files: [pay/refund*.go, admin/refund_*.go]
    constraints: [A refund names the charge it returns.]
    invariants: [{id: REF, statement: A refund never exceeds its charge.}]
    brief: ../../pay/rules/refunds.md
`,
		"pay/rules/refunds.md": "Refunds go through the gateway that charged.\n",
	})
	s := mustLoad(t, filepath.Join(dir, "_aspect", "aspect.yaml"))
	tp := s.Module("pay").Topics[0]
	if tp.Brief.Path != "../pay/rules/refunds.md" || !strings.Contains(tp.Brief.Text, "gateway that charged") {
		t.Fatalf("a topic's brief in an included file must be rebased to the root spec and loaded: %+v", tp.Brief)
	}
	if issues := Validate(s); issues.HasErrors() {
		t.Fatalf("spec must validate:\n%s", join(issues))
	}
	if b := s.Briefs()["module pay.topics[0]"]; b == nil || b.Text != tp.Brief.Text {
		t.Fatalf("Briefs() must list the topic's brief under its owner: %v", b)
	}
	if loc := s.Sources.Locate("module pay.topics[0].brief"); !strings.HasSuffix(loc, "pay.yaml:12") {
		t.Fatalf("Locate(topic brief) = %q, want the fragment's line 12", loc)
	}
	for file, want := range map[string]bool{"pay/refund.go": true, "admin/refund_page.go": true, "admin/refund/x.go": false, "pay/charge.go": false} {
		if got := tp.Covers(file); got != want {
			t.Errorf("Covers(%q) = %v, want %v", file, got, want)
		}
	}
	// A change touches a topic through its files, its brief and the
	// module's fragment; the root spec file touches none.
	pay := s.Module("pay")
	for file, want := range map[string]int{
		"admin/refund_page.go":     1,
		"pay/rules/refunds.md":     1,
		"_aspect/modules/pay.yaml": 1,
		"_aspect/aspect.yaml":      0,
		"pay/charge.go":            0,
	} {
		if touched, rest := s.TopicsTouching(pay, []string{file}); len(touched) != want || len(touched)+len(rest) != 1 {
			t.Errorf("TopicsTouching(%q) = %d touched, %d rest; want %d touched", file, len(touched), len(rest), want)
		}
	}
	if got := s.TopicBrief(&tp); got != "pay/rules/refunds.md" {
		t.Errorf("TopicBrief = %q, want the repository-relative path", got)
	}
}

// A module whose invariants all live in its topics is not warned that it
// has none; one whose topics hold none still is.
func TestTopicInvariantsSilenceTheNoInvariantsWarning(t *testing.T) {
	for topic, wantWarn := range map[string]bool{"invariants: [Capped.]": false, "constraints: [c]": true} {
		s, err := Parse([]byte(`
aspect: 1
system: {name: s, intent: i, module_path: m, goals: [{id: G1, statement: x}]}
modules:
  - name: pay
    intent: i
    goals: [G1]
    topics:
      - {name: refunds, files: [pay/r*.go], ` + topic + `}
`))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(join(Validate(s)), "has no scenarios or invariants"); got != wantWarn {
			t.Errorf("%s: warning = %v, want %v", topic, got, wantWarn)
		}
	}
}

func TestValidateTopics(t *testing.T) {
	s, err := Parse([]byte(`
aspect: 1
system:
  name: shop
  intent: sell
  module_path: example.com/shop
  goals: [{id: G1, statement: Charge once.}]
modules:
  - name: pay
    intent: Charge money.
    goals: [G1]
    invariants: [{id: ONE, statement: One live charge.}]
    scenarios:
      - {id: S1, when: a refund is asked, then: it is capped, invariants: [REF]}
    topics:
      - name: refunds
        files: [pay/refund*.go]
        invariants: [{id: REF, statement: Capped.}, {id: ONE, statement: Reused id.}]
      - name: refunds
        files: ["pay/[x.go"]
        constraints: [""]
      - name: Bad-Name
      - name: empty
        files: [pay/empty.go]
`))
	if err != nil {
		t.Fatal(err)
	}
	out := join(Validate(s))
	for _, want := range []string{
		`modules[0].topics[0].invariants[1].id: duplicate invariant id "ONE"`,
		`modules[0].topics[1].name: duplicate topic name "refunds"`,
		`modules[0].topics[1].files[0]: bad path pattern`,
		`modules[0].topics[1].constraints[0]: cannot be empty`,
		`modules[0].topics[2].name: "Bad-Name" must match`,
		`modules[0].topics[2].files: is required`,
		`warning: modules[0].topics[3]: topic "empty" has no constraints, invariants or brief`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "unknown invariant id \"REF\"") {
		t.Errorf("a scenario may name a topic's invariant:\n%s", out)
	}
}
