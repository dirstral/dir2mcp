package tests

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/avutil"
	"github.com/dirstral/dir2mcp/internal/ingest"
)

// TestVideoContainers_Integration encodes a short clip into each container
// that ingest classifies as video and runs the media path on it: the content
// classification, Duration, a stream-copied segment and the audio track for
// transcription. A camcorder .mod file is an MPEG program stream; ffmpeg has
// no muxer for the ".mod" extension, so the segment must still come out.
func TestVideoContainers_Integration(t *testing.T) {
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"clip.mkv", nil},
		{"clip.m4v", []string{"-f", "mp4"}},
		{"clip.avi", []string{"-c:v", "mpeg4", "-c:a", "mp3"}},
		{"clip.flv", []string{"-c:v", "flv", "-c:a", "mp3", "-ar", "44100"}},
		{"clip.wmv", []string{"-c:v", "wmv2", "-c:a", "wmav2"}},
		{"clip.mpg", []string{"-c:v", "mpeg2video", "-c:a", "mp2"}},
		{"clip.vob", []string{"-c:v", "mpeg2video", "-c:a", "mp2"}},
		{"clip.m2ts", nil},
		{"news.ts", nil},
		{"cam.mts", nil},
		{"MOV001.MOD", []string{"-f", "mpeg", "-c:v", "mpeg2video", "-c:a", "mp2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			src := filepath.Join(t.TempDir(), tc.name)
			args := []string{"-v", "error", "-y",
				"-f", "lavfi", "-i", "testsrc=d=4:s=176x144:r=25",
				"-f", "lavfi", "-i", "sine=d=4"}
			args = append(args, tc.args...)
			args = append(args, src)
			if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
				t.Skipf("this ffmpeg cannot encode %s: %v: %s", tc.name, err, out)
			}

			content, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			if got := ingest.SniffTextDocType(ingest.ClassifyDocType(tc.name), tc.name, content); got != "video" {
				t.Fatalf("classification of %s = %q, want video", tc.name, got)
			}

			d, err := avutil.Duration(ctx, src)
			if err != nil {
				t.Fatalf("Duration: %v", err)
			}
			if d < 3*time.Second || d > 5*time.Second {
				t.Fatalf("Duration = %v, want ~4s", d)
			}
			seg, err := avutil.ExtractSegment(ctx, src, 1000, 2500)
			if err != nil {
				t.Fatalf("ExtractSegment: %v", err)
			}
			if len(seg) == 0 {
				t.Fatal("ExtractSegment returned no bytes")
			}
			audio, err := avutil.ExtractAudioTrack(ctx, src)
			if err != nil {
				t.Fatalf("ExtractAudioTrack: %v", err)
			}
			if len(audio) == 0 {
				t.Fatal("ExtractAudioTrack returned no bytes")
			}
		})
	}
}
