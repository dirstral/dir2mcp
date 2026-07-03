package retrieval

import (
	"context"
	"sort"

	"github.com/dirstral/dir2mcp/internal/model"
)

// rrfK is the reciprocal-rank-fusion smoothing constant. 60 is the value
// recommended by Cormack et al. (2009) and is a de-facto default in hybrid
// retrieval implementations.
const rrfK = 60

// hybridCandidatePoolSize controls how many candidates each retriever returns
// before fusion. Larger pools improve recall when the two methods disagree
// about ranking, at the cost of additional FTS / vector work. 50 is a
// pragmatic default.
const hybridCandidatePoolSize = 50

// fuseRRF combines two ranked lists of SearchHits into a single ranked list
// using reciprocal-rank fusion. The score for a chunk that appears in either
// list at rank r is 1 / (rrfK + r); chunks appearing in both lists sum their
// per-list contributions. Output is sorted best-first and truncated to k.
//
// The function preserves the metadata (rel_path, snippet, span, etc.) of the
// first list a chunk appears in. This matters when BM25 and vector hits
// expose slightly different snippets for the same chunk; we prefer the
// vector path as the canonical source.
func fuseRRF(primary, secondary []model.SearchHit, k int) []model.SearchHit {
	return fuseRRFMulti([][]model.SearchHit{primary, secondary}, k)
}

// fuseRRFMulti combines any number of ranked SearchHit lists into a single
// ranked list using reciprocal-rank fusion — the n-ary generalization of
// fuseRRF. A chunk appearing in a list at rank r contributes 1/(rrfK+r); a
// chunk appearing in several lists sums its per-list contributions, so a chunk
// surfaced by multiple lists (e.g. retrieved for multiple language variants in
// cross-lingual expansion, #325) is boosted. The metadata of the FIRST list a
// chunk appears in is preserved (lists are processed in argument order), output
// is sorted best-first with a deterministic chunk_id tiebreak, and truncated to
// k. Empty lists are skipped. This is the shared fusion primitive for both
// hybrid BM25+vector fusion and cross-lingual per-variant fusion.
func fuseRRFMulti(lists [][]model.SearchHit, k int) []model.SearchHit {
	if k <= 0 {
		k = 10
	}
	type fused struct {
		hit   model.SearchHit
		score float64
	}
	total := 0
	for _, l := range lists {
		total += len(l)
	}
	bag := make(map[uint64]*fused, total)

	contribute := func(hit model.SearchHit, rank int) {
		entry, ok := bag[hit.ChunkID]
		if !ok {
			entry = &fused{hit: hit}
			bag[hit.ChunkID] = entry
		}
		entry.score += 1.0 / float64(rrfK+rank)
	}

	for _, list := range lists {
		for i, h := range list {
			contribute(h, i+1)
		}
	}

	out := make([]model.SearchHit, 0, len(bag))
	for _, entry := range bag {
		entry.hit.Score = entry.score
		out = append(out, entry.hit)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ChunkID < out[j].ChunkID
	})
	if len(out) > k {
		out = out[:k]
	}
	return out
}

// runHybridSearch combines BM25 lexical and dense vector candidates via
// reciprocal-rank fusion. Returns (hits, true) when hybrid was used, and
// (nil, false) when the caller should fall back to vector-only search (e.g.
// the store does not implement LexicalSearcher, or the BM25 query failed).
//
// Errors from the BM25 path are deliberately swallowed and trigger a
// vector-only fallback rather than failing the search outright. This keeps
// hybrid retrieval an optimization, not a hard dependency.
func (s *Service) runHybridSearch(
	ctx context.Context,
	query string,
	k int,
	indexKind string,
	vectorHits []model.SearchHit,
) ([]model.SearchHit, bool) {
	s.metaMu.RLock()
	enabled := s.hybridEnabled
	store := s.store
	rerankPool := s.rerankCandidatePool
	s.metaMu.RUnlock()
	if !enabled {
		return nil, false
	}
	ls, ok := store.(model.LexicalSearcher)
	if !ok {
		// Hybrid is enabled but the store cannot serve BM25 — this silently
		// degrades retrieval to vector-only (the BM25-regression class, issue
		// #399). Warn once so it is diagnosable instead of invisible, without
		// flooding the log on every query.
		if s.hybridNoLexicalWarned.CompareAndSwap(false, true) {
			s.logf("hybrid: enabled but store %T does not implement LexicalSearcher; degrading to vector-only search", store)
		}
		return nil, false
	}
	bm25Hits, err := ls.SearchBM25(ctx, query, hybridCandidatePoolSize, indexKind)
	if err != nil {
		s.logf("hybrid: BM25 search failed, falling back to vector-only: %v", err)
		return nil, false
	}
	if len(bm25Hits) == 0 {
		return vectorHits, true
	}
	// Re-attach cached metadata to BM25 hits for fields the FTS query did
	// not populate (Span, etc.). The vector path already merges metadata via
	// searchHitForLabel, so we use the same path here for parity.
	for i, h := range bm25Hits {
		cached := s.searchHitForLabel(indexKind, h.ChunkID)
		// Preserve BM25's score and snippet; pull in the better Span and any
		// other fields the lexical query left unset.
		if cached.Span.Kind != "" {
			bm25Hits[i].Span = cached.Span
		}
		if bm25Hits[i].RepType == "" {
			bm25Hits[i].RepType = cached.RepType
		}
		if bm25Hits[i].DocType == "" || bm25Hits[i].DocType == "unknown" {
			bm25Hits[i].DocType = cached.DocType
		}
	}
	// Fuse into a candidate pool sized for the downstream rerank stage, not just
	// the final k. Truncating fusion to k here would defeat the over-fetch pool
	// (#417): the reranker (rerankPool) could then only reorder the top-k and
	// never rescue a relevant chunk fused at rank k+1..pool. The caller truncates
	// to k when rerank is disabled, so the wider pool is harmless there. This
	// mirrors the HyDE path, which already fuses to hybridCandidatePoolSize.
	return fuseRRF(vectorHits, bm25Hits, rerankFusionPoolSize(k, rerankPool)), true
}

// rerankFusionPoolSize returns how many fused candidates to keep before the
// rerank stage: at least hybridCandidatePoolSize and the configured rerank
// candidate pool (falling back to the default when unset), but never fewer than
// the caller's k so a larger requested k is still honored.
func rerankFusionPoolSize(k, rerankPool int) int {
	pool := rerankPool
	if pool <= 0 {
		pool = defaultRerankCandidatePool
	}
	if pool < hybridCandidatePoolSize {
		pool = hybridCandidatePoolSize
	}
	if k > pool {
		return k
	}
	return pool
}
