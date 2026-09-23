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
)

// SPEC §8.2.2 "quality gate per window" (dirstral-spec 0.71.0), dir2mcp #1030.
// Under media.stt.language_scope: window the §8.6.6 degenerate-output checks run
// per decoded window: a failing window is a refused range with reason
// quality_gate and never reaches the transcript, the item stays ok when at
// least one window passed, and a decode whose every window failed is
// TRANSCRIBE_FAILED as a failed document-level gate is. The script check judges
// each window against the language the window itself resolved to.

// loopText is a whisper-style repetition loop: one short phrase repeated until
// it explains far more than half of the window (§8.6.6 repetition_loop).
const loopText = "[00:00] thank you thank you thank you thank you thank you thank you thank you thank you thank you"

// ruText is fluent Russian prose, on-script for a window the decoder reports as
// Russian. The fakes report "ru" for their windows, and the script check judges
// each window against that report, so a window's text must be in its script.
const ruText = "[00:00] сегодня мы говорим о бюджете на следующий год\n[00:30] участники высказали несколько возражений по срокам"

// latinText is fluent Latin-script prose, off-script for a window resolved to
// Russian (§8.6.6 language_mismatch).
const latinText = "[00:00] the committee met on tuesday to discuss the budget for the coming year\n[00:30] members raised several objections about the timeline"

func newGatedHarness(t *testing.T, tr *langWindowTranscriber) (*windowSTTHarness, []byte) {
	t.Helper()
	h, content := newScopedHarness(t, tr, scopeTotalMS)
	h.svc.SetQualityGate(quality.New(quality.DefaultConfig()))
	return h, content
}

// TestWindowQuality_RepetitionLoopRefusesOnlyThatWindow: the third window is a
// repetition loop; it is refused as quality_gate over its core, the other three
// are indexed, and the loop text never reaches the transcript.
//
// Mutants killed: running the gate on the merged text only (the loop is a
// quarter of the transcript and passes); refusing the whole item; counting the
// refused window as decoded.
func TestWindowQuality_RepetitionLoopRefusesOnlyThatWindow(t *testing.T) {
	t.Parallel()
	tr := &langWindowTranscriber{
		def:    langReply{lang: "ru", conf: 0.9, text: ruText},
		script: map[int]langReply{3: {lang: "ru", conf: 0.9, text: loopText}},
	}
	h, content := newGatedHarness(t, tr)

	meta := runScoped(t, h, "talks/loop.m4a", content)
	cov := meta.Coverage
	if cov.WindowsAttempted != 4 || cov.WindowsDecoded != 3 {
		t.Errorf("coverage = %d/%d windows, want 3 decoded of 4", cov.WindowsDecoded, cov.WindowsAttempted)
	}
	// The refused range starts where the second window's decoded audio ends
	// (its 600 s reach into the third window's core), as a language refusal's
	// does, so the decoded and refused ranges stay disjoint.
	refusedStart := win2StartMS + scopeWinMS
	if len(cov.Refused) != 1 || cov.Refused[0].Reason != "quality_gate" || cov.Refused[0].StartMS != refusedStart || cov.Refused[0].EndMS != win4StartMS {
		t.Errorf("refused = %+v, want one quality_gate range [%d,%d)", cov.Refused, refusedStart, win4StartMS)
	}
	for _, r := range cov.Ranges {
		if r.StartMS < win4StartMS && r.EndMS > refusedStart {
			t.Errorf("decoded range %+v overlaps the refused range [%d,%d)", r, refusedStart, win4StartMS)
		}
	}
	if strings.Contains(persistedText(h), "thank you thank you") {
		t.Errorf("the refused window's loop reached the transcript:\n%s", persistedText(h))
	}
	if !strings.Contains(h.logs.String(), "quality gate refused window") || !strings.Contains(h.logs.String(), "repetition_loop") {
		t.Errorf("no content-free log line named the refusal and its reason:\n%s", h.logs.String())
	}
	if len(h.store.reps) != 1 {
		t.Fatalf("the surviving windows must still be persisted as a transcript")
	}
}

// TestWindowQuality_ScriptMismatchUsesTheWindowLanguage: every window is pinned
// to Russian, and the second window's text is Latin-script prose. The script
// check runs against the window's resolved language, so that window is refused
// while the Cyrillic windows around it are kept.
func TestWindowQuality_ScriptMismatchUsesTheWindowLanguage(t *testing.T) {
	t.Parallel()
	tr := &langWindowTranscriber{
		def:    langReply{lang: "ru", conf: 0.9, text: ruText},
		script: map[int]langReply{2: {lang: "ru", conf: 0.9, text: latinText}},
	}
	h, content := newGatedHarness(t, tr)
	h.svc.SetTranscriptLanguage("ru")

	meta := runScoped(t, h, "talks/script.m4a", content)
	cov := meta.Coverage
	if len(cov.Refused) != 1 || cov.Refused[0].Reason != "quality_gate" || cov.Refused[0].StartMS != scopeWinMS || cov.Refused[0].EndMS != win3StartMS {
		t.Errorf("refused = %+v, want the second window refused as quality_gate over [%d,%d), after the first window's decoded reach", cov.Refused, scopeWinMS, win3StartMS)
	}
	if !strings.Contains(h.logs.String(), "language_mismatch") {
		t.Errorf("the refusal must name language_mismatch:\n%s", h.logs.String())
	}
	if strings.Contains(persistedText(h), "committee met") {
		t.Errorf("the off-script window reached the transcript")
	}
}

// TestWindowQuality_AllWindowsRefusedIsTranscribeFailed: every window is a
// loop, so nothing is persisted and the document is status=error with the
// TRANSCRIBE_FAILED code, exactly as a failed document-level gate records it.
func TestWindowQuality_AllWindowsRefusedIsTranscribeFailed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	cfg := config.Config{RootDir: root, StateDir: t.TempDir(), STTProvider: "off"}
	svc := mustNewIngestService(t, cfg, st)
	svc.SetIndexingState(appstate.NewIndexingState(appstate.ModeIncremental))
	svc.SetTranscriber(&langWindowTranscriber{def: langReply{lang: "ru", conf: 0.9, text: loopText}})
	svc.SetSTTIdentity("whisper", "large-v3")
	svc.SetLanguageScope("window")
	svc.SetQualityGate(quality.New(quality.DefaultConfig()))
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) {
		return time.Duration(scopeTotalMS) * time.Millisecond, nil
	}
	svc.ExtractSegmentFunc = func(_ context.Context, _ string, startMS, endMS int) ([]byte, error) {
		return make([]byte, 10*(endMS-startMS)/scopeTotalMS+1), nil
	}

	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("ProcessDocument hard-failed on an all-refused decode: %v", err)
	}
	doc := mustGetDoc(t, st, "talk.mp3")
	if doc.Status != "error" || !strings.Contains(doc.ErrorMessage, "TRANSCRIBE_FAILED") {
		t.Fatalf("talk.mp3: status=%q error=%q, want status=error carrying TRANSCRIBE_FAILED", doc.Status, doc.ErrorMessage)
	}
}

// TestWindowQuality_MixedRefusalsAreALanguageSkip: one window refused for its
// language under skip and the rest by the gate. The language refusal wins the
// terminal status (§8.2.2): it is the operator's decision and recurs every run.
func TestWindowQuality_MixedRefusalsAreALanguageSkip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	cfg := config.Config{RootDir: root, StateDir: t.TempDir(), STTProvider: "off"}
	svc := mustNewIngestService(t, cfg, st)
	svc.SetIndexingState(appstate.NewIndexingState(appstate.ModeIncremental))
	svc.SetTranscriber(&langWindowTranscriber{
		def:    langReply{lang: "ru", conf: 0.9, text: loopText},
		script: map[int]langReply{1: {lang: "uk", conf: 0.9}},
	})
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
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("ProcessDocument hard-failed: %v", err)
	}
	doc := mustGetDoc(t, st, "talk.mp3")
	if doc.Status != "skipped" || doc.SkipReason != model.SkipReasonLanguageUncovered {
		t.Fatalf("talk.mp3: status=%q skip_reason=%q, want skipped/%s", doc.Status, doc.SkipReason, model.SkipReasonLanguageUncovered)
	}
}

// TestWindowQuality_ItemScopeDoesNotRefusePerWindow pins the default: under
// item scope the gate runs on the merged text as before, so a loop that is one
// window of four passes and nothing is refused per window.
func TestWindowQuality_ItemScopeDoesNotRefusePerWindow(t *testing.T) {
	t.Parallel()
	tr := &langWindowTranscriber{
		def:    langReply{lang: "ru", conf: 0.9, text: ruText},
		script: map[int]langReply{3: {lang: "ru", conf: 0.9, text: loopText}},
	}
	h, content := newGatedHarness(t, tr)
	h.svc.SetLanguageScope("item")

	meta := runScoped(t, h, "talks/item-loop.m4a", content)
	if meta.Coverage == nil || meta.Coverage.WindowsDecoded != 4 || len(meta.Coverage.Refused) != 0 {
		t.Errorf("item scope refused per window: %+v", meta.Coverage)
	}
}

// TestWindowQuality_GateOffRefusesNothing pins that with quality gates off the
// per-window gate is off too: a loop window is indexed like any other.
func TestWindowQuality_GateOffRefusesNothing(t *testing.T) {
	t.Parallel()
	tr := &langWindowTranscriber{
		def:    langReply{lang: "ru", conf: 0.9},
		script: map[int]langReply{3: {lang: "ru", conf: 0.9, text: loopText}},
	}
	h, content := newScopedHarness(t, tr, scopeTotalMS)
	h.svc.SetQualityGate(nil)

	meta := runScoped(t, h, "talks/gate-off.m4a", content)
	if meta.Coverage.WindowsDecoded != 4 || len(meta.Coverage.Refused) != 0 {
		t.Errorf("gate off still refused: %+v", meta.Coverage)
	}
}

// TestWindowQuality_MixedTracksAreALanguageSkip applies the same precedence
// across the SELECTED tracks of a multi-track document (§8.6.12): track 0 is
// refused for its language under skip, track 1 is rejected by the per-window
// gate. No provider failed, so the document is a durable language skip, not
// TRANSCRIBE_FAILED; a run that reported the error would retry a decision the
// operator made and hide the reason that recurs.
//
// The decode is sequential (track 0's four windows, then track 1's), so the
// fake's call ordinal tells the tracks apart: calls 1 to 4 report Ukrainian,
// the rest are a repetition loop.
func TestWindowQuality_MixedTracksAreALanguageSkip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "dub.m4a"), "fake-audio")
	st := newRealStore(t)
	cfg := config.Config{RootDir: root, StateDir: t.TempDir(), STTProvider: "off", MediaSTTTracks: []string{"0", "1"}}
	svc := mustNewIngestService(t, cfg, st)
	svc.SetIndexingState(appstate.NewIndexingState(appstate.ModeIncremental))
	svc.SetTranscriber(&langWindowTranscriber{
		def: langReply{lang: "ru", conf: 0.9, text: loopText},
		script: map[int]langReply{
			1: {lang: "uk", conf: 0.9}, 2: {lang: "uk", conf: 0.9}, 3: {lang: "uk", conf: 0.9}, 4: {lang: "uk", conf: 0.9},
		},
	})
	svc.SetSTTIdentity("whisper", "large-v3")
	svc.SetSTTLanguages([]string{"ru"})
	svc.SetOnUncoveredLanguage("skip")
	svc.SetLanguageScope("window")
	svc.SetQualityGate(quality.New(quality.DefaultConfig()))
	svc.ProbeMediaInfoFunc = threeTrackProbe()
	svc.ExtractAudioTrackIndexFunc = func(_ context.Context, _ string, audioIndex int) ([]byte, error) {
		return []byte(trackAudioBytes(audioIndex)), nil
	}
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) {
		return time.Duration(scopeTotalMS) * time.Millisecond, nil
	}
	svc.ExtractSegmentFunc = func(_ context.Context, _ string, startMS, endMS int) ([]byte, error) {
		return make([]byte, 10*(endMS-startMS)/scopeTotalMS+1), nil
	}

	f := ingest.DiscoveredFile{RelPath: "dub.m4a", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("ProcessDocument hard-failed: %v", err)
	}
	doc := mustGetDoc(t, st, "dub.m4a")
	if doc.Status != "skipped" || doc.SkipReason != model.SkipReasonLanguageUncovered {
		t.Fatalf("dub.m4a: status=%q skip_reason=%q error=%q, want skipped/%s", doc.Status, doc.SkipReason, doc.ErrorMessage, model.SkipReasonLanguageUncovered)
	}
}

// TestWindowQuality_ACustomGateIsHonouredPerWindow: a caller whose own gate
// disables the repetition detector must not see a window refused for
// repetition. The per-window gate is derived from the installed gate.
func TestWindowQuality_ACustomGateIsHonouredPerWindow(t *testing.T) {
	t.Parallel()
	tr := &langWindowTranscriber{
		def: langReply{lang: "ru", conf: 0.9, text: ruText},
		// Cyrillic, so the script check passes and repetition is the only
		// detector that could refuse it.
		script: map[int]langReply{3: {lang: "ru", conf: 0.9, text: "[00:00] спасибо спасибо спасибо спасибо спасибо спасибо спасибо спасибо спасибо"}},
	}
	h, content := newScopedHarness(t, tr, scopeTotalMS)
	cfg := quality.DefaultConfig()
	cfg.Repetition.Enabled = false
	h.svc.SetQualityGate(quality.New(cfg))

	meta := runScoped(t, h, "talks/custom.m4a", content)
	if len(meta.Coverage.Refused) != 0 {
		t.Errorf("refused = %+v, want none: the installed gate has repetition off", meta.Coverage.Refused)
	}
}

// TestWindowQuality_PassingWindowsDoNotFailTheMergedTranscript: each window is
// too short for the repetition detector (it needs 24 runes) and passes, but
// the merged transcript is a long loop. The document-level gate under window
// scope keeps only empty and density, so the recording is indexed, not failed
// through checks its windows already passed.
func TestWindowQuality_PassingWindowsDoNotFailTheMergedTranscript(t *testing.T) {
	t.Parallel()
	tr := &langWindowTranscriber{def: langReply{lang: "en", conf: 0.9, text: "[00:00] ok ok ok ok ok"}}
	h, content := newScopedHarness(t, tr, scopeTotalMS)
	cfg := quality.DefaultConfig()
	cfg.Density.Enabled = false // isolate the detectors the windows already ran
	h.svc.SetQualityGate(quality.New(cfg))

	meta := runScoped(t, h, "talks/short-loops.m4a", content)
	if len(meta.Coverage.Refused) != 0 || meta.Coverage.WindowsDecoded != 4 {
		t.Fatalf("coverage = %+v, want every window decoded and none refused", meta.Coverage)
	}
	if strings.Contains(h.logs.String(), "quality gate quarantined transcript") {
		t.Errorf("the merged transcript was quarantined by a detector its windows already passed:\n%s", h.logs.String())
	}
}

// TestWindowQuality_ARefusedTrackRetiresItsOldTranscript: a two-track document
// whose second track's windows are all refused by the gate on a later run must
// not keep that track's earlier transcript searchable.
func TestWindowQuality_ARefusedTrackRetiresItsOldTranscript(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	body := "fake-audio"
	writeFile(t, filepath.Join(root, "dub.m4a"), body)
	st := newRealStore(t)
	cfg := config.Config{RootDir: root, StateDir: t.TempDir(), STTProvider: "off", MediaSTTTracks: []string{"0", "1"}}
	svc := mustNewIngestService(t, cfg, st)
	svc.SetIndexingState(appstate.NewIndexingState(appstate.ModeIncremental))
	tr := &langWindowTranscriber{def: langReply{lang: "ru", conf: 0.9, text: ruText}}
	svc.SetTranscriber(tr)
	svc.SetSTTIdentity("whisper", "large-v3")
	svc.SetLanguageScope("window")
	svc.SetQualityGate(quality.New(quality.DefaultConfig()))
	svc.ProbeMediaInfoFunc = threeTrackProbe()
	track1 := "track-1-audio-run-1"
	svc.ExtractAudioTrackIndexFunc = func(_ context.Context, _ string, audioIndex int) ([]byte, error) {
		if audioIndex == 1 {
			return []byte(track1), nil
		}
		return []byte(trackAudioBytes(audioIndex)), nil
	}
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) {
		return time.Duration(scopeTotalMS) * time.Millisecond, nil
	}
	svc.ExtractSegmentFunc = func(_ context.Context, _ string, startMS, endMS int) ([]byte, error) {
		return make([]byte, 10*(endMS-startMS)/scopeTotalMS+1), nil
	}
	f := ingest.DiscoveredFile{RelPath: "dub.m4a", SizeBytes: int64(len(body)), MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if types := repTypesFor(t, st, "dub.m4a"); !types["transcript"] || !types["transcript@t1"] {
		t.Fatalf("run 1 must produce both tracks, got %v", types)
	}

	// Run 2: track 1's audio changed and now decodes to a loop in every window;
	// track 0 is unchanged.
	track1 = "track-1-audio-run-2"
	calls := tr.callCount()
	tr.mu.Lock()
	tr.script = map[int]langReply{}
	for i := calls + 1; i <= calls+16; i++ {
		tr.script[i] = langReply{lang: "ru", conf: 0.9, text: loopText}
	}
	tr.mu.Unlock()
	if err := svc.ProcessDocument(ctx, f, nil, true); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	types := repTypesFor(t, st, "dub.m4a")
	if types["transcript@t1"] {
		t.Errorf("the refused track's old transcript is still live: %v", types)
	}
}
