package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// fakeMediaStore is a minimal model.Store that also reports chunk-modality
// presence, so open_file can classify a document as media-only (SPEC 8.1.7).
type fakeMediaStore struct {
	hasMedia bool
	hasText  bool
}

func (f *fakeMediaStore) Init(context.Context) error { return nil }
func (f *fakeMediaStore) UpsertDocument(context.Context, model.Document) error {
	return nil
}
func (f *fakeMediaStore) GetDocumentByPath(context.Context, string) (model.Document, error) {
	return model.Document{}, model.ErrNotFound
}
func (f *fakeMediaStore) ListFiles(context.Context, string, string, int, int) ([]model.Document, int64, error) {
	return nil, 0, nil
}
func (f *fakeMediaStore) Close() error { return nil }
func (f *fakeMediaStore) ChunkModalityPresence(_ context.Context, _ string) (bool, bool, error) {
	return f.hasMedia, f.hasText, nil
}

// TestOpenFile_MediaOnly_ReturnsMediaNoText pins SPEC 8.1.7/§15.4: a replace-mode
// media-only document (media chunks, no text) returns the non-retryable
// MEDIA_NO_TEXT, never raw bytes.
func TestOpenFile_MediaOnly_ReturnsMediaNoText(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "doc.pdf"), []byte("%PDF-1.4 binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := retrieval.NewService(&fakeMediaStore{hasMedia: true, hasText: false}, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(filepath.Join(root, ".dir2mcp"))

	_, err := svc.OpenFile(context.Background(), "doc.pdf", model.Span{}, 200)
	if !errors.Is(err, model.ErrMediaNoText) {
		t.Fatalf("media-only doc: expected ErrMediaNoText, got %v", err)
	}
}

// TestOpenFile_MediaWithText_IsNotMediaNoText confirms the distinction: a media
// document that also has a text path (augment) is NOT media-only — with no OCR
// cache yet it surfaces the retryable OCR_NOT_READY, not MEDIA_NO_TEXT.
func TestOpenFile_MediaWithText_IsNotMediaNoText(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "doc.pdf"), []byte("%PDF-1.4 binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := retrieval.NewService(&fakeMediaStore{hasMedia: true, hasText: true}, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(filepath.Join(root, ".dir2mcp"))

	_, err := svc.OpenFile(context.Background(), "doc.pdf", model.Span{}, 200)
	if errors.Is(err, model.ErrMediaNoText) {
		t.Fatalf("doc with a text path must not be MEDIA_NO_TEXT; got %v", err)
	}
	if !errors.Is(err, model.ErrOCRNotReady) {
		t.Fatalf("expected OCR_NOT_READY (text pending, no cache), got %v", err)
	}
}

// TestOpenFile_TextFile_UnaffectedByMediaGuard confirms the media-only guard is
// gated to media extensions: a markdown file is served normally even with a
// store that would report media presence.
func TestOpenFile_TextFile_UnaffectedByMediaGuard(t *testing.T) {
	root := t.TempDir()
	body := "# Title\n\nhello"
	if err := os.WriteFile(filepath.Join(root, "readme.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := retrieval.NewService(&fakeMediaStore{hasMedia: true, hasText: false}, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(filepath.Join(root, ".dir2mcp"))

	out, err := svc.OpenFile(context.Background(), "readme.md", model.Span{}, 20000)
	if err != nil {
		t.Fatalf("markdown open_file err: %v", err)
	}
	if out != body {
		t.Fatalf("markdown body mismatch: %q", out)
	}
}

// TestAsk_MediaOnlyHit_CitedWithoutQuotedText pins SPEC 8.1.7 ask grounding: a
// media-only hit (no text) is still cited, and the RAG prompt marks it as cited
// without quoted context rather than dropping it as a missing snippet.
func TestAsk_MediaOnlyHit_CitedWithoutQuotedText(t *testing.T) {
	idx := index.NewHNSWIndex("")
	if err := idx.Add(1, []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	gen := &fakeGenerator{out: "an answer [clip.mp4]"}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	svc.SetChunkMetadata(1, model.SearchHit{
		RelPath: "clip.mp4", DocType: "video", Modality: "video", MediaRef: "clip.mp4",
		Snippet: "", Span: model.Span{Kind: "time", StartMS: 0, EndMS: 60000},
	})

	res, err := svc.Ask(context.Background(), "what happens in the clip", model.SearchQuery{K: 5})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(res.Citations) != 1 || res.Citations[0].RelPath != "clip.mp4" {
		t.Fatalf("expected a single clip.mp4 citation, got %#v", res.Citations)
	}
	if !strings.Contains(gen.lastPrompt, "video media; cited without quoted text") {
		t.Fatalf("RAG prompt should mark the media-only hit as cited without quoted text; got:\n%s", gen.lastPrompt)
	}
}
