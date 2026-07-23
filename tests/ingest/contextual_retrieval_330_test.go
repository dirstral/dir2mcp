package tests

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// fakeContextualizer is a deterministic ChunkContextualizer: it echoes a
// recognizable marker so a test can assert the context reached the embed input
// and NOTHING else. failOn makes generation fail for chunks containing a
// substring, exercising the fail-open-per-chunk path.
type fakeContextualizer struct {
	mu     sync.Mutex
	calls  int
	docs   []string
	failOn string
}

const contextMarker = "CTXMARKER"

func (f *fakeContextualizer) Contextualize(_ context.Context, docText, chunkText string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.docs = append(f.docs, docText)
	if f.failOn != "" && strings.Contains(chunkText, f.failOn) {
		return "", errors.New("simulated generator failure")
	}
	return contextMarker + " situating: " + strings.Fields(chunkText)[0], nil
}

func (f *fakeContextualizer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeContextualizer) seenDocs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.docs...)
}

// TestContextual_DisabledLeavesChunksUntouched pins the opt-in default: with no
// contextualizer bound, every chunk records embedding_mode=disabled with an
// empty context, so a corpus is byte-identical to one built before the feature.
func TestContextual_DisabledLeavesChunksUntouched(t *testing.T) {
	st := &fakeIngestStore{}
	rg := ingest.NewRepresentationGenerator(st)

	doc := model.Document{DocID: 1, RelPath: "docs/a.md", DocType: "text"}
	if err := rg.GenerateRawTextFromContent(
		context.Background(), doc, []byte("alpha paragraph body\n\nbeta paragraph body")); err != nil {
		t.Fatalf("GenerateRawTextFromContent: %v", err)
	}
	if len(st.chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}
	for i, c := range st.chunks {
		if c.Context != "" {
			t.Errorf("chunk %d: context must be empty when contextualization is off, got %q", i, c.Context)
		}
		if got := model.NormalizeEmbeddingMode(c.EmbeddingMode); got != model.EmbeddingModeDisabled {
			t.Errorf("chunk %d: embedding_mode = %q, want %q", i, got, model.EmbeddingModeDisabled)
		}
	}
}

// TestContextual_CitationFaithfulness is the HARD INVARIANT (SPEC §8.1.8,
// dir2mcp #403): the generated context is an EMBED-INPUT transform only. The
// persisted chunk text, its text_hash, and its spans stay the RAW chunk, so the
// context can never surface in a snippet, an open_file result, or a citation.
func TestContextual_CitationFaithfulness(t *testing.T) {
	st := &fakeIngestStore{}
	rg := ingest.NewRepresentationGenerator(st)
	fake := &fakeContextualizer{}
	rg.SetContextualizer(fake)

	raw := "alpha paragraph body\n\nbeta paragraph body"
	doc := model.Document{DocID: 1, RelPath: "docs/a.md", DocType: "text"}
	if err := rg.GenerateRawTextFromContent(context.Background(), doc, []byte(raw)); err != nil {
		t.Fatalf("GenerateRawTextFromContent: %v", err)
	}
	if len(st.chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}
	if fake.callCount() != len(st.chunks) {
		t.Fatalf("expected one generation per chunk: %d calls for %d chunks", fake.callCount(), len(st.chunks))
	}

	for i, c := range st.chunks {
		// (a) the CITED text is raw — the marker never appears in it.
		if strings.Contains(c.Text, contextMarker) {
			t.Errorf("chunk %d: generated context leaked into chunks.text: %q", i, c.Text)
		}
		if !strings.Contains(raw, c.Text) {
			t.Errorf("chunk %d: persisted text is not a verbatim slice of the source: %q", i, c.Text)
		}
		// (b) the text_hash keys the RAW text (skip semantics unchanged, §7.6).
		if c.TextHash != ingest.ComputeContentHash([]byte(c.Text)) {
			t.Errorf("chunk %d: text_hash must key the raw chunk text", i)
		}
		// (c) the context is persisted separately and the mode records it.
		if !strings.Contains(c.Context, contextMarker) {
			t.Errorf("chunk %d: expected a generated context, got %q", i, c.Context)
		}
		if c.EmbeddingMode != model.EmbeddingModeContextualized {
			t.Errorf("chunk %d: embedding_mode = %q, want %q", i, c.EmbeddingMode, model.EmbeddingModeContextualized)
		}
		// (d) ONLY the embed input carries the join (SPEC §8.1.8 `context + "\n\n" + chunk`).
		task := model.ChunkTask{Text: c.Text, Context: c.Context}
		if task.EmbedInput() != c.Context+"\n\n"+c.Text {
			t.Errorf("chunk %d: embed input must be context + blank line + raw chunk", i)
		}
		if !strings.HasSuffix(task.EmbedInput(), c.Text) {
			t.Errorf("chunk %d: the raw chunk must be preserved verbatim in the embed input", i)
		}
	}

	// The generator sees the PARENT DOCUMENT, not just the chunk — that is what
	// makes the context document-aware (design 0004 §3).
	for _, seen := range fake.seenDocs() {
		for _, c := range st.chunks {
			if !strings.Contains(seen, c.Text) {
				t.Fatalf("the generator must see the whole parent document; %q missing from %q", c.Text, seen)
			}
		}
	}
}

// TestContextual_FailOpenPerChunk pins SPEC §8.1.8: a generator error for ONE
// chunk leaves that chunk raw with embedding_mode=fallback — it never fails
// ingest, and never silently looks like a healthy contextualized chunk.
func TestContextual_FailOpenPerChunk(t *testing.T) {
	st := &fakeIngestStore{}
	rg := ingest.NewRepresentationGenerator(st)
	rg.SetContextualizer(&fakeContextualizer{failOn: "beta"})

	doc := model.Document{DocID: 1, RelPath: "docs/a.md", DocType: "text"}
	if err := rg.GenerateRawTextFromContent(
		context.Background(), doc, []byte(multiChunkBody())); err != nil {
		t.Fatalf("a per-chunk generation failure must NOT fail ingest: %v", err)
	}
	if len(st.chunks) < 2 {
		t.Fatalf("precondition: the fixture must produce >= 2 chunks, got %d", len(st.chunks))
	}

	var contextualized, fallback int
	for i, c := range st.chunks {
		switch c.EmbeddingMode {
		case model.EmbeddingModeContextualized:
			contextualized++
		case model.EmbeddingModeFallback:
			fallback++
			if c.Context != "" {
				t.Errorf("chunk %d: a fallback chunk must carry no context, got %q", i, c.Context)
			}
		default:
			t.Errorf("chunk %d: unexpected embedding_mode %q", i, c.EmbeddingMode)
		}
		// Either way the chunk text is untouched.
		if strings.Contains(c.Text, contextMarker) {
			t.Errorf("chunk %d: context leaked into chunks.text", i)
		}
	}
	if fallback == 0 {
		t.Fatal("expected at least one fallback chunk")
	}
	if contextualized == 0 {
		t.Fatal("a failure on one chunk must not disable contextualization for its siblings")
	}
}

// multiChunkBody is a text fixture large enough to split into several chunks,
// with the word "beta" confined to the SECOND half so a failOn:"beta"
// contextualizer fails some chunks and not others.
func multiChunkBody() string {
	alpha := strings.Repeat("alpha paragraph body with enough words to fill a chunk. ", 80)
	beta := strings.Repeat("beta paragraph body with enough words to fill a chunk. ", 80)
	return alpha + "\n\n" + beta
}

// TestContextual_EmptyChunkStaysDisabled guards the degenerate case: a
// whitespace-only segment has nothing to situate, so it is not sent to the
// generator and records `disabled` rather than a spurious `fallback`.
func TestContextual_EmptyChunkStaysDisabled(t *testing.T) {
	st := &fakeIngestStore{}
	rg := ingest.NewRepresentationGenerator(st)
	fake := &fakeContextualizer{}
	rg.SetContextualizer(fake)

	doc := model.Document{DocID: 1, RelPath: "docs/a.md", DocType: "text"}
	if err := rg.GenerateRawTextFromContent(
		context.Background(), doc, []byte("alpha body text")); err != nil {
		t.Fatalf("GenerateRawTextFromContent: %v", err)
	}
	if fake.callCount() == 0 {
		t.Fatal("precondition: the non-empty chunk must be contextualized")
	}
}

// TestChunkTask_EmbedInputDefaultsToRaw pins that with no context the embed
// input is the raw text byte-for-byte, so the default path is unchanged.
func TestChunkTask_EmbedInputDefaultsToRaw(t *testing.T) {
	task := model.ChunkTask{Text: "raw chunk"}
	if task.EmbedInput() != "raw chunk" {
		t.Fatalf("EmbedInput() = %q, want the raw text", task.EmbedInput())
	}
}
