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
	b, err := os.ReadFile(normalized)
	if err != nil {
		t.Fatal(err)
	}
	r, err := gate.DecodeReport(b)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Findings[0].Category; got != "correctness" {
		t.Errorf("error-handling maps to correctness, got %q", got)
	}

	if err := runGateExtract([]string{"-o", normalized, "-agent", "quality-reviewer", transcript}); err == nil || !strings.Contains(err.Error(), "go together") {
		t.Errorf("a partial run must be refused, got %v", err)
	}
	if err := runGateExtract([]string{"-o", normalized, "-agent", "quality-reviewer", "-mode", "gates", "-base", "aaaaaaa1", "-head", "bbbbbbb2", transcript}); err == nil || !strings.Contains(err.Error(), "-mode must be one of") {
		t.Errorf("a mistyped mode must be refused, got %v", err)
	}
}

// The run's values stand in for those a report leaves out; a report that
// names another revision or an unknown severity is refused, names the
// agent, and writes nothing.
func TestGateExtractTakesTheRunAndRefusesWhatItCannotTrust(t *testing.T) {
	dir := t.TempDir()
	extract := func(report string) (string, error) {
		t.Helper()
		transcript := filepath.Join(dir, "t.txt")
		if err := os.WriteFile(transcript, []byte("```json\n"+report+"\n```\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(dir, "r.json")
		os.Remove(out)
		err := runGateExtract([]string{"-o", out, "-agent", "security-reviewer", "-mode", "gate", "-base", "aaaaaaa1", "-head", "bbbbbbb2", transcript})
		return out, err
	}
	out, err := extract(`{"summary": "ok", "findings": []}`)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	r, err := gate.DecodeReport(b)
	if err != nil {
		t.Fatal(err)
	}
	if r.Agent != "security-reviewer" || r.Mode != "gate" || r.Base != "aaaaaaa1" || r.Head != "bbbbbbb2" {
		t.Errorf("the run's values are stamped: %+v", r)
	}
	for name, report := range map[string]string{
		"stale":   `{"head": "ccccccc3", "findings": []}`,
		"unknown": `{"findings": [{"severity": "weird", "title": "SQL injection", "location": {"file": "a.go", "line": 3}}]}`,
	} {
		out, err := extract(report)
		if err == nil || !strings.Contains(err.Error(), "security-reviewer's report") {
			t.Errorf("%s: want a refusal naming the agent, got %v", name, err)
		}
		if _, statErr := os.Stat(out); statErr == nil {
			t.Errorf("%s: a refused report must not be written", name)
		}
		if kept, readErr := os.ReadFile(out + ".rejected"); readErr != nil || !strings.Contains(string(kept), "findings") {
			t.Errorf("%s: the refused report is kept for inspection: %v", name, readErr)
		}
		os.Remove(out + ".rejected")
	}
}
