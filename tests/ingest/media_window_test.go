package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/store"
)

// processMediaForWindows ingests a single media file under the given multimodal
// mode with a stubbed duration probe (so ffprobe is not required) and returns
// the pending chunk tasks.
func processMediaForWindows(t *testing.T, mode, name string, dur time.Duration) []struct {
	Modality string
	MediaRef string
	Text     string
	Kind     string
	EndMS    int
} {
	t.Helper()
	t.Setenv("GEMINI_API_KEY", "gk")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, name), []byte("MEDIADATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadMultimodalConfig(t, root, mode)
	cfg.STTProvider = "off" // no transcript path in these unit tests
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
	tasks, err := st.NextPending(context.Background(), 50, "text")
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	out := make([]struct {
		Modality string
		MediaRef string
		Text     string
		Kind     string
		EndMS    int
	}, 0, len(tasks))
	for _, tk := range tasks {
		out = append(out, struct {
			Modality string
			MediaRef string
			Text     string
			Kind     string
			EndMS    int
		}{tk.Modality, tk.MediaRef, tk.Text, tk.Metadata.Span.Kind, tk.Metadata.Span.EndMS})
	}
	return out
}

// TestProcessDocument_VideoWindows pins SPEC 8.1.7: under augment a video yields
// one media chunk per time window (here 150s / 60s => 3 windows), each with a
// time span, the last ending at the full duration.
func TestProcessDocument_VideoWindows(t *testing.T) {
	chunks := processMediaForWindows(t, "augment", "clip.mp4", 150*time.Second)
	var windows, last int
	for _, c := range chunks {
		if c.Modality != "video" {
			continue
		}
		windows++
		if c.MediaRef != "clip.mp4" || c.Kind != "time" || c.Text != "" {
			t.Errorf("unexpected video chunk: %+v", c)
		}
		if c.EndMS > last {
			last = c.EndMS
		}
	}
	if windows != 3 {
		t.Fatalf("video windows = %d, want 3 (chunks=%v)", windows, chunks)
	}
	if last != 150000 {
		t.Errorf("last window end = %dms, want 150000", last)
	}
}

// TestProcessDocument_AudioMP3Windows pins SPEC 8.1.7: a supported audio format
// (MP3) is windowed (150s / 120s => 2 windows) for direct embedding.
func TestProcessDocument_AudioMP3Windows(t *testing.T) {
	chunks := processMediaForWindows(t, "augment", "talk.mp3", 150*time.Second)
	var windows, last int
	for _, c := range chunks {
		if c.Modality != "audio" {
			continue
		}
		windows++
		if c.Kind != "time" {
			t.Errorf("audio chunk span kind = %q, want time", c.Kind)
		}
		if c.EndMS > last {
			last = c.EndMS
		}
	}
	if windows != 2 {
		t.Fatalf("audio windows = %d, want 2 (chunks=%v)", windows, chunks)
	}
	if last != 150000 {
		t.Errorf("last window end = %dms, want 150000", last)
	}
}

// TestProcessDocument_AudioUnsupportedFormatNoMedia pins SPEC 8.1.7: an audio
// format the model does not accept directly (FLAC) produces no media chunk; it
// keeps only its (here disabled) transcript path.
func TestProcessDocument_AudioUnsupportedFormatNoMedia(t *testing.T) {
	chunks := processMediaForWindows(t, "augment", "song.flac", 90*time.Second)
	for _, c := range chunks {
		if c.Modality == "audio" {
			t.Fatalf("flac must not yield a direct media chunk, got %+v", c)
		}
	}
}

// TestProcessDocument_UndecodableDurationSkipsMedia pins the SPEC 8.1.7
// fallback: when the duration probe fails (undecodable, or ffprobe absent), no
// media chunk is produced and the ingest still succeeds.
func TestProcessDocument_UndecodableDurationSkipsMedia(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "gk")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "broken.mp4"), []byte("MEDIADATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadMultimodalConfig(t, root, "replace")
	cfg.STTProvider = "off"
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := mustNewIngestService(t, cfg, st)
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) {
		return 0, errors.New("ffprobe boom")
	}
	df := ingest.DiscoveredFile{AbsPath: filepath.Join(root, "broken.mp4"), RelPath: "broken.mp4", SizeBytes: 9}
	if err := svc.ProcessDocument(context.Background(), df, nil, false); err != nil {
		t.Fatalf("ProcessDocument must not fail on undecodable media: %v", err)
	}
	tasks, err := st.NextPending(context.Background(), 50, "text")
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	for _, tk := range tasks {
		if tk.Modality == "video" {
			t.Fatalf("undecodable video must yield no media chunk, got %+v", tk)
		}
	}
}

// TestProcessDocument_VideoOffNoMedia confirms off mode is behavior-preserving:
// a video yields no media chunk.
func TestProcessDocument_VideoOffNoMedia(t *testing.T) {
	chunks := processMediaForWindows(t, "off", "clip.mp4", 150*time.Second)
	for _, c := range chunks {
		if c.Modality == "video" {
			t.Fatalf("off mode must not emit media chunks, got %+v", c)
		}
	}
}
