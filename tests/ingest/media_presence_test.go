package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestChunkModalityPresence_AfterIngest validates the SQL backing the
// MEDIA_NO_TEXT classification (SPEC 8.1.7): a replace-mode media file yields a
// media-only document (media chunk, no text), while a text document yields the
// opposite.
func TestChunkModalityPresence_AfterIngest(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "gk")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "pic.png"), []byte("PNGDATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("# Notes\n\nhello"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadMultimodalConfig(t, root, "replace")
	cfg.STTProvider = "off"
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	defer func() { _ = st.Close() }()

	svc := mustNewIngestService(t, cfg, st)
	for _, name := range []string{"pic.png", "notes.md"} {
		df := ingest.DiscoveredFile{AbsPath: filepath.Join(root, name), RelPath: name}
		if err := svc.ProcessDocument(context.Background(), df, nil, false); err != nil {
			t.Fatalf("ProcessDocument(%s): %v", name, err)
		}
	}

	// replace-mode image: media chunk, no text representation -> media-only.
	hasMedia, hasText, err := st.ChunkModalityPresence(context.Background(), "pic.png")
	if err != nil {
		t.Fatalf("ChunkModalityPresence(pic.png): %v", err)
	}
	if !hasMedia || hasText {
		t.Fatalf("pic.png: got (hasMedia=%v, hasText=%v), want (true, false)", hasMedia, hasText)
	}

	// markdown: text chunk, no media -> not media-only.
	hasMedia, hasText, err = st.ChunkModalityPresence(context.Background(), "notes.md")
	if err != nil {
		t.Fatalf("ChunkModalityPresence(notes.md): %v", err)
	}
	if hasMedia || !hasText {
		t.Fatalf("notes.md: got (hasMedia=%v, hasText=%v), want (false, true)", hasMedia, hasText)
	}
}
