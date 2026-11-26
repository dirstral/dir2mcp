package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Spec 0.102.0 (SPEC §8.9.4) adds two requirements to the recognition path:
//
//   - a wire `derivation` of "generated" reaches the span, an "observed" or
//     absent one stores nothing (absent means observed), and any other value
//     drops the annotation;
//   - the entity dictionary entries that a persisted annotation references are
//     kept in the representation's meta_json.
//
// Both join the rep_hash only when present, so a corpus recognized before this
// change keeps its hash and is not recognized again.

func derived(text, derivation string, start int) model.RecognizedAnnotation {
	return model.RecognizedAnnotation{
		StartMS: start, EndMS: start + 1000, Text: text, Event: "caption",
		Derivation: derivation,
	}
}

func TestRecognition0102_GeneratedDerivationReachesTheSpan(t *testing.T) {
	segments, _ := ingest.RecognitionSegments([]model.RecognizedAnnotation{
		derived("a presenter at a desk", "generated", 0),
		derived("ticker reads BREAKING", "observed", 2000),
		derived("a chyron with a name", "", 4000),
	})
	if len(segments) != 3 {
		t.Fatalf("expected 3 segments, got %d", len(segments))
	}
	want := []string{"generated", "", ""}
	for i, w := range want {
		if got := segments[i].Span.Derivation; got != w {
			t.Fatalf("segment %d derivation = %q, want %q (absent means observed)", i, got, w)
		}
	}
}

func TestRecognition0102_UnknownDerivationDropsOnlyThatAnnotation(t *testing.T) {
	segments, _ := ingest.RecognitionSegments([]model.RecognizedAnnotation{
		derived("kept before", "", 0),
		derived("bad value", "inferred", 2000),
		derived("kept after", "generated", 4000),
	})
	if len(segments) != 2 {
		t.Fatalf("expected the invalid annotation dropped and its siblings kept, got %d", len(segments))
	}
	for _, seg := range segments {
		if seg.Text == "bad value" {
			t.Fatalf("an annotation with derivation %q was stored", "inferred")
		}
	}
}

// TestRecognition0102_ObservedKeepsTheEarlierHash: an observed or absent
// derivation must leave the hash input byte-identical to what the code
// produced before it read the field; generated must change it.
func TestRecognition0102_ObservedKeepsTheEarlierHash(t *testing.T) {
	_, absent := ingest.RecognitionSegments([]model.RecognizedAnnotation{derived("same", "", 0)})
	_, observed := ingest.RecognitionSegments([]model.RecognizedAnnotation{derived("same", "observed", 0)})
	_, generated := ingest.RecognitionSegments([]model.RecognizedAnnotation{derived("same", "generated", 0)})
	if absent != observed {
		t.Fatalf("observed changed the hash input:\n%q\n%q", absent, observed)
	}
	if strings.Contains(absent, "|D") {
		t.Fatalf("an annotation without derivation gained a derivation group: %q", absent)
	}
	if absent == generated {
		t.Fatal("a generated annotation hashes like an observed one, so a changed derivation would not re-derive")
	}
}

func dictSegments(t *testing.T, ids ...[]string) []ingest.ChunkSegment {
	t.Helper()
	anns := make([]model.RecognizedAnnotation, 0, len(ids))
	for i, e := range ids {
		anns = append(anns, model.RecognizedAnnotation{StartMS: i * 1000, EndMS: i*1000 + 500, Text: "statement", Entities: e})
	}
	segments, _ := ingest.RecognitionSegments(anns)
	return segments
}

func TestRecognition0102_DictionaryKeepsOnlyReferencedEntries(t *testing.T) {
	segments := dictSegments(t, []string{"person:b"}, []string{"person:a", "person:undefined"})
	kept, hash := ingest.RecognitionEntityDictionary([]model.RecognizedEntity{
		{ID: "person:z", Label: "Not referenced"},
		{ID: "person:b", Label: "Bea", Aliases: []string{" B ", "B", ""}},
		{ID: " ", Label: "empty id"},
		{ID: "person:a", Label: ""},
		{ID: "person:a", Label: "Ann"},
		{ID: "person:b", Label: "second definition is ignored"},
	}, segments)
	got, _ := json.Marshal(kept)
	want := `[{"id":"person:a","label":"Ann"},{"id":"person:b","label":"Bea","aliases":["B"]}]`
	if string(got) != want {
		t.Fatalf("dictionary = %s\nwant       %s", got, want)
	}
	if !strings.HasPrefix(hash, "E|2|") {
		t.Fatalf("dictionary hash line = %q, want an E-tagged line with 2 entries", hash)
	}
}

func TestRecognition0102_NoKeptEntryAddsNothingToTheHash(t *testing.T) {
	for name, tc := range map[string]struct {
		segments []ingest.ChunkSegment
		defs     []model.RecognizedEntity
	}{
		"no dictionary":         {dictSegments(t, []string{"person:a"}), nil},
		"no referenced entry":   {dictSegments(t, []string{"person:a"}), []model.RecognizedEntity{{ID: "person:z", Label: "Z"}}},
		"no annotation with id": {dictSegments(t, nil), []model.RecognizedEntity{{ID: "person:a", Label: "A"}}},
	} {
		t.Run(name, func(t *testing.T) {
			kept, hash := ingest.RecognitionEntityDictionary(tc.defs, tc.segments)
			if kept != nil || hash != "" {
				t.Fatalf("kept=%v hash=%q, want nothing (the append-only rule of §8.9.4)", kept, hash)
			}
		})
	}
}

func TestRecognition0102_DictionaryOrderAndLabelsAffectTheHashCorrectly(t *testing.T) {
	segments := dictSegments(t, []string{"person:a", "person:b"})
	a := model.RecognizedEntity{ID: "person:a", Label: "Ann"}
	b := model.RecognizedEntity{ID: "person:b", Label: "Bea"}
	_, h1 := ingest.RecognitionEntityDictionary([]model.RecognizedEntity{a, b}, segments)
	_, h2 := ingest.RecognitionEntityDictionary([]model.RecognizedEntity{b, a}, segments)
	if h1 != h2 {
		t.Fatalf("the response order changed the dictionary hash:\n%q\n%q", h1, h2)
	}
	renamed := model.RecognizedEntity{ID: "person:a", Label: "Anna"}
	_, h3 := ingest.RecognitionEntityDictionary([]model.RecognizedEntity{renamed, b}, segments)
	if h1 == h3 {
		t.Fatal("a changed label left the hash unchanged, so the stored dictionary would go stale")
	}
	// Length prefixes keep field boundaries: a label that contains a delimiter
	// must not collide with a different split of the same characters.
	_, x := ingest.RecognitionEntityDictionary([]model.RecognizedEntity{{ID: "person:a", Label: "A|1:B"}}, dictSegments(t, []string{"person:a"}))
	_, y := ingest.RecognitionEntityDictionary([]model.RecognizedEntity{{ID: "person:a", Label: "A", Aliases: []string{"B"}}}, dictSegments(t, []string{"person:a"}))
	if x == y {
		t.Fatalf("two different dictionaries share a hash input: %q", x)
	}
}

func TestRecognition0102_RepresentationStoresDictionaryAndKeepsHashWithout(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, result model.RecognizeResult) model.Representation {
		t.Helper()
		root := testutil.TempDir(t)
		writeFile(t, filepath.Join(root, "clip.mp4"), "fake-video")
		st := &fakeIngestStore{}
		svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: testutil.TempDir(t)}, st)
		svc.SetRecognizer(&fakeRecognizer{result: result})
		doc := model.Document{DocID: 1, RelPath: "clip.mp4", DocType: "video"}
		if err := svc.GenerateRecognitionRepresentation(context.Background(), doc); err != nil {
			t.Fatalf("GenerateRecognitionRepresentation: %v", err)
		}
		if len(st.reps) != 1 {
			t.Fatalf("expected one representation, got %d", len(st.reps))
		}
		return st.reps[0]
	}
	base := recognizeTestResult()
	plain := run(t, base)
	if strings.Contains(plain.MetaJSON, `"entities"`) {
		t.Fatalf("a response without a dictionary stored one: %s", plain.MetaJSON)
	}

	unreferenced := recognizeTestResult()
	unreferenced.Entities = []model.RecognizedEntity{{ID: "player:nobody", Label: "Nobody"}}
	if got := run(t, unreferenced); got.RepHash != plain.RepHash || strings.Contains(got.MetaJSON, `"entities"`) {
		t.Fatalf("an unreferenced dictionary changed the representation: hash %s vs %s, meta %s", got.RepHash, plain.RepHash, got.MetaJSON)
	}

	named := recognizeTestResult()
	named.Entities = []model.RecognizedEntity{{ID: "player:webb-logan", Label: "Logan Webb", Aliases: []string{"#62"}}}
	got := run(t, named)
	if !strings.Contains(got.MetaJSON, `"entities":[{"id":"player:webb-logan","label":"Logan Webb","aliases":["#62"]}]`) {
		t.Fatalf("meta_json does not keep the referenced dictionary entry: %s", got.MetaJSON)
	}
	if got.RepHash == plain.RepHash {
		t.Fatal("a kept dictionary did not change the rep_hash, so a later label change would not re-derive")
	}
}

func TestRecognition0102_ServeClientReadsDictionaryAndDerivation(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"recognizer": {"name": "test-backend", "version": "1"},
			"entities": [{"id": "person:a", "label": "Ann", "aliases": ["A."]}],
			"annotations": [
				{"start_s": 1.0, "end_s": 2.0, "text": "a woman at a desk", "entities": ["person:a"], "derivation": "generated"}
			]
		}`))
	}))
	defer srv.Close()
	res, err := ingest.NewRecognizeServeClient(srv.URL).Recognize(context.Background(), "/abs/clip.mp4")
	if err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	if len(res.Entities) != 1 || res.Entities[0].ID != "person:a" || res.Entities[0].Label != "Ann" ||
		len(res.Entities[0].Aliases) != 1 {
		t.Fatalf("entity dictionary not decoded: %+v", res.Entities)
	}
	if len(res.Annotations) != 1 || res.Annotations[0].Derivation != "generated" {
		t.Fatalf("derivation not decoded: %+v", res.Annotations)
	}
}
