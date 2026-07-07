package retrieval

import (
	"context"
	"strings"
	"sync"

	"github.com/dirstral/dir2mcp/internal/model"
)

// boundedGenerate runs a generation, passing maxTokens when the generator
// implements model.BoundedGenerator so a single expansion completion cannot
// fan out to an unbounded output (#444). A generator without that capability
// falls back to Generate and is bounded only by the provider's own default —
// behaviorally identical to before this change. A maxTokens <= 0 also falls
// back to Generate (the BoundedGenerator contract treats it as "use default").
func boundedGenerate(ctx context.Context, gen model.Generator, prompt string, maxTokens int) (string, error) {
	if maxTokens > 0 {
		if bg, ok := gen.(model.BoundedGenerator); ok {
			return bg.GenerateWithMaxTokens(ctx, prompt, maxTokens)
		}
	}
	return gen.Generate(ctx, prompt)
}

// expansionCacheMaxEntries bounds each of the HyDE and cross-lingual expansion
// caches. Expansions are small strings keyed on (model, query[, langs]); a few
// hundred entries covers an interactive session's repeated queries while
// keeping memory trivially bounded. On overflow the cache is cleared wholesale
// (a coarse but allocation-free eviction) rather than growing without limit.
const expansionCacheMaxEntries = 512

// expansionCache memoizes the LLM-backed query-expansion outputs — the HyDE
// hypothetical document and the cross-lingual translation variants — so an
// identical query does not re-pay the (serial, per-variant) generation cost on
// every call (#444, F4). Keys fold in the generation model so a model swap
// (hot-reload) never serves stale expansions. A nil *expansionCache is a valid
// no-op (all methods are cache-misses), so a Service constructed without one
// still works.
type expansionCache struct {
	mu    sync.Mutex
	hyde  map[string]string
	xling map[string][]string
}

func newExpansionCache() *expansionCache {
	return &expansionCache{
		hyde:  make(map[string]string),
		xling: make(map[string][]string),
	}
}

// hydeKey / xlingKey build cache keys. \x00 is a safe field separator because a
// query cannot contain a NUL byte after the callers' TrimSpace and it never
// appears in a model name or language tag.
func hydeKey(genModel, query string) string {
	return genModel + "\x00" + query
}

func xlingKey(model, query string, targets []string) string {
	return model + "\x00" + strings.Join(targets, ",") + "\x00" + query
}

func (c *expansionCache) getHyDE(genModel, query string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.hyde[hydeKey(genModel, query)]
	return v, ok
}

func (c *expansionCache) putHyDE(genModel, query, answer string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Lazy-init (nil when the cache was built as a struct literal instead of via
	// newExpansionCache) and reset once over the entry cap.
	if c.hyde == nil || len(c.hyde) >= expansionCacheMaxEntries {
		c.hyde = make(map[string]string)
	}
	c.hyde[hydeKey(genModel, query)] = answer
}

func (c *expansionCache) getVariants(model, query string, targets []string) ([]string, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.xling[xlingKey(model, query, targets)]
	if !ok {
		return nil, false
	}
	// Return a defensive copy so a caller mutating/appending the slice cannot
	// corrupt the cached value.
	return append([]string(nil), v...), true
}

func (c *expansionCache) putVariants(model, query string, targets, variants []string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Lazy-init (nil when the cache was built as a struct literal instead of via
	// newExpansionCache) and reset once over the entry cap.
	if c.xling == nil || len(c.xling) >= expansionCacheMaxEntries {
		c.xling = make(map[string][]string)
	}
	c.xling[xlingKey(model, query, targets)] = append([]string(nil), variants...)
}
