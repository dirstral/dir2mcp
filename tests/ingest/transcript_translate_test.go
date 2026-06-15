package tests

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// fakeTranslator is a deterministic model.Generator stand-in for transcript
// translation tests: it appends a per-call tag so the translated text differs
// from the source, and counts Generate calls so cache reuse can be asserted.
type fakeTranslator struct {
	mu     sync.Mutex
	calls  int
	prompt string
}

func (f *fakeTranslator) Generate(_ context.Context, prompt string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.prompt = prompt
	// The prompt ends with the source line text after a blank line; echo a
	// translated marker plus the original so output is non-empty and traceable.
	parts := strings.Split(prompt, "\n\n")
	src := parts[len(parts)-1]
	return "translated:" + src, nil
}

func (f *fakeTranslator) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestTranscriptTranslation_Disabled_NoExtraReps confirms that with translation
// off (the default), only the single source transcript representation is
// produced — behaviour is identical to before the feature existed.
func TestTranscriptTranslation_Disabled_NoExtraReps(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir}, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] intro\n[00:02] chapter one"})
	// Translator left nil (translation disabled).

	doc := model.Document{DocID: 7, RelPath: "audio/talk.mp3", DocType: "audio"}
	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, []byte("audio")); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation: %v", err)
	}

	if len(st.reps) != 1 {
		t.Fatalf("expected exactly one transcript rep with translation off, got %d (%+v)", len(st.reps), st.reps)
	}
	if st.reps[0].RepType != ingest.RepTypeTranscript {
		t.Fatalf("expected source rep_type %q, got %q", ingest.RepTypeTranscript, st.reps[0].RepType)
	}
}

// TestTranscriptTranslation_ProducesPerLanguageRep verifies that enabling
// translation for target "en" produces a second transcript-en representation
// that is time-aligned to the source segments and records the translation
// derivation metadata.
func TestTranscriptTranslation_ProducesPerLanguageRep(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir}, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] intro\n[00:02] chapter one\n[00:05] chapter two"})
	tr := &fakeTranslator{}
	svc.SetTranscriptLanguage("de")
	svc.SetTranslator(tr, "mistral", "mistral-small-2506", []string{"en"})

	doc := model.Document{DocID: 9, RelPath: "audio/lecture.mp3", DocType: "audio"}
	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, []byte("audio")); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation: %v", err)
	}

	if len(st.reps) != 2 {
		t.Fatalf("expected source + translated transcript reps, got %d (%+v)", len(st.reps), st.reps)
	}

	var translated *model.Representation
	for i := range st.reps {
		if st.reps[i].RepType == ingest.TranscriptRepType("en") {
			translated = &st.reps[i]
		}
	}
	if translated == nil {
		t.Fatalf("expected a %q representation, got reps %+v", ingest.TranscriptRepType("en"), st.reps)
	}

	meta := translated.MetaJSON
	for _, want := range []string{
		`"source":"translation"`,
		`"language":"en"`,
		`"source_language":"de"`,
		`"translate_provider":"mistral"`,
		`"translate_model":"mistral-small-2506"`,
	} {
		if !strings.Contains(meta, want) {
			t.Fatalf("translated transcript meta_json missing %s: %s", want, meta)
		}
	}

	// Time-aligned: the translated transcript must produce the same three time
	// spans as the source. Source spans precede translated spans in insertion
	// order; assert the last three (translated) line up with the known source
	// windows: 0-2000, 2000-5000, 5000-6000.
	if len(st.spans) != 6 {
		t.Fatalf("expected 3 source + 3 translated time spans, got %d (%+v)", len(st.spans), st.spans)
	}
	translatedSpans := st.spans[3:]
	wantWindows := [][2]int{{0, 2000}, {2000, 5000}, {5000, 6000}}
	for i, w := range wantWindows {
		got := translatedSpans[i]
		if got.Kind != "time" || got.StartMS != w[0] || got.EndMS != w[1] {
			t.Fatalf("translated span %d not time-aligned: got %+v want kind=time start=%d end=%d", i, got, w[0], w[1])
		}
	}
}

// TestTranscriptTranslation_CacheReused verifies the per-language translation
// cache (TranscriptLangSuffix): a second run over the same source content reuses
// the cached translation instead of re-calling the chat provider.
func TestTranscriptTranslation_CacheReused(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	content := []byte("audio-bytes")

	run := func(tr *fakeTranslator) {
		st := &fakeIngestStore{}
		svc := mustNewIngestService(t, config.Config{StateDir: stateDir}, st)
		svc.SetTranscriber(&fakeTranscriber{text: "[00:00] one line"})
		svc.SetTranscriptLanguage("de")
		svc.SetTranslator(tr, "mistral", "m", []string{"en"})
		doc := model.Document{DocID: 3, RelPath: "audio/clip.mp3", DocType: "audio"}
		if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, content); err != nil {
			t.Fatalf("GenerateTranscriptRepresentation: %v", err)
		}
	}

	tr := &fakeTranslator{}
	run(tr)
	if tr.callCount() == 0 {
		t.Fatalf("expected translator to be called on first run")
	}
	firstCalls := tr.callCount()

	// Second run with the SAME stateDir/content: the translation must come from
	// cache, so Generate is not called again.
	tr2 := &fakeTranslator{}
	run(tr2)
	if tr2.callCount() != 0 {
		t.Fatalf("expected cached translation reused (0 calls) on second run, got %d", tr2.callCount())
	}
	_ = firstCalls
}

// TestTranscriptTranslation_RealStoreSearchable confirms the translated
// transcript is surfaced by store.TranscriptRepresentations alongside the source
// transcript when persisted to a real sqlite store.
func TestTranscriptTranslation_RealStoreSearchable(t *testing.T) {
	t.Parallel()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := mustNewIngestService(t, config.Config{StateDir: t.TempDir()}, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] hello\n[00:02] world"})
	svc.SetTranscriptLanguage("de")
	svc.SetTranslator(&fakeTranslator{}, "mistral", "m", []string{"en"})

	if err := st.UpsertDocument(context.Background(), model.Document{RelPath: "talk.mp3", DocType: "audio"}); err != nil {
		t.Fatalf("upsert document: %v", err)
	}
	doc, err := st.GetDocumentByPath(context.Background(), "talk.mp3")
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, []byte("audio")); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation: %v", err)
	}

	reps, err := st.TranscriptRepresentations(context.Background(), "talk.mp3")
	if err != nil {
		t.Fatalf("TranscriptRepresentations: %v", err)
	}
	if len(reps) != 2 {
		t.Fatalf("expected source + translated transcript reps in real store, got %d (%+v)", len(reps), reps)
	}
	var sawTranslation bool
	for _, r := range reps {
		if strings.Contains(r.MetaJSON, `"source":"translation"`) &&
			strings.Contains(r.MetaJSON, `"language":"en"`) {
			sawTranslation = true
		}
	}
	if !sawTranslation {
		t.Fatalf("translated transcript not surfaced by TranscriptRepresentations: %+v", reps)
	}
}

// degenerateTranslator is a model.Generator that returns a degenerate
// (repetition-loop) translation so the output quality gate quarantines it.
type degenerateTranslator struct{}

func (degenerateTranslator) Generate(_ context.Context, _ string) (string, error) {
	return strings.Repeat("merci ", 60), nil
}

// TestTranscriptTranslation_RoutesThroughQualityGate proves a translated
// transcript is screened by the output quality gate (anti-hallucination) like
// any model output — it does NOT bypass the gate the way sidecar transcripts do.
// A degenerate translation is quarantined (its chunks inserted error/quality_gate)
// while the clean source transcript's chunks remain pending.
func TestTranscriptTranslation_RoutesThroughQualityGate(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir, QualityGatesEnabled: true}, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] a clean source line\n[00:02] another clean line"})
	svc.SetTranscriptLanguage("de")
	svc.SetTranslator(degenerateTranslator{}, "mistral", "m", []string{"en"})

	doc := model.Document{DocID: 11, RelPath: "audio/talk.mp3", DocType: "audio"}
	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, []byte("audio")); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation: %v", err)
	}

	// Find the translated chunks (their rep is transcript-en, the second rep).
	if len(st.reps) != 2 {
		t.Fatalf("expected source + translated reps, got %d", len(st.reps))
	}
	var translatedRepID int64
	for i := range st.reps {
		if st.reps[i].RepType == ingest.TranscriptRepType("en") {
			translatedRepID = int64(i + 1) // fakeIngestStore assigns rep IDs sequentially from 1
		}
	}
	if translatedRepID == 0 {
		t.Fatalf("translated rep not found")
	}

	var sawQuarantined, sawPending bool
	for _, c := range st.chunks {
		if c.RepID == translatedRepID {
			if c.EmbeddingStatus == "error" && c.ErrorCategory == string(store.ErrorCategoryQualityGate) {
				sawQuarantined = true
			}
		} else if c.EmbeddingStatus == "pending" {
			sawPending = true
		}
	}
	if !sawQuarantined {
		t.Fatalf("expected degenerate translated chunks to be quarantined by the quality gate; chunks=%+v", st.chunks)
	}
	if !sawPending {
		t.Fatalf("expected clean source transcript chunks to remain pending; chunks=%+v", st.chunks)
	}
}

// TestTranscriptTranslation_ValidationRejectsEmptyTargets verifies that enabling
// translation with an empty target_langs is CONFIG_INVALID, while leaving it off
// is valid (general-purpose: no default target language).
func TestTranscriptTranslation_ValidationRejectsEmptyTargets(t *testing.T) {
	t.Parallel()

	enabledNoTargets := config.Default()
	enabledNoTargets.MediaTranslateEnabled = true
	enabledNoTargets.MediaTranslateTargetLangs = nil
	if err := enabledNoTargets.Validate(); err == nil {
		t.Fatalf("expected CONFIG_INVALID when translation enabled with empty target_langs")
	}

	enabledBlankTargets := config.Default()
	enabledBlankTargets.MediaTranslateEnabled = true
	enabledBlankTargets.MediaTranslateTargetLangs = []string{"  ", ""}
	if err := enabledBlankTargets.Validate(); err == nil {
		t.Fatalf("expected CONFIG_INVALID when translation enabled with all-blank target_langs")
	}

	enabledWithTargets := config.Default()
	enabledWithTargets.MediaTranslateEnabled = true
	enabledWithTargets.MediaTranslateTargetLangs = []string{"EN", "en", "fr"}
	if err := enabledWithTargets.Validate(); err != nil {
		t.Fatalf("expected valid config with targets, got %v", err)
	}
	if got := enabledWithTargets.MediaTranslateTargetLangs; len(got) != 2 || got[0] != "en" || got[1] != "fr" {
		t.Fatalf("expected normalized de-duped lower-cased targets [en fr], got %v", got)
	}

	off := config.Default()
	off.MediaTranslateEnabled = false
	off.MediaTranslateTargetLangs = nil
	if err := off.Validate(); err != nil {
		t.Fatalf("translation off must be valid, got %v", err)
	}
}
