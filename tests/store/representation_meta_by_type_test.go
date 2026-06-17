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

// TestRepresentationMetaByType covers the read side of the derivation-identity
// re-ingest gate (spec §8.6.7): it returns the active representation's meta_json
// for an exact rep_type, an empty string when the document has no such
// representation, and os.ErrNotExist when the document is missing.
func TestRepresentationMetaByType(t *testing.T) {
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	if err := st.UpsertDocument(ctx, model.Document{RelPath: "talk.mp3", DocType: "audio"}); err != nil {
		t.Fatalf("upsert document: %v", err)
	}
	doc, err := st.GetDocumentByPath(ctx, "talk.mp3")
	if err != nil {
		t.Fatalf("get document: %v", err)
	}

	const meta = `{"source":"stt","provider":"whisper","model":"whisper-large-v3"}`
	if _, err := st.UpsertRepresentation(ctx, model.Representation{
		DocID:    doc.DocID,
		RepType:  "transcript",
		RepHash:  "h1",
		MetaJSON: meta,
	}); err != nil {
		t.Fatalf("upsert representation: %v", err)
	}

	got, err := st.RepresentationMetaByType(ctx, "talk.mp3", "transcript")
	if err != nil {
		t.Fatalf("RepresentationMetaByType(transcript): %v", err)
	}
	if got != meta {
		t.Fatalf("meta = %q, want %q", got, meta)
	}

	// Exact rep_type match: a language-suffixed transcript is not returned for
	// the bare "transcript" lookup.
	if _, err := st.UpsertRepresentation(ctx, model.Representation{
		DocID:    doc.DocID,
		RepType:  "transcript-en",
		RepHash:  "h2",
		MetaJSON: `{"source":"translation"}`,
	}); err != nil {
		t.Fatalf("upsert translated representation: %v", err)
	}
	got, err = st.RepresentationMetaByType(ctx, "talk.mp3", "transcript")
	if err != nil {
		t.Fatalf("RepresentationMetaByType(transcript) after translation: %v", err)
	}
	if got != meta {
		t.Fatalf("bare transcript meta changed by language-suffixed rep: got %q", got)
	}

	// No representation of this type: empty string, nil error.
	got, err = st.RepresentationMetaByType(ctx, "talk.mp3", "extracted_markdown")
	if err != nil {
		t.Fatalf("RepresentationMetaByType(extracted_markdown): %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty meta for absent rep_type, got %q", got)
	}

	// Missing document: os.ErrNotExist.
	if _, err := st.RepresentationMetaByType(ctx, "missing.mp3", "transcript"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing document err = %v, want os.ErrNotExist", err)
	}
}
