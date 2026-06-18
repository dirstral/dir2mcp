package tests

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestLanguageMetadata_SidecarRecordsDeclaredSourceAndChunkLanguage pins the
// detected-language metadata contract (SPEC §5.2/§8.8) plus the §9.5 denorm: a
// per-language sidecar transcript records its effective language with
// language_source="declared" (the filename suffix is a source-asserted
// declaration), and that effective language is denormalized onto the persisted
// chunk rows so the per-language retrieval filter can predicate on it.
func TestLanguageMetadata_SidecarRecordsDeclaredSourceAndChunkLanguage(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	writeFile(t, filepath.Join(root, "talk.pt.vtt"),
		"WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nola mundo\n")

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

	// Representation meta_json records the effective language + declared source.
	reps, err := st.TranscriptRepresentations(context.Background(), "talk.mp3")
	if err != nil {
		t.Fatalf("TranscriptRepresentations: %v", err)
	}
	if len(reps) != 1 {
		t.Fatalf("expected one transcript representation, got %d", len(reps))
	}
	meta := reps[0].MetaJSON
	if !strings.Contains(meta, `"language":"pt"`) {
		t.Fatalf("expected language=pt in meta_json, got %s", meta)
	}
	if !strings.Contains(meta, `"language_source":"declared"`) {
		t.Fatalf("expected language_source=declared in meta_json, got %s", meta)
	}

	// The effective language is denormalized onto the persisted chunk rows.
	tasks, err := st.NextPending(context.Background(), 100, "")
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatal("expected at least one pending chunk for the sidecar transcript")
	}
	for _, task := range tasks {
		if task.Metadata.Language != "pt" {
			t.Fatalf("chunk %d must carry the representation language pt, got %q",
				task.Metadata.ChunkID, task.Metadata.Language)
		}
	}
}

// TestLanguageMetadata_UndifferentiatedSidecarIsUnknown pins §8.8 graceful
// degradation: an undifferentiated sidecar (clip.vtt, no language suffix)
// records NO language (unknown) and NO language_source, and its chunks carry an
// empty language — which never matches a specific §9.5 filter.
func TestLanguageMetadata_UndifferentiatedSidecarIsUnknown(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "lecture.mp3"), "fake-audio")
	writeFile(t, filepath.Join(root, "lecture.vtt"),
		"WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nIntro\n")

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := newSidecarService(t, root, t.TempDir(), st)

	if err := st.UpsertDocument(context.Background(), model.Document{RelPath: "lecture.mp3", DocType: "audio"}); err != nil {
		t.Fatalf("upsert document: %v", err)
	}
	doc, err := st.GetDocumentByPath(context.Background(), "lecture.mp3")
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	if _, err := svc.IngestSidecarTranscripts(context.Background(), doc); err != nil {
		t.Fatalf("IngestSidecarTranscripts: %v", err)
	}

	reps, err := st.TranscriptRepresentations(context.Background(), "lecture.mp3")
	if err != nil {
		t.Fatalf("TranscriptRepresentations: %v", err)
	}
	if len(reps) != 1 {
		t.Fatalf("expected one transcript representation, got %d", len(reps))
	}
	if strings.Contains(reps[0].MetaJSON, `"language"`) {
		t.Fatalf("undifferentiated sidecar must record no language, got %s", reps[0].MetaJSON)
	}
	if strings.Contains(reps[0].MetaJSON, `"language_source"`) {
		t.Fatalf("undifferentiated sidecar must record no language_source, got %s", reps[0].MetaJSON)
	}

	tasks, err := st.NextPending(context.Background(), 100, "")
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatal("expected at least one pending chunk")
	}
	for _, task := range tasks {
		if task.Metadata.Language != "" {
			t.Fatalf("unknown-language chunk must carry empty language, got %q", task.Metadata.Language)
		}
	}
}
