package tests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §7.2 (spec 0.93.0): until the re-evaluation pass has read a document
// again under a BROADER secret policy, no tool returns any part of it. A
// credential can cross a chunk boundary, and then no single excerpt matches,
// so the excerpt screen alone would serve both halves. A change that can only
// match less needs no pass and hides nothing.

// pendingChunkStore is the real SQLite store (documents, the pending mark)
// with the chunk text served from a map, so a test controls how a credential
// is split across chunks.
type pendingChunkStore struct {
	*store.SQLiteStore
	text map[uint64]string
}

func (s *pendingChunkStore) ChunkTaskByID(_ context.Context, chunkID uint64) (model.ChunkTask, string, error) {
	text, ok := s.text[chunkID]
	if !ok {
		return model.ChunkTask{}, "", model.ErrNotFound
	}
	return model.ChunkTask{Label: chunkID, Text: text, Metadata: model.ChunkMetadata{ChunkID: chunkID}}, "", nil
}

const splitKeyHalf2 = "KEN=Xy7Zq9WvXy7Zq9WvXy7Zq9WvXy7Zq9WvXy7Zq9Wv"

type pendingFixture struct {
	svc   *retrieval.Service
	store *store.SQLiteStore
	gen   *fakeGenerator
	root  string
}

// newPendingFixture indexes two documents. split.md holds a credential split
// across chunks 1 and 2: neither chunk matches on its own. clean.md (chunk 3)
// holds nothing. Both rows are `ok` with a content hash.
func newPendingFixture(t *testing.T) *pendingFixture {
	t.Helper()
	ctx := context.Background()
	root := testutil.TempDir(t)
	if err := os.WriteFile(filepath.Join(root, "clean.md"), []byte("clean deployment notes\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "split.md"), []byte("staging notes\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	st := store.NewSQLiteStore(filepath.Join(testutil.TempDir(t), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, rel := range []string{"split.md", "clean.md"} {
		if err := st.UpsertDocument(ctx, model.Document{RelPath: rel, DocType: "md", Status: "ok", ContentHash: "h1"}); err != nil {
			t.Fatalf("UpsertDocument(%s): %v", rel, err)
		}
	}
	chunks := &pendingChunkStore{SQLiteStore: st, text: map[uint64]string{
		1: "deployment notes for staging. GITHUB_TO",
		2: splitKeyHalf2,
		3: "clean deployment notes without any credential.",
	}}
	idx := index.NewHNSWIndex("")
	addVecP(t, idx, 1, []float32{1, 0}, "split.md", "md")
	addVecP(t, idx, 2, []float32{0.95, 0.05}, "split.md", "md")
	addVecP(t, idx, 3, []float32{0.9, 0.1}, "clean.md", "md")
	gen := &fakeGenerator{out: "notes [clean.md]"}
	svc := retrieval.NewService(chunks, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	svc.SetRootDir(root)
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "split.md", DocType: "md", Snippet: "deployment notes for staging"})
	svc.SetChunkMetadata(2, model.SearchHit{ChunkID: 2, RelPath: "split.md", DocType: "md", Snippet: "KEN="})
	svc.SetChunkMetadata(3, model.SearchHit{ChunkID: 3, RelPath: "clean.md", DocType: "md", Snippet: "clean deployment notes"})
	return &pendingFixture{svc: svc, store: st, gen: gen, root: root}
}

func (f *pendingFixture) searchIDs(t *testing.T) string {
	t.Helper()
	hits, err := f.svc.Search(context.Background(), model.SearchQuery{Query: "deployment notes", K: 5, Index: "text"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	return fmt.Sprint(hitIDs(hits))
}

func (f *pendingFixture) reset(t *testing.T, broader bool) {
	t.Helper()
	if _, err := f.store.ResetForSecretPolicy(context.Background(), "v2:test", []string{`BLUEHERON`}, nil, broader); err != nil {
		t.Fatalf("ResetForSecretPolicy: %v", err)
	}
}

func TestSecretPending_BroaderSetHidesTheWholeDocument(t *testing.T) {
	f := newPendingFixture(t)
	ctx := context.Background()

	// The excerpt screen alone serves both halves of the split credential.
	if got := f.searchIDs(t); got != "[1 2 3]" {
		t.Fatalf("hits before the policy change = %s, want [1 2 3]", got)
	}

	f.reset(t, true)

	// search: a pending document returns no hit; clean.md is pending too, so
	// nothing is returned until the scan reads the documents again.
	if got := f.searchIDs(t); got != "[]" {
		t.Fatalf("hits while documents wait for re-evaluation = %s, want []", got)
	}

	// ask: no passage of a pending document reaches the generator.
	f.gen.lastPrompt = ""
	if _, err := f.svc.Ask(ctx, "what do the deployment notes say", model.SearchQuery{K: 5, Index: "text"}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	for _, part := range []string{"GITHUB_TO", splitKeyHalf2} {
		if strings.Contains(f.gen.lastPrompt, part) {
			t.Fatalf("the prompt holds %q of a pending document:\n%s", part, f.gen.lastPrompt)
		}
	}

	// open_file: a pending document is FORBIDDEN, even when its text is clean.
	for _, rel := range []string{"split.md", "clean.md"} {
		if _, err := f.svc.OpenFile(ctx, rel, model.Span{}, 200); !errors.Is(err, model.ErrForbidden) {
			t.Fatalf("open_file %s while pending: err = %v, want ErrForbidden", rel, err)
		}
	}

	// The scan decides clean.md again: its row gets a content hash, which
	// clears the mark, and the document is served again.
	if err := f.store.UpsertDocument(ctx, model.Document{RelPath: "clean.md", DocType: "md", Status: "ok", ContentHash: "h2"}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	if got := f.searchIDs(t); got != "[3]" {
		t.Fatalf("hits after clean.md was decided again = %s, want [3]", got)
	}
	got, err := f.svc.OpenFile(ctx, "clean.md", model.Span{}, 200)
	if err != nil || !strings.Contains(got, "clean deployment notes") {
		t.Fatalf("open_file clean.md after the rescan = (%q, %v), want its text", got, err)
	}

	// An upsert that does not finalize (an empty content hash) keeps the mark.
	if err := f.store.UpsertDocument(ctx, model.Document{RelPath: "split.md", DocType: "md", Status: "ok"}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	if got := f.searchIDs(t); got != "[3]" {
		t.Fatalf("hits after an unfinalized upsert of split.md = %s, want [3]", got)
	}
}

func TestSecretPending_NarrowingChangeHidesNothing(t *testing.T) {
	f := newPendingFixture(t)
	f.reset(t, false)
	if got := f.searchIDs(t); got != "[1 2 3]" {
		t.Fatalf("hits after a narrowing change = %s, want [1 2 3]", got)
	}
	got, err := f.svc.OpenFile(context.Background(), "clean.md", model.Span{}, 200)
	if err != nil || !strings.Contains(got, "clean deployment notes") {
		t.Fatalf("open_file clean.md after a narrowing change = (%q, %v), want its text", got, err)
	}
}

// TestSecretAllow_ServeTimeScreen pins that the serve-time screen applies the
// allow list: a chunk that quotes a redirect link is served, and a chunk that
// also holds a credential is not.
func TestSecretAllow_ServeTimeScreen(t *testing.T) {
	tok := "to" + "ken"
	redirect := "https://www.youtube.com/redirect?event=video_description&redir_" + tok + "=QUFFLUhqbkJ3T2xYbE5fZ3lQdmZ4&q=x"
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	addVec(t, idx, 2, []float32{0.9, 0.1})
	st := &fullTextStore{text: map[uint64]string{
		1: "source link " + redirect,
		2: "source link " + redirect + " and GITHUB_" + strings.ToUpper(tok) + "=Xy7Zq9WvXy7Zq9WvXy7Zq9WvXy7Zq9Wv",
	}}
	svc := retrieval.NewService(st, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "a.md", DocType: "md", Snippet: "source link"})
	svc.SetChunkMetadata(2, model.SearchHit{ChunkID: 2, RelPath: "b.md", DocType: "md", Snippet: "source link"})

	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "source link", K: 5, Index: "text"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := fmt.Sprint(hitIDs(hits)); got != "[1]" {
		t.Fatalf("hits = %s, want [1] (the redirect link is allowed, the credential is not)", got)
	}

	// An empty allow list cancels nothing: the redirect link matches again.
	if err := svc.SetSecretAllowPatterns(nil); err != nil {
		t.Fatalf("SetSecretAllowPatterns: %v", err)
	}
	hits, err = svc.Search(context.Background(), model.SearchQuery{Query: "source link", K: 5, Index: "text"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := fmt.Sprint(hitIDs(hits)); got != "[]" {
		t.Fatalf("hits with an empty allow list = %s, want []", got)
	}
}

// TestSecretAllow_OpenFileStreamingWindows pins the windowed open_file scan:
// the allow list is applied on the same window as the secret patterns, a
// credential that crosses the 64 KiB read boundary is still refused, and a
// redirect link at the same place is still served.
func TestSecretAllow_OpenFileStreamingWindows(t *testing.T) {
	tok := "to" + "ken"
	const boundary = 64 << 10
	build := func(payload string, offset int) []byte {
		pad := strings.Repeat("plain archive text line\n", (boundary+4096)/24)
		body := []byte(pad[:boundary-offset] + payload + "\n" + pad)
		return body
	}
	credential := "GITHUB_" + strings.ToUpper(tok) + "=Xy7Zq9WvXy7Zq9WvXy7Zq9WvXy7Zq9WvXy7Zq9Wv"
	redirect := "https://www.youtube.com/redirect?event=video_description&redir_" + tok + "=QUFFLUhqbkJ3T2xYbE5fZ3lQdmZ4&q=x"

	root := testutil.TempDir(t)
	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	for _, offset := range []int{5, 12, 20, 30} {
		credPath := fmt.Sprintf("cred-%d.txt", offset)
		linkPath := fmt.Sprintf("link-%d.txt", offset)
		if err := os.WriteFile(filepath.Join(root, credPath), build(credential, offset), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, linkPath), build(redirect, offset), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := svc.OpenFile(context.Background(), credPath, model.Span{}, 100); !errors.Is(err, model.ErrForbidden) {
			t.Errorf("open_file %s (credential across the boundary): err = %v, want ErrForbidden", credPath, err)
		}
		got, err := svc.OpenFile(context.Background(), linkPath, model.Span{}, 100)
		if err != nil || !strings.HasPrefix(got, "plain archive text") {
			t.Errorf("open_file %s (redirect link across the boundary) = (%q, %v), want the text", linkPath, got, err)
		}
	}
}
