package tests

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// skipUncoveredService builds a media-ingesting service whose STT identity,
// pinned source language, declared coverage, and honest-coverage floor action are
// all set deterministically (no real provider), backed by a real on-disk store so
// a full ProcessDocument exercises status persistence and the SkipSummary
// aggregate end to end (SPEC §8.2.1, #566). The returned fakeTranscriber lets a
// caller assert whether transcription was ATTEMPTED (calls) — the load-bearing
// "did not transcribe" signal for the skip path.
func skipUncoveredService(t *testing.T, root string, st *store.SQLiteStore, pinLang string, coverage []string, action string) (*ingest.Service, *appstate.IndexingState, *fakeTranscriber) {
	t.Helper()
	cfg := config.Config{RootDir: root, StateDir: t.TempDir(), STTProvider: "off"}
	svc := mustNewIngestService(t, cfg, st)
	state := appstate.NewIndexingState(appstate.ModeIncremental)
	svc.SetIndexingState(state)
	ft := &fakeTranscriber{text: "[00:00] a clean line of speech\n[00:02] another clean line of speech"}
	svc.SetTranscriber(ft)
	svc.SetSTTIdentity("whisper", "large-v3")
	svc.SetTranscriptLanguage(pinLang)
	svc.SetSTTLanguages(coverage)
	svc.SetOnUncoveredLanguage(action)
	return svc, state, ft
}

// TestSTTSkipUncovered_SkipRecordsDurableSkip is the core #566 skip case: the
// pinned source language "kir" is outside the model's declared coverage [ru, en]
// and media.stt.on_uncovered_language=skip, so the audio document is recorded as a
// durable status="skipped" with skip_reason="language_uncovered" — no transcript
// is produced and the transcriber is never invoked. The gap surfaces in the
// SkipSummary honest-coverage aggregate rather than as garbage the quality gate
// silently drops (SPEC §8.2.1).
func TestSTTSkipUncovered_SkipRecordsDurableSkip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)

	svc, state, ft := skipUncoveredService(t, root, st, "kir", []string{"ru", "en"}, "skip")

	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("ProcessDocument hard-failed on a non-fatal language-uncovered skip: %v", err)
	}

	doc := mustGetDoc(t, st, "talk.mp3")
	if doc.Status != "skipped" {
		t.Fatalf("talk.mp3: status = %q, want \"skipped\" (uncovered language under skip mode, §8.2.1)", doc.Status)
	}
	if doc.SkipReason != model.SkipReasonLanguageUncovered {
		t.Fatalf("talk.mp3: skip_reason = %q, want %q", doc.SkipReason, model.SkipReasonLanguageUncovered)
	}
	// Load-bearing: transcription must NOT have been attempted (no degraded output).
	if ft.calls != 0 {
		t.Fatalf("talk.mp3: transcriber was invoked %d time(s), want 0 (skip must not transcribe)", ft.calls)
	}
	// The skipped counter is credited exactly once.
	if snap := state.Snapshot(); snap.Skipped != 1 {
		t.Fatalf("talk.mp3: run skipped = %d, want 1", snap.Skipped)
	}
	// The reason flows into the honest-coverage SkipSummary aggregate (SPEC §15.2).
	stats, err := st.CorpusStats(ctx)
	if err != nil {
		t.Fatalf("CorpusStats: %v", err)
	}
	if stats.SkipSummary == nil || stats.SkipSummary.Categories[model.SkipReasonLanguageUncovered] != 1 {
		t.Fatalf("expected SkipSummary category %q == 1, got %+v", model.SkipReasonLanguageUncovered, stats.SkipSummary)
	}
}

// TestSTTSkipUncovered_WarnTranscribesAnyway is the fail-open default: the SAME
// uncovered "kir"/[ru,en] pairing under media.stt.on_uncovered_language=warn (the
// default) still transcribes — the item indexes normally and the transcriber IS
// invoked. This proves the new skip branch is inert unless the operator opts into
// skip; the Slice-A honest-coverage warning + meta flag remain the backstop.
func TestSTTSkipUncovered_WarnTranscribesAnyway(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)

	svc, _, ft := skipUncoveredService(t, root, st, "kir", []string{"ru", "en"}, "warn")

	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}

	doc := mustGetDoc(t, st, "talk.mp3")
	if doc.Status != "ok" {
		t.Fatalf("talk.mp3 (warn): status = %q, want \"ok\" (warn transcribes anyway)", doc.Status)
	}
	if ft.calls == 0 {
		t.Fatal("talk.mp3 (warn): transcriber was not invoked, want it to transcribe under the fail-open default")
	}
}

// TestSTTSkipUncovered_CoveredLanguageTranscribesUnderSkip proves the floor is not
// over-eager: even with media.stt.on_uncovered_language=skip, a pinned language
// that IS in the declared coverage ("ru" in [ru, en]) transcribes normally — skip
// only suppresses genuinely uncovered languages, never covered ones.
func TestSTTSkipUncovered_CoveredLanguageTranscribesUnderSkip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)

	svc, _, ft := skipUncoveredService(t, root, st, "ru", []string{"ru", "en"}, "skip")

	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}

	doc := mustGetDoc(t, st, "talk.mp3")
	if doc.Status != "ok" {
		t.Fatalf("talk.mp3 (covered+skip): status = %q, want \"ok\" (covered language must transcribe)", doc.Status)
	}
	if ft.calls == 0 {
		t.Fatal("talk.mp3 (covered+skip): transcriber was not invoked, want it to transcribe a covered language")
	}
}

// TestSTTSkipUncovered_UnknownCoverageTranscribesUnderSkip proves absence of a
// declared coverage set is not evidence of non-coverage: with skip mode but NO
// declared stt_languages (open/unknown), the floor does not apply and the item
// transcribes — an empty coverage set never floors every language (SPEC §8.2.1).
func TestSTTSkipUncovered_UnknownCoverageTranscribesUnderSkip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)

	svc, _, ft := skipUncoveredService(t, root, st, "kir", nil, "skip")

	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}

	doc := mustGetDoc(t, st, "talk.mp3")
	if doc.Status != "ok" {
		t.Fatalf("talk.mp3 (unknown coverage+skip): status = %q, want \"ok\" (unknown coverage never floors)", doc.Status)
	}
	if ft.calls == 0 {
		t.Fatal("talk.mp3 (unknown coverage+skip): transcriber was not invoked, want it to transcribe")
	}
}
