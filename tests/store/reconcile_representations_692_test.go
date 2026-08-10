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

// Store primitives behind output-set reconciliation (dir2mcp #692):
// ActiveRepresentations enumerates a document's live outputs, and
// SoftDeleteRepresentations retires a chosen subset with their chunks.

func newReconcileStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return st
}

func addDoc(t *testing.T, st *store.SQLiteStore, relPath string) int64 {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: relPath, DocType: "video", ContentHash: "h-" + relPath, Status: "ok",
	}); err != nil {
		t.Fatalf("UpsertDocument(%s): %v", relPath, err)
	}
	doc, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath(%s): %v", relPath, err)
	}
	return doc.DocID
}

func addRep(t *testing.T, st *store.SQLiteStore, docID int64, repType, metaJSON string) int64 {
	t.Helper()
	repID, err := st.UpsertRepresentation(context.Background(), model.Representation{
		DocID: docID, RepType: repType, RepHash: "h-" + repType, MetaJSON: metaJSON,
	})
	if err != nil {
		t.Fatalf("UpsertRepresentation(%s): %v", repType, err)
	}
	return repID
}

func addChunk(t *testing.T, st *store.SQLiteStore, repID int64, ordinal int) {
	t.Helper()
	if _, err := st.InsertChunkWithSpans(context.Background(), model.Chunk{
		RepID: repID, Ordinal: ordinal, Text: "body", TextHash: "th", IndexKind: "text",
	}, nil); err != nil {
		t.Fatalf("InsertChunkWithSpans: %v", err)
	}
}

// TestActiveRepresentations_ListsLiveOutputs pins the enumeration: every live
// representation with its rep_type and recorded provenance, ordered by rep_id,
// and nothing that is already tombstoned.
func TestActiveRepresentations_ListsLiveOutputs(t *testing.T) {
	ctx := context.Background()
	st := newReconcileStore(t)
	docID := addDoc(t, st, "media/a.mp4")
	addRep(t, st, docID, "transcript", `{"source":"stt"}`)
	esRepID := addRep(t, st, docID, "transcript-es", `{"source":"translation","language":"es"}`)

	reps, err := st.ActiveRepresentations(ctx, "media/a.mp4")
	if err != nil {
		t.Fatalf("ActiveRepresentations: %v", err)
	}
	if len(reps) != 2 {
		t.Fatalf("reps = %+v, want 2", reps)
	}
	if reps[0].RepType != "transcript" || reps[1].RepType != "transcript-es" {
		t.Errorf("reps are not rep_id ordered: %+v", reps)
	}
	if reps[1].MetaJSON != `{"source":"translation","language":"es"}` {
		t.Errorf("meta_json = %q, want the recorded provenance", reps[1].MetaJSON)
	}

	if _, err := st.SoftDeleteRepresentations(ctx, "media/a.mp4", []int64{esRepID}); err != nil {
		t.Fatalf("SoftDeleteRepresentations: %v", err)
	}
	reps, err = st.ActiveRepresentations(ctx, "media/a.mp4")
	if err != nil {
		t.Fatalf("ActiveRepresentations after retire: %v", err)
	}
	if len(reps) != 1 || reps[0].RepType != "transcript" {
		t.Errorf("retired representation is still listed: %+v", reps)
	}

	// A missing document is reported as os.ErrNotExist, not as an empty list.
	if _, err := st.ActiveRepresentations(ctx, "media/nope.mp4"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ActiveRepresentations(missing) err = %v, want os.ErrNotExist", err)
	}
}

// TestSoftDeleteRepresentations_TombstonesChunks confirms the retired
// representation's chunks are tombstoned too. The SQLite tombstone is what keeps
// a retired output out of retrieval, in the session and after a restart.
func TestSoftDeleteRepresentations_TombstonesChunks(t *testing.T) {
	ctx := context.Background()
	st := newReconcileStore(t)
	docID := addDoc(t, st, "media/a.mp4")
	keepRepID := addRep(t, st, docID, "transcript", `{"source":"stt"}`)
	dropRepID := addRep(t, st, docID, "transcript-es", `{"source":"translation","language":"es"}`)
	addChunk(t, st, keepRepID, 0)
	addChunk(t, st, dropRepID, 0)
	addChunk(t, st, dropRepID, 1)

	retired, err := st.SoftDeleteRepresentations(ctx, "media/a.mp4", []int64{dropRepID})
	if err != nil {
		t.Fatalf("SoftDeleteRepresentations: %v", err)
	}
	if retired != 1 {
		t.Fatalf("retired = %d, want 1", retired)
	}

	dropped, err := st.GetChunksByRepID(ctx, dropRepID)
	if err != nil {
		t.Fatalf("GetChunksByRepID(dropped): %v", err)
	}
	if len(dropped) != 2 {
		t.Fatalf("dropped chunks = %d, want 2", len(dropped))
	}
	for _, chunk := range dropped {
		if !chunk.Deleted {
			t.Errorf("chunk %d of a retired representation is still live", chunk.ChunkID)
		}
	}

	kept, err := st.GetChunksByRepID(ctx, keepRepID)
	if err != nil {
		t.Fatalf("GetChunksByRepID(kept): %v", err)
	}
	if len(kept) != 1 || kept[0].Deleted {
		t.Errorf("the kept representation's chunk was tombstoned: %+v", kept)
	}

	// Idempotent: retiring the same representation again reports nothing retired.
	again, err := st.SoftDeleteRepresentations(ctx, "media/a.mp4", []int64{dropRepID})
	if err != nil {
		t.Fatalf("SoftDeleteRepresentations (repeat): %v", err)
	}
	if again != 0 {
		t.Errorf("repeat retire = %d, want 0", again)
	}
}

// TestSoftDeleteRepresentations_ScopedToDocument is the blast-radius guard: a
// rep_id that belongs to another document is ignored, so a caller that mixes up
// two documents cannot retire the wrong corpus.
func TestSoftDeleteRepresentations_ScopedToDocument(t *testing.T) {
	ctx := context.Background()
	st := newReconcileStore(t)
	aDocID := addDoc(t, st, "media/a.mp4")
	bDocID := addDoc(t, st, "media/b.mp4")
	aRepID := addRep(t, st, aDocID, "transcript", `{"source":"stt"}`)
	bRepID := addRep(t, st, bDocID, "transcript", `{"source":"stt"}`)

	retired, err := st.SoftDeleteRepresentations(ctx, "media/a.mp4", []int64{aRepID, bRepID})
	if err != nil {
		t.Fatalf("SoftDeleteRepresentations: %v", err)
	}
	if retired != 1 {
		t.Fatalf("retired = %d, want 1 (the foreign rep_id must be ignored)", retired)
	}

	bReps, err := st.ActiveRepresentations(ctx, "media/b.mp4")
	if err != nil {
		t.Fatalf("ActiveRepresentations(b): %v", err)
	}
	if len(bReps) != 1 {
		t.Errorf("another document's representation was retired: %+v", bReps)
	}
}

// TestPipelineOutputIdentity_RoundTrip pins the setting the reconciliation gate
// reads once per scan: absent reads as empty, and a recorded value survives.
func TestPipelineOutputIdentity_RoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newReconcileStore(t)

	got, err := st.PipelineOutputIdentity(ctx)
	if err != nil {
		t.Fatalf("PipelineOutputIdentity (absent): %v", err)
	}
	if got != "" {
		t.Fatalf("absent identity = %q, want empty", got)
	}

	if err := st.SetPipelineOutputIdentity(ctx, "translate=on:en,es|summary=off"); err != nil {
		t.Fatalf("SetPipelineOutputIdentity: %v", err)
	}
	got, err = st.PipelineOutputIdentity(ctx)
	if err != nil {
		t.Fatalf("PipelineOutputIdentity: %v", err)
	}
	if got != "translate=on:en,es|summary=off" {
		t.Errorf("identity = %q, want the recorded value", got)
	}
}
