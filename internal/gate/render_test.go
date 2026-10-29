package gate

import (
	"strings"
	"testing"
	"time"
)

func TestFormatTokens(t *testing.T) {
	cases := []struct {
		input int64
		want  string
	}{
		{-10, "0"},
		{0, "0"},
		{1, "1"},
		{123, "123"},
		{999, "999"},
		{1000, "1k"},
		{1049, "1k"},
		{1050, "1.1k"},
		{4500, "4.5k"},
		{9999, "10k"},
		{10000, "10k"},
		{12499, "12k"},
		{12500, "13k"},
		{789000, "789k"},
		{999499, "999k"},
		{1000000, "1M"},
		{1200000, "1.2M"},
		{25400000, "25.4M"},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := FormatTokens(tc.input)
			if got != tc.want {
				t.Errorf("FormatTokens(%d) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestFormatCost(t *testing.T) {
	cases := []struct {
		input float64
		want  string
	}{
		{-1.5, "$0.0000"},
		{-0.0001, "$0.0000"},
		{0.0, "$0.0000"},
		{0.0595, "$0.0595"},
		{0.05954, "$0.0595"},
		{1.23456, "$1.2346"},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := FormatCost(tc.input)
			if got != tc.want {
				t.Errorf("FormatCost(%f) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		input time.Duration
		want  string
	}{
		{-5 * time.Second, "~0s"},
		{0, "~0s"},
		{500 * time.Millisecond, "<1s"},
		{999 * time.Millisecond, "<1s"},
		{1 * time.Second, "~1s"},
		{12 * time.Second, "~12s"},
		{87 * time.Second, "~1m27s"},
		{125 * time.Second, "~2m5s"},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := FormatDuration(tc.input)
			if got != tc.want {
				t.Errorf("FormatDuration(%v) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestRender_NilUsagePreservesBaseline(t *testing.T) {
	c := &Config{Spec: "spec.yaml"}
	p := &Plan{Base: "main", Head: "feature", ChangedFiles: 1, ChangedLines: 5}
	r := &Result{
		Outcome: Pass,
		Agents: []AgentResult{
			{Agent: "quality-reviewer", Mode: "gate", Status: StatusOK, Summary: "All good."},
		},
	}

	withoutOpt := Render(c, p, r, RenderOptions{})
	withNil := Render(c, p, r, RenderOptions{Usage: nil})

	if withoutOpt != withNil {
		t.Errorf("nil Usage changed render output:\nwithout: %q\nwith nil: %q", withoutOpt, withNil)
	}
	if strings.Contains(withoutOpt, "Cost:") {
		t.Errorf("baseline render contains Cost footer:\n%s", withoutOpt)
	}
}

func TestRender_UsageFooterAppended(t *testing.T) {
	c := &Config{Spec: "spec.yaml"}
	p := &Plan{Base: "main", Head: "feature", ChangedFiles: 1, ChangedLines: 5}
	r := &Result{
		Outcome: Pass,
		Agents: []AgentResult{
			{Agent: "quality-reviewer", Mode: "gate", Status: StatusOK, Summary: "All good."},
		},
	}

	t.Run("fresh run with duration and cache", func(t *testing.T) {
		opt := RenderOptions{
			Usage: &UsageSummary{
				CostUSD:          0.0595,
				PromptTokens:     4500,
				CompletionTokens: 1200,
				CachedTokens:     2100,
				TotalTokens:      7800,
				Duration:         87 * time.Second,
			},
		}
		rendered := Render(c, p, r, opt)
		wantFooter := "\n---\nCost: $0.0595 (4.5k prompt, 1.2k completion, 2.1k cached [75% discount], ~1m27s)\n"
		if !strings.HasSuffix(rendered, wantFooter) {
			t.Errorf("rendered does not end with expected footer:\nGot:\n%s\nWant footer:\n%s", rendered, wantFooter)
		}
	})

	t.Run("fresh run without cache and without duration", func(t *testing.T) {
		opt := RenderOptions{
			Usage: &UsageSummary{
				CostUSD:          0.02,
				PromptTokens:     1000,
				CompletionTokens: 500,
				TotalTokens:      1500,
			},
		}
		rendered := Render(c, p, r, opt)
		wantFooter := "\n---\nCost: $0.0200 (1k prompt, 500 completion)\n"
		if !strings.HasSuffix(rendered, wantFooter) {
			t.Errorf("rendered does not end with expected footer:\nGot:\n%s\nWant footer:\n%s", rendered, wantFooter)
		}
	})

	t.Run("reused run", func(t *testing.T) {
		opt := RenderOptions{
			Usage: &UsageSummary{
				Reused: true,
			},
		}
		rendered := Render(c, p, r, opt)
		wantFooter := "\n---\nCost: $0.0000 (reused)\n"
		if !strings.HasSuffix(rendered, wantFooter) {
			t.Errorf("rendered does not end with expected footer:\nGot:\n%s\nWant footer:\n%s", rendered, wantFooter)
		}
	})

	t.Run("carried from run", func(t *testing.T) {
		opt := RenderOptions{
			Usage: &UsageSummary{
				CarriedFrom: "local-20261028T120000Z",
			},
		}
		rendered := Render(c, p, r, opt)
		wantFooter := "\n---\nCost: $0.0000 (reused)\n"
		if !strings.HasSuffix(rendered, wantFooter) {
			t.Errorf("rendered does not end with expected footer:\nGot:\n%s\nWant footer:\n%s", rendered, wantFooter)
		}
	})

	t.Run("zero usage non-reused renders no footer", func(t *testing.T) {
		opt := RenderOptions{
			Usage: &UsageSummary{},
		}
		rendered := Render(c, p, r, opt)
		if strings.Contains(rendered, "Cost:") {
			t.Errorf("expected no footer for zero usage, got:\n%s", rendered)
		}
	})

	t.Run("no gates apply with usage footer", func(t *testing.T) {
		rNoAgents := &Result{
			Outcome: Pass,
			Agents:  nil,
		}
		opt := RenderOptions{
			Usage: &UsageSummary{
				CostUSD:          0.01,
				PromptTokens:     80,
				CompletionTokens: 20,
				TotalTokens:      100,
			},
		}
		rendered := Render(c, p, rNoAgents, opt)
		if !strings.Contains(rendered, "No gate applies to this change.") {
			t.Errorf("expected 'No gate applies to this change.' in output:\n%s", rendered)
		}
		wantFooter := "\n---\nCost: $0.0100 (80 prompt, 20 completion)\n"
		if !strings.HasSuffix(rendered, wantFooter) {
			t.Errorf("rendered does not end with expected footer:\nGot:\n%s\nWant footer:\n%s", rendered, wantFooter)
		}
	})
}
