// Package llm is the only place in Aspect that talks to a model provider.
// Agents depend on the Client interface, so tests run against a fake and the
// pipeline can be exercised end to end without network access or an API key.
package llm

import (
	"context"
	"errors"
)

// Sentinel errors agents can test for with errors.Is. They are wrapped by
// every Client implementation so the recovery policy lives in one place
// (agents.complete) and not in each provider.
var (
	// ErrTruncated means the model hit MaxTokens before finishing. The
	// partial text is still returned in the Response.
	ErrTruncated = errors.New("response truncated")
	// ErrRefused means the model declined to answer.
	ErrRefused = errors.New("model refused")
	// ErrTransient marks failures worth retrying as they are: a broken
	// stream, a timeout, a rate limit, an overloaded or failing server.
	ErrTransient = errors.New("transient failure")
)

// IsTransient reports whether err is worth retrying unchanged.
func IsTransient(err error) bool { return errors.Is(err, ErrTransient) }

// Request is one single-turn completion. Agents in Aspect are stateless: every
// call carries its whole context, which keeps runs reproducible and auditable.
type Request struct {
	// System is the agent's role prompt. It is cached across calls.
	System string
	// Prompt is the task-specific user turn.
	Prompt string
	// Schema, when set, is a JSON schema the response must conform to
	// (structured outputs). Every object in it must declare
	// additionalProperties: false.
	Schema map[string]any
	// MaxTokens caps the response. Zero uses the client default.
	MaxTokens int64
	// JSON asks for an answer that is one JSON object, without a schema.
	// Clients that cannot enforce it leave it to the prompt.
	JSON bool
	// ThinkingBudget, when non-nil, caps the model's thinking tokens.
	ThinkingBudget *int
}

// Response is the model's answer plus accounting.
type Response struct {
	Text            string
	Model           string
	StopReason      string
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	// ThoughtTokens are the model's thinking tokens, billed as output but not
	// part of Text (Gemini reports them apart from OutputTokens).
	ThoughtTokens int64
	// Calls is how many API calls this response aggregates; zero means one.
	Calls int
}

// Usage accumulates token counts across a run.
type Usage struct {
	Calls           int   `json:"calls"`
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`
}

// Add folds a response into the totals. A Response that aggregates several
// calls (see Merge) counts each of them.
func (u *Usage) Add(r Response) {
	calls := r.Calls
	if calls == 0 && (r.InputTokens > 0 || r.OutputTokens > 0 || r.Text != "") {
		calls = 1
	}
	u.Calls += calls
	u.InputTokens += r.InputTokens
	u.OutputTokens += r.OutputTokens
	u.CacheReadTokens += r.CacheReadTokens
}

// Merge folds next into r when one agent step needed several calls (a
// retry, a fallback, a re-ask): tokens add up, the text and stop reason are
// those of the latest call, and Calls counts them so accounting stays
// honest.
func (r Response) Merge(next Response) Response {
	prev, cur := r.Calls, next.Calls
	if prev == 0 && (r.InputTokens > 0 || r.OutputTokens > 0 || r.Text != "") {
		prev = 1
	}
	if cur == 0 {
		cur = 1
	}
	next.Calls = prev + cur
	next.InputTokens += r.InputTokens
	next.OutputTokens += r.OutputTokens
	next.CacheReadTokens += r.CacheReadTokens
	next.ThoughtTokens += r.ThoughtTokens
	return next
}

// Client completes requests.
type Client interface {
	Complete(ctx context.Context, req Request) (Response, error)
}
