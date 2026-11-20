package tests

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/quality"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/internal/whisperapi"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC 0.99.0. On a live archive, 28 of the first 1,500 videos failed with
// "WHISPER_FAILED: transcription response had no text content". Their audio is
// real but holds only music or ambient sound, and both STT servers answered with
// a well-formed {"text":"","segments":[]}. These tests pin the no-speech outcome:
// a successful decode with nothing said is a no_speech skip, never a provider
// failure, and a no-speech window is decoded audio, never a coverage shortfall.

// measuredNoSpeech is the response both STT servers returned on music.
const measuredNoSpeech = `{"task":"transcribe","language":"nn","duration":60.0,"text":"","segments":[]}`

// noSpeechWhisperServer is an OpenAI-compatible /v1/audio/transcriptions
// server that answers every request with the measured no-speech body, and
// counts the requests.
func noSpeechWhisperServer(t *testing.T) (*whisperapi.Client, *int32) {
	t.Helper()
	return scriptedWhisperServer(t, nil)
}

// scriptedWhisperServer answers the 1-based request indices in speechAt with a
// segment of speech and every other request with the measured no-speech body.
func scriptedWhisperServer(t *testing.T, speechAt map[int32]bool) (*whisperapi.Client, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		if speechAt[n] {
			_, _ = fmt.Fprintf(w, `{"text":"spoken line %d","language":"en","segments":[{"start":0,"end":3,"text":"spoken line %d"}]}`, n, n)
			return
		}
		_, _ = w.Write([]byte(measuredNoSpeech))
	}))
	t.Cleanup(srv.Close)
	c := whisperapi.NewClient(srv.URL, "")
	c.InitialBackoff = time.Millisecond
	c.MaxBackoff = time.Millisecond
	return c, &calls
}

// speechWindowTranscriber answers with speech on every request except the
// 1-based request indices in silentAt, which get the no-speech result (empty
// text, no error, as the adapters now return it). failAll fails every request.
type speechWindowTranscriber struct {
	mu       sync.Mutex
	calls    int
	silentAt map[int]bool
	silent   bool
	failAll  error
	// failAt fails the 1-based request indices it names with the given error.
	failAt map[int]error
}

func (s *speechWindowTranscriber) Transcribe(ctx context.Context, relPath string, data []byte) (string, error) {
	res, err := s.TranscribeStructured(ctx, relPath, data)
	return res.Text, err
}

func (s *speechWindowTranscriber) TranscribeStructured(_ context.Context, _ string, _ []byte) (model.TranscriptResult, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()
	if s.failAll != nil {
		return model.TranscriptResult{}, s.failAll
	}
	if err := s.failAt[n]; err != nil {
		return model.TranscriptResult{}, err
	}
	if s.silent || s.silentAt[n] {
		return model.TranscriptResult{}, nil
	}
	return model.TranscriptResult{
		Text:     fmt.Sprintf("[00:00] spoken line of decode %d", n),
		Language: "en",
	}, nil
}

func (s *speechWindowTranscriber) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// noSpeechService builds an ingest service over a real store with a run state
// and a captured log. totalMS > 0 schedules a windowed decode through stub
// probe and cut functions; totalMS == 0 makes the duration probe fail, so the
// recording goes to STT in one request.
func noSpeechService(t *testing.T, root, stateDir string, st model.Store, tr model.Transcriber, totalMS int) (*ingest.Service, *appstate.IndexingState, *syncBuffer) {
	t.Helper()
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: stateDir, STTProvider: "off"}, st)
	state := appstate.NewIndexingState(appstate.ModeIncremental)
	svc.SetIndexingState(state)
	logs := &syncBuffer{}
	svc.SetLogger(log.New(logs, "", 0))
	svc.SetTranscriber(tr)
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) {
		if totalMS <= 0 {
			return 0, errors.New("no duration in this test")
		}
		return time.Duration(totalMS) * time.Millisecond, nil
	}
	svc.ExtractSegmentFunc = func(context.Context, string, int, int) ([]byte, error) {
		return []byte("sliced-audio-bytes"), nil
	}
	svc.ExtractAudioTrackFunc = func(context.Context, string) ([]byte, error) {
		return []byte("extracted-audio-track"), nil
	}
	return svc, state, logs
}

// assertNoSpeechOutcome checks the durable outcome of a no-speech recording: the
// skip row, the skip_summary category, the run counters, no transcript, and no
// provider-failure line in the log.
func assertNoSpeechOutcome(t *testing.T, st *store.SQLiteStore, state *appstate.IndexingState, logs *syncBuffer, relPath string) {
	t.Helper()
	doc := mustGetDoc(t, st, relPath)
	if doc.Status != "skipped" || doc.SkipReason != model.SkipReasonNoSpeech {
		t.Fatalf("%s: status=%q skip_reason=%q error=%q, want skipped/no_speech", relPath, doc.Status, doc.SkipReason, doc.ErrorMessage)
	}
	// The row's detail carries the STT identity token (spec 0.98.0, #1184),
	// never a provider error.
	if !strings.HasPrefix(doc.ErrorMessage, "no speech (stt_identity=") {
		t.Errorf("%s: a no_speech skip must carry the identity detail, got %q", relPath, doc.ErrorMessage)
	}
	stats, err := st.CorpusStats(context.Background())
	if err != nil {
		t.Fatalf("CorpusStats: %v", err)
	}
	if stats.SkipSummary == nil || stats.SkipSummary.Categories[model.SkipReasonNoSpeech] != 1 {
		t.Fatalf("skip_summary must count one no_speech, got %+v", stats.SkipSummary)
	}
	if stats.Errors != 0 {
		t.Errorf("corpus errors = %d, want 0: no speech is not a failure", stats.Errors)
	}
	snap := state.Snapshot()
	if snap.Skipped != 1 || snap.Errors != 0 || snap.Indexed != 0 {
		t.Errorf("run counters skipped=%d errors=%d indexed=%d, want 1/0/0", snap.Skipped, snap.Errors, snap.Indexed)
	}
	if meta, err := st.RepresentationMetaByType(context.Background(), relPath, ingest.RepTypeTranscript); err == nil && meta != "" {
		t.Errorf("%s: a no_speech recording must have no transcript representation, got meta %s", relPath, meta)
	}
	assertNoSpeechLog(t, logs)
}

// assertNoSpeechLog checks that the log names the no-speech outcome and holds
// no provider-failure line.
func assertNoSpeechLog(t *testing.T, logs *syncBuffer) {
	t.Helper()
	for _, bad := range []string{"WHISPER_FAILED", "transcription skipped", "no text content", "no representation produced"} {
		if strings.Contains(logs.String(), bad) {
			t.Errorf("log must not report %q for a no-speech recording:\n%s", bad, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "stt: no speech in") {
		t.Errorf("log must say the recording holds no speech:\n%s", logs.String())
	}
}

// TestNoSpeech_WindowScopeRecordsDecodedWindows: under language_scope=window a
// no-speech window is recorded as decoded with no language, never refused for
// the language the decoder made up on music, so the recording is ok and whole.
func TestNoSpeech_WindowScopeRecordsDecodedWindows(t *testing.T) {
	t.Parallel()
	const totalMS = 30 * 60 * 1000
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "long.mp3"), "fake-audio")
	st := newRealStore(t)
	tr := &speechWindowTranscriber{silentAt: map[int]bool{2: true}}
	stateDir := testutil.TempDir(t)
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: stateDir, STTProvider: "off", MediaSTTLanguageScope: "window"}, st)
	state := appstate.NewIndexingState(appstate.ModeIncremental)
	svc.SetIndexingState(state)
	logs := &syncBuffer{}
	svc.SetLogger(log.New(logs, "", 0))
	svc.SetTranscriber(tr)
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return totalMS * time.Millisecond, nil }
	svc.ExtractSegmentFunc = func(context.Context, string, int, int) ([]byte, error) { return []byte("slice"), nil }
	svc.SetPartialTranscriptFloor(0.99, "skip")

	f := ingest.DiscoveredFile{RelPath: "long.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	doc := mustGetDoc(t, st, "long.mp3")
	if doc.Status != "ok" {
		t.Fatalf("status=%q skip_reason=%q, want ok", doc.Status, doc.SkipReason)
	}
	meta, err := st.RepresentationMetaByType(context.Background(), "long.mp3", ingest.RepTypeTranscript)
	if err != nil || meta == "" {
		t.Fatalf("transcript representation missing: meta=%q err=%v", meta, err)
	}
	cov, _ := transcriptCoverageFromMeta(t, meta)
	if cov.WindowsAttempted != 4 || cov.WindowsDecoded != 4 {
		t.Errorf("coverage windows %d/%d, want 4/4", cov.WindowsDecoded, cov.WindowsAttempted)
	}
	if !strings.Contains(logs.String(), "holds no speech; it counts as decoded") {
		t.Errorf("the no-speech window must be logged as decoded:\n%s", logs.String())
	}
}

// fixedWhisperServer answers every request with body and counts the requests.
func fixedWhisperServer(t *testing.T, body string) (*whisperapi.Client, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := whisperapi.NewClient(srv.URL, "")
	c.InitialBackoff = time.Millisecond
	c.MaxBackoff = time.Millisecond
	return c, &calls
}

// TestNoSpeech_IdentifierThatHearsNoSpeechIsNoIdentification pins the §8.2.4
// item identifier path. An identifier whose probe holds no speech and reports
// no language gives no identification: no WHISPER_FAILED, a log line that says
// it found no speech, and the item decodes on the default profile. When that
// decode finds speech the item is indexed; when it finds none too, the item is
// a no_speech skip.
func TestNoSpeech_IdentifierThatHearsNoSpeechIsNoIdentification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		silent   bool
		wantStat string
	}{
		{"default decode has speech", false, "ok"},
		{"default decode has none", true, "skipped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := testutil.TempDir(t)
			writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
			st := newRealStore(t)
			def := &speechWindowTranscriber{silent: tc.silent}
			svc, state, logs := noSpeechService(t, root, testutil.TempDir(t), st, def, 0)
			lid, lidCalls := fixedWhisperServer(t, `{"text":"","segments":[]}`)
			svc.SetLanguageIdentifier(lid, "lid")

			f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
			if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
				t.Fatalf("ProcessDocument: %v", err)
			}
			if got := atomic.LoadInt32(lidCalls); got != 1 {
				t.Errorf("identifier requests = %d, want 1", got)
			}
			if def.count() != 1 {
				t.Errorf("default profile decodes = %d, want 1", def.count())
			}
			out := logs.String()
			if !strings.Contains(out, `identifier "lid" found no speech`) {
				t.Errorf("log must say the identifier found no speech:\n%s", out)
			}
			if strings.Contains(out, "WHISPER_FAILED") || strings.Contains(out, `identifier "lid" failed`) {
				t.Errorf("an identifier that hears no speech has not failed:\n%s", out)
			}
			doc := mustGetDoc(t, st, "talk.mp3")
			if doc.Status != tc.wantStat {
				t.Fatalf("status=%q skip_reason=%q, want %s", doc.Status, doc.SkipReason, tc.wantStat)
			}
			if tc.silent {
				assertNoSpeechOutcome(t, st, state, logs, "talk.mp3")
			}
		})
	}
}

// TestNoSpeech_IdentifierLanguageWithEmptyTextIsASignal: an identifier
// response with empty text that still reports a language is a signal
// (§8.2.3): the item is routed on that language, not treated as no speech.
func TestNoSpeech_IdentifierLanguageWithEmptyTextIsASignal(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	def := &speechWindowTranscriber{}
	svc, _, logs := noSpeechService(t, root, testutil.TempDir(t), st, def, 0)
	lid, _ := fixedWhisperServer(t, `{"text":"","language":"en","language_probability":0.95,"segments":[]}`)
	svc.SetLanguageIdentifier(lid, "lid")

	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, `identifier "lid" resolved "en"`) {
		t.Errorf("the reported language must be used as a signal:\n%s", out)
	}
	if strings.Contains(out, "found no speech") {
		t.Errorf("a response that reports a language is not no identification:\n%s", out)
	}
	if doc := mustGetDoc(t, st, "talk.mp3"); doc.Status != "ok" {
		t.Fatalf("status=%q skip_reason=%q, want ok", doc.Status, doc.SkipReason)
	}
}

// TestNoSpeech_FailedWindowBesideNoSpeechIsAnError pins §8.6.13: when one
// window failed and every other window holds no speech, the recording is an
// error and retried, because the failed window can hold speech.
func TestNoSpeech_FailedWindowBesideNoSpeechIsAnError(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "long.mp3"), "fake-audio")
	st := newRealStore(t)
	tr := &speechWindowTranscriber{silent: true, failAt: map[int]error{3: &model.ProviderError{Code: "WHISPER_FAILED", Message: "upstream returned non-200 response"}}}
	svc, state, _ := noSpeechService(t, root, testutil.TempDir(t), st, tr, 30*60*1000)

	f := ingest.DiscoveredFile{RelPath: "long.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	doc := mustGetDoc(t, st, "long.mp3")
	if doc.Status != "error" || doc.SkipReason != "" {
		t.Fatalf("status=%q skip_reason=%q, want error: a failed window can hold speech", doc.Status, doc.SkipReason)
	}
	if !strings.Contains(doc.ErrorMessage, "every other window holds no speech") {
		t.Errorf("error message must say why, got %q", doc.ErrorMessage)
	}
	if snap := state.Snapshot(); snap.Errors != 1 || snap.Skipped != 0 {
		t.Errorf("run counters errors=%d skipped=%d, want 1/0", snap.Errors, snap.Skipped)
	}
}

// TestNoSpeech_LowDensityLeavesOutNoSpeechWindows pins §8.6.6: the low-density
// check does not count no-speech windows in the duration. One short line in a
// 30-minute recording of music is too sparse over 30 minutes and dense enough
// over the one window that holds speech.
func TestNoSpeech_LowDensityLeavesOutNoSpeechWindows(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "long.mp3"), "fake-audio")
	st := newRealStore(t)
	client, _ := scriptedWhisperServer(t, map[int32]bool{1: true})
	svc, _, logs := noSpeechService(t, root, testutil.TempDir(t), st, client, 30*60*1000)
	cfg := quality.DefaultConfig()
	cfg.Density.MinCharsPerMinute = 1.0
	svc.SetQualityGate(quality.New(cfg))

	f := ingest.DiscoveredFile{RelPath: "long.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	if doc := mustGetDoc(t, st, "long.mp3"); doc.Status != "ok" {
		t.Fatalf("status=%q error=%q, want ok: no-speech windows are not low density\n%s", doc.Status, doc.ErrorMessage, logs.String())
	}
	if strings.Contains(logs.String(), "low_text_density") {
		t.Errorf("the low-density check must not count no-speech windows:\n%s", logs.String())
	}
}
