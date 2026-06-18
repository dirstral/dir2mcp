package tests

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/dirstral/dir2mcp/internal/avutil"
)

// TestParseMediaInfoFull pins decoding of a complete ffprobe -of json payload:
// container/bitrate from format, and the first video + first audio stream codecs
// and the video dimensions.
func TestParseMediaInfoFull(t *testing.T) {
	raw := []byte(`{
	  "streams": [
	    {"codec_type":"video","codec_name":"h264","width":1920,"height":1080},
	    {"codec_type":"audio","codec_name":"aac"}
	  ],
	  "format": {"format_name":"mov,mp4,m4a,3gp,3g2,mj2","bit_rate":"1500000"}
	}`)
	info, err := avutil.ParseMediaInfo(raw)
	if err != nil {
		t.Fatalf("ParseMediaInfo: %v", err)
	}
	if info.Container != "mov,mp4,m4a,3gp,3g2,mj2" {
		t.Fatalf("container = %q", info.Container)
	}
	if info.VideoCodec != "h264" || info.AudioCodec != "aac" {
		t.Fatalf("codecs = %q/%q", info.VideoCodec, info.AudioCodec)
	}
	if info.BitRateBPS != 1500000 {
		t.Fatalf("bitrate = %d", info.BitRateBPS)
	}
	if info.Width != 1920 || info.Height != 1080 || !info.HasVideo() {
		t.Fatalf("dimensions = %dx%d (hasVideo=%v)", info.Width, info.Height, info.HasVideo())
	}
}

// TestParseMediaInfoAudioOnly pins that an audio-only payload yields no video
// codec/dimensions and HasVideo() is false, so SMIL falls back to <audio>.
func TestParseMediaInfoAudioOnly(t *testing.T) {
	raw := []byte(`{
	  "streams": [{"codec_type":"audio","codec_name":"mp3"}],
	  "format": {"format_name":"mp3","bit_rate":"128000"}
	}`)
	info, err := avutil.ParseMediaInfo(raw)
	if err != nil {
		t.Fatalf("ParseMediaInfo: %v", err)
	}
	if info.HasVideo() {
		t.Fatalf("audio-only must not report video")
	}
	if info.AudioCodec != "mp3" || info.BitRateBPS != 128000 {
		t.Fatalf("audio info wrong: %+v", info)
	}
}

// TestParseMediaInfoPartial pins fail-open decoding: a payload missing bitrate /
// dimensions yields a partial MediaInfo (zero fields) rather than an error.
func TestParseMediaInfoPartial(t *testing.T) {
	raw := []byte(`{"streams":[{"codec_type":"video","codec_name":"vp9"}],"format":{"format_name":"webm","bit_rate":"N/A"}}`)
	info, err := avutil.ParseMediaInfo(raw)
	if err != nil {
		t.Fatalf("ParseMediaInfo: %v", err)
	}
	if info.VideoCodec != "vp9" {
		t.Fatalf("video codec = %q", info.VideoCodec)
	}
	if info.BitRateBPS != 0 || info.HasVideo() {
		t.Fatalf("partial info should have zero bitrate/dimensions: %+v", info)
	}
}

// TestProbeMediaInfoToolNotFound pins the fail-open contract: when ffprobe is
// absent, ProbeMediaInfo returns ErrToolNotFound so callers omit SMIL. Skipped
// when ffprobe IS installed (then there is no "not found" path to assert).
func TestProbeMediaInfoToolNotFound(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err == nil {
		t.Skip("ffprobe is installed; the not-found path is unobservable here")
	}
	_, err := avutil.ProbeMediaInfo(context.Background(), "/nonexistent/media.mp4")
	if !errors.Is(err, avutil.ErrToolNotFound) {
		t.Fatalf("expected ErrToolNotFound, got %v", err)
	}
}
