package tests

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// #807: a reindex intermittently finished with documents.content_hash empty and
// nothing reported. The corpus then re-extracts and re-embeds every document on
// the next scan, which costs real provider money.
//
// Two properties are pinned here. Both were absent, and either one alone would
// have turned that failure from silent into loud.
//
// The FILE half of the same staging already refuses to overwrite an unrecovered
// backup, and its comment records why: it used to remove the destination first,
// "which is exactly how the last-known-good generation got destroyed". The SQL
// half still did that, so it is the same defect in the other half of one
// mechanism.

func snapshotStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seedHash(t *testing.T, st *store.SQLiteStore, relPath, hash string) {
	t.Helper()
	if err := st.UpsertDocument(context.Background(), model.Document{
		RelPath: relPath, DocType: "md", ContentHash: hash, Status: "ok",
	}); err != nil {
		t.Fatalf("seed %s: %v", relPath, err)
	}
}

func hashOf(t *testing.T, st *store.SQLiteStore, relPath string) string {
	t.Helper()
	doc, err := st.GetDocumentByPath(context.Background(), relPath)
	if err != nil {
		t.Fatalf("read %s: %v", relPath, err)
	}
	return doc.ContentHash
}

// TestSnapshotRefusesToOverwriteAnUnrecoveredOne is the suspected root cause.
// A crashed run leaves a good snapshot behind and the hashes cleared. If the
// next run snapshots again before restoring, it copies the CLEARED hashes over
// the good snapshot, and the later restore then puts empty strings back.
func TestSnapshotRefusesToOverwriteAnUnrecoveredOne(t *testing.T) {
	ctx := context.Background()
	st := snapshotStore(t)
	seedHash(t, st, "docs/a.md", "GOOD-HASH")

	// The crashed run: snapshot, clear, then die without resolving either.
	if err := st.BackupContentHashes(ctx); err != nil {
		t.Fatalf("first snapshot: %v", err)
	}
	if err := st.ClearDocumentContentHashes(ctx); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := hashOf(t, st, "docs/a.md"); got != "" {
		t.Fatalf("precondition: the hash should be cleared, got %q", got)
	}

	// The next run snapshots without restoring first. This must refuse.
	err := st.BackupContentHashes(ctx)
	if err == nil {
		t.Fatal("a second snapshot overwrote the unrecovered one; the good hashes are now lost")
	}

	// And the good snapshot must still be intact, so a restore recovers it.
	if err := st.RestoreContentHashes(ctx); err != nil {
		t.Fatalf("restore after the refusal: %v", err)
	}
	if got := hashOf(t, st, "docs/a.md"); got != "GOOD-HASH" {
		t.Fatalf("the refusal did not protect the snapshot; want GOOD-HASH got %q", got)
	}
}

// TestRestoreReportsThatItRestoredNothing: a restore that matched no rows is a
// failure, not a no-op. It used to return nil, so the corpus could end with no
// content hashes and the run still exited 0.
func TestRestoreReportsThatItRestoredNothing(t *testing.T) {
	ctx := context.Background()
	st := snapshotStore(t)

	// A snapshot taken over an EMPTY corpus matches nothing on restore.
	if err := st.BackupContentHashes(ctx); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	seedHash(t, st, "docs/late.md", "ARRIVED-AFTER-THE-SNAPSHOT")

	err := st.RestoreContentHashes(ctx)
	if !errors.Is(err, store.ErrEmptyContentHashSnapshot) {
		t.Fatalf("a restore that matched nothing must say so, got %v", err)
	}
}

// TestRestoreDistinguishesAbsentFromEmpty: the two cases need different
// handling by the caller. At startup an absent snapshot is the ordinary state
// of a healthy corpus; after a clear it means the hashes are gone.
func TestRestoreDistinguishesAbsentFromEmpty(t *testing.T) {
	ctx := context.Background()
	st := snapshotStore(t)
	seedHash(t, st, "docs/a.md", "GOOD-HASH")

	if err := st.RestoreContentHashes(ctx); !errors.Is(err, store.ErrNoContentHashSnapshot) {
		t.Fatalf("with no snapshot the restore must report absence, got %v", err)
	}
	if got := hashOf(t, st, "docs/a.md"); got != "GOOD-HASH" {
		t.Fatalf("an absent snapshot must not disturb the hash, got %q", got)
	}
}

// TestASuccessfulRestoreStillReportsSuccess is the guard against over-reporting.
// The ordinary rollback path must stay quiet, or every reindex failure would
// print a scary warning that does not apply.
func TestASuccessfulRestoreStillReportsSuccess(t *testing.T) {
	ctx := context.Background()
	st := snapshotStore(t)
	seedHash(t, st, "docs/a.md", "GOOD-HASH")

	if err := st.BackupContentHashes(ctx); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := st.ClearDocumentContentHashes(ctx); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if err := st.RestoreContentHashes(ctx); err != nil {
		t.Fatalf("an ordinary restore must succeed quietly, got %v", err)
	}
	if got := hashOf(t, st, "docs/a.md"); got != "GOOD-HASH" {
		t.Fatalf("restore did not put the hash back; got %q", got)
	}
}
