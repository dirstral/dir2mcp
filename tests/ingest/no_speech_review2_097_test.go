package tests

import (
	"context"
	"errors"
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
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Edge cases of the SPEC 0.97.0 no-speech outcome from the second review of
// #1169.

// cappedSilentTranscriber declares a payload cap, so an oversized window is
// split into pieces. Every request is a no-speech result, except the 1-based
// requests in failAt, which fail at the provider.
type cappedSilentTranscriber struct {
	mu     sync.Mutex
	calls  int
	limit  int
	failAt map[int]error
	// replies answers the 1-based requests it names with speech.
	replies map[int]model.TranscriptResult
}

func (c *cappedSilentTranscriber) Transcribe(ctx context.Context, relPath string, data []byte) (string, error) {
	res, err := c.TranscribeStructured(ctx, relPath, data)
	return res.Text, err
}

func (c *cappedSilentTranscriber) TranscribeStructured(context.Context, string, []byte) (model.TranscriptResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if err := c.failAt[c.calls]; err != nil {
		return model.TranscriptResult{}, err
	}
	if res, ok := c.replies[c.calls]; ok {
		return res, nil
	}
	return model.TranscriptResult{}, nil
}

func (c *cappedSilentTranscriber) MaxTranscribePayloadBytes() int { return c.limit }

// TestNoSpeech_FailedPieceBesideSilentPiecesIsNotNoSpeech: each window is over
// the provider cap and is split into pieces. One piece fails and every other
// piece of the recording holds no speech. The failed piece can hold speech, so
// the recording is a failure that is retried, not a no-speech recording. With
// no failed piece the same recording is a no-speech recording.
//
// Mutant killed: counting a split window as decoded when one of its pieces has
// no result, which settles the recording as no_speech.
func TestNoSpeech_FailedPieceBesideSilentPiecesIsNotNoSpeech(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{"item", "window"} {
		for _, failed := range []bool{true, false} {
			scope, failed := scope, failed
			name := scope + "/all pieces silent"
			if failed {
				name = scope + "/one piece failed"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				const totalMS = 30 * 60 * 1000
				content := make([]byte, 300_000)
				tr := &cappedSilentTranscriber{limit: 200_000}
				if failed {
					tr.failAt = map[int]error{2: windowProviderFailure}
				}
				h := newWindowSTTHarness(t, &windowRecordingTranscriber{}, totalMS, content)
				h.oversize = 3 // every window comes out over the cap and is split
				h.svc.SetTranscriber(tr)
				h.svc.SetSTTIdentity("whisper", "large-v3")
				h.svc.SetLanguageScope(scope)

				err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/music.m4a"), content)
				if tr.calls <= 4 {
					t.Fatalf("decode requests = %d, want more than 4: the windows must be split into pieces", tr.calls)
				}
				settled := strings.Contains(h.logs.String(), "so no transcript is produced")
				if !failed {
					if err != nil || !settled {
						t.Fatalf("all pieces silent: err=%v, no-speech settled=%v, want no error and a no-speech recording\n%s", err, settled, h.logs.String())
					}
					return
				}
				if settled {
					t.Fatalf("a recording with a failed piece was settled as no speech:\n%s", h.logs.String())
				}
				if !errors.Is(err, ingest.ErrTranscriptProviderFailure) {
					t.Fatalf("err = %v, want a transcript provider failure (the failed piece can hold speech)", err)
				}
			})
		}
	}
}

// settingFailingStore is a real SQLite store whose no-speech record write can
// be made to fail.
type settingFailingStore struct {
	*store.SQLiteStore
	fail atomic.Bool
}

func (s *settingFailingStore) SetSetting(ctx context.Context, key, value string) error {
	if s.fail.Load() && strings.HasPrefix(key, "no_speech_identity:") {
		return errors.New("store refused the setting")
	}
	return s.SQLiteStore.SetSetting(ctx, key, value)
}

// TestNoSpeech_IdentityRecordWriteFailureIsAnError: the record of the STT
// identity that found no speech is what makes a later model change decode the
// recording again. When the store refuses that write, the document must not be
// settled as a no_speech skip with no record: it is an error, and the next run
// (which reads the cached decode) settles it once the write works.
//
// Mutant killed: logging the failed write, which leaves a skip that no later
// identity change can reach.
func TestNoSpeech_IdentityRecordWriteFailureIsAnError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "music.mp3"), "fake-audio")
	st := &settingFailingStore{SQLiteStore: newRealStore(t)}
	tr := &speechWindowTranscriber{silent: true}
	svc, _, _ := noSpeechService(t, root, testutil.TempDir(t), st, tr, 0)
	f := ingest.DiscoveredFile{RelPath: "music.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}

	st.fail.Store(true)
	_ = svc.ProcessDocument(ctx, f, nil, false)
	doc := mustGetDoc(t, st.SQLiteStore, "music.mp3")
	if doc.Status != "error" {
		t.Fatalf("status=%q skip_reason=%q, want error: a no_speech skip with no identity record is never decoded again after a model change", doc.Status, doc.SkipReason)
	}
	if _, ok := noSpeechRecord(t, st.SQLiteStore, "music.mp3"); ok {
		t.Fatalf("a record exists after a refused write; the fixture is wrong")
	}

	st.fail.Store(false)
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("retry run: %v", err)
	}
	doc = mustGetDoc(t, st.SQLiteStore, "music.mp3")
	if doc.Status != "skipped" || doc.SkipReason != model.SkipReasonNoSpeech {
		t.Fatalf("after the retry: status=%q skip_reason=%q error=%q, want skipped/no_speech", doc.Status, doc.SkipReason, doc.ErrorMessage)
	}
	if _, ok := noSpeechRecord(t, st.SQLiteStore, "music.mp3"); !ok {
		t.Error("the retry settled no_speech with no identity record")
	}
	if n := tr.count(); n != 1 {
		t.Errorf("decode requests = %d, want 1: the retry reads the cached decode", n)
	}
}

// byAudioTranscriber answers by the audio bytes: text for a known track, a
// no-speech result for every other track.
type byAudioTranscriber struct {
	text map[string]string
}

func (b *byAudioTranscriber) Transcribe(_ context.Context, _ string, data []byte) (string, error) {
	return b.text[string(data)], nil
}

// TestNoSpeech_DerivationPassSkipsANoSpeechTrack: under the two-phase batch
// mode the derivation pass reads each selected track's transcript to translate
// it. Track 0 holds speech and track 1 holds none. The no-speech track has
// nothing to translate: the pass must not count it as a translation failure.
//
// Mutant killed: returning the no-speech sentinel from the derivation read as
// an error, which marks the asset errored with a translation failure code.
func TestNoSpeech_DerivationPassSkipsANoSpeechTrack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "dub.m4a"), "fake-audio")
	st := newRealStore(t)
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = testutil.TempDir(t)
	cfg.STTProvider = "off"
	cfg.MediaSTTTracks = []string{"0", "1"}
	cfg.MediaBatchTwoPhase = true
	cfg.MediaTranslateEnabled = true
	cfg.MediaTranslateTargetLangs = []string{"en"}
	svc := mustNewIngestService(t, cfg, st)
	state := appstate.NewIndexingState(appstate.ModeIncremental)
	svc.SetIndexingState(state)
	svc.SetTranscriber(&byAudioTranscriber{text: map[string]string{
		// Track 0 is decoded from the file bytes as they are.
		"fake-audio": "[00:00] erste Zeile\n[00:03] zweite Zeile",
	}})
	svc.SetSTTIdentity("whisper", "large-v3")
	svc.SetTranscriptLanguage("de")
	svc.SetTranslator(&fakeTranslator{}, "mistral", "mistral-small-2506", []string{"en"})
	svc.ProbeMediaInfoFunc = threeTrackProbe()
	svc.ExtractAudioTrackIndexFunc = func(_ context.Context, _ string, audioIndex int) ([]byte, error) {
		return []byte(trackAudioBytes(audioIndex)), nil
	}
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) {
		return 0, errors.New("no duration in this test")
	}

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if doc := mustGetDoc(t, st, "dub.m4a"); doc.Status != "ok" {
		t.Fatalf("status=%q error=%q, want ok (track 0 holds speech)", doc.Status, doc.ErrorMessage)
	}
	types := repTypesFor(t, st, "dub.m4a")
	if !types["transcript"] || !types["transcript-en"] {
		t.Fatalf("representations %v: want the track 0 transcript and its translation", types)
	}
	if types["transcript@t1"] {
		t.Errorf("representations %v: a no-speech track must have no transcript", types)
	}
	if snap := state.Snapshot(); snap.Errors != 0 {
		t.Errorf("run errors = %d, want 0: a no-speech track is not a translation failure", snap.Errors)
	}
}

// TestNoSpeech_BracketedTextIsNotATimestampMarker: the merge drops a window
// with no text, and a line that is only a timestamp marker is no text. A
// bracketed line that is not a timestamp ("[hello]", "[music]") is text, as
// the segment parser reads it, so the merge keeps that window.
//
// Mutant killed: removing every leading bracketed token before the text test,
// which drops the window and returns an empty transcript.
func TestNoSpeech_BracketedTextIsNotATimestampMarker(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, text string
		wantText   bool
	}{
		{"bracketed word", "[hello]", true},
		{"bracketed word after a marker", "[00:05] [music]", true},
		{"bracketed sound tag", "[music]", true},
		{"timestamp marker only", "[00:10]", false},
		{"sub-second marker only", "[00:10.500]", false},
		{"blank", "  \n ", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, _ := ingest.MergeTranscriptWindows([]ingest.TranscriptWindow{{StartMS: 0, Res: model.TranscriptResult{Text: tc.text}}}, 590_000)
			if got := strings.TrimSpace(text) != ""; got != tc.wantText {
				t.Fatalf("merged text of %q = %q, want text=%v", tc.text, text, tc.wantText)
			}
		})
	}
}

// TestNoSpeech_FailedPieceOfASingleWindowIsNotNoSpeech: a short recording over
// the provider cap is one scheduled window, and the cap splits it into two
// pieces. One piece fails and the other holds no speech. A decode of one
// window records no coverage, so the failed count must not be lost: the
// recording is a failure that is retried, not a no-speech recording. With no
// failed piece it is a no-speech recording.
//
// Mutant killed: reading the empty text of a one-window decode with no
// coverage record as a successful no-speech decode.
func TestNoSpeech_FailedPieceOfASingleWindowIsNotNoSpeech(t *testing.T) {
	t.Parallel()
	for _, failed := range []bool{true, false} {
		failed := failed
		name := "both pieces silent"
		if failed {
			name = "one piece failed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			const totalMS = 20 * 1000
			content := make([]byte, 300_000)
			tr := &cappedSilentTranscriber{limit: 200_000}
			if failed {
				tr.failAt = map[int]error{1: windowProviderFailure}
			}
			h := newWindowSTTHarness(t, &windowRecordingTranscriber{}, totalMS, content)
			h.svc.SetTranscriber(tr)
			h.svc.SetSTTIdentity("whisper", "large-v3")

			err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/jingle.m4a"), content)
			if !strings.Contains(h.logs.String(), "/1 windows decoded") || tr.calls != 2 {
				t.Fatalf("want one scheduled window sent as two pieces, got %d request(s):\n%s", tr.calls, h.logs.String())
			}
			settled := strings.Contains(h.logs.String(), "so no transcript is produced")
			if !failed {
				if err != nil || !settled {
					t.Fatalf("both pieces silent: err=%v, no-speech settled=%v, want a no-speech recording\n%s", err, settled, h.logs.String())
				}
				return
			}
			if settled {
				t.Fatalf("a recording with a failed piece was settled as no speech:\n%s", h.logs.String())
			}
			if !errors.Is(err, ingest.ErrTranscriptProviderFailure) {
				t.Fatalf("err = %v, want a transcript provider failure", err)
			}
		})
	}
}

// TestNoSpeech_SilentCandidateDoesNotDependOnCandidateOrder: a route candidate
// that does not declare the window's language can stay in the candidate list.
// Window 1: the first candidate decodes a loop that the gate refuses,
// and the next candidate finds no speech. A no-speech result reports no
// language, so it is not judged against that candidate's coverage: the window
// is a no-speech window whether the silent candidate is the last one or a
// failing candidate follows it. The failing candidate is not asked.
//
// Mutant killed: judging the silent decode as language_uncovered, which walks
// on to the failing candidate and turns the window into a failed window.
func TestNoSpeech_SilentCandidateDoesNotDependOnCandidateOrder(t *testing.T) {
	t.Parallel()
	for _, trailing := range []bool{false, true} {
		trailing := trailing
		name := "silent candidate is last"
		if trailing {
			name = "a failing candidate follows"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			def := &langWindowTranscriber{def: langReply{lang: "ru", conf: 0.9, text: ruText}}
			h, content := newScopedHarness(t, def, scopeTotalMS)
			h.svc.SetQualityGate(quality.New(quality.DefaultConfig()))
			h.svc.SetOnUncoveredLanguage("skip")
			h.svc.AddRouteCandidate("ru", &langWindowTranscriber{def: langReply{lang: "ru", conf: 0.9, text: loopText}}, "ru-first", []string{"ru"})
			silent := &replyTranscriber{}
			h.svc.AddRouteCandidate("ru", silent, "ru-silent", []string{"de"})
			failing := &replyTranscriber{err: windowProviderFailure}
			if trailing {
				h.svc.AddRouteCandidate("ru", failing, "ru-broken", []string{"ru"})
			}

			_ = h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/music.m4a"), content)
			out := h.logs.String()
			if !strings.Contains(out, "window [0,590000]ms of talks/music.m4a holds no speech; it counts as decoded") {
				t.Errorf("window 1 must be a no-speech window:\n%s", out)
			}
			if strings.Contains(out, "refusing window [0,") {
				t.Errorf("a no-speech result must not be refused for a language it does not report:\n%s", out)
			}
			if silent.calls == 0 {
				t.Fatalf("the silent candidate was not asked; the fixture is wrong:\n%s", out)
			}
			if failing.calls != 0 {
				t.Errorf("the failing candidate was asked %d time(s) after a complete no-speech decode", failing.calls)
			}
		})
	}
}

// TestNoSpeech_RetiresTheSummaryOfTheEarlierTranscript pins SPEC 0.97.0
// §8.6.13: when a no-speech result retires an earlier transcript, the
// summaries derived from that transcript go with it. Scan 1 indexes a
// transcript and its document summary. In scan 2 another STT model finds no
// speech in the same bytes: the transcript and the summary are retired, their
// chunks are tombstoned, and the lexical index returns no hit for either.
//
// Mutant killed: retiring only the transcript and its translations, which
// leaves a summary that answers from text the document no longer has.
func TestNoSpeech_RetiresTheSummaryOfTheEarlierTranscript(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)

	scan := func(sttModel, transcript string) {
		t.Helper()
		cfg := hierarchicalConfig(stateDir)
		cfg.RootDir = root
		cfg.STTProvider = "off"
		svc := mustNewIngestService(t, cfg, st)
		svc.SetIndexingState(appstate.NewIndexingState(appstate.ModeIncremental))
		svc.SetTranscriber(&fakeTranscriber{text: transcript})
		svc.SetSTTIdentity("whisper", sttModel)
		svc.SetTranscriptLanguage("en")
		svc.SetSummarizer(&fakeSummarizer{out: "summarywordzq of the talk"}, "mistral", "mistral-small-latest")
		if err := svc.Run(ctx); err != nil {
			t.Fatalf("scan (%s): %v", sttModel, err)
		}
	}

	scan("large-v3", "[00:00] the budget talkwordzq is here\n[00:03] second line of the talk")
	types := activeRepTypes(t, st, "talk.mp3")
	if !types[ingest.RepTypeTranscript] || !types[model.SummaryRepType] {
		t.Fatalf("scan 1 must index a transcript and its summary, got %v", types)
	}
	transcriptID := repIDForType(t, st, "talk.mp3", ingest.RepTypeTranscript)
	summaryID := repIDForType(t, st, "talk.mp3", model.SummaryRepType)
	for _, word := range []string{"talkwordzq", "summarywordzq"} {
		if hits, _ := st.SearchBM25(ctx, word, 5, ""); len(hits) == 0 {
			t.Fatalf("%q gives no lexical hit after scan 1; the fixture is wrong", word)
		}
	}

	scan("large-v4", "")
	doc := mustGetDoc(t, st, "talk.mp3")
	if doc.Status != "skipped" || doc.SkipReason != model.SkipReasonNoSpeech {
		t.Fatalf("scan 2: status=%q skip_reason=%q error=%q, want skipped/no_speech", doc.Status, doc.SkipReason, doc.ErrorMessage)
	}
	if types := activeRepTypes(t, st, "talk.mp3"); len(types) != 0 {
		t.Errorf("live representations after the no-speech result = %v, want none", types)
	}
	assertChunksTombstoned(t, st, transcriptID)
	assertChunksTombstoned(t, st, summaryID)
	for _, word := range []string{"talkwordzq", "summarywordzq"} {
		if hits, _ := st.SearchBM25(ctx, word, 5, ""); len(hits) != 0 {
			t.Errorf("%q still gives %d lexical hit(s) after the no-speech result", word, len(hits))
		}
	}
}

// TestNoSpeech_WindowDoesNotChangeTheContinuityState pins SPEC 0.97.0 §8.6.13
// under window scope. Window 1 resolves Kyrgyz and moves to the Kyrgyz route.
// Window 2 is a no-speech result: it has no language and does not change the
// continuity state. Window 3 has speech with no confident language: it
// inherits Kyrgyz, the last language resolved before the no-speech window,
// and stays on the Kyrgyz route.
//
// Mutant killed: advancing the continuity state on a no-speech window, which
// leaves window 3 with no language to inherit and sends it to the default
// profile.
func TestNoSpeech_WindowDoesNotChangeTheContinuityState(t *testing.T) {
	t.Parallel()
	def := &langWindowTranscriber{def: langReply{lang: "ky", conf: 0.9}}
	h, content := newScopedHarness(t, def, scopeTotalMS)
	route := &replyTranscriber{
		def: model.TranscriptResult{Text: ruText, Language: "ky", LanguageConfidence: 0.9},
		replies: map[int]model.TranscriptResult{
			2: {},
			3: {Text: ruText, Language: "tr", LanguageConfidence: 0.1},
		},
	}
	h.svc.SetRouteTranscriber("ky", route, "ky-ckpt", nil)

	meta := runScoped(t, h, "talks/mixed.m4a", content)
	if def.callCount() != 1 {
		t.Errorf("default profile calls = %d, want 1: windows 2 to 4 must start on the carried Kyrgyz route", def.callCount())
	}
	if meta.Coverage == nil {
		t.Fatalf("no coverage record:\n%s", h.logs.String())
	}
	var silent, inherited bool
	for _, e := range meta.Coverage.Languages {
		if e.Route != "ky-ckpt" {
			t.Errorf("entry [%d,%d] is on route %q, want ky-ckpt", e.StartMS, e.EndMS, e.Route)
		}
		switch {
		case e.Language == "":
			silent = true
		case e.Language == "ky" && e.LanguageSource == "inherited":
			inherited = true
		case e.Language != "ky":
			t.Errorf("entry [%d,%d] has language %q, want ky or none", e.StartMS, e.EndMS, e.Language)
		}
	}
	if !silent {
		t.Errorf("the no-speech window must have an entry with no language: %+v", meta.Coverage.Languages)
	}
	if !inherited {
		t.Errorf("the window after the no-speech window must inherit ky: %+v", meta.Coverage.Languages)
	}
}

// TestNoSpeech_PinnedWindowKeepsThePinnedLanguage: the no-speech decode adds
// no language, but a pin resolves the language of every window (§8.2.2). So a
// no-speech window of a pinned recording records the pinned language, with
// the source "configured", and the coverage record holds no entry without a
// language.
//
// Mutant killed: recording the no-speech window of a pinned recording with no
// language.
func TestNoSpeech_PinnedWindowKeepsThePinnedLanguage(t *testing.T) {
	t.Parallel()
	def := &langWindowTranscriber{def: langReply{lang: "ky", conf: 0.9}}
	h, content := newScopedHarness(t, def, scopeTotalMS)
	tr := &replyTranscriber{
		def:     model.TranscriptResult{Text: ruText, Language: "ky", LanguageConfidence: 0.9},
		replies: map[int]model.TranscriptResult{2: {}},
	}
	h.svc.SetTranscriber(tr)
	h.svc.SetTranscriptLanguage("ky")

	meta := runScoped(t, h, "talks/pinned.m4a", content)
	if !strings.Contains(h.logs.String(), "holds no speech; it counts as decoded") {
		t.Fatalf("window 2 must be a no-speech window; the fixture is wrong:\n%s", h.logs.String())
	}
	if meta.Coverage == nil || len(meta.Coverage.Languages) == 0 {
		t.Fatalf("no coverage.languages record:\n%s", h.logs.String())
	}
	for _, e := range meta.Coverage.Languages {
		if e.Language != "ky" || e.LanguageSource != "configured" {
			t.Errorf("entry [%d,%d] = language %q source %q, want ky/configured for every window of a pinned recording", e.StartMS, e.EndMS, e.Language, e.LanguageSource)
		}
	}
}

// TestNoSpeech_RefusedWindowWithAFailedPieceCountsAsFailed: window 1 is over
// the provider cap and is sent as two pieces. The first piece fails at the
// provider. The second decodes in a language that the profile does not
// declare, so the window is refused for its language. Every other window
// holds no speech. The failed piece can hold speech in a covered language, so
// the recording is a failure that is retried, not the language_uncovered skip.
// With no failed piece the same recording is refused for its language and is
// not a failure.
//
// Mutant killed: dropping the failed piece of a refused window from the
// failed count.
func TestNoSpeech_RefusedWindowWithAFailedPieceCountsAsFailed(t *testing.T) {
	t.Parallel()
	for _, failed := range []bool{true, false} {
		failed := failed
		name := "no failed piece"
		if failed {
			name = "one piece failed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			const totalMS = 30 * 60 * 1000
			content := make([]byte, 300_000)
			uncovered := model.TranscriptResult{
				Text:     "[00:00] opening line spoken at some length\n[00:30] closing line spoken at some length",
				Language: "uk", LanguageConfidence: 0.9,
			}
			tr := &cappedSilentTranscriber{limit: 200_000, replies: map[int]model.TranscriptResult{2: uncovered}}
			if failed {
				tr.failAt = map[int]error{1: windowProviderFailure}
			} else {
				tr.replies[1] = uncovered
			}
			h := newWindowSTTHarness(t, &windowRecordingTranscriber{}, totalMS, content)
			h.oversize = 3 // every window comes out over the cap and is split
			h.svc.SetTranscriber(tr)
			h.svc.SetSTTIdentity("whisper", "large-v3")
			h.svc.SetSTTLanguages([]string{"ru"})
			h.svc.SetOnUncoveredLanguage("skip")
			h.svc.SetLanguageScope("window")

			err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/mixed.m4a"), content)
			if !strings.Contains(h.logs.String(), "refusing window [0,") {
				t.Fatalf("window 1 must be refused for its language; the fixture is wrong:\n%s", h.logs.String())
			}
			if failed {
				if !errors.Is(err, ingest.ErrTranscriptProviderFailure) {
					t.Fatalf("err = %v, want a transcript provider failure: the failed piece of the refused window can hold speech\n%s", err, h.logs.String())
				}
				return
			}
			if errors.Is(err, ingest.ErrTranscriptProviderFailure) {
				t.Fatalf("with no failed piece the recording is refused, not failed: %v", err)
			}
		})
	}
}

// silentWindowTranslator is a structured transcriber for the whisper translate
// path. It fails the 1-based requests in failAt and answers every other
// request with no speech.
type silentWindowTranslator struct {
	mu     sync.Mutex
	calls  int
	failAt map[int]error
}

func (f *silentWindowTranslator) Transcribe(ctx context.Context, relPath string, data []byte) (string, error) {
	res, err := f.TranscribeStructured(ctx, relPath, data)
	return res.Text, err
}

func (f *silentWindowTranslator) TranscribeStructured(context.Context, string, []byte) (model.TranscriptResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if err := f.failAt[f.calls]; err != nil {
		return model.TranscriptResult{}, err
	}
	return model.TranscriptResult{}, nil
}

// TestNoSpeech_TranslateWithAFailedWindowIsNotCachedAsEmpty: the whisper
// translate pass decodes the source audio in windows. One window fails and
// every other window holds no speech, so the translate decode has no text.
// That is a failure, not an empty translation: it must not be cached, and the
// next run must decode again. With no failed window the empty translation is
// a success, and the next run reads it from the cache.
//
// Mutant killed: returning the empty text of a translate decode with a failed
// window as a success, which caches it and stops every later retry.
func TestNoSpeech_TranslateWithAFailedWindowIsNotCachedAsEmpty(t *testing.T) {
	t.Parallel()
	for _, failed := range []bool{true, false} {
		failed := failed
		name := "every window silent"
		if failed {
			name = "one window failed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stateDir := testutil.TempDir(t)
			content := []byte("audio-bytes")
			run := func() *silentWindowTranslator {
				tr := &silentWindowTranslator{}
				if failed {
					tr.failAt = map[int]error{1: windowProviderFailure}
				}
				svc := mustNewIngestService(t, config.Config{StateDir: stateDir, MediaTranslateWhisperWindowSec: 600}, &fakeIngestStore{})
				svc.SetTranscriber(&fakeTranscriber{text: "[00:00] привет\n[00:02] пока"})
				svc.SetTranscriptLanguage("ru")
				svc.SetTranslateTranscriber(tr, "whisper", "large-v3", []string{"en"})
				svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 30 * time.Minute, nil }
				svc.ExtractSegmentFunc = func(context.Context, string, int, int) ([]byte, error) { return []byte("slice"), nil }
				doc := model.Document{DocID: 3, RelPath: "audio/clip.mp3", DocType: "audio"}
				if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, content); err != nil {
					t.Fatalf("GenerateTranscriptRepresentation: %v", err)
				}
				return tr
			}

			first := run()
			if first.calls < 2 {
				t.Fatalf("translate requests on the first run = %d, want a windowed decode; the fixture is wrong", first.calls)
			}
			second := run()
			if failed && second.calls == 0 {
				t.Fatalf("the second run sent no translate request: the failed decode was cached as an empty translation")
			}
			if !failed && second.calls != 0 {
				t.Fatalf("the second run sent %d translate request(s), want 0: an empty translation with no failed window is cached", second.calls)
			}
		})
	}
}

// TestNoSpeech_SettleOutsideScanLeavesTheScanState: the MCP transcribe tool
// decodes outside a scan. When its decode finds no speech it calls
// SettleNoSpeechOutsideScan, which must leave what a scan leaves: the earlier
// transcript and the summary derived from it are retired, the no-speech
// record holds the STT identity, and a document with no other representation
// is skipped as no_speech and keeps its content_hash.
func TestNoSpeech_SettleOutsideScanLeavesTheScanState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	cfg := hierarchicalConfig(testutil.TempDir(t))
	cfg.RootDir = root
	cfg.STTProvider = "off"
	svc := mustNewIngestService(t, cfg, st)
	svc.SetIndexingState(appstate.NewIndexingState(appstate.ModeIncremental))
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] the budget talkwordzq is here\n[00:03] second line of the talk"})
	svc.SetSTTIdentity("whisper", "large-v3")
	svc.SetTranscriptLanguage("en")
	svc.SetSummarizer(&fakeSummarizer{out: "summarywordzq of the talk"}, "mistral", "mistral-small-latest")
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if types := activeRepTypes(t, st, "talk.mp3"); !types[ingest.RepTypeTranscript] || !types[model.SummaryRepType] {
		t.Fatalf("the scan must index a transcript and its summary, got %v", types)
	}
	before := mustGetDoc(t, st, "talk.mp3")

	if err := svc.SettleNoSpeechOutsideScan(ctx, before); err != nil {
		t.Fatalf("SettleNoSpeechOutsideScan: %v", err)
	}
	if types := activeRepTypes(t, st, "talk.mp3"); len(types) != 0 {
		t.Errorf("live representations = %v, want none: the transcript and its summary are retired", types)
	}
	for _, word := range []string{"talkwordzq", "summarywordzq"} {
		if hits, _ := st.SearchBM25(ctx, word, 5, ""); len(hits) != 0 {
			t.Errorf("%q still gives %d lexical hit(s)", word, len(hits))
		}
	}
	if _, ok := noSpeechRecord(t, st, "talk.mp3"); !ok {
		t.Error("no no-speech record was stored")
	}
	doc := mustGetDoc(t, st, "talk.mp3")
	if doc.Status != "skipped" || doc.SkipReason != model.SkipReasonNoSpeech {
		t.Errorf("status=%q skip_reason=%q, want skipped/no_speech", doc.Status, doc.SkipReason)
	}
	if doc.ContentHash == "" || doc.ContentHash != before.ContentHash {
		t.Errorf("content_hash = %q, was %q: the row must keep it so the next scan does not decode again", doc.ContentHash, before.ContentHash)
	}
}
