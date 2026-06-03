package index

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// withExtractSegment swaps the package segment extractor for the test (so no
// ffmpeg binary is required) and restores it afterward.
func withExtractSegment(t *testing.T, fn func(context.Context, string, int, int) ([]byte, error)) {
	t.Helper()
	prev := extractSegment
	extractSegment = fn
	t.Cleanup(func() { extractSegment = prev })
}

func avChunk(modality string, span model.Span) model.ChunkTask {
	tk := model.NewChunkTask(1, "", "text", model.ChunkMetadata{ChunkID: 1, RelPath: "clip" + extFor(modality), DocType: modality, Span: span})
	tk.Modality = modality
	tk.MediaRef = "clip" + extFor(modality)
	return tk
}

func extFor(modality string) string {
	if modality == "video" {
		return ".mp4"
	}
	return ".mp3"
}

// TestLoadMediaInput_AudioSegmentExtracted pins SPEC 8.1.7: an audio chunk
// embeds only its time window — the worker hands the chunk's [start,end) span
// to the extractor and uses the returned bytes, with the MIME from the source.
func TestLoadMediaInput_AudioVideoSegmentExtracted(t *testing.T) {
	for _, tc := range []struct {
		modality string
		mime     string
	}{
		{"audio", "audio/mp3"},
		{"video", "video/mp4"},
	} {
		t.Run(tc.modality, func(t *testing.T) {
			root := t.TempDir()
			ref := "clip" + extFor(tc.modality)
			if err := os.WriteFile(filepath.Join(root, ref), []byte("FULLMEDIA"), 0o600); err != nil {
				t.Fatal(err)
			}
			var gotStart, gotEnd int
			var gotPath string
			withExtractSegment(t, func(_ context.Context, path string, s, e int) ([]byte, error) {
				gotPath, gotStart, gotEnd = path, s, e
				return []byte("SEGMENT"), nil
			})

			w := &EmbeddingWorker{RootDir: root}
			in, err := w.loadMediaInput(context.Background(), avChunk(tc.modality, model.Span{Kind: "time", StartMS: 1000, EndMS: 3000}))
			if err != nil {
				t.Fatalf("loadMediaInput: %v", err)
			}
			if string(in.Data) != "SEGMENT" {
				t.Errorf("data = %q, want SEGMENT (extracted segment, not whole file)", in.Data)
			}
			if in.MimeType != tc.mime {
				t.Errorf("mime = %q, want %q", in.MimeType, tc.mime)
			}
			if gotStart != 1000 || gotEnd != 3000 {
				t.Errorf("segment span = [%d,%d), want [1000,3000)", gotStart, gotEnd)
			}
			if filepath.Base(gotPath) != ref {
				t.Errorf("extracted from %q, want basename %q", gotPath, ref)
			}
		})
	}
}

// TestLoadMediaInput_InvalidTimeSpanFatal: an audio/video chunk without a valid
// time span is a fatal task error (surface the mismatch, never embed the wrong
// segment), mirroring the PDF page-span guard.
func TestLoadMediaInput_InvalidTimeSpanFatal(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "clip.mp3"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	withExtractSegment(t, func(context.Context, string, int, int) ([]byte, error) {
		t.Fatal("extractor must not be called for an invalid span")
		return nil, nil
	})
	w := &EmbeddingWorker{RootDir: root}

	for _, span := range []model.Span{
		{Kind: "page", Page: 1},              // wrong kind
		{Kind: "time", StartMS: 5, EndMS: 5}, // empty window
		{Kind: "time", StartMS: -1, EndMS: 9},
	} {
		if _, err := w.loadMediaInput(context.Background(), avChunk("audio", span)); !errors.Is(err, ErrFatal) {
			t.Fatalf("span %+v: expected fatal error, got %v", span, err)
		}
	}
}

// TestLoadMediaInput_SegmentExtractFails surfaces an extractor failure as a
// fatal task error.
func TestLoadMediaInput_SegmentExtractFails(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "clip.mp3"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	withExtractSegment(t, func(context.Context, string, int, int) ([]byte, error) {
		return nil, errors.New("ffmpeg missing")
	})
	w := &EmbeddingWorker{RootDir: root}
	if _, err := w.loadMediaInput(context.Background(), avChunk("audio", model.Span{Kind: "time", StartMS: 0, EndMS: 1000})); !errors.Is(err, ErrFatal) {
		t.Fatalf("expected fatal error on extract failure, got %v", err)
	}
}
