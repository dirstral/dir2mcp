package tests

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// queryAwareEmbedder maps a query string to a fixed vector so a translated
// query variant retrieves a different chunk than the original. The first text
// is treated as the query (the service embeds exactly one query string per
// search). An unmapped query falls back to {1, 0}.
type queryAwareEmbedder struct {
	vecByQuery map[string][]float32
}

func (e *queryAwareEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		vec, ok := e.vecByQuery[strings.TrimSpace(t)]
		if !ok {
			vec = []float32{1, 0}
		}
		clone := make([]float32, len(vec))
		copy(clone, vec)
		out[i] = clone
	}
	return out, nil
}

// vectorRoutingIndex returns ONLY the labels mapped to the queried vector, so a
// per-language query variant deterministically retrieves a disjoint result set.
// Routing is keyed on the first vector component: 1 ⇒ EN labels, 0 ⇒ RU labels.
// Unlike a real ANN index it does not surface orthogonal candidates, which keeps
// the cross-lingual fusion assertions unambiguous (the base query alone never
// returns the other language's doc).
type vectorRoutingIndex struct {
	enLabels []uint64
	ruLabels []uint64
}

func (v *vectorRoutingIndex) Upsert(context.Context, []float32, model.IndexPayload) error {
	return nil
}
func (v *vectorRoutingIndex) Delete(context.Context, []uint64) error { return nil }
func (v *vectorRoutingIndex) Search(_ context.Context, vec []float32, k int, _ model.Filter) ([]model.IndexHit, error) {
	labels := v.ruLabels
	if len(vec) > 0 && vec[0] >= 0.5 {
		labels = v.enLabels
	}
	out := make([]model.IndexHit, 0, len(labels))
	for i, l := range labels {
		if i >= k {
			break
		}
		out = append(out, model.IndexHit{ChunkID: l, Score: 1.0})
	}
	return out, nil
}
func (v *vectorRoutingIndex) Identity(context.Context) (string, error) { return "", nil }
func (v *vectorRoutingIndex) Reset(context.Context, string) error      { return nil }
func (v *vectorRoutingIndex) Close() error                             { return nil }

// fakeTranslator is a model.Generator that returns a canned translation keyed by
// the target language embedded in the prompt, simulating the chat translate
// primitive without credentials. errForLang, when set for a language, makes that
// translation fail (to exercise graceful degradation). callsByLang records how
// many times each target language was requested.
type fakeTranslator struct {
	byLang     map[string]string
	errForLang map[string]error
	callsByLang map[string]int
}

func (f *fakeTranslator) Generate(_ context.Context, prompt string) (string, error) {
	lang := ""
	for l := range f.byLang {
		if strings.Contains(prompt, " into "+l+".") {
			lang = l
			break
		}
	}
	if lang == "" {
		for l := range f.errForLang {
			if strings.Contains(prompt, " into "+l+".") {
				lang = l
				break
			}
		}
	}
	if f.callsByLang == nil {
		f.callsByLang = map[string]int{}
	}
	f.callsByLang[lang]++
	if err, ok := f.errForLang[lang]; ok {
		return "", err
	}
	return f.byLang[lang], nil
}

func searchRelPaths(t *testing.T, svc *retrieval.Service, q model.SearchQuery) []string {
	t.Helper()
	hits, err := svc.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.RelPath)
	}
	return out
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// newCrossLingualService builds a vector-only service with two chunks that match
// disjoint query vectors: the EN query vector {1, 0} surfaces the EN doc, and the
// translated RU query vector {0, 1} surfaces the RU doc. The fake translator maps
// the EN query to its RU form.
func newCrossLingualService(t *testing.T) *retrieval.Service {
	t.Helper()
	idx := &vectorRoutingIndex{enLabels: []uint64{1}, ruLabels: []uint64{2}}

	emb := &queryAwareEmbedder{vecByQuery: map[string][]float32{
		"sanctions": {1, 0}, // original EN query
		"санкции":   {0, 1}, // RU translation of the query
	}}
	svc := retrieval.NewService(nil, idx, emb, nil)
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "en.md", DocType: "md", Snippet: "english", Language: "en"})
	svc.SetChunkMetadata(2, model.SearchHit{ChunkID: 2, RelPath: "ru.md", DocType: "md", Snippet: "russian", Language: "ru"})
	svc.SetCorpusLanguagesProvider(func() []string { return []string{"en", "ru"} })
	return svc
}

// TestCrossLingual_FusesMultiLanguageHits is the core behavior: an EN query with
// cross-lingual expansion enabled (auto targets) surfaces BOTH the EN doc (from
// the original query) and the RU doc (from the translated variant), fused via RRF.
func TestCrossLingual_FusesMultiLanguageHits(t *testing.T) {
	tr := &fakeTranslator{byLang: map[string]string{"ru": "санкции"}}
	svc := newCrossLingualService(t)
	svc.SetCrossLingual(true, nil /* auto */, tr)

	paths := searchRelPaths(t, svc, model.SearchQuery{Query: "sanctions", K: 10})
	if !containsPath(paths, "en.md") {
		t.Fatalf("expected en.md (original query hit) in results, got %v", paths)
	}
	if !containsPath(paths, "ru.md") {
		t.Fatalf("expected ru.md (cross-lingual variant hit) in results, got %v", paths)
	}
}

// TestCrossLingual_ExplicitTargetLangs pins that an explicit target list (not
// "auto") drives the same fusion without needing a corpus-languages provider.
func TestCrossLingual_ExplicitTargetLangs(t *testing.T) {
	tr := &fakeTranslator{byLang: map[string]string{"ru": "санкции"}}
	idx := &vectorRoutingIndex{enLabels: []uint64{1}, ruLabels: []uint64{2}}
	emb := &queryAwareEmbedder{vecByQuery: map[string][]float32{
		"sanctions": {1, 0},
		"санкции":   {0, 1},
	}}
	svc := retrieval.NewService(nil, idx, emb, nil)
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "en.md", DocType: "md", Snippet: "english", Language: "en"})
	svc.SetChunkMetadata(2, model.SearchHit{ChunkID: 2, RelPath: "ru.md", DocType: "md", Snippet: "russian", Language: "ru"})
	// No corpus-languages provider registered: explicit list must still work.
	svc.SetCrossLingual(true, []string{"ru"}, tr)

	paths := searchRelPaths(t, svc, model.SearchQuery{Query: "sanctions", K: 10})
	if !containsPath(paths, "en.md") || !containsPath(paths, "ru.md") {
		t.Fatalf("expected both en.md and ru.md with explicit target_langs, got %v", paths)
	}
}

// TestCrossLingual_DisabledIsUnchanged pins that with the feature off, the search
// returns only the original-query hit and never calls the translator.
func TestCrossLingual_DisabledIsUnchanged(t *testing.T) {
	tr := &fakeTranslator{byLang: map[string]string{"ru": "санкции"}}
	svc := newCrossLingualService(t)
	// Cross-lingual not enabled.

	paths := searchRelPaths(t, svc, model.SearchQuery{Query: "sanctions", K: 10})
	if containsPath(paths, "ru.md") {
		t.Fatalf("disabled cross-lingual must not surface ru.md, got %v", paths)
	}
	if !containsPath(paths, "en.md") {
		t.Fatalf("expected en.md from the original query, got %v", paths)
	}
	if len(tr.callsByLang) != 0 {
		t.Fatalf("translator must not be called when cross-lingual is disabled, got %v", tr.callsByLang)
	}
}

// TestCrossLingual_FailingTranslationSkipped pins graceful degradation: when the
// translation for a target language errors, that variant is skipped (not fatal),
// and the search still returns the original-query hit.
func TestCrossLingual_FailingTranslationSkipped(t *testing.T) {
	tr := &fakeTranslator{
		byLang:     map[string]string{},
		errForLang: map[string]error{"ru": errors.New("translate boom")},
	}
	svc := newCrossLingualService(t)
	svc.SetCrossLingual(true, nil, tr)

	paths := searchRelPaths(t, svc, model.SearchQuery{Query: "sanctions", K: 10})
	if !containsPath(paths, "en.md") {
		t.Fatalf("a failing translation must not fail the search; expected en.md, got %v", paths)
	}
	if containsPath(paths, "ru.md") {
		t.Fatalf("the failed RU variant must be skipped, got %v", paths)
	}
	if tr.callsByLang["ru"] == 0 {
		t.Fatalf("expected an attempted RU translation, got %v", tr.callsByLang)
	}
}

// TestCrossLingual_SkipsQueryLanguage pins that the detected query language (here
// Cyrillic ⇒ ru) is never re-translated: with a RU query and auto targets
// {en, ru}, only EN is requested.
func TestCrossLingual_SkipsQueryLanguage(t *testing.T) {
	tr := &fakeTranslator{byLang: map[string]string{"en": "sanctions", "ru": "санкции"}}
	svc := newCrossLingualService(t)
	svc.SetCrossLingual(true, nil, tr)

	// RU query: detectQueryLanguage(Cyrillic) ⇒ ru, so ru is skipped.
	_ = searchRelPaths(t, svc, model.SearchQuery{Query: "санкции", K: 10})
	if tr.callsByLang["ru"] != 0 {
		t.Fatalf("must not translate a RU query into RU (self-translation), got %v", tr.callsByLang)
	}
	if tr.callsByLang["en"] == 0 {
		t.Fatalf("expected the RU query to be translated into EN, got %v", tr.callsByLang)
	}
}

// TestCrossLingual_NilTranslatorIsInert pins that enabling the feature without a
// translator leaves search unchanged (no panic, no expansion).
func TestCrossLingual_NilTranslatorIsInert(t *testing.T) {
	svc := newCrossLingualService(t)
	svc.SetCrossLingual(true, []string{"ru"}, nil)

	paths := searchRelPaths(t, svc, model.SearchQuery{Query: "sanctions", K: 10})
	if containsPath(paths, "ru.md") {
		t.Fatalf("nil translator must not expand the query, got %v", paths)
	}
	if !containsPath(paths, "en.md") {
		t.Fatalf("expected en.md from the original query, got %v", paths)
	}
}
