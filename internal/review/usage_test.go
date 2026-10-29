package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOutcomeLineFormatting(t *testing.T) {
	cases := []struct {
		name    string
		outcome string
		usage   Usage
		reused  bool
		want    string
	}{
		{
			name:    "reused run always shows zero cost and reused marker",
			outcome: "PASS",
			usage:   Usage{CostUSD: 0.1234, PromptTokens: 5000, CompletionTokens: 1000},
			reused:  true,
			want:    "Aspect gate outcome: PASS — Cost: $0.0000 (reused)",
		},
		{
			name:    "reused run with blocking verdict",
			outcome: "BLOCK",
			usage:   Usage{CostUSD: 0.50},
			reused:  true,
			want:    "Aspect gate outcome: BLOCK — Cost: $0.0000 (reused)",
		},
		{
			name:    "fresh run with zero usage produces plain outcome line",
			outcome: "PASS",
			usage:   Usage{},
			reused:  false,
			want:    "Aspect gate outcome: PASS",
		},
		{
			name:    "fresh run with prompt and completion",
			outcome: "PASS",
			usage: Usage{
				CostUSD:          0.0250,
				PromptTokens:     4500,
				CompletionTokens: 1200,
				TotalTokens:      5700,
			},
			reused: false,
			want:   "Aspect gate outcome: PASS — Cost: $0.0250 (4.5k prompt, 1.2k completion)",
		},
		{
			name:    "fresh run with cached tokens discount",
			outcome: "PASS",
			usage: Usage{
				CostUSD:          0.0595,
				PromptTokens:     4500,
				CompletionTokens: 1200,
				CachedTokens:     2100,
				TotalTokens:      7800,
			},
			reused: false,
			want:   "Aspect gate outcome: PASS — Cost: $0.0595 (4.5k prompt, 1.2k completion, 2.1k cached [75% discount])",
		},
		{
			name:    "fresh run with duration included",
			outcome: "BLOCK",
			usage: Usage{
				CostUSD:          0.0595,
				PromptTokens:     4500,
				CompletionTokens: 1200,
				CachedTokens:     2100,
				TotalTokens:      7800,
				Duration:         87 * time.Second,
			},
			reused: false,
			want:   "Aspect gate outcome: BLOCK — Cost: $0.0595 (4.5k prompt, 1.2k completion, 2.1k cached [75% discount], ~1m27s)",
		},
		{
			name:    "fresh run with sub-second duration",
			outcome: "WARN",
			usage: Usage{
				CostUSD:          0.0010,
				PromptTokens:     500,
				CompletionTokens: 100,
				TotalTokens:      600,
				Duration:         500 * time.Millisecond,
			},
			reused: false,
			want:   "Aspect gate outcome: WARN — Cost: $0.0010 (500 prompt, 100 completion, <1s)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatOutcomeLine(tc.outcome, tc.usage, tc.reused)
			if got != tc.want {
				t.Errorf("FormatOutcomeLine() =\n%q\nwant:\n%q", got, tc.want)
			}
		})
	}
}

func TestRunPopulatesUsage(t *testing.T) {
	repo := testRepo(t)
	te := newTestEngine(t, repo, map[string]any{
		"security-reviewer": clean,
		"red-team":          clean,
	})

	res := te.run(t, Options{})
	if res.Outcome != OutcomePass {
		t.Fatalf("run failed: %v", res.Outcome)
	}

	// 2 agents * (100 prompt + 10 completion)
	if res.Usage.PromptTokens != 200 {
		t.Errorf("PromptTokens = %d, want 200", res.Usage.PromptTokens)
	}
	if res.Usage.CompletionTokens != 20 {
		t.Errorf("CompletionTokens = %d, want 20", res.Usage.CompletionTokens)
	}
	if res.Usage.CachedTokens != 160 {
		t.Errorf("CachedTokens = %d, want 160", res.Usage.CachedTokens)
	}
	if res.Usage.TotalTokens != 220 {
		t.Errorf("TotalTokens = %d, want 220", res.Usage.TotalTokens)
	}

	// Verify rendered footer contains cost breakdown
	if !strings.Contains(res.Rendered, "Cost: $") {
		t.Errorf("res.Rendered missing cost footer:\n%s", res.Rendered)
	}
	if !strings.Contains(res.Rendered, "prompt") || !strings.Contains(res.Rendered, "completion") {
		t.Errorf("res.Rendered missing token breakdown:\n%s", res.Rendered)
	}

	// Verify status.json persisted usage
	stBytes, err := os.ReadFile(filepath.Join(res.RunDir, "status.json"))
	if err != nil {
		t.Fatalf("reading status.json: %v", err)
	}
	var st map[string]any
	if err := json.Unmarshal(stBytes, &st); err != nil {
		t.Fatalf("unmarshaling status.json: %v", err)
	}
	if st["total_tokens"] != float64(220) {
		t.Errorf("status.json total_tokens = %v, want 220", st["total_tokens"])
	}
	us, ok := st["usage"].(map[string]any)
	if !ok {
		t.Fatalf("status.json missing usage object: %+v", st)
	}
	if us["prompt_tokens"] != float64(200) || us["completion_tokens"] != float64(20) || us["cached_tokens"] != float64(160) {
		t.Errorf("status.json usage breakdown unexpected: %+v", us)
	}
}

func TestPrintRunsDisplaysCostAndTokens(t *testing.T) {
	runs := []*RunSummary{
		{
			RunID:         "local-20261028T120000Z",
			PR:            "101",
			State:         "completed",
			Outcome:       "PASS",
			TotalBlockers: 0,
			TotalFindings: 0,
			Engine:        "flash",
			Usage: Usage{
				CostUSD:     0.0595,
				TotalTokens: 7800,
			},
		},
		{
			RunID:         "local-20261028T110000Z",
			State:         "completed",
			Outcome:       "BLOCK",
			TotalBlockers: 2,
			TotalFindings: 3,
			Engine:        "pro",
			TotalCostUSD:  0.1500,
			TotalTokens:   12500,
		},
		{
			RunID:   "local-20261028T100000Z",
			State:   "completed",
			Outcome: "PASS",
			Engine:  "flash",
		},
	}

	var b strings.Builder
	PrintRuns(&b, runs)
	out := b.String()

	// Verify header has COST and TOKENS
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 4 {
		t.Fatalf("PrintRuns output too short:\n%s", out)
	}
	header := lines[0]
	if !strings.Contains(header, "COST") || !strings.Contains(header, "TOKENS") {
		t.Errorf("PrintRuns header missing COST or TOKENS:\n%s", header)
	}

	// Verify Run 1 displays $0.0595 and 8k tokens
	if !strings.Contains(lines[1], "$0.0595") || !strings.Contains(lines[1], "8k") {
		t.Errorf("Run 1 row unexpected:\n%s", lines[1])
	}

	// Verify Run 2 uses fallback TotalCostUSD and TotalTokens
	if !strings.Contains(lines[2], "$0.1500") || !strings.Contains(lines[2], "13k") {
		t.Errorf("Run 2 row unexpected:\n%s", lines[2])
	}

	// Verify Run 3 with 0 cost displays "-"
	if !strings.Contains(lines[3], "-\t-") && !strings.Contains(lines[3], "-  -") {
		fields := strings.Fields(lines[3])
		// Check that "-" is present for cost and tokens
		foundHyphens := 0
		for _, f := range fields {
			if f == "-" {
				foundHyphens++
			}
		}
		if foundHyphens < 3 { // PR, COST, TOKENS
			t.Errorf("Run 3 row expected '-' for cost and tokens, got:\n%s", lines[3])
		}
	}
}

func TestPrintStatusDisplaysTokenBreakdown(t *testing.T) {
	t.Run("status with full usage breakdown", func(t *testing.T) {
		r := &RunSummary{
			RunID:           "local-20261028T120000Z",
			State:           "completed",
			Outcome:         "PASS",
			CompletedAgents: 2,
			TotalAgents:     2,
			Base:            "main",
			Head:            "feature",
			Engine:          "flash",
			Summary:         "Review gates passed.",
			Usage: Usage{
				CostUSD:          0.0595,
				PromptTokens:     4500,
				CompletionTokens: 1200,
				CachedTokens:     2100,
				TotalTokens:      7800,
			},
		}

		var b strings.Builder
		PrintStatus(&b, r)
		out := b.String()

		if !strings.Contains(out, "$0.0595") {
			t.Errorf("PrintStatus output missing cost:\n%s", out)
		}
		if !strings.Contains(out, "4.5k prompt") {
			t.Errorf("PrintStatus output missing prompt tokens:\n%s", out)
		}
		if !strings.Contains(out, "1.2k completion") {
			t.Errorf("PrintStatus output missing completion tokens:\n%s", out)
		}
		if !strings.Contains(out, "2.1k cached") {
			t.Errorf("PrintStatus output missing cached tokens:\n%s", out)
		}
	})

	t.Run("status without cached tokens", func(t *testing.T) {
		r := &RunSummary{
			RunID:           "local-20261028T120000Z",
			State:           "completed",
			Outcome:         "PASS",
			CompletedAgents: 2,
			TotalAgents:     2,
			Base:            "main",
			Head:            "feature",
			Engine:          "flash",
			Summary:         "Review gates passed.",
			Usage: Usage{
				CostUSD:          0.02,
				PromptTokens:     1000,
				CompletionTokens: 500,
				TotalTokens:      1500,
			},
		}

		var b strings.Builder
		PrintStatus(&b, r)
		out := b.String()

		if strings.Contains(out, "cached") {
			t.Errorf("PrintStatus should not display cached when CachedTokens=0:\n%s", out)
		}
		if !strings.Contains(out, "1k prompt") || !strings.Contains(out, "500 completion") {
			t.Errorf("PrintStatus missing prompt/completion tokens:\n%s", out)
		}
	})

	t.Run("legacy status with total tokens but no token breakdown", func(t *testing.T) {
		r := &RunSummary{
			RunID:           "local-20261028T120000Z",
			State:           "completed",
			Outcome:         "PASS",
			CompletedAgents: 2,
			TotalAgents:     2,
			Base:            "main",
			Head:            "feature",
			Engine:          "flash",
			Summary:         "Review gates passed.",
			TotalTokens:     5000,
			Usage:           Usage{},
		}

		var b strings.Builder
		PrintStatus(&b, r)
		out := b.String()

		if !strings.Contains(out, " (5k tokens)") {
			t.Errorf("PrintStatus output missing ' (5k tokens)':\n%s", out)
		}
	})
}

func TestReusedAndCarriedUsageReporting(t *testing.T) {
	repo := testRepo(t)
	te := newTestEngine(t, repo, map[string]any{
		"security-reviewer": clean,
		"red-team":          clean,
	})

	first := te.run(t, Options{})
	if first.Usage.TotalTokens == 0 {
		t.Fatal("first run usage was not populated")
	}

	// Second run with unchanged diff: reused
	second := te.run(t, Options{})
	if !second.Cached {
		t.Fatal("second run was not cached")
	}
	if second.Usage.TotalTokens != first.Usage.TotalTokens {
		t.Errorf("second.Usage.TotalTokens = %d, want %d", second.Usage.TotalTokens, first.Usage.TotalTokens)
	}
	if !strings.Contains(second.Rendered, "Cost: $0.0000 (reused)") {
		t.Errorf("second.Rendered missing reused cost footer:\n%s", second.Rendered)
	}

	line := FormatOutcomeLine(second.Outcome, second.Usage, second.Cached || second.CarriedFrom != "")
	wantLine := "Aspect gate outcome: PASS — Cost: $0.0000 (reused)"
	if line != wantLine {
		t.Errorf("FormatOutcomeLine for reused run = %q, want %q", line, wantLine)
	}
}

func TestUsageUnmarshalJSON(t *testing.T) {
	t.Run("unmarshals cost_usd", func(t *testing.T) {
		raw := `{"cost_usd": 0.045, "prompt_tokens": 100, "total_tokens": 150}`
		var u Usage
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			t.Fatal(err)
		}
		if u.CostUSD != 0.045 || u.PromptTokens != 100 || u.TotalTokens != 150 {
			t.Errorf("unmarshaled = %+v", u)
		}
	})

	t.Run("unmarshals total_cost_usd", func(t *testing.T) {
		raw := `{"total_cost_usd": 0.075, "prompt_tokens": 200, "total_tokens": 300}`
		var u Usage
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			t.Fatal(err)
		}
		if u.CostUSD != 0.075 || u.PromptTokens != 200 || u.TotalTokens != 300 {
			t.Errorf("unmarshaled = %+v", u)
		}
	})

	t.Run("malformed json returns error", func(t *testing.T) {
		raw := `{"cost_usd": "not-a-number"}`
		var u Usage
		if err := json.Unmarshal([]byte(raw), &u); err == nil {
			t.Fatalf("expected error unmarshaling malformed JSON, got nil")
		}
		if err := u.UnmarshalJSON([]byte(`{invalid`)); err == nil {
			t.Fatalf("expected error from direct UnmarshalJSON call, got nil")
		}
	})
}
