package retrieval

import (
	"context"
	"sort"
	"strings"

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
//
// filters are the query's candidate predicates. They MUST be applied to the
// lexical candidates too (issue #856): the vector path filters its own
// candidates in collectFilteredHits, so a filter evaluated only there was
// undone here — fusion added back candidates no predicate had judged, and a
// filter that selected nothing still returned the whole BM25 pool.
func (s *Service) runHybridSearch(
	ctx context.Context,
	query string,
	k int,
	indexKind string,
	filters model.SearchQuery,
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
	lexicalHits := s.selectLexicalCandidates(indexKind, bm25Hits, filters)
	if len(lexicalHits) == 0 {
		// Every lexical candidate was filtered out. Return the vector candidates
		// alone, exactly as an empty BM25 result does above.
		return vectorHits, true
	}
	// Fuse into a candidate pool sized for the downstream rerank stage, not just
	// the final k. Truncating fusion to k here would defeat the over-fetch pool
	// (#519): the reranker (rerankPool) could then only reorder the top-k and
	// never rescue a relevant chunk fused at rank k+1..pool. The caller truncates
	// to k when rerank is disabled, so the wider pool is harmless there. This
	// mirrors the HyDE path, which already fuses to hybridCandidatePoolSize.
	return fuseRRF(vectorHits, lexicalHits, rerankFusionPoolSize(k, rerankPool)), true
}

// selectLexicalCandidates hydrates each BM25 candidate from the in-memory chunk
// metadata and then keeps only the candidates the query's predicates admit. It
// is the lexical half of candidate selection, and the counterpart of
// collectFilteredHits on the vector side.
//
// Order matters: hydrate first, filter second. matchFilters judges a candidate
// on the metadata the candidate carries, so a lexical retriever that returns no
// span, language or mtime would otherwise be judged against absent values and
// every one of its hits would be dropped by an active filter. Hydration is a
// map lookup per candidate against metadata the service already holds, so it
// adds no query and no I/O.
//
// The candidate pool is the BM25 top-N for the query text, filtered. A highly
// selective filter may therefore leave the lexical contribution empty, which
// costs no recall relative to a vector-only search: the vector path runs its own
// widening overfetch loop against the same filter.
func (s *Service) selectLexicalCandidates(
	indexKind string,
	bm25Hits []model.SearchHit,
	filters model.SearchQuery,
) []model.SearchHit {
	out := make([]model.SearchHit, 0, len(bm25Hits))
	for _, hit := range bm25Hits {
		hydrated := s.hydrateLexicalHit(indexKind, hit)
		if !matchFilters(hydrated, filters) {
			continue
		}
		out = append(out, hydrated)
	}
	return out
}

// hydrateLexicalHit re-attaches cached metadata to one BM25 hit for the fields
// the lexical query did not populate. The vector path merges metadata via
// searchHitForLabel, so the same source is used here for parity.
//
// BM25's own score and snippet are preserved. The span, the representation and
// document types, the recorded language and the document's calendar anchor are
// pulled from the cache when the lexical hit lacks them; those last two are what
// the language (SPEC §9.5) and date-window (§9.6) predicates read, and the span
// is what the speaker (§8.6.8), media time-window (§9.8) and recognition
// entity/event (design 0004 §7) predicates read.
func (s *Service) hydrateLexicalHit(indexKind string, hit model.SearchHit) model.SearchHit {
	cached := s.searchHitForLabel(indexKind, hit.ChunkID)
	hit.Span = resolveLexicalSpan(hit.Span, cached.Span)
	if hit.RepType == "" {
		hit.RepType = cached.RepType
	}
	if hit.DocType == "" || hit.DocType == "unknown" {
		hit.DocType = cached.DocType
	}
	if strings.TrimSpace(hit.Language) == "" {
		hit.Language = cached.Language
	}
	if hit.MTimeUnix == 0 {
		hit.MTimeUnix = cached.MTimeUnix
	}
	return hit
}

// resolveLexicalSpan merges a lexical hit's span with the cached one.
//
// The cached span wins when it exists, as it always has: it is the span row the
// vector path cites, so a chunk localizes to the same place whichever retriever
// found it. One exception keeps the filter honest: when the cached span carries
// no recognition attribution and the lexical span does, the attribution is
// carried across. A candidate must never be judged against attribution it does
// not have but its chunk does, because a non-empty filter rejects an
// attribution-less span by design and the hit would be dropped for the wrong
// reason (issue #856).
func resolveLexicalSpan(lexical, cached model.Span) model.Span {
	if cached.Kind == "" {
		return lexical
	}
	resolved := cached
	if !spanHasAttribution(resolved) && spanHasAttribution(lexical) {
		resolved.Entities = lexical.Entities
		resolved.Event = lexical.Event
	}
	return resolved
}

// spanHasAttribution reports whether a span carries a recognition annotation's
// entity ids or event (design 0004 §7).
func spanHasAttribution(span model.Span) bool {
	return len(span.Entities) > 0 || strings.TrimSpace(span.Event) != ""
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
