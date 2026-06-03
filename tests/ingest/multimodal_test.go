package tests

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// makeTestPDF renders an n-page PDF for ingestion tests.
func makeTestPDF(t *testing.T, n int) []byte {
	t.Helper()
	api.DisableConfigDir()
	parts := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		parts = append(parts, fmt.Sprintf(`"%d":{"content":{"text":[{"value":"page","position":[100,700],"font":{"name":"Helvetica","size":12}}]}}`, i))
	}
	js := `{"pages":{` + strings.Join(parts, ",") + `}}`
	var buf bytes.Buffer
	if err := api.Create(nil, strings.NewReader(js), &buf, model.NewDefaultConfiguration()); err != nil {
		t.Fatalf("create %d-page pdf: %v", n, err)
	}
	return buf.Bytes()
}

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

// TestProcessDocument_AugmentEmitsPdfPageChunks pins SPEC 8.1.7: under
// augment, a PDF yields one media chunk per page (modality=pdf, media_ref set,
// a page span per page) for direct per-page multimodal embedding.
func TestProcessDocument_AugmentEmitsPdfPageChunks(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "gk")
	root := t.TempDir()
	pdf := makeTestPDF(t, 2)
	if err := os.WriteFile(filepath.Join(root, "doc.pdf"), pdf, 0o644); err != nil {
		t.Fatal(err)
	}
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	defer func() { _ = st.Close() }()

	svc := mustNewIngestService(t, loadMultimodalConfig(t, root, "augment"), st)
	df := ingest.DiscoveredFile{AbsPath: filepath.Join(root, "doc.pdf"), RelPath: "doc.pdf", SizeBytes: int64(len(pdf))}
	if err := svc.ProcessDocument(context.Background(), df, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}

	tasks, err := st.NextPending(context.Background(), 10, "text")
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	pages := map[int]bool{}
	for _, tk := range tasks {
		if tk.Modality != "pdf" {
			continue
		}
		if tk.MediaRef != "doc.pdf" {
			t.Errorf("media_ref = %q, want doc.pdf", tk.MediaRef)
		}
		pages[tk.Metadata.Span.Page] = true
	}
	if !pages[1] || !pages[2] || len(pages) != 2 {
		t.Fatalf("want pdf media chunks for pages {1,2}, got %v (tasks=%d)", pages, len(tasks))
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
