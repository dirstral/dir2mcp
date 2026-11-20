package tests

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §7.2 (spec 0.99.0): the pending mark is looked up by path, at one
// moment. A broader policy can mark a document after a hit was read, and a
// scan can replace the document and clear the mark before that lookup. The
// hit text then belongs to a generation that the new policy never evaluated,
// and a credential split across two such hits passes the per-hit screen. So
// each hit is bound to the document generation: the text that is served or
// sent comes from the stored chunk, and the document must have the same
// status and content hash before the text is read and after the pending
// check.

const (
	genOldHalf1 = "staging notes. GITHUB_TO"
	genOldHalf2 = "KEN=Xy7Zq9WvXy7Zq9WvXy7Zq9WvXy7Zq9WvXy7Zq9Wv"
	genNewText1 = "staging notes, first part of the new version"
	genNewText2 = "staging notes, second part of the new version"
	genCleanTxt = "staging notes about the bridge"
)

// generationStore is the real SQLite store (documents, policy, pending marks)
// with chunk text served from a map. atPendingLookup runs once, just before
// pending lookup number hookAt answers (1 is the first lookup).
type generationStore struct {
	*store.SQLiteStore
	text            map[uint64]string
	path            map[uint64]string
	lookups         int
	hookAt          int
	atPendingLookup func()
}

func (s *generationStore) ChunkTaskByID(_ context.Context, chunkID uint64) (model.ChunkTask, string, error) {
	text, ok := s.text[chunkID]
	if !ok {
		return model.ChunkTask{}, "", model.ErrNotFound
	}
	return model.ChunkTask{Label: chunkID, Text: text, IndexKind: "text",
		Metadata: model.ChunkMetadata{ChunkID: chunkID, RelPath: s.path[chunkID], Snippet: text}}, "", nil
}

func (s *generationStore) SecretReevalPendingPaths(ctx context.Context, relPaths []string) (map[string]struct{}, error) {
	s.lookups++
	if s.lookups == s.hookAt && s.atPendingLookup != nil {
		s.atPendingLookup()
	}
	return s.SQLiteStore.SecretReevalPendingPaths(ctx, relPaths)
}

type generationFixture struct {
	svc   *retrieval.Service
	store *generationStore
	gen   *fakeGenerator
}

// newGenerationFixture indexes split.md (chunks 1 and 2, the two halves of a
// credential, as the earlier generation of the document) and clean.md (chunk
// 3). Both documents are decided under the recorded policy "v2:first". The
// index holds the snippets of that generation.
func newGenerationFixture(t *testing.T) *generationFixture {
	t.Helper()
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(testutil.TempDir(t), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.ResetForSecretPolicy(ctx, "v2:first", []string{`BLUEHERON`}, nil, false); err != nil {
		t.Fatalf("ResetForSecretPolicy: %v", err)
	}
	for _, rel := range []string{"split.md", "clean.md"} {
		if err := st.UpsertDocument(model.WithSecretPolicyIdentity(ctx, "v2:first"), model.Document{RelPath: rel, DocType: "md", Status: "ok", ContentHash: "h1"}); err != nil {
			t.Fatalf("UpsertDocument(%s): %v", rel, err)
		}
	}
	gs := &generationStore{SQLiteStore: st,
		text: map[uint64]string{1: genOldHalf1, 2: genOldHalf2, 3: genCleanTxt},
		path: map[uint64]string{1: "split.md", 2: "split.md", 3: "clean.md"},
	}
	idx := index.NewHNSWIndex("")
	addVecP(t, idx, 1, []float32{1, 0}, "split.md", "md")
	addVecP(t, idx, 2, []float32{0.97, 0.03}, "split.md", "md")
	addVecP(t, idx, 3, []float32{0.9, 0.1}, "clean.md", "md")
	gen := &fakeGenerator{out: "notes [clean.md]"}
	svc := retrieval.NewService(gs, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	for id, text := range gs.text {
		svc.SetChunkMetadata(id, model.SearchHit{ChunkID: id, RelPath: gs.path[id], DocType: "md", Snippet: text})
	}
	return &generationFixture{svc: svc, store: gs, gen: gen}
}

// replaceSplit records a broader policy and lets a scan replace split.md with
// a new, clean version under it: the mark is cleared, the chunk rows hold the
// new text, and the index still holds the earlier snippets.
func (f *generationFixture) replaceSplit(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.store.ResetForSecretPolicy(ctx, "v2:later", []string{`BLUEHERON`, `GITHUB_TOKEN=\S+`}, nil, true); err != nil {
		t.Fatalf("ResetForSecretPolicy: %v", err)
	}
	f.store.text[1], f.store.text[2] = genNewText1, genNewText2
	later := model.WithSecretPolicyIdentity(ctx, "v2:later")
	for _, rel := range []string{"split.md", "clean.md"} {
		if err := f.store.UpsertDocument(later, model.Document{RelPath: rel, DocType: "md", Status: "ok", ContentHash: "h2-" + rel}); err != nil {
			t.Fatalf("UpsertDocument(%s): %v", rel, err)
		}
	}
}

func requireNoOldGeneration(t *testing.T, where string, texts ...string) {
	t.Helper()
	for _, text := range texts {
		for _, part := range []string{"GITHUB_TO", genOldHalf2} {
			if strings.Contains(text, part) {
				t.Fatalf("%s holds text of the earlier generation (%q):\n%s", where, part, text)
			}
		}
	}
}

// TestSecretGeneration_ReplacementBeforeTheScreen: the replacement commits
// after the index read the earlier snippets and before the request. The path
// is clean and not pending. The served snippets must be the stored ones.
func TestSecretGeneration_ReplacementBeforeTheScreen(t *testing.T) {
	f := newGenerationFixture(t)
	f.replaceSplit(t)
	hits, err := f.svc.Search(context.Background(), model.SearchQuery{Query: "staging notes", K: 5, Index: "text"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := fmt.Sprint(hitIDs(hits)); got != "[1 2 3]" {
		t.Fatalf("hits = %s, want [1 2 3] (the document is decided under the current policy)", got)
	}
	for _, hit := range hits {
		requireNoOldGeneration(t, "a search hit", hit.Snippet)
	}
}

// TestSecretGeneration_ReplacementDuringTheScreen: the replacement commits
// between the first generation reading and the pending lookup of the final
// screen. The mark is already cleared when the lookup runs, so only the
// generation comparison can leave the hits out.
func TestSecretGeneration_ReplacementDuringTheScreen(t *testing.T) {
	f := newGenerationFixture(t)
	f.store.hookAt = 1
	f.store.atPendingLookup = func() { f.replaceSplit(t) }
	hits, err := f.svc.Search(context.Background(), model.SearchQuery{Query: "staging notes", K: 5, Index: "text"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if f.store.lookups == 0 {
		t.Fatal("no pending lookup ran; the test proves nothing")
	}
	if len(hits) != 0 {
		t.Fatalf("search returned %v for documents that changed during the request, want none", hitIDs(hits))
	}
	// The next request sees the new generation.
	hits, err = f.svc.Search(context.Background(), model.SearchQuery{Query: "staging notes", K: 5, Index: "text"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := fmt.Sprint(hitIDs(hits)); got != "[1 2 3]" {
		t.Fatalf("hits on the next request = %s, want [1 2 3]", got)
	}
}

// TestSecretGeneration_AskPromptAfterAReplacement: search returned the hits,
// and the replacement commits before ask reads the chunk texts for the
// prompt. The prompt must hold no text of a document that changed.
func TestSecretGeneration_AskPromptAfterAReplacement(t *testing.T) {
	f := newGenerationFixture(t)
	// Lookup 1 is the final screen of search; lookup 2 is the prompt context.
	f.store.hookAt = 2
	f.store.atPendingLookup = func() {
		ctx := context.Background()
		if _, err := f.store.ResetForSecretPolicy(ctx, "v2:later", []string{`BLUEHERON`, `GITHUB_TOKEN=\S+`}, nil, true); err != nil {
			t.Fatalf("ResetForSecretPolicy: %v", err)
		}
		// The scan decides only clean.md again; split.md still waits.
		if err := f.store.UpsertDocument(model.WithSecretPolicyIdentity(ctx, "v2:later"), model.Document{RelPath: "clean.md", DocType: "md", Status: "ok", ContentHash: "h2"}); err != nil {
			t.Fatalf("UpsertDocument: %v", err)
		}
	}
	if _, err := f.svc.Ask(context.Background(), "what do the staging notes say", model.SearchQuery{K: 5, Index: "text"}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if f.store.lookups < 2 {
		t.Fatalf("only %d pending lookup(s) ran; the test proves nothing", f.store.lookups)
	}
	if f.gen.lastPrompt == "" {
		t.Fatal("the generator was not called; the test proves nothing")
	}
	requireNoOldGeneration(t, "the ask prompt", f.gen.lastPrompt)
	if strings.Contains(f.gen.lastPrompt, genCleanTxt) {
		t.Fatalf("the ask prompt holds the text of clean.md, which changed during the request:\n%s", f.gen.lastPrompt)
	}
}

// TestSecretGeneration_OpenFileMetadataSliceIsBound: open_file checks the
// withholding state, then reads a page slice from the stored chunks. A
// broader policy that is recorded between the two steps (with or without a
// replacement that clears the mark again) must refuse the call.
func TestSecretGeneration_OpenFileMetadataSliceIsBound(t *testing.T) {
	for name, replace := range map[string]bool{"marked": false, "marked and replaced": true} {
		f := newGenerationFixture(t)
		ctx := context.Background()
		if err := f.store.UpsertDocument(model.WithSecretPolicyIdentity(ctx, "v2:first"), model.Document{RelPath: "scan.pdf", DocType: "pdf", Status: "ok", ContentHash: "h1"}); err != nil {
			t.Fatalf("UpsertDocument: %v", err)
		}
		span := model.Span{Kind: "page", Page: 1}
		f.svc.SetChunkMetadata(9, model.SearchHit{RelPath: "scan.pdf", Snippet: "page one text", Span: span})
		f.svc.SetRootDir(testutil.TempDir(t))

		out, err := f.svc.OpenFile(ctx, "scan.pdf", span, 200)
		if err != nil || out != "page one text" {
			t.Fatalf("%s: OpenFile before the change = (%q, %v), want the page", name, out, err)
		}

		// Lookup 1 of this call is the check in openFile; lookup 2 runs after
		// the stored chunks are read.
		f.store.lookups = 0
		f.store.hookAt = 2
		f.store.atPendingLookup = func() {
			if _, err := f.store.ResetForSecretPolicy(ctx, "v2:later", []string{`BLUEHERON`, `page one`}, nil, true); err != nil {
				t.Fatalf("ResetForSecretPolicy: %v", err)
			}
			if replace {
				if err := f.store.UpsertDocument(model.WithSecretPolicyIdentity(ctx, "v2:later"), model.Document{RelPath: "scan.pdf", DocType: "pdf", Status: "ok", ContentHash: "h2"}); err != nil {
					t.Fatalf("UpsertDocument: %v", err)
				}
			}
		}
		if out, err := f.svc.OpenFile(ctx, "scan.pdf", span, 200); !errors.Is(err, model.ErrForbidden) {
			t.Errorf("%s: OpenFile = (%q, %v), want ErrForbidden for a document that changed during the call", name, out, err)
		}
		if f.store.lookups < 2 {
			t.Errorf("%s: only %d pending lookup(s) ran; the slice was not checked again", name, f.store.lookups)
		}
	}
}

// TestSecretGeneration_ResetOfAnUnfinalizedDocumentChangesTheGeneration: a
// live `ok` row with no content hash (a document that is not finalized) keeps
// its status and hash through a broader reset. The pending mark is part of
// the generation, so the reset is still seen.
func TestSecretGeneration_ResetOfAnUnfinalizedDocumentChangesTheGeneration(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(testutil.TempDir(t), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.UpsertDocument(ctx, model.Document{RelPath: "wip.md", DocType: "md", Status: "ok"}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	before, err := st.DocumentGenerations(ctx, []string{"wip.md"})
	if err != nil {
		t.Fatalf("DocumentGenerations: %v", err)
	}
	if _, err := st.ResetForSecretPolicy(ctx, "v2:later", []string{`BLUEHERON`}, nil, true); err != nil {
		t.Fatalf("ResetForSecretPolicy: %v", err)
	}
	after, err := st.DocumentGenerations(ctx, []string{"wip.md"})
	if err != nil {
		t.Fatalf("DocumentGenerations: %v", err)
	}
	if before["wip.md"] == "" || before["wip.md"] == after["wip.md"] {
		t.Fatalf("generation before = %q, after = %q; a broader reset must change it", before["wip.md"], after["wip.md"])
	}
}
