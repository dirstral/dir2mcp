package tests

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/ingest"
)

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestLocalFSWalk_MatchesDiscoverFiles asserts the LocalFS walker is
// behavior-preserving: it returns the same files (rel paths, sizes, mtimes) as
// the legacy ingest.DiscoverFiles entry point over the same tree.
func TestLocalFSWalk_MatchesDiscoverFiles(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "keep.txt"), []byte("hello"))
	mustWrite(t, filepath.Join(root, "src", "main.go"), []byte("package main\n"))
	mustWrite(t, filepath.Join(root, "docs", "readme.md"), []byte("# title\n"))
	mustWrite(t, filepath.Join(root, ".git", "config"), []byte("[core]"))
	mustWrite(t, filepath.Join(root, "node_modules", "lib.js"), []byte("x"))
	mustWrite(t, filepath.Join(root, "big.bin"), make([]byte, 4096))

	maxSize := int64(1024)

	want, err := ingest.DiscoverFiles(context.Background(), root, maxSize)
	if err != nil {
		t.Fatalf("DiscoverFiles: %v", err)
	}

	got, err := corpusfs.NewLocalFS(root).Walk(context.Background(), root, corpusfs.Options{MaxSizeBytes: maxSize})
	if err != nil {
		t.Fatalf("LocalFS.Walk: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("file count mismatch: walk=%d discover=%d", len(got), len(want))
	}
	for i := range want {
		if got[i].RelPath != want[i].RelPath {
			t.Fatalf("relpath[%d] mismatch: walk=%q discover=%q", i, got[i].RelPath, want[i].RelPath)
		}
		if got[i].AbsPath != want[i].AbsPath {
			t.Fatalf("abspath[%d] mismatch: walk=%q discover=%q", i, got[i].AbsPath, want[i].AbsPath)
		}
		if got[i].SizeBytes != want[i].SizeBytes {
			t.Fatalf("size[%d] mismatch: walk=%d discover=%d", i, got[i].SizeBytes, want[i].SizeBytes)
		}
		if got[i].MTimeUnix != want[i].MTimeUnix {
			t.Fatalf("mtime[%d] mismatch: walk=%d discover=%d", i, got[i].MTimeUnix, want[i].MTimeUnix)
		}
		if got[i].ETag != "" {
			t.Fatalf("LocalFS ETag should be empty, got %q", got[i].ETag)
		}
	}

	// Spot-check the exclusions: .git, node_modules, and the over-cap file are
	// absent; the regular files are present.
	relset := map[string]bool{}
	for _, f := range got {
		relset[f.RelPath] = true
	}
	for _, want := range []string{"keep.txt", "src/main.go", "docs/readme.md"} {
		if !relset[want] {
			t.Fatalf("expected %q in walk output", want)
		}
	}
	for _, bad := range []string{".git/config", "node_modules/lib.js", "big.bin"} {
		if relset[bad] {
			t.Fatalf("did not expect %q in walk output", bad)
		}
	}
}

// TestLocalFSOpen_RoundTrip verifies Open returns the file bytes and supports
// seeking.
func TestLocalFSOpen_RoundTrip(t *testing.T) {
	root := t.TempDir()
	body := []byte("the quick brown fox")
	mustWrite(t, filepath.Join(root, "a", "b.txt"), body)

	fsys := corpusfs.NewLocalFS(root)
	rc, err := fsys.Open(context.Background(), "a/b.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("body mismatch: got %q want %q", got, body)
	}

	// Seek back to a fixed offset and re-read a slice.
	if _, err := rc.Seek(4, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(rc, buf); err != nil {
		t.Fatalf("ReadFull after seek: %v", err)
	}
	if string(buf) != "quick" {
		t.Fatalf("seek/read mismatch: got %q want %q", buf, "quick")
	}
}

// TestLocalFSLocalize_RoundTrip verifies Localize returns the in-root path and a
// no-op cleanup that does not delete the source file.
func TestLocalFSLocalize_RoundTrip(t *testing.T) {
	root := t.TempDir()
	body := []byte("media-bytes")
	rel := "clip.mp3"
	mustWrite(t, filepath.Join(root, rel), body)

	fsys := corpusfs.NewLocalFS(root)
	localPath, cleanup, err := fsys.Localize(context.Background(), rel)
	if err != nil {
		t.Fatalf("Localize: %v", err)
	}
	defer cleanup()

	got, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("read localized path: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("localized body mismatch: got %q want %q", got, body)
	}

	cleanup()
	// LocalFS cleanup must be a no-op: the original file still exists.
	if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
		t.Fatalf("local file should survive cleanup: %v", err)
	}
}

// TestLocalFSContainment_RejectsTraversal verifies that both Open and Localize
// reject paths that escape the corpus root.
func TestLocalFSContainment_RejectsTraversal(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.txt"), []byte("top secret"))
	mustWrite(t, filepath.Join(root, "ok.txt"), []byte("fine"))

	fsys := corpusfs.NewLocalFS(root)

	// Lexical traversal out of the root.
	if _, err := fsys.Open(context.Background(), "../"+filepath.Base(outside)+"/secret.txt"); err == nil {
		t.Fatal("Open: expected traversal rejection")
	}
	if _, _, err := fsys.Localize(context.Background(), "../"+filepath.Base(outside)+"/secret.txt"); err == nil {
		t.Fatal("Localize: expected traversal rejection")
	}

	// Symlink inside the corpus pointing outside it must also be rejected.
	if runtime.GOOS != "windows" {
		link := filepath.Join(root, "escape.txt")
		if err := os.Symlink(filepath.Join(outside, "secret.txt"), link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if _, err := fsys.Open(context.Background(), "escape.txt"); err == nil {
			t.Fatal("Open: expected symlink-escape rejection")
		}
		if _, _, err := fsys.Localize(context.Background(), "escape.txt"); err == nil {
			t.Fatal("Localize: expected symlink-escape rejection")
		}
	}
}
