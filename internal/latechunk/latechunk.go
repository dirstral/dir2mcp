// Package latechunk implements the opt-in "late chunking" embedding path
// (issue #332, Jina's technique). Instead of today's chunk-then-embed, the WHOLE
// document is embedded once through a long-context model to get contextually
// enriched token embeddings, then the existing chunk boundaries are applied and
// the token vectors within each chunk's rune span are mean-pooled — so each chunk
// embedding carries document-level context.
//
// The path is provider/model-dependent: it requires the configured embedder to
// expose token-level embeddings via the optional model.TokenEmbedder capability.
// No shipped provider implements that today, so Decide returns a fallback in the
// stock build and the pipeline keeps chunk-then-embed. This package contains the
// pure, deterministic, credential-free logic (capability gate + mean-pool) so a
// future self-hosted token-embedding backend plugs in, and so the behavior is
// unit-testable with a fake embedder.
//
// Wiring status (issue #446): this library is complete and tested, but the
// embedding worker does NOT yet call EmbedDocument on the active path — doing so
// needs the source document text and per-chunk rune spans that the worker's
// per-chunk tasks do not carry today, so wiring it is more than a drop-in. Until
// then the worker treats an Active decision as observability only (an honest
// "not yet wired" log) and still embeds chunk-then-embed; it does not silently
// claim the pooling path ran.
package latechunk

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/dirstral/dir2mcp/internal/model"
)

// FallbackReason explains why the late-chunking path did not run for a given
// embed call, so the caller can record the fallback (it never fails silently).
// The empty value means "no fallback — late chunking ran".
type FallbackReason string

const (
	// FallbackDisabled means ingest.late_chunking was off (the default).
	FallbackDisabled FallbackReason = "disabled"
	// FallbackNoTokenEmbedder means the configured embedder does not implement
	// model.TokenEmbedder, so token-level embeddings are unavailable. This is the
	// reason every shipped provider hits today.
	FallbackNoTokenEmbedder FallbackReason = "embedder_lacks_token_embeddings"
	// FallbackEmbedError means the token-embedding call failed at runtime, so the
	// caller must fall back to chunk-then-embed for this document.
	FallbackEmbedError FallbackReason = "token_embed_error"
)

// Decision is the outcome of the capability gate: whether the late-chunking path
// is active for the active embedder, the TokenEmbedder to use when it is, and the
// reason when it is not.
type Decision struct {
	// Active is true only when late chunking is enabled AND the embedder exposes
	// token embeddings; the caller embeds whole documents and mean-pools.
	Active bool
	// Embedder is the token embedder to use; non-nil iff Active.
	Embedder model.TokenEmbedder
	// Fallback explains why late chunking is inactive; empty iff Active.
	Fallback FallbackReason
}

// Decide resolves the late-chunking capability gate for the configured embedder.
// It is the single routing point: enabled is the resolved value of
// config.IngestLateChunking, and embedder is the active corpus embedder. The
// decision is pure (no I/O) so callers can take it once per run and log the
// fallback. A nil embedder is treated as "no token embeddings".
func Decide(enabled bool, embedder model.Embedder) Decision {
	if !enabled {
		return Decision{Active: false, Fallback: FallbackDisabled}
	}
	te, ok := embedder.(model.TokenEmbedder)
	if !ok || te == nil {
		return Decision{Active: false, Fallback: FallbackNoTokenEmbedder}
	}
	return Decision{Active: true, Embedder: te}
}

// Span is the rune-offset window of a chunk within its source document, as
// produced by the ingest chunkers (which operate on []rune). Start is inclusive,
// End is exclusive, both measured in runes of the whole-document string passed to
// EmbedDocument. Spans need not be contiguous or non-overlapping.
type Span struct {
	Start int
	End   int
}

// ErrNoTokensInSpan is returned for a chunk span that no token overlaps, so the
// caller can decide how to handle a degenerate chunk (typically: fall back to
// embedding that chunk's text directly). It is a sentinel so callers can match it
// with errors.Is.
var ErrNoTokensInSpan = errors.New("latechunk: no token embeddings overlap chunk span")

// MeanPoolSpan mean-pools the token vectors of tok whose token span overlaps the
// rune window [span.Start, span.End) and returns the resulting chunk embedding
// (issue #332). A token at [Offsets[i], Ends[i]) overlaps the window when
// Offsets[i] < span.End && Ends[i] > span.Start (half-open intersection), so a
// token straddling a chunk boundary contributes to both neighboring chunks —
// matching the overlap the char chunker already builds in. The pooled vector is
// the component-wise arithmetic mean of the contributing token vectors and has
// the provider dimension. Returns ErrNoTokensInSpan when no token overlaps (the
// caller falls back for that chunk) and an error on a malformed TokenEmbedding or
// ragged vector dimensions.
func MeanPoolSpan(tok model.TokenEmbedding, span Span) ([]float32, error) {
	if err := validateTokenEmbedding(tok); err != nil {
		return nil, err
	}
	if span.End <= span.Start {
		return nil, fmt.Errorf("latechunk: invalid span [%d,%d)", span.Start, span.End)
	}

	var (
		sum []float32
		n   int
		dim int
	)
	for i := range tok.Vectors {
		if !overlaps(tok.Offsets[i], tok.Ends[i], span.Start, span.End) {
			continue
		}
		v := tok.Vectors[i]
		if n == 0 {
			dim = len(v)
			if dim == 0 {
				return nil, fmt.Errorf("latechunk: token %d has zero-dimension vector", i)
			}
			sum = make([]float32, dim)
		} else if len(v) != dim {
			return nil, fmt.Errorf("latechunk: token %d dimension %d != %d", i, len(v), dim)
		}
		for d := 0; d < dim; d++ {
			sum[d] += v[d]
		}
		n++
	}
	if n == 0 {
		return nil, ErrNoTokensInSpan
	}
	inv := float32(1) / float32(n)
	for d := 0; d < dim; d++ {
		sum[d] *= inv
	}
	return sum, nil
}

// MeanPoolSpans applies MeanPoolSpan to each span, returning one pooled vector
// per span aligned 1:1 with spans. A span that no token overlaps yields a nil
// vector at its index (and is reported in the returned fallbackIdx slice) rather
// than aborting the whole document, so the caller can embed just those chunks the
// old way while still using context-pooled vectors for the rest. A structurally
// invalid TokenEmbedding (ragged dimensions etc.) is a hard error.
func MeanPoolSpans(tok model.TokenEmbedding, spans []Span) (vectors [][]float32, fallbackIdx []int, err error) {
	if err := validateTokenEmbedding(tok); err != nil {
		return nil, nil, err
	}
	vectors = make([][]float32, len(spans))
	for i, sp := range spans {
		v, perr := MeanPoolSpan(tok, sp)
		if errors.Is(perr, ErrNoTokensInSpan) {
			fallbackIdx = append(fallbackIdx, i)
			continue
		}
		if perr != nil {
			return nil, nil, fmt.Errorf("span %d: %w", i, perr)
		}
		vectors[i] = v
	}
	return vectors, fallbackIdx, nil
}

// EmbedDocument runs the full late-chunking path for ONE document: it embeds the
// whole document text once via the decided TokenEmbedder, then mean-pools the
// token vectors within each chunk span. It returns one chunk vector per span. The
// fallbackIdx slice lists span indices that no token overlapped (nil vector at
// that index); a runtime token-embed failure returns a FallbackEmbedError-tagged
// error so the caller embeds the whole document the old way. dec MUST be Active.
//
// Each returned chunk vector is L2-normalized to unit length (issue #446 F3): the
// pooled arithmetic mean of contextual token vectors has no fixed norm, and while
// that is harmless under cosine similarity (which normalizes internally, as the
// default HNSW index does), a vector store configured for raw inner/dot-product
// distance (a Qdrant/pgvector deployment MAY be) assumes unit-norm vectors. The
// query side already produces provider-normalized embeddings via Embed, so
// normalizing here keeps the late-chunked corpus vectors comparable to queries in
// that space too. Pooling stays in MeanPoolSpan/MeanPoolSpans as a pure arithmetic
// mean (that is the documented, unit-tested contract); normalization is applied
// only here, on the index-bound document path. A degenerate zero-magnitude pooled
// vector is returned unchanged (it cannot be normalized and, being all-zero, is an
// upstream token-embedding anomaly rather than something this step should mask).
func EmbedDocument(ctx context.Context, dec Decision, modelName string, docText string, spans []Span) (vectors [][]float32, fallbackIdx []int, err error) {
	if !dec.Active || dec.Embedder == nil {
		return nil, nil, fmt.Errorf("latechunk: EmbedDocument called with inactive decision (%s)", dec.Fallback)
	}
	toks, terr := dec.Embedder.EmbedDocumentTokens(ctx, modelName, model.EmbedDocument, []string{docText})
	if terr != nil {
		return nil, nil, fmt.Errorf("%s: %w", FallbackEmbedError, terr)
	}
	if len(toks) != 1 {
		return nil, nil, fmt.Errorf("%s: token-embedder returned %d results for 1 input", FallbackEmbedError, len(toks))
	}
	vectors, fallbackIdx, err = MeanPoolSpans(toks[0], spans)
	if err != nil {
		return nil, nil, err
	}
	for i := range vectors {
		// nil vectors are per-span fallbacks (no token overlapped); leave them nil
		// so the caller still sees which spans to embed the old way.
		if vectors[i] != nil {
			l2NormalizeInPlace(vectors[i])
		}
	}
	return vectors, fallbackIdx, nil
}

// l2NormalizeInPlace scales v to unit L2 norm in place. A zero-magnitude vector
// (no direction to preserve) is left unchanged rather than dividing by zero.
func l2NormalizeInPlace(v []float32) {
	var sumSq float64
	for _, x := range v {
		sumSq += float64(x) * float64(x)
	}
	if sumSq == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(sumSq))
	for i := range v {
		v[i] *= inv
	}
}

// IsEmbedFallback reports whether err is a runtime token-embed failure from
// EmbedDocument (FallbackEmbedError), so callers can route just that document to
// chunk-then-embed without treating structural/programming errors the same way.
func IsEmbedFallback(err error) bool {
	return err != nil && containsReason(err, FallbackEmbedError)
}

func containsReason(err error, r FallbackReason) bool {
	return err != nil && len(err.Error()) >= len(r) && stringHasPrefix(err.Error(), string(r))
}

func stringHasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// overlaps reports whether token rune window [ts,te) intersects chunk window
// [cs,ce) under half-open semantics.
func overlaps(ts, te, cs, ce int) bool {
	return ts < ce && te > cs
}

// validateTokenEmbedding checks the structural invariants of a TokenEmbedding:
// the three parallel slices have equal length and every token window is
// well-formed (Ends >= Offsets). A nil/empty embedding is valid (it simply
// overlaps no span, yielding ErrNoTokensInSpan downstream).
func validateTokenEmbedding(tok model.TokenEmbedding) error {
	if len(tok.Vectors) != len(tok.Offsets) || len(tok.Vectors) != len(tok.Ends) {
		return fmt.Errorf("latechunk: ragged token embedding (vectors=%d offsets=%d ends=%d)",
			len(tok.Vectors), len(tok.Offsets), len(tok.Ends))
	}
	for i := range tok.Offsets {
		if tok.Offsets[i] < 0 || tok.Ends[i] < tok.Offsets[i] {
			return fmt.Errorf("latechunk: token %d has invalid window [%d,%d)", i, tok.Offsets[i], tok.Ends[i])
		}
	}
	return nil
}
