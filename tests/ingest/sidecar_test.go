package tests

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// mapStore is a minimal Store + RepresentationStore that records documents by
// rel_path across runs so the incremental gate (content-hash comparison) behaves
// like the real store. It is used by the full-Run sidecar tests.
type mapStore struct {
	docs  map[string]model.Document
	reps  []model.Representation
	chunk int
}

func newMapStore() *mapStore { return &mapStore{docs: map[string]model.Document{}} }

func (m *mapStore) Init(context.Context) error { return nil }
func (m *mapStore) Close() error               { return nil }
func (m *mapStore) UpsertDocument(_ context.Context, doc model.Document) error {
	if doc.DocID == 0 {
		if prev, ok := m.docs[doc.RelPath]; ok {
			doc.DocID = prev.DocID
		} else {
			doc.DocID = int64(len(m.docs) + 1)
		}
	}
	m.docs[doc.RelPath] = doc
	return nil
}
func (m *mapStore) GetDocumentByPath(_ context.Context, relPath string) (model.Document, error) {
	if d, ok := m.docs[relPath]; ok {
		return d, nil
	}
	return model.Document{}, os.ErrNotExist
}
func (m *mapStore) ListFiles(context.Context, string, string, int, int) ([]model.Document, int64, error) {
	out := make([]model.Document, 0, len(m.docs))
	for _, d := range m.docs {
		out = append(out, d)
	}
	return out, int64(len(out)), nil
}
func (m *mapStore) UpsertRepresentation(_ context.Context, rep model.Representation) (int64, error) {
	m.reps = append(m.reps, rep)
	return int64(len(m.reps)), nil
}
func (m *mapStore) InsertChunkWithSpans(_ context.Context, _ model.Chunk, _ []model.Span) (int64, error) {
	m.chunk++
	return int64(m.chunk), nil
}
func (m *mapStore) SoftDeleteChunksFromOrdinal(context.Context, int64, int) error { return nil }
func (m *mapStore) WithTx(ctx context.Context, fn func(tx model.RepresentationStore) error) error {
	return fn(m)
}
func (m *mapStore) lastHashFor(relPath string) string { return m.docs[relPath].ContentHash }

// explodingTranscriber fails the test the moment Transcribe is called. Sidecar
// ingestion MUST short-circuit STT, so any call indicates the precedence rule
// was not honoured.
type explodingTranscriber struct{ t *testing.T }

func (e *explodingTranscriber) Transcribe(context.Context, string, []byte) (string, error) {
	e.t.Fatalf("STT must not be called when a subtitle sidecar exists")
	return "", nil
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newSidecarService builds a service rooted at root with an exploding
// transcriber so a stray STT call fails the test.
func newSidecarService(t *testing.T, root, stateDir string, st model.Store) *ingest.Service {
	t.Helper()
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: stateDir}, st)
	svc.SetTranscriber(&explodingTranscriber{t: t})
	return svc
}

func TestSidecar_VTT_IngestsTranscriptWithoutSTT(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "media", "lecture.mp3"), "fake-audio")
	writeFile(t, filepath.Join(root, "media", "lecture.vtt"),
		"WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nIntro\n\n00:00:02.000 --> 00:00:05.000\nChapter one\n")

	st := &fakeIngestStore{}
	svc := newSidecarService(t, root, t.TempDir(), st)

	doc := model.Document{DocID: 1, RelPath: "media/lecture.mp3", DocType: "audio"}
	ingested, err := svc.IngestSidecarTranscripts(context.Background(), doc)
	if err != nil {
		t.Fatalf("IngestSidecarTranscripts: %v", err)
	}
	if !ingested {
		t.Fatal("expected sidecar to be ingested")
	}
	if len(st.reps) != 1 || st.reps[0].RepType != ingest.RepTypeTranscript {
		t.Fatalf("expected one transcript representation, got %+v", st.reps)
	}
	if len(st.chunks) != 1 {
		t.Fatalf("expected one merged transcript chunk, got %d", len(st.chunks))
	}
	if len(st.spans) != 1 || st.spans[0].Kind != "time" || st.spans[0].StartMS != 0 || st.spans[0].EndMS != 5000 {
		t.Fatalf("unexpected span: %+v", st.spans)
	}
	assertSidecarMeta(t, st.reps[0].MetaJSON, "")
}

func TestSidecar_SRT_IngestsTranscriptWithoutSTT(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "clip.mp4"), "fake-video")
	writeFile(t, filepath.Join(root, "clip.srt"),
		"1\n00:00:00,000 --> 00:00:01,500\nHello\n\n2\n00:00:01,500 --> 00:00:03,000\nGoodbye\n")

	st := &fakeIngestStore{}
	svc := newSidecarService(t, root, t.TempDir(), st)

	doc := model.Document{DocID: 2, RelPath: "clip.mp4", DocType: "video"}
	ingested, err := svc.IngestSidecarTranscripts(context.Background(), doc)
	if err != nil {
		t.Fatalf("IngestSidecarTranscripts: %v", err)
	}
	if !ingested {
		t.Fatal("expected sidecar to be ingested")
	}
	if len(st.reps) != 1 {
		t.Fatalf("expected one representation, got %d", len(st.reps))
	}
	if len(st.spans) != 1 || st.spans[0].EndMS != 3000 {
		t.Fatalf("unexpected spans: %+v", st.spans)
	}
}

func TestSidecar_PerLanguage_DistinctTranscripts(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	writeFile(t, filepath.Join(root, "talk.en.vtt"), "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nhello\n")
	writeFile(t, filepath.Join(root, "talk.fr.vtt"), "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nbonjour\n")

	st := &fakeIngestStore{}
	svc := newSidecarService(t, root, t.TempDir(), st)

	doc := model.Document{DocID: 3, RelPath: "talk.mp3", DocType: "audio"}
	ingested, err := svc.IngestSidecarTranscripts(context.Background(), doc)
	if err != nil {
		t.Fatalf("IngestSidecarTranscripts: %v", err)
	}
	if !ingested {
		t.Fatal("expected sidecars to be ingested")
	}
	if len(st.reps) != 2 {
		t.Fatalf("expected two per-language transcript representations, got %d", len(st.reps))
	}
	// Languages are processed sorted: "en" first, then "fr". Each language gets
	// a distinct rep_type ("transcript-en"/"transcript-fr") so the two rows
	// coexist under UNIQUE(doc_id, rep_type) instead of overwriting each other.
	assertSidecarMeta(t, st.reps[0].MetaJSON, "en")
	assertSidecarMeta(t, st.reps[1].MetaJSON, "fr")
	if st.reps[0].RepType == st.reps[1].RepType {
		t.Fatalf("expected distinct per-language rep_types, both were %q", st.reps[0].RepType)
	}
}

// TestSidecar_PerLanguage_RealStorePersistsBoth guards the
// UNIQUE(doc_id, rep_type) collision: against the real SQLite store, two
// per-language sidecars MUST persist as two distinct transcript representations
// (selectable by language), not collapse into one.
func TestSidecar_PerLanguage_RealStorePersistsBoth(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	writeFile(t, filepath.Join(root, "talk.en.vtt"), "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nhello\n")
	writeFile(t, filepath.Join(root, "talk.fr.vtt"), "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nbonjour\n")

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := newSidecarService(t, root, t.TempDir(), st)

	if err := st.UpsertDocument(context.Background(), model.Document{RelPath: "talk.mp3", DocType: "audio"}); err != nil {
		t.Fatalf("upsert document: %v", err)
	}
	doc, err := st.GetDocumentByPath(context.Background(), "talk.mp3")
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	if _, err := svc.IngestSidecarTranscripts(context.Background(), doc); err != nil {
		t.Fatalf("IngestSidecarTranscripts: %v", err)
	}

	reps, err := st.TranscriptRepresentations(context.Background(), "talk.mp3")
	if err != nil {
		t.Fatalf("TranscriptRepresentations: %v", err)
	}
	if len(reps) != 2 {
		t.Fatalf("expected 2 per-language transcript reps in real store, got %d (%+v)", len(reps), reps)
	}
}

// TestSidecar_Undifferentiated_HasNoLanguage verifies that an undifferentiated
// sidecar ("clip.vtt", not "clip.en.vtt") is recorded with an EMPTY language —
// the bare extension must not be mistaken for a language tag.
func TestSidecar_Undifferentiated_HasNoLanguage(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "media", "lecture.mp3"), "fake-audio")
	writeFile(t, filepath.Join(root, "media", "lecture.vtt"),
		"WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nIntro\n")

	st := &fakeIngestStore{}
	svc := newSidecarService(t, root, t.TempDir(), st)

	doc := model.Document{DocID: 1, RelPath: "media/lecture.mp3", DocType: "audio"}
	if _, err := svc.IngestSidecarTranscripts(context.Background(), doc); err != nil {
		t.Fatalf("IngestSidecarTranscripts: %v", err)
	}
	if len(st.reps) != 1 {
		t.Fatalf("expected one transcript representation, got %d", len(st.reps))
	}
	if got := st.reps[0].RepType; got != ingest.RepTypeTranscript {
		t.Fatalf("undifferentiated sidecar must use the bare transcript rep_type, got %q", got)
	}
	if strings.Contains(st.reps[0].MetaJSON, `"language"`) {
		t.Fatalf("undifferentiated sidecar must record no language, got meta %s", st.reps[0].MetaJSON)
	}
}

func TestSidecar_ForceReindex_RunsSTTInsteadOfSidecar(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "song.mp3"), "fake-audio")
	writeFile(t, filepath.Join(root, "song.vtt"), "WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nignored sidecar\n")

	st := &fakeIngestStore{}
	stt := &fakeTranscriber{text: "[00:00] from stt"}
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir()}, st)
	svc.SetTranscriber(stt)

	f := ingest.DiscoveredFile{RelPath: "song.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, true); err != nil {
		t.Fatalf("ProcessDocument(force): %v", err)
	}
	if stt.calls != 1 {
		t.Fatalf("expected STT to run once under --force, got %d call(s)", stt.calls)
	}
	if len(st.reps) != 1 || st.reps[0].RepType != ingest.RepTypeTranscript {
		t.Fatalf("expected one bare STT transcript rep, got %+v", st.reps)
	}
	// The STT transcript now records source=stt (not sidecar) and an STT
	// derivation identity (spec §8.6.7); it must NOT be a sidecar rep.
	if strings.Contains(st.reps[0].MetaJSON, `"source":"sidecar"`) {
		t.Fatalf("STT transcript must not carry sidecar source, got meta %q", st.reps[0].MetaJSON)
	}
	if !strings.Contains(st.reps[0].MetaJSON, `"source":"stt"`) {
		t.Fatalf("STT transcript must record source=stt, got meta %q", st.reps[0].MetaJSON)
	}
}

func TestSidecar_FreshSidecarReprocessesUnchangedMedia(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mediaPath := filepath.Join(root, "rec.mp3")
	sidecarPath := filepath.Join(root, "rec.vtt")
	writeFile(t, mediaPath, "fake-audio")
	writeFile(t, sidecarPath, "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nv1\n")

	st := newMapStore()
	svc := newSidecarService(t, root, t.TempDir(), st)

	// First scan: media is new, sidecar is ingested.
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	firstHash := st.lastHashFor("rec.mp3")
	if firstHash == "" {
		t.Fatal("expected media document to be recorded on first run")
	}

	// Modify only the sidecar (media bytes unchanged) and bump its mtime so the
	// fingerprint changes and the incremental gate re-processes the media.
	writeFile(t, sidecarPath, "WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nv2 updated\n")
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(sidecarPath, future, future); err != nil {
		t.Fatalf("chtimes sidecar: %v", err)
	}

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	secondHash := st.lastHashFor("rec.mp3")
	if secondHash == firstHash {
		t.Fatalf("expected media content hash to change after sidecar update (was %q)", firstHash)
	}
}

// TestSidecar_FilenameMatching_RejectsExtraDottedSuffixes guards CodeRabbit
// finding sidecar.go ~105: only "<base><ext>" (no lang) and "<base>.<lang><ext>"
// (single token, no extra dots) bind to the media. Files with an extra dotted
// segment — "clip.mp3.vtt" or "clip.notes.en.vtt" for media "clip.mp3" — must
// NOT be treated as that media's sidecars (and so must not suppress STT).
func TestSidecar_FilenameMatching_RejectsExtraDottedSuffixes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "clip.mp3"), "fake-audio")
	// Valid sidecars (must bind):
	writeFile(t, filepath.Join(root, "clip.vtt"), "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nundiff\n")
	writeFile(t, filepath.Join(root, "clip.en.vtt"), "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nenglish\n")
	// Invalid (extra dotted suffix) — must NOT bind to clip.mp3:
	writeFile(t, filepath.Join(root, "clip.mp3.vtt"), "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nwrong\n")
	writeFile(t, filepath.Join(root, "clip.notes.en.vtt"), "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nnotes\n")

	st := &fakeIngestStore{}
	svc := newSidecarService(t, root, t.TempDir(), st)

	doc := model.Document{DocID: 1, RelPath: "clip.mp3", DocType: "audio"}
	ingested, err := svc.IngestSidecarTranscripts(context.Background(), doc)
	if err != nil {
		t.Fatalf("IngestSidecarTranscripts: %v", err)
	}
	if !ingested {
		t.Fatal("expected the valid sidecars to be ingested")
	}
	// Exactly two reps: the undifferentiated (bare "transcript") and the English
	// ("transcript-en"). The two extra-dotted files must contribute nothing.
	if len(st.reps) != 2 {
		t.Fatalf("expected exactly 2 transcript reps (clip.vtt + clip.en.vtt), got %d: %+v", len(st.reps), st.reps)
	}
	repTypes := map[string]bool{}
	for _, r := range st.reps {
		repTypes[r.RepType] = true
	}
	if !repTypes[ingest.RepTypeTranscript] || !repTypes[ingest.TranscriptRepType("en")] {
		t.Fatalf("expected bare + en transcript rep_types, got %v", repTypes)
	}
}

// TestSidecar_PathExcludes_NotUsedAsSidecar guards CodeRabbit finding
// sidecar.go ~137 / service.go seeding: a sidecar whose rel_path matches a
// configured PathExclude must be ignored entirely — neither read nor persisted —
// so STT runs instead of being suppressed by an excluded subtitle file.
func TestSidecar_PathExcludes_NotUsedAsSidecar(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "song.mp3"), "fake-audio")
	writeFile(t, filepath.Join(root, "song.vtt"), "WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nexcluded sidecar\n")

	st := &fakeIngestStore{}
	stt := &fakeTranscriber{text: "[00:00] from stt"}
	cfg := config.Config{RootDir: root, StateDir: t.TempDir(), PathExcludes: []string{"**/*.vtt"}}
	svc := mustNewIngestService(t, cfg, st)
	svc.SetTranscriber(stt)

	f := ingest.DiscoveredFile{RelPath: "song.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	if stt.calls != 1 {
		t.Fatalf("expected STT to run once (excluded .vtt must not suppress it), got %d call(s)", stt.calls)
	}
	if len(st.reps) != 1 || st.reps[0].RepType != ingest.RepTypeTranscript {
		t.Fatalf("expected one bare STT transcript rep, got %+v", st.reps)
	}
	if strings.Contains(st.reps[0].MetaJSON, `"source":"sidecar"`) {
		t.Fatalf("STT transcript must not carry sidecar source, got meta %q", st.reps[0].MetaJSON)
	}
}

// TestSidecar_ForceReindex_RetiresStaleSidecarReps guards CodeRabbit finding
// service.go ~1231: on a forced STT reindex the document's existing
// sidecar-sourced "transcript-<lang>" reps must be tombstoned before the fresh
// STT transcript is written, so TranscriptRepresentations returns only the live
// STT rep — never a stale sidecar transcript alongside it.
func TestSidecar_ForceReindex_RetiresStaleSidecarReps(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	writeFile(t, filepath.Join(root, "talk.en.vtt"), "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nold sidecar\n")

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// First, ingest the sidecar as the transcript (no STT).
	sidecarSvc := newSidecarService(t, root, t.TempDir(), st)
	if err := st.UpsertDocument(context.Background(), model.Document{RelPath: "talk.mp3", DocType: "audio"}); err != nil {
		t.Fatalf("upsert document: %v", err)
	}
	doc, err := st.GetDocumentByPath(context.Background(), "talk.mp3")
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	if _, err := sidecarSvc.IngestSidecarTranscripts(context.Background(), doc); err != nil {
		t.Fatalf("IngestSidecarTranscripts: %v", err)
	}
	reps, err := st.TranscriptRepresentations(context.Background(), "talk.mp3")
	if err != nil {
		t.Fatalf("TranscriptRepresentations: %v", err)
	}
	if len(reps) != 1 || !strings.Contains(reps[0].MetaJSON, `"source":"sidecar"`) {
		t.Fatalf("expected one live sidecar transcript rep before force, got %+v", reps)
	}

	// Now force a reindex: STT must run and the stale sidecar rep must be retired.
	sttSvc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir()}, st)
	stt := &fakeTranscriber{text: "[00:00] fresh stt"}
	sttSvc.SetTranscriber(stt)
	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := sttSvc.ProcessDocument(context.Background(), f, nil, true); err != nil {
		t.Fatalf("ProcessDocument(force): %v", err)
	}
	if stt.calls != 1 {
		t.Fatalf("expected STT to run once under --force, got %d", stt.calls)
	}

	reps, err = st.TranscriptRepresentations(context.Background(), "talk.mp3")
	if err != nil {
		t.Fatalf("TranscriptRepresentations after force: %v", err)
	}
	if len(reps) != 1 {
		t.Fatalf("expected exactly 1 live transcript rep after force (stale sidecar retired), got %d: %+v", len(reps), reps)
	}
	if strings.Contains(reps[0].MetaJSON, `"source":"sidecar"`) {
		t.Fatalf("expected the live rep to be the fresh STT transcript, but it is still a sidecar: %s", reps[0].MetaJSON)
	}
}

func assertSidecarMeta(t *testing.T, metaJSON, wantLang string) {
	t.Helper()
	if metaJSON == "" {
		t.Fatal("expected non-empty sidecar transcript meta_json")
	}
	if !strings.Contains(metaJSON, `"source":"sidecar"`) {
		t.Fatalf("expected source=sidecar in meta_json, got %s", metaJSON)
	}
	if wantLang != "" && !strings.Contains(metaJSON, `"language":"`+wantLang+`"`) {
		t.Fatalf("expected language=%q in meta_json, got %s", wantLang, metaJSON)
	}
}
