package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// seedTranscript inserts a document plus a transcript representation with the
// given meta_json and time-spanned chunks, returning the open store.
func seedTranscript(t *testing.T, relPath, metaJSON string, chunks []model.Chunk, spans [][]model.Span) *store.SQLiteStore {
	t.Helper()
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: relPath, DocType: "audio", SourceType: "local", Status: "ok",
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	doc, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	err = st.WithTx(ctx, func(tx model.RepresentationStore) error {
		repID, err := tx.UpsertRepresentation(ctx, model.Representation{
			DocID: doc.DocID, RepType: "transcript", RepHash: "h-" + relPath + metaJSON, MetaJSON: metaJSON,
		})
		if err != nil {
			return err
		}
		for i, ch := range chunks {
			ch.RepID = repID
			ch.Ordinal = i
			if _, err := tx.InsertChunkWithSpans(ctx, ch, spans[i]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed transcript: %v", err)
	}
	return st
}

// TestTranscriptSpanChunksOrdered pins that TranscriptSpanChunks returns active
// chunks joined to their time spans, ordered by span start, so the cue builder
// receives playback-ordered input regardless of insertion order.
func TestTranscriptSpanChunksOrdered(t *testing.T) {
	ctx := context.Background()
	st := seedTranscript(t, "media/talk.mp3", "",
		[]model.Chunk{
			{Text: "second", IndexKind: "text"},
			{Text: "first", IndexKind: "text"},
		},
		[][]model.Span{
			{{Kind: "time", StartMS: 2000, EndMS: 3000}},
			{{Kind: "time", StartMS: 0, EndMS: 2000}},
		},
	)

	reps, err := st.TranscriptRepresentations(ctx, "media/talk.mp3")
	if err != nil {
		t.Fatalf("TranscriptRepresentations: %v", err)
	}
	if len(reps) != 1 {
		t.Fatalf("got %d transcript reps, want 1", len(reps))
	}

	rows, err := st.TranscriptSpanChunks(ctx, reps[0].RepID)
	if err != nil {
		t.Fatalf("TranscriptSpanChunks: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d span chunks, want 2", len(rows))
	}
	if rows[0].Text != "first" || rows[0].Span.StartMS != 0 {
		t.Errorf("row0 = %+v, want first at 0", rows[0])
	}
	if rows[1].Text != "second" || rows[1].Span.StartMS != 2000 {
		t.Errorf("row1 = %+v, want second at 2000", rows[1])
	}
}

// TestTranscriptRepresentationsMissingDoc pins that a missing document is
// reported as os.ErrNotExist (distinct from "no transcript").
func TestTranscriptRepresentationsMissingDoc(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := st.TranscriptRepresentations(ctx, "nope.mp3"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
}

// TestTranscriptRepresentationsNoTranscript pins that an existing document with
// no transcript representation yields an empty slice and a nil error.
func TestTranscriptRepresentationsNoTranscript(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: "doc.md", DocType: "md", SourceType: "local", Status: "ok",
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	reps, err := st.TranscriptRepresentations(ctx, "doc.md")
	if err != nil {
		t.Fatalf("TranscriptRepresentations: %v", err)
	}
	if len(reps) != 0 {
		t.Fatalf("got %d reps, want 0", len(reps))
	}
}
