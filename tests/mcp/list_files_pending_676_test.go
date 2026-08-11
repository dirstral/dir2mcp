package tests

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// #676: `list_files` reported a `pending` document as `status: "ok"`.
//
// `normalizeFileStatus` recognised `skipped`, `secret_excluded` and `error`,
// so a row the corpus knows about and has not finished indexing fell through
// the default arm. An agent reads `ok` as "indexed and readable", asks for the
// content through `search` or `open_file`, and gets nothing back. The document
// has no chunks yet.
//
// This half of the audit needed the spec to move first. The published enum was
// `ok|skipped|error`, and neither value was honest: `skipped` means "not
// indexed, and not going to be", which is a different claim with no matching
// skip reason, and `error` is false. SPEC 0.48.0 added `pending` for exactly
// this row and made the storage-to-public mapping normative, including the rule
// this defect broke: a server MUST NOT report a document as `ok` unless it is
// retrievable now.
//
// The sibling file (#712) covers the `secret_excluded` half of the same audit.

// TestListFilesReportsAPendingDocumentAsPending is the contract. A document
// that is not retrievable yet must not be advertised as indexed.
func TestListFilesReportsAPendingDocumentAsPending(t *testing.T) {
	tmp := t.TempDir()
	st := store.NewSQLiteStore(filepath.Join(tmp, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seed := []model.Document{
		{RelPath: "notes/indexed.md", DocType: "md", MTimeUnix: 100, Status: "ok"},
		{RelPath: "notes/in-progress.md", DocType: "md", MTimeUnix: 200, Status: "pending"},
	}
	root := filepath.Join(tmp, "corpus")
	seedCorpus(t, st, root, seed)

	statuses := listFileStatuses(t, tmp, root, st)

	if got := statuses["notes/in-progress.md"]; got != "pending" {
		t.Fatalf("a document that is not indexed yet is reported as %q; a caller reads that as indexed and readable, then asks for content that does not exist (#676)", got)
	}
	// A document that IS retrievable must keep saying so. The fix must not turn
	// the honest answer into a hedge.
	if got := statuses["notes/indexed.md"]; got != "ok" {
		t.Fatalf("indexed document reported as %q, want ok", got)
	}
}

// TestListFilesNeverReportsUnfinishedWorkAsOK states the SPEC 0.48.0 rule
// directly, rather than the one value that broke it: `ok` is a promise that the
// document is retrievable now, so no state that means "not yet" may map onto
// it. A future store state that reuses the default arm would pass the test
// above and fail this one.
func TestListFilesNeverReportsUnfinishedWorkAsOK(t *testing.T) {
	tmp := t.TempDir()
	st := store.NewSQLiteStore(filepath.Join(tmp, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Every stored state that means "this document is not retrievable".
	notRetrievable := []string{"pending", "skipped", "secret_excluded", "error"}
	docs := make([]model.Document, 0, len(notRetrievable))
	for i, status := range notRetrievable {
		docs = append(docs, model.Document{
			RelPath:   "f" + string(rune('a'+i)) + ".md",
			DocType:   "md",
			MTimeUnix: int64(100 * (i + 1)),
			Status:    status,
		})
	}
	root := filepath.Join(tmp, "corpus")
	seedCorpus(t, st, root, docs)

	for relPath, status := range listFileStatuses(t, tmp, root, st) {
		if status == "ok" {
			t.Errorf("%s is not retrievable in the store, but list_files reports it as ok; SPEC 15.5 forbids reporting work that has not finished as ok", relPath)
		}
	}
}
