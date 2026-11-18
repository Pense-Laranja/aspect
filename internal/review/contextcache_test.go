package review

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pense-Laranja/aspect/internal/gate"
	"github.com/Pense-Laranja/aspect/internal/llm"
)

// agentRequest is the request runAgent builds for agent, with the change's
// shared parts the same for every agent.
func agentRequest(agent, diff string) request {
	return request{
		agent:         agent,
		instructions:  "You are the " + agent + ". Hunt for what " + agent + " hunts for.",
		outputFormat:  "Produce your report strictly conforming to the `aspect-review/v1` schema.\n" + reportFormat,
		diff:          diff,
		sharedContext: "### Pull request\nTitle: Add the thing\n\nBody.",
		specContext:   "### Specification of the touched modules (_aspect/aspect.yaml)\n" + strings.Repeat("An invariant. ", 3000),
		agentContext:  "### Your review assignment\n```\nagent: " + agent + "\nmode: gate\n```",
	}
}

func TestPromptPrefixIsSharedAcrossAgents(t *testing.T) {
	agents := []string{"adversarial-reviewer", "quality-reviewer", "red-team", "i18n-reviewer"}
	for _, budget := range []int{800_000, 12_000} { // one part, then several
		var prefixes [][]string
		for _, a := range agents {
			req := agentRequest(a, manyFiles())
			s, parts := planParts(req, budget)
			var ps []string
			for _, p := range parts {
				prefix, suffix := promptPrefix(req, s, p), promptSuffix(req)
				if strings.Contains(prefix, a) {
					t.Errorf("budget %d: %s's prefix names the agent", budget, a)
				}
				if !strings.Contains(suffix, req.instructions) || !strings.Contains(suffix, "agent: "+a) {
					t.Errorf("budget %d: %s's suffix lacks its role or assignment", budget, a)
				}
				// The cached request reads the same text in the same order as
				// the whole prompt.
				if userPrompt(req, s, p) != prefix+"\n\n"+suffix {
					t.Errorf("budget %d: %s's prompt is not prefix + suffix", budget, a)
				}
				ps = append(ps, prefix)
			}
			prefixes = append(prefixes, ps)
		}
		if budget < 800_000 && len(prefixes[0]) < 2 {
			t.Fatalf("budget %d: want several parts, got %d", budget, len(prefixes[0]))
		}
		for i := range agents[1:] {
			if strings.Join(prefixes[i+1], "\x00") != strings.Join(prefixes[0], "\x00") {
				t.Errorf("budget %d: %s's prefixes differ from %s's", budget, agents[i+1], agents[0])
			}
		}
	}
	// A different file scope is a different prefix.
	a, b := agentRequest("x", manyFiles()), agentRequest("x", fileDiffText("p/other.go", 1, 3, "y = 2"))
	sa, pa := planParts(a, 800_000)
	sb, pb := planParts(b, 800_000)
	if promptPrefix(a, sa, pa[0]) == promptPrefix(b, sb, pb[0]) {
		t.Error("two diffs share a prefix")
	}
}

// fakeCacheClient answers like Gemini with explicit caching: a request
// naming a cache is billed its tokens as cached.
type fakeCacheClient struct {
	mu        sync.Mutex
	created   map[string]string // name -> system + prefix
	tokens    map[string]int64
	deleted   []string
	requests  []llm.Request
	createErr error
	refuse    bool // answer every cached request with ErrCacheUnavailable
}

func newFakeCacheClient() *fakeCacheClient {
	return &fakeCacheClient{created: map[string]string{}, tokens: map[string]int64{}}
}

func (f *fakeCacheClient) CreateCache(_ context.Context, system, prefix string, _ time.Duration) (llm.Cache, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return llm.Cache{}, f.createErr
	}
	name := fmt.Sprintf("cachedContents/c%d", len(f.created)+1)
	f.created[name] = system + "\x00" + prefix
	f.tokens[name] = int64(estimateTokens(system + prefix))
	return llm.Cache{Name: name, Tokens: f.tokens[name]}, nil
}

func (f *fakeCacheClient) DeleteCache(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, name)
	return nil
}

func (f *fakeCacheClient) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	resp := llm.Response{Text: `{"summary": "ok", "findings": []}`, Model: "gemini-3.8-flash", OutputTokens: 100, ThoughtTokens: 1000}
	if req.CachedContent != "" {
		if f.refuse {
			return llm.Response{}, fmt.Errorf("%w: gone", llm.ErrCacheUnavailable)
		}
		resp.CacheReadTokens = f.tokens[req.CachedContent]
		resp.InputTokens = resp.CacheReadTokens + int64(estimateTokens(req.Prompt))
	} else {
		resp.InputTokens = int64(estimateTokens(req.System + req.Prompt))
	}
	return resp, nil
}

// reviewAll plans and runs reqs the way Engine.Run does.
func reviewAll(t *testing.T, client llm.Client, reqs []request) ([]usage, usage, []cacheRecord) {
	t.Helper()
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return at }
	rv := &reviewer{client: client, engine: "gemini (gemini-flash-latest)", model: "gemini-flash-latest", promptBudget: 800_000, now: now}
	rv.cache = newContextCache(client, 20*time.Minute, now)
	for _, req := range reqs {
		s, parts := planParts(req, rv.promptBudget)
		for _, p := range parts {
			rv.cache.plan(sharedSystemPrompt, promptPrefix(req, s, p))
		}
	}
	us := make([]usage, len(reqs))
	var wg sync.WaitGroup
	for i, req := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, u, err := rv.review(context.Background(), req, hooks{})
			if err != nil {
				t.Error(err)
			}
			us[i] = u
		}()
	}
	wg.Wait()
	cost, records := rv.cache.close(context.Background(), rv.model)
	return us, cost, records
}

func TestSharedPrefixIsCachedOnceAndReadByEachRequest(t *testing.T) {
	f := newFakeCacheClient()
	reqs := []request{
		agentRequest("adversarial-reviewer", manyFiles()),
		agentRequest("quality-reviewer", manyFiles()),
		agentRequest("i18n-reviewer", manyFiles()),
		agentRequest("database-reviewer", fileDiffText("models/x.go", 1, 3, "y = 2")), // its own scope
	}
	us, cacheCost, records := reviewAll(t, f, reqs)
	if len(f.created) != 1 {
		t.Fatalf("created %d caches, want 1 (a prefix sent once is not worth one)", len(f.created))
	}
	cached, whole := 0, 0
	for _, r := range f.requests {
		if r.CachedContent != "" {
			cached++
			if r.System != "" || strings.Contains(r.Prompt, "```diff") || !strings.Contains(r.Prompt, "### Your role") {
				t.Errorf("a cached request sent more than its own part: system %q, prompt %.80q", r.System, r.Prompt)
			}
			if f.created[r.CachedContent] != sharedSystemPrompt+"\x00"+promptPrefix(reqs[0], splitReviewable(reqs[0].diff), mustOnePart(t, reqs[0])) {
				t.Error("the cache does not hold the system prompt and the shared prefix")
			}
		} else {
			whole++
			if r.System != sharedSystemPrompt || !strings.Contains(r.Prompt, "database-reviewer") {
				t.Errorf("the uncached request is not database-reviewer's whole prompt")
			}
		}
	}
	if cached != 3 || whole != 1 {
		t.Errorf("%d cached and %d whole requests, want 3 and 1", cached, whole)
	}
	if len(f.deleted) != 1 || len(records) != 1 || records[0].Uses != 3 || records[0].PlannedUses != 3 || records[0].PricedAs != "gemini-3.8-flash" {
		t.Errorf("deleted %v, records %+v", f.deleted, records)
	}
	// Each request is billed its cached tokens at the cached rate and its
	// thinking at the output rate; the cache's writing on top, once.
	for i, u := range us[:3] {
		price, _, _ := llm.LookupGeminiPrice("gemini-3.8-flash", time.Now(), 0)
		want := price.Cost(u.PromptTokens, u.CachedTokens, 100, 1000)
		if u.CachedTokens == 0 || u.ThoughtsTokens != 1000 || u.CostUSD != want || u.Model != "gemini-3.8-flash" {
			t.Errorf("request %d: usage %+v, want cost %v", i, u, want)
		}
	}
	if want := records[0].CostUSD; cacheCost.CostUSD != want || want <= 0 {
		t.Errorf("cache cost %v, record says %v", cacheCost.CostUSD, want)
	}
}

func mustOnePart(t *testing.T, req request) diffPart {
	t.Helper()
	_, parts := planParts(req, 800_000)
	if len(parts) != 1 {
		t.Fatalf("%d parts", len(parts))
	}
	return parts[0]
}

func TestLostCacheFallsBackToTheWholePrompt(t *testing.T) {
	f := newFakeCacheClient()
	f.refuse = true
	reqs := []request{agentRequest("a", manyFiles()), agentRequest("b", manyFiles())}
	us, _, records := reviewAll(t, f, reqs)
	for i, u := range us {
		if u.CachedTokens != 0 || u.PromptTokens == 0 {
			t.Errorf("request %d: usage %+v", i, u)
		}
	}
	whole := 0
	for _, r := range f.requests {
		if r.CachedContent == "" && r.System == sharedSystemPrompt {
			whole++
		}
	}
	if whole != 2 || len(records) != 1 || !records[0].Lost {
		t.Errorf("%d whole requests, records %+v", whole, records)
	}
}

func TestCacheThatCannotBeCreatedSendsWholePrompts(t *testing.T) {
	f := newFakeCacheClient()
	f.createErr = fmt.Errorf("HTTP 400: too small")
	reqs := []request{agentRequest("a", manyFiles()), agentRequest("b", manyFiles())}
	_, cost, records := reviewAll(t, f, reqs)
	for _, r := range f.requests {
		if r.CachedContent != "" {
			t.Error("a request named a cache that was never created")
		}
	}
	if cost.CostUSD != 0 || len(records) != 1 || !strings.Contains(records[0].Error, "too small") || len(f.deleted) != 0 {
		t.Errorf("cost %v, records %+v, deleted %v", cost, records, f.deleted)
	}
}

func TestSmallPrefixIsNotCached(t *testing.T) {
	f := newFakeCacheClient()
	small := func(a string) request {
		r := agentRequest(a, fileDiffText("a.go", 1, 2, "x = 1"))
		r.specContext = ""
		return r
	}
	reviewAll(t, f, []request{small("a"), small("b")})
	if len(f.created) != 0 {
		t.Errorf("cached a prefix below Gemini's minimum")
	}
}

func TestClientWithoutCachingSendsWholePrompts(t *testing.T) {
	c := &scripted{script: []any{`{"summary": "ok", "findings": []}`, `{"summary": "ok", "findings": []}`}}
	if newContextCache(c, time.Minute, time.Now) != nil {
		t.Fatal("a client that cannot cache got a context cache")
	}
	if newContextCache(newFakeCacheClient(), 0, time.Now) != nil {
		t.Fatal("a zero ttl did not turn caching off")
	}
}

// cachingModel is fakeModel with explicit caching: a cached request's
// prompt is the agent's own part, which still names its role.
type cachingModel struct {
	*fakeModel
	mu      sync.Mutex
	created int
	deleted int
}

func (c *cachingModel) CreateCache(context.Context, string, string, time.Duration) (llm.Cache, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.created++
	return llm.Cache{Name: "cachedContents/run", Tokens: 50_000}, nil
}

func (c *cachingModel) DeleteCache(context.Context, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleted++
	return nil
}

func TestRunRecordsTheRealCostWithItsContextCache(t *testing.T) {
	repo := testRepo(t)
	// A change large enough to be worth caching, seen whole by both agents.
	writeFile(t, filepath.Join(repo, "a.go"), "package a\n\n"+strings.Repeat("func A() { _ = \"some long line of code\" }\n", 600))
	gitIn(t, repo, "commit", "-q", "-am", "big")
	answer := llm.Response{Text: clean, Model: "gemini-3.8-flash", InputTokens: 60_000, CacheReadTokens: 50_000, OutputTokens: 100, ThoughtTokens: 5_000}
	te := newTestEngine(t, repo, map[string]any{"security-reviewer": answer, "red-team": answer})
	cm := &cachingModel{fakeModel: te.model}
	te.Client = cm
	te.Config.CacheTTL = 20 * time.Minute
	res := te.run(t, Options{})
	if res.Outcome != OutcomePass {
		t.Fatalf("outcome %s", res.Outcome)
	}
	if cm.created != 1 || cm.deleted != 1 {
		t.Errorf("created %d, deleted %d caches; want 1, 1", cm.created, cm.deleted)
	}
	price, _, _ := llm.LookupGeminiPrice("gemini-3.8-flash", time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC), 0)
	perAgent := price.Cost(60_000, 50_000, 100, 5_000) // 10k*0.75 + 50k*0.075 + 5.1k*3.75 = $0.030375
	var cc struct {
		CostUSD float64       `json:"cost_usd"`
		Caches  []cacheRecord `json:"caches"`
	}
	b, err := os.ReadFile(filepath.Join(res.RunDir, "context-cache.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(b, &cc)
	if len(cc.Caches) != 1 || cc.Caches[0].Uses != 2 || cc.CostUSD <= 0 {
		t.Errorf("context-cache.json = %s", b)
	}
	st := readStatus(t, res.RunDir)
	want := roundTo(2*perAgent+cc.CostUSD, 6)
	if st["total_cost_usd"] != want {
		t.Errorf("total_cost_usd = %v, want %v (two agents at %v plus the cache's %v)", st["total_cost_usd"], want, perAgent, cc.CostUSD)
	}
	us, _ := st["usage"].(map[string]any)
	if us["thoughts_tokens"] != float64(10_000) || us["context_cache_cost_usd"] != cc.CostUSD {
		t.Errorf("run usage = %+v", us)
	}
	var side map[string]any
	ub, _ := os.ReadFile(filepath.Join(res.RunDir, "red-team.usage.json"))
	_ = json.Unmarshal(ub, &side)
	if side["thoughts_tokens"] != float64(5_000) || side["cost_usd"] != perAgent || side["model"] != "gemini-3.8-flash" || side["priced_as"] != "gemini-3.8-flash" {
		t.Errorf("red-team.usage.json = %+v", side)
	}
	if !strings.Contains(res.Rendered, gate.FormatCost(want)) {
		t.Errorf("the rendered result does not show the run's cost %s:\n%s", gate.FormatCost(want), res.Rendered)
	}
}
