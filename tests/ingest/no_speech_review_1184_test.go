package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Review follow-ups on #1184 (SPEC §8.6.6 "Silence is an outcome", #1182).

// noSpeechWriteFailingStore refuses the no_speech skip upsert, as a full disk
// or a locked database would.
type noSpeechWriteFailingStore struct {
	*store.SQLiteStore
}

func (s *noSpeechWriteFailingStore) UpsertDocument(ctx context.Context, doc model.Document) error {
	if doc.Status == string(model.DocumentStatusSkipped) && doc.SkipReason == model.SkipReasonNoSpeech {
		return errors.New("disk I/O error")
	}
	return s.SQLiteStore.UpsertDocument(ctx, doc)
}

// TestNoSpeech_FailedSkipWriteIsNotFinalized: when the no_speech row cannot be
// written, the document must not be stamped done as an ok row with nothing
// indexed. It is recorded as an error with no done marker, so the next scan
// processes it again.
func TestNoSpeech_FailedSkipWriteIsNotFinalized(t *testing.T) {
	t.Parallel()
	fx := newNoSpeechFixture(t, "silent.mp4", "fake-video-with-a-silent-audio-track")
	failing := &noSpeechWriteFailingStore{SQLiteStore: fx.st}
	svc := mustNewIngestService(t, config.Config{RootDir: fx.root, StateDir: fx.stateDir, STTProvider: "off"}, failing)
	svc.SetTranscriber(&countingNoSpeechTranscriber{})
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	svc.ExtractAudioTrackFunc = func(context.Context, string) ([]byte, error) { return []byte("aac"), nil }
	state := appstate.NewIndexingState(appstate.ModeIncremental)
	svc.SetIndexingState(state)
	_ = svc.ProcessDocument(context.Background(), fx.file, nil, false)

	doc := documentByPath(t, fx.st, "silent.mp4")
	if doc.Status == string(model.DocumentStatusOK) || strings.TrimSpace(doc.ContentHash) != "" {
		t.Fatalf("status = %q content_hash = %q after a failed no_speech write, want a non-ok row with no done marker", doc.Status, doc.ContentHash)
	}
	if snap := state.Snapshot(); snap.Skipped != 0 {
		t.Errorf("skipped = %d, want 0: a skip the store refused must not be counted", snap.Skipped)
	}
}

// TestNoSpeech_ArchiveMemberNotCountedAsExtraSkip: archive members are not
// credited to the run's counters (the container accounts for the archive), so
// a silent member must not add a second skip to a one-file scan.
func TestNoSpeech_ArchiveMemberNotCountedAsExtraSkip(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	if err := os.WriteFile(filepath.Join(root, "media.zip"), buildZip(t, map[string]string{"silent.mp3": "fake-mp3"}), 0o600); err != nil {
		t.Fatal(err)
	}
	st := newRealStore(t)
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: testutil.TempDir(t), STTProvider: "off"}, st)
	svc.SetTranscriber(&countingNoSpeechTranscriber{})
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	state := appstate.NewIndexingState(appstate.ModeIncremental)
	svc.SetIndexingState(state)
	var skipEvents []string
	svc.SetOnDocumentSkip(func(relPath, _, _ string) { skipEvents = append(skipEvents, relPath) })
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	assertNoSpeechSkip(t, st, "media.zip/silent.mp3")
	snap := state.Snapshot()
	if snap.Skipped > snap.Scanned {
		t.Errorf("scanned = %d skipped = %d: the silent member was counted on top of its container", snap.Scanned, snap.Skipped)
	}
	for _, p := range skipEvents {
		if p == "media.zip/silent.mp3" {
			t.Errorf("a file_skip fired for the archive member %s; members raise no events (SPEC §3.2)", p)
		}
	}
}

// TestNoSpeech_ArchiveMemberRederivedOnIdentityChange: a silent member of an
// unchanged archive is re-derived when the STT identity changes, exactly as a
// top-level file is. The container's own unchanged hash must not hide it.
func TestNoSpeech_ArchiveMemberRederivedOnIdentityChange(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	if err := os.WriteFile(filepath.Join(root, "media.zip"), buildZip(t, map[string]string{"silent.mp3": "fake-mp3"}), 0o600); err != nil {
		t.Fatal(err)
	}
	st := newRealStore(t)
	stateDir := testutil.TempDir(t)
	tr := &countingNoSpeechTranscriber{}
	run := func(sttModel string) {
		svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: stateDir, STTProvider: "off"}, st)
		svc.SetTranscriber(tr)
		svc.SetSTTIdentity("whisper", sttModel)
		svc.SetIndexingState(appstate.NewIndexingState(appstate.ModeIncremental))
		if err := svc.Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	run("whisper-large-v3")
	assertNoSpeechSkip(t, st, "media.zip/silent.mp3")
	run("whisper-large-v3")
	if got := tr.callCount(); got != 1 {
		t.Fatalf("STT calls after an unchanged rescan = %d, want 1", got)
	}
	run("whisper-large-v4")
	if got := tr.callCount(); got != 2 {
		t.Errorf("STT calls after the STT identity changed = %d, want 2 (the silent member is re-derived)", got)
	}
}

// TestNoSpeech_TrackSelectionChangeRederives: a no_speech verdict was judged
// over the selected tracks. Track 0 is silent and track 1 has speech; widening
// media.stt.tracks from first to all must reach track 1.
func TestNoSpeech_TrackSelectionChangeRederives(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	body := "audio-file-bytes"
	writeFile(t, filepath.Join(root, "talk.mp3"), body)
	st := newRealStore(t)
	stateDir := testutil.TempDir(t)
	tr := &perTrackTranscriber{byInput: map[string]string{body: "", trackAudioBytes(1): "[00:00] the dub has speech"}}

	ingestPerTrack(t, newPerTrackService(t, root, stateDir, st, nil, tr), st, "talk.mp3", body)
	assertNoSpeechSkip(t, st, "talk.mp3")

	ingestPerTrack(t, newPerTrackService(t, root, stateDir, st, []string{"all"}, tr), st, "talk.mp3", body)
	doc := documentByPath(t, st, "talk.mp3")
	if doc.Status != string(model.DocumentStatusOK) {
		t.Errorf("status = %q skip_reason = %q after widening media.stt.tracks to all, want ok (track 1 has speech)", doc.Status, doc.SkipReason)
	}
}

// TestNoSpeech_EmptyPartialDecodeStaysRetryable: one window answers with no
// speech and the others fail. That is not silence, and with the coverage floor
// off it must not settle as an ok document with nothing indexed: it is an
// error, and the next scan sends the failed audio to STT again.
func TestNoSpeech_EmptyPartialDecodeStaysRetryable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "quiet.mp3"), "fake-audio")
	st := newRealStore(t)
	svc, _, _ := partialFloorService(t, root, st, 0, "warn")
	tr := &silentWindowTranscriber{}
	svc.SetTranscriber(tr)
	f := ingest.DiscoveredFile{RelPath: "quiet.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	doc := mustGetDoc(t, st, "quiet.mp3")
	if doc.Status != string(model.DocumentStatusError) {
		t.Fatalf("status = %q skip_reason = %q, want error: an empty transcript from a decode whose other windows failed is a provider failure", doc.Status, doc.SkipReason)
	}
	first := tr.calls
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("ProcessDocument (rescan): %v", err)
	}
	if tr.calls == first {
		t.Errorf("STT calls = %d after the rescan, want more than %d: the failed windows must be retried, not served from the cache", tr.calls, first)
	}
}

// TestNoSpeech_EmptyTranscriptCacheWithoutDocumentRow: the empty transcript is
// cached on its own, independently of the skipped row. A fresh metadata store
// over the same state dir records no_speech without calling STT again.
func TestNoSpeech_EmptyTranscriptCacheWithoutDocumentRow(t *testing.T) {
	t.Parallel()
	fx := newNoSpeechFixture(t, "silent.mp4", "fake-video-with-a-silent-audio-track")
	tr := &countingNoSpeechTranscriber{}
	svc, _ := fx.service(t, tr, "whisper-large-v3")
	fx.process(t, svc, fx.file)
	assertNoSpeechSkip(t, fx.st, "silent.mp4")

	fresh := fx
	fresh.st = newRealStore(t)
	svc2, _ := fresh.service(t, tr, "whisper-large-v3")
	fresh.process(t, svc2, fresh.file)
	if got := tr.callCount(); got != 1 {
		t.Errorf("STT calls with a fresh store = %d, want 1 (the empty transcript is served from the cache)", got)
	}
	assertNoSpeechSkip(t, fresh.st, "silent.mp4")
}

// TestNoSpeech_FallbackStampDueFirstWins: a multi-track no_speech row keeps one
// §8.2.4 stamp, so it must keep the one that becomes due first, whatever the
// track order: a stamp from another default profile first, then the earliest
// route_fallback_at.
func TestNoSpeech_FallbackStampDueFirstWins(t *testing.T) {
	t.Parallel()
	const producer = "whisper|base"
	cases := []struct {
		name               string
		aAt, aBy, bAt, bBy string
		wantAt, wantBy     string
	}{
		{"earlier at wins in either order", "2026-10-18T10:30:00Z", producer, "2026-10-18T10:00:00Z", producer, "2026-10-18T10:00:00Z", producer},
		{"earlier at kept", "2026-10-18T10:00:00Z", producer, "2026-10-18T10:30:00Z", producer, "2026-10-18T10:00:00Z", producer},
		{"other producer is due at once", "2026-10-18T10:00:00Z", producer, "2026-10-18T10:30:00Z", "whisper|old", "2026-10-18T10:30:00Z", "whisper|old"},
		{"unparseable at is due at once", "2026-10-18T10:00:00Z", producer, "", producer, "", producer},
	}
	for _, c := range cases {
		at, by := ingest.NoSpeechFallbackDueFirstForTest(producer, c.aAt, c.aBy, c.bAt, c.bBy)
		if at != c.wantAt || by != c.wantBy {
			t.Errorf("%s: kept (%q, %q), want (%q, %q)", c.name, at, by, c.wantAt, c.wantBy)
		}
	}
}
