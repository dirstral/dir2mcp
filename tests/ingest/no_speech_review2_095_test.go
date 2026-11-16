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
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Edge cases of the SPEC 0.95.0 no-speech outcome from the second review of
// #1169.

// cappedSilentTranscriber declares a payload cap, so an oversized window is
// split into pieces. Every request is a no-speech result, except the 1-based
// requests in failAt, which fail at the provider.
type cappedSilentTranscriber struct {
	mu     sync.Mutex
	calls  int
	limit  int
	failAt map[int]error
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
