package tests

import (
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/scancache"
)

func TestSQLiteCache_StoreAndLookupRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache", "scan.sqlite")
	c := scancache.Open(path)
	defer func() { _ = c.Close() }()

	// Nanosecond values with a non-zero sub-second part (#667). A seconds-magnitude
	// value would round-trip even through a column that silently lost precision.
	const dirNano = int64(1700000000_123456789)
	const fileNano = int64(1699999999_987654321)
	sig := corpusfs.CachedDirSignature{
		DirMTimeUnixNano: dirNano,
		Entries: []corpusfs.CachedDirEntry{
			{Name: "a.txt", SizeBytes: 5, MTimeUnixNano: fileNano, Mode: 0o644},
			{Name: "sub", IsDir: true},
		},
	}
	if err := c.StoreDir("docs", sig); err != nil {
		t.Fatalf("StoreDir: %v", err)
	}

	got, ok, err := c.LookupDir("docs")
	if err != nil {
		t.Fatalf("LookupDir: %v", err)
	}
	if !ok {
		t.Fatalf("LookupDir: expected hit")
	}
	if got.DirMTimeUnixNano != sig.DirMTimeUnixNano {
		t.Fatalf("mtime mismatch: got %d want %d", got.DirMTimeUnixNano, sig.DirMTimeUnixNano)
	}
	if len(got.Entries) != 2 {
		t.Fatalf("entry count mismatch: got %d want 2", len(got.Entries))
	}
	if got.Entries[0].Name != "a.txt" || got.Entries[0].SizeBytes != 5 || got.Entries[0].Mode != 0o644 {
		t.Fatalf("file entry round-trip mismatch: %+v", got.Entries[0])
	}
	if got.Entries[0].MTimeUnixNano != fileNano {
		t.Fatalf("child mtime lost precision: got %d want %d", got.Entries[0].MTimeUnixNano, fileNano)
	}
	if !got.Entries[1].IsDir || got.Entries[1].Name != "sub" {
		t.Fatalf("dir entry round-trip mismatch: %+v", got.Entries[1])
	}
}

func TestSQLiteCache_LookupMissReturnsNoError(t *testing.T) {
	c := scancache.Open(filepath.Join(t.TempDir(), "scan.sqlite"))
	defer func() { _ = c.Close() }()

	_, ok, err := c.LookupDir("nope")
	if err != nil {
		t.Fatalf("LookupDir miss should not error: %v", err)
	}
	if ok {
		t.Fatalf("LookupDir: expected miss")
	}
}

func TestSQLiteCache_StoreUpsertsAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scan.sqlite")

	c := scancache.Open(path)
	if err := c.StoreDir("d", corpusfs.CachedDirSignature{DirMTimeUnixNano: 1}); err != nil {
		t.Fatalf("first store: %v", err)
	}
	// Overwrite the same key with a new mtime.
	if err := c.StoreDir("d", corpusfs.CachedDirSignature{DirMTimeUnixNano: 2}); err != nil {
		t.Fatalf("upsert store: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen the same file: the value must survive (persistence) and reflect the
	// upsert (mtime=2, not 1).
	reopened := scancache.Open(path)
	defer func() { _ = reopened.Close() }()
	got, ok, err := reopened.LookupDir("d")
	if err != nil || !ok {
		t.Fatalf("reopened LookupDir: ok=%v err=%v", ok, err)
	}
	if got.DirMTimeUnixNano != 2 {
		t.Fatalf("upsert not persisted: got mtime %d want 2", got.DirMTimeUnixNano)
	}
}

func TestSQLiteCache_DefaultPath(t *testing.T) {
	want := filepath.Join("state", "cache", "scan.sqlite")
	if got := scancache.DefaultPath("state"); got != want {
		t.Fatalf("DefaultPath: got %q want %q", got, want)
	}
}
