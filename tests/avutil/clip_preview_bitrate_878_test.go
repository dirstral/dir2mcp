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
	"bytes"
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/avutil"
)

const (
	// clipSpanStartMS/clipSpanEndMS is the window both cuts must cover.
	clipSpanStartMS = 1000
	clipSpanEndMS   = 4000
	// probeBackoffMS keeps the end-of-span probe inside the last frame.
	probeBackoffMS = 100
	// lumaToleranceLevels is how far a re-encoded frame may drift from the
	// source frame it came from. Measured drift is under one level. The fixture
	// ramps about 22 luma levels per second, so 5 levels is about a fifth of a
	// second of timeline: tight enough to catch a misplaced cut.
	lumaToleranceLevels = 5
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

// writeHighBitrateSource renders a short video fixture with two properties the
// test needs at once.
//
// It is deliberately FAT: an all-intra MJPEG at the top quality level over a
// noisy pattern, which stands in for the pilot's 20 Mbit/s recording at a size a
// test can afford.
//
// It is also TIME-ENCODED: brightness ramps monotonically with the source
// timestamp, so a frame's mean luma says where on the source timeline it came
// from. That is what lets the span be asserted on CONTENT. A duration check
// alone would pass a 3 second clip cut from the wrong place, which is the
// failure this test exists to exclude.
//
// mjpeg, testsrc2 and eq are native to ffmpeg, so no third-party encoder is
// required. It skips (never fails) when the local build cannot render the
// fixture, so a reduced ffmpeg degrades cleanly instead of reporting a false
// regression.
func writeHighBitrateSource(t *testing.T, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=25:duration=6",
		"-vf", "eq=brightness='-0.3+0.1*t':eval=frame",
		"-c:v", "mjpeg", "-q:v", "1", "-pix_fmt", "yuvj420p",
		"-y", path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this ffmpeg build cannot render the fixture: %v: %s", err, out)
	}
}

// meanLuma returns the mean luma of the single frame at offsetMS. It is the
// timeline probe: on this fixture the value maps back to a source timestamp.
func meanLuma(t *testing.T, path string, offsetMS int) float64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-nostdin", "-v", "error",
		"-ss", msArg(offsetMS), "-i", path,
		"-frames:v", "1", "-f", "rawvideo", "-pix_fmt", "gray", "-",
	)
	var frame bytes.Buffer
	cmd.Stdout = &frame
	if err := cmd.Run(); err != nil {
		t.Fatalf("read the frame at %dms of %s: %v", offsetMS, filepath.Base(path), err)
	}
	pixels := frame.Bytes()
	if len(pixels) == 0 {
		t.Fatalf("no frame at %dms of %s", offsetMS, filepath.Base(path))
	}
	sum := 0
	for _, pixel := range pixels {
		sum += int(pixel)
	}
	return float64(sum) / float64(len(pixels))
}

// videoCodec returns the codec name of a file's first video stream.
func videoCodec(t *testing.T, path string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=codec_name",
		"-of", "default=nw=1:nk=1",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("probe the codec of %s: %v", filepath.Base(path), err)
	}
	return strings.TrimSpace(string(out))
}

// byteRate returns a file's mean bytes per second: its size over its duration.
// It is derived rather than read from the container, because a container may
// omit the bitrate tag while the file on the wire still costs what it costs.
func byteRate(t *testing.T, path string, sizeBytes int) float64 {
	t.Helper()
	duration, err := avutil.Duration(context.Background(), path)
	if err != nil {
		t.Fatalf("%s: Duration: %v", filepath.Base(path), err)
	}
	if duration <= 0 {
		t.Fatalf("%s: duration = %v, want > 0", filepath.Base(path), duration)
	}
	return float64(sizeBytes) / duration.Seconds()
}

// assertCutKeepsSourceFidelity pins the CURRENT default: ExtractSegment stream
// copies, so the clip keeps the source codec and costs the source bitrate.
//
// This is the baseline half of the measurement, and it is the guard the size
// comparison alone does not give. A future re-encode inside ExtractSegment could
// still leave the preview three times smaller and pass the size assertion while
// silently downgrading every caller that asked for nothing. That downgrade is
// exactly what #878 must NOT do without a spec change, so it is asserted here.
func assertCutKeepsSourceFidelity(t *testing.T, srcPath string, srcSize int, copyPath string, copySize int) {
	t.Helper()
	srcCodec := videoCodec(t, srcPath)
	copyCodec := videoCodec(t, copyPath)
	if copyCodec != srcCodec {
		t.Fatalf("stream copy re-encoded: codec = %q, source = %q. open_media_clip must keep source fidelity by default (#878)", copyCodec, srcCodec)
	}

	srcRate := byteRate(t, srcPath, srcSize)
	copyRate := byteRate(t, copyPath, copySize)
	if math.Abs(copyRate-srcRate) > 0.25*srcRate {
		t.Fatalf("stream copy byte rate = %.0f B/s, source = %.0f B/s: the cut no longer costs the source bitrate, so this measurement no longer measures #878",
			copyRate, srcRate)
	}
}

// spanReference holds the source luma at the two ends of the requested span,
// plus the luma at the source's own start. A cut that ignored the offset would
// look like origin, which is what makes the check discriminating.
type spanReference struct {
	start  float64
	end    float64
	origin float64
}

// readSpanReference probes the source timeline and refuses to run the span
// assertions on a fixture too flat to tell one moment from another.
func readSpanReference(t *testing.T, src string) spanReference {
	t.Helper()
	ref := spanReference{
		start:  meanLuma(t, src, clipSpanStartMS),
		end:    meanLuma(t, src, clipSpanEndMS-probeBackoffMS),
		origin: meanLuma(t, src, 0),
	}
	if math.Abs(ref.start-ref.origin) < 2*lumaToleranceLevels {
		t.Skipf("this ffmpeg build renders a fixture too flat to locate a span: origin=%.1f start=%.1f", ref.origin, ref.start)
	}
	return ref
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

// assertCoversSpan requires that a cut covers the requested window, on both
// length and content.
//
// Length: stream copy can only start at the preceding keyframe, so a copy may
// run slightly LONGER than the request; it must never run short.
//
// Content: the clip's first frame must be the source frame at clipSpanStartMS,
// and its last frame the source frame at clipSpanEndMS. Length alone would
// accept a clip of the right size cut from the wrong moment, which is the
// regression a smaller-clip fix could introduce. The content check can be exact
// here because the fixture is all-intra: every frame is a keyframe, so the copy
// has no keyframe drift to absorb.
func assertCoversSpan(t *testing.T, ref spanReference, path, label string) {
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

	gotStart := meanLuma(t, path, 0)
	if math.Abs(gotStart-ref.start) > lumaToleranceLevels {
		t.Fatalf("%s: first frame luma = %.1f, want %.1f (the source at %dms; the source at 0ms is %.1f): the clip starts at the wrong moment",
			label, gotStart, ref.start, clipSpanStartMS, ref.origin)
	}
	gotEnd := meanLuma(t, path, clipSpanEndMS-clipSpanStartMS-probeBackoffMS)
	if math.Abs(gotEnd-ref.end) > lumaToleranceLevels {
		t.Fatalf("%s: last frame luma = %.1f, want %.1f (the source at %dms): the clip ends at the wrong moment",
			label, gotEnd, ref.end, clipSpanEndMS-probeBackoffMS)
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

	// The baseline must still BE the baseline: same codec, same bitrate as the
	// source. Without this the size comparison below would also pass for a
	// server that had quietly started re-encoding every clip.
	srcInfo, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat source: %v", err)
	}
	assertCutKeepsSourceFidelity(t, src, int(srcInfo.Size()), copyPath, len(copied))

	// Deliberately loose: the point is an order of magnitude, not an exact
	// encoder output, which varies by ffmpeg build.
	if len(preview)*3 > len(copied) {
		t.Fatalf("preview = %d bytes vs stream copy = %d bytes: the preview profile does not materially reduce bytes",
			len(preview), len(copied))
	}

	// A smaller clip of the WRONG span is worse than a large correct one, so
	// both cuts are checked against the source timeline, not only for length.
	ref := readSpanReference(t, src)
	assertCoversSpan(t, ref, copyPath, "stream copy")
	assertCoversSpan(t, ref, previewPath, "preview")
}
