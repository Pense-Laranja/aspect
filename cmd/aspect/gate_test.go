package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eideroliveira/aspect/internal/gate"
)

// A report whose model invented a category (error-handling) is invalid as
// printed; extracted with the run's agent, mode, base and head it is
// normalized as aspect review does and validates.
func TestGateExtractNormalizesWithTheRun(t *testing.T) {
	report := map[string]any{
		"schema": gate.ReportSchema, "agent": "quality-reviewer", "mode": "gate", "base": "aaaaaaa1", "head": "bbbbbbb2",
		"summary": "checked",
		"findings": []map[string]any{{
			"id": "q1", "severity": "low", "category": "error-handling", "title": "t", "evidence": "e",
			"recommendation": "r", "confidence": 0.7, "location": map[string]any{"file": "a.go", "line": 3},
		}},
		"handoffs": []any{}, "changes": []any{},
	}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	transcript := filepath.Join(dir, "t.txt")
	if err := os.WriteFile(transcript, []byte("Done.\n\n```json\n"+string(body)+"\n```\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	decode := func(out string) error {
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		r, err := gate.DecodeReport(b)
		if err != nil {
			return err
		}
		return r.Validate()
	}

	verbatim := filepath.Join(dir, "verbatim.json")
	if err := runGateExtract([]string{"-o", verbatim, transcript}); err != nil {
		t.Fatal(err)
	}
	if err := decode(verbatim); err == nil || !strings.Contains(err.Error(), "error-handling") {
		t.Fatalf("saved verbatim, the report must be invalid, got %v", err)
	}

	normalized := filepath.Join(dir, "normalized.json")
	if err := runGateExtract([]string{"-o", normalized, "-agent", "quality-reviewer", "-mode", "gate", "-base", "aaaaaaa1", "-head", "bbbbbbb2", transcript}); err != nil {
		t.Fatal(err)
	}
	if err := decode(normalized); err != nil {
		t.Fatalf("normalized, the report must validate: %v", err)
	}
	if b, _ := os.ReadFile(normalized); !strings.Contains(string(b), `"category": "correctness"`) {
		t.Errorf("error-handling maps to correctness:\n%s", b)
	}

	if err := runGateExtract([]string{"-o", normalized, "-agent", "quality-reviewer", transcript}); err == nil || !strings.Contains(err.Error(), "go together") {
		t.Errorf("a partial run must be refused, got %v", err)
	}
}
