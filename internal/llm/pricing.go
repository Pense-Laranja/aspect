package llm

import (
	"math"
	"sort"
	"strings"
	"time"
)

// GeminiPricingSource is where the Gemini price table below comes from, and
// GeminiPricingChecked the day it was last compared with that page. Update
// both whenever a row changes.
const (
	GeminiPricingSource  = "https://ai.google.dev/gemini-api/docs/pricing"
	GeminiPricingChecked = "2026-11-17"
)

// GeminiPrice is a Gemini model's paid-tier Standard price for text, in USD
// per million tokens (StoragePerHour: per million cached tokens per hour).
// Thinking tokens are billed as output. A zero StoragePerHour means the
// model offers no explicit context cache.
type GeminiPrice struct {
	Input, Output, Cached float64
	StoragePerHour        float64
}

// geminiPriceRow is one row of the table: the price of Model (an exact
// model id, or the prefix of a dated version such as "-001") from From
// (inclusive) until Until (exclusive), for prompts of at most MaxPrompt
// tokens (zero: any size).
type geminiPriceRow struct {
	Model       string
	From, Until string // "2006-01-02", UTC; "" is open-ended
	MaxPrompt   int64
	GeminiPrice
}

// geminiPrices is the price table, checked on GeminiPricingSource on
// GeminiPricingChecked. Rows of one model are ordered by MaxPrompt, smallest
// first, with the open-ended size last.
var geminiPrices = []geminiPriceRow{
	{Model: "gemini-4-carbon", Until: "2026-12-01", GeminiPrice: GeminiPrice{Input: 2.00, Output: 10.00, Cached: 0.10}},
	{Model: "gemini-4-carbon", From: "2026-12-01", GeminiPrice: GeminiPrice{Input: 4.00, Output: 20.00, Cached: 0.20}},
	{Model: "gemini-4-argon", Until: "2026-12-01", GeminiPrice: GeminiPrice{Input: 2.00, Output: 10.00, Cached: 0.10}},
	{Model: "gemini-4-argon", From: "2026-12-01", GeminiPrice: GeminiPrice{Input: 4.00, Output: 20.00, Cached: 0.20}},
	{Model: "gemini-4-hydrogen", GeminiPrice: GeminiPrice{Input: 1.50, Output: 7.50, Cached: 0.075}},
	{Model: "gemini-3.8-flash", Until: "2027-01-01", GeminiPrice: GeminiPrice{Input: 0.75, Output: 3.75, Cached: 0.075, StoragePerHour: 0.50}},
	{Model: "gemini-3.8-flash", From: "2027-01-01", GeminiPrice: GeminiPrice{Input: 1.50, Output: 7.50, Cached: 0.15, StoragePerHour: 1.00}},
	{Model: "gemini-3.6-flash", Until: "2027-01-01", GeminiPrice: GeminiPrice{Input: 0.75, Output: 3.75, Cached: 0.075, StoragePerHour: 0.50}},
	{Model: "gemini-3.6-flash", From: "2027-01-01", GeminiPrice: GeminiPrice{Input: 1.50, Output: 7.50, Cached: 0.15, StoragePerHour: 1.00}},
	{Model: "gemini-3.5-flash-lite", GeminiPrice: GeminiPrice{Input: 0.30, Output: 2.50, Cached: 0.03, StoragePerHour: 1.00}},
	{Model: "gemini-3.1-flash-lite", GeminiPrice: GeminiPrice{Input: 0.25, Output: 1.50, Cached: 0.025, StoragePerHour: 1.00}},
	{Model: "gemini-3.1-pro-preview", MaxPrompt: 200_000, GeminiPrice: GeminiPrice{Input: 2.00, Output: 12.00, Cached: 0.20, StoragePerHour: 4.50}},
	{Model: "gemini-3.1-pro-preview", GeminiPrice: GeminiPrice{Input: 4.00, Output: 18.00, Cached: 0.40, StoragePerHour: 4.50}},
	{Model: "gemini-3-flash-preview", GeminiPrice: GeminiPrice{Input: 0.50, Output: 3.00, Cached: 0.05, StoragePerHour: 1.00}},
	{Model: "gemini-2.5-pro", MaxPrompt: 200_000, GeminiPrice: GeminiPrice{Input: 1.25, Output: 10.00, Cached: 0.125, StoragePerHour: 4.50}},
	{Model: "gemini-2.5-pro", GeminiPrice: GeminiPrice{Input: 2.50, Output: 15.00, Cached: 0.25, StoragePerHour: 4.50}},
	{Model: "gemini-2.5-flash-lite", GeminiPrice: GeminiPrice{Input: 0.10, Output: 0.40, Cached: 0.01, StoragePerHour: 1.00}},
	{Model: "gemini-2.5-flash", GeminiPrice: GeminiPrice{Input: 0.30, Output: 2.50, Cached: 0.03, StoragePerHour: 1.00}},
}

// geminiAliases are what the API's aliases served when the table was
// checked (a request's modelVersion). They price a call only when the
// response did not say which model answered it.
var geminiAliases = map[string]string{
	"gemini-flash-latest":      "gemini-3.8-flash",
	"gemini-flash-lite-latest": "gemini-3.5-flash-lite",
	"gemini-pro-latest":        "gemini-3.1-pro-preview",
}

// LookupGeminiPrice is the price of model at time at for a prompt of
// prompt tokens, and the table's model id it was priced as. model is the
// model that answered (a response's modelVersion), or an alias, which is
// resolved through geminiAliases. ok is false for a model the table does
// not know.
func LookupGeminiPrice(model string, at time.Time, prompt int64) (price GeminiPrice, pricedAs string, ok bool) {
	m := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "models/")
	if a, isAlias := geminiAliases[m]; isAlias {
		m = a
	}
	// The longest matching model id wins: "gemini-2.5-flash-lite-001" is
	// Flash-Lite, not Flash.
	best := ""
	for _, r := range geminiPrices {
		if (m == r.Model || strings.HasPrefix(m, r.Model+"-")) && len(r.Model) > len(best) {
			best = r.Model
		}
	}
	if best == "" {
		return GeminiPrice{}, "", false
	}
	day := at.UTC().Format("2006-01-02")
	rows := make([]geminiPriceRow, 0, 4)
	for _, r := range geminiPrices {
		if r.Model == best && (r.From == "" || day >= r.From) && (r.Until == "" || day < r.Until) {
			rows = append(rows, r)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].MaxPrompt, rows[j].MaxPrompt
		return a != 0 && (b == 0 || a < b)
	})
	for _, r := range rows {
		if r.MaxPrompt == 0 || prompt <= r.MaxPrompt {
			return r.GeminiPrice, best, true
		}
	}
	return GeminiPrice{}, "", false
}

// Cost is what one call cost in USD: the uncached prompt at the input
// rate, the cached part of it at the cached rate, and the answer and the
// thinking at the output rate. cached is clamped to the prompt.
func (p GeminiPrice) Cost(prompt, cached, completion, thoughts int64) float64 {
	cached = max(0, min(cached, prompt))
	usd := float64(prompt-cached)*p.Input + float64(cached)*p.Cached + float64(max(0, completion)+max(0, thoughts))*p.Output
	return round6(usd / 1_000_000)
}

// CacheCost is what an explicit context cache of tokens cost: writing it,
// billed as input, and keeping it for d.
func (p GeminiPrice) CacheCost(tokens int64, d time.Duration) float64 {
	usd := float64(tokens)*p.Input + float64(tokens)*p.StoragePerHour*max(0, d.Hours())
	return round6(usd / 1_000_000)
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }
