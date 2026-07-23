package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
)

// maxContextDocumentRunes bounds how much of the parent document is shown to the
// context generator. Contextual retrieval is document-aware, but a whole large
// document would blow past a chat model's window and make every per-chunk call
// expensive; the leading window carries the title/lead that situates a chunk.
// The bound is applied to the prompt only — the CACHE KEY hashes the full
// document snapshot, so a change past the cutoff still re-derives (the safe
// direction: over-derive, never serve a stale context).
const maxContextDocumentRunes = 24000

// maxGeneratedContextRunes is a defensive ceiling on what a generator may return
// for one chunk. `max_tokens` already bounds the completion on providers that
// honour it; this guards the embed input against a provider that does not, so a
// runaway completion can never dominate the chunk it is supposed to situate.
const maxGeneratedContextRunes = 2000

// ChunkContextualizer generates the per-chunk, document-aware context strings
// contextual retrieval prepends to a chunk's EMBED input (SPEC §8.1.8). It is
// the seam the representation generator holds, so contextualization can be
// exercised without a live chat provider.
type ChunkContextualizer interface {
	// Contextualize returns the context for chunkText within docText. An error is
	// FAIL-OPEN per chunk: the caller embeds that chunk raw and records
	// model.EmbeddingModeFallback, never failing ingest.
	Contextualize(ctx context.Context, docText, chunkText string) (string, error)
}

// chunkContextGenerator is the production ChunkContextualizer: it renders the
// effective prompt, calls the bound chat generator under a tight max-tokens cap,
// and caches the result content-addressed by
// (chunk content, parent-document snapshot, generator identity) per SPEC §8.6.7 —
// so an unchanged document re-scans without a single provider round-trip, and
// ANY change to those inputs re-derives.
type chunkContextGenerator struct {
	gen       model.Generator
	prompt    string
	maxTokens int
	// identity is the canonical generator-identity token (`ctx:<hash>`) that is
	// also the embed identity's terminal component. Folding it into the cache key
	// means a provider/model/max_tokens/prompt change MISSES the cache instead of
	// returning a context another generator produced.
	identity string
	cacheDir string
	logf     func(format string, args ...any)
}

// newChunkContextGenerator builds the production contextualizer from a resolved
// binding. cacheDir may be empty, which disables caching (generation still
// works) rather than failing.
func newChunkContextGenerator(gen model.Generator, binding config.ContextualBinding, cacheDir string, logf func(string, ...any)) *chunkContextGenerator {
	return &chunkContextGenerator{
		gen:       gen,
		prompt:    binding.Prompt,
		maxTokens: binding.MaxTokens,
		identity:  binding.Identity,
		cacheDir:  cacheDir,
		logf:      logf,
	}
}

// Contextualize implements ChunkContextualizer.
func (g *chunkContextGenerator) Contextualize(ctx context.Context, docText, chunkText string) (string, error) {
	if g == nil || g.gen == nil {
		return "", fmt.Errorf("contextual retrieval: no generator bound")
	}
	if strings.TrimSpace(chunkText) == "" {
		return "", nil
	}
	cachePath := g.cachePath(docText, chunkText)
	if cachePath != "" {
		if cached, err := os.ReadFile(cachePath); err == nil {
			return string(cached), nil
		}
	}

	prompt := config.RenderContextualPrompt(g.prompt, truncateRunes(docText, maxContextDocumentRunes), chunkText)
	out, err := g.generateBounded(ctx, prompt)
	if err != nil {
		return "", err
	}
	// One or two sentences on one line: collapse whitespace runs so the context
	// cannot inject blank lines into the embed input, then bound it defensively.
	out = truncateRunes(strings.Join(strings.Fields(out), " "), maxGeneratedContextRunes)
	if out == "" {
		return "", fmt.Errorf("contextual retrieval: generator returned an empty context")
	}
	g.writeCache(cachePath, out)
	return out, nil
}

// generateBounded runs the generator under the configured per-context token cap
// when it implements model.BoundedGenerator, else falls back to plain Generate —
// mirroring ingest's translate path and retrieval's boundedGenerate.
func (g *chunkContextGenerator) generateBounded(ctx context.Context, prompt string) (string, error) {
	if g.maxTokens > 0 {
		if bg, ok := g.gen.(model.BoundedGenerator); ok {
			return bg.GenerateWithMaxTokens(ctx, prompt, g.maxTokens)
		}
	}
	return g.gen.Generate(ctx, prompt)
}

// cachePath is the on-disk cache file for one (chunk, parent document,
// generator) triple, or "" when caching is disabled. The PARENT-DOCUMENT hash is
// part of the key, not only the chunk's own bytes (design 0004 §3): the context
// is document-aware, so a title edit or a change to a neighbouring section
// alters it even when the chunk's bytes are unchanged and MUST re-derive.
func (g *chunkContextGenerator) cachePath(docText, chunkText string) string {
	if strings.TrimSpace(g.cacheDir) == "" {
		return ""
	}
	key := computeContentHash([]byte(strings.Join([]string{
		computeContentHash([]byte(chunkText)),
		computeContentHash([]byte(docText)),
		g.identity,
	}, "\x00")))
	return filepath.Join(g.cacheDir, key+".txt")
}

// writeCache persists a generated context best-effort: a cache write failure
// costs a regeneration next scan, never the ingest.
func (g *chunkContextGenerator) writeCache(cachePath, contextText string) {
	if cachePath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		g.warn("contextual retrieval: create cache dir: %v", err)
		return
	}
	if err := os.WriteFile(cachePath, []byte(contextText), 0o644); err != nil {
		g.warn("contextual retrieval: write cache: %v", err)
	}
}

func (g *chunkContextGenerator) warn(format string, args ...any) {
	if g.logf != nil {
		g.logf(format, args...)
	}
}

// truncateRunes caps s at max runes (rune-safe, so a multi-byte character is
// never split). A non-positive max returns s unchanged.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
