package tests

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/dirstral/dir2mcp/internal/latechunk"
	"github.com/dirstral/dir2mcp/internal/model"
)

// plainEmbedder implements only model.Embedder (one pooled vector per input),
// like every shipped provider today. It never exposes token embeddings, so the
// late-chunking gate must fall back when it is the active embedder.
type plainEmbedder struct{}

func (plainEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i := range inputs {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

// fakeTokenEmbedder implements model.TokenEmbedder: it returns the canned
// per-token embeddings/offsets it is constructed with, so the mean-pool and
// routing logic can be exercised deterministically without any credentials. err,
// when set, is returned from EmbedDocumentTokens to exercise the runtime
// fallback path.
type fakeTokenEmbedder struct {
	tok model.TokenEmbedding
	err error
}

func (f fakeTokenEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i := range inputs {
		out[i] = []float32{0, 0, 0}
	}
	return out, nil
}

func (f fakeTokenEmbedder) EmbedDocumentTokens(_ context.Context, _ string, _ model.EmbedRole, inputs []string) ([]model.TokenEmbedding, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]model.TokenEmbedding, len(inputs))
	for i := range inputs {
		out[i] = f.tok
	}
	return out, nil
}

func vecsAlmostEqual(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	const eps = 1e-6
	for i := range a {
		d := a[i] - b[i]
		if d < -eps || d > eps {
			return false
		}
	}
	return true
}

// fourTokenDoc is a 4-token document over rune offsets:
//
//	tok0 [0,2)=(1,0,0)  tok1 [2,4)=(0,1,0)  tok2 [4,6)=(0,0,1)  tok3 [6,8)=(1,1,1)
func fourTokenDoc() model.TokenEmbedding {
	return model.TokenEmbedding{
		Vectors: [][]float32{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}, {1, 1, 1}},
		Offsets: []int{0, 2, 4, 6},
		Ends:    []int{2, 4, 6, 8},
	}
}

// TestMeanPoolSpan_AveragesOverlappingTokens checks the core late-chunking
// arithmetic: a span covering tokens 0..1 mean-pools (1,0,0) and (0,1,0) to
// (0.5,0.5,0).
func TestMeanPoolSpan_AveragesOverlappingTokens(t *testing.T) {
	v, err := latechunk.MeanPoolSpan(fourTokenDoc(), latechunk.Span{Start: 0, End: 4})
	if err != nil {
		t.Fatalf("MeanPoolSpan: %v", err)
	}
	if want := []float32{0.5, 0.5, 0}; !vecsAlmostEqual(v, want) {
		t.Fatalf("pooled vector = %v, want %v", v, want)
	}
}

// TestMeanPoolSpan_BoundaryTokenContributesToBothChunks verifies a token that
// straddles a chunk boundary (half-open overlap) is pooled into both neighbors,
// mirroring the overlap the char chunker builds in.
func TestMeanPoolSpan_BoundaryTokenContributesToBothChunks(t *testing.T) {
	doc := fourTokenDoc()
	// Span [3,5) overlaps tok1 [2,4) and tok2 [4,6): mean of (0,1,0),(0,0,1).
	v, err := latechunk.MeanPoolSpan(doc, latechunk.Span{Start: 3, End: 5})
	if err != nil {
		t.Fatalf("MeanPoolSpan: %v", err)
	}
	if want := []float32{0, 0.5, 0.5}; !vecsAlmostEqual(v, want) {
		t.Fatalf("boundary pooled vector = %v, want %v", v, want)
	}
}

// TestMeanPoolSpan_NoOverlapIsSentinel checks a span no token overlaps returns
// the ErrNoTokensInSpan sentinel (so callers fall back for that chunk).
func TestMeanPoolSpan_NoOverlapIsSentinel(t *testing.T) {
	_, err := latechunk.MeanPoolSpan(fourTokenDoc(), latechunk.Span{Start: 100, End: 200})
	if !errors.Is(err, latechunk.ErrNoTokensInSpan) {
		t.Fatalf("expected ErrNoTokensInSpan, got %v", err)
	}
}

// TestMeanPoolSpan_RaggedEmbeddingErrors checks structural validation: parallel
// slices of unequal length are a hard error, not a silent skip.
func TestMeanPoolSpan_RaggedEmbeddingErrors(t *testing.T) {
	bad := model.TokenEmbedding{
		Vectors: [][]float32{{1, 0}},
		Offsets: []int{0, 2},
		Ends:    []int{2},
	}
	if _, err := latechunk.MeanPoolSpan(bad, latechunk.Span{Start: 0, End: 2}); err == nil {
		t.Fatal("expected error for ragged token embedding, got nil")
	}
}

// TestMeanPoolSpans_PerSpanFallbackIndex checks that a doc with one in-range and
// one out-of-range span returns a pooled vector for the first and reports the
// second in fallbackIdx (rather than aborting the whole document).
func TestMeanPoolSpans_PerSpanFallbackIndex(t *testing.T) {
	spans := []latechunk.Span{{Start: 0, End: 2}, {Start: 100, End: 110}}
	vecs, fallbackIdx, err := latechunk.MeanPoolSpans(fourTokenDoc(), spans)
	if err != nil {
		t.Fatalf("MeanPoolSpans: %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("expected 2 vectors, got %d", len(vecs))
	}
	if vecs[0] == nil {
		t.Fatal("span 0 should have a pooled vector")
	}
	if vecs[1] != nil {
		t.Fatal("span 1 (no overlap) should have a nil vector")
	}
	if len(fallbackIdx) != 1 || fallbackIdx[0] != 1 {
		t.Fatalf("fallbackIdx = %v, want [1]", fallbackIdx)
	}
}

// TestDecide_DisabledFallsBack verifies the gate returns inactive+disabled when
// the feature is off, regardless of embedder capability.
func TestDecide_DisabledFallsBack(t *testing.T) {
	dec := latechunk.Decide(false, fakeTokenEmbedder{tok: fourTokenDoc()})
	if dec.Active {
		t.Fatal("decision should be inactive when late chunking is disabled")
	}
	if dec.Fallback != latechunk.FallbackDisabled {
		t.Fatalf("fallback = %q, want %q", dec.Fallback, latechunk.FallbackDisabled)
	}
}

// TestDecide_PlainEmbedderFallsBack verifies the gate falls back when enabled
// but the active embedder lacks token embeddings — the path every shipped
// provider hits today.
func TestDecide_PlainEmbedderFallsBack(t *testing.T) {
	dec := latechunk.Decide(true, plainEmbedder{})
	if dec.Active {
		t.Fatal("decision should be inactive for a plain (non-token) embedder")
	}
	if dec.Fallback != latechunk.FallbackNoTokenEmbedder {
		t.Fatalf("fallback = %q, want %q", dec.Fallback, latechunk.FallbackNoTokenEmbedder)
	}
}

// TestDecide_TokenEmbedderActivates verifies the gate activates (and exposes the
// token embedder) when enabled and the embedder implements model.TokenEmbedder.
func TestDecide_TokenEmbedderActivates(t *testing.T) {
	dec := latechunk.Decide(true, fakeTokenEmbedder{tok: fourTokenDoc()})
	if !dec.Active {
		t.Fatal("decision should be active for a token embedder when enabled")
	}
	if dec.Embedder == nil {
		t.Fatal("active decision must carry a non-nil token embedder")
	}
	if dec.Fallback != "" {
		t.Fatalf("active decision should have empty fallback, got %q", dec.Fallback)
	}
}

// TestDecide_NilEmbedderFallsBack verifies a nil embedder is treated as lacking
// token embeddings (no panic, clean fallback).
func TestDecide_NilEmbedderFallsBack(t *testing.T) {
	dec := latechunk.Decide(true, nil)
	if dec.Active {
		t.Fatal("decision should be inactive for a nil embedder")
	}
	if dec.Fallback != latechunk.FallbackNoTokenEmbedder {
		t.Fatalf("fallback = %q, want %q", dec.Fallback, latechunk.FallbackNoTokenEmbedder)
	}
}

// l2Norm returns the Euclidean norm of v.
func l2Norm(v []float32) float64 {
	var sumSq float64
	for _, x := range v {
		sumSq += float64(x) * float64(x)
	}
	return math.Sqrt(sumSq)
}

// TestEmbedDocument_PoolsPerSpan exercises the full active path: embed the whole
// document once via the fake token embedder, then mean-pool per chunk span. The
// index-bound document path L2-normalizes each pooled vector (issue #446 F3), so
// the expected vectors are the unit-normalized means (pooling itself, tested via
// MeanPoolSpan above, stays a pure arithmetic mean).
func TestEmbedDocument_PoolsPerSpan(t *testing.T) {
	dec := latechunk.Decide(true, fakeTokenEmbedder{tok: fourTokenDoc()})
	spans := []latechunk.Span{{Start: 0, End: 4}, {Start: 4, End: 8}}
	vecs, fallbackIdx, err := latechunk.EmbedDocument(context.Background(), dec, "fake-model", "12345678", spans)
	if err != nil {
		t.Fatalf("EmbedDocument: %v", err)
	}
	if len(fallbackIdx) != 0 {
		t.Fatalf("unexpected per-span fallbacks: %v", fallbackIdx)
	}
	// span [0,4): mean of (1,0,0),(0,1,0) = (0.5,0.5,0), normalized to unit norm.
	if want := normalize([]float32{0.5, 0.5, 0}); !vecsAlmostEqual(vecs[0], want) {
		t.Fatalf("span 0 = %v, want %v", vecs[0], want)
	}
	// span [4,8) overlaps tok2 (0,0,1) and tok3 (1,1,1): mean = (0.5,0.5,1),
	// normalized to unit norm.
	if want := normalize([]float32{0.5, 0.5, 1}); !vecsAlmostEqual(vecs[1], want) {
		t.Fatalf("span 1 = %v, want %v", vecs[1], want)
	}
	// Both index-bound vectors must be unit-norm so an inner/dot-product backend
	// (which assumes unit vectors) compares them correctly against normalized
	// query embeddings.
	for i, v := range vecs {
		if n := l2Norm(v); n < 1-1e-6 || n > 1+1e-6 {
			t.Fatalf("vec %d not unit-norm: |%v| = %v", i, v, n)
		}
	}
}

// normalize returns a unit-L2-norm copy of v (test helper mirroring the
// normalization EmbedDocument applies on the index-bound path).
func normalize(v []float32) []float32 {
	n := l2Norm(v)
	if n == 0 {
		return v
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / n)
	}
	return out
}

// TestEmbedDocument_RuntimeErrorIsFallback verifies a runtime token-embed
// failure is reported as a fallback-tagged error so the caller routes just that
// document to chunk-then-embed (never a silent failure).
func TestEmbedDocument_RuntimeErrorIsFallback(t *testing.T) {
	dec := latechunk.Decide(true, fakeTokenEmbedder{err: errors.New("backend down")})
	_, _, err := latechunk.EmbedDocument(context.Background(), dec, "fake-model", "12345678", []latechunk.Span{{Start: 0, End: 4}})
	if err == nil {
		t.Fatal("expected an error from a failing token embedder")
	}
	if !latechunk.IsEmbedFallback(err) {
		t.Fatalf("error should be classified as an embed fallback, got %v", err)
	}
}

// TestEmbedDocument_InactiveDecisionErrors verifies EmbedDocument refuses an
// inactive decision rather than silently doing nothing.
func TestEmbedDocument_InactiveDecisionErrors(t *testing.T) {
	dec := latechunk.Decide(false, fakeTokenEmbedder{tok: fourTokenDoc()})
	if _, _, err := latechunk.EmbedDocument(context.Background(), dec, "m", "12345678", []latechunk.Span{{Start: 0, End: 4}}); err == nil {
		t.Fatal("expected error when EmbedDocument is called with an inactive decision")
	}
}
