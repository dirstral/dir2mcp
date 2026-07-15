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

// TestParseMediaInfoMultiTrackAudio pins the issue #567 census: every audio
// stream is enumerated (in ffprobe order) with its index/codec/channels and
// declared language/title tags, while AudioCodec stays the first stream's codec
// for backward compatibility. A single proxy MP4 carrying an original mix, a
// per-language dub, and a music-&-effects track must report all three so the
// dropped tracks are not silent.
func TestParseMediaInfoMultiTrackAudio(t *testing.T) {
	raw := []byte(`{
	  "streams": [
	    {"index":0,"codec_type":"video","codec_name":"h264","width":1280,"height":720},
	    {"index":1,"codec_type":"audio","codec_name":"aac","channels":2,"tags":{"language":"eng","title":"Original"}},
	    {"index":2,"codec_type":"audio","codec_name":"aac","channels":2,"tags":{"language":"rus"}},
	    {"index":3,"codec_type":"audio","codec_name":"aac","channels":2,"tags":{"title":"Music & Effects"}}
	  ],
	  "format": {"format_name":"mov,mp4,m4a,3gp,3g2,mj2","bit_rate":"2000000"}
	}`)
	info, err := avutil.ParseMediaInfo(raw)
	if err != nil {
		t.Fatalf("ParseMediaInfo: %v", err)
	}
	if info.AudioCodec != "aac" {
		t.Fatalf("AudioCodec = %q, want first stream's codec aac", info.AudioCodec)
	}
	if !info.HasMultipleAudioStreams() {
		t.Fatalf("HasMultipleAudioStreams() = false, want true for 3 audio streams")
	}
	if got := info.AudioStreamCount(); got != 3 {
		t.Fatalf("AudioStreamCount() = %d, want 3", got)
	}
	// Enumerated in ffprobe order with absolute stream index preserved.
	if info.AudioStreams[0].Index != 1 || info.AudioStreams[0].Language != "eng" ||
		info.AudioStreams[0].Channels != 2 || info.AudioStreams[0].Title != "Original" {
		t.Errorf("stream[0] = %+v, want {Index:1 aac 2ch lang=eng title=Original}", info.AudioStreams[0])
	}
	if info.AudioStreams[1].Index != 2 || info.AudioStreams[1].Language != "rus" {
		t.Errorf("stream[1] = %+v, want {Index:2 lang=rus}", info.AudioStreams[1])
	}
	if info.AudioStreams[2].Index != 3 || info.AudioStreams[2].Title != "Music & Effects" ||
		info.AudioStreams[2].Language != "" {
		t.Errorf("stream[2] = %+v, want {Index:3 title=\"Music & Effects\" no-lang}", info.AudioStreams[2])
	}
}

// TestParseMediaInfoSingleTrackAudio pins that ordinary single-track media reports
// exactly one audio stream and is not flagged as multi-track (issue #567), so the
// diagnostic never fires for the common case.
func TestParseMediaInfoSingleTrackAudio(t *testing.T) {
	raw := []byte(`{
	  "streams": [
	    {"index":0,"codec_type":"video","codec_name":"h264","width":1920,"height":1080},
	    {"index":1,"codec_type":"audio","codec_name":"aac","channels":2}
	  ],
	  "format": {"format_name":"mov,mp4","bit_rate":"1500000"}
	}`)
	info, err := avutil.ParseMediaInfo(raw)
	if err != nil {
		t.Fatalf("ParseMediaInfo: %v", err)
	}
	if info.HasMultipleAudioStreams() {
		t.Fatalf("single audio track must not report multi-track")
	}
	if info.AudioStreamCount() != 1 || info.AudioStreams[0].CodecName != "aac" {
		t.Fatalf("audio streams = %+v, want one aac stream", info.AudioStreams)
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
