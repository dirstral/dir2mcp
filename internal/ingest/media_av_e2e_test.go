package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/store"
)

// loadAVMultimodalConfig writes a config binding embed to Gemini's multimodal
// model with the given mode, rooted at root.
func loadAVMultimodalConfig(t *testing.T, root, mode string) config.Config {
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
	// No STT credential in unit tests; disable transcription (not needed for
	// the video media path).
	cfg.STTProvider = "off"
	return cfg
}

// TestProcessDocument_AugmentEmitsVideoTimeChunks pins SPEC 8.1.7 end-to-end:
// under augment, a video yields one media chunk per time window (modality=video,
// media_ref set, a time span per window) for direct multimodal embedding. The
// duration probe is stubbed so the test does not require ffprobe.
func TestProcessDocument_AugmentEmitsVideoTimeChunks(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "gk")
	// 150s with the 60s video window => 3 windows.
	withProbeDuration(t, func(context.Context, string) (time.Duration, error) { return 150 * time.Second, nil })

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "clip.mp4"), []byte("MP4DATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	defer func() { _ = st.Close() }()

	svc, err := NewService(loadAVMultimodalConfig(t, root, "augment"), st)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	df := DiscoveredFile{AbsPath: filepath.Join(root, "clip.mp4"), RelPath: "clip.mp4", SizeBytes: int64(len("MP4DATA"))}
	if err := svc.ProcessDocument(context.Background(), df, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}

	tasks, err := st.NextPending(context.Background(), 10, "text")
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	var windows int
	var lastEnd int
	for _, tk := range tasks {
		if tk.Modality != "video" {
			continue
		}
		windows++
		if tk.MediaRef != "clip.mp4" {
			t.Errorf("media_ref = %q, want clip.mp4", tk.MediaRef)
		}
		if tk.Metadata.Span.Kind != "time" {
			t.Errorf("span kind = %q, want time", tk.Metadata.Span.Kind)
		}
		if tk.Text != "" {
			t.Errorf("media chunk text = %q, want empty", tk.Text)
		}
		if tk.Metadata.Span.EndMS > lastEnd {
			lastEnd = tk.Metadata.Span.EndMS
		}
	}
	if windows != 3 {
		t.Fatalf("got %d video media chunks, want 3 (tasks=%d)", windows, len(tasks))
	}
	if lastEnd != 150000 {
		t.Errorf("last window end = %dms, want 150000", lastEnd)
	}
}
