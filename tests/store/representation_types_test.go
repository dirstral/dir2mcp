package tests

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestRepresentationTypesByPath pins the read side of the batch run manifest's
// "outputs produced" field (SPEC §8.6.11): the distinct, sorted rep_types of a
// document's active representations, with a missing document reported as an empty
// slice (no outputs) rather than an error.
func TestRepresentationTypesByPath(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: "media/a.mp4", DocType: "video", ContentHash: "h1", Status: "ok",
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	doc, err := st.GetDocumentByPath(ctx, "media/a.mp4")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}

	// Two distinct active rep types (out of insertion order, to prove sorting).
	for _, r := range []model.Representation{
		{DocID: doc.DocID, RepType: "transcript-es", RepHash: "r1"},
		{DocID: doc.DocID, RepType: "transcript", RepHash: "r2"},
	} {
		if _, err := st.UpsertRepresentation(ctx, r); err != nil {
			t.Fatalf("UpsertRepresentation %s: %v", r.RepType, err)
		}
	}

	types, err := st.RepresentationTypesByPath(ctx, "media/a.mp4")
	if err != nil {
		t.Fatalf("RepresentationTypesByPath: %v", err)
	}
	want := []string{"transcript", "transcript-es"}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("types = %v, want %v (distinct + sorted)", types, want)
	}

	// A missing document yields an empty slice and a nil error.
	missing, err := st.RepresentationTypesByPath(ctx, "media/nope.mp4")
	if err != nil {
		t.Fatalf("RepresentationTypesByPath(missing): %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("missing doc types = %v, want empty", missing)
	}
}
