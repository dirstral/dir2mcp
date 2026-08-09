package tests

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
)

// #781: with the default ingest.follow_symlinks=false the LocalFS walker
// dropped a symlinked entry and told nobody. An operator whose corpus is a
// curated tree of links into a media library (the natural layout when the media
// is large and lives elsewhere) saw `scanned: 0, skipped: 0, errors: 0` and a
// daemon reporting itself ready, which is indistinguishable from an empty
// directory or a wrong root_dir.
//
// Not following links is the right default and is not what these tests pin.
// They pin that the refusal is observable, the way Options.OnOversize (#497)
// and Options.OnUnsafeKey (#735) already make their drops observable.

// symlinkOrSkip creates a symlink, skipping the test when the filesystem cannot
// (Windows without the privilege, or a mount with no symlink support). A skip is
// honest here: with no link to drop there is nothing to assert.
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("filesystem cannot create symlinks (%v); nothing to observe", err)
	}
}

// walkReportingSymlinks walks root with following disabled and returns the
// discovered rel paths plus every rel path handed to OnSkippedSymlink, both
// sorted so assertions do not depend on directory read order.
func walkReportingSymlinks(t *testing.T, root string, cache corpusfs.ScanCache) (discovered, skipped []string) {
	t.Helper()
	files, err := corpusfs.NewLocalFS(root).Walk(context.Background(), root, corpusfs.Options{
		MaxSizeBytes:     1 << 20,
		ScanCache:        cache,
		OnSkippedSymlink: func(relPath string) { skipped = append(skipped, relPath) },
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	sort.Strings(skipped)
	return relPaths(files), skipped
}

// TestLocalFSWalk_ReportsSkippedSymlinks covers the corpus shape from the issue:
// links pointing at a library outside the corpus root, so nothing at all is
// discovered through them.
func TestLocalFSWalk_ReportsSkippedSymlinks(t *testing.T) {
	library := t.TempDir()
	mustWrite(t, filepath.Join(library, "clip.mp4"), []byte("frames"))
	mustWrite(t, filepath.Join(library, "season", "ep1.mp4"), []byte("more frames"))

	root := t.TempDir()
	// A file link and a directory link: the walker cannot tell them apart with
	// following disabled (it never resolves the target), so both must report.
	symlinkOrSkip(t, filepath.Join(library, "clip.mp4"), filepath.Join(root, "clip.mp4"))
	symlinkOrSkip(t, filepath.Join(library, "season"), filepath.Join(root, "season"))

	discovered, skipped := walkReportingSymlinks(t, root, nil)

	if len(discovered) != 0 {
		t.Fatalf("discovered %v; links are still not followed by default", discovered)
	}
	want := []string{"clip.mp4", "season"}
	if !reflect.DeepEqual(skipped, want) {
		t.Fatalf("OnSkippedSymlink saw %v, want %v (once per dropped link, file and directory alike)", skipped, want)
	}
}

// TestLocalFSWalk_SkippedSymlinkDoesNotSuppressRealFiles guards the other half:
// reporting a link must not cost the ordinary files sitting next to it.
func TestLocalFSWalk_SkippedSymlinkDoesNotSuppressRealFiles(t *testing.T) {
	library := t.TempDir()
	mustWrite(t, filepath.Join(library, "clip.mp4"), []byte("frames"))

	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "notes.txt"), []byte("hello"))
	mustWrite(t, filepath.Join(root, "sub", "deep.txt"), []byte("deeper"))
	symlinkOrSkip(t, filepath.Join(library, "clip.mp4"), filepath.Join(root, "sub", "clip.mp4"))

	discovered, skipped := walkReportingSymlinks(t, root, nil)

	if want := []string{"notes.txt", "sub/deep.txt"}; !reflect.DeepEqual(discovered, want) {
		t.Fatalf("discovered %v, want %v", discovered, want)
	}
	// Reported by its corpus-relative path, not its bare name, so an operator can
	// find it in a deep tree.
	if want := []string{"sub/clip.mp4"}; !reflect.DeepEqual(skipped, want) {
		t.Fatalf("OnSkippedSymlink saw %v, want %v", skipped, want)
	}
}

// TestLocalFSWalk_SkippedSymlinkSurvivesScanCache pins the reporting against the
// directory-scan cache (#267 item 5): a second walk of an unchanged tree must
// still report the link. The cache is what could plausibly swallow it, since its
// whole purpose is to avoid re-reading a directory.
func TestLocalFSWalk_SkippedSymlinkSurvivesScanCache(t *testing.T) {
	library := t.TempDir()
	mustWrite(t, filepath.Join(library, "clip.mp4"), []byte("frames"))

	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "notes.txt"), []byte("hello"))
	symlinkOrSkip(t, filepath.Join(library, "clip.mp4"), filepath.Join(root, "clip.mp4"))

	cache := newFakeScanCache()
	want := []string{"clip.mp4"}
	for i := 1; i <= 2; i++ {
		_, skipped := walkReportingSymlinks(t, root, cache)
		if !reflect.DeepEqual(skipped, want) {
			t.Fatalf("walk %d: OnSkippedSymlink saw %v, want %v", i, skipped, want)
		}
	}
}

// TestLocalFSWalk_FollowSymlinksReportsNoSkips pins the opposite policy: with
// following enabled a link is not a skip. An in-root link is discovered; a link
// escaping the root is refused by the #717 containment rule, and that refusal is
// its own event, not a follow_symlinks skip, so the hook must stay silent.
func TestLocalFSWalk_FollowSymlinksReportsNoSkips(t *testing.T) {
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.txt"), []byte("top secret"))

	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "real", "inside.txt"), []byte("in the corpus"))
	symlinkOrSkip(t, filepath.Join(root, "real", "inside.txt"), filepath.Join(root, "alias.txt"))
	symlinkOrSkip(t, filepath.Join(outside, "secret.txt"), filepath.Join(root, "escape.txt"))

	var skipped []string
	files, err := corpusfs.NewLocalFS(root).Walk(context.Background(), root, corpusfs.Options{
		MaxSizeBytes:     1 << 20,
		FollowSymlinks:   true,
		OnSkippedSymlink: func(relPath string) { skipped = append(skipped, relPath) },
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if want := []string{"alias.txt", "real/inside.txt"}; !reflect.DeepEqual(relPaths(files), want) {
		t.Fatalf("discovered %v, want %v (the escaping link stays out, #717)", relPaths(files), want)
	}
	if len(skipped) != 0 {
		t.Fatalf("OnSkippedSymlink fired %v with follow_symlinks enabled; nothing was skipped for being a link", skipped)
	}
}

// TestLocalFSWalk_NilSkippedSymlinkHookIsSafe keeps the hook optional: an
// unset callback must leave the walk exactly as it was, not panic.
func TestLocalFSWalk_NilSkippedSymlinkHookIsSafe(t *testing.T) {
	library := t.TempDir()
	mustWrite(t, filepath.Join(library, "clip.mp4"), []byte("frames"))

	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "notes.txt"), []byte("hello"))
	symlinkOrSkip(t, filepath.Join(library, "clip.mp4"), filepath.Join(root, "clip.mp4"))

	files, err := corpusfs.NewLocalFS(root).Walk(context.Background(), root, corpusfs.Options{MaxSizeBytes: 1 << 20})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if want := []string{"notes.txt"}; !reflect.DeepEqual(relPaths(files), want) {
		t.Fatalf("discovered %v, want %v", relPaths(files), want)
	}
}
