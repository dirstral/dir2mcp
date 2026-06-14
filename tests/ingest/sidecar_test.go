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
	// Languages are processed sorted: "en" first, then "fr".
	assertSidecarMeta(t, st.reps[0].MetaJSON, "en")
	assertSidecarMeta(t, st.reps[1].MetaJSON, "fr")
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
	if len(st.reps) != 1 || st.reps[0].MetaJSON != "" {
		t.Fatalf("expected one STT transcript rep with no sidecar meta, got %+v", st.reps)
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
