package tests

import (
	"context"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// sttSizeLog records, per request, the audio duration the ingest pipeline sized
// that request for (issue #962). 0 means the request went out unsized, which is
// the bug: a window of audio then gets the constant 120 s timeout.
type sttSizeLog struct {
	mu       sync.Mutex
	requests []int
}

func (l *sttSizeLog) record(audioMS int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests = append(l.requests, audioMS)
}

func (l *sttSizeLog) snapshot() []int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]int(nil), l.requests...)
}

// sizedSTT is a fake transcriber with the optional
// model.AudioDurationTranscriber capability. Each instance remembers the
// duration it was sized for and reports it when a request is actually sent, so a
// test can assert what the pipeline told the provider about every request.
type sizedSTT struct {
	log *sttSizeLog
	// audioMS is the duration this instance was sized for; 0 means unsized.
	audioMS int
}

func (s *sizedSTT) Transcribe(ctx context.Context, relPath string, data []byte) (string, error) {
	res, err := s.TranscribeStructured(ctx, relPath, data)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

func (s *sizedSTT) TranscribeStructured(_ context.Context, _ string, _ []byte) (model.TranscriptResult, error) {
	s.log.record(s.audioMS)
	return model.TranscriptResult{Text: "[00:00] decoded line\n[00:30] another line"}, nil
}

// ForAudioDuration returns a copy sized for audioMS, never mutating the
// receiver, exactly as the whisper client does.
func (s *sizedSTT) ForAudioDuration(audioMS int) model.Transcriber {
	return &sizedSTT{log: s.log, audioMS: audioMS}
}

// newSizedSTTHarness wires an ingest Service whose duration probe and segment
// extraction are deterministic stubs, so the request sizing is exercised without
// ffmpeg/ffprobe.
func newSizedSTTHarness(t *testing.T, totalMS int, payload []byte) (*ingest.Service, *sttSizeLog) {
	t.Helper()
	logRec := &sttSizeLog{}
	svc := mustNewIngestService(t, config.Config{StateDir: testutil.TempDir(t)}, &fakeIngestStore{})
	svc.SetTranscriber(&sizedSTT{log: logRec})
	svc.SetLogger(log.New(&syncBuffer{}, "", 0))
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) {
		return time.Duration(totalMS) * time.Millisecond, nil
	}
	svc.ExtractSegmentFunc = func(_ context.Context, _ string, startMS, endMS int) ([]byte, error) {
		return make([]byte, len(payload)*(endMS-startMS)/totalMS), nil
	}
	return svc, logRec
}

// TestWindowedSTT_EachRequestIsSizedToItsWindow is the ingest half of #962: every
// window request carries the duration of that window, so the client can give the
// request a timeout that a slow decode survives. Before the fix each window went
// out with the constant 120 s, which a 10-minute window of a hard language
// exceeds (110 to 140 s measured), and the window became a hole in the
// transcript.
//
// Mutant killed: dropping model.TranscriberForAudioDuration from
// decodeWindowPieces (every request would report an unsized 0).
func TestWindowedSTT_EachRequestIsSizedToItsWindow(t *testing.T) {
	t.Parallel()
	const totalMS = 30 * 60 * 1000 // 30 minutes: past the 10-minute decode window
	content := make([]byte, 300_000)
	svc, sizes := newSizedSTTHarness(t, totalMS, content)

	if err := svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/long.m4a"), content); err != nil {
		t.Fatalf("windowed transcription failed: %v", err)
	}

	reqs := sizes.snapshot()
	if len(reqs) < 2 {
		t.Fatalf("a 30-minute recording made %d requests, want one per window", len(reqs))
	}
	for i, audioMS := range reqs {
		if audioMS <= 0 {
			t.Errorf("request %d went out unsized (%d ms), so it keeps the constant timeout", i+1, audioMS)
			continue
		}
		if audioMS > ingest.DefaultSTTWindowMS {
			t.Errorf("request %d was sized for %d ms, more audio than the %d ms window carries", i+1, audioMS, ingest.DefaultSTTWindowMS)
		}
		if audioMS == totalMS {
			t.Errorf("request %d was sized for the WHOLE recording (%d ms) rather than its window", i+1, audioMS)
		}
	}
}

// TestSTT_SingleRequestIsSizedToTheWholeRecording covers the case the windowing
// rules leave alone: a recording under the 10-minute window goes out as ONE
// request carrying all of it, so it must be sized for all of it. Nine minutes of
// a language the model struggles with decodes well past 120 s.
//
// Mutant killed: sizing only the windowed path (this request would report an
// unsized 0).
func TestSTT_SingleRequestIsSizedToTheWholeRecording(t *testing.T) {
	t.Parallel()
	const totalMS = 9 * 60 * 1000 // under the window threshold: one request
	content := []byte("short-audio-bytes")
	svc, sizes := newSizedSTTHarness(t, totalMS, content)

	if err := svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/nine-minutes.m4a"), content); err != nil {
		t.Fatalf("transcription failed: %v", err)
	}

	reqs := sizes.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("made %d requests, want exactly 1 for a recording that fits", len(reqs))
	}
	if reqs[0] != totalMS {
		t.Errorf("the single request was sized for %d ms, want the whole recording (%d ms)", reqs[0], totalMS)
	}
}
