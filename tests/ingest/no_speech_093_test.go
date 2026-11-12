package tests

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
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

// SPEC 0.93.0. On a live archive, 28 of the first 1,500 videos failed with
// "WHISPER_FAILED: transcription response had no text content". Their audio is
// real but holds only music or ambient sound, and both STT servers answered with
// a well-formed {"text":"","segments":[]}. These tests pin the no-speech outcome:
// a successful decode with nothing said is a no_speech skip, never a provider
// failure, and a no-speech window is decoded audio, never a coverage shortfall.

// measuredNoSpeech is the response both STT servers returned on music.
const measuredNoSpeech = `{"task":"transcribe","language":"nn","duration":60.0,"text":"","segments":[]}`

// legacyNoTextError is the provider error an older binary recorded for a
// no-speech recording.
var legacyNoTextError = &model.ProviderError{
	Code:      "WHISPER_FAILED",
	Message:   "transcription response had no text content",
	Retryable: false,
}

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

// assertNoSpeechSkip checks the durable outcome of a no-speech recording: the
// skip row, the skip_summary category, the run counters, no transcript, and no
// provider-failure line in the log.
func assertNoSpeechSkip(t *testing.T, st *store.SQLiteStore, state *appstate.IndexingState, logs *syncBuffer, relPath string) {
	t.Helper()
	doc := mustGetDoc(t, st, relPath)
	if doc.Status != "skipped" || doc.SkipReason != model.SkipReasonNoSpeech {
		t.Fatalf("%s: status=%q skip_reason=%q error=%q, want skipped/no_speech", relPath, doc.Status, doc.SkipReason, doc.ErrorMessage)
	}
	if doc.ErrorMessage != "" {
		t.Errorf("%s: a no_speech skip must carry no error message, got %q", relPath, doc.ErrorMessage)
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
	if !strings.Contains(logs.String(), "holds no speech") {
		t.Errorf("log must say the recording holds no speech:\n%s", logs.String())
	}
}

// TestNoSpeech_SingleDecodeIsANoSpeechSkip drives the measured response through
// the real whisper adapter: an audio file and a video file whose one decode
// finds no speech are recorded as skipped with skip_reason=no_speech.
func TestNoSpeech_SingleDecodeIsANoSpeechSkip(t *testing.T) {
	t.Parallel()
	for _, relPath := range []string{"music.mp3", "ambient.mp4"} {
		t.Run(relPath, func(t *testing.T) {
			t.Parallel()
			root := testutil.TempDir(t)
			writeFile(t, filepath.Join(root, relPath), "fake-media-bytes")
			st := newRealStore(t)
			client, calls := noSpeechWhisperServer(t)
			svc, state, logs := noSpeechService(t, root, testutil.TempDir(t), st, client, 0)

			f := ingest.DiscoveredFile{RelPath: relPath, SizeBytes: 16, MTimeUnix: time.Now().Unix()}
			if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
				t.Fatalf("ProcessDocument: %v", err)
			}
			assertNoSpeechSkip(t, st, state, logs, relPath)
			if got := atomic.LoadInt32(calls); got != 1 {
				t.Errorf("STT requests = %d, want 1", got)
			}
		})
	}
}

// TestNoSpeech_UpgradeRetriesTheLegacyError is the upgrade path a live corpus
// depends on. An older binary recorded a no-speech video as status=error with
// the WHISPER_FAILED "no text content" message. On the next incremental run of
// this binary (a daemon restart over the same state) the errored document is
// retried, decoded, and recorded as a no_speech skip. The run after that does
// not send it to the provider again: a skip is not an error to retry.
func TestNoSpeech_UpgradeRetriesTheLegacyError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "clip.mp4"), "fake-video-bytes")
	st := newRealStore(t)
	f := ingest.DiscoveredFile{RelPath: "clip.mp4", SizeBytes: 16, MTimeUnix: time.Now().Unix()}

	// The older binary: the provider error is what it recorded.
	old, _, _ := noSpeechService(t, root, stateDir, st, &speechWindowTranscriber{failAll: legacyNoTextError}, 0)
	if err := old.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("legacy run: %v", err)
	}
	if doc := mustGetDoc(t, st, "clip.mp4"); doc.Status != "error" || !strings.Contains(doc.ErrorMessage, "no text content") {
		t.Fatalf("legacy precondition: status=%q error=%q, want the WHISPER_FAILED error row", doc.Status, doc.ErrorMessage)
	}

	// The daemon restarts on this binary, over the same store and state dir.
	client, calls := noSpeechWhisperServer(t)
	svc, state, logs := noSpeechService(t, root, stateDir, st, client, 0)
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("upgrade run: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("the errored document must be retried on the next run: STT requests = %d, want 1", got)
	}
	assertNoSpeechSkip(t, st, state, logs, "clip.mp4")

	// The next run: unchanged file, no_speech skip, no new decode.
	again, againState, _ := noSpeechService(t, root, stateDir, st, client, 0)
	if err := again.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("third run: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("a no_speech skip must not be decoded again while unchanged: STT requests = %d, want 1", got)
	}
	if doc := mustGetDoc(t, st, "clip.mp4"); doc.Status != "skipped" || doc.SkipReason != model.SkipReasonNoSpeech {
		t.Errorf("third run: status=%q skip_reason=%q, want skipped/no_speech", doc.Status, doc.SkipReason)
	}
	if snap := againState.Snapshot(); snap.Skipped != 1 || snap.Errors != 0 {
		t.Errorf("third run counters skipped=%d errors=%d, want 1/0", snap.Skipped, snap.Errors)
	}
}

// TestNoSpeech_ForcedReindexReadsTheCache: a forced reindex reprocesses the
// document, and the transcript cache remembers the no-speech decode, so the same
// bytes are not sent to the provider again and the outcome is the same.
func TestNoSpeech_ForcedReindexReadsTheCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "music.mp3"), "fake-audio-bytes")
	st := newRealStore(t)
	client, calls := noSpeechWhisperServer(t)
	f := ingest.DiscoveredFile{RelPath: "music.mp3", SizeBytes: 16, MTimeUnix: time.Now().Unix()}

	first, _, _ := noSpeechService(t, root, stateDir, st, client, 0)
	if err := first.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("first run: %v", err)
	}
	svc, state, logs := noSpeechService(t, root, stateDir, st, client, 0)
	if err := svc.ProcessDocument(ctx, f, nil, true); err != nil {
		t.Fatalf("forced run: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("a cached no-speech decode must not reach the provider again: STT requests = %d, want 1", got)
	}
	assertNoSpeechSkip(t, st, state, logs, "music.mp3")
}

// assertWholeCoverage checks that the persisted transcript records every one of
// its windows as decoded and the whole recording as covered.
func assertWholeCoverage(t *testing.T, st *store.SQLiteStore, relPath string, windows, totalMS int) {
	t.Helper()
	meta, err := st.RepresentationMetaByType(context.Background(), relPath, ingest.RepTypeTranscript)
	if err != nil || meta == "" {
		t.Fatalf("transcript representation missing: meta=%q err=%v", meta, err)
	}
	cov, present := transcriptCoverageFromMeta(t, meta)
	if !present {
		t.Fatal("a windowed transcript must record its coverage")
	}
	if cov.WindowsAttempted != windows || cov.WindowsDecoded != windows {
		t.Errorf("coverage windows %d/%d, want %d/%d: a no-speech window counts as decoded", cov.WindowsDecoded, cov.WindowsAttempted, windows, windows)
	}
	if cov.DecodedMS != totalMS || cov.DurationMS != totalMS {
		t.Errorf("coverage decoded_ms=%d duration_ms=%d, want both %d", cov.DecodedMS, cov.DurationMS, totalMS)
	}
}

// TestNoSpeech_MixedWindowsAreFullCoverage: a 30-minute recording decodes in
// four windows, and the middle two hold no speech. Every window decoded, so the
// transcript covers the whole recording: a no-speech window is decoded audio,
// not a shortfall, and a strict floor (min_coverage 0.99, skip) does not refuse
// it. The windows with speech are joined.
func TestNoSpeech_MixedWindowsAreFullCoverage(t *testing.T) {
	t.Parallel()
	const totalMS = 30 * 60 * 1000
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "long.mp3"), "fake-audio")
	st := newRealStore(t)
	// The real whisper adapter: windows 1 and 4 hold speech, 2 and 3 answer
	// with the measured no-speech body.
	client, calls := scriptedWhisperServer(t, map[int32]bool{1: true, 4: true})
	stateDir := testutil.TempDir(t)
	svc, state, logs := noSpeechService(t, root, stateDir, st, client, totalMS)
	svc.SetPartialTranscriptFloor(0.99, "skip")

	f := ingest.DiscoveredFile{RelPath: "long.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	if n := atomic.LoadInt32(calls); n != 4 {
		t.Fatalf("decode requests = %d, want 4 windows", n)
	}
	if doc := mustGetDoc(t, st, "long.mp3"); doc.Status != "ok" {
		t.Fatalf("status=%q skip_reason=%q, want ok: two windows hold speech", doc.Status, doc.SkipReason)
	}
	assertWholeCoverage(t, st, "long.mp3", 4, totalMS)
	// The merged transcript, as the run cached it (no STT identity is set, so
	// the cache key is the media bytes alone).
	raw, err := os.ReadFile(filepath.Join(stateDir, "cache", "transcribe", ingest.ComputeContentHash([]byte("fake-audio"))+".txt"))
	if err != nil {
		t.Fatalf("read the cached transcript: %v", err)
	}
	for _, want := range []string{"spoken line 1", "spoken line 4"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the windows with speech must be joined: %q missing from %q", want, raw)
		}
	}
	if strings.Contains(logs.String(), "skip window") || strings.Contains(logs.String(), "PARTIAL") {
		t.Errorf("a no-speech window must not be logged as a failed window or a partial transcript:\n%s", logs.String())
	}
	if snap := state.Snapshot(); snap.Indexed != 1 || snap.Skipped != 0 || snap.Errors != 0 {
		t.Errorf("run counters indexed=%d skipped=%d errors=%d, want 1/0/0", snap.Indexed, snap.Skipped, snap.Errors)
	}
}

// TestNoSpeech_EveryWindowSilentIsANoSpeechSkip: when every window of a
// windowed decode holds no speech, the recording is a no_speech skip. Under a
// strict coverage floor it is still no_speech, not transcript_partial: nothing
// was missed.
func TestNoSpeech_EveryWindowSilentIsANoSpeechSkip(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "long.mp3"), "fake-audio")
	st := newRealStore(t)
	client, calls := noSpeechWhisperServer(t)
	svc, state, logs := noSpeechService(t, root, testutil.TempDir(t), st, client, 30*60*1000)
	svc.SetPartialTranscriptFloor(0.99, "skip")

	f := ingest.DiscoveredFile{RelPath: "long.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	if n := atomic.LoadInt32(calls); n != 4 {
		t.Fatalf("decode requests = %d, want 4 windows", n)
	}
	assertNoSpeechSkip(t, st, state, logs, "long.mp3")
	if strings.Contains(logs.String(), "all 4 windows failed") {
		t.Errorf("no-speech windows must not be reported as failed:\n%s", logs.String())
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
				assertNoSpeechSkip(t, st, state, logs, "talk.mp3")
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

// TestNoSpeech_IdentityChangeDecodesAgain pins the §8.6.13 re-decode guard: a
// no-speech recording is not decoded again while its content and the STT
// derivation identity are unchanged, and is decoded again when the identity
// changes, because another model can find speech that this one did not.
func TestNoSpeech_IdentityChangeDecodesAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "clip.mp4"), "fake-video-bytes")
	st := newRealStore(t)
	f := ingest.DiscoveredFile{RelPath: "clip.mp4", SizeBytes: 16, MTimeUnix: time.Now().Unix()}
	client, calls := noSpeechWhisperServer(t)

	run := func(sttModel string) *speechWindowTranscriber {
		svc, _, _ := noSpeechService(t, root, stateDir, st, client, 0)
		svc.SetSTTIdentity("whisper", sttModel)
		if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
			t.Fatalf("ProcessDocument (%s): %v", sttModel, err)
		}
		return nil
	}
	run("large-v3")
	run("large-v3")
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("an unchanged no-speech recording must not be decoded again: STT requests = %d, want 1", got)
	}
	run("gigaam-v2")
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("an STT identity change must decode the recording again: STT requests = %d, want 2", got)
	}
	if doc := mustGetDoc(t, st, "clip.mp4"); doc.Status != "skipped" || doc.SkipReason != model.SkipReasonNoSpeech {
		t.Errorf("status=%q skip_reason=%q, want skipped/no_speech", doc.Status, doc.SkipReason)
	}
	run("gigaam-v2")
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Errorf("the new identity is recorded, so the next run decodes nothing: STT requests = %d, want 2", got)
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

// TestNoSpeech_FailedWindowStaysAProviderFailure: a recording whose every
// window FAILED (not a no-speech answer) is still a provider failure, recorded
// as status=error and retried. Only a well-formed empty answer is no speech.
func TestNoSpeech_FailedWindowStaysAProviderFailure(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "long.mp3"), "fake-audio")
	st := newRealStore(t)
	tr := &speechWindowTranscriber{failAll: &model.ProviderError{Code: "WHISPER_FAILED", Message: "failed to decode transcription response"}}
	svc, _, _ := noSpeechService(t, root, testutil.TempDir(t), st, tr, 30*60*1000)

	f := ingest.DiscoveredFile{RelPath: "long.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	if doc := mustGetDoc(t, st, "long.mp3"); doc.Status != "error" || doc.SkipReason != "" {
		t.Fatalf("status=%q skip_reason=%q, want error with no skip reason", doc.Status, doc.SkipReason)
	}
}
