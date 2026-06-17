package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/store"
)

// processMediaWithWindowCfg ingests a single media file under augment mode with
// a stubbed duration probe and the given configured window seconds (0 = unset,
// falls back to the built-in constant), returning the per-modality time-window
// chunk count and the last window end (ms).
func processMediaWithWindowCfg(t *testing.T, name, modality string, dur time.Duration, audioSec, videoSec int) (windows, lastEndMS int) {
	t.Helper()
	t.Setenv("GEMINI_API_KEY", "gk")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, name), []byte("MEDIADATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadMultimodalConfig(t, root, "augment")
	cfg.STTProvider = "off"
	cfg.MediaAudioWindowSec = audioSec
	cfg.MediaVideoWindowSec = videoSec

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := mustNewIngestService(t, cfg, st)
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return dur, nil }

	df := ingest.DiscoveredFile{AbsPath: filepath.Join(root, name), RelPath: name, SizeBytes: int64(len("MEDIADATA"))}
	if err := svc.ProcessDocument(context.Background(), df, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	tasks, err := st.NextPending(context.Background(), 100, "text")
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	for _, tk := range tasks {
		if tk.Modality != modality || tk.Metadata.Span.Kind != "time" {
			continue
		}
		windows++
		if tk.Metadata.Span.EndMS > lastEndMS {
			lastEndMS = tk.Metadata.Span.EndMS
		}
	}
	return windows, lastEndMS
}

// TestMediaWindow_ConfiguredAudioWindowHonored asserts a configured audio window
// overrides the 120 s default: at 300 s with a 60 s window => 5 windows.
func TestMediaWindow_ConfiguredAudioWindowHonored(t *testing.T) {
	windows, last := processMediaWithWindowCfg(t, "talk.mp3", "audio", 300*time.Second, 60, 0)
	if windows != 5 {
		t.Fatalf("audio windows = %d, want 5 (60s window over 300s)", windows)
	}
	if last != 300000 {
		t.Errorf("last audio window end = %dms, want 300000", last)
	}
}

// TestMediaWindow_ConfiguredVideoWindowHonored asserts a configured video window
// overrides the 60 s default: at 300 s with a 100 s window => 3 windows.
func TestMediaWindow_ConfiguredVideoWindowHonored(t *testing.T) {
	windows, last := processMediaWithWindowCfg(t, "clip.mp4", "video", 300*time.Second, 0, 100)
	if windows != 3 {
		t.Fatalf("video windows = %d, want 3 (100s window over 300s)", windows)
	}
	if last != 300000 {
		t.Errorf("last video window end = %dms, want 300000", last)
	}
}

// TestMediaWindow_UnsetFallsBackToDefault asserts an unset (0) window keeps the
// built-in default: video defaults to 60 s, so 180 s => 3 windows (unchanged
// behavior).
func TestMediaWindow_UnsetFallsBackToDefault(t *testing.T) {
	windows, _ := processMediaWithWindowCfg(t, "clip.mp4", "video", 180*time.Second, 0, 0)
	if windows != 3 {
		t.Fatalf("video windows with unset config = %d, want 3 (60s default)", windows)
	}
}

// TestMediaWindow_AudioClampedToCap asserts an over-cap audio window is clamped
// to the 180 s per-modality cap (SPEC 8.1.7): a 600 s window over 360 s of media
// is clamped to 180 s => 2 windows (not 1, which an un-clamped 600 s would give).
func TestMediaWindow_AudioClampedToCap(t *testing.T) {
	windows, last := processMediaWithWindowCfg(t, "talk.mp3", "audio", 360*time.Second, 600, 0)
	if windows != 2 {
		t.Fatalf("audio windows = %d, want 2 (over-cap window clamped to 180s)", windows)
	}
	if last != 360000 {
		t.Errorf("last audio window end = %dms, want 360000", last)
	}
}

// TestMediaWindow_VideoClampedToCap asserts an over-cap video window is clamped
// to the 120 s per-modality cap (SPEC 8.1.7): a 600 s window over 240 s is
// clamped to 120 s => 2 windows.
func TestMediaWindow_VideoClampedToCap(t *testing.T) {
	windows, last := processMediaWithWindowCfg(t, "clip.mp4", "video", 240*time.Second, 0, 600)
	if windows != 2 {
		t.Fatalf("video windows = %d, want 2 (over-cap window clamped to 120s)", windows)
	}
	if last != 240000 {
		t.Errorf("last video window end = %dms, want 240000", last)
	}
}
