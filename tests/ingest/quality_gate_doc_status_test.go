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
	"github.com/dirstral/dir2mcp/internal/store"
)

// These tests cover the §8.6.6 DOCUMENT-level contract of the output quality
// gate (issue #422 V2): a rejected OCR/transcript/translation output is a
// non-fatal per-document error. The document MUST end status=error carrying the
// canonical §14.4 code (OCR_FAILED / TRANSCRIBE_FAILED / TRANSLATE_FAILED), its
// content_hash MUST stay withheld so it is retried (#402/#413), it MUST count as
// exactly one error and zero indexed (#426), and its chunks MUST still be
// quarantined (embedding_status=error, category quality_gate). The chunk-level
// quarantine on its own is already covered by quality_gate_test.go; these tests
// add the document/status/counter surface that was the gap.

// qgService builds a service with the output quality gate enabled, backed by a
// real on-disk store and a fresh run-progress counter, so a full ProcessDocument
// exercises status persistence, content_hash withholding, and the indexed/error
// counters end to end.
func qgService(t *testing.T, root string, st *store.SQLiteStore) (*ingest.Service, *appstate.IndexingState) {
	t.Helper()
	cfg := config.Config{RootDir: root, StateDir: t.TempDir(), STTProvider: "off", QualityGatesEnabled: true}
	svc := mustNewIngestService(t, cfg, st)
	state := appstate.NewIndexingState(appstate.ModeIncremental)
	svc.SetIndexingState(state)
	return svc, state
}

// mustGetDoc reads a persisted document by rel_path, failing the test on error.
func mustGetDoc(t *testing.T, st *store.SQLiteStore, relPath string) model.Document {
	t.Helper()
	doc, err := st.GetDocumentByPath(context.Background(), relPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath(%s): %v", relPath, err)
	}
	return doc
}

// assertQuarantinedDoc bundles the shared §8.6.6/#402/#413/#426 assertions for a
// document whose generated output was rejected by the quality gate.
func assertQuarantinedDoc(t *testing.T, st *store.SQLiteStore, state *appstate.IndexingState, relPath, wantCode string) {
	t.Helper()

	doc := mustGetDoc(t, st, relPath)
	if doc.Status != "error" {
		t.Fatalf("%s: status = %q, want \"error\" (a rejected quality gate is a per-document error, §8.6.6)", relPath, doc.Status)
	}
	// The canonical §14.4 code leads the error_message so it is queryable through
	// RecentFailures / error_message without a store schema change.
	if !strings.HasPrefix(doc.ErrorMessage, wantCode) {
		t.Fatalf("%s: error_message = %q, want it to lead with canonical code %q", relPath, doc.ErrorMessage, wantCode)
	}
	// #402/#413: the done-marker stays withheld so the document is retried.
	if doc.ContentHash != "" {
		t.Fatalf("%s: content_hash = %q, want \"\" (withheld so the doc is retried next run)", relPath, doc.ContentHash)
	}

	// #426: exactly one error, zero indexed.
	snap := state.Snapshot()
	if snap.Errors != 1 {
		t.Fatalf("%s: run errors = %d, want 1", relPath, snap.Errors)
	}
	if snap.Indexed != 0 {
		t.Fatalf("%s: run indexed = %d, want 0 (a quarantined doc counts solely as an error, #426)", relPath, snap.Indexed)
	}

	// The document persists as a failure surfaced by RecentFailures.
	failures, err := st.RecentFailures(context.Background(), 20)
	if err != nil {
		t.Fatalf("RecentFailures: %v", err)
	}
	if !recentFailuresContain(failures, relPath) {
		t.Fatalf("%s missing from RecentFailures: %+v", relPath, failures)
	}

	// The chunk-level quarantine still holds: the rejected chunks are recorded as
	// quality_gate embedding failures.
	stats, err := st.CorpusStats(context.Background())
	if err != nil {
		t.Fatalf("CorpusStats: %v", err)
	}
	if stats.FailureSummary == nil || stats.FailureSummary.Categories[string(store.ErrorCategoryQualityGate)] == 0 {
		t.Fatalf("%s: expected quarantined chunks under FailureSummary category %q, got %+v",
			relPath, store.ErrorCategoryQualityGate, stats.FailureSummary)
	}
}

// TestQualityGate_DegenerateTranscript_MarksDocError is the transcript case:
// a repetition-loop transcript (the classic STT hallucination) is rejected, so
// the audio document ends status=error with TRANSCRIBE_FAILED, is retried
// (withheld hash), and counts as one error / zero indexed.
func TestQualityGate_DegenerateTranscript_MarksDocError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "loop.mp3"), "fake-audio")
	st := newRealStore(t)

	svc, state := qgService(t, root, st)
	svc.SetTranscriber(&fakeTranscriber{text: strings.Repeat("thank you ", 60)})
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	svc.SetTranscriptLanguage("en")

	f := ingest.DiscoveredFile{RelPath: "loop.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument hard-failed on a non-fatal quality-gate rejection: %v", err)
	}

	assertQuarantinedDoc(t, st, state, "loop.mp3", "TRANSCRIBE_FAILED")
}

// TestQualityGate_DegenerateOCR_MarksDocError is the OCR case: a repetition-loop
// OCR extraction is rejected, so the visual document ends status=error with
// OCR_FAILED.
func TestQualityGate_DegenerateOCR_MarksDocError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "scan.pdf"), "fake-pdf-bytes")
	st := newRealStore(t)

	svc, state := qgService(t, root, st)
	svc.SetOCR(&fakeOCR{text: strings.Repeat("thank you ", 60)})

	f := ingest.DiscoveredFile{RelPath: "scan.pdf", SizeBytes: 13, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument hard-failed on a non-fatal quality-gate rejection: %v", err)
	}

	assertQuarantinedDoc(t, st, state, "scan.pdf", "OCR_FAILED")
}

// TestQualityGate_DegenerateTranslation_MarksDocError is the translation case: a
// clean source transcript passes, but a degenerate translation output is
// rejected, so the document ends status=error with TRANSLATE_FAILED (§8.6.6 —
// a failed translation gate is a per-document error, distinct from a best-effort
// translation provider failure which does not mark the doc).
func TestQualityGate_DegenerateTranslation_MarksDocError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)

	svc, state := qgService(t, root, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] a clean source line\n[00:02] another clean line"})
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	svc.SetTranscriptLanguage("de")
	svc.SetTranslator(degenerateTranslator{}, "mistral", "m", []string{"en"})

	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument hard-failed on a non-fatal quality-gate rejection: %v", err)
	}

	assertQuarantinedDoc(t, st, state, "talk.mp3", "TRANSLATE_FAILED")
}

// TestQualityGate_CleanTranscript_IndexedNoError is the passing companion: a
// clean transcript is unaffected — the document indexes normally (status ok,
// content_hash stamped, one indexed, zero errors).
func TestQualityGate_CleanTranscript_IndexedNoError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "clean.mp3"), "fake-audio")
	st := newRealStore(t)

	svc, state := qgService(t, root, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] welcome to the lecture today\n[00:02] we discuss several distinct topics in depth\n[00:05] including history geography and science across many regions"})
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	svc.SetTranscriptLanguage("en")

	f := ingest.DiscoveredFile{RelPath: "clean.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}

	doc := mustGetDoc(t, st, "clean.mp3")
	if doc.Status != "ok" {
		t.Fatalf("clean.mp3: status = %q, want \"ok\"", doc.Status)
	}
	if doc.ContentHash == "" {
		t.Fatal("clean.mp3: content_hash was withheld for a clean transcript (should be stamped/indexed)")
	}
	snap := state.Snapshot()
	if snap.Indexed != 1 || snap.Errors != 0 {
		t.Fatalf("clean.mp3: indexed=%d errors=%d, want indexed=1 errors=0", snap.Indexed, snap.Errors)
	}
}

// TestQualityGate_QuarantineDoesNotSuppressLaterIndexedCredit pins the
// per-document reset of the §8.6.6/#426 quarantine flag (the authoritative reset
// now lives at processDocument entry). A quarantined document leaves the flag
// true; a subsequent document that takes an early-return path (here an ok cache
// hit) must still be credited as indexed — the stale flag from the prior document
// must NOT leak in and suppress its indexed credit.
func TestQualityGate_QuarantineDoesNotSuppressLaterIndexedCredit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "clean.mp3"), "clean-audio")
	writeFile(t, filepath.Join(root, "loop.mp3"), "loop-audio")
	st := newRealStore(t)

	svc, state := qgService(t, root, st)
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	svc.SetTranscriptLanguage("en")

	cleanF := ingest.DiscoveredFile{RelPath: "clean.mp3", SizeBytes: 11, MTimeUnix: time.Now().Unix()}
	loopF := ingest.DiscoveredFile{RelPath: "loop.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}

	// 1) Index a clean audio document so it becomes a cache hit on a later scan.
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] welcome to the lecture today\n[00:02] we discuss several distinct topics in depth\n[00:05] including history geography and science across many regions"})
	if err := svc.ProcessDocument(ctx, cleanF, nil, false); err != nil {
		t.Fatalf("ProcessDocument(clean.mp3): %v", err)
	}
	if snap := state.Snapshot(); snap.Indexed != 1 || snap.Errors != 0 {
		t.Fatalf("after clean.mp3: indexed=%d errors=%d, want indexed=1 errors=0", snap.Indexed, snap.Errors)
	}

	// 2) A quarantined document sets quarantinedThisDoc=true and does not clear it.
	svc.SetTranscriber(&fakeTranscriber{text: strings.Repeat("thank you ", 60)})
	if err := svc.ProcessDocument(ctx, loopF, nil, false); err != nil {
		t.Fatalf("ProcessDocument(loop.mp3) hard-failed on a non-fatal quality-gate rejection: %v", err)
	}
	if snap := state.Snapshot(); snap.Indexed != 1 || snap.Errors != 1 {
		t.Fatalf("after loop.mp3: indexed=%d errors=%d, want indexed=1 errors=1", snap.Indexed, snap.Errors)
	}

	// 3) Re-process the clean document. It is now an ok cache hit — an early-return
	// path that credits the indexed counter WITHOUT re-entering rep generation. The
	// stale quarantine flag from loop.mp3 must not suppress this credit, and no new
	// error may be recorded.
	if err := svc.ProcessDocument(ctx, cleanF, nil, false); err != nil {
		t.Fatalf("ProcessDocument(clean.mp3 cache hit): %v", err)
	}
	snap := state.Snapshot()
	if snap.Indexed != 2 {
		t.Fatalf("cache-hit after a quarantine: indexed=%d, want 2 (flag leaked and suppressed the indexed credit)", snap.Indexed)
	}
	if snap.Errors != 1 {
		t.Fatalf("cache-hit after a quarantine: errors=%d, want 1 (no error may leak across documents)", snap.Errors)
	}
}

// TestQualityGate_TranslationRejected_WithholdsHash_BothModes pins two coupled
// invariants for a translation-gate quarantine across BOTH the single-phase and
// the two-phase (transcription + derivation) pipelines:
//
//   - FIX 2 (#402/#413): each quarantined audio document ends status=error with an
//     EMPTY content_hash, so the next incremental run retries it. In the two-phase
//     path the derivation pass re-reads the doc after pass 1 already stamped the
//     hash, so recordQualityGateDocError must blank it — otherwise the unchanged
//     -content gate would skip (never retry) the quarantined doc.
//   - FIX 1 (#426): with two audio documents, the per-document quarantine flag is
//     reset per document, so BOTH translation rejections are counted (errors=2). A
//     leaked flag would suppress the second document's error accounting.
func TestQualityGate_TranslationRejected_WithholdsHash_BothModes(t *testing.T) {
	t.Parallel()
	for _, twoPhase := range []bool{false, true} {
		twoPhase := twoPhase
		name := "single-phase"
		if twoPhase {
			name = "two-phase"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			root := t.TempDir()
			mustWriteFile(t, filepath.Join(root, "audio", "one.mp3"), []byte("fake-audio-one"))
			mustWriteFile(t, filepath.Join(root, "audio", "two.mp3"), []byte("fake-audio-two"))
			st := newRealStore(t)

			cfg := config.Config{
				RootDir:             root,
				StateDir:            t.TempDir(),
				STTProvider:         "off",
				QualityGatesEnabled: true,
				MediaBatchTwoPhase:  twoPhase,
			}
			svc := mustNewIngestService(t, cfg, st)
			state := appstate.NewIndexingState(appstate.ModeIncremental)
			svc.SetIndexingState(state)
			// A clean source transcript (passes the gate) with a degenerate translation
			// (rejected by the gate).
			svc.SetTranscriber(&fakeTranscriber{text: "[00:00] a clean source line\n[00:02] another clean line"})
			svc.SetSTTIdentity("whisper", "whisper-large-v3")
			svc.SetTranscriptLanguage("de")
			svc.SetTranslator(degenerateTranslator{}, "mistral", "m", []string{"en"})

			if err := svc.Run(ctx); err != nil {
				t.Fatalf("Run: %v", err)
			}

			for _, rel := range []string{"audio/one.mp3", "audio/two.mp3"} {
				doc := mustGetDoc(t, st, rel)
				if doc.Status != "error" {
					t.Fatalf("%s (%s): status=%q, want \"error\"", rel, name, doc.Status)
				}
				if !strings.HasPrefix(doc.ErrorMessage, "TRANSLATE_FAILED") {
					t.Fatalf("%s (%s): error_message=%q, want it to lead with TRANSLATE_FAILED", rel, name, doc.ErrorMessage)
				}
				// FIX 2: the done-marker is withheld so the doc is retried next run — the
				// same behavior in single-phase and two-phase (removing the two-phase
				// content_hash-inconsistency limitation).
				if doc.ContentHash != "" {
					t.Fatalf("%s (%s): content_hash=%q, want \"\" (withheld so the quality-gate quarantine is retried in %s mode)", rel, name, doc.ContentHash, name)
				}
			}

			// FIX 1: both documents' translation rejections are counted — the flag did
			// not leak from the first quarantined document into the second.
			if snap := state.Snapshot(); snap.Errors != 2 {
				t.Fatalf("%s: run errors=%d, want 2 (both translation rejections counted; a leaked quarantine flag would suppress the second)", name, snap.Errors)
			}
		})
	}
}

// TestQualityGate_Disabled_LeavesDocOK proves the master switch: with the gate
// off (the default), even a degenerate transcript leaves the document status=ok
// and indexed — the nil gate is a pure no-op with no document-status side effect.
func TestQualityGate_Disabled_LeavesDocOK(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "loop.mp3"), "fake-audio")
	st := newRealStore(t)

	// QualityGatesEnabled defaults to false here.
	cfg := config.Config{RootDir: root, StateDir: t.TempDir(), STTProvider: "off"}
	svc := mustNewIngestService(t, cfg, st)
	state := appstate.NewIndexingState(appstate.ModeIncremental)
	svc.SetIndexingState(state)
	svc.SetTranscriber(&fakeTranscriber{text: strings.Repeat("thank you ", 60)})
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	svc.SetTranscriptLanguage("en")

	f := ingest.DiscoveredFile{RelPath: "loop.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}

	doc := mustGetDoc(t, st, "loop.mp3")
	if doc.Status != "ok" {
		t.Fatalf("gate off: status = %q, want \"ok\" (a nil gate must not change document status)", doc.Status)
	}
	if doc.ContentHash == "" {
		t.Fatal("gate off: content_hash was withheld though the gate is disabled")
	}
	snap := state.Snapshot()
	if snap.Errors != 0 {
		t.Fatalf("gate off: run errors = %d, want 0 (nil gate is a no-op)", snap.Errors)
	}
}
