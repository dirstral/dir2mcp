package tests

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestSQLiteStore_DocumentErrorMessageRoundtrip pins the persistence
// behaviour of documents.error_message: it round-trips through both
// GetDocumentByPath and ListFiles, control characters are stripped so
// the message stays one line in the support bundle, and a successful
// re-ingest (Status="ok", empty ErrorMessage) clears the stale message
// rather than leaving the prior failure footprint in place.
func TestSQLiteStore_DocumentErrorMessageRoundtrip(t *testing.T) {
	ctx := context.Background()
	st := newTempSQLiteStore(t, ctx)
	defer func() { _ = st.Close() }()

	failed := model.Document{
		RelPath:      "broken.pdf",
		DocType:      "pdf",
		SizeBytes:    2048,
		MTimeUnix:    1_700_000_000,
		Status:       "error",
		ErrorMessage: "docling extraction failed\n\tunsupported PDF version 1.7",
	}
	if err := st.UpsertDocument(ctx, failed); err != nil {
		t.Fatalf("UpsertDocument(failed): %v", err)
	}

	assertGetErrorMessageSanitized(t, ctx, st, failed.RelPath)
	assertListFilesCarriesErrorMessage(t, ctx, st, failed.RelPath)
	assertSuccessfulReingestClearsMessage(t, ctx, st, failed)
}

// assertGetErrorMessageSanitized verifies GetDocumentByPath returns a
// non-empty error_message with its embedded control characters
// stripped — the field must stay one line so the support bundle is
// readable per-row.
func assertGetErrorMessageSanitized(t *testing.T, ctx context.Context, st *store.SQLiteStore, relPath string) {
	t.Helper()
	got, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath(%s): %v", relPath, err)
	}
	if got.Status != "error" {
		t.Errorf("status = %q, want error", got.Status)
	}
	if !strings.Contains(got.ErrorMessage, "docling extraction failed") {
		t.Errorf("error_message lost substance: %q", got.ErrorMessage)
	}
	if strings.ContainsAny(got.ErrorMessage, "\n\r\t") {
		t.Errorf("error_message should have control chars stripped: %q", got.ErrorMessage)
	}
}

// assertListFilesCarriesErrorMessage verifies the same error_message
// is exposed via ListFiles so the support bundle can carry it without
// an extra per-doc lookup.
func assertListFilesCarriesErrorMessage(t *testing.T, ctx context.Context, st *store.SQLiteStore, relPath string) {
	t.Helper()
	docs, _, err := st.ListFiles(ctx, "", "", 10, 0)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	for _, d := range docs {
		if d.RelPath != relPath {
			continue
		}
		if !strings.Contains(d.ErrorMessage, "docling extraction failed") {
			t.Errorf("ListFiles error_message = %q, want substring 'docling extraction failed'", d.ErrorMessage)
		}
		return
	}
	t.Fatalf("ListFiles missing %s", relPath)
}

// assertSuccessfulReingestClearsMessage pins the "clear stale" half of
// the contract: a re-ingest with Status="ok" and empty ErrorMessage
// must wipe the prior failure message, not leave it visible to
// doctor / support-bundle as if the document were still broken.
func assertSuccessfulReingestClearsMessage(t *testing.T, ctx context.Context, st *store.SQLiteStore, prior model.Document) {
	t.Helper()
	recovered := prior
	recovered.Status = "ok"
	recovered.ErrorMessage = ""
	if err := st.UpsertDocument(ctx, recovered); err != nil {
		t.Fatalf("UpsertDocument(recovered): %v", err)
	}
	got, err := st.GetDocumentByPath(ctx, recovered.RelPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath(recovered): %v", err)
	}
	if got.ErrorMessage != "" {
		t.Errorf("error_message not cleared after successful re-ingest: %q", got.ErrorMessage)
	}
	if got.Status != "ok" {
		t.Errorf("status = %q, want ok", got.Status)
	}
}

// TestSQLiteStore_DocumentErrorMessageTruncation verifies that runaway
// upstream errors (stack traces, blob dumps) are capped before they
// reach the documents table so a single bad doc can't bloat the store.
func TestSQLiteStore_DocumentErrorMessageTruncation(t *testing.T) {
	ctx := context.Background()
	st := newTempSQLiteStore(t, ctx)
	defer func() { _ = st.Close() }()

	huge := strings.Repeat("A", 4096)
	doc := model.Document{
		RelPath:      "huge-error.pdf",
		DocType:      "pdf",
		Status:       "error",
		ErrorMessage: huge,
	}
	if err := st.UpsertDocument(ctx, doc); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	got, err := st.GetDocumentByPath(ctx, doc.RelPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if len(got.ErrorMessage) > 512 {
		t.Errorf("error_message len = %d, want <= 512 (truncation cap)", len(got.ErrorMessage))
	}
	if len(got.ErrorMessage) == 0 {
		t.Errorf("error_message was emptied entirely; expected truncation, not deletion")
	}
}

func newTempSQLiteStore(t *testing.T, ctx context.Context) *store.SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return st
}
