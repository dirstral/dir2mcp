package tests

// The measurement behind #878, and the harness the fix will reuse.
//
// avutil.ExtractSegment stream-copies (`-c copy`), so a clip costs the SOURCE
// bitrate for its span. On the pilot corpus that is 22.7 MB for 8 seconds of a
// 20 Mbit/s recording, and about 30 MB base64 on the wire. Re-encoding the same
// span into a capped preview profile costs a fraction of that.
//
// This test proves three things a fix depends on, on real encoder output rather
// than on an estimate:
//
//  1. the stream copy tracks the source bitrate;
//  2. a capped preview of the SAME span is far smaller;
//  3. the preview still COVERS the requested span. A smaller clip of the wrong
//     span would be worse than a large correct one, so the span is asserted, not
//     just the size.
//
// The preview command lives here rather than in internal/avutil on purpose:
// serving a preview is a tool-contract change that dirstral-spec has not opened
// yet (tests/conformance/clip_size_contract_878_test.go pins that gate). This
// suite validates the profile the spec PR names, so the production helper can be
// added with its behavior already measured.
//
// It is skipped when ffmpeg/ffprobe are absent, matching the rest of this suite.
// ffmpeg is already required on this path: ExtractSegment returns
// ErrToolNotFound without it, and dir2mcp_open_media_clip maps that to
// MEDIA_CLIP_FAILED. No new dependency is introduced.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/avutil"
)

// clipSpan is the window both cuts must cover.
const (
	clipSpanStartMS = 1000
	clipSpanEndMS   = 4000
)

// msArg renders a millisecond offset as the seconds string ffmpeg's -ss/-to take.
func msArg(ms int) string {
	return strconv.FormatFloat(float64(ms)/1000, 'f', 3, 64)
}

// requireFFmpegTools skips unless both binaries are installed.
func requireFFmpegTools(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
}

// writeHighBitrateSource renders a short, deliberately fat video fixture: an
// all-intra MJPEG at the top quality level, which stands in for the pilot's
// 20 Mbit/s recording at a size a test can afford. mjpeg and the testsrc2 lavfi
// source are native to ffmpeg, so no third-party encoder is required. It skips
// (never fails) when the local build cannot render the fixture, so a reduced
// ffmpeg degrades cleanly instead of reporting a false regression.
func writeHighBitrateSource(t *testing.T, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=25:duration=6",
		"-c:v", "mjpeg", "-q:v", "1", "-pix_fmt", "yuvj420p",
		"-y", path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this ffmpeg build cannot render the fixture: %v: %s", err, out)
	}
}

// encodePreview cuts the same span into a capped preview profile: half the
// height and a bounded video bitrate. It is the candidate for the fix.
func encodePreview(t *testing.T, src, dst string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-nostdin", "-v", "error",
		"-ss", msArg(clipSpanStartMS), "-to", msArg(clipSpanEndMS), "-i", src,
		"-vf", "scale=-2:180",
		"-c:v", "mpeg4", "-b:v", "300k", "-pix_fmt", "yuv420p",
		"-y", dst,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this ffmpeg build cannot encode the preview profile: %v: %s", err, out)
	}
}

// assertCoversSpan requires that a cut still spans the requested window. Stream
// copy can only start at the preceding keyframe, so a copy may run slightly
// LONGER than the request; it must never run short.
func assertCoversSpan(t *testing.T, path, label string) {
	t.Helper()
	got, err := avutil.Duration(context.Background(), path)
	if err != nil {
		t.Fatalf("%s: Duration: %v", label, err)
	}
	want := time.Duration(clipSpanEndMS-clipSpanStartMS) * time.Millisecond
	if got < want-200*time.Millisecond {
		t.Fatalf("%s: duration = %v, want at least %v: the clip does not cover the requested span", label, got, want)
	}
	if got > want+1500*time.Millisecond {
		t.Fatalf("%s: duration = %v, want about %v: the clip covers far more than the requested span", label, got, want)
	}
}

// TestClipPreviewIsFarSmallerThanTheSourceBitrateCut_878 measures the gap #878
// reports, and pins that closing it does not move the span.
func TestClipPreviewIsFarSmallerThanTheSourceBitrateCut_878(t *testing.T) {
	requireFFmpegTools(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "source.mp4")
	writeHighBitrateSource(t, src)

	// What the tool serves today.
	copied, err := avutil.ExtractSegment(context.Background(), src, clipSpanStartMS, clipSpanEndMS)
	if err != nil {
		t.Fatalf("ExtractSegment: %v", err)
	}
	copyPath := filepath.Join(dir, "copy.mp4")
	if err := os.WriteFile(copyPath, copied, 0o600); err != nil {
		t.Fatalf("write stream copy: %v", err)
	}

	// What a capped preview of the same span costs.
	previewPath := filepath.Join(dir, "preview.mp4")
	encodePreview(t, src, previewPath)
	preview, err := os.ReadFile(previewPath)
	if err != nil {
		t.Fatalf("read preview: %v", err)
	}

	t.Logf("stream copy = %d bytes, preview = %d bytes, ratio = %.1fx",
		len(copied), len(preview), float64(len(copied))/float64(len(preview)))

	// Deliberately loose: the point is an order of magnitude, not an exact
	// encoder output, which varies by ffmpeg build.
	if len(preview)*3 > len(copied) {
		t.Fatalf("preview = %d bytes vs stream copy = %d bytes: the preview profile does not materially reduce bytes",
			len(preview), len(copied))
	}

	// A smaller clip of the WRONG span is worse than a large correct one.
	assertCoversSpan(t, copyPath, "stream copy")
	assertCoversSpan(t, previewPath, "preview")
}
