package tests

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/avutil"
)

// TestExtractSegment_InvalidRange validates the half-open window guard before
// any binary is needed.
func TestExtractSegment_InvalidRange(t *testing.T) {
	for _, tc := range []struct{ start, end int }{
		{0, 0}, {5, 5}, {10, 3}, {-1, 5},
	} {
		if _, err := avutil.ExtractSegment(context.Background(), "/whatever.mp3", tc.start, tc.end); err == nil {
			t.Errorf("ExtractSegment(_, [%d,%d)) = nil error, want range error", tc.start, tc.end)
		}
	}
}

// TestToolNotFound confirms a missing ffprobe/ffmpeg is reported distinctly so
// callers can skip gracefully (keep the text path) rather than hard-fail.
func TestToolNotFound(t *testing.T) {
	// Empty PATH => LookPath fails for both binaries.
	t.Setenv("PATH", "")

	if _, err := avutil.Duration(context.Background(), "/x.mp3"); !errors.Is(err, avutil.ErrToolNotFound) {
		t.Errorf("Duration with empty PATH = %v, want ErrToolNotFound", err)
	}
	if _, err := avutil.ExtractSegment(context.Background(), "/x.mp3", 0, 1000); !errors.Is(err, avutil.ErrToolNotFound) {
		t.Errorf("ExtractSegment with empty PATH = %v, want ErrToolNotFound", err)
	}
}

// writeWAV writes a minimal PCM WAV (mono, 8 kHz, 16-bit) of the given duration
// filled with silence — enough for ffprobe/ffmpeg to report a real duration.
func writeWAV(t *testing.T, path string, dur time.Duration) {
	t.Helper()
	const sampleRate = 8000
	const bitsPerSample = 16
	const channels = 1
	samples := int(float64(sampleRate) * dur.Seconds())
	dataLen := samples * channels * (bitsPerSample / 8)

	var buf []byte
	put := func(b ...byte) { buf = append(buf, b...) }
	putU32 := func(v uint32) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], v)
		buf = append(buf, b[:]...)
	}
	putU16 := func(v uint16) {
		var b [2]byte
		binary.LittleEndian.PutUint16(b[:], v)
		buf = append(buf, b[:]...)
	}
	put('R', 'I', 'F', 'F')
	putU32(uint32(36 + dataLen))
	put('W', 'A', 'V', 'E')
	put('f', 'm', 't', ' ')
	putU32(16)
	putU16(1) // PCM
	putU16(channels)
	putU32(sampleRate)
	putU32(sampleRate * channels * (bitsPerSample / 8))
	putU16(channels * (bitsPerSample / 8))
	putU16(bitsPerSample)
	put('d', 'a', 't', 'a')
	putU32(uint32(dataLen))
	buf = append(buf, make([]byte, dataLen)...)

	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("write wav: %v", err)
	}
}

// TestDurationAndExtract_Integration round-trips a real WAV through ffprobe and
// ffmpeg. Skipped when the binaries are not installed.
func TestDurationAndExtract_Integration(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "tone.wav")
	writeWAV(t, src, 3*time.Second)

	d, err := avutil.Duration(context.Background(), src)
	if err != nil {
		t.Fatalf("Duration: %v", err)
	}
	if d < 2500*time.Millisecond || d > 3500*time.Millisecond {
		t.Fatalf("Duration = %v, want ~3s", d)
	}

	seg, err := avutil.ExtractSegment(context.Background(), src, 1000, 2000)
	if err != nil {
		t.Fatalf("ExtractSegment: %v", err)
	}
	if len(seg) == 0 {
		t.Fatal("ExtractSegment returned no bytes")
	}
	// The extracted clip must itself be a shorter, valid WAV.
	out := filepath.Join(dir, "seg.wav")
	if err := os.WriteFile(out, seg, 0o600); err != nil {
		t.Fatal(err)
	}
	sd, err := avutil.Duration(context.Background(), out)
	if err != nil {
		t.Fatalf("Duration(segment): %v", err)
	}
	if sd > 1500*time.Millisecond {
		t.Fatalf("segment Duration = %v, want ~1s", sd)
	}
}
