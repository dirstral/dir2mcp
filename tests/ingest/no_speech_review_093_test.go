package tests

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/quality"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Edge cases of the SPEC 0.93.0 no-speech outcome that a review of #1169 found.

// replyTranscriber answers each call (1-based) with replies[n], else def. A
// reply with empty text is a no-speech result.
type replyTranscriber struct {
	mu      sync.Mutex
	calls   int
	replies map[int]model.TranscriptResult
	def     model.TranscriptResult
	err     error
}

func (r *replyTranscriber) Transcribe(ctx context.Context, relPath string, data []byte) (string, error) {
	res, err := r.TranscribeStructured(ctx, relPath, data)
	return res.Text, err
}

func (r *replyTranscriber) TranscribeStructured(context.Context, string, []byte) (model.TranscriptResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return model.TranscriptResult{}, r.err
	}
	if res, ok := r.replies[r.calls]; ok {
		return res, nil
	}
	return r.def, nil
}

// MaxTranscribePayloadBytes declares no cap, so only the duration rule windows.
func (r *replyTranscriber) MaxTranscribePayloadBytes() int { return 0 }

// TestNoSpeech_EmptyWindowKeepsThePrecedingOverlapText: a window with no
// speech gives the merge no text, so it must not end the preceding window's
// core. The preceding window's only line sits in its overlap tail.
//
// Mutant killed: merging the empty window as a surviving window, which drops
// the line and turns the recording into "no speech".
func TestNoSpeech_EmptyWindowKeepsThePrecedingOverlapText(t *testing.T) {
	t.Parallel()
	windows := []ingest.TranscriptWindow{
		{StartMS: 0, Res: model.TranscriptResult{Text: "[09:55] the last line before the music"}},
		{StartMS: 590_000, Res: model.TranscriptResult{}},
	}
	text, _ := ingest.MergeTranscriptWindows(windows, 590_000)
	if !strings.Contains(text, "the last line before the music") {
		t.Fatalf("merged text %q lost the overlap line of the window before an empty window", text)
	}
}

// TestNoSpeech_CandidateWithNoSpeechIsADecodedWindow pins §8.2.3 with a
// no-speech result: in the first window the first candidate decodes a loop
// the gate refuses, and the next candidate finds no speech. That candidate
// declares only "de", and on_uncovered_language is skip. The window is a
// decoded window with no language, not a window refused for the language
// ("ru") the loop resolved: a no-speech result reports no language.
//
// Mutant killed: judging the candidate's empty result as a normal window,
// which refuses it as language_uncovered.
func TestNoSpeech_CandidateWithNoSpeechIsADecodedWindow(t *testing.T) {
	t.Parallel()
	def := &langWindowTranscriber{def: langReply{lang: "ru", conf: 0.9, text: ruText}}
	h, content := newScopedHarness(t, def, scopeTotalMS)
	h.svc.SetQualityGate(quality.New(quality.DefaultConfig()))
	h.svc.SetOnUncoveredLanguage("skip")
	h.svc.AddRouteCandidate("ru", &langWindowTranscriber{def: langReply{lang: "ru", conf: 0.9, text: loopText}}, "ru-first", []string{"ru"})
	second := &replyTranscriber{
		replies: map[int]model.TranscriptResult{2: {Text: ruText, Language: "ru", LanguageConfidence: 0.9}},
		def:     model.TranscriptResult{Language: "ru", LanguageConfidence: 0.9},
	}
	h.svc.AddRouteCandidate("ru", second, "ru-second", []string{"de"})

	if err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/music.m4a"), content); err != nil {
		t.Fatalf("transcription failed: %v\nlogs:\n%s", err, h.logs.String())
	}
	out := h.logs.String()
	if strings.Contains(out, "refusing window [0,") {
		t.Errorf("a no-speech window must not be refused for a language it does not report:\n%s", out)
	}
	if !strings.Contains(out, "window [0,590000]ms of talks/music.m4a holds no speech; it counts as decoded") {
		t.Errorf("the first window must be logged as decoded with no speech:\n%s", out)
	}
}

// TestNoSpeech_RouteFallbackWithNoSpeechIsRetried pins §8.2.4 with §8.6.13:
// the identifier routes the item to a checkpoint that fails, and the default
// profile finds no speech. The failed route can hold speech, so the result is
// an error that is retried, not a settled no_speech skip.
//
// Mutant killed: settling the fallback's empty result as no_speech, which
// stamps the done marker and never tries the selected route again.
func TestNoSpeech_RouteFallbackWithNoSpeechIsRetried(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	def := &replyTranscriber{}
	svc, state, _ := noSpeechService(t, root, testutil.TempDir(t), st, def, 0)
	svc.SetLanguageIdentifier(&replyTranscriber{def: model.TranscriptResult{Text: "[00:00] салам", Language: "ky", LanguageConfidence: 0.9}}, "lid")
	svc.SetRouteTranscriber("ky", &replyTranscriber{err: errors.New("checkpoint unavailable: 503")}, "whisper-ky", []string{"ky"})
	svc.SetRouteErrorFallback(true)

	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	doc := mustGetDoc(t, st, "talk.mp3")
	if doc.Status != "error" || doc.SkipReason != "" {
		t.Fatalf("status=%q skip_reason=%q, want error: the failed route can hold speech", doc.Status, doc.SkipReason)
	}
	if !strings.Contains(doc.ErrorMessage, "found no speech") {
		t.Errorf("error message must say why, got %q", doc.ErrorMessage)
	}
	if snap := state.Snapshot(); snap.Errors != 1 || snap.Skipped != 0 {
		t.Errorf("run counters errors=%d skipped=%d, want 1/0", snap.Errors, snap.Skipped)
	}
}

// TestNoSpeech_EarlierRecognitionKeepsTheDocumentOK: recognition indexed the
// video on an earlier run. On a forced reindex the backend returns no
// annotations, so this run produces nothing new, but the earlier recognition
// representation is still active and searchable. The document stays ok.
//
// Mutant killed: deciding on this run's recognition result alone, which
// records no_speech and hides the earlier recognition chunks.
func TestNoSpeech_EarlierRecognitionKeepsTheDocumentOK(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "match.mp4"), "fake-video-bytes")
	st := newRealStore(t)
	client, _ := noSpeechWhisperServer(t)
	f := ingest.DiscoveredFile{RelPath: "match.mp4", SizeBytes: 16, MTimeUnix: time.Now().Unix()}

	run := func(rec *fakeRecognizer) {
		cfg := config.Config{RootDir: root, StateDir: stateDir, STTProvider: "off", RecognizeProvider: "serve"}
		svc := mustNewIngestService(t, cfg, st)
		svc.SetIndexingState(appstate.NewIndexingState(appstate.ModeIncremental))
		svc.SetLogger(log.New(&syncBuffer{}, "", 0))
		svc.SetTranscriber(client)
		svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 0, errors.New("no duration") }
		svc.ExtractAudioTrackFunc = func(context.Context, string) ([]byte, error) { return []byte("audio-track"), nil }
		svc.SetRecognizer(rec)
		if err := svc.ProcessDocument(ctx, f, nil, true); err != nil {
			t.Fatalf("ProcessDocument: %v", err)
		}
	}
	run(&fakeRecognizer{result: recognizeTestResult()})
	run(&fakeRecognizer{result: model.RecognizeResult{Name: "dirstral-annotator", Version: "0.2.0"}})

	doc := mustGetDoc(t, st, "match.mp4")
	if doc.Status != "ok" || doc.SkipReason != "" {
		t.Fatalf("status=%q skip_reason=%q, want ok: the earlier recognition representation is still searchable", doc.Status, doc.SkipReason)
	}
	assertRecognitionWithoutTranscript(t, st, "match.mp4")
}
