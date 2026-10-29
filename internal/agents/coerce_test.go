package agents

import (
	"strings"
	"testing"

	"github.com/Pense-Laranja/aspect/internal/spec"
)

func TestDecodeLenientRepairsModelTypos(t *testing.T) {
	raw := `{
	  "package": "modules/events",
	  "intent": "events",
	  "goals": [{"label": "g", "statement": "s", "verify": "test"}],
	  "entities": [{"name": "Event", "intent": "an event", "fields": [
	     {"name": "ID", "type": "int", "key": true, "required": "yes", "unique": false},
	     {"name": "Slug", "type": "string", "key": false, "unique": "true", "default": 0}
	  ], "relations": [], "constraints": "slugs are unique"}],
	  "interfaces": [{"name": "web", "kind": "web", "intent": "pages", "surfaces": [
	     {"name": "list", "route": "/events", "operations": "list", "errors": ["404"], "auth": null}
	  ]}],
	  "operations": [{"name": "Publish", "signature": "Publish() -> void", "pre": "the event has a date"}],
	  "invariants": ["never overlap"],
	  "scenarios": [{"id": 1, "when": "publish", "then": "visible", "given": null}],
	  "constraints": [],
	  "notes": "",
	  "unexpected": {"ignored": true}
	}`
	var f Fragment
	if err := decodeLenient([]byte(raw), &f); err != nil {
		t.Fatal(err)
	}
	e := f.Entities[0]
	if e.Fields[0].Key != "primary" || !e.Fields[0].Required || e.Fields[1].Key != "" || !e.Fields[1].Unique || e.Fields[1].Default != "0" {
		t.Fatalf("fields = %+v", e.Fields)
	}
	if len(e.Constraints) != 1 || e.Constraints[0] != "slugs are unique" {
		t.Fatalf("constraints = %v", e.Constraints)
	}
	if got := f.Interfaces[0].Surfaces[0].Operations; len(got) != 1 || got[0] != "list" {
		t.Fatalf("operations = %v", got)
	}
	if f.Operations[0].Pre[0] != "the event has a date" || f.Scenarios[0].ID != "1" {
		t.Fatalf("ops = %+v scenarios = %+v", f.Operations, f.Scenarios)
	}
}

func TestDecodeLenientKeepsGoodAnswersAndPointsAtBadOnes(t *testing.T) {
	var v Verdict
	if err := decodeLenient([]byte(`{"module":"m","goals":[{"id":"G1","status":"achieved","evidence":"e","gaps":[],"confidence":"90%"}],"intent_status":"aligned","intent_rationale":"r","scenarios":[],"recommendations":[]}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.Goals[0].Confidence != 0.9 {
		t.Fatalf("confidence = %v", v.Goals[0].Confidence)
	}
	var m spec.Module
	err := decodeLenient([]byte(strings.Repeat(" ", 500)+`{"name": "m", "intent": "i", "scenarios": [{"id": "S1", "when": "w", "then": "t"}], "goals": [`), &m)
	if err == nil || !strings.Contains(err.Error(), "around byte") {
		t.Fatalf("a syntax error must be located: %v", err)
	}
}
