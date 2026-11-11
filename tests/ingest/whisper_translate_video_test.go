package tests

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// The whisper translate engine decodes the same input as the source
// transcription. On the TV Rain archive it sent the whole video container
// instead, so every video over the whisper server's 50 MB payload cap got no
// English track ("transcription input too large"), and the failure was only a
// log line.

// payloadRecordingTranscriber records the path and bytes of every request.
type payloadRecordingTranscriber struct {
	mu    sync.Mutex
	paths []string
	data  []string
	text  string
}

func (p *payloadRecordingTranscriber) Transcribe(_ context.Context, relPath string, data []byte) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paths = append(p.paths, relPath)
	p.data = append(p.data, string(data))
	return p.text, nil
}

// TestWhisperTranslate_VideoDecodesItsAudioTrack pins that a video's translate
// pass receives the demuxed audio track under an audio name, exactly like the
// source transcription, and never the video bytes.
func TestWhisperTranslate_VideoDecodesItsAudioTrack(t *testing.T) {
	t.Parallel()
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{StateDir: testutil.TempDir(t)}, st)
	svc.ExtractAudioTrackFunc = func(context.Context, string) ([]byte, error) { return []byte("AUDIO-TRACK"), nil }
	source := &payloadRecordingTranscriber{text: "[00:00] привет\n[00:02] пока"}
	svc.SetTranscriber(source)
	svc.SetTranscriptLanguage("ru")
	translate := &payloadRecordingTranscriber{text: "[00:00] hello\n[00:02] bye"}
	svc.SetTranslateTranscriber(translate, "whisper", "large-v3", []string{"en"})

	doc := model.Document{DocID: 7, RelPath: "v/abc_1080p.mp4", DocType: "video"}
	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, []byte("VIDEO-CONTAINER-BYTES")); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation: %v", err)
	}
	if len(translate.data) != 1 || translate.data[0] != "AUDIO-TRACK" || !strings.HasSuffix(translate.paths[0], ".m4a") {
		t.Fatalf("translate pass must decode the audio track as .m4a, got paths=%v data=%v", translate.paths, translate.data)
	}
	if len(source.data) != 1 || source.data[0] != "AUDIO-TRACK" {
		t.Fatalf("source pass must decode the audio track too, got %v", source.data)
	}
	var en bool
	for _, r := range st.reps {
		en = en || r.RepType == ingest.TranscriptRepType("en")
	}
	if !en {
		t.Fatalf("expected an English transcript representation, got %+v", st.reps)
	}
}

// TestWhisperTranslate_OverCapAudioIsWindowed pins that translate audio over the
// provider's payload cap is decoded in windows that fit, as the source
// transcription is (#954), even with media.translate.whisper_window_sec at its
// default of 0.
func TestWhisperTranslate_OverCapAudioIsWindowed(t *testing.T) {
	t.Parallel()
	const totalMS = 5 * 60 * 1000
	content := make([]byte, 9000)
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{StateDir: testutil.TempDir(t)}, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] привет\n[00:30] пока"})
	svc.SetTranscriptLanguage("ru")
	tr := &windowRecordingTranscriber{capBytes: 5000}
	svc.SetTranslateTranscriber(tr, "whisper", "large-v3", []string{"en"})
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) {
		return totalMS * time.Millisecond, nil
	}
	svc.ExtractSegmentFunc = func(_ context.Context, _ string, startMS, endMS int) ([]byte, error) {
		return make([]byte, len(content)*(endMS-startMS)/totalMS), nil
	}

	doc := model.Document{DocID: 8, RelPath: "talks/dense.wav", DocType: "audio"}
	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, content); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation: %v", err)
	}
	reqs := tr.requests()
	if len(reqs) < 2 {
		t.Fatalf("over-cap translate audio must be windowed, got requests %v", reqs)
	}
	for i, n := range reqs {
		if n > tr.capBytes {
			t.Fatalf("translate request %d carried %d bytes, over the cap %d", i+1, n, tr.capBytes)
		}
	}
	var en bool
	for _, r := range st.reps {
		en = en || r.RepType == ingest.TranscriptRepType("en")
	}
	if !en {
		t.Fatalf("expected an English transcript representation, got %+v", st.reps)
	}
}
