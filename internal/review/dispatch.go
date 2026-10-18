package review

import (
	"context"
	"fmt"
	"strings"

	"github.com/eideroliveira/aspect/internal/llm"
)

// The prompt layout, the same for every reviewer:
//
//	system: sharedSystemPrompt (identical for all reviewers)
//	user:   PR context and changed-file overview   (shared by the run)
//	        the diff, or one part of it             (shared by reviewers
//	                                                 with the same file scope)
//	        this reviewer's context, role instructions and output format
//
// Everything reviewers share comes first and the instructions come last,
// next to where the model starts writing, as long-context guidance
// recommends. A diff over the prompt budget is reviewed in parts.

const sharedSystemPrompt = `You are a senior code reviewer. The user message holds a code change ` +
	`followed by the role you review it in, and the exact output format.

You have no tools in this session: you cannot run commands, open files, run tests or write ` +
	`files. The diff shown is everything you can see. Where your role's instructions say to run git, ` +
	`read a file, run tests or write a report file, apply that step to the diff given instead, and ` +
	`return the report as your answer. Cite file paths and line numbers from the new (head) side of ` +
	`the diff. Report only issues the diff shows evidence for.`

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
	agentContext  string // this reviewer's assignment
}

func userPrompt(req request, s *splitDiff, p diffPart) string {
	var sections []string
	if req.sharedContext != "" {
		sections = append(sections, strings.TrimSpace(req.sharedContext))
	}
	sections = append(sections, "### Change overview\n"+s.overview())
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
	fixed := sharedSystemPrompt + req.sharedContext + s.overview() + req.agentContext + req.instructions + req.outputFormat
	return promptBudget - estimateTokens(fixed) - promptOverheadTokens
}

// planParts splits the diff into parts that each fit the prompt budget.
func planParts(req request, promptBudget int) (*splitDiff, []diffPart) {
	s := splitReviewable(req.diff)
	return s, chunkDiff(s, diffBudget(req, s, promptBudget))
}

// usage is what one review cost.
type usage struct {
	Engine           string
	CostUSD          float64
	PromptTokens     int64
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
	var results []*Object
	var total usage
	for _, p := range parts {
		if h.waiting != nil {
			h.waiting()
		}
		res, u, err := rv.ask(ctx, userPrompt(req, s, p), h.started)
		if err != nil {
			return nil, total, err
		}
		results = append(results, res)
		total.add(u)
		if h.partDone != nil {
			h.partDone(p.index, p.total)
		}
	}
	return mergeParts(results), total, nil
}

// ask sends one prompt once a slot is free.
func (rv *reviewer) ask(ctx context.Context, prompt string, started func()) (*Object, usage, error) {
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
	resp, err := rv.client.Complete(ctx, llm.Request{System: sharedSystemPrompt, Prompt: prompt, JSON: true})
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
		CompletionTokens: resp.OutputTokens,
		TotalTokens:      resp.InputTokens + resp.OutputTokens + resp.ThoughtTokens,
		CostUSD:          geminiCost(model, resp.InputTokens, resp.OutputTokens),
	}
	return res, u, nil
}

// geminiCost is the estimated cost in USD of a Gemini call.
func geminiCost(model string, prompt, completion int64) float64 {
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
	return roundTo((float64(prompt)*in+float64(completion)*out)/1_000_000, 6)
}
