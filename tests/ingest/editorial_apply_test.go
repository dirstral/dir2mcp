package tests

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/editorial"
)

// The apply step of the external editorial annotation import (SPEC §7.11.3,
// §7.11.4, §7.11.5) and the conformance cases of §7.11.8 that the index
// writer owns. All records are made up.

const edSource = "example-catalogue"

// mappingRecord is the record of conformance case 2.
const mappingRecord = `{"stem": "news", "content_id": "C-1001", "match": {"score": 1.25, "external_id": "X-2002", "asset_id": "A-7"}, ` +
	`"header": "Пример передачи", "people": [{"name": "Пётр Ёлкин", "role": "инженер", "x": 1}, {"name": "Анна Белова"}], ` +
	`"segments": [{"t": "00:00:00", "text": "Вступление"}, {"t": "00:01:00", "text": "Прогноз погоды на неделю"}, {"t": "00:02:30", "text": "Новости города"}], ` +
	`"description": "Выпуск о погоде и о городе."}`

// Conformance case 2 (mapping), with the attribute half of case 1.
func TestEditorialApply_Mapping(t *testing.T) {
	e := newEditorialEnv(t, nil)
	e.addDoc("news.mp4", "video", "ok", 3*time.Minute)
	e.stage(edSource, editorial.KeyStem, mappingRecord)
	e.apply()

	anns := e.annotations("news.mp4")
	assertProvenanceOnEvery(t, anns)
	byEvent := splitByEvent(anns)
	assertPeopleMapping(t, byEvent[editorial.EventAppears])
	assertSegmentMapping(t, byEvent[editorial.EventSegment])
	assertDescriptionMapping(t, byEvent[editorial.EventDescription])
	assertMappingReportAndMeta(t, e, len(anns))
}

// assertProvenanceOnEvery checks the fields every editorial span carries.
func assertProvenanceOnEvery(t *testing.T, anns []edAnnotation) {
	t.Helper()
	for _, a := range anns {
		if a.RepType != "editorial" {
			t.Fatalf("chunk rep_type = %q, want editorial", a.RepType)
		}
		if !reflect.DeepEqual(a.Sources, []string{edSource}) {
			t.Fatalf("span sources = %v, want [%s]", a.Sources, edSource)
		}
		if a.Attributes["match_score"] != "1.25" || a.Attributes["content_id"] != "C-1001" || a.Attributes["match_asset_id"] != "A-7" {
			t.Fatalf("span attributes %v miss the provenance", a.Attributes)
		}
		if a.Derivation != "" {
			t.Fatalf("an editorial span carries derivation %q", a.Derivation)
		}
	}
}

func splitByEvent(anns []edAnnotation) map[string][]edAnnotation {
	out := map[string][]edAnnotation{}
	for _, a := range anns {
		out[a.Event] = append(out[a.Event], a)
	}
	return out
}

func assertPeopleMapping(t *testing.T, appears []edAnnotation) {
	t.Helper()
	if len(appears) != 2 {
		t.Fatalf("appears annotations = %d, want 2", len(appears))
	}
	for _, a := range appears {
		if a.SpanKind != "time" || a.Start != 0 || a.End != 180000 {
			t.Fatalf("person span = %s [%d, %d], want time [0, 180000]", a.SpanKind, a.Start, a.End)
		}
	}
	if appears[0].Entities[0] != "пётр ёлкин" || appears[0].Attributes["role"] != "инженер" || appears[0].Text != "Пётр Ёлкин, инженер" {
		t.Fatalf("first person = %+v", appears[0])
	}
	if _, has := appears[1].Attributes["role"]; has {
		t.Fatalf("a person with no role carries a role attribute: %v", appears[1].Attributes)
	}
}

func assertSegmentMapping(t *testing.T, segments []edAnnotation) {
	t.Helper()
	want := [][2]int{{0, 60000}, {60000, 150000}, {150000, 180000}}
	if len(segments) != len(want) {
		t.Fatalf("segment annotations = %d, want %d", len(segments), len(want))
	}
	for i, a := range segments {
		if a.SpanKind != "time" || a.Start != want[i][0] || a.End != want[i][1] || len(a.Entities) != 0 {
			t.Fatalf("segment %d = %+v, want time %v and no entities", i, a, want[i])
		}
	}
}

func assertDescriptionMapping(t *testing.T, description []edAnnotation) {
	t.Helper()
	if len(description) == 0 {
		t.Fatal("no description chunk")
	}
	for _, a := range description {
		if a.SpanKind != "document" || a.Start != 0 || a.End != 0 {
			t.Fatalf("description span = %s [%d, %d], want a document span", a.SpanKind, a.Start, a.End)
		}
	}
	if !strings.Contains(description[0].Text, "Пример передачи") || !strings.Contains(description[0].Text, "Выпуск о погоде") {
		t.Fatalf("description text = %q", description[0].Text)
	}
}

func assertMappingReportAndMeta(t *testing.T, e *editorialEnv, annotations int) {
	t.Helper()
	report := e.lastReport()
	if report.Applied != 1 || report.Annotations != annotations || report.Level() != "info" {
		t.Fatalf("report = %+v", report)
	}
	if report.IgnoredFields["people[].x"] != 1 {
		t.Fatalf("ignored_fields = %v, want people[].x: 1", report.IgnoredFields)
	}
	meta, _ := e.editorialMeta("news.mp4", edSource)
	for _, key := range []string{"source", "key_kind", "key", "match", "record_hash", "imported_unix", "name_folds"} {
		if _, ok := meta[key]; !ok {
			t.Fatalf("meta_json %v misses %s", meta, key)
		}
	}
	for _, key := range []string{"provider", "model"} {
		if _, ok := meta[key]; ok {
			t.Fatalf("meta_json records %s: %v", key, meta)
		}
	}
	if staged, _ := editorial.ListStaged(e.stateDir); len(staged) != 0 {
		t.Fatalf("the applied staged import was not removed: %v", staged)
	}
	if entries, err := os.ReadDir(editorial.ReportsDir(e.stateDir)); err != nil || len(entries) != 1 {
		t.Fatalf("imports/reports holds %d entries (err %v), want one report", len(entries), err)
	}
}

// Conformance case 3 (idempotent re-import, replace per source and document).
func TestEditorialApply_IdempotentAndScopedReplace(t *testing.T) {
	e := newEditorialEnv(t, nil)
	e.addDoc("news.mp4", "video", "ok", 3*time.Minute)
	e.addDoc("other.mp4", "video", "ok", time.Minute)
	other := `{"stem": "other", "people": [{"name": "Анна Белова"}]}`
	e.stage(edSource, editorial.KeyStem, mappingRecord, other)
	e.stage("second-source", editorial.KeyStem, `{"stem": "news", "header": "Вторая разметка"}`)
	e.apply()

	first := e.annotations("news.mp4")
	_, firstHash := e.editorialMeta("news.mp4", edSource)
	firstMeta, _ := e.editorialMeta("news.mp4", edSource)
	otherBefore := e.annotations("other.mp4")

	e.stage(edSource, editorial.KeyStem, mappingRecord, other)
	e.apply()
	again := e.annotations("news.mp4")
	againMeta, againHash := e.editorialMeta("news.mp4", edSource)
	if !reflect.DeepEqual(first, again) || firstHash != againHash || !reflect.DeepEqual(firstMeta, againMeta) {
		t.Fatal("importing the same file again changed the representation")
	}

	e.stage(edSource, editorial.KeyStem, `{"stem": "news", "people": [{"name": "Новое Имя"}]}`)
	e.apply()
	after := e.annotations("news.mp4")
	var bySource = map[string]int{}
	for _, a := range after {
		bySource[a.Sources[0]]++
		if a.Sources[0] == edSource && a.Entities[0] != "новое имя" {
			t.Fatalf("the changed record did not replace the source's annotations: %+v", a)
		}
	}
	if bySource[edSource] != 1 || bySource["second-source"] != 1 {
		t.Fatalf("annotations per source after the replace = %v, want 1 and 1", bySource)
	}
	if !reflect.DeepEqual(otherBefore, e.annotations("other.mp4")) {
		t.Fatal("a document the file did not name changed")
	}
}

// Conformance case 4 (unknown key) and the level of the report.
func TestEditorialApply_UnknownAndNonMediaKeys(t *testing.T) {
	e := newEditorialEnv(t, nil)
	e.addDoc("news.mp4", "video", "ok", time.Minute)
	e.addDoc("abc_thumb.bin", "binary", "skipped", 0)
	e.stage(edSource, editorial.KeyStem,
		`{"stem": "news", "header": "Есть"}`,
		`{"stem": "missing", "header": "Нет документа"}`,
		`{"stem": "abc", "header": "Только не медиа"}`)
	e.apply()
	r := e.lastReport()
	if r.Unmatched != 1 || !reflect.DeepEqual(r.UnmatchedKeys, []string{"missing"}) {
		t.Fatalf("unmatched = %d %v", r.Unmatched, r.UnmatchedKeys)
	}
	if r.NonMediaOnly != 1 || !reflect.DeepEqual(r.NonMediaOnlyKeys, []string{"abc"}) {
		t.Fatalf("non_media_only = %d %v", r.NonMediaOnly, r.NonMediaOnlyKeys)
	}
	if r.Applied != 1 || len(e.annotations("news.mp4")) == 0 {
		t.Fatalf("the matched record was not applied: %+v", r)
	}
	if r.Level() != "warn" {
		t.Fatalf("level = %s, want warn", r.Level())
	}
	if _, found, _ := editorial.LoadRetained(e.stateDir, "abc_thumb.bin", edSource); found {
		t.Fatal("a record with no eligible match was retained")
	}
}

// Conformance case 5 (name prefix and eligibility, multi_matched).
func TestEditorialApply_StemPrefixAndEligibility(t *testing.T) {
	e := newEditorialEnv(t, nil)
	e.addDoc("v/abc_1080p.mp4", "video", "ok", time.Minute)
	e.addDoc("v/abc_thumb.bin", "binary", "skipped", 0)
	e.addDoc("v/abcd.mp4", "video", "ok", time.Minute)
	e.addDoc("v/x_abc.mp4", "video", "ok", time.Minute)
	e.stage(edSource, editorial.KeyStem, `{"stem": "abc", "header": "Шапка"}`)
	e.apply()
	if len(e.annotations("v/abc_1080p.mp4")) == 0 {
		t.Fatal("abc_1080p.mp4 got no annotations")
	}
	for _, p := range []string{"v/abc_thumb.bin", "v/abcd.mp4", "v/x_abc.mp4"} {
		if n := len(e.annotations(p)); n != 0 {
			t.Fatalf("%s got %d annotations", p, n)
		}
	}
	if r := e.lastReport(); r.MultiMatched != 0 {
		t.Fatalf("multi_matched = %d with one eligible match", r.MultiMatched)
	}

	e.addDoc("w/abc_720p.mp4", "video", "ok", time.Minute)
	e.stage(edSource, editorial.KeyStem, `{"stem": "abc", "header": "Шапка"}`)
	e.apply()
	if len(e.annotations("w/abc_720p.mp4")) == 0 || len(e.annotations("v/abc_1080p.mp4")) == 0 {
		t.Fatal("both renditions must carry the annotations")
	}
	if r := e.lastReport(); r.MultiMatched != 1 || r.Applied != 2 {
		t.Fatalf("report = %+v, want multi_matched 1 and applied 2", r)
	}
}

// Two records of one import that resolve to the same document: neither is
// applied there, both keys are conflicting (SPEC §7.11.3).
func TestEditorialApply_ConflictingRecords(t *testing.T) {
	e := newEditorialEnv(t, nil)
	e.addDoc("abc_def.mp4", "video", "ok", time.Minute)
	e.stage(edSource, editorial.KeyStem, `{"stem": "abc", "header": "Одна"}`, `{"stem": "abc_def", "header": "Другая"}`)
	e.apply()
	r := e.lastReport()
	if r.Conflicting != 2 || r.Applied != 0 || len(e.annotations("abc_def.mp4")) != 0 {
		t.Fatalf("report = %+v, annotations = %d", r, len(e.annotations("abc_def.mp4")))
	}
}

// Conformance case 9: no editorial text or span field enters a transcript.
func TestEditorialApply_TranscriptUntouched(t *testing.T) {
	e := newEditorialEnv(t, nil)
	e.addDoc("news.mp4", "video", "ok", 3*time.Minute)
	e.seedTranscript("news.mp4")
	before := e.transcriptChunks("news.mp4")
	e.stage(edSource, editorial.KeyStem, mappingRecord)
	e.apply()
	after := e.transcriptChunks("news.mp4")
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the transcript chunks changed:\nbefore %+v\nafter  %+v", before, after)
	}
	for _, c := range after {
		if strings.Contains(c.Text, "Прогноз") || strings.Contains(c.Text, "Пётр") || len(c.Sources) > 0 || c.Event != "" || len(c.Attributes) > 0 {
			t.Fatalf("a transcript chunk carries editorial content: %+v", c)
		}
	}
}

// A changed fold list or media duration builds the representation again from
// the retained record, with no new import (SPEC §7.11.3, §7.11.4).
func TestEditorialApply_RebuildsOnFoldsAndDuration(t *testing.T) {
	e := newEditorialEnv(t, nil)
	e.addDoc("news.mp4", "video", "ok", 3*time.Minute)
	e.stage(edSource, editorial.KeyStem, mappingRecord)
	e.apply()
	if got := e.annotations("news.mp4")[0].Entities[0]; got != "пётр ёлкин" {
		t.Fatalf("default id = %q", got)
	}

	e.newService([]string{"ё=е"}, nil)
	e.apply()
	if got := e.annotations("news.mp4")[0].Entities[0]; got != "петр елкин" {
		t.Fatalf("after the fold change the id is %q, want петр елкин", got)
	}
	meta, _ := e.editorialMeta("news.mp4", edSource)
	if folds, _ := meta["name_folds"].([]any); len(folds) != 1 || folds[0] != "ё=е" {
		t.Fatalf("meta name_folds = %v, want [ё=е]", meta["name_folds"])
	}

	// A content change gives a new hash and a new duration.
	e.addDoc("news.mp4", "video", "ok", 2*time.Minute)
	if err := e.st.UpsertDocument(e.ctx, docWithHash("news.mp4", "hash-changed")); err != nil {
		t.Fatal(err)
	}
	e.apply()
	if a := e.annotations("news.mp4")[0]; a.End != 120000 {
		t.Fatalf("after the duration change the person span ends at %d, want 120000", a.End)
	}
}

// The applied records are kept in the state directory: a rebuilt index gets
// the annotations back with no new import, and a tombstone removes them.
func TestEditorialApply_RetainedRecordsSurviveRebuildAndGoWithTombstone(t *testing.T) {
	e := newEditorialEnv(t, nil)
	e.addDoc("news.mp4", "video", "ok", 3*time.Minute)
	e.stage(edSource, editorial.KeyStem, mappingRecord)
	e.apply()
	want := e.annotations("news.mp4")

	// A full rebuild from an empty metadata store.
	if err := e.st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(e.stateDir, "meta.sqlite")); err != nil {
		t.Fatal(err)
	}
	e.openStore()
	e.newService(nil, nil)
	e.addDoc("news.mp4", "video", "ok", 3*time.Minute)
	e.apply()
	if got := e.annotations("news.mp4"); !reflect.DeepEqual(got, want) {
		t.Fatalf("the rebuilt index did not restore the annotations:\n got %+v\nwant %+v", got, want)
	}

	if err := e.st.MarkDocumentDeleted(e.ctx, "news.mp4"); err != nil {
		t.Fatal(err)
	}
	e.apply()
	if _, found, _ := editorial.LoadRetained(e.stateDir, "news.mp4", edSource); found {
		t.Fatal("the tombstone did not remove the retained record")
	}
}

// The scan writes the index, so it applies a staged import at its end: this is
// the path of the next `up` or `reindex` (SPEC §7.11.1).
func TestEditorialApply_ScanAppliesStagedImport(t *testing.T) {
	e := newEditorialEnv(t, nil)
	e.stage(edSource, editorial.KeyStem, `{"stem": "nothing-here", "header": "Шапка"}`)
	if err := e.svc.Run(e.ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if staged, _ := editorial.ListStaged(e.stateDir); len(staged) != 0 {
		t.Fatalf("the scan left the staged import in place: %v", staged)
	}
	if r := e.lastReport(); r.Unmatched != 1 || r.Records != 1 {
		t.Fatalf("report = %+v", r)
	}
}

// A record with no content removes the source from the document, and a record
// that matches a secret pattern is withheld.
func TestEditorialApply_EmptyRecordRemovesAndSecretIsWithheld(t *testing.T) {
	e := newEditorialEnv(t, nil)
	e.addDoc("news.mp4", "video", "ok", time.Minute)
	e.addDoc("leak.mp4", "video", "ok", time.Minute)
	e.newService(nil, []string{`SECRET-[0-9]{6}`})
	e.stage(edSource, editorial.KeyStem, `{"stem": "news", "header": "Шапка"}`,
		`{"stem": "leak", "description": "the code is SECRET-123456"}`)
	e.apply()
	r := e.lastReport()
	if r.Withheld != 1 || !reflect.DeepEqual(r.WithheldKeys, []string{"leak"}) || len(e.annotations("leak.mp4")) != 0 {
		t.Fatalf("report = %+v", r)
	}
	if len(e.annotations("news.mp4")) == 0 {
		t.Fatal("the clean record was not applied")
	}
	e.stage(edSource, editorial.KeyStem, `{"stem": "news", "content_id": "C-1001"}`)
	e.apply()
	if n := len(e.annotations("news.mp4")); n != 0 {
		t.Fatalf("an empty record left %d annotations", n)
	}
	if _, found, _ := editorial.LoadRetained(e.stateDir, "news.mp4", edSource); found {
		t.Fatal("an empty record left a retained record")
	}
}
