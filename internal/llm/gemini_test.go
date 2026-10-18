package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const okBody = `{"candidates":[{"content":{"parts":[{"text":"{\"findings\": [], \"summary\": \"ok\"}"}]},"finishReason":"STOP"}],` +
	`"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":7}}`

const blockedBody = `{"promptFeedback":{"blockReason":"OTHER"},"usageMetadata":{"promptTokenCount":83297}}`

const quotaBody = `{"error": {"code": 429, "status": "RESOURCE_EXHAUSTED", "details": [{"retryDelay": "22s"}]}}`

// reply is one scripted HTTP answer.
type reply struct {
	status int
	body   string
	header map[string]string
}

// fakeGemini serves scripted replies and records each request's body.
type fakeGemini struct {
	mu       sync.Mutex
	replies  []reply
	requests []geminiRequest
	keys     []string
}

func (f *fakeGemini) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var req geminiRequest
	_ = json.Unmarshal(b, &req)
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.keys = append(f.keys, r.Header.Get("x-goog-api-key"))
	rep := reply{status: 200, body: okBody}
	if len(f.replies) > 0 {
		rep = f.replies[0]
		if len(f.replies) > 1 {
			f.replies = f.replies[1:]
		}
	}
	f.mu.Unlock()
	for k, v := range rep.header {
		w.Header().Set(k, v)
	}
	w.WriteHeader(rep.status)
	io.WriteString(w, rep.body)
}

func newTestGemini(t *testing.T, f *fakeGemini, opts ...GeminiOption) (*Gemini, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	var sleeps []time.Duration
	opts = append([]GeminiOption{
		WithGeminiBaseURL(srv.URL),
		WithGeminiModel("flash"),
		WithGeminiSleep(func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }),
	}, opts...)
	return NewGemini("test-key", opts...), &sleeps
}

func TestGeminiRequestAndUsage(t *testing.T) {
	f := &fakeGemini{}
	g, _ := newTestGemini(t, f, WithGeminiTemperature(0.2))
	resp, err := g.Complete(context.Background(), Request{System: "sys", Prompt: "user", JSON: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != `{"findings": [], "summary": "ok"}` {
		t.Errorf("text = %q", resp.Text)
	}
	if resp.InputTokens != 10 || resp.OutputTokens != 5 || resp.ThoughtTokens != 7 || resp.Model != "flash" || resp.Calls != 1 {
		t.Errorf("usage = %+v", resp)
	}
	req := f.requests[0]
	if req.SystemInstruction == nil || req.SystemInstruction.Parts[0].Text != "sys" || req.Contents[0].Parts[0].Text != "user" {
		t.Errorf("request = %+v", req)
	}
	if req.GenerationConfig.ResponseMIMEType != "application/json" || req.GenerationConfig.Temperature != 0.2 {
		t.Errorf("generationConfig = %+v", req.GenerationConfig)
	}
	if f.keys[0] != "test-key" {
		t.Errorf("api key header = %q", f.keys[0])
	}
}

func TestGeminiNoKeyMakesNoRequest(t *testing.T) {
	f := &fakeGemini{}
	g, _ := newTestGemini(t, f)
	g.key = ""
	_, err := g.Complete(context.Background(), Request{Prompt: "u"})
	if !errors.Is(err, ErrNoGeminiKey) || !strings.Contains(err.Error(), "GEMINI_API_KEY") {
		t.Fatalf("err = %v", err)
	}
	if len(f.requests) != 0 {
		t.Fatal("a request was sent without a key")
	}
}

func TestGemini429WaitsTheServersRetryDelay(t *testing.T) {
	f := &fakeGemini{replies: []reply{{status: 429, body: quotaBody}, {status: 200, body: okBody}}}
	g, sleeps := newTestGemini(t, f)
	resp, err := g.Complete(context.Background(), Request{Prompt: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.requests) != 2 || resp.Calls != 2 {
		t.Errorf("requests = %d, calls = %d", len(f.requests), resp.Calls)
	}
	if want := []time.Duration{22 * time.Second}; !equalDurations(*sleeps, want) {
		t.Errorf("sleeps = %v, want %v", *sleeps, want)
	}
}

func TestGeminiRetryAfterHeaderWins(t *testing.T) {
	f := &fakeGemini{replies: []reply{{status: 503, body: "overloaded", header: map[string]string{"Retry-After": "7"}}, {status: 200, body: okBody}}}
	g, sleeps := newTestGemini(t, f)
	if _, err := g.Complete(context.Background(), Request{Prompt: "u"}); err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{7 * time.Second}; !equalDurations(*sleeps, want) {
		t.Errorf("sleeps = %v, want %v", *sleeps, want)
	}
}

func TestGeminiBackoffDoublesWithoutAServerHint(t *testing.T) {
	f := &fakeGemini{replies: []reply{{status: 429, body: "{}"}, {status: 429, body: "{}"}, {status: 200, body: okBody}}}
	g, sleeps := newTestGemini(t, f, WithGeminiRetries(3, 5*time.Second))
	if _, err := g.Complete(context.Background(), Request{Prompt: "u"}); err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{5 * time.Second, 10 * time.Second}; !equalDurations(*sleeps, want) {
		t.Errorf("sleeps = %v, want %v", *sleeps, want)
	}
}

func TestGemini429GivesUpAfterMaxRetries(t *testing.T) {
	f := &fakeGemini{replies: []reply{{status: 429, body: quotaBody}}}
	g, _ := newTestGemini(t, f, WithGeminiRetries(2, time.Second))
	_, err := g.Complete(context.Background(), Request{Prompt: "u"})
	if err == nil || len(f.requests) != 3 {
		t.Fatalf("err = %v after %d requests", err, len(f.requests))
	}
	for _, want := range []string{"HTTP 429", "after 2 retries", "RESOURCE_EXHAUSTED"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if !IsTransient(err) {
		t.Error("a rate limit is transient")
	}
}

func TestGeminiClientErrorsAreNotRetried(t *testing.T) {
	f := &fakeGemini{replies: []reply{{status: 400, body: "bad request"}}}
	g, sleeps := newTestGemini(t, f)
	_, err := g.Complete(context.Background(), Request{Prompt: "u"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") || !strings.Contains(err.Error(), "bad request") {
		t.Fatalf("err = %v", err)
	}
	if len(f.requests) != 1 || len(*sleeps) != 0 {
		t.Errorf("requests = %d, sleeps = %v", len(f.requests), *sleeps)
	}
}

func TestGeminiEmptyErrorBodyIsNamed(t *testing.T) {
	f := &fakeGemini{replies: []reply{{status: 403, body: ""}}}
	g, _ := newTestGemini(t, f)
	_, err := g.Complete(context.Background(), Request{Prompt: "u"})
	if err == nil || !strings.Contains(err.Error(), "<empty body>") {
		t.Fatalf("err = %v", err)
	}
}

func TestGeminiBlockedPromptIsSentAgain(t *testing.T) {
	f := &fakeGemini{replies: []reply{{status: 200, body: blockedBody}, {status: 200, body: okBody}}}
	g, sleeps := newTestGemini(t, f)
	if _, err := g.Complete(context.Background(), Request{Prompt: "u"}); err != nil {
		t.Fatal(err)
	}
	if len(f.requests) != 2 || len(*sleeps) != 0 {
		t.Errorf("requests = %d, sleeps = %v (a block waits for nothing)", len(f.requests), *sleeps)
	}
	for i, r := range f.requests {
		if r.GenerationConfig.ThinkingConfig != nil {
			t.Errorf("request %d turned thinking off before the last retry", i)
		}
	}
}

func TestGeminiBlockedPromptGivesUp(t *testing.T) {
	f := &fakeGemini{replies: []reply{{status: 200, body: blockedBody}}}
	g, _ := newTestGemini(t, f, WithGeminiBlockRetries(2, true))
	_, err := g.Complete(context.Background(), Request{Prompt: "u"})
	if err == nil || len(f.requests) != 3 {
		t.Fatalf("err = %v after %d requests", err, len(f.requests))
	}
	if !strings.Contains(err.Error(), "after 2 retries") || !strings.Contains(err.Error(), "blockReason") || !errors.Is(err, ErrRefused) {
		t.Errorf("err = %v", err)
	}
}

// Measured: the red-team prompt is blocked about half the time with thinking
// on and never with it off. Four retries by default, the last one without
// thinking.
func TestGeminiDefaultBlockRetriesEndWithThinkingOff(t *testing.T) {
	blocked := reply{status: 200, body: blockedBody}
	f := &fakeGemini{replies: []reply{blocked, blocked, blocked, blocked, {status: 200, body: okBody}}}
	g, _ := newTestGemini(t, f)
	if _, err := g.Complete(context.Background(), Request{Prompt: "u"}); err != nil {
		t.Fatal(err)
	}
	if len(f.requests) != 5 {
		t.Fatalf("requests = %d", len(f.requests))
	}
	for i, r := range f.requests {
		off := r.GenerationConfig.ThinkingConfig != nil && r.GenerationConfig.ThinkingConfig.ThinkingBudget == 0
		if off != (i == 4) {
			t.Errorf("request %d: thinking off = %v", i, off)
		}
	}
}

func TestGeminiThinkingStaysOnWhenTheFallbackIsDisabled(t *testing.T) {
	blocked := reply{status: 200, body: blockedBody}
	f := &fakeGemini{replies: []reply{blocked, blocked, {status: 200, body: okBody}}}
	g, _ := newTestGemini(t, f, WithGeminiBlockRetries(2, false))
	if _, err := g.Complete(context.Background(), Request{Prompt: "u"}); err != nil {
		t.Fatal(err)
	}
	for i, r := range f.requests {
		if r.GenerationConfig.ThinkingConfig != nil {
			t.Errorf("request %d turned thinking off", i)
		}
	}
}

func TestGeminiTimeoutNamesTheSetting(t *testing.T) {
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))
	defer srv.Close()
	defer close(done)
	g := NewGemini("k", WithGeminiBaseURL(srv.URL), WithGeminiTimeout(20*time.Millisecond))
	_, err := g.Complete(context.Background(), Request{Prompt: "u"})
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "CODE_REVIEWER_GEMINI_TIMEOUT") || !IsTransient(err) {
		t.Fatalf("err = %v", err)
	}
}

func equalDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
