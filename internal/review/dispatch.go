package review

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/eideroliveira/aspect/internal/llm"
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

func userPrompt(req request, s *splitDiff, p diffPart) string {
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

// usage is what one review cost.
type usage struct {
	Engine           string
	CostUSD          float64
	PromptTokens     int64
	CachedTokens     int64
	CompletionTokens int64
	TotalTokens      int64
}

func (u *usage) add(o usage) {
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
	u.TotalTokens += o.TotalTokens
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
		res, u, err := rv.ask(ctx, userPrompt(req, s, p), &budget, h.started)
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
			res, u, err := rv.ask(pctx, userPrompt(req, s, p), &budget, started)
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

// ask sends one prompt once a slot is free.
func (rv *reviewer) ask(ctx context.Context, prompt string, thinkingBudget *int, started func()) (*Object, usage, error) {
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
	resp, err := rv.client.Complete(ctx, llm.Request{
		System:         sharedSystemPrompt,
		Prompt:         prompt,
		JSON:           true,
		ThinkingBudget: thinkingBudget,
	})
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
	u := usage{
		Engine:           rv.engine,
		PromptTokens:     resp.InputTokens,
		CachedTokens:     resp.CacheReadTokens,
		CompletionTokens: resp.OutputTokens,
		TotalTokens:      resp.InputTokens + resp.OutputTokens + resp.ThoughtTokens,
		CostUSD:          geminiCost(model, resp.InputTokens, resp.CacheReadTokens, resp.OutputTokens),
	}
	return res, u, nil
}

// geminiCost is the estimated cost in USD of a Gemini call. Cached prompt
// tokens are billed at 25% of the input rate (75% discount).
func geminiCost(model string, prompt, cached, completion int64) float64 {
	var in, out float64
	switch {
	case strings.Contains(strings.ToLower(model), "pro") && prompt > 128_000:
		in, out = 2.50, 10.00
	case strings.Contains(strings.ToLower(model), "pro"):
		in, out = 1.25, 5.00
	case prompt > 128_000:
		in, out = 0.15, 0.60
	default:
		in, out = 0.075, 0.30
	}
	cached = max(0, min(cached, prompt))
	uncached := prompt - cached
	inCost := float64(uncached)*in + float64(cached)*(in*0.25)
	return roundTo((inCost+float64(completion)*out)/1_000_000, 6)
}
