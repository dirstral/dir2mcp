package avutil

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// synthClip renders a short clip at path using ffmpeg's lavfi sources. When
// withAudio is false the clip carries only a video stream, so it exercises the
// no-audio-track degradation path. It fails the test if ffmpeg cannot render.
func synthClip(t *testing.T, path string, withAudio bool) {
	t.Helper()
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatalf("ffmpeg required to synthesize a test clip: %v", err)
	}
	args := []string{"-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=160x120:rate=15"}
	if withAudio {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-shortest")
	}
	args = append(args, "-c:v", "libx264", "-pix_fmt", "yuv420p")
	if withAudio {
		args = append(args, "-c:a", "aac")
	}
	args = append(args, path)
	cmd := exec.CommandContext(context.Background(), bin, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("synthesize clip %q: %v: %s", path, err, out)
	}
}

// TestExtractAudioTrack_Integration exercises the real ffmpeg audio-extraction
// path (issue #495): a video clip yields a non-empty audio-only clip that probes
// as carrying an audio (and no video) stream, and a video with no audio stream
// returns ErrNoAudioStream so callers degrade gracefully. Gated behind
// RUN_INTEGRATION_TESTS and skipped when ffmpeg/ffprobe are absent.
func TestExtractAudioTrack_Integration(t *testing.T) {
	if os.Getenv("RUN_INTEGRATION_TESTS") == "" {
		t.Skip("set RUN_INTEGRATION_TESTS=1 to run integration tests")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()

	t.Run("video with audio yields an audio-only clip", func(t *testing.T) {
		src := filepath.Join(dir, "clip.mp4")
		synthClip(t, src, true)

		audio, err := ExtractAudioTrack(ctx, src)
		if err != nil {
			t.Fatalf("ExtractAudioTrack: %v", err)
		}
		if len(audio) == 0 {
			t.Fatal("ExtractAudioTrack returned empty audio")
		}

		out := filepath.Join(dir, "extracted.m4a")
		if err := os.WriteFile(out, audio, 0o644); err != nil {
			t.Fatalf("write extracted audio: %v", err)
		}
		info, err := ProbeMediaInfo(ctx, out)
		if err != nil {
			t.Fatalf("probe extracted audio: %v", err)
		}
		if info.AudioCodec == "" {
			t.Errorf("extracted clip has no audio stream: %+v", info)
		}
		if info.VideoCodec != "" {
			t.Errorf("extracted clip still carries a video stream (%q); -vn should have dropped it", info.VideoCodec)
		}
	})

	t.Run("video with no audio returns ErrNoAudioStream", func(t *testing.T) {
		src := filepath.Join(dir, "silent.mp4")
		synthClip(t, src, false)

		if _, err := ExtractAudioTrack(ctx, src); !errors.Is(err, ErrNoAudioStream) {
			t.Fatalf("ExtractAudioTrack on a soundless video = %v, want ErrNoAudioStream", err)
		}
	})
}
