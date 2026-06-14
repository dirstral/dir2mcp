package tests

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/qdrant/go-client/qdrant"

	"github.com/dirstral/dir2mcp/internal/index/qdrantindex"
	"github.com/dirstral/dir2mcp/internal/model"
)

// stubQdrant is an in-memory implementation of qdrantindex.Client. It records
// the requests the Index issues so the tests can assert the filter/payload
// mapping without any network access.
type stubQdrant struct {
	collExists bool
	created    bool
	deletedCol bool

	points map[uint64]map[string]*qdrant.Value

	lastQuery  *qdrant.QueryPoints
	lastUpsert *qdrant.UpsertPoints
	lastDelete *qdrant.DeletePoints

	// queryResult, when set, is returned verbatim from Query (ignoring points).
	queryResult []*qdrant.ScoredPoint

	closed bool
}

func newStubQdrant() *stubQdrant {
	return &stubQdrant{points: map[uint64]map[string]*qdrant.Value{}}
}

func (s *stubQdrant) CollectionExists(_ context.Context, _ string) (bool, error) {
	return s.collExists, nil
}

func (s *stubQdrant) CreateCollection(_ context.Context, _ *qdrant.CreateCollection) error {
	s.created = true
	s.collExists = true
	return nil
}

func (s *stubQdrant) DeleteCollection(_ context.Context, _ string) error {
	s.deletedCol = true
	s.collExists = false
	s.points = map[uint64]map[string]*qdrant.Value{}
	return nil
}

func (s *stubQdrant) CreateFieldIndex(_ context.Context, _ *qdrant.CreateFieldIndexCollection) (*qdrant.UpdateResult, error) {
	return &qdrant.UpdateResult{}, nil
}

func (s *stubQdrant) Upsert(_ context.Context, req *qdrant.UpsertPoints) (*qdrant.UpdateResult, error) {
	s.lastUpsert = req
	for _, p := range req.GetPoints() {
		s.points[p.GetId().GetNum()] = p.GetPayload()
	}
	return &qdrant.UpdateResult{}, nil
}

func (s *stubQdrant) Delete(_ context.Context, req *qdrant.DeletePoints) (*qdrant.UpdateResult, error) {
	s.lastDelete = req
	for _, id := range req.GetPoints().GetPoints().GetIds() {
		delete(s.points, id.GetNum())
	}
	return &qdrant.UpdateResult{}, nil
}

func (s *stubQdrant) Query(_ context.Context, req *qdrant.QueryPoints) ([]*qdrant.ScoredPoint, error) {
	s.lastQuery = req
	if s.queryResult != nil {
		return s.queryResult, nil
	}
	out := make([]*qdrant.ScoredPoint, 0, len(s.points))
	for id, payload := range s.points {
		out = append(out, &qdrant.ScoredPoint{
			Id:      qdrant.NewIDNum(id),
			Score:   1,
			Payload: payload,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetId().GetNum() < out[j].GetId().GetNum() })
	return out, nil
}

func (s *stubQdrant) Get(_ context.Context, req *qdrant.GetPoints) ([]*qdrant.RetrievedPoint, error) {
	out := []*qdrant.RetrievedPoint{}
	for _, id := range req.GetIds() {
		if payload, ok := s.points[id.GetNum()]; ok {
			out = append(out, &qdrant.RetrievedPoint{Id: id, Payload: payload})
		}
	}
	return out, nil
}

func (s *stubQdrant) Close() error { s.closed = true; return nil }

func newStubIndex(t *testing.T, s *stubQdrant) *qdrantindex.Index {
	t.Helper()
	idx, err := qdrantindex.NewWithClient(s, qdrantindex.Config{Collection: "test"})
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	return idx
}

func TestQdrant_NewWithClient_Validation(t *testing.T) {
	if _, err := qdrantindex.NewWithClient(nil, qdrantindex.Config{Collection: "c"}); err == nil {
		t.Fatal("expected error for nil client")
	}
	if _, err := qdrantindex.NewWithClient(newStubQdrant(), qdrantindex.Config{}); err == nil {
		t.Fatal("expected error for empty collection")
	}
}

func TestQdrant_UpsertCreatesCollectionAndRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := newStubQdrant()
	idx := newStubIndex(t, s)

	in := model.IndexPayload{
		ChunkID:  7,
		RelPath:  "docs/a.md",
		DocType:  "MD",
		RepType:  "ocr",
		Modality: "text",
		Title:    "A",
		StartMS:  100,
		EndMS:    200,
		Language: "en",
		Speaker:  "host",
		Snippet:  "hello",
		MediaRef: "media/a.mp3",
	}
	if err := idx.Upsert(ctx, []float32{1, 0, 0}, in); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if !s.created {
		t.Fatal("expected collection to be created on first upsert")
	}

	hits, err := idx.Search(ctx, []float32{1, 0, 0}, 10, model.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	got := hits[0].Payload
	// Span is intentionally not persisted in Qdrant, so it does not round-trip;
	// every other field must.
	in.Span = model.Span{}
	if got != in {
		t.Fatalf("payload did not round-trip: got %+v want %+v", got, in)
	}
}

func TestQdrant_UpsertRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	idx := newStubIndex(t, newStubQdrant())
	if err := idx.Upsert(ctx, nil, model.IndexPayload{ChunkID: 1}); err == nil {
		t.Fatal("expected error for empty vector")
	}
	if err := idx.Upsert(ctx, []float32{1}, model.IndexPayload{ChunkID: 0}); err == nil {
		t.Fatal("expected error for zero chunk id")
	}
}

func TestQdrant_DeleteAndSearchBeforeReadyAreNoops(t *testing.T) {
	ctx := context.Background()
	s := newStubQdrant()
	idx := newStubIndex(t, s)

	// Delete before any collection exists is a silent no-op.
	if err := idx.Delete(ctx, []uint64{1}); err != nil {
		t.Fatalf("Delete (not ready): %v", err)
	}
	if s.lastDelete != nil {
		t.Fatal("Delete should not hit the backend before the collection is ready")
	}

	// Search before ready returns no hits without querying.
	hits, err := idx.Search(ctx, []float32{1}, 5, model.Filter{})
	if err != nil {
		t.Fatalf("Search (not ready): %v", err)
	}
	if len(hits) != 0 || s.lastQuery != nil {
		t.Fatal("Search should be a no-op before the collection is ready")
	}
}

func TestQdrant_Delete(t *testing.T) {
	ctx := context.Background()
	s := newStubQdrant()
	idx := newStubIndex(t, s)
	if err := idx.Upsert(ctx, []float32{1, 0}, model.IndexPayload{ChunkID: 1, RelPath: "a"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := idx.Upsert(ctx, []float32{0, 1}, model.IndexPayload{ChunkID: 2, RelPath: "b"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := idx.Delete(ctx, []uint64{1}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	hits, err := idx.Search(ctx, []float32{1, 0}, 10, model.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ChunkID != 2 {
		t.Fatalf("expected only chunk 2 to remain, got %+v", chunkIDs(hits))
	}
}

// --- Filter mapping coverage ---

func TestQdrant_CanFilter(t *testing.T) {
	idx := newStubIndex(t, newStubQdrant())
	cases := []struct {
		name string
		f    model.Filter
		want bool
	}{
		{"zero", model.Filter{}, true},
		{"doctypes", model.Filter{DocTypes: []string{"md"}}, true},
		{"orphans", model.Filter{ExcludeOrphans: true}, true},
		{"doctypes+orphans", model.Filter{DocTypes: []string{"md"}, ExcludeOrphans: true}, true},
		{"path_prefix", model.Filter{PathPrefix: "docs/"}, false},
		{"path_glob", model.Filter{PathGlob: "docs/*.md"}, false},
		{"prefix+doctypes", model.Filter{PathPrefix: "docs/", DocTypes: []string{"md"}}, false},
	}
	for _, tc := range cases {
		if got := idx.CanFilter(tc.f); got != tc.want {
			t.Errorf("CanFilter(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// fieldMatches extracts keyword-match conditions keyed by field from a filter's
// Must clause.
func mustMatchKeywords(f *qdrant.Filter) map[string][]string {
	out := map[string][]string{}
	if f == nil {
		return out
	}
	for _, c := range f.GetMust() {
		if fc := c.GetField(); fc != nil {
			out[fc.GetKey()] = fc.GetMatch().GetKeywords().GetStrings()
		}
	}
	return out
}

func mustNotEmptyFields(f *qdrant.Filter) []string {
	var out []string
	if f == nil {
		return out
	}
	for _, c := range f.GetMustNot() {
		if ie := c.GetIsEmpty(); ie != nil {
			out = append(out, ie.GetKey())
		}
	}
	return out
}

func TestQdrant_PushesDocTypeFilterDown(t *testing.T) {
	ctx := context.Background()
	s := newStubQdrant()
	idx := newStubIndex(t, s)
	if err := idx.Upsert(ctx, []float32{1, 0}, model.IndexPayload{ChunkID: 1, RelPath: "a", DocType: "md"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// DocTypes are matched case-insensitively via the lower-cased doc_type_lc field.
	if _, err := idx.Search(ctx, []float32{1, 0}, 5, model.Filter{DocTypes: []string{"MD", " Pdf ", "MD"}}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	matches := mustMatchKeywords(s.lastQuery.GetFilter())
	got := matches["doc_type_lc"]
	want := []string{"md", "pdf"}
	if len(got) != len(want) {
		t.Fatalf("doc_type_lc keywords = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("doc_type_lc keywords = %v, want %v (normalized/deduped)", got, want)
		}
	}
}

func TestQdrant_PushesExcludeOrphansDown(t *testing.T) {
	ctx := context.Background()
	s := newStubQdrant()
	idx := newStubIndex(t, s)
	if err := idx.Upsert(ctx, []float32{1, 0}, model.IndexPayload{ChunkID: 1, RelPath: "a", DocType: "md"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := idx.Search(ctx, []float32{1, 0}, 5, model.Filter{ExcludeOrphans: true}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := mustNotEmptyFields(s.lastQuery.GetFilter())
	if len(got) != 1 || got[0] != "rel_path" {
		t.Fatalf("expected MustNot is_empty(rel_path), got %v", got)
	}
}

func TestQdrant_PathFilterNotPushedDown(t *testing.T) {
	ctx := context.Background()
	s := newStubQdrant()
	idx := newStubIndex(t, s)
	if err := idx.Upsert(ctx, []float32{1, 0}, model.IndexPayload{ChunkID: 1, RelPath: "docs/a.md", DocType: "md"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// PathPrefix declines push-down, so no Qdrant filter is emitted (retrieval
	// re-applies it in Go).
	if _, err := idx.Search(ctx, []float32{1, 0}, 5, model.Filter{PathPrefix: "docs/"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if s.lastQuery.GetFilter() != nil {
		t.Fatalf("expected nil filter for path-prefix (declined push-down), got %+v", s.lastQuery.GetFilter())
	}
}

// --- Identity / Reset lifecycle ---

func TestQdrant_ResetThenIdentityRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := newStubQdrant()
	idx := newStubIndex(t, s)

	const identity = "mistral/mistral-embed:1024"
	if err := idx.Reset(ctx, identity); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	// Reset defers collection (re)creation to the next Upsert; identity is
	// written into the sentinel then.
	if err := idx.Upsert(ctx, []float32{1, 0}, model.IndexPayload{ChunkID: 1, RelPath: "a", DocType: "md"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// A fresh Index over the same backend reads the identity from the sentinel.
	idx2 := newStubIndex(t, s)
	got, err := idx2.Identity(ctx)
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if got != identity {
		t.Fatalf("Identity = %q, want %q", got, identity)
	}

	// The identity sentinel is never surfaced as a search hit.
	hits, err := idx2.Search(ctx, []float32{1, 0}, 10, model.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ChunkID != 1 {
		t.Fatalf("identity sentinel leaked into search results: %v", chunkIDs(hits))
	}
}

func TestQdrant_IdentityEmptyWhenNoCollection(t *testing.T) {
	ctx := context.Background()
	idx := newStubIndex(t, newStubQdrant())
	got, err := idx.Identity(ctx)
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if got != "" {
		t.Fatalf("Identity = %q, want empty for fresh index", got)
	}
}

func TestQdrant_Close(t *testing.T) {
	s := newStubQdrant()
	idx := newStubIndex(t, s)
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !s.closed {
		t.Fatal("expected underlying client to be closed")
	}
}

// --- Open / dial URL validation (no network for the error paths) ---

func TestQdrant_OpenRejectsBadURL(t *testing.T) {
	ctx := context.Background()
	cases := []string{
		"",                 // missing url
		"ftp://localhost",  // unsupported scheme
		"http://:6334",     // missing host
		"http://host:nota", // invalid port
	}
	for _, raw := range cases {
		if _, err := qdrantindex.Open(ctx, qdrantindex.BackendConfig{URL: raw}); err == nil {
			t.Errorf("Open(%q) expected error, got nil", raw)
		}
	}
}

// satisfy the model.Index contract at compile time from the test side too.
var _ model.Index = (*qdrantindex.Index)(nil)

func TestQdrant_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	idx := newStubIndex(t, newStubQdrant())
	if err := idx.Upsert(ctx, []float32{1}, model.IndexPayload{ChunkID: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Upsert with canceled ctx = %v, want context.Canceled", err)
	}
	if _, err := idx.Search(ctx, []float32{1}, 1, model.Filter{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Search with canceled ctx = %v, want context.Canceled", err)
	}
}
