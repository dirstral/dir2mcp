package tests

import (
	"bytes"
	"context"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/avutil"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
)

// TestMultiTrackAudio_SurfacedNotSilent is the honest-coverage guard for issue
// #567: a media file whose container carries more than one audio stream is still
// transcribed from the first stream only, but the dropped tracks must no longer be
// silent — a single structured warning naming every track has to be emitted so an
// operator can see that per-language / M&E tracks were not indexed.
//
// The stream census is injected via ProbeMediaInfoFunc and the audio extraction is
// stubbed via ExtractAudioTrackFunc, so neither ffprobe nor ffmpeg is required and
// CI stays hermetic.
func TestMultiTrackAudio_SurfacedNotSilent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "proxy.mp4"), "fake-multitrack-video-bytes")
	st := newRealStore(t)

	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir(), STTProvider: "off"}, st)
	tr := &capturingTranscriber{text: "[00:00] hello\n[00:03] world"}
	svc.SetTranscriber(tr)
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	svc.SetTranscriptLanguage("en")

	var logBuf bytes.Buffer
	svc.SetLogger(log.New(&logBuf, "", 0))

	// Stub audio extraction so the video routes to STT without ffmpeg (#495 path).
	svc.ExtractAudioTrackFunc = func(_ context.Context, _ string) ([]byte, error) {
		return []byte("fake-extracted-audio"), nil
	}
	// Inject a three-track census: an original mix, a per-language dub, and a
	// music-&-effects track — the exact shape #567 describes.
	svc.ProbeMediaInfoFunc = func(_ context.Context, _ string) (avutil.MediaInfo, error) {
		return avutil.MediaInfo{
			Container:  "mov,mp4",
			VideoCodec: "h264",
			AudioCodec: "aac",
			Width:      1280,
			Height:     720,
			AudioStreams: []avutil.AudioStream{
				{Index: 1, CodecName: "aac", Channels: 2, Language: "eng", Title: "Original"},
				{Index: 2, CodecName: "aac", Channels: 2, Language: "rus"},
				{Index: 3, CodecName: "aac", Channels: 2, Title: "Music & Effects"},
			},
		}, nil
	}

	f := ingest.DiscoveredFile{RelPath: "proxy.mp4", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument(proxy.mp4): %v", err)
	}

	// The transcript is still produced (from the first track) — multi-track media
	// must not break transcription.
	if tr.calls != 1 {
		t.Fatalf("STT calls = %d, want 1 (the first audio stream must still be transcribed)", tr.calls)
	}
	meta := transcriptMetaFor(t, st, "proxy.mp4")
	if strings.TrimSpace(meta) == "" {
		t.Fatal("multi-track media produced no transcript representation")
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "multi-track audio") {
		t.Fatalf("no multi-track diagnostic logged; dropped tracks are silent (#567). logs:\n%s", logs)
	}
	// The warning must name the count and the dropped tracks so the selection is
	// visible, not a bare "there is more than one" line.
	for _, want := range []string{"3 audio streams", "2 additional", "lang=rus", "Music & Effects"} {
		if !strings.Contains(logs, want) {
			t.Errorf("multi-track diagnostic missing %q; logs:\n%s", want, logs)
		}
	}
}

// TestMultiTrackAudio_EmptyFirstTrackStillWarns is the optibot #596 regression:
// when the first (default-selected) audio stream is a no-dialogue track — an M&E
// track — STT yields an empty transcript. The multi-track warning MUST still fire
// so the dropped dialogue tracks are not silent, even though no transcript
// representation is produced (the diagnostic is emitted before the empty-transcript
// early return).
func TestMultiTrackAudio_EmptyFirstTrackStillWarns(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "me_first.mp4"), "fake-multitrack-video-bytes")
	st := newRealStore(t)

	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir(), STTProvider: "off"}, st)
	svc.SetTranscriber(&capturingTranscriber{text: ""}) // M&E track 0 → empty transcript
	svc.SetSTTIdentity("whisper", "whisper-large-v3")

	var logBuf bytes.Buffer
	svc.SetLogger(log.New(&logBuf, "", 0))
	svc.ExtractAudioTrackFunc = func(_ context.Context, _ string) ([]byte, error) {
		return []byte("fake-extracted-audio"), nil
	}
	svc.ProbeMediaInfoFunc = func(_ context.Context, _ string) (avutil.MediaInfo, error) {
		return avutil.MediaInfo{
			Container: "mov,mp4", VideoCodec: "h264", AudioCodec: "aac",
			AudioStreams: []avutil.AudioStream{
				{Index: 1, CodecName: "aac", Channels: 6, Title: "Music & Effects"},
				{Index: 2, CodecName: "aac", Channels: 2, Language: "eng", Title: "Dialogue"},
			},
		}, nil
	}

	f := ingest.DiscoveredFile{RelPath: "me_first.mp4", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument(me_first.mp4): %v", err)
	}
	if logs := logBuf.String(); !strings.Contains(logs, "multi-track audio") || !strings.Contains(logs, "lang=eng") {
		t.Fatalf("empty first-track transcript suppressed the multi-track warning (#596); logs:\n%s", logs)
	}
}

// TestSingleTrackAudio_NoMultiTrackWarning pins that ordinary single-track media
// (the common case) is byte-for-byte unchanged: it transcribes normally and emits
// no multi-track diagnostic, so #567's warning never becomes noise.
func TestSingleTrackAudio_NoMultiTrackWarning(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "single.mp4"), "fake-single-track-video-bytes")
	st := newRealStore(t)

	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir(), STTProvider: "off"}, st)
	tr := &capturingTranscriber{text: "[00:00] only one track here"}
	svc.SetTranscriber(tr)
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	svc.SetTranscriptLanguage("en")

	var logBuf bytes.Buffer
	svc.SetLogger(log.New(&logBuf, "", 0))

	svc.ExtractAudioTrackFunc = func(_ context.Context, _ string) ([]byte, error) {
		return []byte("fake-extracted-audio"), nil
	}
	svc.ProbeMediaInfoFunc = func(_ context.Context, _ string) (avutil.MediaInfo, error) {
		return avutil.MediaInfo{
			Container:    "mov,mp4",
			VideoCodec:   "h264",
			AudioCodec:   "aac",
			Width:        1280,
			Height:       720,
			AudioStreams: []avutil.AudioStream{{Index: 1, CodecName: "aac", Channels: 2, Language: "eng"}},
		}, nil
	}

	f := ingest.DiscoveredFile{RelPath: "single.mp4", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument(single.mp4): %v", err)
	}

	if tr.calls != 1 {
		t.Fatalf("STT calls = %d, want 1", tr.calls)
	}
	if strings.Contains(logBuf.String(), "multi-track audio") {
		t.Errorf("single-track media emitted a multi-track diagnostic; want none. logs:\n%s", logBuf.String())
	}
}
