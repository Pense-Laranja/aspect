package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Pense-Laranja/aspect/internal/llm"
)

// The prompt layout, the same for every reviewer:
//
//	system: sharedSystemPrompt (identical for all reviewers)
//	user:   PR context and changed-file overview   (shared by the run)
//	        what the spec says about the modules    (shared by reviewers
//	        the reviewer's files belong to           with the same file scope)
//	        the diff, or one part of it             (likewise)
//	        this reviewer's context, role instructions and output format
//
// Everything reviewers share comes first and the instructions come last,
// next to where the model starts writing, as long-context guidance
// recommends. A diff over the prompt budget is reviewed in parts.
//
// The system prompt and the shared part of the user turn (promptPrefix)
// are the same bytes for every reviewer with the same file scope. When two
// or more requests of a run share them, the run stores them once in an
// explicit Gemini context cache (contextCache) and each request sends only
// its own part (promptSuffix) as a second user turn after it: the model
// reads the same text in the same order, and the shared tokens are billed
// at the cached rate. Gemini's implicit cache was meant to do this alone,
// but it hit on 3% of the prompt tokens of gordpress's runs (2026-11).

const sharedSystemPrompt = `You are a senior code reviewer. The user message holds a code change ` +
	`followed by the role you review it in, and the exact output format.

You have no tools in this session: you cannot run commands, open files, run tests or write ` +
	`files. The diff, and the specification excerpt when one is shown, are everything you can see. Where your role's instructions say to run git, ` +
	`read a file, run tests or write a report file, apply that step to the diff given instead, and ` +
	`return the report as your answer. Cite file paths and line numbers from the new (head) side of ` +
	`the diff: every added (+) or context ( ) line is prefixed with L<n>: giving its exact 1-based line ` +
	`number in the head file, and every removed (-) line is prefixed with OLD: (use the L<n>: number ` +
	`directly for location.line; never cite an OLD: line as a head line number). Every non-info finding's ` +
	`recommendation must include 3 concrete parts: (1) Code Fix (exact function, guard, or state transition ` +
	`at file:line), (2) Contract/Stamp/Lock Check (matching spec_ref, cache-key, stamp, or concurrency lock ` +
	`if applicable), and (3) Regression Test Anchor (exact Test... function in *_test.go). Report only ` +
	`issues the diff shows evidence for.`

// promptOverheadTokens is the fixed text around the parts, counted against
// the budget.
const promptOverheadTokens = 300

// request is one reviewer's review of a diff.
type request struct {
	agent         string
	instructions  string // the reviewer's role prompt
	outputFormat  string // the JSON format the answer must follow
	diff          string
	sharedContext string // PR title and description: the same for every reviewer
	specContext   string // what the spec says about the modules the reviewer's files belong to
	agentContext  string // this reviewer's assignment
}

// userPrompt is the whole user turn: the shared prefix, then this
// reviewer's part.
func userPrompt(req request, s *splitDiff, p diffPart) string {
	return promptPrefix(req, s, p) + "\n\n" + promptSuffix(req)
}

// promptPrefix is the start of the user turn, everything but the reviewer's
// own part: the PR, the change overview, the spec and the diff (or one part
// of it). It depends only on the change and the reviewer's file scope, so
// every reviewer of a run with the same scope gets it byte for byte, and
// sharedSystemPrompt plus this prefix is what the run's context cache
// holds (contextCache). Nothing that names or concerns one reviewer may go
// here; TestPromptPrefixIsSharedAcrossAgents holds it to that.
func promptPrefix(req request, s *splitDiff, p diffPart) string {
	var sections []string
	if req.sharedContext != "" {
		sections = append(sections, strings.TrimSpace(req.sharedContext))
	}
	sections = append(sections, "### Change overview\n"+s.overview())
	if req.specContext != "" {
		sections = append(sections, strings.TrimSpace(req.specContext))
	}
	heading := "### Code diff under review"
	if p.total > 1 {
		heading = fmt.Sprintf("### Code diff, %s\nThe diff is too large for one prompt and is reviewed in %d parts. "+
			"This part shows only the files below; the overview lists the rest. Do not report "+
			"something as missing only because it is not in this part.", p.label(), p.total)
	}
	sections = append(sections, heading+"\n```diff\n"+p.text+"\n```")
	return strings.Join(sections, "\n\n")
}

// promptSuffix is the reviewer's own part of the user turn, after the
// shared prefix and next to where the model starts writing: its
// assignment, its role instructions and the output format.
func promptSuffix(req request) string {
	var sections []string
	if req.agentContext != "" {
		sections = append(sections, strings.TrimSpace(req.agentContext))
	}
	sections = append(sections, "### Your role\n"+strings.TrimSpace(req.instructions))
	sections = append(sections, "### Output format\n"+strings.TrimSpace(req.outputFormat))
	return strings.Join(sections, "\n\n")
}

// diffBudget is what the prompt budget leaves for the diff.
func diffBudget(req request, s *splitDiff, promptBudget int) int {
	fixed := sharedSystemPrompt + req.sharedContext + s.overview() + req.specContext + req.agentContext + req.instructions + req.outputFormat
	return promptBudget - estimateTokens(fixed) - promptOverheadTokens
}

// planParts splits the diff into parts that each fit the prompt budget.
func planParts(req request, promptBudget int) (*splitDiff, []diffPart) {
	s := splitReviewable(req.diff)
	return s, chunkDiff(s, diffBudget(req, s, promptBudget))
}

func thinkingBudgetForPart(p diffPart) int {
	tokens := estimateTokens(p.text)
	switch {
	case tokens <= 1000:
		return 1024
	case tokens <= 4000:
		return 2048
	default:
		return 4096
	}
}

// Usage is what one review cost.
type Usage struct {
	Engine           string  `json:"engine,omitempty"`
	CostUSD          float64 `json:"cost_usd,omitempty"`
	PromptTokens     int64   `json:"prompt_tokens,omitempty"`
	CachedTokens     int64   `json:"cached_tokens,omitempty"`
	CompletionTokens int64   `json:"completion_tokens,omitempty"`
	// ThoughtsTokens are the model's thinking tokens: billed at the output
	// rate, counted in TotalTokens, not in CompletionTokens.
	ThoughtsTokens int64         `json:"thoughts_tokens,omitempty"`
	TotalTokens    int64         `json:"total_tokens,omitempty"`
	Duration       time.Duration `json:"duration,omitempty"`
	// Model is the model that answered (the API's modelVersion), PricedAs
	// the price table row it was billed by; they differ when the model was
	// unknown to the table.
	Model    string `json:"model,omitempty"`
	PricedAs string `json:"priced_as,omitempty"`
}

type usage = Usage

func (u *Usage) UnmarshalJSON(b []byte) error {
	type alias Usage
	var aux struct {
		alias
		TotalCostUSD float64 `json:"total_cost_usd"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	*u = Usage(aux.alias)
	if u.CostUSD == 0 && aux.TotalCostUSD != 0 {
		u.CostUSD = aux.TotalCostUSD
	}
	return nil
}

func (u *Usage) Add(o Usage) {
	if o.Engine != "" && !strings.Contains(u.Engine, o.Engine) {
		if u.Engine != "" {
			u.Engine += " + "
		}
		u.Engine += o.Engine
	}
	u.CostUSD += o.CostUSD
	u.PromptTokens += o.PromptTokens
	u.CachedTokens += o.CachedTokens
	u.CompletionTokens += o.CompletionTokens
	u.ThoughtsTokens += o.ThoughtsTokens
	u.TotalTokens += o.TotalTokens
	if o.Model != "" {
		u.Model = o.Model
	}
	if o.PricedAs != "" {
		u.PricedAs = o.PricedAs
	}
}

func (u *Usage) add(o Usage) {
	u.Add(o)
}

// mergeParts makes one report from per-part reports: lists joined, the
// parts' finding ids suffixed (they repeat across parts), summaries
// labelled.
func mergeParts(results []*Object) *Object {
	if len(results) == 1 {
		return results[0]
	}
	merged := NewObject()
	var summaries []string
	for i, res := range results {
		n := i + 1
		summaries = append(summaries, fmt.Sprintf("[part %d] %s", n, asText(res.Get("summary"))))
		for _, k := range res.Keys() {
			if k == "summary" {
				continue
			}
			v := res.Get(k)
			list, isList := v.([]any)
			if !isList {
				if !merged.Has(k) {
					merged.Set(k, v)
				}
				continue
			}
			if k == "findings" {
				renamed := make([]any, len(list))
				for j, e := range list {
					renamed[j] = e
					if f, ok := e.(*Object); ok && truthy(f.Get("id")) {
						c := NewObject()
						for _, fk := range f.Keys() {
							c.Set(fk, f.Get(fk))
						}
						c.Set("id", fmt.Sprintf("%s-p%d", asText(f.Get("id")), n))
						renamed[j] = c
					}
				}
				list = renamed
			}
			prev, _ := merged.Get(k).([]any)
			merged.Set(k, append(prev, list...))
		}
	}
	merged.Set("summary", fmt.Sprintf("Reviewed in %d parts. %s", len(results), strings.Join(summaries, " ")))
	return merged
}

// hooks report a review's progress to the run status.
type hooks struct {
	planned  func(parts int)       // the diff is reviewed in this many parts
	waiting  func()                // a request waits for a model slot
	started  func()                // a request holds a slot and starts
	partDone func(done, total int) // a part's answer arrived
}

// reviewer sends requests to the model, at most slots at once for the run.
type reviewer struct {
	client       llm.Client
	engine       string // "gemini (<model>)"
	model        string
	promptBudget int
	slots        chan struct{} // nil: no cap
	cache        *contextCache // nil: no explicit cache
	now          func() time.Time
}

// review runs one reviewer over its diff and returns the merged raw report.
// Any part failing fails the whole review: a gate must never pass on a
// partial review.
func (rv *reviewer) review(ctx context.Context, req request, h hooks) (*Object, usage, error) {
	s, parts := planParts(req, rv.promptBudget)
	if h.planned != nil {
		h.planned(len(parts))
	}
	if len(parts) == 1 {
		p := parts[0]
		if h.waiting != nil {
			h.waiting()
		}
		budget := thinkingBudgetForPart(p)
		res, u, err := rv.ask(ctx, promptPrefix(req, s, p), promptSuffix(req), &budget, h.started)
		if err != nil {
			return nil, u, err
		}
		if h.partDone != nil {
			h.partDone(1, 1)
		}
		return res, u, nil
	}

	pctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([]*Object, len(parts))
	usages := make([]usage, len(parts))
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
		hookMu   sync.Mutex
		done     int
	)
	started := func() {
		if h.started != nil {
			hookMu.Lock()
			h.started()
			hookMu.Unlock()
		}
	}
	if h.waiting != nil {
		for range parts {
			h.waiting()
		}
	}
	for i, p := range parts {
		wg.Add(1)
		go func(i int, p diffPart) {
			defer wg.Done()
			budget := thinkingBudgetForPart(p)
			res, u, err := rv.ask(pctx, promptPrefix(req, s, p), promptSuffix(req), &budget, started)
			if err != nil {
				once.Do(func() {
					firstErr = err
					cancel()
				})
				return
			}
			results[i] = res
			usages[i] = u
			if h.partDone != nil {
				hookMu.Lock()
				done++
				h.partDone(done, p.total)
				hookMu.Unlock()
			}
		}(i, p)
	}
	wg.Wait()
	var total usage
	for _, u := range usages {
		total.add(u)
	}
	if firstErr != nil {
		return nil, total, firstErr
	}
	return mergeParts(results), total, nil
}

// ask sends one prompt, prefix then suffix, once a slot is free. When the
// run's context cache holds sharedSystemPrompt and prefix, only suffix is
// sent, after it; a cache the API no longer has falls back to sending the
// whole prompt.
func (rv *reviewer) ask(ctx context.Context, prefix, suffix string, thinkingBudget *int, started func()) (*Object, usage, error) {
	cacheName := rv.cache.get(ctx, sharedSystemPrompt, prefix)
	if rv.slots != nil {
		select {
		case rv.slots <- struct{}{}:
		case <-ctx.Done():
			return nil, usage{}, ctx.Err()
		}
		defer func() { <-rv.slots }()
	}
	if started != nil {
		started()
	}
	whole := llm.Request{
		System:         sharedSystemPrompt,
		Prompt:         prefix + "\n\n" + suffix,
		JSON:           true,
		ThinkingBudget: thinkingBudget,
	}
	var resp llm.Response
	var err error
	if cacheName != "" {
		cached := whole
		cached.System, cached.Prompt, cached.CachedContent = "", suffix, cacheName
		resp, err = rv.client.Complete(ctx, cached)
		if errors.Is(err, llm.ErrCacheUnavailable) {
			rv.cache.lost(cacheName)
			resp, err = rv.client.Complete(ctx, whole)
		} else if err == nil {
			rv.cache.served(cacheName, resp.Model)
		}
	} else {
		resp, err = rv.client.Complete(ctx, whole)
	}
	if err != nil {
		return nil, usage{}, err
	}
	res, err := extractJSON(resp.Text)
	if err != nil {
		return nil, usage{}, err
	}
	model := resp.Model
	if model == "" {
		model = rv.model
	}
	price, pricedAs := priceOf(model, rv.model, rv.clock(), resp.InputTokens)
	u := usage{
		Engine:           rv.engine,
		PromptTokens:     resp.InputTokens,
		CachedTokens:     resp.CacheReadTokens,
		CompletionTokens: resp.OutputTokens,
		ThoughtsTokens:   resp.ThoughtTokens,
		TotalTokens:      resp.InputTokens + resp.OutputTokens + resp.ThoughtTokens,
		CostUSD:          price.Cost(resp.InputTokens, resp.CacheReadTokens, resp.OutputTokens, resp.ThoughtTokens),
		Model:            model,
		PricedAs:         pricedAs,
	}
	return res, u, nil
}

func (rv *reviewer) clock() time.Time {
	if rv.now != nil {
		return rv.now()
	}
	return time.Now()
}

// priceOf is the price of a call answered by model (or, when the table does
// not know it, of the configured model, else of the default model, so an
// unknown model is never free), and the table row it was priced as. A
// fallback row is named "<row> (for <model>)" so the usage file shows it.
func priceOf(model, configured string, at time.Time, prompt int64) (llm.GeminiPrice, string) {
	if p, as, ok := llm.LookupGeminiPrice(model, at, prompt); ok {
		return p, as
	}
	for _, m := range []string{configured, llm.DefaultGeminiModel} {
		if p, as, ok := llm.LookupGeminiPrice(m, at, prompt); ok {
			return p, fmt.Sprintf("%s (for %s)", as, model)
		}
	}
	return llm.GeminiPrice{}, ""
}
