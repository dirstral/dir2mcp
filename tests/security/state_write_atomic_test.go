package tests

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dirstral/dir2mcp/internal/statefs"
)

// WriteFileAtomic publishes a cache entry only after the complete write. A
// failed write must leave no partial file at the target path and no temporary
// file in the directory, so a later read never takes a truncated entry as a
// cache hit.

func TestWriteFileAtomic_WritesAndReplacesOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "entry.md")
	if err := os.WriteFile(path, []byte("old and longer content"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := statefs.WriteFileAtomic(path, []byte("new")); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new" {
		t.Fatalf("content = %q, %v; want %q", got, err, "new")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if mode := info.Mode().Perm(); mode != statefs.FileMode {
			t.Fatalf("mode = %o, want %o", mode, statefs.FileMode)
		}
	}
	assertOnlyEntries(t, dir, "entry.md")
}

func TestWriteFileAtomic_FailedPublishLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	// A non-empty directory at the target path makes the rename fail after
	// the temporary file is complete.
	target := filepath.Join(dir, "entry.md")
	if err := os.MkdirAll(filepath.Join(target, "child"), 0o700); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	if err := statefs.WriteFileAtomic(target, []byte("data")); err == nil {
		t.Fatal("WriteFileAtomic over a non-empty directory must fail")
	}
	assertOnlyEntries(t, dir, "entry.md")
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatalf("target must stay the original directory: %v", err)
	}
}

func TestWriteFileAtomic_MissingDirectoryFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "entry.md")
	if err := statefs.WriteFileAtomic(path, []byte("data")); err == nil {
		t.Fatal("WriteFileAtomic into a missing directory must fail")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("no file may exist at the target: %v", err)
	}
}

// assertOnlyEntries fails unless dir holds exactly the named entries.
func assertOnlyEntries(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != len(names) {
		var got []string
		for _, e := range entries {
			got = append(got, e.Name())
		}
		t.Fatalf("dir entries = %v, want %v", got, names)
	}
	for i, e := range entries {
		if e.Name() != names[i] {
			t.Fatalf("dir entry %d = %q, want %q", i, e.Name(), names[i])
		}
	}
}
