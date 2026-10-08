package tests

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// newArtifactStore opens a fresh store with two media documents and returns it
// with their doc_ids.
func newArtifactStore(t *testing.T) (*store.SQLiteStore, int64, int64) {
	t.Helper()
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(testutil.TempDir(t), "meta.sqlite"))
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	for _, rel := range []string{"media/a.mp3", "media/b.mp3"} {
		if err := st.UpsertDocument(ctx, model.Document{RelPath: rel, DocType: "audio", SourceType: "local", Status: "ok"}); err != nil {
			t.Fatalf("UpsertDocument %s: %v", rel, err)
		}
	}
	a, _ := st.GetDocumentByPath(ctx, "media/a.mp3")
	b, _ := st.GetDocumentByPath(ctx, "media/b.mp3")
	return st, a.DocID, b.DocID
}

func mustUpsertArtifact(t *testing.T, st *store.SQLiteStore, a store.EmittedArtifact) {
	t.Helper()
	if err := st.UpsertEmittedArtifact(context.Background(), a); err != nil {
		t.Fatalf("UpsertEmittedArtifact %s: %v", a.RelPath, err)
	}
}

// artifactRelPaths returns the rel_paths of rows in order.
func artifactRelPaths(rows []store.EmittedArtifact) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.RelPath)
	}
	return out
}

func assertRelPaths(t *testing.T, rows []store.EmittedArtifact, want ...string) {
	t.Helper()
	got := artifactRelPaths(rows)
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rows = %v, want %v", got, want)
		}
	}
}

// TestEmittedArtifacts_UpsertAndList pins the df-003 §5.6 ownership table's
// write and read side: fields round-trip, per-document and whole-table listing
// come back in rel_path order, and a second upsert of the same rel_path
// replaces the row.
func TestEmittedArtifacts_UpsertAndList(t *testing.T) {
	ctx := context.Background()
	st, a, b := newArtifactStore(t)
	mustUpsertArtifact(t, st, store.EmittedArtifact{RelPath: "media/a.ttml", DocID: a, Format: "ttml", SizeBytes: 10, MTimeUnix: 100, ContentSHA256: "aa", EmittedUnix: 1})
	mustUpsertArtifact(t, st, store.EmittedArtifact{RelPath: "media/a.de.vtt", DocID: a, Format: "vtt", Lang: "de", SizeBytes: 20, MTimeUnix: 200, ContentSHA256: "bb", EmittedUnix: 1})
	mustUpsertArtifact(t, st, store.EmittedArtifact{RelPath: "media/b.de.vtt", DocID: b, Format: "vtt", Lang: "de", SizeBytes: 30, MTimeUnix: 300, ContentSHA256: "cc", EmittedUnix: 1})

	got, err := st.EmittedArtifactsForDoc(ctx, a)
	if err != nil {
		t.Fatalf("EmittedArtifactsForDoc: %v", err)
	}
	assertRelPaths(t, got, "media/a.de.vtt", "media/a.ttml")
	if got[0].Lang != "de" || got[0].SizeBytes != 20 || got[0].MTimeUnix != 200 || got[0].ContentSHA256 != "bb" {
		t.Fatalf("row fields not round-tripped: %+v", got[0])
	}

	mustUpsertArtifact(t, st, store.EmittedArtifact{RelPath: "media/a.de.vtt", DocID: a, Format: "vtt", Lang: "de", SizeBytes: 21, MTimeUnix: 201, ContentSHA256: "bb2", EmittedUnix: 2})
	all, err := st.AllEmittedArtifacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertRelPaths(t, all, "media/a.de.vtt", "media/a.ttml", "media/b.de.vtt")
	if all[0].SizeBytes != 21 || all[0].ContentSHA256 != "bb2" {
		t.Fatalf("upsert did not replace: %+v", all[0])
	}
}

// TestEmittedArtifacts_DeleteAndTombstoneCascade pins removal: an explicit
// delete drops one row (deleting an unknown row is not an error), and
// tombstoning a document drops all of its rows while another document's stay.
func TestEmittedArtifacts_DeleteAndTombstoneCascade(t *testing.T) {
	ctx := context.Background()
	st, a, b := newArtifactStore(t)
	mustUpsertArtifact(t, st, store.EmittedArtifact{RelPath: "media/a.ttml", DocID: a, Format: "ttml", EmittedUnix: 1})
	mustUpsertArtifact(t, st, store.EmittedArtifact{RelPath: "media/a.de.vtt", DocID: a, Format: "vtt", Lang: "de", EmittedUnix: 1})
	mustUpsertArtifact(t, st, store.EmittedArtifact{RelPath: "media/b.de.vtt", DocID: b, Format: "vtt", Lang: "de", EmittedUnix: 1})

	if err := st.DeleteEmittedArtifact(ctx, "media/a.ttml"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteEmittedArtifact(ctx, "media/never-existed.vtt"); err != nil {
		t.Fatalf("deleting an unknown row must not error: %v", err)
	}
	got, _ := st.EmittedArtifactsForDoc(ctx, a)
	assertRelPaths(t, got, "media/a.de.vtt")

	if err := st.MarkDocumentDeleted(ctx, "media/a.mp3"); err != nil {
		t.Fatalf("MarkDocumentDeleted: %v", err)
	}
	all, _ := st.AllEmittedArtifacts(ctx)
	assertRelPaths(t, all, "media/b.de.vtt")
}

// TestEmittedArtifacts_RejectsInvalidRows pins the validation: a row needs a
// document and a format.
func TestEmittedArtifacts_RejectsInvalidRows(t *testing.T) {
	ctx := context.Background()
	st, _, b := newArtifactStore(t)
	if err := st.UpsertEmittedArtifact(ctx, store.EmittedArtifact{RelPath: "x.vtt", Format: "vtt"}); err == nil {
		t.Fatalf("doc_id 0 must be rejected")
	}
	if err := st.UpsertEmittedArtifact(ctx, store.EmittedArtifact{RelPath: "x.vtt", DocID: b}); err == nil {
		t.Fatalf("empty format must be rejected")
	}
}

// TestEmittedArtifacts_OutputRootColumnIsMigrated pins the additive migration
// for emitted_artifacts.output_root: a state database whose table predates the
// column gets it at Init, with the ” default, and a row can then record its
// output root.
func TestEmittedArtifacts_OutputRootColumnIsMigrated(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(testutil.TempDir(t), "meta.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE emitted_artifacts (
  rel_path TEXT PRIMARY KEY,
  doc_id INTEGER NOT NULL,
  format TEXT NOT NULL,
  lang TEXT NOT NULL DEFAULT '',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  mtime_unix INTEGER NOT NULL DEFAULT 0,
  content_sha256 TEXT NOT NULL DEFAULT '',
  emitted_unix INTEGER NOT NULL DEFAULT 0
)`); err != nil {
		t.Fatalf("create old table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st := store.NewSQLiteStore(dbPath)
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init over a table without output_root: %v", err)
	}
	if err := st.UpsertDocument(ctx, model.Document{RelPath: "media/a.mp3", DocType: "audio", SourceType: "local", Status: "ok"}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	doc, err := st.GetDocumentByPath(ctx, "media/a.mp3")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if err := st.UpsertEmittedArtifact(ctx, store.EmittedArtifact{RelPath: "media/a.de.vtt", DocID: doc.DocID, OutputRoot: "/out", Format: "vtt", Lang: "de"}); err != nil {
		t.Fatalf("UpsertEmittedArtifact: %v", err)
	}
	rows, err := st.AllEmittedArtifacts(ctx)
	if err != nil {
		t.Fatalf("AllEmittedArtifacts: %v", err)
	}
	if len(rows) != 1 || rows[0].OutputRoot != "/out" {
		t.Fatalf("expected one row with output_root /out, got %+v", rows)
	}
}
