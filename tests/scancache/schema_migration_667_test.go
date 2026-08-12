package tests

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/scancache"

	_ "modernc.org/sqlite" // same pure-Go driver the cache itself uses.
)

// TestSQLiteCache667_LegacySecondsTableIsDropped covers the upgrade path for #667.
//
// The old table stored whole seconds in `dir_signatures.dir_mtime_unix`. This build
// reads nanoseconds, so every one of those rows states an instant it does not mean.
// Such a row is harmless (the walker compares it, sees a mismatch, and re-reads the
// directory), but an operator upgrading with a warm cache would carry one dead row
// per directory forever. The cache holds no authoritative state, so it is dropped.
func TestSQLiteCache667_LegacySecondsTableIsDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scan.sqlite")
	seedLegacyCache667(t, path)

	// Open through the cache: the legacy row must not be served.
	c := scancache.Open(path)
	if _, ok, err := c.LookupDir("docs"); err != nil || ok {
		t.Fatalf("legacy row survived the upgrade: ok=%v err=%v", ok, err)
	}
	assertNanoRoundTrip667(t, c)
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	assertTableAbsent667(t, path, "dir_signatures")
}

// seedLegacyCache667 writes a cache file exactly as the previous build left it: a
// `dir_signatures` table holding whole seconds.
func seedLegacyCache667(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close legacy db: %v", err)
		}
	}()
	for _, stmt := range []string{
		`CREATE TABLE dir_signatures (
  rel_dir        TEXT PRIMARY KEY,
  dir_mtime_unix INTEGER NOT NULL,
  entries_json   TEXT NOT NULL
);`,
		`INSERT INTO dir_signatures
		 VALUES ('docs', 1700000000, '[{"Name":"a.txt","SizeBytes":5,"MTimeUnix":1699999999}]');`,
	} {
		if _, err := db.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("seed legacy db: %v", err)
		}
	}
}

// assertNanoRoundTrip667 confirms the upgraded file still works and keeps full
// nanosecond precision.
func assertNanoRoundTrip667(t *testing.T, c *scancache.SQLiteCache) {
	t.Helper()
	want := corpusfs.CachedDirSignature{
		DirMTimeUnixNano: 1700000000_123456789,
		Entries: []corpusfs.CachedDirEntry{
			{Name: "a.txt", SizeBytes: 5, MTimeUnixNano: 1699999999_987654321},
		},
	}
	if err := c.StoreDir("docs", want); err != nil {
		t.Fatalf("StoreDir after upgrade: %v", err)
	}
	got, ok, err := c.LookupDir("docs")
	if err != nil || !ok {
		t.Fatalf("LookupDir after upgrade: ok=%v err=%v", ok, err)
	}
	if got.DirMTimeUnixNano != want.DirMTimeUnixNano {
		t.Fatalf("dir mtime after upgrade: got %d want %d", got.DirMTimeUnixNano, want.DirMTimeUnixNano)
	}
	if len(got.Entries) != 1 || got.Entries[0].MTimeUnixNano != want.Entries[0].MTimeUnixNano {
		t.Fatalf("child mtime after upgrade: got %+v want %+v", got.Entries, want.Entries)
	}
}

// assertTableAbsent667 confirms the named table is gone, not merely unused.
func assertTableAbsent667(t *testing.T, path, table string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer func() { _ = db.Close() }()
	var name string
	err = db.QueryRowContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, table).Scan(&name)
	if err == nil {
		t.Fatalf("legacy table %q still present after the upgrade", name)
	}
	if err != sql.ErrNoRows {
		t.Fatalf("query sqlite_master: %v", err)
	}
}
