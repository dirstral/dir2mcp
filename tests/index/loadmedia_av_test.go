package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
)

func avTask(label uint64, ref, modality string, span model.Span) model.ChunkTask {
	tk := model.NewChunkTask(label, "", "text", model.ChunkMetadata{ChunkID: label, RelPath: ref, DocType: modality, Span: span})
	tk.Modality = modality
	tk.MediaRef = ref
	return tk
}

// TestEmbeddingWorker_RunOnce_AudioVideoSegmentExtracted pins SPEC 8.1.7: an
// audio/video media chunk embeds only its time window — the worker hands the
// chunk's [start,end) span to ExtractSegmentFunc and embeds the returned bytes,
// with the MIME inferred from the source extension.
func TestEmbeddingWorker_RunOnce_AudioVideoSegmentExtracted(t *testing.T) {
	for _, tc := range []struct {
		modality, ref, mime string
	}{
		{"audio", "talk.mp3", "audio/mp3"},
		{"video", "clip.mp4", "video/mp4"},
	} {
		t.Run(tc.modality, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, tc.ref), []byte("FULLMEDIA"), 0o600); err != nil {
				t.Fatal(err)
			}
			var gotStart, gotEnd int
			var gotPath string
			emb := &fakeMultimodalEmbedder{mediaVecs: [][]float32{{0.6, 0.8}}}
			source := &fakeChunkSource{tasks: []model.ChunkTask{
				avTask(7, tc.ref, tc.modality, model.Span{Kind: "time", StartMS: 1000, EndMS: 3000}),
			}}
			worker := &index.EmbeddingWorker{
				Source: source, Index: index.NewHNSWIndex(""), Embedder: emb,
				RootDir: root, BatchSize: 4, ModelForText: "gemini-embedding-2",
				ExtractSegmentFunc: func(_ context.Context, path string, s, e int) ([]byte, error) {
					gotPath, gotStart, gotEnd = path, s, e
					return []byte("SEGMENT"), nil
				},
			}

			n, err := worker.RunOnce(context.Background(), "text")
			if err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if n != 1 {
				t.Fatalf("indexed = %d, want 1", n)
			}
			if len(emb.gotMedia) != 1 || string(emb.gotMedia[0].Data) != "SEGMENT" {
				t.Fatalf("EmbedMedia got %+v, want the extracted segment bytes", emb.gotMedia)
			}
			if emb.gotMedia[0].MimeType != tc.mime {
				t.Errorf("mime = %q, want %q", emb.gotMedia[0].MimeType, tc.mime)
			}
			if gotStart != 1000 || gotEnd != 3000 {
				t.Errorf("segment span = [%d,%d), want [1000,3000)", gotStart, gotEnd)
			}
			if filepath.Base(gotPath) != tc.ref {
				t.Errorf("extracted from %q, want basename %q", gotPath, tc.ref)
			}
		})
	}
}

// TestEmbeddingWorker_RunOnce_InvalidTimeSpanFails: an audio/video chunk without
// a valid time span is a fatal task error (surface the mismatch instead of
// embedding the wrong segment), mirroring the PDF page-span guard.
func TestEmbeddingWorker_RunOnce_InvalidTimeSpanFails(t *testing.T) {
	for _, span := range []model.Span{
		{Kind: "page", Page: 1},               // wrong kind
		{Kind: "time", StartMS: 5, EndMS: 5},  // empty window
		{Kind: "time", StartMS: -1, EndMS: 9}, // negative start
	} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "talk.mp3"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		source := &fakeChunkSource{tasks: []model.ChunkTask{avTask(6, "talk.mp3", "audio", span)}}
		worker := &index.EmbeddingWorker{
			Source: source, Index: index.NewHNSWIndex(""), Embedder: &fakeMultimodalEmbedder{mediaVecs: [][]float32{{1, 0}}},
			RootDir: root, BatchSize: 4, ModelForText: "gemini-embedding-2",
			ExtractSegmentFunc: func(context.Context, string, int, int) ([]byte, error) {
				t.Fatal("extractor must not be called for an invalid span")
				return nil, nil
			},
		}
		n, err := worker.RunOnce(context.Background(), "text")
		if !errors.Is(err, index.ErrFatal) {
			t.Fatalf("span %+v: expected fatal error, got %v", span, err)
		}
		if n != 0 {
			t.Fatalf("span %+v: indexed = %d, want 0", span, n)
		}
		if len(source.failedLabels) != 1 || source.failedLabels[0] != 6 {
			t.Fatalf("span %+v: failed labels = %#v, want [6]", span, source.failedLabels)
		}
	}
}

// TestEmbeddingWorker_RunOnce_SegmentExtractFails surfaces an extractor failure
// (e.g. ffmpeg missing) as a fatal task error.
func TestEmbeddingWorker_RunOnce_SegmentExtractFails(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "clip.mp4"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := &fakeChunkSource{tasks: []model.ChunkTask{
		avTask(8, "clip.mp4", "video", model.Span{Kind: "time", StartMS: 0, EndMS: 1000}),
	}}
	worker := &index.EmbeddingWorker{
		Source: source, Index: index.NewHNSWIndex(""), Embedder: &fakeMultimodalEmbedder{mediaVecs: [][]float32{{1, 0}}},
		RootDir: root, BatchSize: 4, ModelForText: "gemini-embedding-2",
		ExtractSegmentFunc: func(context.Context, string, int, int) ([]byte, error) {
			return nil, errors.New("ffmpeg missing")
		},
	}
	if _, err := worker.RunOnce(context.Background(), "text"); !errors.Is(err, index.ErrFatal) {
		t.Fatalf("expected fatal error on extract failure, got %v", err)
	}
}
