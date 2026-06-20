package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// evalDoc is one labeled corpus document loaded from testdata/corpus.json.
type evalDoc struct {
	ChunkID     uint64    `json:"chunk_id"`
	RelPath     string    `json:"rel_path"`
	DocType     string    `json:"doc_type"`
	Language    string    `json:"language"`
	ContentHash string    `json:"content_hash"`
	Vector      []float32 `json:"vector"`
	Text        string    `json:"text"`
}

// evalQuery is one labeled query loaded from testdata/queries.json.
type evalQuery struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	Vector    []float32 `json:"vector"`
	Languages []string  `json:"languages"`
	Relevant  []string  `json:"relevant"`
}

type corpusFile struct {
	Documents []evalDoc `json:"documents"`
}

type queriesFile struct {
	Queries []evalQuery `json:"queries"`
}

// evalCorpus is the loaded, validated fixture: documents plus the query set.
type evalCorpus struct {
	docs    []evalDoc
	queries []evalQuery
}

// loadCorpus reads and validates the labeled fixture corpus + query set from
// the given testdata directory. It fails fast on malformed fixtures (duplicate
// or zero chunk ids, mismatched vector dimensions, queries referencing unknown
// rel_paths) so a broken fixture surfaces as a clear test failure rather than
// silently skewing metrics.
func loadCorpus(dir string) (evalCorpus, error) {
	var corpus corpusFile
	if err := readJSON(filepath.Join(dir, "corpus.json"), &corpus); err != nil {
		return evalCorpus{}, err
	}
	var queries queriesFile
	if err := readJSON(filepath.Join(dir, "queries.json"), &queries); err != nil {
		return evalCorpus{}, err
	}
	if err := validateCorpus(corpus.Documents, queries.Queries); err != nil {
		return evalCorpus{}, err
	}
	return evalCorpus{docs: corpus.Documents, queries: queries.Queries}, nil
}

func readJSON(path string, dst any) error {
	data, err := os.ReadFile(path) //nolint:gosec // fixture path is test-controlled
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func validateCorpus(docs []evalDoc, queries []evalQuery) error {
	if len(docs) == 0 {
		return fmt.Errorf("corpus has no documents")
	}
	if len(queries) == 0 {
		return fmt.Errorf("query set is empty")
	}
	dim := len(docs[0].Vector)
	if dim == 0 {
		return fmt.Errorf("document %q has an empty vector", docs[0].RelPath)
	}
	seenID := make(map[uint64]struct{}, len(docs))
	relPaths := make(map[string]struct{}, len(docs))
	for _, d := range docs {
		if d.ChunkID == 0 {
			return fmt.Errorf("document %q has chunk_id 0 (must be > 0)", d.RelPath)
		}
		if _, dup := seenID[d.ChunkID]; dup {
			return fmt.Errorf("duplicate chunk_id %d", d.ChunkID)
		}
		seenID[d.ChunkID] = struct{}{}
		if len(d.Vector) != dim {
			return fmt.Errorf("document %q vector dim %d != %d", d.RelPath, len(d.Vector), dim)
		}
		relPaths[d.RelPath] = struct{}{}
	}
	for _, q := range queries {
		if len(q.Vector) != dim {
			return fmt.Errorf("query %q vector dim %d != %d", q.ID, len(q.Vector), dim)
		}
		if len(q.Relevant) == 0 {
			return fmt.Errorf("query %q has no relevant documents", q.ID)
		}
		for _, rp := range q.Relevant {
			if _, ok := relPaths[rp]; !ok {
				return fmt.Errorf("query %q references unknown rel_path %q", q.ID, rp)
			}
		}
	}
	return nil
}

// dictEmbedder is a deterministic, creds-free embedder for the eval harness. It
// returns the pre-registered vector for an exact query text, so retrieval is
// fully reproducible in CI with no provider calls. Unknown texts fall back to a
// zero-ish unit vector (which simply ranks nothing strongly) rather than
// erroring, keeping the harness robust to typos in fixtures.
type dictEmbedder struct {
	byText map[string][]float32
	dim    int
}

func newDictEmbedder(queries []evalQuery, dim int) *dictEmbedder {
	m := make(map[string][]float32, len(queries))
	for _, q := range queries {
		m[q.Text] = normalizeVec(q.Vector)
	}
	return &dictEmbedder{byText: m, dim: dim}
}

func (e *dictEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if v, ok := e.byText[t]; ok {
			out[i] = append([]float32(nil), v...)
			continue
		}
		fallback := make([]float32, e.dim)
		if e.dim > 0 {
			fallback[0] = 1
		}
		out[i] = fallback
	}
	return out, nil
}

// bm25Store is an in-memory model.Store that also implements LexicalSearcher
// (so the hybrid knob actually engages without sqlite/FTS5 or any creds) and
// DocumentHashLister (so cross-file dedup can group byte-identical aliases). Its
// BM25 is a deterministic term-overlap scorer: enough to make the lexical axis
// rank a document the dense axis ranks lower, exercising RRF fusion.
type bm25Store struct {
	docs []evalDoc
}

func newBM25Store(docs []evalDoc) *bm25Store {
	return &bm25Store{docs: append([]evalDoc(nil), docs...)}
}

func (s *bm25Store) Init(context.Context) error                           { return nil }
func (s *bm25Store) UpsertDocument(context.Context, model.Document) error { return nil }
func (s *bm25Store) Close() error                                         { return nil }
func (s *bm25Store) GetDocumentByPath(context.Context, string) (model.Document, error) {
	return model.Document{}, model.ErrNotImplemented
}

func (s *bm25Store) ListFiles(_ context.Context, _, _ string, _, _ int) ([]model.Document, int64, error) {
	return nil, 0, nil
}

// ListDocumentHashes feeds retrieval-time cross-file dedup (SPEC §9.2).
func (s *bm25Store) ListDocumentHashes(context.Context) ([]model.DocumentHash, error) {
	out := make([]model.DocumentHash, 0, len(s.docs))
	for _, d := range s.docs {
		out = append(out, model.DocumentHash{RelPath: d.RelPath, ContentHash: d.ContentHash})
	}
	return out, nil
}

// SearchBM25 ranks documents by term-overlap count with the query, breaking
// ties by chunk_id, and returns the top k. Higher score is better, matching the
// production sqlite adapter's negated-bm25 convention.
func (s *bm25Store) SearchBM25(_ context.Context, query string, k int, _ string) ([]model.SearchHit, error) {
	qterms := tokenize(query)
	if len(qterms) == 0 {
		return nil, nil
	}
	type scored struct {
		hit   model.SearchHit
		score float64
	}
	ranked := make([]scored, 0, len(s.docs))
	for _, d := range s.docs {
		overlap := termOverlap(qterms, tokenize(d.Text))
		if overlap == 0 {
			continue
		}
		ranked = append(ranked, scored{
			hit: model.SearchHit{
				ChunkID:  d.ChunkID,
				RelPath:  d.RelPath,
				DocType:  d.DocType,
				Snippet:  d.Text,
				Span:     model.Span{Kind: "lines"},
				Language: d.Language,
				Score:    float64(overlap),
			},
			score: float64(overlap),
		})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].hit.ChunkID < ranked[j].hit.ChunkID
	})
	if k > 0 && len(ranked) > k {
		ranked = ranked[:k]
	}
	out := make([]model.SearchHit, len(ranked))
	for i, r := range ranked {
		out[i] = r.hit
	}
	return out, nil
}

func tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	return fields
}

func termOverlap(a, b []string) int {
	set := make(map[string]struct{}, len(b))
	for _, t := range b {
		set[t] = struct{}{}
	}
	seen := make(map[string]struct{}, len(a))
	count := 0
	for _, t := range a {
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		if _, ok := set[t]; ok {
			count++
		}
	}
	return count
}

func normalizeVec(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return append([]float32(nil), v...)
	}
	norm := math.Sqrt(sum)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / norm)
	}
	return out
}

// lexicalReranker is a deterministic, creds-free model.Reranker for the eval
// harness. It rescores each candidate snippet by term overlap with the query
// (the same signal the in-memory BM25 store uses), so the rerank knob is
// genuinely exercised — and reproducibly — without any provider call. Ties keep
// the incoming order via a stable sort, and the result is best-first.
type lexicalReranker struct{}

func (lexicalReranker) Rerank(_ context.Context, _, query string, documents []string, topN int) ([]model.Reranked, error) {
	qterms := tokenize(query)
	scored := make([]model.Reranked, len(documents))
	for i, d := range documents {
		scored[i] = model.Reranked{Index: i, RelevanceScore: float64(termOverlap(qterms, tokenize(d)))}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].RelevanceScore > scored[j].RelevanceScore
	})
	if topN > 0 && len(scored) > topN {
		scored = scored[:topN]
	}
	return scored, nil
}

// knobConfig names which retrieval knobs an ablation row enables.
type knobConfig struct {
	name        string
	hybrid      bool
	rerank      bool
	crossFileDD bool
	minScore    float64 // 0 disables the floor
	useLangs    bool    // pass each query's languages filter through to Search
}

// buildService constructs a retrieval.Service over the loaded corpus with the
// knobs of cfg applied. The store is the in-memory BM25 store only when hybrid
// or cross-file dedup is requested (both rely on optional store capabilities);
// otherwise the store is nil so the vector-only path is exercised exactly as in
// the existing retrieval tests.
func (c evalCorpus) buildService(cfg knobConfig) (*retrieval.Service, error) {
	idx := index.NewHNSWIndex("")
	for _, d := range c.docs {
		payload := model.IndexPayload{
			ChunkID:  d.ChunkID,
			RelPath:  d.RelPath,
			DocType:  d.DocType,
			Language: d.Language,
		}
		if err := idx.Upsert(context.Background(), normalizeVec(d.Vector), payload); err != nil {
			return nil, fmt.Errorf("upsert fixture chunk %d (%s): %w", d.ChunkID, d.RelPath, err)
		}
	}

	var store model.Store
	if cfg.hybrid || cfg.crossFileDD {
		store = newBM25Store(c.docs)
	}

	dim := len(c.docs[0].Vector)
	svc := retrieval.NewService(store, idx, newDictEmbedder(c.queries, dim), nil)

	// hybridEnabled defaults to true in NewService; pin it explicitly both ways
	// so the vector-only rows truly bypass RRF fusion.
	svc.SetHybridEnabled(cfg.hybrid)
	svc.SetCrossFileDedupEnabled(cfg.crossFileDD)
	if cfg.crossFileDD {
		hashes, err := newBM25Store(c.docs).ListDocumentHashes(context.Background())
		if err != nil {
			return nil, fmt.Errorf("list document hashes for dedup: %w", err)
		}
		svc.SetDocumentHashes(hashes)
	}
	svc.SetMinScore(cfg.minScore)
	if cfg.rerank {
		svc.SetReranker(lexicalReranker{}, "eval-lexical", 0)
		svc.SetRerankEnabled(true)
	}

	for _, d := range c.docs {
		svc.SetChunkMetadata(d.ChunkID, model.SearchHit{
			ChunkID:  d.ChunkID,
			RelPath:  d.RelPath,
			DocType:  d.DocType,
			Snippet:  d.Text,
			Span:     model.Span{Kind: "lines", StartLine: 1, EndLine: 1},
			Language: d.Language,
		})
	}
	return svc, nil
}

// runQuery executes one labeled query against svc and returns the retrieved
// rel_paths in rank order (deduplicated, preserving first occurrence) for
// metric computation.
func (c evalCorpus) runQuery(svc *retrieval.Service, q evalQuery, cfg knobConfig, k int) ([]string, error) {
	sq := model.SearchQuery{Query: q.Text, K: k}
	if cfg.useLangs && len(q.Languages) > 0 {
		sq.Languages = append([]string(nil), q.Languages...)
	}
	hits, err := svc.Search(context.Background(), sq)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(hits))
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		if h.RelPath == "" {
			continue
		}
		if _, dup := seen[h.RelPath]; dup {
			continue
		}
		seen[h.RelPath] = struct{}{}
		out = append(out, h.RelPath)
	}
	return out, nil
}
