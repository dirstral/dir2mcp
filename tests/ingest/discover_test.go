package tests

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
)

func TestDiscoverFiles_SkipsDefaultExcludedDirsSymlinksAndLargeFiles(t *testing.T) {
	root := t.TempDir()

	mustWriteFile(t, filepath.Join(root, "keep.txt"), []byte("hello"))
	mustWriteFile(t, filepath.Join(root, "src", "main.go"), []byte("package main\n"))
	mustWriteFile(t, filepath.Join(root, ".git", "config"), []byte("[core]"))
	mustWriteFile(t, filepath.Join(root, "node_modules", "lib.js"), []byte("module.exports={}"))
	mustWriteFile(t, filepath.Join(root, "vendor", "dep.go"), []byte("package dep\n"))
	mustWriteFile(t, filepath.Join(root, "__pycache__", "x.pyc"), []byte{0x1, 0x2, 0x3})
	mustWriteFile(t, filepath.Join(root, "too-big.bin"), []byte("0123456789ABCDEF"))

	// Best-effort symlink setup; Windows CI often disallows it without elevated privileges.
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(root, "keep.txt"), filepath.Join(root, "keep-link.txt")); err != nil {
			t.Fatalf("create symlink: %v", err)
		}
	}

	files, err := ingest.DiscoverFiles(context.Background(), root, 10)
	if err != nil {
		t.Fatalf("DiscoverFiles failed: %v", err)
	}

	got := make([]string, 0, len(files))
	for _, f := range files {
		got = append(got, f.RelPath)
	}

	want := []string{"keep.txt"}
	if !slices.Equal(got, want) {
		t.Fatalf("unexpected files:\nwant=%v\ngot=%v", want, got)
	}
}

// TestDiscoverFilesWithOptions_OnOversize_SurfacesSizeCapDrops asserts that a
// file excluded solely because it exceeds MaxSizeBytes is reported via the
// OnOversize hook (issue #497) instead of vanishing silently — the operator must
// have a signal that files were dropped, not just an unexplained skipped=0.
func TestDiscoverFilesWithOptions_OnOversize_SurfacesSizeCapDrops(t *testing.T) {
	root := t.TempDir()

	mustWriteFile(t, filepath.Join(root, "keep.txt"), []byte("hi"))
	// 32 bytes, well over the 16-byte (rounded-down) cap below.
	big := []byte("0123456789ABCDEF0123456789ABCDEF")
	mustWriteFile(t, filepath.Join(root, "media", "big.mp4"), big)

	type drop struct {
		relPath string
		size    int64
	}
	var drops []drop

	files, err := ingest.DiscoverFilesWithOptions(context.Background(), root, ingest.DiscoverOptions{
		MaxSizeBytes: 16,
		OnOversize: func(relPath string, size int64) {
			drops = append(drops, drop{relPath: relPath, size: size})
		},
	})
	if err != nil {
		t.Fatalf("DiscoverFilesWithOptions failed: %v", err)
	}

	got := make([]string, 0, len(files))
	for _, f := range files {
		got = append(got, f.RelPath)
	}
	if !slices.Equal(got, []string{"keep.txt"}) {
		t.Fatalf("expected only the small file discovered, got %v", got)
	}

	if len(drops) != 1 {
		t.Fatalf("expected exactly one oversize drop reported, got %d: %+v", len(drops), drops)
	}
	if drops[0].relPath != "media/big.mp4" {
		t.Fatalf("unexpected oversize relPath: %q", drops[0].relPath)
	}
	if drops[0].size != int64(len(big)) {
		t.Fatalf("unexpected oversize size: got %d want %d", drops[0].size, len(big))
	}
}

// TestDiscoverFilesWithOptions_OnOversize_NotCalledWhenAllFit guards against a
// false positive: the hook must fire only for genuine size-cap exclusions, not
// for every discovered file.
func TestDiscoverFilesWithOptions_OnOversize_NotCalledWhenAllFit(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), []byte("x"))
	mustWriteFile(t, filepath.Join(root, "b.txt"), []byte("y"))

	called := 0
	if _, err := ingest.DiscoverFilesWithOptions(context.Background(), root, ingest.DiscoverOptions{
		MaxSizeBytes: 1024,
		OnOversize:   func(string, int64) { called++ },
	}); err != nil {
		t.Fatalf("DiscoverFilesWithOptions failed: %v", err)
	}
	if called != 0 {
		t.Fatalf("OnOversize should not fire when every file fits the cap, got %d calls", called)
	}
}

func TestDiscoverFiles_ContextCancelled(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), []byte("x"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := ingest.DiscoverFiles(ctx, root, 1024); err == nil {
		t.Fatal("expected context cancellation error")
	}
}

func TestDiscoverFilesWithOptions_GitIgnore(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "keep.txt"), []byte("hello"))
	mustWriteFile(t, filepath.Join(root, "ignore.tmp"), []byte("tmp"))
	mustWriteFile(t, filepath.Join(root, "secret.env"), []byte("env"))
	mustWriteFile(t, filepath.Join(root, ".gitignore"), []byte("*.tmp\n*.env\n.gitignore\n"))

	files, err := ingest.DiscoverFilesWithOptions(context.Background(), root, ingest.DiscoverOptions{
		MaxSizeBytes: 1024,
		UseGitIgnore: true,
	})
	if err != nil {
		t.Fatalf("DiscoverFilesWithOptions failed: %v", err)
	}

	got := make([]string, 0, len(files))
	for _, f := range files {
		got = append(got, f.RelPath)
	}
	if !slices.Equal(got, []string{"keep.txt"}) {
		t.Fatalf("unexpected files with gitignore enabled: %v", got)
	}
}

func TestDiscoverFilesWithOptions_FollowSymlinks_RespectsRootAndPreventsCycles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior requires elevated privileges on windows")
	}

	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "target.txt"), []byte("in-root"))
	mustWriteFile(t, filepath.Join(root, "loop", "inside.txt"), []byte("loop"))
	mustWriteFile(t, filepath.Join(root, "..cache", "inner.txt"), []byte("dotdot"))

	if err := os.Symlink(filepath.Join(root, "target.txt"), filepath.Join(root, "alias.txt")); err != nil {
		t.Fatalf("create in-root symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "..cache", "inner.txt"), filepath.Join(root, "dotdot-cache-link.txt")); err != nil {
		t.Fatalf("create dotdot in-root symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "loop"), filepath.Join(root, "loop", "self")); err != nil {
		t.Fatalf("create cycle symlink: %v", err)
	}

	outsideRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(outsideRoot, "outside.txt"), []byte("outside"))
	if err := os.Symlink(filepath.Join(outsideRoot, "outside.txt"), filepath.Join(root, "outside-link.txt")); err != nil {
		t.Fatalf("create outside symlink: %v", err)
	}

	files, err := ingest.DiscoverFilesWithOptions(context.Background(), root, ingest.DiscoverOptions{
		MaxSizeBytes:   1024,
		FollowSymlinks: true,
	})
	if err != nil {
		t.Fatalf("DiscoverFilesWithOptions failed: %v", err)
	}

	got := make([]string, 0, len(files))
	for _, f := range files {
		got = append(got, f.RelPath)
	}

	if !slices.Contains(got, "alias.txt") {
		t.Fatalf("expected followed in-root symlink file, got %v", got)
	}
	if !slices.Contains(got, "dotdot-cache-link.txt") {
		t.Fatalf("expected followed in-root '..cache' symlink file, got %v", got)
	}
	if slices.Contains(got, "outside-link.txt") {
		t.Fatalf("outside-root symlink should be skipped, got %v", got)
	}
	if !slices.Contains(got, "loop/inside.txt") {
		t.Fatalf("expected file in loop directory, got %v", got)
	}
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}
