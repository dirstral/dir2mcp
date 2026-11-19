package tests

import (
	"context"
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
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// countingNoSpeechTranscriber answers every request with text (empty: the
// provider heard no speech) or err (a provider failure), and counts the calls so
// a test can tell a cache hit from a fresh STT request.
type countingNoSpeechTranscriber struct {
	mu    sync.Mutex
	text  string
	err   error
	calls int
}

func (c *countingNoSpeechTranscriber) Transcribe(_ context.Context, _ string, _ []byte) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.text, c.err
}

func (c *countingNoSpeechTranscriber) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// noSpeechFixture is one media file in a fresh corpus with its own state dir,
// so the transcript cache is shared across the scans of one test only.
type noSpeechFixture struct {
	root     string
	stateDir string
	st       *store.SQLiteStore
	file     ingest.DiscoveredFile
}

func newNoSpeechFixture(t *testing.T, relPath, body string) noSpeechFixture {
	t.Helper()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, relPath), body)
	return noSpeechFixture{
		root:     root,
		stateDir: testutil.TempDir(t),
		st:       newRealStore(t),
		file:     ingest.DiscoveredFile{RelPath: relPath, SizeBytes: int64(len(body)), MTimeUnix: time.Now().Unix()},
	}
}

// service builds a fresh ingest service over the fixture, as a daemon restart
// would: the store and the transcript cache persist, the service does not.
func (f noSpeechFixture) service(t *testing.T, tr model.Transcriber, sttModel string) (*ingest.Service, *appstate.IndexingState) {
	t.Helper()
	svc := mustNewIngestService(t, config.Config{RootDir: f.root, StateDir: f.stateDir, STTProvider: "off"}, f.st)
	if tr != nil {
		svc.SetTranscriber(tr)
		svc.SetSTTIdentity("whisper", sttModel)
	}
	svc.ExtractAudioTrackFunc = func(_ context.Context, _ string) ([]byte, error) {
		return []byte("fake-extracted-aac-audio"), nil
	}
	state := appstate.NewIndexingState(appstate.ModeIncremental)
	svc.SetIndexingState(state)
	return svc, state
}

func (f noSpeechFixture) process(t *testing.T, svc *ingest.Service, file ingest.DiscoveredFile) {
	t.Helper()
	if err := svc.ProcessDocument(context.Background(), file, nil, false); err != nil {
		t.Fatalf("ProcessDocument(%s): %v", file.RelPath, err)
	}
}

func assertNoSpeechSkip(t *testing.T, st *store.SQLiteStore, relPath string) {
	t.Helper()
	doc := documentByPath(t, st, relPath)
	if doc.Status != string(model.DocumentStatusSkipped) || doc.SkipReason != model.SkipReasonNoSpeech {
		t.Fatalf("%s status = %q skip_reason = %q (error_message %q), want skipped/no_speech (SPEC §8.6.6: silence is an outcome, not an error)",
			relPath, doc.Status, doc.SkipReason, doc.ErrorMessage)
	}
}

// TestNoSpeech_SpeechlessVideo_SkippedNotError is the #1182 regression guard: a
// video whose STT answered successfully with an empty transcript has no
// representation, and is recorded status=skipped with skip_reason=no_speech,
// counted once as a skip. Before the fix it was status=error and retried (and
// re-sent to STT) on every scan.
func TestNoSpeech_SpeechlessVideo_SkippedNotError(t *testing.T) {
	t.Parallel()
	fx := newNoSpeechFixture(t, "silent.mp4", "fake-video-with-a-silent-audio-track")
	tr := &countingNoSpeechTranscriber{}
	svc, state := fx.service(t, tr, "whisper-large-v3")
	fx.process(t, svc, fx.file)

	if got := tr.callCount(); got != 1 {
		t.Fatalf("STT calls = %d, want 1", got)
	}
	assertNoSpeechSkip(t, fx.st, "silent.mp4")
	snap := state.Snapshot()
	if snap.Skipped != 1 || snap.Errors != 0 || snap.Indexed != 0 {
		t.Errorf("counters indexed=%d skipped=%d errors=%d, want 0/1/0 (one no_speech skip, #426)", snap.Indexed, snap.Skipped, snap.Errors)
	}
}

// TestNoSpeech_SpeechlessAudio_SkippedNotOK: an audio file with an empty
// transcript used to end status=ok with no representation, an indexed document
// nothing could find. It follows the same no_speech rule as video.
func TestNoSpeech_SpeechlessAudio_SkippedNotOK(t *testing.T) {
	t.Parallel()
	fx := newNoSpeechFixture(t, "room-tone.mp3", "fake-mp3-room-tone")
	tr := &countingNoSpeechTranscriber{}
	svc, _ := fx.service(t, tr, "whisper-large-v3")
	fx.process(t, svc, fx.file)

	assertNoSpeechSkip(t, fx.st, "room-tone.mp3")
}

// TestNoSpeech_SecondScanDoesNotCallSTT: the empty transcript is cached like any
// transcript, and the skip carries the content_hash done marker, so a later scan
// neither re-transcribes the item nor turns it into an error or an ok document.
// Both incremental paths are covered: the (size, mtime) fast path, and the full
// read path (no mtime) where the transcript cache answers.
func TestNoSpeech_SecondScanDoesNotCallSTT(t *testing.T) {
	t.Parallel()
	fx := newNoSpeechFixture(t, "silent.mp4", "fake-video-with-a-silent-audio-track")
	tr := &countingNoSpeechTranscriber{}
	svc, _ := fx.service(t, tr, "whisper-large-v3")
	fx.process(t, svc, fx.file)
	assertNoSpeechSkip(t, fx.st, "silent.mp4")
	if doc := documentByPath(t, fx.st, "silent.mp4"); strings.TrimSpace(doc.ContentHash) == "" {
		t.Errorf("no_speech row carries no content_hash done marker; every scan would re-read and re-derive it")
	}

	// Second scan after a restart, cheap signal unchanged.
	svc2, state2 := fx.service(t, tr, "whisper-large-v3")
	fx.process(t, svc2, fx.file)
	if got := tr.callCount(); got != 1 {
		t.Errorf("STT calls after the fast-path rescan = %d, want 1 (a no_speech item is not sent to STT again)", got)
	}
	assertNoSpeechSkip(t, fx.st, "silent.mp4")
	if snap := state2.Snapshot(); snap.Skipped != 1 || snap.Errors != 0 {
		t.Errorf("rescan counters skipped=%d errors=%d, want 1/0", snap.Skipped, snap.Errors)
	}

	// Third scan on the full read path (no cheap signal).
	full := fx.file
	full.MTimeUnix = 0
	svc3, _ := fx.service(t, tr, "whisper-large-v3")
	fx.process(t, svc3, full)
	if got := tr.callCount(); got != 1 {
		t.Errorf("STT calls after the full-read rescan = %d, want 1 (the empty transcript is cached)", got)
	}
	assertNoSpeechSkip(t, fx.st, "silent.mp4")
}

// TestNoSpeech_STTIdentityChange_Rederives: the no_speech outcome belongs to the
// STT identity that produced it (§8.6.7). A different model may hear speech the
// first one did not, so changing the identity re-runs STT on unchanged bytes,
// and the item is indexed when the new model hears something.
func TestNoSpeech_STTIdentityChange_Rederives(t *testing.T) {
	t.Parallel()
	fx := newNoSpeechFixture(t, "silent.mp4", "fake-video-with-a-quiet-audio-track")
	tr := &countingNoSpeechTranscriber{}
	svc, _ := fx.service(t, tr, "whisper-large-v3")
	fx.process(t, svc, fx.file)
	assertNoSpeechSkip(t, fx.st, "silent.mp4")

	tr.mu.Lock()
	tr.text = "[00:00] a whisper the old model missed"
	tr.mu.Unlock()
	svc2, _ := fx.service(t, tr, "whisper-large-v4")
	fx.process(t, svc2, fx.file)
	if got := tr.callCount(); got != 2 {
		t.Fatalf("STT calls after the identity change = %d, want 2 (a new STT identity re-derives a no_speech item)", got)
	}
	doc := documentByPath(t, fx.st, "silent.mp4")
	if doc.Status != string(model.DocumentStatusOK) || doc.SkipReason != "" {
		t.Errorf("status = %q skip_reason = %q after the new model heard speech, want ok with no skip_reason", doc.Status, doc.SkipReason)
	}
}

// TestNoSpeech_ProviderFailure_StaysErrorAndRetried: a provider failure is not
// silence. It stays status=error and the next scan sends the item to STT again.
func TestNoSpeech_ProviderFailure_StaysErrorAndRetried(t *testing.T) {
	t.Parallel()
	fx := newNoSpeechFixture(t, "clip.mp4", "fake-video-bytes")
	tr := &countingNoSpeechTranscriber{err: &model.ProviderError{Code: "WHISPER_FAILED", Message: "upstream 502", Retryable: true}}
	svc, _ := fx.service(t, tr, "whisper-large-v3")
	fx.process(t, svc, fx.file)

	doc := documentByPath(t, fx.st, "clip.mp4")
	if doc.Status != string(model.DocumentStatusError) || doc.SkipReason != "" {
		t.Fatalf("status = %q skip_reason = %q, want error with no skip_reason (a provider failure is not no_speech)", doc.Status, doc.SkipReason)
	}

	svc2, _ := fx.service(t, tr, "whisper-large-v3")
	fx.process(t, svc2, fx.file)
	if got := tr.callCount(); got != 2 {
		t.Errorf("STT calls = %d, want 2 (a provider failure is retried on the next scan)", got)
	}
}

// TestNoSpeech_STTOff_StaysError: with no STT configured nothing was asked, so
// nothing was heard. A video with no other representation source keeps the
// #398 no-representation error; it is not a no_speech skip.
func TestNoSpeech_STTOff_StaysError(t *testing.T) {
	t.Parallel()
	fx := newNoSpeechFixture(t, "clip.mp4", "fake-video-bytes")
	svc, _ := fx.service(t, nil, "")
	fx.process(t, svc, fx.file)

	doc := documentByPath(t, fx.st, "clip.mp4")
	if doc.Status != string(model.DocumentStatusError) || doc.SkipReason != "" {
		t.Fatalf("status = %q skip_reason = %q, want error (STT off is not an STT answer)", doc.Status, doc.SkipReason)
	}
	if !strings.Contains(strings.ToLower(doc.ErrorMessage), "no representation") {
		t.Errorf("error_message = %q, want the no-representation diagnostic", doc.ErrorMessage)
	}
}

// scriptedSilenceTranscriber answers the Nth windowed request (1-based) with
// texts[N], else def, exactly as given: an empty string is an empty answer
// (silence), not a placeholder. Every answer reports lang at confidence 0.9.
type scriptedSilenceTranscriber struct {
	mu    sync.Mutex
	calls int
	texts map[int]string
	def   string
	lang  string
}

func (w *scriptedSilenceTranscriber) Transcribe(ctx context.Context, relPath string, data []byte) (string, error) {
	res, err := w.TranscribeStructured(ctx, relPath, data)
	return res.Text, err
}

func (w *scriptedSilenceTranscriber) TranscribeStructured(context.Context, string, []byte) (model.TranscriptResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	text, ok := w.texts[w.calls]
	if !ok {
		text = w.def
	}
	return model.TranscriptResult{Text: text, Language: w.lang, LanguageConfidence: 0.9}, nil
}

// MaxTranscribePayloadBytes declares no cap, so only the duration rule windows.
func (w *scriptedSilenceTranscriber) MaxTranscribePayloadBytes() int { return 0 }

// windowedSilenceService drives a 30-minute recording (four ~10-minute decode
// windows) through ProcessDocument with deterministic duration and cut stubs.
// scope "" keeps the default item scope; gate turns the §8.6.6 quality gate on.
func windowedSilenceService(t *testing.T, root string, st *store.SQLiteStore, tr model.Transcriber, scope string, gate bool) *ingest.Service {
	t.Helper()
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: testutil.TempDir(t), STTProvider: "off"}, st)
	svc.SetIndexingState(appstate.NewIndexingState(appstate.ModeIncremental))
	svc.SetTranscriber(tr)
	svc.SetSTTIdentity("whisper", "large-v3")
	if scope != "" {
		svc.SetLanguageScope(scope)
	}
	if gate {
		svc.SetQualityGate(quality.New(quality.DefaultConfig()))
	}
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) {
		return time.Duration(scopeTotalMS) * time.Millisecond, nil
	}
	svc.ExtractSegmentFunc = func(_ context.Context, _ string, startMS, endMS int) ([]byte, error) {
		return make([]byte, 10*(endMS-startMS)/scopeTotalMS+1), nil
	}
	return svc
}

func processWindowed(t *testing.T, svc *ingest.Service, root, relPath string) {
	t.Helper()
	writeFile(t, filepath.Join(root, relPath), "fake-audio")
	f := ingest.DiscoveredFile{RelPath: relPath, SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument(%s): %v", relPath, err)
	}
}

// TestNoSpeech_SilentWindowsCountAsDecoded: a window that completes with no
// speech was listened to, so it counts as decoded and its range is covered
// (§8.6.13). A recording with speech in its first window and silence in the
// other three is complete coverage, so a min_coverage 0.8 floor under skip does
// not drop it as transcript_partial.
func TestNoSpeech_SilentWindowsCountAsDecoded(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	st := newRealStore(t)
	tr := &scriptedSilenceTranscriber{texts: map[int]string{1: ruText}, lang: "ru"}
	svc := windowedSilenceService(t, root, st, tr, "", false)
	svc.SetPartialTranscriptFloor(0.8, "skip")
	processWindowed(t, svc, root, "half-silent.mp3")

	doc := mustGetDoc(t, st, "half-silent.mp3")
	if doc.Status != string(model.DocumentStatusOK) || doc.SkipReason != "" {
		t.Fatalf("status = %q skip_reason = %q, want ok: silent windows were decoded, not missed", doc.Status, doc.SkipReason)
	}
	cov, present := transcriptCoverageFromMeta(t, transcriptMetaFor(t, st, "half-silent.mp3"))
	if !present || cov.WindowsAttempted != 4 || cov.WindowsDecoded != 4 {
		t.Fatalf("coverage = %+v (present %v), want 4 of 4 windows decoded", cov, present)
	}
	if cov.DecodedMS < cov.DurationMS {
		t.Errorf("decoded_ms = %d of %d: the silent windows' ranges are missing from coverage", cov.DecodedMS, cov.DurationMS)
	}
}

// TestNoSpeech_FullySilentWindowedDecode: every window answers with no speech
// and none is refused. The item is a no_speech skip, not TRANSCRIBE_FAILED or a
// quality-gate empty_output error, under item scope and under window scope.
func TestNoSpeech_FullySilentWindowedDecode(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{"", "window"} {
		root := testutil.TempDir(t)
		st := newRealStore(t)
		tr := &scriptedSilenceTranscriber{lang: "ru"}
		svc := windowedSilenceService(t, root, st, tr, scope, true)
		processWindowed(t, svc, root, "silent.mp3")

		if tr.calls != 4 {
			t.Errorf("scope %q: STT calls = %d, want 4 windows", scope, tr.calls)
		}
		doc := mustGetDoc(t, st, "silent.mp3")
		if doc.Status != string(model.DocumentStatusSkipped) || doc.SkipReason != model.SkipReasonNoSpeech {
			t.Errorf("scope %q: status = %q skip_reason = %q error = %q, want skipped/no_speech", scope, doc.Status, doc.SkipReason, doc.ErrorMessage)
		}
	}
}

// TestNoSpeech_RefusedWindowStaysOutsideCoverage: silence counts as decoded, a
// refusal does not. Under window scope the second window is a repetition loop
// the per-window gate refuses; the first has speech and the last two are
// silent. Coverage counts three decoded windows and the refused range is kept
// out of the decoded ranges.
func TestNoSpeech_RefusedWindowStaysOutsideCoverage(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	st := newRealStore(t)
	tr := &scriptedSilenceTranscriber{texts: map[int]string{1: ruText, 2: loopText}, lang: "ru"}
	svc := windowedSilenceService(t, root, st, tr, "window", true)
	processWindowed(t, svc, root, "refused.mp3")

	meta := transcriptMetaFor(t, st, "refused.mp3")
	if strings.TrimSpace(meta) == "" {
		t.Fatalf("no transcript persisted; doc = %+v", mustGetDoc(t, st, "refused.mp3"))
	}
	scoped := decodeScopedMeta(t, meta)
	cov := scoped.Coverage
	if cov == nil {
		t.Fatalf("transcript meta carries no coverage: %s", meta)
	}
	if cov.WindowsAttempted != 4 || cov.WindowsDecoded != 3 {
		t.Errorf("coverage = %d of %d windows decoded, want 3 of 4 (the refused window is not decoded)", cov.WindowsDecoded, cov.WindowsAttempted)
	}
	if len(cov.Refused) != 1 || cov.Refused[0].Reason != "quality_gate" {
		t.Fatalf("refused = %+v, want one quality_gate range", cov.Refused)
	}
	ref := cov.Refused[0]
	for _, r := range cov.Ranges {
		if r.StartMS < ref.EndMS && r.EndMS > ref.StartMS {
			t.Errorf("decoded range %+v overlaps the refused range %+v", r, ref)
		}
	}
}
