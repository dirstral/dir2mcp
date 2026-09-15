package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// translation cache tests (issue #267 item 2): the content-addressed artifact
// cache pattern (transcript/OCR) is extended to translation outputs so the same
// source file in two corpora derives the translation once (cross-corpus reuse),
// while a provider/model/target-language change MISSES the cache and never reads
// another derivation's cached bytes.

// TestTranslateCacheKey_SameIdentityStable locks cross-corpus reuse: the same
// source content + source text + translate identity (provider/model/target-lang)
// always yields the SAME cache key, even across two distinct Service instances
// (two corpora / two runs). A stable key is what lets a second corpus read the
// first corpus's cached translation instead of re-calling the chat provider.
func TestTranslateCacheKey_SameIdentityStable(t *testing.T) {
	t.Parallel()
	content := []byte("identical-audio-bytes")
	sourceText := "[00:00] one line"

	newSvc := func() *ingest.Service {
		st := &fakeIngestStore{}
		s := mustNewIngestService(t, config.Config{StateDir: t.TempDir()}, st)
		s.SetTranslator(&fakeTranslator{}, "mistral", "mistral-small-2506", []string{"en"})
		return s
	}

	corpusA := newSvc()
	corpusB := newSvc()

	keyA := corpusA.TranslateCacheKey(content, sourceText, "ru", "en")
	keyB := corpusB.TranslateCacheKey(content, sourceText, "ru", "en")

	if keyA != keyB {
		t.Fatalf("same source+identity must yield a stable cache key across corpora: %q != %q", keyA, keyB)
	}
}

// TestTranslateCacheKey_IdentityChangeMisses locks the no-cross-identity-bleed
// guarantee: a provider, model, or target-language change MUST land on a distinct
// cache key, so the new derivation never reads the previous one's cached bytes.
func TestTranslateCacheKey_IdentityChangeMisses(t *testing.T) {
	t.Parallel()
	content := []byte("audio-bytes")
	sourceText := "[00:00] one line"

	newSvc := func(prov, modelName string) *ingest.Service {
		st := &fakeIngestStore{}
		s := mustNewIngestService(t, config.Config{StateDir: t.TempDir()}, st)
		s.SetTranslator(&fakeTranslator{}, prov, modelName, []string{"en", "fr"})
		return s
	}

	base := newSvc("mistral", "m1")
	baseKey := base.TranslateCacheKey(content, sourceText, "ru", "en")

	// Same provider/model/target as base -> same key (sanity for the diffs below).
	if again := newSvc("mistral", "m1").TranslateCacheKey(content, sourceText, "ru", "en"); again != baseKey {
		t.Fatalf("identical identity must be stable: %q != %q", again, baseKey)
	}

	// Provider change must miss.
	if k := newSvc("openai", "m1").TranslateCacheKey(content, sourceText, "ru", "en"); k == baseKey {
		t.Fatalf("provider change must yield a distinct cache key (got %q for both)", k)
	}
	// Model change must miss.
	if k := newSvc("mistral", "m2").TranslateCacheKey(content, sourceText, "ru", "en"); k == baseKey {
		t.Fatalf("model change must yield a distinct cache key (got %q for both)", k)
	}
	// Target-language change must miss.
	if k := base.TranslateCacheKey(content, sourceText, "ru", "fr"); k == baseKey {
		t.Fatalf("target-language change must yield a distinct cache key (got %q for both)", k)
	}
	// Source-transcript-text change must miss (translation is of that text; an
	// upstream STT swap changing the source text must not reuse the old body).
	if k := base.TranslateCacheKey(content, "[00:00] DIFFERENT text", "ru", "en"); k == baseKey {
		t.Fatalf("source-text change must yield a distinct cache key (got %q for both)", k)
	}
}

// TestTranslateCacheKey_NoIdentityPathSeparatesTargets locks the disabled/absent
// identity path: with no resolved translate provider/model, the key falls back to
// {content, source-text, target-language} and two distinct targets still never
// collide, and the no-identity key differs from an identity-folded key.
func TestTranslateCacheKey_NoIdentityPathSeparatesTargets(t *testing.T) {
	t.Parallel()
	content := []byte("audio-bytes")
	sourceText := "[00:00] one line"

	// No SetTranslator -> empty translate identity -> bytes+text(+target) key.
	none := mustNewIngestService(t, config.Config{StateDir: t.TempDir()}, &fakeIngestStore{})

	kEn := none.TranslateCacheKey(content, sourceText, "ru", "en")
	kFr := none.TranslateCacheKey(content, sourceText, "ru", "fr")
	if kEn == kFr {
		t.Fatalf("no-identity path must still separate distinct targets (got %q for both)", kEn)
	}

	// An identity-folded key for the same source+target must differ from the
	// no-identity key (the identity is folded in).
	with := mustNewIngestService(t, config.Config{StateDir: t.TempDir()}, &fakeIngestStore{})
	with.SetTranslator(&fakeTranslator{}, "mistral", "m1", []string{"en"})
	if k := with.TranslateCacheKey(content, sourceText, "ru", "en"); k == kEn {
		t.Fatalf("identity-folded key must differ from no-identity key (got %q for both)", k)
	}
}

// TestTranslation_CrossCorpusReuse is the end-to-end analogue of the cache-key
// test: a translation computed by one corpus is reused (no chat-provider call) by
// a SECOND corpus pointed at the SAME stateDir + same source content + same
// translate identity — the cross-corpus "derive once" guarantee of #267 item 2.
func TestTranslation_CrossCorpusReuse(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	content := []byte("audio-bytes")

	run := func(tr *fakeTranslator) {
		s := mustNewIngestService(t, config.Config{StateDir: stateDir}, &fakeIngestStore{})
		s.SetTranscriber(&fakeTranscriber{text: "[00:00] one line"})
		s.SetTranscriptLanguage("de")
		s.SetTranslator(tr, "mistral", "m1", []string{"en"})
		doc := model.Document{DocID: 3, RelPath: "audio/clip.mp3", DocType: "audio"}
		if err := s.GenerateTranscriptRepresentation(context.Background(), doc, content); err != nil {
			t.Fatalf("GenerateTranscriptRepresentation: %v", err)
		}
	}

	first := &fakeTranslator{}
	run(first)
	if first.callCount() == 0 {
		t.Fatalf("expected translator called on the first corpus run")
	}

	// A different corpus (fresh Service, same stateDir) over the same source must
	// read the cached translation: zero chat-provider calls.
	second := &fakeTranslator{}
	run(second)
	if second.callCount() != 0 {
		t.Fatalf("expected cross-corpus cache reuse (0 translator calls), got %d", second.callCount())
	}

	// The cache file the second run reused must exist on disk under cache/translate.
	probe := mustNewIngestService(t, config.Config{StateDir: stateDir}, &fakeIngestStore{})
	probe.SetTranslator(&fakeTranslator{}, "mistral", "m1", []string{"en"})
	cachePath := filepath.Join(stateDir, "cache", "translate",
		probe.TranslateCacheKey(content, "[00:00] one line", "ru", "en")+ingest.TranscriptLangSuffix("en")+".txt")
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("expected translation cache file at %s: %v", cachePath, err)
	}
}

// TestTranslateCacheKey_NameHintModeMisses locks the prompt-shape guarantee
// (SPEC §8.6.7): the name-hint augmentation rewrites the translation prompt, so
// switching it on must MISS the cache rather than return the un-hinted
// translation produced before. It also locks the converse, which is why the key
// folds the hint GATE and not the raw source language: hints only reach the
// prompt for a Russian source and an English target, so with hints enabled but
// a source that cannot take them the prompt is byte-identical to the un-hinted
// one and the existing cache entry must still hit.
func TestTranslateCacheKey_NameHintModeMisses(t *testing.T) {
	t.Parallel()
	content := []byte("audio-bytes")
	sourceText := "[00:00] одна строка"

	newSvc := func(nameHints bool) *ingest.Service {
		s := mustNewIngestService(t, config.Config{
			StateDir:                t.TempDir(),
			MediaTranslateNameHints: nameHints,
		}, &fakeIngestStore{})
		s.SetTranslator(&fakeTranslator{}, "mistral", "m1", []string{"en", "fr"})
		return s
	}
	off, on := newSvc(false), newSvc(true)

	offKey := off.TranslateCacheKey(content, sourceText, "ru", "en")
	if onKey := on.TranslateCacheKey(content, sourceText, "ru", "en"); onKey == offKey {
		t.Fatalf("enabling name hints changes the prompt, so it must miss the cache (both %q)", onKey)
	}
	if k := on.TranslateCacheKey(content, sourceText, "uk", "en"); k != off.TranslateCacheKey(content, sourceText, "uk", "en") {
		t.Errorf("a non-Russian source cannot take hints; key must not change: %q", k)
	}
	if k := on.TranslateCacheKey(content, sourceText, "", "en"); k != off.TranslateCacheKey(content, sourceText, "", "en") {
		t.Errorf("an unknown source cannot take hints; key must not change: %q", k)
	}
	if k := on.TranslateCacheKey(content, sourceText, "ru", "fr"); k != off.TranslateCacheKey(content, sourceText, "ru", "fr") {
		t.Errorf("a non-English target cannot take hints; key must not change: %q", k)
	}
}
