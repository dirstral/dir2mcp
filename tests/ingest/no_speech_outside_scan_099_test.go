package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SettleNoSpeechOutsideScan serves dir2mcp_transcribe, which decodes only the
// first audio track outside a scan (SPEC §8.6.13, spec 0.99.0).

// seedOKAudioDoc stores voice.wav as an ok audio document with a content hash.
func seedOKAudioDoc(t *testing.T, st model.Store) model.Document {
	t.Helper()
	doc := model.Document{RelPath: "voice.wav", DocType: "audio", SourceType: "file", SizeBytes: 10, ContentHash: "abc", Status: "ok"}
	if err := st.UpsertDocument(context.Background(), doc); err != nil {
		t.Fatalf("seed document: %v", err)
	}
	return doc
}

// TestSettleNoSpeechOutsideScan_FailedRecordMarksTheDocumentError: when the
// store refuses the no-speech record, the call returns the error and the
// document is recorded as status=error. The next scan then processes it again
// and does not skip it as an unchanged ok document.
//
// Mutant killed: returning the error with the row left ok.
func TestSettleNoSpeechOutsideScan_FailedRecordMarksTheDocumentError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := &settingFailingStore{SQLiteStore: newRealStore(t)}
	svc, _, _ := noSpeechService(t, testutil.TempDir(t), testutil.TempDir(t), st, &speechWindowTranscriber{silent: true}, 0)
	doc := seedOKAudioDoc(t, st.SQLiteStore)

	st.fail.Store(true)
	if err := svc.SettleNoSpeechOutsideScan(ctx, doc); err == nil {
		t.Fatal("SettleNoSpeechOutsideScan returned no error for a refused record write")
	}
	got := mustGetDoc(t, st.SQLiteStore, "voice.wav")
	if got.Status != "error" {
		t.Fatalf("status=%q skip_reason=%q, want error so the next scan processes the document again", got.Status, got.SkipReason)
	}
}

// TestSettleNoSpeechOutsideScan_MultiTrackSelectionLeavesTheState: with
// media.stt.tracks selecting more than the first track, a first track with no
// speech does not decide no_speech (§8.6.12). The row keeps its status and no
// record is written, but the done marker is withheld, so the next scan
// decodes every selected track.
//
// Mutant killed: settling no_speech from the first track alone.
func TestSettleNoSpeechOutsideScan_MultiTrackSelectionLeavesTheState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newRealStore(t)
	cfg := config.Config{RootDir: testutil.TempDir(t), StateDir: testutil.TempDir(t), STTProvider: "off", MediaSTTTracks: []string{"all"}}
	svc := mustNewIngestService(t, cfg, st)
	svc.SetTranscriber(&speechWindowTranscriber{silent: true})
	doc := seedOKAudioDoc(t, st)

	if err := svc.SettleNoSpeechOutsideScan(ctx, doc); err != nil {
		t.Fatalf("SettleNoSpeechOutsideScan: %v", err)
	}
	got := mustGetDoc(t, st, "voice.wav")
	if got.Status != "ok" || got.SkipReason != "" {
		t.Fatalf("status=%q skip_reason=%q, want ok: the other selected tracks were not decoded", got.Status, got.SkipReason)
	}
	if rec, err := st.GetSetting(ctx, store.NoSpeechIdentitySettingKey("voice.wav")); err == nil && rec != "" {
		t.Errorf("a no-speech record %q was stored from the first track alone", rec)
	}
	if got.ContentHash != "" {
		t.Errorf("content_hash = %q, want empty: the done marker is withheld so the next scan decodes every selected track", got.ContentHash)
	}
}
