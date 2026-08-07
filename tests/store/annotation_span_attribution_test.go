package tests

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// A recognition annotation's entity ids and `event` ride in the "time" span's
// extra_json, alongside `words` and `speaker` (dirstral-spec design 0004 §7).
// These pin the round trip: the filter can only select on what comes back out.

// annotationChunk seeds a document + representation and inserts one chunk with
// the given span, returning the chunk id.
func annotationChunk(t *testing.T, st *store.SQLiteStore, relPath, repType string, span model.Span) int64 {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: relPath, DocType: "video", SourceType: "local", Status: "ok",
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	doc, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	repID, err := st.UpsertRepresentation(ctx, model.Representation{
		DocID: doc.DocID, RepType: repType, RepHash: "h", CreatedUnix: 1,
	})
	if err != nil {
		t.Fatalf("UpsertRepresentation: %v", err)
	}
	chunkID, err := st.InsertChunkWithSpans(ctx, model.Chunk{
		RepID: repID, Ordinal: 0, Text: "a moment",
		IndexKind: "text", EmbeddingStatus: "pending",
	}, []model.Span{span})
	if err != nil {
		t.Fatalf("InsertChunkWithSpans: %v", err)
	}
	return chunkID
}

func newAnnotationStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestAnnotationAttributionRoundTrips(t *testing.T) {
	st := newAnnotationStore(t)
	entities := []string{"player:robbie-ray", "team:san-francisco-giants"}
	chunkID := annotationChunk(t, st, "game.mp4", "recognition", model.Span{
		Kind: "time", StartMS: 20300, EndMS: 28300,
		Entities: entities, Event: "pitch",
	})

	_, _, span, err := st.ChunkMediaSpanByID(context.Background(), chunkID)
	if err != nil {
		t.Fatalf("ChunkMediaSpanByID: %v", err)
	}
	if span.Event != "pitch" {
		t.Fatalf("event did not round-trip: %q", span.Event)
	}
	if strings.Join(span.Entities, ",") != strings.Join(entities, ",") {
		// Order is part of the contract: the acting entity is emitted first.
		t.Fatalf("entities did not round-trip: %v, want %v", span.Entities, entities)
	}
}

// TestATranscriptSpanIsUndisturbed is the compatibility guard on the shared
// extra_json blob. Two optional keys were added to an object that transcripts
// already write; a transcript without them must store and read back exactly as
// before, or every existing corpus re-derives for no reason.
func TestATranscriptSpanIsUndisturbed(t *testing.T) {
	st := newAnnotationStore(t)
	chunkID := annotationChunk(t, st, "talk.mp3", "transcript", model.Span{
		Kind: "time", StartMS: 0, EndMS: 1000, Speaker: "S1", SpeakerLabel: "Host",
	})
	_, _, span, err := st.ChunkMediaSpanByID(context.Background(), chunkID)
	if err != nil {
		t.Fatalf("ChunkMediaSpanByID: %v", err)
	}
	if span.Speaker != "S1" || span.SpeakerLabel != "Host" {
		t.Fatalf("speaker attribution disturbed: %+v", span)
	}
	if len(span.Entities) != 0 || span.Event != "" {
		t.Fatalf("a transcript span gained annotation attribution: %+v", span)
	}
}

// TestASpanWithNoAttributionStoresNothingExtra: the payload is omitempty, so a
// span carrying neither words, speaker, nor attribution must still write SQL
// NULL rather than an empty JSON object.
func TestASpanWithNoAttributionStoresNothingExtra(t *testing.T) {
	st := newAnnotationStore(t)
	chunkID := annotationChunk(t, st, "plain.mp4", "recognition", model.Span{
		Kind: "time", StartMS: 5, EndMS: 10,
	})
	_, _, span, err := st.ChunkMediaSpanByID(context.Background(), chunkID)
	if err != nil {
		t.Fatalf("ChunkMediaSpanByID: %v", err)
	}
	if len(span.Entities) != 0 || span.Event != "" || span.Speaker != "" {
		t.Fatalf("empty span came back populated: %+v", span)
	}
	if span.StartMS != 5 || span.EndMS != 10 {
		t.Fatalf("span bounds disturbed: %+v", span)
	}
}

// TestDuplicateAndBlankEntityIdsAreNormalised: a backend that repeats an id, or
// pads one, must not inflate the stored attribution or create an id that no
// filter value can ever equal.
func TestDuplicateAndBlankEntityIdsAreNormalised(t *testing.T) {
	st := newAnnotationStore(t)
	chunkID := annotationChunk(t, st, "dupes.mp4", "recognition", model.Span{
		Kind: "time", StartMS: 0, EndMS: 1,
		Entities: []string{" player:a ", "player:a", "", "   ", "player:b"},
		Event:    "  pitch  ",
	})
	_, _, span, err := st.ChunkMediaSpanByID(context.Background(), chunkID)
	if err != nil {
		t.Fatalf("ChunkMediaSpanByID: %v", err)
	}
	if strings.Join(span.Entities, ",") != "player:a,player:b" {
		t.Fatalf("entities = %v, want the trimmed de-duplicated pair", span.Entities)
	}
	if span.Event != "pitch" {
		t.Fatalf("event = %q, want the trimmed value", span.Event)
	}
}
