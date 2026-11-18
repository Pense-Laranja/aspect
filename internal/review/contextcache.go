package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"github.com/Pense-Laranja/aspect/internal/llm"
)

// contextCache keeps, for one run, an explicit Gemini context cache of each
// prompt prefix (system prompt plus promptPrefix) that two or more of the
// run's requests share, so those requests pay the cached rate for it. It
// is created on the first request that needs it and deleted when the run
// ends (close); its ttl bounds what a run that dies on the way costs.
//
// Whether a prefix is worth a cache is decided before any request is sent
// (plan): writing one is billed as input and storing it by the hour, so a
// prefix sent once would cost more cached than not. With Gemini 3.8 Flash
// ($0.75 input, $0.075 cached, $0.50 per million tokens per hour,
// 2026-11-17), two requests sharing a prefix of T tokens cost 1.5T without
// a cache and 0.75T + 2*0.075T + 0.5T*hours with one: cheaper whenever the
// cache lives less than 1.2 hours, and each further request saves 0.675T.
//
// A nil *contextCache caches nothing; every method is safe on it.
type contextCache struct {
	client    llm.CacheClient
	ttl       time.Duration
	minTokens int // the model's smallest cacheable prompt
	now       func() time.Time

	mu      sync.Mutex
	planned map[string]int
	entries map[string]*cacheEntry
}

type cacheEntry struct {
	once    sync.Once
	cache   llm.Cache
	err     string
	model   string // the model that answered a request using it
	uses    int    // requests answered from it
	lost    bool   // the API no longer had it
	created time.Time
}

// cacheRecord is what context-cache.json says of one cache.
type cacheRecord struct {
	Name           string  `json:"name,omitempty"`
	PrefixSHA256   string  `json:"prefix_sha256"`
	Tokens         int64   `json:"tokens"`
	PlannedUses    int     `json:"planned_uses"`
	Uses           int     `json:"uses"`
	Model          string  `json:"model,omitempty"`
	PricedAs       string  `json:"priced_as,omitempty"`
	LifetimeSecond float64 `json:"lifetime_seconds"`
	CostUSD        float64 `json:"cost_usd"`
	Error          string  `json:"error,omitempty"`
	Lost           bool    `json:"lost,omitempty"`
	DeleteError    string  `json:"delete_error,omitempty"`
}

// geminiMinCacheTokens is the smallest prompt Gemini 3.x caches (the
// caching guide's table, 2026-11-17: 4,096 for every Gemini 3 and 4 model;
// 2,048 for 2.5). A smaller prefix is not worth a request to try.
const geminiMinCacheTokens = 4096

func newContextCache(c llm.Client, ttl time.Duration, now func() time.Time) *contextCache {
	cc, ok := c.(llm.CacheClient)
	if !ok || ttl <= 0 {
		return nil
	}
	return &contextCache{
		client: cc, ttl: ttl, minTokens: geminiMinCacheTokens, now: now,
		planned: map[string]int{}, entries: map[string]*cacheEntry{},
	}
}

func prefixKey(system, prefix string) string {
	h := sha256.New()
	h.Write([]byte(system))
	h.Write([]byte{0})
	h.Write([]byte(prefix))
	return hex.EncodeToString(h.Sum(nil))
}

// plan counts one request the run will send with this prefix. Every
// request is planned before the first is sent.
func (c *contextCache) plan(system, prefix string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.planned[prefixKey(system, prefix)]++
	c.mu.Unlock()
}

// get is the name of the cache holding system and prefix, creating it on
// first use, or "" when the request should send its whole prompt: a prefix
// planned once, too small, or one whose cache could not be created.
func (c *contextCache) get(ctx context.Context, system, prefix string) string {
	if c == nil || estimateTokens(system+prefix) < c.minTokens {
		return ""
	}
	key := prefixKey(system, prefix)
	c.mu.Lock()
	if c.planned[key] < 2 {
		c.mu.Unlock()
		return ""
	}
	e, ok := c.entries[key]
	if !ok {
		e = &cacheEntry{}
		c.entries[key] = e
	}
	c.mu.Unlock()
	e.once.Do(func() {
		created := c.now()
		cache, err := c.client.CreateCache(ctx, system, prefix, c.ttl)
		c.mu.Lock()
		defer c.mu.Unlock()
		if err != nil {
			e.err = describe(err)
			return
		}
		e.cache, e.created = cache, created
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.lost {
		return ""
	}
	return e.cache.Name
}

func (c *contextCache) entryNamed(name string) *cacheEntry {
	for _, e := range c.entries {
		if e.cache.Name == name {
			return e
		}
	}
	return nil
}

// served records a request answered from the cache, and by which model.
func (c *contextCache) served(name, model string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entryNamed(name); e != nil {
		e.uses++
		if model != "" {
			e.model = model
		}
	}
}

// lost records that the API no longer has the cache: later requests send
// their whole prompt.
func (c *contextCache) lost(name string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entryNamed(name); e != nil {
		e.lost = true
	}
}

// close deletes the run's caches and returns what they cost (writing and
// storage; reading them is in each request's cost) and a record of each,
// in a stable order. A cache that could not be deleted is charged its
// whole ttl. configured is the model to price a cache by when no request
// said which model served it.
func (c *contextCache) close(ctx context.Context, configured string) (usage, []cacheRecord) {
	if c == nil {
		return usage{}, nil
	}
	c.mu.Lock()
	keys := make([]string, 0, len(c.entries))
	for k := range c.entries {
		keys = append(keys, k)
	}
	c.mu.Unlock()
	sort.Strings(keys)
	var total usage
	var records []cacheRecord
	for _, k := range keys {
		c.mu.Lock()
		e := c.entries[k]
		rec := cacheRecord{
			Name: e.cache.Name, PrefixSHA256: k, Tokens: e.cache.Tokens,
			PlannedUses: c.planned[k], Uses: e.uses, Model: e.model, Error: e.err, Lost: e.lost,
		}
		c.mu.Unlock()
		if rec.Name != "" {
			lifetime := c.now().Sub(e.created)
			// Deleting must not hang on a cancelled run: it is what stops the
			// storage fee.
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			if err := c.client.DeleteCache(dctx, rec.Name); err != nil {
				rec.DeleteError = describe(err)
				lifetime = c.ttl
			}
			cancel()
			model := rec.Model
			if model == "" {
				model = configured
			}
			price, pricedAs := priceOf(model, configured, e.created, rec.Tokens)
			rec.PricedAs = pricedAs
			rec.LifetimeSecond = roundTo(lifetime.Seconds(), 1)
			rec.CostUSD = price.CacheCost(rec.Tokens, lifetime)
			total.CostUSD += rec.CostUSD
		}
		records = append(records, rec)
	}
	total.CostUSD = roundTo(total.CostUSD, 6)
	return total, records
}
