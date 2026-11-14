package tests

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §7.2 (spec 0.93.0): the text of a document that waits for a
// secret-policy re-evaluation must not reach a provider: not the hosted
// reranker that search runs before the final screen, and not the embedder that
// related runs on its seed. A failed pending lookup must never send the text.

type providerChunk struct {
	relPath string
	text    string
}

// providerPendingStore is the real SQLite store (documents, the pending mark)
// with chunk text served from a map. pendErr makes the pending lookup fail.
type providerPendingStore struct {
	*store.SQLiteStore
	chunks  map[uint64]providerChunk
	pendErr error
}

func (s *providerPendingStore) ChunkTaskByID(_ context.Context, chunkID uint64) (model.ChunkTask, string, error) {
	c, ok := s.chunks[chunkID]
	if !ok {
		return model.ChunkTask{}, "", model.ErrNotFound
	}
	return model.ChunkTask{Label: chunkID, Text: c.text, IndexKind: "text", Metadata: model.ChunkMetadata{ChunkID: chunkID, RelPath: c.relPath}}, "", nil
}

func (s *providerPendingStore) EmbeddedChunksByPath(_ context.Context, relPath string) ([]model.ChunkTask, error) {
	ids := make([]uint64, 0, len(s.chunks))
	for id, c := range s.chunks {
		if c.relPath == relPath {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]model.ChunkTask, 0, len(ids))
	for _, id := range ids {
		c := s.chunks[id]
		out = append(out, model.ChunkTask{Label: id, Text: c.text, IndexKind: "text", Metadata: model.ChunkMetadata{ChunkID: id, RelPath: relPath}})
	}
	return out, nil
}

func (s *providerPendingStore) SecretReevalPendingPaths(ctx context.Context, relPaths []string) (map[string]struct{}, error) {
	if s.pendErr != nil {
		return nil, s.pendErr
	}
	return s.SQLiteStore.SecretReevalPendingPaths(ctx, relPaths)
}

// recordingReranker keeps every document text it was sent, over all calls.
type recordingReranker struct {
	mu    sync.Mutex
	calls int
	docs  []string
}

func (r *recordingReranker) Rerank(_ context.Context, _ string, _ string, docs []string, _ int) ([]model.Reranked, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.docs = append(r.docs, docs...)
	out := make([]model.Reranked, len(docs))
	for i := range docs {
		out[i] = model.Reranked{Index: i, RelevanceScore: float64(len(docs) - i)}
	}
	return out, nil
}

// recordingEmbedder keeps every text it was asked to embed.
type recordingEmbedder struct {
	mu    sync.Mutex
	texts []string
}

func (e *recordingEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.texts = append(e.texts, texts...)
	e.mu.Unlock()
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

const (
	pendingHalf1    = "alpha notes for staging. GITHUB_TO"
	pendingHalf2    = "KEN=Xy7Zq9WvXy7Zq9WvXy7Zq9WvXy7Zq9WvXy7Zq9Wv"
	providerSecret  = "gamma notes GITHUB_" + "TOKEN=Xy7Zq9WvXy7Zq9WvXy7Zq9WvXy7Zq9Wv"
	providerBenign1 = "beta notes about the bridge"
	providerBenign2 = "gamma notes about the budget"
)

type providerFixture struct {
	svc      *retrieval.Service
	store    *providerPendingStore
	reranker *recordingReranker
	embedder *recordingEmbedder
}

// newProviderFixture indexes a.md (a credential split across chunks 1 and 2),
// b.md (chunk 3) and c.md (chunk 4 benign, chunk 5 with a whole credential).
// Then a broader policy change marks every document pending, and the scan
// decides b.md and c.md again, so only a.md stays pending.
func newProviderFixture(t *testing.T) *providerFixture {
	t.Helper()
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(testutil.TempDir(t), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, rel := range []string{"a.md", "b.md", "c.md"} {
		if err := st.UpsertDocument(ctx, model.Document{RelPath: rel, DocType: "md", Status: "ok", ContentHash: "h1"}); err != nil {
			t.Fatalf("UpsertDocument(%s): %v", rel, err)
		}
	}
	if _, err := st.ResetForSecretPolicy(ctx, "v2:test", []string{`BLUEHERON`}, nil, true); err != nil {
		t.Fatalf("ResetForSecretPolicy: %v", err)
	}
	for _, rel := range []string{"b.md", "c.md"} {
		if err := st.UpsertDocument(ctx, model.Document{RelPath: rel, DocType: "md", Status: "ok", ContentHash: "h2"}); err != nil {
			t.Fatalf("UpsertDocument(%s): %v", rel, err)
		}
	}
	ps := &providerPendingStore{SQLiteStore: st, chunks: map[uint64]providerChunk{
		1: {"a.md", pendingHalf1},
		2: {"a.md", pendingHalf2},
		3: {"b.md", providerBenign1},
		4: {"c.md", providerBenign2},
		5: {"c.md", providerSecret},
	}}
	idx := index.NewHNSWIndex("")
	addVecP(t, idx, 1, []float32{1, 0}, "a.md", "md")
	addVecP(t, idx, 2, []float32{0.97, 0.03}, "a.md", "md")
	addVecP(t, idx, 3, []float32{0.95, 0.05}, "b.md", "md")
	addVecP(t, idx, 4, []float32{0.9, 0.1}, "c.md", "md")
	addVecP(t, idx, 5, []float32{0.85, 0.15}, "c.md", "md")
	emb := &recordingEmbedder{}
	svc := retrieval.NewService(ps, idx, emb, nil)
	for id, c := range ps.chunks {
		// The snippet is a clean prefix, so only the full text holds the
		// credential.
		snippet := strings.SplitN(c.text, " ", 2)[0]
		svc.SetChunkMetadata(id, model.SearchHit{ChunkID: id, RelPath: c.relPath, DocType: "md", Snippet: snippet})
	}
	rr := &recordingReranker{}
	svc.SetReranker(rr, "m", 50)
	svc.SetRerankEnabled(true)
	return &providerFixture{svc: svc, store: ps, reranker: rr, embedder: emb}
}

func requireNoPendingText(t *testing.T, where string, texts []string) {
	t.Helper()
	for _, text := range texts {
		for _, part := range []string{pendingHalf1, pendingHalf2, providerSecret} {
			if strings.Contains(text, part) {
				t.Fatalf("%s received %q", where, part)
			}
		}
	}
}

func TestSecretPending_RerankerNeverSeesPendingOrSecretText(t *testing.T) {
	f := newProviderFixture(t)
	hits, err := f.svc.Search(context.Background(), model.SearchQuery{Query: "notes", K: 5, Index: "text"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	f.reranker.mu.Lock()
	docs := append([]string(nil), f.reranker.docs...)
	calls := f.reranker.calls
	f.reranker.mu.Unlock()
	if calls == 0 {
		t.Fatal("the reranker was not called; the test proves nothing")
	}
	requireNoPendingText(t, "the reranker", docs)
	joined := strings.Join(docs, "\n")
	for _, want := range []string{providerBenign1, providerBenign2} {
		if !strings.Contains(joined, want) {
			t.Errorf("the reranker did not receive the clean candidate %q", want)
		}
	}
	for _, hit := range hits {
		if hit.RelPath == "a.md" || hit.ChunkID == 5 {
			t.Errorf("search returned chunk %d of %s", hit.ChunkID, hit.RelPath)
		}
	}
}

func TestSecretPending_FailedLookupSendsNothingToTheReranker(t *testing.T) {
	f := newProviderFixture(t)
	f.store.pendErr = errors.New("pending lookup failed")
	if _, err := f.svc.Search(context.Background(), model.SearchQuery{Query: "notes", K: 5, Index: "text"}); err == nil {
		t.Fatal("Search with a failed pending lookup returned no error")
	}
	f.reranker.mu.Lock()
	defer f.reranker.mu.Unlock()
	if f.reranker.calls != 0 || len(f.reranker.docs) != 0 {
		t.Fatalf("the reranker was called %d time(s) with %d text(s) while the pending lookup failed", f.reranker.calls, len(f.reranker.docs))
	}
}

func TestSecretPending_RelatedRejectsAPendingSeed(t *testing.T) {
	f := newProviderFixture(t)
	ctx := context.Background()
	for name, q := range map[string]model.RelatedQuery{
		"chunk_id seed": {SourceChunkID: 1, K: 3, Index: "text", ExcludeSameDocument: true},
		"rel_path seed": {SourceRelPath: "a.md", K: 3, Index: "text"},
	} {
		if _, err := f.svc.Related(ctx, q); !errors.Is(err, model.ErrForbidden) {
			t.Errorf("%s of a pending document: err = %v, want ErrForbidden", name, err)
		}
	}
	f.embedder.mu.Lock()
	embedded := append([]string(nil), f.embedder.texts...)
	f.embedder.mu.Unlock()
	if len(embedded) != 0 {
		t.Fatalf("the embedder received %d seed text(s) of a pending document", len(embedded))
	}

	// A decided seed still works.
	if _, err := f.svc.Related(ctx, model.RelatedQuery{SourceRelPath: "b.md", K: 3, Index: "text"}); err != nil {
		t.Fatalf("related on a decided seed: %v", err)
	}
	f.embedder.mu.Lock()
	embedded = append([]string(nil), f.embedder.texts...)
	f.embedder.mu.Unlock()
	requireNoPendingText(t, "the embedder", embedded)
	if len(embedded) == 0 {
		t.Fatal("the embedder received nothing for a decided seed")
	}
}

func TestSecretPending_RelatedFailedLookupEmbedsNothing(t *testing.T) {
	f := newProviderFixture(t)
	f.store.pendErr = errors.New("pending lookup failed")
	for name, q := range map[string]model.RelatedQuery{
		"chunk_id seed": {SourceChunkID: 3, K: 3, Index: "text", ExcludeSameDocument: true},
		"rel_path seed": {SourceRelPath: "b.md", K: 3, Index: "text"},
	} {
		if _, err := f.svc.Related(context.Background(), q); err == nil {
			t.Errorf("%s with a failed pending lookup returned no error", name)
		}
	}
	f.embedder.mu.Lock()
	defer f.embedder.mu.Unlock()
	if len(f.embedder.texts) != 0 {
		t.Fatalf("the embedder received %d text(s) while the pending lookup failed", len(f.embedder.texts))
	}
}
