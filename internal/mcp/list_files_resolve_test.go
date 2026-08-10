package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TestCanResolveRoot exercises the four shapes that should drive the
// list_files resolvability gate (issue #176 follow-up):
//   - empty RootDir must NOT silently resolve to process CWD; treat it as
//     "not resolvable" so we fall back to the unfiltered listing.
//   - nonexistent path is not resolvable.
//   - a path that exists but is a regular file (not a directory) is not
//     resolvable; otherwise listFilesFiltered would gate every doc against a
//     bogus root and drop them all.
//   - a real directory is resolvable.
func TestCanResolveRoot(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "not-a-dir.txt")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatalf("write probe file: %v", err)
	}

	cases := []struct {
		name string
		root string
		want bool
	}{
		{name: "empty", root: "", want: false},
		{name: "whitespace only", root: "   ", want: false},
		{name: "nonexistent path", root: filepath.Join(dir, "does-not-exist"), want: false},
		{name: "file not dir", root: filePath, want: false},
		{name: "valid dir", root: dir, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.RootDir = tc.root
			s := NewServer(cfg, nil)
			if got := s.canResolveRoot(); got != tc.want {
				t.Fatalf("canResolveRoot(root=%q)=%v want=%v", tc.root, got, tc.want)
			}
		})
	}
}

// TestIsResolvableSourceWithRoot covers the cached per-document helper that
// listFilesFiltered uses in its hot loop. The helper must:
//   - treat archive_member docs as resolvable without touching disk (they're
//     virtual paths backed by an archive),
//   - reject filesystem-source docs whose rel_path doesn't exist under root,
//   - reject path traversal attempts,
//   - accept filesystem-source docs whose rel_path resolves under root.
func TestIsResolvableSourceWithRoot(t *testing.T) {
	rootDir := t.TempDir()
	rootAbs, err := filepath.Abs(rootDir)
	if err != nil {
		t.Fatalf("abs(root): %v", err)
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		t.Fatalf("evalsymlinks(root): %v", err)
	}

	realName := "real.pdf"
	if err := os.WriteFile(filepath.Join(rootDir, realName), []byte("x"), 0o644); err != nil {
		t.Fatalf("write real file: %v", err)
	}

	cases := []struct {
		name string
		doc  model.Document
		want bool
	}{
		{
			name: "archive member skips disk check",
			doc:  model.Document{RelPath: "anything/at/all.txt", SourceType: "archive_member"},
			want: true,
		},
		{
			name: "filesystem doc that exists",
			doc:  model.Document{RelPath: realName, SourceType: "filesystem"},
			want: true,
		},
		{
			name: "filesystem doc that is missing",
			doc:  model.Document{RelPath: "missing.pdf", SourceType: "filesystem"},
			want: false,
		},
		{
			name: "path traversal rejected",
			doc:  model.Document{RelPath: "../escape.pdf", SourceType: "filesystem"},
			want: false,
		},
		{
			name: "absolute path rejected",
			doc:  model.Document{RelPath: filepath.Join(rootAbs, realName), SourceType: "filesystem"},
			want: false,
		},
		{
			name: "padded absolute path rejected",
			doc:  model.Document{RelPath: " /etc/passwd", SourceType: "filesystem"},
			want: false,
		},
		{
			name: "archive member with a malformed path rejected",
			doc:  model.Document{RelPath: "../escape.zip/inner.txt", SourceType: "archive_member"},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isResolvableSourceWithRoot(tc.doc, rootAbs, rootReal); got != tc.want {
				t.Fatalf("isResolvableSourceWithRoot(%+v)=%v want=%v", tc.doc, got, tc.want)
			}
		})
	}
}

// TestIsListableRemoteSource covers the object-store gate of issue #684. An S3
// corpus has no local file at RootDir/rel_path, so the gate must pass a
// well-formed rel_path without touching disk, and must still refuse a path that
// can never round-trip through open_file.
func TestIsListableRemoteSource(t *testing.T) {
	cases := []struct {
		name string
		doc  model.Document
		want bool
	}{
		{
			name: "remote object with no local file is listable",
			doc:  model.Document{RelPath: "docs/a.md", SourceType: "filesystem"},
			want: true,
		},
		{
			name: "archive member is listable",
			doc:  model.Document{RelPath: "bundle.zip/inner/note.md", SourceType: "archive_member"},
			want: true,
		},
		{
			name: "traversal rejected",
			doc:  model.Document{RelPath: "../escape.md", SourceType: "filesystem"},
			want: false,
		},
		{
			name: "absolute path rejected",
			doc:  model.Document{RelPath: "/etc/passwd", SourceType: "filesystem"},
			want: false,
		},
		{
			name: "padded absolute path rejected",
			doc:  model.Document{RelPath: " /etc/passwd", SourceType: "filesystem"},
			want: false,
		},
		{
			name: "empty path rejected",
			doc:  model.Document{RelPath: "  ", SourceType: "filesystem"},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isListableRemoteSource(tc.doc); got != tc.want {
				t.Fatalf("isListableRemoteSource(%+v)=%v want=%v", tc.doc, got, tc.want)
			}
		})
	}
}
