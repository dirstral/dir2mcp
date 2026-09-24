package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// Regression guards for #950, measured on a live pilot: a periodic re-scan retried
// recognition on an already-indexed broadcast, the backend answered HTTP 502, and
// the document was stamped status="error". store.liveParentDocument then hid every
// one of its 1570 chunks from search for about 60 hours, with the chunks still
// marked embedded, nothing logged above info, and no in-process retry: only a
// restart's scan tried again. #894 had fixed exactly this harm for the TIMEOUT
// failure; these tests pin that every recognition failure on a document with
// indexed content takes the same degrade path, and that a document with nothing
// indexed still fails loudly.

// brokenThenHealthyRecognizer fails with a backend error until healAfter calls
// have been made, then returns a canned result. It models a backend that was down
// (OOM, restart) during one scan and back for the next.
type brokenThenHealthyRecognizer struct {
	calls     int
	healAfter int
	result    model.RecognizeResult
}

func (r *brokenThenHealthyRecognizer) Recognize(_ context.Context, _ string) (model.RecognizeResult, error) {
	r.calls++
	if r.calls <= r.healAfter {
		return model.RecognizeResult{}, errors.New("recognize backend returned status 502")
	}
	return r.result, nil
}

func newRecognitionFailureService(t *testing.T, root string, rec *brokenThenHealthyRecognizer) (*store.SQLiteStore, func() error) {
	t.Helper()
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = t.TempDir()
	cfg.RecognizeProvider = "serve"
	cfg.RecognizeTimeout = time.Second
	cfg.RecognizeTimeoutPerMediaSecond = 0
	svc := mustNewIngestService(t, cfg, st)
	svc.SetRecognizer(rec)
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 0, os.ErrNotExist }
	return st, func() error { return svc.Run(ctx) }
}

// TestRecognitionFailure_DoesNotEmptyTheCorpus pins the #950 rule: a NON-timeout
// recognition failure on a document that already has indexed content must not
// stamp status="error" (which would hide every chunk it has), must leave the
// document's content searchable, must withhold the done marker so the next scan
// retries, and the retry must actually recover the document once the backend is
// back.
func TestRecognitionFailure_DoesNotEmptyTheCorpus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "broadcast.mp4"), "fake-video-bytes")
	writeFile(t, filepath.Join(root, "broadcast.vtt"),
		"WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nWebb delivers the pitch\n\n"+
			"00:00:02.000 --> 00:00:05.000\nFreeman flies out to centrefield\n")

	rec := &brokenThenHealthyRecognizer{healAfter: 1}
	st, run := newRecognitionFailureService(t, root, rec)

	// Scan 1: the sidecar is indexed, then recognition hits the broken backend.
	if err := run(); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("recognizer calls after run 1 = %d, want 1", rec.calls)
	}
	doc := documentByPath(t, st, "broadcast.mp4")
	if doc.Status == "error" {
		t.Fatalf("status = %q (%q): a backend failure must not fail a document that already "+
			"carries a transcript, because status=error hides every one of its chunks (#950)",
			doc.Status, doc.ErrorMessage)
	}
	if doc.ContentHash != "" {
		t.Errorf("content_hash = %q, want it withheld so the next scan retries recognition", doc.ContentHash)
	}
	hits, err := st.SearchBM25(ctx, "Freeman", 5, "")
	if err != nil {
		t.Fatalf("SearchBM25: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("the transcript that DID arrive is no longer searchable: the backend failure emptied the corpus")
	}

	// Scan 2: the backend is back. The withheld marker must make this scan retry,
	// and the document must settle as finished.
	if err := run(); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if rec.calls != 2 {
		t.Fatalf("recognizer calls after run 2 = %d, want 2: the failure must be retried on the next scan, not stuck", rec.calls)
	}
	doc = documentByPath(t, st, "broadcast.mp4")
	if doc.Status == "error" {
		t.Fatalf("status after the retry = %q (%q), want a live document", doc.Status, doc.ErrorMessage)
	}
	if doc.ContentHash == "" {
		t.Error("content_hash still withheld after a successful retry: the document would be re-recognized forever")
	}
	if hits, _ := st.SearchBM25(ctx, "Freeman", 5, ""); len(hits) == 0 {
		t.Fatal("the transcript must still be searchable after the retry")
	}
}

// TestRecognitionFailure_EmptyDocumentStillFailsLoudly pins the other half: a
// video with NOTHING indexed has no chunks a status="error" stamp could hide, and
// it genuinely is not searchable, so a backend failure is recorded durably (it
// shows in recent_failures after a restart) exactly as before #950.
func TestRecognitionFailure_EmptyDocumentStillFailsLoudly(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "clip.mp4"), "fake-video-bytes")

	rec := &brokenThenHealthyRecognizer{healAfter: 1 << 30}
	st, run := newRecognitionFailureService(t, root, rec)
	if err := run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	doc := documentByPath(t, st, "clip.mp4")
	if doc.Status != "error" {
		t.Fatalf("status = %q, want \"error\": a document with nothing indexed loses nothing to the stamp, "+
			"and an unsearchable document must be recorded durably", doc.Status)
	}
	if doc.ContentHash != "" {
		t.Errorf("content_hash = %q, want it withheld so the next scan retries", doc.ContentHash)
	}
}
