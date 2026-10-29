package main

import (
	"slices"
	"testing"

	"github.com/Pense-Laranja/aspect/internal/review"
)

func TestParseReadArgs(t *testing.T) {
	cases := []struct {
		sub      string
		args     []string
		runs     []string
		severity string
		err      bool
	}{
		{"findings", []string{"run-1", "-severity", "high"}, []string{"run-1"}, "high", false},
		{"findings", []string{"-severity", "high", "run-1"}, []string{"run-1"}, "high", false},
		{"findings", nil, nil, "", false},
		{"findings", []string{"a", "b"}, nil, "", true},
		{"findings", []string{"-severity", "hgih"}, nil, "", true},
		{"status", []string{"run-1", "-repo", "/r"}, []string{"run-1"}, "", false},
		{"compare", []string{"a", "b"}, []string{"a", "b"}, "", false},
		{"compare", []string{"only-one"}, nil, "", true},
		{"runs", []string{"-limit", "3"}, nil, "", false},
		{"runs", []string{"stray"}, nil, "", true},
	}
	for _, c := range cases {
		a, err := parseReadArgs(c.sub, c.args, ".")
		if (err != nil) != c.err {
			t.Errorf("%s %v: err = %v, want error %v", c.sub, c.args, err, c.err)
			continue
		}
		if c.err {
			continue
		}
		if !slices.Equal(a.runs, c.runs) || a.severity != c.severity {
			t.Errorf("%s %v: runs %v severity %q", c.sub, c.args, a.runs, a.severity)
		}
	}
	if a, _ := parseReadArgs("status", []string{"-repo", "/r"}, "."); a.repo != "/r" {
		t.Errorf("repo = %q", a.repo)
	}
	if a, _ := parseReadArgs("runs", []string{"-limit", "3"}, "."); a.limit != 3 {
		t.Errorf("limit = %d", a.limit)
	}
}

func TestOutcomeLineBannerIntegration(t *testing.T) {
	cases := []struct {
		outcome string
		usage   review.Usage
		reused  bool
		want    string
	}{
		{
			outcome: "PASS",
			usage:   review.Usage{CostUSD: 0.0595, PromptTokens: 4500, CompletionTokens: 1200, CachedTokens: 2100, TotalTokens: 7800},
			reused:  false,
			want:    "Aspect gate outcome: PASS — Cost: $0.0595 (4.5k prompt, 1.2k completion, 2.1k cached [75% discount])",
		},
		{
			outcome: "PASS",
			usage:   review.Usage{CostUSD: 0.0595, PromptTokens: 4500, CompletionTokens: 1200, CachedTokens: 2100, TotalTokens: 7800},
			reused:  true,
			want:    "Aspect gate outcome: PASS — Cost: $0.0000 (reused)",
		},
	}

	for _, tc := range cases {
		got := review.FormatOutcomeLine(tc.outcome, tc.usage, tc.reused)
		if got != tc.want {
			t.Errorf("FormatOutcomeLine() = %q, want %q", got, tc.want)
		}
	}
}
