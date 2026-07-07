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

// The whisper translation engine (media.translate.engine=whisper) sources the
// English transcript from a second Whisper pass with task=translate instead of
// the chat generator. Because Whisper RE-SEGMENTS the audio, the English track
// has its OWN timings — NOT a 1:1 copy of the source segment windows. These
// tests inject a fake translate-transcriber via SetTranslateTranscriber (which
// stands in for the whisperapi client running task=translate).

// TestWhisperTranslate_ProducesEnRepWithOwnTimings verifies the whisper engine
// produces a transcript-en representation whose time spans come from the
// translate pass's OWN [mm:ss] markers (re-segmentation), distinct in count and
// windows from the source transcript, and records the STT provider/model as the
// translation derivation identity.
func TestWhisperTranslate_ProducesEnRepWithOwnTimings(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir}, st)
	// Source: three Russian segments. Translate pass: TWO English segments with
	// different windows — the re-segmentation case Whisper actually produces.
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] привет\n[00:02] как дела\n[00:05] пока"})
	svc.SetTranscriptLanguage("ru")
	svc.SetTranslateTranscriber(
		&fakeTranscriber{text: "[00:00] hello there\n[00:03] goodbye"},
		"whisper", "large-v3", []string{"en"})

	doc := model.Document{DocID: 9, RelPath: "audio/interview.mp3", DocType: "audio"}
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
		`"source_language":"ru"`,
		`"translate_provider":"whisper"`,
		`"translate_model":"large-v3"`,
	} {
		if !strings.Contains(meta, want) {
			t.Fatalf("translated transcript meta_json missing %s: %s", want, meta)
		}
	}

	// Re-segmentation: source produced 3 spans, the whisper-translate pass
	// produced its OWN 2 spans (windows 0-3000, 3000-4000) — NOT the source
	// windows. Source spans precede translated spans in insertion order.
	if len(st.spans) != 5 {
		t.Fatalf("expected 3 source + 2 translated time spans, got %d (%+v)", len(st.spans), st.spans)
	}
	translatedSpans := st.spans[3:]
	wantWindows := [][2]int{{0, 3000}, {3000, 4000}}
	for i, w := range wantWindows {
		got := translatedSpans[i]
		if got.Kind != "time" || got.StartMS != w[0] || got.EndMS != w[1] {
			t.Fatalf("translated span %d not from the translate pass markers: got %+v want kind=time start=%d end=%d", i, got, w[0], w[1])
		}
	}
}

// TestWhisperTranslate_CarriesWordTimings verifies that when the translate
// transcriber is structured (returns per-word timings), those words are attached
// to the English track's time spans — so the translated track can be
// broadcast-segmented, not just chunk-segmented like the source track.
func TestWhisperTranslate_CarriesWordTimings(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir}, st)
	// Source: one Russian segment (1 span). Translate pass: two English segments
	// carrying per-word timings that fall inside each segment window.
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] привет как дела"})
	svc.SetTranscriptLanguage("ru")
	svc.SetTranslateTranscriber(
		&fakeStructuredTranscriber{
			text: "[00:00] hello there\n[00:03] goodbye friend",
			words: []model.TimedWord{
				{Word: "hello", StartMS: 0, EndMS: 500},
				{Word: "there", StartMS: 500, EndMS: 1000},
				{Word: "goodbye", StartMS: 3000, EndMS: 3500},
				{Word: "friend", StartMS: 3500, EndMS: 3900},
			},
		},
		"whisper", "large-v3", []string{"en"})

	doc := model.Document{DocID: 11, RelPath: "audio/interview.mp3", DocType: "audio"}
	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, []byte("audio")); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation: %v", err)
	}

	// Source produced 1 span; the translate pass produced its own 2 spans, which
	// must carry the 4 injected words between them.
	if len(st.spans) != 3 {
		t.Fatalf("expected 1 source + 2 translated spans, got %d (%+v)", len(st.spans), st.spans)
	}
	translatedSpans := st.spans[1:]
	total := 0
	for _, sp := range translatedSpans {
		for _, w := range sp.Words {
			total++
			if w.T < sp.StartMS || w.T >= sp.EndMS {
				t.Errorf("translated word %q at %dms outside span [%d,%d)", w.W, w.T, sp.StartMS, sp.EndMS)
			}
		}
	}
	if total != 4 {
		t.Fatalf("translated spans carry %d words, want 4 (broadcast segmentation needs them)", total)
	}
}

// TestWhisperTranslate_CacheReused verifies the whisper-translate output is
// cached (under the "-translate" discriminated key) so a second ingest of the
// same source bytes does NOT re-invoke the translate pass.
func TestWhisperTranslate_CacheReused(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	content := []byte("audio-bytes")

	run := func(tr *fakeTranscriber) {
		st := &fakeIngestStore{}
		svc := mustNewIngestService(t, config.Config{StateDir: stateDir}, st)
		svc.SetTranscriber(&fakeTranscriber{text: "[00:00] привет"})
		svc.SetTranscriptLanguage("ru")
		svc.SetTranslateTranscriber(tr, "whisper", "large-v3", []string{"en"})
		doc := model.Document{DocID: 3, RelPath: "audio/clip.mp3", DocType: "audio"}
		if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, content); err != nil {
			t.Fatalf("GenerateTranscriptRepresentation: %v", err)
		}
	}

	tr := &fakeTranscriber{text: "[00:00] hello"}
	run(tr)
	if tr.calls == 0 {
		t.Fatalf("expected translate pass to be called on first run")
	}

	// Second run with the SAME stateDir/content: the English track must come from
	// cache, so the translate pass is not called again.
	tr2 := &fakeTranscriber{text: "[00:00] hello"}
	run(tr2)
	if tr2.calls != 0 {
		t.Fatalf("expected cached whisper translation reused (0 calls) on second run, got %d", tr2.calls)
	}
}

// TestWhisperTranslate_FailureIsNonFatal proves a translate-pass failure does
// NOT fail document ingest and does NOT prevent the authoritative source
// transcript from being persisted (best-effort enrichment), matching the chat
// engine's failure semantics.
func TestWhisperTranslate_FailureIsNonFatal(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir}, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] привет\n[00:02] пока"})
	svc.SetTranscriptLanguage("ru")
	svc.SetTranslateTranscriber(
		&fakeTranscriber{err: errors.New("whisper server unavailable")},
		"whisper", "large-v3", []string{"en"})

	doc := model.Document{DocID: 21, RelPath: "audio/talk.mp3", DocType: "audio"}
	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, []byte("audio")); err != nil {
		t.Fatalf("whisper translate failure must be non-fatal, got error: %v", err)
	}

	if len(st.reps) != 1 {
		t.Fatalf("expected only the source transcript rep to survive a failed translation, got %d (%+v)", len(st.reps), st.reps)
	}
	if st.reps[0].RepType != ingest.RepTypeTranscript {
		t.Fatalf("expected surviving rep to be the source transcript %q, got %q", ingest.RepTypeTranscript, st.reps[0].RepType)
	}
}

// TestWhisperTranslate_RoutesThroughQualityGate proves the whisper-translated
// English transcript is screened by the output quality gate like any model
// output: a degenerate (repetition-loop) translate pass is quarantined while the
// clean source transcript's chunks remain pending.
func TestWhisperTranslate_RoutesThroughQualityGate(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir, QualityGatesEnabled: true}, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] чистая строка текста\n[00:02] ещё одна чистая строка"})
	svc.SetTranscriptLanguage("ru")
	// Degenerate English output (repetition loop) — the exact failure mode the
	// gate exists to catch on whisper-translate runs.
	svc.SetTranslateTranscriber(
		&fakeTranscriber{text: "[00:00] " + strings.Repeat("no ", 60)},
		"whisper", "large-v3", []string{"en"})

	doc := model.Document{DocID: 11, RelPath: "audio/talk.mp3", DocType: "audio"}
	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, []byte("audio")); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation: %v", err)
	}

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
		t.Fatalf("expected degenerate whisper-translated chunks to be quarantined by the quality gate; chunks=%+v", st.chunks)
	}
	if !sawPending {
		t.Fatalf("expected clean source transcript chunks to remain pending; chunks=%+v", st.chunks)
	}
}

// TestWhisperTranslate_AppliesFilterWords pins that media.filter_words is applied
// to the translated English track (#538 routed the translate path through the
// filtered chunker, matching the source track). A pure-boilerplate translated
// line is dropped entirely, so the translate pass yields fewer spans than its
// input lines — before #538 the unfiltered chunker would have kept all three.
func TestWhisperTranslate_AppliesFilterWords(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir, MediaFilterWords: []string{"credits roll"}}, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] привет"})
	svc.SetTranscriptLanguage("ru")
	// Translated English track: the middle line is pure boilerplate the filter
	// must strip (and thereby drop its span); the other two survive.
	svc.SetTranslateTranscriber(
		&fakeTranscriber{text: "[00:00] hello there\n[00:03] credits roll\n[00:05] goodbye friend"},
		"whisper", "large-v3", []string{"en"})

	doc := model.Document{DocID: 12, RelPath: "audio/clip.mp3", DocType: "audio"}
	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, []byte("audio")); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation: %v", err)
	}

	// 1 source span + the translated spans. The boilerplate-only "credits roll"
	// line is dropped, so the translate pass yields 2 spans (not 3).
	if len(st.spans) != 3 {
		t.Fatalf("expected 1 source + 2 filtered translated spans, got %d (%+v)", len(st.spans), st.spans)
	}
}

// TestWhisperTranslate_PurgeRemovesTranslateCache proves PurgeTranscriptCache
// deletes the whisper-translate English track AND its .words.json sidecar, not
// just the source transcript cache. Translated text can be secret-gated, so a
// purge that left it on disk under {StateDir}/cache/transcribe would leak
// refused content. A structured translate transcriber is used so the sidecar is
// actually written.
func TestWhisperTranslate_PurgeRemovesTranslateCache(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	content := []byte("audio-bytes-to-purge")
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir}, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] привет"})
	svc.SetTranscriptLanguage("ru")
	svc.SetTranslateTranscriber(
		&fakeStructuredTranscriber{
			text: "[00:00] hello there\n[00:03] goodbye friend",
			words: []model.TimedWord{
				{Word: "hello", StartMS: 0, EndMS: 500},
				{Word: "goodbye", StartMS: 3000, EndMS: 3500},
			},
		},
		"whisper", "large-v3", []string{"en"})

	doc := model.Document{DocID: 21, RelPath: "audio/interview.mp3", DocType: "audio"}
	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, content); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation: %v", err)
	}

	cacheDir := filepath.Join(stateDir, "cache", "transcribe")
	txt, err := filepath.Glob(filepath.Join(cacheDir, "*-translate*.txt"))
	if err != nil {
		t.Fatalf("glob translate txt: %v", err)
	}
	words, err := filepath.Glob(filepath.Join(cacheDir, "*-translate*.words.json"))
	if err != nil {
		t.Fatalf("glob translate words: %v", err)
	}
	if len(txt) == 0 || len(words) == 0 {
		entries, _ := os.ReadDir(cacheDir)
		t.Fatalf("expected translate cache text+words on disk before purge, got txt=%v words=%v (dir=%v)", txt, words, entries)
	}

	// Purge on the SOURCE language: the translate track (keyed on "en"
	// unconditionally) must still be removed.
	svc.PurgeTranscriptCache(content, "ru")

	for _, p := range append(append([]string{}, txt...), words...) {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("translate cache file %s survived purge (stat err=%v)", p, err)
		}
	}
}
