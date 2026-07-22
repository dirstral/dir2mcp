package retrieval

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"

	"github.com/dirstral/dir2mcp/internal/model"
)

// Related implements the query-by-example "more like this" retrieval of SPEC
// §15.12 (dir2mcp #324): given a seed chunk (SourceChunkID) or document
// (SourceRelPath) it ranks indexed segments by embedding similarity to the
// seed's vector(s) over the SAME vector index, applies the §9.5/§9.6 filters,
// excludes the seed itself (and — for a chunk_id request with
// ExcludeSameDocument — every chunk of the seed's document), and returns the top
// k Hits. Ordering is pure vector similarity with an ascending chunk_id tiebreak
// (the reranker does NOT apply — there is no query text). Partial-index state is
// reflected exactly as dir2mcp_search does.
//
// The seed vector is reconstructed by re-embedding the seed segment's stored
// text with the index-time embed role, so the same neighbour ranking is produced
// across every index backend without a per-backend vector-readback capability.
func (s *Service) Related(ctx context.Context, q model.RelatedQuery) (model.RelatedResult, error) {
	if s.embedder == nil {
		return model.RelatedResult{}, ErrMissingEmbedder
	}
	cs, ok := s.store.(relatedChunkStore)
	if !ok {
		return model.RelatedResult{}, model.ErrRelatedNotSupported
	}
	k := q.K
	if k <= 0 {
		k = 15
	}
	seed, err := s.resolveRelatedSource(ctx, cs, q)
	if err != nil {
		return model.RelatedResult{}, err
	}
	hits, indexUsed, err := s.relatedNeighbors(ctx, q, seed, k)
	if err != nil {
		return model.RelatedResult{}, err
	}
	indexingComplete, _ := s.IndexingComplete(ctx)
	return model.RelatedResult{
		SourceChunkID:    seed.chunkID,
		HasSourceChunkID: seed.hasChunkID,
		SourceRelPath:    seed.relPath,
		K:                k,
		IndexUsed:        indexUsed,
		Hits:             hits,
		IndexingComplete: indexingComplete,
	}, nil
}

// compile-time assertion that Service satisfies the optional RelatedSearcher
// capability the MCP dir2mcp_related tool type-asserts against.
var _ model.RelatedSearcher = (*Service)(nil)

// relatedExcludeMargin over-fetches a few extra neighbours beyond k plus the
// seed-exclusion count, so a filter drop or a same-document chunk that ranks
// inside the pool cannot starve the result below k.
const relatedExcludeMargin = 8

// relatedSeedSampleCap bounds how many of a document's chunk texts are embedded
// into a rel_path seed vector. The seed is a centroid, so a large document is
// well-approximated by its first N (chunk-id-ordered, deterministic) chunks
// without paying an unbounded embed cost. Every chunk of the document is still
// excluded from the neighbours regardless of this cap.
const relatedSeedSampleCap = 256

// relatedChunkStore is the store capability dir2mcp_related needs to resolve a
// seed segment. It is type-asserted on the service's store (mirroring the
// LexicalSearcher / DocumentHashLister optional-capability pattern); a store
// that does not implement it makes Related return ErrRelatedNotSupported.
type relatedChunkStore interface {
	ChunkTaskByID(ctx context.Context, chunkID uint64) (model.ChunkTask, string, error)
	EmbeddedChunksByPath(ctx context.Context, relPath string) ([]model.ChunkTask, error)
}

// relatedSeed is the resolved seed of a Related request: the source document, the
// axis to search, the per-axis texts to embed into the seed vector, and the set
// of chunk ids to exclude from the neighbours.
type relatedSeed struct {
	relPath     string
	chunkID     uint64
	hasChunkID  bool
	axis        string // text|code|both
	excludeIDs  map[uint64]struct{}
	textsByAxis map[string][]string
}

// resolveRelatedSource dispatches to the chunk_id or rel_path seed resolver.
func (s *Service) resolveRelatedSource(ctx context.Context, cs relatedChunkStore, q model.RelatedQuery) (*relatedSeed, error) {
	if q.SourceChunkID != 0 {
		return s.resolveRelatedChunkSeed(ctx, cs, q)
	}
	return resolveRelatedPathSeed(ctx, cs, q)
}

// resolveRelatedChunkSeed resolves a chunk_id seed: the neighbours are ranked
// against that chunk's own embedding vector. The source chunk is always excluded;
// ExcludeSameDocument additionally excludes every chunk of its document.
func (s *Service) resolveRelatedChunkSeed(ctx context.Context, cs relatedChunkStore, q model.RelatedQuery) (*relatedSeed, error) {
	task, _, err := cs.ChunkTaskByID(ctx, q.SourceChunkID)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return nil, model.ErrRelatedSourceNotFound
		}
		return nil, err
	}
	relPath := task.Metadata.RelPath
	axis := resolveRelatedAxis(q.Index, chunkAxis(task.IndexKind))
	exclude := map[uint64]struct{}{q.SourceChunkID: {}}
	if q.ExcludeSameDocument {
		docChunks, err := cs.EmbeddedChunksByPath(ctx, relPath)
		if err != nil {
			return nil, err
		}
		for _, c := range docChunks {
			exclude[c.Label] = struct{}{}
		}
	}
	texts := make(map[string][]string, 2)
	for _, ax := range activeAxes(axis) {
		texts[ax] = []string{task.Text}
	}
	return &relatedSeed{
		relPath:     relPath,
		chunkID:     q.SourceChunkID,
		hasChunkID:  true,
		axis:        axis,
		excludeIDs:  exclude,
		textsByAxis: texts,
	}, nil
}

// resolveRelatedPathSeed resolves a rel_path seed: the neighbours are ranked
// against the document's own chunk vectors, aggregated. Every chunk of the
// document is always excluded (a document is never related to itself), so
// ExcludeSameDocument is a no-op here.
func resolveRelatedPathSeed(ctx context.Context, cs relatedChunkStore, q model.RelatedQuery) (*relatedSeed, error) {
	chunks, err := cs.EmbeddedChunksByPath(ctx, q.SourceRelPath)
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, model.ErrRelatedSourceNotFound
	}
	relPath := chunks[0].Metadata.RelPath
	exclude := make(map[uint64]struct{}, len(chunks))
	textsByKind := make(map[string][]string, 2)
	countByKind := make(map[string]int, 2)
	for _, c := range chunks {
		exclude[c.Label] = struct{}{}
		ax := chunkAxis(c.IndexKind)
		textsByKind[ax] = append(textsByKind[ax], c.Text)
		countByKind[ax]++
	}
	axis := resolveRelatedAxis(q.Index, majorityAxis(countByKind))
	texts := make(map[string][]string, 2)
	for _, ax := range activeAxes(axis) {
		texts[ax] = textsByKind[ax]
	}
	return &relatedSeed{
		relPath:     relPath,
		axis:        axis,
		excludeIDs:  exclude,
		textsByAxis: texts,
	}, nil
}

// relatedNeighbors runs the pure vector nearest-neighbour retrieval for the
// resolved seed: it embeds the per-axis seed text(s), searches each active index
// axis with the §9.5/§9.6 filters, merges any per-axis pools, drops the excluded
// seed/same-document chunks, orders by descending similarity (ascending chunk_id
// tiebreak), truncates to k, and prunes tombstoned chunks — exactly as
// dir2mcp_search does for its final result set.
func (s *Service) relatedNeighbors(ctx context.Context, q model.RelatedQuery, seed *relatedSeed, k int) ([]model.SearchHit, string, error) {
	filters := model.SearchQuery{
		K:             k,
		PathPrefix:    q.PathPrefix,
		FileGlob:      q.FileGlob,
		DocTypes:      q.DocTypes,
		Languages:     q.Languages,
		LanguageMatch: q.LanguageMatch,
		DateFrom:      q.DateFrom,
		DateTo:        q.DateTo,
	}
	poolK := k + len(seed.excludeIDs) + relatedExcludeMargin
	perAxis := make([][]model.SearchHit, 0, 2)
	searched := make([]string, 0, 2)
	for _, ax := range activeAxes(seed.axis) {
		texts := seed.textsByAxis[ax]
		if len(texts) == 0 {
			continue
		}
		idx, modelName := s.indexAndModelForAxis(ax)
		if idx == nil {
			continue
		}
		vec, err := s.buildSeedVector(ctx, modelName, texts)
		if err != nil {
			return nil, "", err
		}
		if len(vec) == 0 {
			continue
		}
		cand, err := s.collectVectorCandidates(ctx, vec, idx, ax, filters, poolK)
		if err != nil {
			return nil, "", err
		}
		perAxis = append(perAxis, cand)
		searched = append(searched, ax)
	}
	merged := mergeRelatedAxes(perAxis)
	merged = excludeSeedChunks(merged, seed.excludeIDs)
	sortRelatedHits(merged)
	merged = truncateSearchHits(merged, k)
	return s.pruneTombstonedHits(ctx, merged), effectiveIndexUsed(seed.axis, searched), nil
}

// effectiveIndexUsed reports the axis actually searched (SPEC §15.12: index_used
// reflects what was searched). When "both" was requested but only one axis held
// seed vectors, the single searched axis is reported; a request that produced no
// searchable axis falls back to the requested axis so index_used stays a legal
// enum value.
func effectiveIndexUsed(requested string, searched []string) string {
	switch len(searched) {
	case 0:
		return requested
	case 1:
		return searched[0]
	default:
		return "both"
	}
}

// indexAndModelForAxis returns the physical index and embed model for an axis.
func (s *Service) indexAndModelForAxis(axis string) (model.Index, string) {
	s.metaMu.RLock()
	defer s.metaMu.RUnlock()
	if axis == "code" {
		return s.codeIndex, s.codeModel
	}
	return s.textIndex, s.textModel
}

// buildSeedVector re-embeds the seed text(s) at the index-time embed role and
// mean-pools them into a single seed vector (SPEC §15.12: a rel_path seed
// aggregates the document's chunk vectors; a chunk_id seed has a single text).
func (s *Service) buildSeedVector(ctx context.Context, modelName string, texts []string) ([]float32, error) {
	if len(texts) > relatedSeedSampleCap {
		texts = texts[:relatedSeedSampleCap]
	}
	vecs, err := s.embedder.Embed(ctx, modelName, model.EmbedDocument, texts)
	if err != nil {
		return nil, err
	}
	return meanPool(vecs), nil
}

// mergeRelatedAxes merges the per-axis candidate pools. A single-axis request
// passes its pool through unchanged; an index=both request min-max normalizes
// each axis's scores (they come from different embedding spaces) and keeps the
// best-scoring occurrence of each chunk, mirroring searchBothIndices.
func mergeRelatedAxes(perAxis [][]model.SearchHit) []model.SearchHit {
	if len(perAxis) == 0 {
		return nil
	}
	if len(perAxis) == 1 {
		return perAxis[0]
	}
	for _, hits := range perAxis {
		normalizeScores(hits)
	}
	merged := make(map[uint64]model.SearchHit)
	for _, hits := range perAxis {
		for _, h := range hits {
			if existing, ok := merged[h.ChunkID]; !ok || h.Score > existing.Score {
				merged[h.ChunkID] = h
			}
		}
	}
	out := make([]model.SearchHit, 0, len(merged))
	for _, h := range merged {
		out = append(out, h)
	}
	return out
}

// excludeSeedChunks drops the seed's excluded chunk ids from the candidate pool.
func excludeSeedChunks(hits []model.SearchHit, exclude map[uint64]struct{}) []model.SearchHit {
	out := make([]model.SearchHit, 0, len(hits))
	for _, h := range hits {
		if _, skip := exclude[h.ChunkID]; skip {
			continue
		}
		out = append(out, h)
	}
	return out
}

// sortRelatedHits orders hits by descending similarity, breaking ties by
// ascending chunk_id so repeated calls (and independent implementations) produce
// the same order (SPEC §15.12).
func sortRelatedHits(hits []model.SearchHit) {
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].ChunkID < hits[j].ChunkID
	})
}

// resolveRelatedAxis maps the requested index mode to a physical axis: an
// explicit text/code/both is honored; "auto" (and any unrecognized value) matches
// the seed's own index_kind (SPEC §15.12).
func resolveRelatedAxis(mode, seedAxis string) string {
	switch normalizeRelatedIndex(mode) {
	case "text":
		return "text"
	case "code":
		return "code"
	case "both":
		return "both"
	default: // auto
		return seedAxis
	}
}

// normalizeRelatedIndex lower-cases/trims the requested index mode, defaulting an
// empty or unrecognized value to "auto".
func normalizeRelatedIndex(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "text":
		return "text"
	case "code":
		return "code"
	case "both":
		return "both"
	default:
		return "auto"
	}
}

// chunkAxis maps a chunk's index_kind to a physical axis (anything not "code" is
// text, matching the store's default index_kind).
func chunkAxis(indexKind string) string {
	if strings.EqualFold(strings.TrimSpace(indexKind), "code") {
		return "code"
	}
	return "text"
}

// majorityAxis picks the axis with more chunks for a rel_path seed, breaking a
// tie (and the no-chunks case) toward text.
func majorityAxis(countByKind map[string]int) string {
	if countByKind["code"] > countByKind["text"] {
		return "code"
	}
	return "text"
}

// activeAxes expands an axis into the physical axes to search: "both" fans out to
// text and code; any single axis is searched alone.
func activeAxes(axis string) []string {
	if axis == "both" {
		return []string{"text", "code"}
	}
	return []string{axis}
}

// meanPool averages a set of embedding vectors into one and L2-normalizes the
// result (so a rel_path seed is the centroid of the document's chunk vectors).
// Empty and dimension-mismatched vectors are skipped; nil is returned when no
// usable vector remains.
func meanPool(vecs [][]float32) []float32 {
	dim := 0
	for _, v := range vecs {
		if len(v) > 0 {
			dim = len(v)
			break
		}
	}
	if dim == 0 {
		return nil
	}
	sum := make([]float64, dim)
	n := 0
	for _, v := range vecs {
		if len(v) != dim {
			continue
		}
		for i, x := range v {
			sum[i] += float64(x)
		}
		n++
	}
	if n == 0 {
		return nil
	}
	out := make([]float32, dim)
	for i := range sum {
		out[i] = float32(sum[i] / float64(n))
	}
	return l2Normalize(out)
}

// l2Normalize scales a vector to unit length; a zero-norm vector is returned
// unchanged (cosine ranking is scale-invariant regardless).
func l2Normalize(v []float32) []float32 {
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		return v
	}
	inv := 1.0 / math.Sqrt(norm)
	for i := range v {
		v[i] = float32(float64(v[i]) * inv)
	}
	return v
}
