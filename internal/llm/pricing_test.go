package llm

import (
	"testing"
	"time"
)

var nov2026 = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func TestLookupGeminiPrice(t *testing.T) {
	cases := []struct {
		model    string
		at       time.Time
		prompt   int64
		pricedAs string
		input    float64
	}{
		// The alias is priced as what it served when the table was checked.
		{"gemini-flash-latest", nov2026, 100_000, "gemini-3.8-flash", 0.75},
		{"models/gemini-3.8-flash", nov2026, 100_000, "gemini-3.8-flash", 0.75},
		{"gemini-3.8-flash-001", nov2026, 100_000, "gemini-3.8-flash", 0.75},
		// The price doubles on 2027-01-01.
		{"gemini-3.8-flash", time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC), 1, "gemini-3.8-flash", 0.75},
		{"gemini-3.8-flash", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), 1, "gemini-3.8-flash", 1.50},
		// The longest model id wins.
		{"gemini-2.5-flash-lite-001", nov2026, 1, "gemini-2.5-flash-lite", 0.10},
		{"gemini-2.5-flash", nov2026, 1, "gemini-2.5-flash", 0.30},
		// Long-context tier.
		{"gemini-3.1-pro-preview", nov2026, 200_000, "gemini-3.1-pro-preview", 2.00},
		{"gemini-3.1-pro-preview", nov2026, 200_001, "gemini-3.1-pro-preview", 4.00},
	}
	for _, c := range cases {
		p, as, ok := LookupGeminiPrice(c.model, c.at, c.prompt)
		if !ok || as != c.pricedAs || p.Input != c.input {
			t.Errorf("LookupGeminiPrice(%q, %s, %d) = %+v, %q, %v; want input %v as %q", c.model, c.at.Format(time.DateOnly), c.prompt, p, as, ok, c.input, c.pricedAs)
		}
	}
	if _, _, ok := LookupGeminiPrice("gemini-9-ultra", nov2026, 1); ok {
		t.Error("an unknown model was priced")
	}
	if _, _, ok := LookupGeminiPrice("gemini-3.8-flashy", nov2026, 1); ok {
		t.Error("a model id that only starts with a known one was priced")
	}
}

func TestGeminiPriceCost(t *testing.T) {
	flash, _, _ := LookupGeminiPrice("gemini-3.8-flash", nov2026, 0)
	cases := []struct {
		name                                 string
		prompt, cached, completion, thoughts int64
		want                                 float64
	}{
		// adversarial-reviewer in a gordpress run of 2026-11: 68,977 prompt,
		// 70 completion, 83,262 total, so 14,215 thinking. The engine recorded
		// $0.005194 (no thinking, a stale $0.075 input rate).
		{"thinking billed as output", 68_977, 0, 70, 14_215, 0.105302},
		{"cached at the cached rate", 100_000, 90_000, 0, 0, 0.01425},
		{"uncached", 1_000_000, 0, 0, 0, 0.75},
		{"output and thinking", 0, 0, 500_000, 500_000, 3.75},
		{"negative cached clamped to 0", 100_000, -5, 0, 0, 0.075},
		{"cached over the prompt clamped", 100_000, 200_000, 0, 0, 0.0075},
	}
	for _, c := range cases {
		if got := flash.Cost(c.prompt, c.cached, c.completion, c.thoughts); got != c.want {
			t.Errorf("%s: Cost(%d, %d, %d, %d) = %v, want %v", c.name, c.prompt, c.cached, c.completion, c.thoughts, got, c.want)
		}
	}
}

func TestGeminiPriceCacheCost(t *testing.T) {
	flash, _, _ := LookupGeminiPrice("gemini-3.8-flash", nov2026, 0)
	// Written once at $0.75/M, stored half an hour at $0.50/M/h.
	if got := flash.CacheCost(100_000, 30*time.Minute); got != 0.1 {
		t.Errorf("CacheCost = %v, want 0.1", got)
	}
	if got := flash.CacheCost(100_000, -time.Minute); got != 0.075 {
		t.Errorf("CacheCost with a negative lifetime = %v, want 0.075", got)
	}
}
