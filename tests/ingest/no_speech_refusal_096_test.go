package tests

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/quality"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §8.6.13 (0.96.0): a window that the quality gate refused counts as a
// failed window next to no-speech windows. When every window is a no-speech
// result or a quality-gate refusal, and at least one is a refusal, the outcome
// is the TRANSCRIBE_FAILED error and the recording is retried. With a
// language_uncovered refusal the mixed-reason rule of §8.2.2 applies. The skip
// reason no_speech applies only when every window is a no-speech result.

// refusalBesideNoSpeechService builds a window-scope service over a real store
// whose recording decodes in four windows. The transcriber answers each window
// in order; a window with no scripted reply is a no-speech result.
func refusalBesideNoSpeechService(t *testing.T, replies map[int]model.TranscriptResult) (*ingest.Service, *store.SQLiteStore, *replyTranscriber, ingest.DiscoveredFile) {
	t.Helper()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	cfg := config.Config{RootDir: root, StateDir: testutil.TempDir(t), STTProvider: "off"}
	svc := mustNewIngestService(t, cfg, st)
	svc.SetIndexingState(appstate.NewIndexingState(appstate.ModeIncremental))
	tr := &replyTranscriber{replies: replies}
	svc.SetTranscriber(tr)
	svc.SetSTTIdentity("whisper", "large-v3")
	svc.SetSTTLanguages([]string{"ru"})
	svc.SetOnUncoveredLanguage("skip")
	svc.SetLanguageScope("window")
	svc.SetQualityGate(quality.New(quality.DefaultConfig()))
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) {
		return time.Duration(scopeTotalMS) * time.Millisecond, nil
	}
	svc.ExtractSegmentFunc = func(_ context.Context, _ string, startMS, endMS int) ([]byte, error) {
		return make([]byte, 10*(endMS-startMS)/scopeTotalMS+1), nil
	}
	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	return svc, st, tr, f
}

// loopWindow is a decode that the per-window quality gate refuses.
func loopWindow() model.TranscriptResult {
	return model.TranscriptResult{Text: loopText, Language: "ru", LanguageConfidence: 0.9}
}

// uncoveredWindow is a decode in a language that the profile does not declare.
func uncoveredWindow() model.TranscriptResult {
	return model.TranscriptResult{
		Text:     "[00:00] opening line spoken at some length\n[00:30] closing line spoken at some length",
		Language: "uk", LanguageConfidence: 0.9,
	}
}

// TestNoSpeech_QualityRefusalBesideNoSpeechIsTranscribeFailed: one window is a
// loop that the gate refuses and every other window holds no speech. The
// refused window can hold speech, so the document is an error with
// TRANSCRIBE_FAILED, and the next run decodes it again.
//
// Mutant killed: reading "a window decoded" as "a window produced text", which
// settles the recording as silence (status ok with no transcript) or as a
// no_speech skip and never retries it.
func TestNoSpeech_QualityRefusalBesideNoSpeechIsTranscribeFailed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, st, tr, f := refusalBesideNoSpeechService(t, map[int]model.TranscriptResult{1: loopWindow()})
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("ProcessDocument hard-failed: %v", err)
	}
	doc := mustGetDoc(t, st, "talk.mp3")
	if doc.Status != "error" || !strings.Contains(doc.ErrorMessage, "TRANSCRIBE_FAILED") {
		t.Fatalf("talk.mp3: status=%q skip_reason=%q error=%q, want status=error with TRANSCRIBE_FAILED", doc.Status, doc.SkipReason, doc.ErrorMessage)
	}
	if doc.SkipReason == model.SkipReasonNoSpeech {
		t.Errorf("skip_reason = no_speech: it applies only when every window is a no-speech result")
	}
	if meta, err := st.RepresentationMetaByType(ctx, "talk.mp3", ingest.RepTypeTranscript); err == nil && meta != "" {
		t.Errorf("a recording with no persisted text must have no transcript, got meta %s", meta)
	}

	// The outcome is not settled as no speech: no no-speech record is stored,
	// so no guard stops a later run.
	if rec, err := st.GetSetting(ctx, store.NoSpeechIdentitySettingKey("talk.mp3")); err == nil && rec != "" {
		t.Errorf("a no-speech record %q was stored for a recording with a refused window", rec)
	}

	// The next incremental run processes the errored document again. It reads
	// the cached decode (the refused windows are a decode result, and a decode
	// result is cached), so it sends no new request and reaches the same
	// verdict. The scripted replies would give speech on a new request, so a
	// run that decoded again would end as ok.
	tr.mu.Lock()
	before := tr.calls
	tr.def = model.TranscriptResult{Text: ruText, Language: "ru", LanguageConfidence: 0.9}
	tr.mu.Unlock()
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("second run: %v", err)
	}
	tr.mu.Lock()
	after := tr.calls
	tr.mu.Unlock()
	if after != before {
		t.Errorf("the second run sent %d decode request(s), want 0: the refused decode is read from the transcript cache", after-before)
	}
	doc = mustGetDoc(t, st, "talk.mp3")
	if doc.Status != "error" || !strings.Contains(doc.ErrorMessage, "TRANSCRIBE_FAILED") {
		t.Fatalf("second run: status=%q skip_reason=%q error=%q, want status=error with TRANSCRIBE_FAILED", doc.Status, doc.SkipReason, doc.ErrorMessage)
	}
}

// TestNoSpeech_LanguageRefusalBesideNoSpeechIsALanguageSkip: one window is
// refused for its language under skip and every other window holds no speech.
// No window produced text and a refusal is language_uncovered, so the §8.2.2
// rule applies: skipped with language_uncovered, not no_speech.
func TestNoSpeech_LanguageRefusalBesideNoSpeechIsALanguageSkip(t *testing.T) {
	t.Parallel()
	svc, st, _, f := refusalBesideNoSpeechService(t, map[int]model.TranscriptResult{1: uncoveredWindow()})
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument hard-failed: %v", err)
	}
	doc := mustGetDoc(t, st, "talk.mp3")
	if doc.Status != "skipped" || doc.SkipReason != model.SkipReasonLanguageUncovered {
		t.Fatalf("talk.mp3: status=%q skip_reason=%q error=%q, want skipped/%s", doc.Status, doc.SkipReason, doc.ErrorMessage, model.SkipReasonLanguageUncovered)
	}
}

// TestNoSpeech_BothRefusalsBesideNoSpeechAreALanguageSkip: refusals of both
// reasons next to no-speech windows keep the mixed-reason rule of §8.2.2.
func TestNoSpeech_BothRefusalsBesideNoSpeechAreALanguageSkip(t *testing.T) {
	t.Parallel()
	svc, st, _, f := refusalBesideNoSpeechService(t, map[int]model.TranscriptResult{1: uncoveredWindow(), 3: loopWindow()})
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument hard-failed: %v", err)
	}
	doc := mustGetDoc(t, st, "talk.mp3")
	if doc.Status != "skipped" || doc.SkipReason != model.SkipReasonLanguageUncovered {
		t.Fatalf("talk.mp3: status=%q skip_reason=%q error=%q, want skipped/%s", doc.Status, doc.SkipReason, doc.ErrorMessage, model.SkipReasonLanguageUncovered)
	}
}

// windowProviderFailure is a provider failure of one decode window.
var windowProviderFailure = &model.ProviderError{Code: "WHISPER_FAILED", Message: "upstream 502", Retryable: false}

// assertFailedWindowWins checks the SPEC 0.96.0 §8.6.13 precedence rule: the
// document is a transcription failure (the class that the run manifest reports
// as TRANSCRIBE_FAILED), not a declared skip.
func assertFailedWindowWins(t *testing.T, st *store.SQLiteStore) {
	t.Helper()
	doc := mustGetDoc(t, st, "talk.mp3")
	if doc.Status != "error" || !strings.Contains(doc.ErrorMessage, "transcript provider failure") {
		t.Fatalf("talk.mp3: status=%q skip_reason=%q error=%q, want status=error with a transcript provider failure (a failed window takes precedence)", doc.Status, doc.SkipReason, doc.ErrorMessage)
	}
	if doc.SkipReason != "" {
		t.Errorf("skip_reason = %q, want none on an error", doc.SkipReason)
	}
}

// TestNoSpeech_FailedWindowWinsOverLanguageRefusalAndNoSpeech: one window is
// refused for its language, one window fails at the provider, and the other
// windows hold no speech. No window decoded to text. The failed window can
// hold speech in a covered language, so the document is an error and the next
// run decodes it again. It is not the language_uncovered skip.
//
// Mutant killed: judging the refusals first, which records a skip that hides
// the failure and is never retried as an error.
func TestNoSpeech_FailedWindowWinsOverLanguageRefusalAndNoSpeech(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, st, tr, f := refusalBesideNoSpeechService(t, map[int]model.TranscriptResult{1: uncoveredWindow()})
	tr.errAt = map[int]error{2: windowProviderFailure}
	_ = svc.ProcessDocument(ctx, f, nil, false)
	assertFailedWindowWins(t, st)

	// The error is retried with a real decode: the failed decode is not cached.
	tr.mu.Lock()
	before := tr.calls
	tr.errAt = nil
	tr.replies = map[int]model.TranscriptResult{}
	tr.def = model.TranscriptResult{Text: ruText, Language: "ru", LanguageConfidence: 0.9}
	tr.mu.Unlock()
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("retry run: %v", err)
	}
	if tr.calls == before {
		t.Fatalf("the retry run sent no decode request: the failed decode must not be served from the cache")
	}
	if doc := mustGetDoc(t, st, "talk.mp3"); doc.Status != "ok" {
		t.Fatalf("after the retry: status=%q error=%q, want ok", doc.Status, doc.ErrorMessage)
	}
}

// TestNoSpeech_FailedWindowWinsOverLanguageRefusals: every window that did not
// fail is refused for its language (no window decoded at all). The failed
// window still takes precedence: error, not the language_uncovered skip.
func TestNoSpeech_FailedWindowWinsOverLanguageRefusals(t *testing.T) {
	t.Parallel()
	svc, st, tr, f := refusalBesideNoSpeechService(t, nil)
	tr.def = uncoveredWindow()
	tr.errAt = map[int]error{3: windowProviderFailure}
	_ = svc.ProcessDocument(context.Background(), f, nil, false)
	assertFailedWindowWins(t, st)
}

// TestNoSpeech_AllRefusedWithNoFailedWindowIsUnchanged pins the other side of
// the precedence rule: with no failed window, every window refused for its
// language is still the language_uncovered skip.
func TestNoSpeech_AllRefusedWithNoFailedWindowIsUnchanged(t *testing.T) {
	t.Parallel()
	svc, st, tr, f := refusalBesideNoSpeechService(t, nil)
	tr.def = uncoveredWindow()
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument hard-failed: %v", err)
	}
	doc := mustGetDoc(t, st, "talk.mp3")
	if doc.Status != "skipped" || doc.SkipReason != model.SkipReasonLanguageUncovered {
		t.Fatalf("talk.mp3: status=%q skip_reason=%q error=%q, want skipped/%s", doc.Status, doc.SkipReason, doc.ErrorMessage, model.SkipReasonLanguageUncovered)
	}
}

// fallbackNoSpeechService builds the window-scope service of these tests with
// a route candidate for Russian and media.stt.on_route_error=default. The
// candidate decodes the window that routes to it, then fails on every later
// window. The default profile finds no speech in those windows, so each of
// them is a no-speech result of a route fallback decode.
func fallbackNoSpeechService(t *testing.T, defaultReplies map[int]model.TranscriptResult, candText string, candCoverage []string, candFirstCall int) (*ingest.Service, *store.SQLiteStore, *replyTranscriber, *langWindowTranscriber, ingest.DiscoveredFile) {
	t.Helper()
	svc, st, tr, f := refusalBesideNoSpeechService(t, defaultReplies)
	cand := &langWindowTranscriber{
		def:    langReply{err: windowProviderFailure},
		script: map[int]langReply{candFirstCall: {lang: "ru", conf: 0.9, text: candText}},
	}
	svc.SetRouteTranscriber("ru", cand, "ru-ckpt", candCoverage)
	svc.SetRouteErrorFallback(true)
	return svc, st, tr, cand, f
}

// ruWindow is a decode by the default profile that reports Russian, so the
// window routes to the Russian candidate.
func ruWindow() model.TranscriptResult {
	return model.TranscriptResult{Text: ruText, Language: "ru", LanguageConfidence: 0.9}
}

// TestNoSpeech_FallbackNoSpeechWindowCountsAsFailed pins SPEC 0.96.0 §8.6.13:
// when no window produced transcript text, a window whose route failed and
// whose fallback decode is a no-speech result counts as a failed window. The
// outcome is an error and the recording is retried. The route is carried from
// an earlier window whose text was refused, which is the only way a window
// can start on a candidate route in a recording with no transcript text.
//
//   - language refusal: the candidate does not declare Russian, so window 1 is
//     refused as language_uncovered. Windows 2 to 4 start on the candidate,
//     which fails, and the default finds no speech. Without the rule the
//     document is the language_uncovered skip.
//   - quality refusal: the candidate decodes window 1 as a loop that the gate
//     refuses. Without the rule the verdict is the cached quality-gate error,
//     which sends no new request on the next run.
//   - both refusals: window 1 is refused for its language on the default
//     profile, window 2 by the gate on the candidate.
//
// Mutant killed: counting the fallback no-speech window as an ordinary
// no-speech window.
func TestNoSpeech_FallbackNoSpeechWindowCountsAsFailed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		replies       map[int]model.TranscriptResult
		candText      string
		candCoverage  []string
		candFirstCall int
	}{
		{"language refusal", map[int]model.TranscriptResult{1: ruWindow()}, ruText, []string{"de"}, 1},
		{"quality refusal", map[int]model.TranscriptResult{1: ruWindow()}, loopText, nil, 1},
		{"both refusals", map[int]model.TranscriptResult{1: uncoveredWindow(), 2: ruWindow()}, loopText, nil, 1},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			svc, st, tr, cand, f := fallbackNoSpeechService(t, tc.replies, tc.candText, tc.candCoverage, tc.candFirstCall)
			_ = svc.ProcessDocument(ctx, f, nil, false)
			if cand.callCount() < 2 {
				t.Fatalf("candidate calls = %d, want at least 2: a later window must start on the carried route and fail; the fixture is wrong", cand.callCount())
			}
			assertFailedWindowWins(t, st)
			if rec, err := st.GetSetting(ctx, store.NoSpeechIdentitySettingKey("talk.mp3")); err == nil && rec != "" {
				t.Errorf("a no-speech record %q was stored", rec)
			}

			// The error is not cached: the next run sends new requests.
			tr.mu.Lock()
			before := tr.calls
			tr.mu.Unlock()
			_ = svc.ProcessDocument(ctx, f, nil, false)
			tr.mu.Lock()
			after := tr.calls
			tr.mu.Unlock()
			if after == before {
				t.Errorf("the next run sent no decode request: the recording must be retried")
			}
		})
	}
}
