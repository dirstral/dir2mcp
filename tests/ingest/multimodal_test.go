package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/store"
)

// loadMultimodalConfig writes a config binding embed to Gemini's multimodal
// model with the given mode and returns the loaded config rooted at root.
func loadMultimodalConfig(t *testing.T, root, mode string) config.Config {
	t.Helper()
	yaml := "version: 1\n" +
		"model:\n" +
		"  embed:\n" +
		"    provider: gemini\n" +
		"    text_model: gemini-embedding-2\n" +
		"    code_model: gemini-embedding-2\n" +
		"    multimodal: " + mode + "\n"
	cfgPath := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.LoadFile(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.RootDir = root
	return cfg
}

// TestProcessDocument_AugmentEmitsImageMediaChunk pins SPEC 8.1.7 ingestion:
// under model.embed.multimodal=augment, an image document yields a pending
// media chunk (modality=image, media_ref=relpath, empty text) for direct
// multimodal embedding by the worker.
func TestProcessDocument_AugmentEmitsImageMediaChunk(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "gk")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "pic.png"), []byte("PNGDATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	defer func() { _ = st.Close() }()

	svc := mustNewIngestService(t, loadMultimodalConfig(t, root, "augment"), st)
	df := ingest.DiscoveredFile{
		AbsPath:   filepath.Join(root, "pic.png"),
		RelPath:   "pic.png",
		SizeBytes: int64(len("PNGDATA")),
	}
	if err := svc.ProcessDocument(context.Background(), df, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}

	tasks, err := st.NextPending(context.Background(), 10, "text")
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	var media int
	for _, tk := range tasks {
		if tk.Modality == "image" {
			media++
			if tk.MediaRef != "pic.png" {
				t.Errorf("media_ref = %q, want pic.png", tk.MediaRef)
			}
			if tk.Text != "" {
				t.Errorf("media chunk text = %q, want empty", tk.Text)
			}
		}
	}
	if media != 1 {
		t.Fatalf("got %d image media chunks, want 1 (tasks=%d)", media, len(tasks))
	}
}

// TestProcessDocument_OffEmitsNoMediaChunk confirms the default (off) mode is
// behavior-preserving: an image yields no media chunk.
func TestProcessDocument_OffEmitsNoMediaChunk(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "gk")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "pic.png"), []byte("PNGDATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	defer func() { _ = st.Close() }()

	svc := mustNewIngestService(t, loadMultimodalConfig(t, root, "off"), st)
	df := ingest.DiscoveredFile{AbsPath: filepath.Join(root, "pic.png"), RelPath: "pic.png", SizeBytes: 7}
	if err := svc.ProcessDocument(context.Background(), df, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	tasks, err := st.NextPending(context.Background(), 10, "text")
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	for _, tk := range tasks {
		if tk.Modality == "image" {
			t.Fatalf("off mode must not emit media chunks, got %+v", tk)
		}
	}
}
