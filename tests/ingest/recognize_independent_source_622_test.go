package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// Regression guard for #622: recognition is an INDEPENDENT representation source
// (design 0004 §5.2 `recognize`), not a step subordinate to the transcript.
//
// Before the fix, generateRepresentations returned early as soon as the
// transcript path reported a sidecar-less/STT-less video as representation-less,
// so GenerateRecognitionRepresentation never ran: a configured recognition
// backend was never called, nothing was persisted, and the video was recorded as
// status="error" — even though recognition was the whole reason the operator
// pointed dir2mcp at that corpus.

// runVideoIngestWithRecognizer indexes a single sidecar-less .mp4 through the
// FULL ingest pipeline (not GenerateRecognitionRepresentation directly) with a
// recognizer bound, mirroring runArchiveIngest.
func runVideoIngestWithRecognizer(t *testing.T, rec model.Recognizer) *store.SQLiteStore {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()

	if err := os.WriteFile(filepath.Join(root, "clip.mp4"), []byte("fake-video-bytes"), 0o600); err != nil {
		t.Fatalf("write video: %v", err)
	}

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}

	cfg := config.Default()
	cfg.RootDir = root
	if rec != nil {
		// The backend is a test double, but the provider must look configured so
		// the persisted representation records an honest derivation identity.
		// Left at the default `off` when no double is injected, so the service
		// binds no recognizer at all.
		cfg.RecognizeProvider = "serve"
	}
	svc := mustNewIngestService(t, cfg, st)
	if rec != nil {
		svc.SetRecognizer(rec)
	}

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return st
}

// TestRecognition_RunsWhenTranscriptPathProducesNothing is the core #622 guard:
// STT off + no sidecar + multimodal off, but a recognition backend IS configured.
// Recognition must run, persist its representation, and the video must NOT be
// reported as representation-less.
func TestRecognition_RunsWhenTranscriptPathProducesNothing(t *testing.T) {
	rec := &fakeRecognizer{result: recognizeTestResult()}
	st := runVideoIngestWithRecognizer(t, rec)

	if rec.calls != 1 {
		t.Fatalf("recognition backend calls = %d, want 1 — recognition must run even when "+
			"the transcript path produced nothing (#622)", rec.calls)
	}

	doc := documentByPath(t, st, "clip.mp4")
	if doc.Status == "error" {
		t.Errorf("status = %q with error_message %q; a video carrying recognition annotations "+
			"IS searchable and must not be recorded as representation-less", doc.Status, doc.ErrorMessage)
	}

	repTypes, err := st.RepresentationTypesByPath(context.Background(), "clip.mp4")
	if err != nil {
		t.Fatalf("RepresentationTypesByPath: %v", err)
	}
	var found bool
	for _, rt := range repTypes {
		if rt == ingest.RepTypeRecognition {
			found = true
		}
	}
	if !found {
		t.Fatalf("no %q representation persisted, got %v", ingest.RepTypeRecognition, repTypes)
	}
}

// TestRecognition_NoBackend_StillReportsUnsearchableVideo pins the other side of
// the fix: with NO recognizer bound, the pre-existing #398/#495 diagnostic must
// still fire. The verdict was deferred, not removed.
func TestRecognition_NoBackend_StillReportsUnsearchableVideo(t *testing.T) {
	st := runVideoIngestWithRecognizer(t, nil)

	doc := documentByPath(t, st, "clip.mp4")
	if doc.Status != "error" {
		t.Fatalf("status = %q, want \"error\": with no sidecar, no transcript, no multimodal "+
			"and no recognition the video is genuinely unsearchable (#398/#495)", doc.Status)
	}
	if !strings.Contains(strings.ToLower(doc.ErrorMessage), "no representation") {
		t.Errorf("error_message = %q, want it to explain the video produced no representation", doc.ErrorMessage)
	}
	// The remedy text must now mention recognition too, so an operator who has a
	// backend available is told about it (#622).
	if !strings.Contains(strings.ToLower(doc.ErrorMessage), "recogni") {
		t.Errorf("error_message = %q, want it to offer a recognition backend as a remedy", doc.ErrorMessage)
	}
}

// TestRecognition_RunsWhenTranscriptProviderFails covers the review finding on
// #623: generateTranscriptOrSidecar also soft-fails (nonFatalErrored=true, err=nil)
// when STT is configured but the provider fails and no media chunks exist. That
// leaves the document with no transcript — exactly when recognition is the only
// remaining source — so recognition must still run, and the transcript failure's
// own status="error" must be preserved so it is retried next run.
func TestRecognition_RunsWhenTranscriptProviderFails(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "clip.mp4"), []byte("fake-video-bytes"), 0o600); err != nil {
		t.Fatalf("write video: %v", err)
	}
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	cfg := config.Default()
	cfg.RootDir = root
	cfg.RecognizeProvider = "serve"
	svc := mustNewIngestService(t, cfg, st)
	// STT configured but failing, with no media chunks -> the transcript path
	// soft-fails and previously short-circuited before recognition.
	svc.SetTranscriber(&fakeTranscriber{err: errors.New("stt backend down")})
	rec := &fakeRecognizer{result: recognizeTestResult()}
	svc.SetRecognizer(rec)

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if rec.calls != 1 {
		t.Fatalf("recognition backend calls = %d, want 1 — a transcript provider failure "+
			"must not skip recognition", rec.calls)
	}
	repTypes, err := st.RepresentationTypesByPath(ctx, "clip.mp4")
	if err != nil {
		t.Fatalf("RepresentationTypesByPath: %v", err)
	}
	var found bool
	for _, rt := range repTypes {
		if rt == ingest.RepTypeRecognition {
			found = true
		}
	}
	if !found {
		t.Fatalf("no %q representation persisted, got %v", ingest.RepTypeRecognition, repTypes)
	}
}

// TestRecognition_EmptyAnnotations_StillReportsUnsearchableVideo covers the
// backend-present-but-silent case: a recognizer that returns zero annotations
// persists nothing, so the video really is unsearchable and must be reported.
func TestRecognition_EmptyAnnotations_StillReportsUnsearchableVideo(t *testing.T) {
	rec := &fakeRecognizer{result: model.RecognizeResult{Name: "dirstral-annotator", Version: "0.2.0"}}
	st := runVideoIngestWithRecognizer(t, rec)

	if rec.calls != 1 {
		t.Fatalf("recognition backend calls = %d, want 1", rec.calls)
	}
	doc := documentByPath(t, st, "clip.mp4")
	if doc.Status != "error" {
		t.Fatalf("status = %q, want \"error\": a backend returning zero annotations persists "+
			"nothing, so the video stays unsearchable", doc.Status)
	}
}
