package tests

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/internal/secretmatch"
)

// TestSecretScreen_RefillsAfterTheScreen pins that the secret screen runs
// before the final cut to k (SPEC §7.2, spec 0.93.0). With k=1, the best
// candidate holds a credential and the second one is clean: the caller gets
// the clean one, not an empty result.
func TestSecretScreen_RefillsAfterTheScreen(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	addVec(t, idx, 2, []float32{0.9, 0.1})
	st := &fullTextStore{text: map[uint64]string{
		1: "deployment notes GITHUB_" + "TOKEN=Xy7Zq9WvXy7Zq9WvXy7Zq9WvXy7Zq9Wv",
		2: "deployment notes without any credential",
	}}
	svc := retrieval.NewService(st, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "a.md", DocType: "md", Snippet: "deployment notes"})
	svc.SetChunkMetadata(2, model.SearchHit{ChunkID: 2, RelPath: "b.md", DocType: "md", Snippet: "deployment notes"})

	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "deployment notes", K: 1, Index: "text"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := fmt.Sprint(hitIDs(hits)); got != "[2]" {
		t.Fatalf("hits with k=1 = %s, want [2] (the clean second candidate)", got)
	}
}

// TestSecretScreen_RerankChecksTheStoredTextAsIs pins that the rerank stage
// checks the stored chunk text before any normalization. Trimming the chunk
// " token=AAAA... " makes the anchored allow pattern ^token=A+$ cover it, so
// a check of the trimmed text alone would send it to the reranker.
func TestSecretScreen_RerankChecksTheStoredTextAsIs(t *testing.T) {
	tok := "to" + "ken"
	chunk := " " + tok + "=" + strings.Repeat("A", 24) + " "
	allow := []string{`^` + tok + `=A+$`}
	m, err := secretmatch.Compile([]string{secretmatch.GenericTokenPattern}, allow)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !m.MatchString(chunk) || m.MatchString(strings.TrimSpace(chunk)) {
		t.Fatal("the fixture must match as stored and not match once trimmed")
	}

	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	addVec(t, idx, 2, []float32{0.9, 0.1})
	addVec(t, idx, 3, []float32{0.8, 0.2})
	st := &fullTextStore{text: map[uint64]string{
		1: chunk,
		2: "clean notes about the bridge",
		3: "clean notes about the budget",
	}}
	svc := retrieval.NewService(st, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	if err := svc.SetSecretPolicy([]string{secretmatch.GenericTokenPattern}, allow); err != nil {
		t.Fatalf("SetSecretPolicy: %v", err)
	}
	for id, rel := range map[uint64]string{1: "a.md", 2: "b.md", 3: "c.md"} {
		svc.SetChunkMetadata(id, model.SearchHit{ChunkID: id, RelPath: rel, DocType: "md", Snippet: "notes"})
	}
	rr := &recordingReranker{}
	svc.SetReranker(rr, "m", 50)
	svc.SetRerankEnabled(true)

	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: "notes", K: 3, Index: "text"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if rr.calls == 0 {
		t.Fatal("the reranker was not called; the test proves nothing")
	}
	for _, doc := range rr.docs {
		if strings.Contains(doc, tok+"=AAAA") {
			t.Fatalf("the reranker received a chunk that matches as stored: %q", doc)
		}
	}
}

// TestSecretScreen_AskChecksTheTrimmedPromptText pins the ask prompt boundary:
// the prompt takes the trimmed chunk text, so the trimmed text is checked too.
// With the allow pattern ^ token=A+ $, the stored text " token=AAAA... " is
// covered, but the trimmed text is not. The snippet is clean, so only the
// prompt-boundary check can keep the chunk out.
func TestSecretScreen_AskChecksTheTrimmedPromptText(t *testing.T) {
	tok := "to" + "ken"
	chunk := " " + tok + "=" + strings.Repeat("A", 24) + " "
	allow := []string{`^ ` + tok + `=A+ $`}
	m, err := secretmatch.Compile([]string{secretmatch.GenericTokenPattern}, allow)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if m.MatchString(chunk) || !m.MatchString(strings.TrimSpace(chunk)) {
		t.Fatal("the fixture must pass as stored and match once trimmed")
	}

	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	addVec(t, idx, 2, []float32{0.9, 0.1})
	st := &fullTextStore{text: map[uint64]string{
		1: chunk,
		2: "clean notes about the bridge",
	}}
	gen := &fakeGenerator{out: "notes [b.md]"}
	svc := retrieval.NewService(st, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	if err := svc.SetSecretPolicy([]string{secretmatch.GenericTokenPattern}, allow); err != nil {
		t.Fatalf("SetSecretPolicy: %v", err)
	}
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "a.md", DocType: "md", Snippet: "notes"})
	svc.SetChunkMetadata(2, model.SearchHit{ChunkID: 2, RelPath: "b.md", DocType: "md", Snippet: "notes"})

	if _, err := svc.Ask(context.Background(), "what do the notes say", model.SearchQuery{K: 2, Index: "text"}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if gen.lastPrompt == "" {
		t.Fatal("the generator was not called; the test proves nothing")
	}
	if strings.Contains(gen.lastPrompt, tok+"=AAAA") {
		t.Fatalf("the prompt holds the chunk that matches once trimmed:\n%s", gen.lastPrompt)
	}
}
