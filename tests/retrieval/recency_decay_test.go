package tests

import (
	"context"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// fakeRecencyStore is a minimal model.Store that resolves a document's mtime by
// rel_path so the recency time-decay (config retrieval.recency_half_life) can be
// exercised without sqlite or credentials. A rel_path absent from mtimes
// resolves to ErrNotImplemented, modelling an undated hit.
type fakeRecencyStore struct {
	mtimes map[string]int64
}

func (f *fakeRecencyStore) Init(context.Context) error                           { return nil }
func (f *fakeRecencyStore) UpsertDocument(context.Context, model.Document) error { return nil }
func (f *fakeRecencyStore) GetDocumentByPath(_ context.Context, relPath string) (model.Document, error) {
	mt, ok := f.mtimes[relPath]
	if !ok {
		return model.Document{}, model.ErrNotImplemented
	}
	return model.Document{RelPath: relPath, MTimeUnix: mt}, nil
}
func (f *fakeRecencyStore) ListFiles(context.Context, string, string, int, int) ([]model.Document, int64, error) {
	return nil, 0, nil
}
func (f *fakeRecencyStore) Close() error { return nil }

// recencyNow is the fixed reference instant captured by the service at query
// start, so age computations are deterministic regardless of wall clock.
var recencyNow = time.Date(2026, time.June, 19, 0, 0, 0, 0, time.UTC)

// newRecencyService builds a vector-only retrieval service (the fake store does
// not implement LexicalSearcher so fusion stays off and each hit's Score is the
// raw cosine similarity) backed by a store that dates each candidate document.
//
// The two candidates have deterministic cosine scores against the query vector
// {1, 0}: chunk 1 (older) → 1.00, chunk 2 (newer) → 0.60. Without decay chunk 1
// outranks chunk 2; with a half-life shorter than chunk 1's extra age the decay
// must flip the order so the newer chunk 2 wins.
// newRecencyService is the fixed two-doc specialisation used by most recency
// tests: chunk 1 = cosine 1.00 (old.md), chunk 2 = cosine 0.60 (new.md). It
// delegates to buildRecencyService to avoid duplicating the index/store/embedder
// wiring.
func newRecencyService(t *testing.T, halfLife time.Duration, mtimes map[string]int64) *retrieval.Service {
	t.Helper()
	return buildRecencyService(t, halfLife, mtimes,
		map[uint64][]float32{1: {1, 0}, 2: {0.6, 0.8}},
		map[uint64]string{1: "old.md", 2: "new.md"})
}

func searchRecency(t *testing.T, svc *retrieval.Service) []model.SearchHit {
	t.Helper()
	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "alpha", K: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	return hits
}

func recencyChunkIDs(hits []model.SearchHit) []uint64 {
	ids := make([]uint64, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.ChunkID)
	}
	return ids
}

// TestSearch_Recency_NewerOutranksOlder pins the core behavior: chunk 1 is 365
// days old (raw score 1.00) and chunk 2 is fresh (raw score 0.60). With a
// 30-day half-life the old chunk decays by exp(-ln2*365/30) ≈ 2.2e-4 to ~0.0002
// while the fresh chunk keeps ~0.60, so the newer chunk 2 now ranks first.
func TestSearch_Recency_NewerOutranksOlder(t *testing.T) {
	mtimes := map[string]int64{
		"old.md": recencyNow.Add(-365 * 24 * time.Hour).Unix(),
		"new.md": recencyNow.Unix(),
	}
	svc := newRecencyService(t, 30*24*time.Hour, mtimes)
	got := recencyChunkIDs(searchRecency(t, svc))
	want := []uint64{2, 1}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("decay must rank newer first: want %v, got %v", want, got)
		}
	}
}

// TestSearch_Recency_DisabledIsUnchanged pins the default-off behavior: with a
// zero half-life no decay is applied, so the raw cosine order (chunk 1 then
// chunk 2) is preserved even though chunk 1 is far older.
func TestSearch_Recency_DisabledIsUnchanged(t *testing.T) {
	mtimes := map[string]int64{
		"old.md": recencyNow.Add(-365 * 24 * time.Hour).Unix(),
		"new.md": recencyNow.Unix(),
	}
	svc := newRecencyService(t, 0, mtimes)
	got := recencyChunkIDs(searchRecency(t, svc))
	want := []uint64{1, 2}
	if len(got) != len(want) {
		t.Fatalf("half-life=0 must preserve raw order; want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("half-life=0 must be pass-through: want %v, got %v", want, got)
		}
	}
}

// TestSearch_Recency_UndatedHitUnaffected pins that a hit whose source document
// has no resolvable date is neither boosted nor penalized: only the dated old
// chunk decays. Here chunk 1 (old, dated) decays below chunk 2 (undated, raw
// 0.60 kept), so the undated chunk ends up first purely because the dated one
// lost score — the undated hit's own score is untouched.
func TestSearch_Recency_UndatedHitUnaffected(t *testing.T) {
	// Only old.md is dated; new.md is absent ⇒ undated ⇒ no decay.
	mtimes := map[string]int64{
		"old.md": recencyNow.Add(-365 * 24 * time.Hour).Unix(),
	}
	svc := newRecencyService(t, 30*24*time.Hour, mtimes)
	hits := searchRecency(t, svc)
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}
	byID := map[uint64]float64{}
	for _, h := range hits {
		byID[h.ChunkID] = h.Score
	}
	// Undated chunk 2 keeps its raw cosine (~0.60) exactly.
	if got := byID[2]; got < 0.5999 || got > 0.6001 {
		t.Fatalf("undated hit score must be unchanged (~0.60), got %v", got)
	}
	// Dated chunk 1 decayed well below its raw 1.00.
	if got := byID[1]; got >= 0.5 {
		t.Fatalf("dated old hit should have decayed below 0.5, got %v", got)
	}
}

// TestSearch_Recency_FutureDatedNotAmplified pins that a hit whose mtime is in
// the future (clock skew) is clamped to age 0 (factor 1) rather than amplified:
// its score stays at the raw cosine, never above it. Chunk 1 is future-dated
// (raw 1.00) and chunk 2 is fresh (raw 0.60); the order is unchanged and chunk
// 1's score is not pushed above 1.00.
func TestSearch_Recency_FutureDatedNotAmplified(t *testing.T) {
	mtimes := map[string]int64{
		"old.md": recencyNow.Add(48 * time.Hour).Unix(), // future-dated
		"new.md": recencyNow.Unix(),
	}
	svc := newRecencyService(t, 30*24*time.Hour, mtimes)
	hits := searchRecency(t, svc)
	got := recencyChunkIDs(hits)
	want := []uint64{1, 2}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("future-dated hit must keep raw order: want %v, got %v", want, got)
		}
	}
	for _, h := range hits {
		if h.ChunkID == 1 && h.Score > 1.0001 {
			t.Fatalf("future-dated hit must not be amplified above raw 1.00, got %v", h.Score)
		}
	}
}

// buildRecencyService is a variant of newRecencyService that lets a test choose
// each candidate's raw cosine (via its stored vector) so negative-similarity and
// top-k-membership behaviors can be pinned. Query vector is {1,0}; a candidate
// vector v yields cosine = v·{1,0}/|v|.
func buildRecencyService(t *testing.T, halfLife time.Duration, mtimes map[string]int64, vecs map[uint64][]float32, paths map[uint64]string) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	for id, v := range vecs {
		addVec(t, idx, id, v)
	}
	svc := retrieval.NewService(&fakeRecencyStore{mtimes: mtimes}, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	for id, p := range paths {
		svc.SetChunkMetadata(id, model.SearchHit{RelPath: p, Snippet: "alpha " + p})
	}
	svc.SetRecencyHalfLife(halfLife)
	svc.SetNowFunc(func() time.Time { return recencyNow })
	return svc
}

// TestSearch_Recency_NegativeScoreNoInversion pins #427 (finding 2): multiplicative
// decay must not invert ordering for NEGATIVE scores. chunk 1 (raw -0.60, NEWER)
// should outrank chunk 2 (raw -1.00, OLDER). The buggy `score *= factor` pushes the
// old, very-negative chunk 2 toward 0 (≈ -0.0002), lifting it above chunk 1 — an
// inversion. The fix leaves non-positive scores untouched, preserving pure order.
func TestSearch_Recency_NegativeScoreNoInversion(t *testing.T) {
	mtimes := map[string]int64{
		"new.md": recencyNow.Unix(),                            // chunk 1: fresh
		"old.md": recencyNow.Add(-365 * 24 * time.Hour).Unix(), // chunk 2: 365d old
	}
	svc := buildRecencyService(t, 30*24*time.Hour, mtimes,
		map[uint64][]float32{
			1: {-0.6, 0.8}, // cosine -0.60 (NEWER, less negative ⇒ should rank first)
			2: {-1, 0},     // cosine -1.00 (OLDER, more negative)
		},
		map[uint64]string{1: "new.md", 2: "old.md"},
	)
	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "alpha", K: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := recencyChunkIDs(hits)
	want := []uint64{1, 2}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("negative-score decay must not invert: want %v, got %v", want, got)
	}
}

// TestSearch_Recency_DecayAffectsTopKMembership pins #427 (finding 1): decay must
// run over a wider pool BEFORE the final top-k truncation, so a fresh doc that pure
// relevance ranks at k+1 can still surface. With K=1, chunk 1 (raw 1.00, 365d old)
// is the sole pre-decay top-1; the newer chunk 2 (raw 0.60, fresh) sits at k+1.
// Before the fix, decay ran after truncation and could never promote chunk 2, so
// the result was [1]. After the fix, decay re-ranks the wider pool and chunk 2 wins.
func TestSearch_Recency_DecayAffectsTopKMembership(t *testing.T) {
	mtimes := map[string]int64{
		"old.md": recencyNow.Add(-365 * 24 * time.Hour).Unix(),
		"new.md": recencyNow.Unix(),
	}
	svc := buildRecencyService(t, 30*24*time.Hour, mtimes,
		map[uint64][]float32{
			1: {1, 0},     // cosine 1.00 (OLD) — pre-decay rank #1
			2: {0.6, 0.8}, // cosine 0.60 (NEW) — pre-decay rank #2 (k+1 when K=1)
		},
		map[uint64]string{1: "old.md", 2: "new.md"},
	)
	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "alpha", K: 1})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := recencyChunkIDs(hits)
	if len(got) != 1 {
		t.Fatalf("K=1 must return exactly 1 hit, got %v", got)
	}
	if got[0] != 2 {
		t.Fatalf("decay must promote the fresh k+1 doc into top-1: want [2], got %v", got)
	}
}
