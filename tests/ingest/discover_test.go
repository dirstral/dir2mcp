package tests

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"dir2mcp/internal/ingest"
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

	if err := os.Symlink(filepath.Join(root, "target.txt"), filepath.Join(root, "alias.txt")); err != nil {
		t.Fatalf("create in-root symlink: %v", err)
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
