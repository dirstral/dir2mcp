package tests

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
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

// SPEC 0.99.0 §8.6.13: the lifecycle of a no-speech outcome. A document that
// keeps another searchable representation stays ok, and the per-document
// record of the STT identity that found no speech never outlives the
// derivation or the document it describes.

// noSpeechRecord reads relPath's no-speech identity record. present is false
// when the store holds no row for it.
func noSpeechRecord(t *testing.T, st *store.SQLiteStore, relPath string) (string, bool) {
	t.Helper()
	value, err := st.GetSetting(context.Background(), store.NoSpeechIdentitySettingKey(relPath))
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	return value, true
}

// assertRecognitionWithoutTranscript checks that relPath holds a recognition
// representation and no transcript representation.
func assertRecognitionWithoutTranscript(t *testing.T, st *store.SQLiteStore, relPath string) {
	t.Helper()
	repTypes, err := st.RepresentationTypesByPath(context.Background(), relPath)
	if err != nil {
		t.Fatalf("RepresentationTypesByPath: %v", err)
	}
	var recognized, transcript bool
	for _, rt := range repTypes {
		recognized = recognized || rt == ingest.RepTypeRecognition
		transcript = transcript || rt == ingest.RepTypeTranscript
	}
	if !recognized || transcript {
		t.Fatalf("representations %v: want the recognition one and no transcript", repTypes)
	}
}

// TestNoSpeech_RecognitionKeepsTheDocumentOK: a video whose audio holds no
// speech but whose recognition backend produced annotations keeps a searchable
// representation, so it stays status=ok and is not a no_speech skip.
func TestNoSpeech_RecognitionKeepsTheDocumentOK(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "match.mp4"), "fake-video-bytes")
	st := newRealStore(t)
	client, calls := noSpeechWhisperServer(t)

	cfg := config.Config{RootDir: root, StateDir: testutil.TempDir(t), STTProvider: "off", RecognizeProvider: "serve"}
	svc := mustNewIngestService(t, cfg, st)
	state := appstate.NewIndexingState(appstate.ModeIncremental)
	svc.SetIndexingState(state)
	logs := &syncBuffer{}
	svc.SetLogger(log.New(logs, "", 0))
	svc.SetTranscriber(client)
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 0, errors.New("no duration") }
	svc.ExtractAudioTrackFunc = func(context.Context, string) ([]byte, error) { return []byte("audio-track"), nil }
	rec := &fakeRecognizer{result: recognizeTestResult()}
	svc.SetRecognizer(rec)

	f := ingest.DiscoveredFile{RelPath: "match.mp4", SizeBytes: 16, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	if n := atomic.LoadInt32(calls); n != 1 || rec.calls != 1 {
		t.Fatalf("STT requests = %d, recognition calls = %d, want 1 and 1", n, rec.calls)
	}
	doc := mustGetDoc(t, st, "match.mp4")
	if doc.Status != "ok" || doc.SkipReason != "" {
		t.Fatalf("status=%q skip_reason=%q, want ok: recognition keeps the video searchable\n%s", doc.Status, doc.SkipReason, logs.String())
	}
	assertRecognitionWithoutTranscript(t, st, "match.mp4")
	stats, err := st.CorpusStats(ctx)
	if err != nil {
		t.Fatalf("CorpusStats: %v", err)
	}
	if stats.SkipSummary != nil && stats.SkipSummary.Categories[model.SkipReasonNoSpeech] != 0 {
		t.Errorf("skip_summary counts a no_speech skip for an ok document: %+v", stats.SkipSummary)
	}
	if snap := state.Snapshot(); snap.Indexed != 1 || snap.Skipped != 0 || snap.Errors != 0 {
		t.Errorf("run counters indexed=%d skipped=%d errors=%d, want 1/0/0", snap.Indexed, snap.Skipped, snap.Errors)
	}
	// The no-speech track is still guarded: its identity is recorded, so an STT
	// model change decodes it again.
	if _, ok := noSpeechRecord(t, st, "match.mp4"); !ok {
		t.Error("the no-speech track of an ok document must keep its identity record")
	}
}

// TestNoSpeech_DeleteRemovesTheRecord: tombstoning the document removes its
// no-speech record in the same transaction. A rename or a move reaches the
// same delete for the old path.
func TestNoSpeech_DeleteRemovesTheRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "clip.mp4"), "fake-video-bytes")
	st := newRealStore(t)
	client, _ := noSpeechWhisperServer(t)
	svc, _, _ := noSpeechService(t, root, testutil.TempDir(t), st, client, 0)
	svc.SetSTTIdentity("whisper", "large-v3")

	f := ingest.DiscoveredFile{RelPath: "clip.mp4", SizeBytes: 16, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	if value, ok := noSpeechRecord(t, st, "clip.mp4"); !ok || value == "" {
		t.Fatalf("a no_speech skip must record the identity that found no speech, got %q present=%v", value, ok)
	}
	if err := st.MarkDocumentDeleted(ctx, "clip.mp4"); err != nil {
		t.Fatalf("MarkDocumentDeleted: %v", err)
	}
	if value, ok := noSpeechRecord(t, st, "clip.mp4"); ok {
		t.Fatalf("the record must go with the document, still present: %q", value)
	}
}

// TestNoSpeech_LaterSpeechRemovesTheRecord: when the file changes and its new
// decode holds speech, the document is indexed and its no-speech record is
// gone, so it cannot mark the transcript stale on a later run.
func TestNoSpeech_LaterSpeechRemovesTheRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	st := newRealStore(t)
	// Request 1 (the first file) holds no speech; request 2 (the edited file)
	// holds speech.
	client, calls := scriptedWhisperServer(t, map[int32]bool{2: true})

	writeFile(t, filepath.Join(root, "clip.mp4"), "first-cut-bytes!")
	first, _, _ := noSpeechService(t, root, stateDir, st, client, 0)
	first.SetSTTIdentity("whisper", "large-v3")
	f := ingest.DiscoveredFile{RelPath: "clip.mp4", SizeBytes: 16, MTimeUnix: time.Now().Unix()}
	if err := first.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, ok := noSpeechRecord(t, st, "clip.mp4"); !ok {
		t.Fatal("precondition: the no-speech decode must write its record")
	}

	writeFile(t, filepath.Join(root, "clip.mp4"), "the edited cut, with dialogue")
	second, _, _ := noSpeechService(t, root, stateDir, st, client, 0)
	second.SetSTTIdentity("whisper", "large-v3")
	f2 := ingest.DiscoveredFile{RelPath: "clip.mp4", SizeBytes: 29, MTimeUnix: f.MTimeUnix + 60}
	if err := second.ProcessDocument(ctx, f2, nil, false); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if n := atomic.LoadInt32(calls); n != 2 {
		t.Fatalf("STT requests = %d, want 2: the edited file is decoded again", n)
	}
	if doc := mustGetDoc(t, st, "clip.mp4"); doc.Status != "ok" {
		t.Fatalf("status=%q skip_reason=%q, want ok: the new decode holds speech", doc.Status, doc.SkipReason)
	}
	if value, ok := noSpeechRecord(t, st, "clip.mp4"); ok {
		t.Fatalf("a decode with speech must remove the no-speech record, still present: %q", value)
	}
}

// TestNoSpeech_IdentityChangeDecodesAnOKDocumentAgain pins SPEC §7.6 with
// §8.6.13 for a document that is status=ok: a video whose only selected audio
// track holds no speech stays ok through its recognition representation. It
// has no transcript, so the §8.6.7 transcript identity gate has nothing to
// compare; only the no-speech record can make it stale. Scan 2 (nothing
// changed) decodes nothing. Then the STT derivation identity changes and the
// content_hash does not: scan 3 decodes the selected track again. Scan 4
// (the new identity is recorded) decodes nothing.
//
// Mutant killed: limiting the no-speech identity gate to a document skipped
// as no_speech, which leaves the track of an ok document on the old model.
func TestNoSpeech_IdentityChangeDecodesAnOKDocumentAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "match.mp4"), "fake-video-bytes")
	st := newRealStore(t)
	client, calls := noSpeechWhisperServer(t)
	f := ingest.DiscoveredFile{RelPath: "match.mp4", SizeBytes: 16, MTimeUnix: time.Now().Unix()}

	scan := func(sttModel string) {
		t.Helper()
		cfg := config.Config{RootDir: root, StateDir: stateDir, STTProvider: "off", RecognizeProvider: "serve"}
		svc := mustNewIngestService(t, cfg, st)
		svc.SetIndexingState(appstate.NewIndexingState(appstate.ModeIncremental))
		svc.SetTranscriber(client)
		svc.SetSTTIdentity("whisper", sttModel)
		svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 0, errors.New("no duration") }
		svc.ExtractAudioTrackFunc = func(context.Context, string) ([]byte, error) { return []byte("audio-track"), nil }
		svc.SetRecognizer(&fakeRecognizer{result: recognizeTestResult()})
		if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
			t.Fatalf("ProcessDocument (%s): %v", sttModel, err)
		}
		doc := mustGetDoc(t, st, "match.mp4")
		if doc.Status != "ok" || doc.SkipReason != "" {
			t.Fatalf("%s: status=%q skip_reason=%q, want ok", sttModel, doc.Status, doc.SkipReason)
		}
		assertRecognitionWithoutTranscript(t, st, "match.mp4")
	}

	scan("large-v3")
	hash := mustGetDoc(t, st, "match.mp4").ContentHash
	first, _ := noSpeechRecord(t, st, "match.mp4")
	scan("large-v3")
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Fatalf("STT requests after an unchanged scan = %d, want 1", n)
	}

	scan("gigaam-v2")
	if n := atomic.LoadInt32(calls); n != 2 {
		t.Fatalf("STT requests after the identity change = %d, want 2: the no-speech track of an ok document must be decoded again", n)
	}
	if got := mustGetDoc(t, st, "match.mp4").ContentHash; got != hash || hash == "" {
		t.Errorf("content_hash = %q, was %q: the test needs an unchanged, settled hash", got, hash)
	}
	second, ok := noSpeechRecord(t, st, "match.mp4")
	if !ok || second == first {
		t.Errorf("no-speech record = %q (present=%v), was %q: the new identity must be recorded", second, ok, first)
	}

	scan("gigaam-v2")
	if n := atomic.LoadInt32(calls); n != 2 {
		t.Errorf("STT requests after one more unchanged scan = %d, want 2", n)
	}
}
