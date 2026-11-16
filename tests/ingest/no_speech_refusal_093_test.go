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

// SPEC §8.6.13 (0.93.0): a window that the quality gate refused counts as a
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
	svc, st, _, f := refusalBesideNoSpeechService(t, map[int]model.TranscriptResult{1: loopWindow()})
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
	// so no guard stops a later run, and the next incremental run processes the
	// errored document again and reaches the same verdict.
	if rec, err := st.GetSetting(ctx, store.NoSpeechIdentitySettingKey("talk.mp3")); err == nil && rec != "" {
		t.Errorf("a no-speech record %q was stored for a recording with a refused window", rec)
	}
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("second run: %v", err)
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
