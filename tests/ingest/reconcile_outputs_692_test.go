package tests

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// Output-set reconciliation (dir2mcp #692).
//
// Incremental ingest wrote the outputs the ACTIVE pipeline produces but never
// retired the outputs a PREVIOUS pipeline produced. A removed translation target
// or a switched-off summary level therefore stayed live, searchable, citable,
// and exportable forever. A full reindex did not fix it either: a reindex clears
// documents.content_hash to force reprocessing, it does not tombstone
// representations.
//
// Every test below drives two complete scans over one unchanged corpus, so it
// exercises the INCREMENTAL path: the second scan finds the content unchanged
// and derives nothing.

// translateConfig returns a config with transcript translation enabled for the
// given target languages, as config validation would leave it.
func translateConfig(root, stateDir string, targets ...string) config.Config {
	return config.Config{
		RootDir:                   root,
		StateDir:                  stateDir,
		STTProvider:               "off",
		MediaTranslateEnabled:     true,
		MediaTranslateTargetLangs: targets,
	}
}

// translatingService builds a service that transcribes a media file and
// translates the transcript into every target language in cfg.
func translatingService(t *testing.T, cfg config.Config, st model.Store) *ingest.Service {
	t.Helper()
	svc := mustNewIngestService(t, cfg, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] intro\n[00:02] chapter one"})
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	svc.SetTranscriptLanguage("de")
	svc.SetTranslator(&fakeTranslator{}, "mistral", "mistral-small-2506", cfg.MediaTranslateTargetLangs)
	return svc
}

// activeRepTypes returns the rep_types of a document's live representations.
func activeRepTypes(t *testing.T, st *store.SQLiteStore, relPath string) map[string]bool {
	t.Helper()
	types, err := st.RepresentationTypesByPath(context.Background(), relPath)
	if err != nil {
		t.Fatalf("RepresentationTypesByPath(%s): %v", relPath, err)
	}
	out := make(map[string]bool, len(types))
	for _, repType := range types {
		out[repType] = true
	}
	return out
}

// repIDForType returns the rep_id of a document's live representation of
// repType, so the test can look at its chunks after it is retired.
func repIDForType(t *testing.T, st *store.SQLiteStore, relPath, repType string) int64 {
	t.Helper()
	reps, err := st.ActiveRepresentations(context.Background(), relPath)
	if err != nil {
		t.Fatalf("ActiveRepresentations(%s): %v", relPath, err)
	}
	for _, rep := range reps {
		if rep.RepType == repType {
			return rep.RepID
		}
	}
	t.Fatalf("no live %q representation on %s (have %+v)", repType, relPath, reps)
	return 0
}

// assertChunksTombstoned fails unless every chunk of repID is tombstoned. The
// SQLite tombstone is what removes a retired output from retrieval, in the
// current session and after a restart (SPEC §6.6).
func assertChunksTombstoned(t *testing.T, st *store.SQLiteStore, repID int64) {
	t.Helper()
	chunks, err := st.GetChunksByRepID(context.Background(), repID)
	if err != nil {
		t.Fatalf("GetChunksByRepID(%d): %v", repID, err)
	}
	if len(chunks) == 0 {
		t.Fatalf("rep %d has no chunks, so the tombstone assertion proves nothing", repID)
	}
	for _, chunk := range chunks {
		if !chunk.Deleted {
			t.Errorf("chunk %d of retired rep %d is still live; its vector stays searchable", chunk.ChunkID, repID)
		}
	}
}

// TestReconcile_RemovedTranslationTargetIsRetired is the headline case: the
// operator drops "es" from the target list. The es transcript must be retired,
// while the still-wanted en transcript and the source transcript are untouched.
func TestReconcile_RemovedTranslationTargetIsRetired(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	ctx := context.Background()

	first := translatingService(t, translateConfig(root, stateDir, "en", "es"), st)
	if err := first.Run(ctx); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	types := activeRepTypes(t, st, "talk.mp3")
	for _, want := range []string{ingest.RepTypeTranscript, ingest.TranscriptRepType("en"), ingest.TranscriptRepType("es")} {
		if !types[want] {
			t.Fatalf("first scan did not produce %q (have %v)", want, types)
		}
	}
	esRepID := repIDForType(t, st, "talk.mp3", ingest.TranscriptRepType("es"))

	// Second scan: same corpus, same bytes, "es" removed from the targets.
	second := translatingService(t, translateConfig(root, stateDir, "en"), st)
	if err := second.Run(ctx); err != nil {
		t.Fatalf("second scan: %v", err)
	}

	types = activeRepTypes(t, st, "talk.mp3")
	if types[ingest.TranscriptRepType("es")] {
		t.Errorf("transcript-es is still live after es was removed from the targets (have %v)", types)
	}
	if !types[ingest.TranscriptRepType("en")] {
		t.Errorf("transcript-en was retired, but en is still a configured target (have %v)", types)
	}
	if !types[ingest.RepTypeTranscript] {
		t.Errorf("the source transcript was retired (have %v)", types)
	}
	assertChunksTombstoned(t, st, esRepID)
}

// TestReconcile_DisablingTranslationRetiresEveryTranslation covers the whole
// capability going away. Both translated transcripts must go; the machine
// transcript stays, because STT still produces it.
func TestReconcile_DisablingTranslationRetiresEveryTranslation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	ctx := context.Background()

	first := translatingService(t, translateConfig(root, stateDir, "en", "es"), st)
	if err := first.Run(ctx); err != nil {
		t.Fatalf("first scan: %v", err)
	}

	// Second scan: translation switched off, and no translator wired.
	off := config.Config{RootDir: root, StateDir: stateDir, STTProvider: "off"}
	second := mustNewIngestService(t, off, st)
	second.SetTranscriber(&fakeTranscriber{text: "[00:00] intro\n[00:02] chapter one"})
	second.SetSTTIdentity("whisper", "whisper-large-v3")
	second.SetTranscriptLanguage("de")
	if err := second.Run(ctx); err != nil {
		t.Fatalf("second scan: %v", err)
	}

	types := activeRepTypes(t, st, "talk.mp3")
	for _, gone := range []string{ingest.TranscriptRepType("en"), ingest.TranscriptRepType("es")} {
		if types[gone] {
			t.Errorf("%q is still live after translation was switched off (have %v)", gone, types)
		}
	}
	if !types[ingest.RepTypeTranscript] {
		t.Errorf("the source transcript was retired (have %v)", types)
	}
}

// TestReconcile_UnresolvedTranslatorKeepsTranslations is the cost guard. A
// missing credential leaves translation enabled but unwired. That is NOT the
// operator asking for the translations to go away, and re-deriving them costs a
// paid provider call, so every existing translation must survive.
func TestReconcile_UnresolvedTranslatorKeepsTranslations(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	ctx := context.Background()

	first := translatingService(t, translateConfig(root, stateDir, "en", "es"), st)
	if err := first.Run(ctx); err != nil {
		t.Fatalf("first scan: %v", err)
	}

	// Second scan: translation still enabled, but the translator failed to build.
	second := mustNewIngestService(t, translateConfig(root, stateDir, "en", "es"), st)
	second.SetTranscriber(&fakeTranscriber{text: "[00:00] intro\n[00:02] chapter one"})
	second.SetSTTIdentity("whisper", "whisper-large-v3")
	second.SetTranscriptLanguage("de")
	if err := second.Run(ctx); err != nil {
		t.Fatalf("second scan: %v", err)
	}

	types := activeRepTypes(t, st, "talk.mp3")
	for _, want := range []string{ingest.TranscriptRepType("en"), ingest.TranscriptRepType("es")} {
		if !types[want] {
			t.Errorf("%q was retired because the translator did not resolve; paid output must survive a wiring failure (have %v)", want, types)
		}
	}
}

// TestReconcile_SidecarTranscriptSurvivesTranslationOff is the provenance
// guard. An authored subtitle sidecar persists under the same
// "transcript-<lang>" rep_type a translation uses. A sidecar is not model
// output, so no translation setting may ever retire it (§8.6.4).
func TestReconcile_SidecarTranscriptSurvivesTranslationOff(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	writeFile(t, filepath.Join(root, "talk.en.srt"),
		"1\n00:00:00,000 --> 00:00:02,000\nauthored line\n\n2\n00:00:02,000 --> 00:00:04,000\nsecond line\n")
	st := newRealStore(t)
	ctx := context.Background()

	// First scan: translation on, so the pipeline output identity records it.
	first := mustNewIngestService(t, translateConfig(root, stateDir, "fr"), st)
	first.SetTranslator(&fakeTranslator{}, "mistral", "mistral-small-2506", []string{"fr"})
	if err := first.Run(ctx); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if !activeRepTypes(t, st, "talk.mp3")[ingest.TranscriptRepType("en")] {
		t.Fatalf("first scan did not ingest the en sidecar (have %v)", activeRepTypes(t, st, "talk.mp3"))
	}

	// Second scan: translation off entirely. "en" is not a target and never was.
	second := mustNewIngestService(t, config.Config{RootDir: root, StateDir: stateDir, STTProvider: "off"}, st)
	if err := second.Run(ctx); err != nil {
		t.Fatalf("second scan: %v", err)
	}

	if !activeRepTypes(t, st, "talk.mp3")[ingest.TranscriptRepType("en")] {
		t.Errorf("the authored en sidecar transcript was retired by a translation setting (have %v)",
			activeRepTypes(t, st, "talk.mp3"))
	}
}

// TestReconcile_SteadyStateScanRetiresNothing pins the no-op case: rescanning
// with an unchanged pipeline leaves every output in place.
func TestReconcile_SteadyStateScanRetiresNothing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		svc := translatingService(t, translateConfig(root, stateDir, "en", "es"), st)
		if err := svc.Run(ctx); err != nil {
			t.Fatalf("scan %d: %v", i, err)
		}
	}

	types := activeRepTypes(t, st, "talk.mp3")
	for _, want := range []string{ingest.RepTypeTranscript, ingest.TranscriptRepType("en"), ingest.TranscriptRepType("es")} {
		if !types[want] {
			t.Errorf("steady-state rescan retired %q (have %v)", want, types)
		}
	}
}

// TestReconcile_DisabledSummariesAreRetired covers the second output type, and
// with it #686: while a stale `summary` vector stays live, a disabled
// hierarchical mode is not flat, because the summary keeps consuming result
// slots.
func TestReconcile_DisabledSummariesAreRetired(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := t.TempDir()
	writeFile(t, filepath.Join(root, "notes.txt"), "alpha beta gamma delta")
	st := newRealStore(t)
	ctx := context.Background()

	on := hierarchicalConfig(stateDir)
	on.RootDir = root
	on.STTProvider = "off"
	first := mustNewIngestService(t, on, st)
	first.SetSummarizer(&fakeSummarizer{}, "mistral", "mistral-small-latest")
	if err := first.Run(ctx); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if !activeRepTypes(t, st, "notes.txt")[model.SummaryRepType] {
		t.Fatalf("first scan produced no summary (have %v)", activeRepTypes(t, st, "notes.txt"))
	}
	summaryRepID := repIDForType(t, st, "notes.txt", model.SummaryRepType)

	// Second scan: hierarchical retrieval switched off, no summarizer wired.
	second := mustNewIngestService(t, config.Config{RootDir: root, StateDir: stateDir, STTProvider: "off"}, st)
	if err := second.Run(ctx); err != nil {
		t.Fatalf("second scan: %v", err)
	}

	types := activeRepTypes(t, st, "notes.txt")
	if types[model.SummaryRepType] {
		t.Errorf("the summary is still live after hierarchical retrieval was switched off (have %v)", types)
	}
	if !types[ingest.RepTypeRawText] {
		t.Errorf("the raw_text representation was retired (have %v)", types)
	}
	assertChunksTombstoned(t, st, summaryRepID)
}
