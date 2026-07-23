package retrieval

import (
	"context"

	"github.com/dirstral/dir2mcp/internal/model"
)

// Hierarchical (coarse-to-fine) retrieval — SPEC §9.7, dir2mcp #329.
//
// The flow is the RAPTOR / parent-document technique: SUMMARIES RETRIEVE, CHUNKS
// CITE. `summary` vectors live on the same text axis as the fine chunks, so the
// coarse match happens inside the existing search; this file adds only the
// EXPAND step that runs on the fused candidate pool:
//
//  1. a `summary` hit is replaced, IN PLACE (so it keeps its rank position), by
//     the fine chunks its `coverage` names — resolved by identity in the store,
//     not by a vector match, so it can cross to chunks on another axis;
//  2. a fine-chunk hit is kept as-is;
//  3. the merged pool is deduped (a chunk reached both directly and via its
//     summary appears once, at its best rank) and reranked, then truncated;
//  4. the result contains FINE CHUNKS ONLY — a summary is a routing device, and
//     one whose children are all filtered out contributes nothing.
//
// Citation faithfulness (invariant): a `summary` is model-generated prose, never
// source text, so it MUST NOT surface as a Citation.snippet or an answer quote.
// Dropping summary hits is therefore UNCONDITIONAL — it does not depend on the
// feature flag — while the expansion itself is opt-in. The Hit/Citation/Span
// shapes are untouched.

// summaryExpander is the optional store capability coarse-to-fine retrieval
// needs: resolve a `summary` chunk to the fine chunks its coverage names (SPEC
// §5.2). The retrieval service type-asserts against it, mirroring the
// LexicalSearcher / DocumentHashLister optional-capability pattern; a store that
// does not implement it yields no expansion and search behaves exactly as flat
// retrieval.
type summaryExpander interface {
	ExpandSummaryChunk(ctx context.Context, chunkID uint64) ([]model.ChunkMetadata, error)
}

// SetHierarchical toggles the opt-in coarse-to-fine expand step (SPEC §9.7).
// Wired from config.RetrievalHierarchicalEnabled; default false keeps retrieval
// on the flat path.
func (s *Service) SetHierarchical(enabled bool) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.hierarchicalEnabled = enabled
}

// expandHierarchical applies the §9.7 expand + dedup step to a fused candidate
// pool and reranks the merged pool when expansion actually added candidates.
//
// It is a no-op fast path for the overwhelmingly common case: a pool with no
// `summary` hits (every corpus with the feature off) returns the input slice
// untouched, with no store lookups and no reallocation, so flat retrieval is
// byte-identical to before this feature existed.
func (s *Service) expandHierarchical(ctx context.Context, query model.SearchQuery, hits []model.SearchHit, k int) []model.SearchHit {
	if !containsSummaryHit(hits) {
		return hits
	}
	s.metaMu.RLock()
	enabled := s.hierarchicalEnabled
	store := s.store
	s.metaMu.RUnlock()

	expander, _ := store.(summaryExpander)
	if !enabled || expander == nil {
		// The feature is off (or the store cannot expand), but a summary must never
		// be cited. Drop the coarse hits and keep the fine ones: flat retrieval.
		return dropSummaryHits(hits)
	}

	out := make([]model.SearchHit, 0, len(hits))
	seen := make(map[uint64]struct{}, len(hits))
	expanded := false
	for _, hit := range hits {
		if !model.IsSummaryRepType(hit.RepType) {
			appendUnseenHit(&out, seen, hit)
			continue
		}
		children, err := s.expandSummaryHit(ctx, expander, query, hit)
		if err != nil {
			s.logf("hierarchical: expanding summary chunk %d failed, dropping it: %v", hit.ChunkID, err)
			continue
		}
		for _, child := range children {
			// The child inherits its summary's score: the summary is what matched the
			// query, and the fine chunk is the evidence beneath it. A chunk reached
			// BOTH directly and via a summary keeps whichever score it earned at its
			// best rank (first-wins below), and the optional rerank re-scores the
			// merged pool anyway.
			child.Score = hit.Score
			if appendUnseenHit(&out, seen, child) {
				expanded = true
			}
		}
	}
	if !expanded {
		// Every summary expanded to nothing new (all children filtered out or
		// already present). The pool is the fine hits, unchanged in order, so skip
		// the extra rerank round-trip.
		return out
	}
	return s.rerankPool(ctx, query.Query, out, k)
}

// expandSummaryHit resolves one summary hit to its covered fine chunks and
// applies the query's candidate filters to them. Expansion crosses to the
// children by COVERAGE IDENTITY, so the same path/doc-type/language/date
// predicates that gate directly-retrieved candidates must gate these too —
// otherwise a summary could smuggle a filtered-out chunk into the result.
func (s *Service) expandSummaryHit(ctx context.Context, expander summaryExpander, query model.SearchQuery, hit model.SearchHit) ([]model.SearchHit, error) {
	covered, err := expander.ExpandSummaryChunk(ctx, hit.ChunkID)
	if err != nil {
		return nil, err
	}
	out := make([]model.SearchHit, 0, len(covered))
	for _, meta := range covered {
		child := meta.ToSearchHit()
		if model.IsSummaryRepType(child.RepType) {
			// Defence in depth: a summary never expands to another summary (§9.7).
			continue
		}
		if !matchFilters(child, query) {
			continue
		}
		out = append(out, child)
	}
	return out, nil
}

// containsSummaryHit reports whether the pool holds at least one `summary`
// candidate. It keeps the flat path allocation-free.
func containsSummaryHit(hits []model.SearchHit) bool {
	for _, hit := range hits {
		if model.IsSummaryRepType(hit.RepType) {
			return true
		}
	}
	return false
}

// dropSummaryHits returns the pool with every `summary` candidate removed,
// preserving the order of the survivors. It is the unconditional half of the
// citation-faithfulness invariant.
func dropSummaryHits(hits []model.SearchHit) []model.SearchHit {
	out := make([]model.SearchHit, 0, len(hits))
	for _, hit := range hits {
		if model.IsSummaryRepType(hit.RepType) {
			continue
		}
		out = append(out, hit)
	}
	return out
}

// appendUnseenHit appends hit unless its chunk has already been added, and
// reports whether it was appended. First-wins dedup: the pool is walked in rank
// order, so a chunk keeps its BEST rank whether it was reached directly or via a
// summary (SPEC §9.7 step 3). A zero chunk id carries no identity and is passed
// through without deduping so distinct un-identified candidates never collapse.
func appendUnseenHit(out *[]model.SearchHit, seen map[uint64]struct{}, hit model.SearchHit) bool {
	if hit.ChunkID == 0 {
		*out = append(*out, hit)
		return true
	}
	if _, dup := seen[hit.ChunkID]; dup {
		return false
	}
	seen[hit.ChunkID] = struct{}{}
	*out = append(*out, hit)
	return true
}
