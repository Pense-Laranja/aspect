package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// GeminiAPIBase is the Gemini API the client calls.
const GeminiAPIBase = "https://generativelanguage.googleapis.com/v1beta"

// DefaultGeminiModel is the review engine's model when none is configured.
const DefaultGeminiModel = "gemini-flash-latest"

// geminiMaxErrorBody bounds the error bodies quoted in errors, which end up
// in run status files.
const geminiMaxErrorBody = 1000

// geminiMaxRetryWait caps the wait before a retry, whatever the server asks.
const geminiMaxRetryWait = 60 * time.Second

// geminiRetryStatuses are worth retrying: the rate limit and transient
// server errors.
var geminiRetryStatuses = map[int]bool{429: true, 500: true, 502: true, 503: true, 504: true}

// google.rpc.RetryInfo in a 429 body: "retryDelay": "22s"
var retryDelayRE = regexp.MustCompile(`"retryDelay"\s*:\s*"(\d+(?:\.\d+)?)s"`)

// Gemini is a Client for the Gemini generateContent API over plain HTTP.
//
// A request is retried on HTTP 429 and transient 5xx, waiting what the server
// asks (Retry-After, RetryInfo) or backing off exponentially. A blocked
// prompt (promptFeedback.blockReason) is sent again: the block is random per
// attempt, and the last retry turns the model's thinking off, which is where
// the blocks come from (measured 2026-10-09: the red-team prompt was blocked
// 3 of 6 times with thinking on, 0 of 5 with it off).
type Gemini struct {
	http            *http.Client
	base            string
	key             string
	model           string
	temperature     float64
	timeout         time.Duration
	retries         int
	retryBase       time.Duration
	blockRetries    int
	blockNoThinking bool
	sleep           func(context.Context, time.Duration) error
}

// GeminiOption configures the Gemini client.
type GeminiOption func(*Gemini)

// WithGeminiModel selects the model (for example "gemini-flash-latest").
func WithGeminiModel(model string) GeminiOption {
	return func(g *Gemini) {
		if model != "" {
			g.model = model
		}
	}
}

// WithGeminiTemperature sets the sampling temperature.
func WithGeminiTemperature(t float64) GeminiOption { return func(g *Gemini) { g.temperature = t } }

// WithGeminiTimeout bounds one HTTP attempt. The answer arrives only after
// the model's thinking, so the bound has to cover it.
func WithGeminiTimeout(d time.Duration) GeminiOption { return func(g *Gemini) { g.timeout = d } }

// WithGeminiRetries sets how many times a rate-limited or failing request is
// retried, and the base of the exponential backoff.
func WithGeminiRetries(n int, base time.Duration) GeminiOption {
	return func(g *Gemini) { g.retries, g.retryBase = max(0, n), base }
}

// WithGeminiBlockRetries sets how many times a blocked prompt is sent again,
// and whether the last of those retries turns thinking off.
func WithGeminiBlockRetries(n int, finalNoThinking bool) GeminiOption {
	return func(g *Gemini) { g.blockRetries, g.blockNoThinking = max(0, n), finalNoThinking }
}

// WithGeminiBaseURL points the client at another endpoint (tests).
func WithGeminiBaseURL(base string) GeminiOption { return func(g *Gemini) { g.base = base } }

// WithGeminiHTTPClient replaces the HTTP client (tests).
func WithGeminiHTTPClient(c *http.Client) GeminiOption { return func(g *Gemini) { g.http = c } }

// WithGeminiSleep replaces the wait between retries (tests must not sleep).
func WithGeminiSleep(sleep func(context.Context, time.Duration) error) GeminiOption {
	return func(g *Gemini) { g.sleep = sleep }
}

// NewGemini returns a Gemini client using key.
func NewGemini(key string, opts ...GeminiOption) *Gemini {
	g := &Gemini{
		http:            &http.Client{},
		base:            GeminiAPIBase,
		key:             key,
		model:           DefaultGeminiModel,
		temperature:     0.2,
		timeout:         360 * time.Second,
		retries:         3,
		retryBase:       5 * time.Second,
		blockRetries:    4,
		blockNoThinking: true,
		sleep:           sleepCtx,
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

// Model is the model the client calls.
func (g *Gemini) Model() string { return g.model }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ErrNoGeminiKey means no API key was configured.
var ErrNoGeminiKey = errors.New("no Gemini API key configured (set GEMINI_API_KEY or GOOGLE_API_KEY)")

type geminiPart struct {
	Text    string `json:"text"`
	Thought bool   `json:"thought,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiThinking struct {
	ThinkingBudget int `json:"thinkingBudget"`
}

type geminiGenConfig struct {
	ResponseMIMEType string          `json:"response_mime_type,omitempty"`
	Temperature      float64         `json:"temperature"`
	MaxOutputTokens  int64           `json:"maxOutputTokens,omitempty"`
	ThinkingConfig   *geminiThinking `json:"thinkingConfig,omitempty"`
}

type geminiRequest struct {
	SystemInstruction *geminiContent  `json:"system_instruction,omitempty"`
	Contents          []geminiContent `json:"contents"`
	GenerationConfig  geminiGenConfig `json:"generationConfig"`
}

type geminiResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback map[string]any `json:"promptFeedback"`
	UsageMetadata  struct {
		PromptTokenCount     int64 `json:"promptTokenCount"`
		CandidatesTokenCount int64 `json:"candidatesTokenCount"`
		ThoughtsTokenCount   int64 `json:"thoughtsTokenCount"`
		CachedContentTokens  int64 `json:"cachedContentTokenCount"`
	} `json:"usageMetadata"`
}

// Complete sends one generateContent request, with the retries above.
func (g *Gemini) Complete(ctx context.Context, req Request) (Response, error) {
	if g.key == "" {
		return Response{}, ErrNoGeminiKey
	}
	body := geminiRequest{
		Contents:         []geminiContent{{Role: "user", Parts: []geminiPart{{Text: req.Prompt}}}},
		GenerationConfig: geminiGenConfig{Temperature: g.temperature, MaxOutputTokens: req.MaxTokens},
	}
	if req.System != "" {
		body.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: req.System}}}
	}
	if req.JSON || req.Schema != nil {
		body.GenerationConfig.ResponseMIMEType = "application/json"
	}
	if req.ThinkingBudget != nil {
		body.GenerationConfig.ThinkingConfig = &geminiThinking{ThinkingBudget: *req.ThinkingBudget}
	}
	url := fmt.Sprintf("%s/models/%s:generateContent", g.base, g.model)

	attempt, blocked, calls := 0, 0, 0
	for {
		calls++
		status, header, raw, err := g.post(ctx, url, body)
		if err != nil {
			return Response{}, err
		}
		if status != http.StatusOK {
			text := strings.TrimSpace(string(raw))
			if len(text) > geminiMaxErrorBody {
				text = text[:geminiMaxErrorBody]
			}
			if text == "" {
				text = "<empty body>"
			}
			if geminiRetryStatuses[status] && attempt < g.retries {
				wait := geminiRetryWait(header, string(raw), attempt, g.retryBase)
				attempt++
				if err := g.sleep(ctx, wait); err != nil {
					return Response{}, err
				}
				continue
			}
			tried := ""
			if attempt > 0 {
				tried = fmt.Sprintf(" (after %d retries)", attempt)
			}
			err := fmt.Errorf("Gemini API returned HTTP %d for %s%s: %s", status, g.model, tried, text)
			if geminiRetryStatuses[status] {
				err = fmt.Errorf("%w: %w", ErrTransient, err)
			}
			return Response{}, err
		}

		var resp geminiResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			text := string(raw)
			if len(text) > geminiMaxErrorBody {
				text = text[:geminiMaxErrorBody]
			}
			return Response{}, fmt.Errorf("Gemini returned a non-JSON body (%v): %s", err, text)
		}
		if len(resp.Candidates) == 0 {
			if resp.PromptFeedback["blockReason"] != nil && blocked < g.blockRetries {
				blocked++
				if blocked == g.blockRetries && g.blockNoThinking {
					body.GenerationConfig.ThinkingConfig = &geminiThinking{ThinkingBudget: 0}
				}
				continue
			}
			tried := ""
			if blocked > 0 {
				tried = fmt.Sprintf(" (after %d retries)", blocked)
			}
			fb, _ := json.Marshal(resp.PromptFeedback)
			err := fmt.Errorf("Gemini returned no candidates%s: promptFeedback %s", tried, fb)
			if resp.PromptFeedback["blockReason"] != nil {
				err = fmt.Errorf("%w: %w", ErrRefused, err)
			}
			return Response{}, err
		}
		var text strings.Builder
		for _, p := range resp.Candidates[0].Content.Parts {
			if !p.Thought {
				text.WriteString(p.Text)
			}
		}
		if len(resp.Candidates[0].Content.Parts) == 0 {
			return Response{}, fmt.Errorf("Gemini candidate has no parts (finishReason %q)", resp.Candidates[0].FinishReason)
		}
		u := resp.UsageMetadata
		out := Response{
			Text:            strings.TrimSpace(text.String()),
			Model:           g.model,
			StopReason:      resp.Candidates[0].FinishReason,
			InputTokens:     u.PromptTokenCount,
			OutputTokens:    u.CandidatesTokenCount,
			ThoughtTokens:   u.ThoughtsTokenCount,
			CacheReadTokens: u.CachedContentTokens,
			Calls:           calls,
		}
		if out.StopReason == "MAX_TOKENS" {
			return out, fmt.Errorf("%w: Gemini %s hit its output limit", ErrTruncated, g.model)
		}
		return out, nil
	}
}

// post sends one attempt and returns its status, headers and body. A
// timeout or transport failure is an error worth retrying by the caller.
func (g *Gemini) post(ctx context.Context, url string, body geminiRequest) (int, http.Header, []byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, nil, nil, err
	}
	actx := ctx
	if g.timeout > 0 {
		var cancel context.CancelFunc
		actx, cancel = context.WithTimeout(ctx, g.timeout)
		defer cancel()
	}
	hreq, err := http.NewRequestWithContext(actx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return 0, nil, nil, err
	}
	hreq.Header.Set("x-goog-api-key", g.key)
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := g.http.Do(hreq)
	if err == nil {
		defer resp.Body.Close()
		var raw []byte
		raw, err = io.ReadAll(resp.Body)
		if err == nil {
			return resp.StatusCode, resp.Header, raw, nil
		}
	}
	if ctx.Err() != nil {
		return 0, nil, nil, ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		return 0, nil, nil, fmt.Errorf("%w: Gemini %s request timed out (limit %s; the model's thinking counts against it, raise CODE_REVIEWER_GEMINI_TIMEOUT)", ErrTransient, g.model, g.timeout)
	}
	return 0, nil, nil, fmt.Errorf("%w: Gemini %s request failed: %v", ErrTransient, g.model, err)
}

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

// geminiRetryWait is how long to wait before retrying: the server's
// Retry-After header or RetryInfo retryDelay when given, else exponential
// backoff from base. Never more than geminiMaxRetryWait.
func geminiRetryWait(h http.Header, body string, attempt int, base time.Duration) time.Duration {
	if v := h.Get("Retry-After"); v != "" {
		if s, err := strconv.ParseFloat(v, 64); err == nil {
			return min(time.Duration(s*float64(time.Second)), geminiMaxRetryWait)
		}
	}
	if m := retryDelayRE.FindStringSubmatch(body); m != nil {
		s, _ := strconv.ParseFloat(m[1], 64)
		return min(time.Duration(s*float64(time.Second)), geminiMaxRetryWait)
	}
	return min(time.Duration(float64(base)*math.Pow(2, float64(attempt))), geminiMaxRetryWait)
}
