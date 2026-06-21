package tests

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// textDispatchEmbedder returns a different query vector depending on the input
// text, so the HyDE transform (which embeds a generated hypothetical answer) can
// be steered to different chunks than the raw query. Any text containing the
// "hyde" marker embeds toward chunk 2; everything else embeds toward chunk 1.
type textDispatchEmbedder struct {
	rawVec  []float32
	hydeVec []float32
}

func (e *textDispatchEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, texts []string) ([][]float32, error) {
	res := make([][]float32, len(texts))
	for i, txt := range texts {
		vec := e.rawVec
		if strings.Contains(strings.ToLower(txt), "hyde-doc") {
			vec = e.hydeVec
		}
		clone := make([]float32, len(vec))
		copy(clone, vec)
		res[i] = clone
	}
	return res, nil
}

// recordingGenerator returns a fixed hypothetical document (carrying the
// "hyde-doc" marker so textDispatchEmbedder routes it to chunk 2) and records
// the prompt it was asked to generate from.
type recordingGenerator struct {
	out        string
	err        error
	calls      int
	lastPrompt string
}

func (g *recordingGenerator) Generate(_ context.Context, prompt string) (string, error) {
	g.calls++
	g.lastPrompt = prompt
	if g.err != nil {
		return "", g.err
	}
	return g.out, nil
}

// newHyDEService builds a vector-only retrieval service (nil store ⇒ no hybrid
// fusion) over two well-separated chunks. The raw query embeds to {1,0} (chunk
// 1); the generated hypothetical document embeds to {0,1} (chunk 2).
func newHyDEService(t *testing.T, gen model.Generator) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	addVec(t, idx, 2, []float32{0, 1})

	emb := &textDispatchEmbedder{rawVec: []float32{1, 0}, hydeVec: []float32{0, 1}}
	svc := retrieval.NewService(nil, idx, emb, gen)
	svc.SetChunkMetadata(1, model.SearchHit{RelPath: "raw.md", Snippet: "raw"})
	svc.SetChunkMetadata(2, model.SearchHit{RelPath: "hyde.md", Snippet: "hyde"})
	return svc
}

func hydeSearchIDs(t *testing.T, svc *retrieval.Service) []uint64 {
	t.Helper()
	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "terse keyword", Index: "text", K: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ids := make([]uint64, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.ChunkID)
	}
	return ids
}

func containsID(ids []uint64, want uint64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestSearch_HyDE_Disabled_IsRawQueryOnly pins the default-off behavior: with
// HyDE never enabled the generator is not called and only the raw-query chunk
// (1) is returned, ranked first.
func TestSearch_HyDE_Disabled_IsRawQueryOnly(t *testing.T) {
	gen := &recordingGenerator{out: "hyde-doc answer"}
	svc := newHyDEService(t, gen)

	got := hydeSearchIDs(t, svc)
	if len(got) == 0 || got[0] != 1 {
		t.Fatalf("disabled HyDE must rank the raw-query chunk first; got %v", got)
	}
	if gen.calls != 0 {
		t.Fatalf("disabled HyDE must not call the generator; calls=%d", gen.calls)
	}
}

// TestSearch_HyDE_Fuse_IncludesHypotheticalDocHits pins the fuse mode: the
// hypothetical-document chunk (2), unreachable by the raw query alone, surfaces
// alongside the raw-query chunk (1) via RRF fusion.
func TestSearch_HyDE_Fuse_IncludesHypotheticalDocHits(t *testing.T) {
	gen := &recordingGenerator{out: "this is a hyde-doc hypothetical passage"}
	svc := newHyDEService(t, gen)
	svc.SetHyDE(true, "fuse")

	got := hydeSearchIDs(t, svc)
	if gen.calls != 1 {
		t.Fatalf("HyDE fuse must call the generator exactly once; calls=%d", gen.calls)
	}
	if !containsID(got, 1) {
		t.Fatalf("fuse must keep the raw-query chunk (1); got %v", got)
	}
	if !containsID(got, 2) {
		t.Fatalf("fuse must include the hypothetical-doc chunk (2); got %v", got)
	}
	if !strings.Contains(gen.lastPrompt, "terse keyword") {
		t.Fatalf("HyDE prompt must embed the raw query; got %q", gen.lastPrompt)
	}
}

// TestSearch_HyDE_Replace_UsesHypotheticalDocAlone pins the replace mode:
// retrieval uses the hypothetical embedding alone, so the hypothetical-document
// chunk (2) ranks first (the small 2-chunk corpus still returns both via
// overfetch, but ordering reflects the embedding actually searched with).
func TestSearch_HyDE_Replace_UsesHypotheticalDocAlone(t *testing.T) {
	gen := &recordingGenerator{out: "hyde-doc replacement passage"}
	svc := newHyDEService(t, gen)
	svc.SetHyDE(true, "replace")

	got := hydeSearchIDs(t, svc)
	if len(got) == 0 || got[0] != 2 {
		t.Fatalf("replace must rank the hypothetical-doc chunk (2) first; got %v", got)
	}
}

// TestSearch_HyDE_GenerationFailure_FallsBackToRawQuery pins graceful
// degradation: when the generator errors, Search must not fail and must fall
// back to the raw-query ranking (chunk 1 first), as if HyDE were off.
func TestSearch_HyDE_GenerationFailure_FallsBackToRawQuery(t *testing.T) {
	gen := &recordingGenerator{err: errors.New("generation boom")}
	svc := newHyDEService(t, gen)
	svc.SetHyDE(true, "fuse")

	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "terse keyword", Index: "text", K: 10})
	if err != nil {
		t.Fatalf("generation failure must not fail Search: %v", err)
	}
	if len(hits) == 0 || hits[0].ChunkID != 1 {
		t.Fatalf("fallback must rank the raw-query chunk (1) first; got %v", hits)
	}
}

// TestSearch_HyDE_EmptyGeneration_FallsBackToRawQuery pins that a generator
// returning only whitespace is treated like a failure: the raw-query ranking
// (chunk 1 first) is used, not the hypothetical embedding.
func TestSearch_HyDE_EmptyGeneration_FallsBackToRawQuery(t *testing.T) {
	gen := &recordingGenerator{out: "   \n  "}
	svc := newHyDEService(t, gen)
	svc.SetHyDE(true, "replace")

	got := hydeSearchIDs(t, svc)
	if len(got) == 0 || got[0] != 1 {
		t.Fatalf("empty generation must fall back to raw-query ranking (chunk 1 first); got %v", got)
	}
}

// TestSearch_HyDE_NilGenerator_IsRawQueryOnly pins that enabling HyDE without a
// configured generator is a safe no-op (raw-query ranking), not a panic.
func TestSearch_HyDE_NilGenerator_IsRawQueryOnly(t *testing.T) {
	svc := newHyDEService(t, nil)
	svc.SetHyDE(true, "fuse")

	got := hydeSearchIDs(t, svc)
	if len(got) == 0 || got[0] != 1 {
		t.Fatalf("nil generator must fall back to raw-query ranking (chunk 1 first); got %v", got)
	}
}
