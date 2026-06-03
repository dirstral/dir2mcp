package ingest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TestWindowSpans pins the deterministic time-window math (SPEC 8.1.7):
// contiguous, non-overlapping half-open windows of at most windowMS, the last
// holding the remainder.
func TestWindowSpans(t *testing.T) {
	cases := []struct {
		name     string
		totalMS  int
		windowMS int
		want     []model.Span
	}{
		{"exact-multiple", 2000, 1000, []model.Span{
			{Kind: "time", StartMS: 0, EndMS: 1000},
			{Kind: "time", StartMS: 1000, EndMS: 2000},
		}},
		{"remainder", 2500, 1000, []model.Span{
			{Kind: "time", StartMS: 0, EndMS: 1000},
			{Kind: "time", StartMS: 1000, EndMS: 2000},
			{Kind: "time", StartMS: 2000, EndMS: 2500},
		}},
		{"single-short", 400, 1000, []model.Span{
			{Kind: "time", StartMS: 0, EndMS: 400},
		}},
		{"zero-total", 0, 1000, nil},
		{"zero-window", 1000, 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := windowSpans(tc.totalMS, tc.windowMS)
			if len(got) != len(tc.want) {
				t.Fatalf("windowSpans(%d,%d) = %v, want %v", tc.totalMS, tc.windowMS, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("span[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestWindowSpansDeterministic confirms identical inputs produce identical
// boundaries (stable citations across re-index, SPEC 8.1.7).
func TestWindowSpansDeterministic(t *testing.T) {
	a := windowSpans(7321, 2000)
	b := windowSpans(7321, 2000)
	if len(a) != len(b) {
		t.Fatalf("non-deterministic length: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("non-deterministic span[%d]: %+v vs %+v", i, a[i], b[i])
		}
	}
	// 7321ms / 2000ms => 4 windows (last = remainder 1321ms).
	if len(a) != 4 || a[3] != (model.Span{Kind: "time", StartMS: 6000, EndMS: 7321}) {
		t.Fatalf("unexpected windows: %+v", a)
	}
}

func TestIsEmbeddableAudio(t *testing.T) {
	for _, ok := range []string{"a.mp3", "a.wav", "dir/B.MP3", "x.WAV"} {
		if !isEmbeddableAudio(ok) {
			t.Errorf("isEmbeddableAudio(%q) = false, want true", ok)
		}
	}
	for _, no := range []string{"a.m4a", "a.flac", "a.aac", "a.ogg", "a.opus", "a.txt"} {
		if isEmbeddableAudio(no) {
			t.Errorf("isEmbeddableAudio(%q) = true, want false", no)
		}
	}
}

// withProbeDuration swaps the package duration probe for the test and restores
// it afterward.
func withProbeDuration(t *testing.T, fn func(context.Context, string) (time.Duration, error)) {
	t.Helper()
	prev := probeDuration
	probeDuration = fn
	t.Cleanup(func() { probeDuration = prev })
}

// TestMediaSpansFor pins SPEC 8.1.7 unit selection per modality/mode.
func TestMediaSpansFor(t *testing.T) {
	const dur = 150 * time.Second // 150s with 120s audio / 60s video windows
	withProbeDuration(t, func(context.Context, string) (time.Duration, error) { return dur, nil })

	svc := &Service{embedMultimodal: "augment"}
	svc.cfg = config.Config{RootDir: t.TempDir()}
	ctx := context.Background()

	t.Run("image-one-page", func(t *testing.T) {
		got := svc.mediaSpansFor(ctx, model.Document{DocType: "image", RelPath: "p.png"}, nil)
		if len(got) != 1 || got[0].Kind != "page" || got[0].Page != 1 {
			t.Fatalf("image spans = %+v, want one page span", got)
		}
	})

	t.Run("audio-mp3-windows", func(t *testing.T) {
		got := svc.mediaSpansFor(ctx, model.Document{DocType: "audio", RelPath: "a.mp3"}, nil)
		// 150s / 120s window => 2 windows.
		if len(got) != 2 || got[0].Kind != "time" || got[1].EndMS != 150000 {
			t.Fatalf("audio spans = %+v, want 2 time windows ending at 150000", got)
		}
	})

	t.Run("audio-unsupported-format-skipped", func(t *testing.T) {
		if got := svc.mediaSpansFor(ctx, model.Document{DocType: "audio", RelPath: "a.flac"}, nil); got != nil {
			t.Fatalf("flac audio must not be directly embedded, got %+v", got)
		}
	})

	t.Run("video-windows", func(t *testing.T) {
		got := svc.mediaSpansFor(ctx, model.Document{DocType: "video", RelPath: "v.mp4"}, nil)
		// 150s / 60s window => 3 windows.
		if len(got) != 3 || got[2].EndMS != 150000 {
			t.Fatalf("video spans = %+v, want 3 time windows ending at 150000", got)
		}
	})

	t.Run("off-mode-nil", func(t *testing.T) {
		off := &Service{embedMultimodal: "off"}
		off.cfg = config.Config{RootDir: t.TempDir()}
		if got := off.mediaSpansFor(ctx, model.Document{DocType: "audio", RelPath: "a.mp3"}, nil); got != nil {
			t.Fatalf("off mode must yield no media spans, got %+v", got)
		}
	})
}

// TestMediaTimeSpans_UndecodableSkips confirms the graceful fallback: when the
// duration probe fails, no spans are produced (media skipped, text path kept).
func TestMediaTimeSpans_UndecodableSkips(t *testing.T) {
	withProbeDuration(t, func(context.Context, string) (time.Duration, error) {
		return 0, errors.New("ffprobe boom")
	})
	svc := &Service{embedMultimodal: "replace"}
	svc.cfg = config.Config{RootDir: t.TempDir()}
	if got := svc.mediaSpansFor(context.Background(), model.Document{DocType: "audio", RelPath: "a.wav"}, nil); got != nil {
		t.Fatalf("undecodable media must yield nil spans, got %+v", got)
	}
}
