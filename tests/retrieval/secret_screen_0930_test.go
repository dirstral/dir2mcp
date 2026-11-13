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
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Serve-time secret screen (spec 0.93.0, SPEC §7.2). When the effective secret
// pattern set changes, a background pass retires the indexed chunks that the
// new set matches. Until the pass reaches a document, its chunks stay in the
// index, so every tool that returns indexed text screens that text against the
// new set: search and related leave a matching hit out, ask does not place it
// in the prompt, and open_file returns FORBIDDEN.

const (
	screenCredential = "internal-credential-QXZ"
	screenPattern    = `internal-credential-[A-Z]{3}`
)

// newScreenService indexes two chunks. Chunk 1 holds the credential in its full
// text only: the snippet on the hit is clean, as a truncated snippet is. Chunk 2
// is clean. No operator pattern is set yet: the pattern arrives "after indexing".
func newScreenService(t *testing.T, gen model.Generator) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	addVec(t, idx, 2, []float32{0.9, 0.1})
	store := &fullTextStore{text: map[uint64]string{
		1: "alpha deployment notes. The key is " + screenCredential + " for the staging host.",
		2: "beta deployment notes without any credential.",
	}}
	svc := retrieval.NewService(store, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "a.md", DocType: "md", Snippet: "alpha deployment notes"})
	svc.SetChunkMetadata(2, model.SearchHit{ChunkID: 2, RelPath: "b.md", DocType: "md", Snippet: "beta deployment notes"})
	return svc
}

func TestSecretScreen_SearchLeavesOutAMatchingHit(t *testing.T) {
	svc := newScreenService(t, nil)
	query := model.SearchQuery{Query: "deployment notes", K: 5, Index: "text"}

	before, err := svc.Search(context.Background(), query)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := fmt.Sprint(hitIDs(before)); got != "[1 2]" {
		t.Fatalf("hits before the pattern = %s, want [1 2]", got)
	}

	if err := svc.SetSecretPatterns([]string{screenPattern}); err != nil {
		t.Fatalf("SetSecretPatterns: %v", err)
	}
	after, err := svc.Search(context.Background(), query)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Chunk 1 matches in its full text, not in its snippet, and is left out.
	// The clean chunk 2 is unaffected.
	if got := fmt.Sprint(hitIDs(after)); got != "[2]" {
		t.Fatalf("hits after the pattern = %s, want [2]", got)
	}
}

func TestSecretScreen_SearchLeavesOutAMatchingSnippet(t *testing.T) {
	svc := newScreenService(t, nil)
	if err := svc.SetSecretPatterns([]string{`beta deployment`}); err != nil {
		t.Fatalf("SetSecretPatterns: %v", err)
	}
	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "deployment notes", K: 5, Index: "text"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := fmt.Sprint(hitIDs(hits)); got != "[1]" {
		t.Fatalf("hits = %s, want [1]", got)
	}
}

func TestSecretScreen_AskDoesNotPassAMatchingChunkToTheGenerator(t *testing.T) {
	gen := &fakeGenerator{out: "The notes describe a deployment. [b.md]"}
	svc := newScreenService(t, gen)
	if err := svc.SetSecretPatterns([]string{screenPattern}); err != nil {
		t.Fatalf("SetSecretPatterns: %v", err)
	}
	res, err := svc.Ask(context.Background(), "what do the deployment notes say", model.SearchQuery{K: 5, Index: "text"})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if gen.lastPrompt == "" {
		t.Fatal("the generator was not called")
	}
	if strings.Contains(gen.lastPrompt, screenCredential) {
		t.Fatalf("the prompt holds the credential:\n%s", gen.lastPrompt)
	}
	if !strings.Contains(gen.lastPrompt, "beta deployment notes without any credential.") {
		t.Fatalf("the clean chunk did not reach the prompt:\n%s", gen.lastPrompt)
	}
	for _, c := range res.Citations {
		if c.ChunkID == 1 {
			t.Fatalf("the answer cites the screened chunk: %+v", res.Citations)
		}
	}
	for _, h := range res.Hits {
		if h.ChunkID == 1 {
			t.Fatalf("the ask result returns the screened hit: %+v", res.Hits)
		}
	}
}

func TestSecretScreen_RelatedLeavesOutAMatchingHit(t *testing.T) {
	svc := newRelatedService(t)
	if err := svc.SetSecretPatterns([]string{`^beta$`}); err != nil {
		t.Fatalf("SetSecretPatterns: %v", err)
	}
	res, err := svc.Related(context.Background(), model.RelatedQuery{SourceChunkID: 1, K: 10, ExcludeSameDocument: true})
	if err != nil {
		t.Fatalf("Related: %v", err)
	}
	// Without the screen the hits are [3 4] (see TestRelated_ChunkID_ExcludesSameDocument).
	if got := fmt.Sprint(hitIDs(res.Hits)); got != "[4]" {
		t.Fatalf("related hits = %s, want [4]", got)
	}
}

func TestSecretScreen_OpenFileRefusesTheSameText(t *testing.T) {
	root := testutil.TempDir(t)
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("The key is "+screenCredential+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.md"), []byte("beta deployment notes\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	if err := svc.SetSecretPatterns([]string{screenPattern}); err != nil {
		t.Fatalf("SetSecretPatterns: %v", err)
	}
	if _, err := svc.OpenFile(context.Background(), "a.md", model.Span{}, 200); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("open_file on the matching file: err = %v, want ErrForbidden", err)
	}
	got, err := svc.OpenFile(context.Background(), "b.md", model.Span{}, 200)
	if err != nil || !strings.Contains(got, "beta deployment notes") {
		t.Fatalf("open_file on the clean file = (%q, %v), want its text", got, err)
	}
	if !svc.MatchesSecret("x " + screenCredential) {
		t.Fatal("MatchesSecret misses the operator pattern")
	}
	if svc.MatchesSecret("beta deployment notes") {
		t.Fatal("MatchesSecret matches clean text")
	}
}
