package tests

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/ingest"
)

// spec716ExcludedDirs are the default-ignore directory names SPEC §7.1 requires
// but that discovery did not implement (#716). They are listed separately from
// the already-honored names so a regression re-introducing the gap names itself.
var spec716ExcludedDirs = []string{"dist", "build", ".venv"}

// TestLocalFSWalk_ExcludesSpecDefaultDirs pins SPEC §7.1's default ignore list
// on the local backend: `dist/`, `build/`, and `.venv/` are normative defaults
// that discovery previously enumerated and ingested (#716).
func TestLocalFSWalk_ExcludesSpecDefaultDirs(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "keep.txt"), []byte("hello"))
	mustWrite(t, filepath.Join(root, "dist", "bundle.js"), []byte("bundled"))
	mustWrite(t, filepath.Join(root, "build", "generated.txt"), []byte("generated"))
	mustWrite(t, filepath.Join(root, ".venv", "lib", "site-packages", "pkg.py"), []byte("pkg"))
	// Nested under a normal directory: exclusion is by directory name at any
	// depth, not only at the corpus root.
	mustWrite(t, filepath.Join(root, "src", "dist", "nested.js"), []byte("nested"))

	got, err := corpusfs.NewLocalFS(root).Walk(context.Background(), root, corpusfs.DefaultOptions())
	if err != nil {
		t.Fatalf("LocalFS.Walk: %v", err)
	}
	rels := map[string]bool{}
	for _, f := range got {
		rels[f.RelPath] = true
	}
	if !rels["keep.txt"] {
		t.Fatalf("expected keep.txt to be discovered; got %v", rels)
	}
	for _, bad := range []string{
		"dist/bundle.js",
		"build/generated.txt",
		".venv/lib/site-packages/pkg.py",
		"src/dist/nested.js",
	} {
		if rels[bad] {
			t.Errorf("SPEC §7.1 default ignore list: %q must not be discovered; got %v", bad, rels)
		}
	}
}

// TestLocalFSWalk_SpecDefaultDirNamesAsFilesStillDiscovered guards the
// directory-vs-file rule while closing #716: only directories named `dist`,
// `build`, `.venv` are skipped; a regular FILE with one of those names is a
// normal corpus document and must still be discovered.
func TestLocalFSWalk_SpecDefaultDirNamesAsFilesStillDiscovered(t *testing.T) {
	root := t.TempDir()
	for _, name := range spec716ExcludedDirs {
		mustWrite(t, filepath.Join(root, name), []byte("a file, not a dir"))
		mustWrite(t, filepath.Join(root, "sub", name), []byte("also a file"))
	}

	got, err := corpusfs.NewLocalFS(root).Walk(context.Background(), root, corpusfs.DefaultOptions())
	if err != nil {
		t.Fatalf("LocalFS.Walk: %v", err)
	}
	rels := map[string]bool{}
	for _, f := range got {
		rels[f.RelPath] = true
	}
	for _, name := range spec716ExcludedDirs {
		if !rels[name] {
			t.Errorf("file %q must still be discovered (it is a file, not a directory); got %v", name, rels)
		}
		if !rels["sub/"+name] {
			t.Errorf("file %q must still be discovered (it is a file, not a directory); got %v", "sub/"+name, rels)
		}
	}
}

// TestLocalFSWalk_ExcludedDirNamesMatchExactly pins that the exclusion is an
// exact name match. A directory called ` dist ` is a different directory than
// `dist`, and nothing else in the pipeline would exclude its documents, so
// trimming before the lookup would silently drop a legitimately-named tree.
func TestLocalFSWalk_ExcludedDirNamesMatchExactly(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, " dist ", "keep.txt"), []byte("padded name"))
	mustWrite(t, filepath.Join(root, "dist.old", "keep.txt"), []byte("different name"))
	mustWrite(t, filepath.Join(root, "dist", "bundle.js"), []byte("excluded"))

	got, err := corpusfs.NewLocalFS(root).Walk(context.Background(), root, corpusfs.DefaultOptions())
	if err != nil {
		t.Fatalf("LocalFS.Walk: %v", err)
	}
	rels := map[string]bool{}
	for _, f := range got {
		rels[f.RelPath] = true
	}
	for _, want := range []string{" dist /keep.txt", "dist.old/keep.txt"} {
		if !rels[want] {
			t.Errorf("%q must be discovered: only an exact `dist` directory is excluded; got %v", want, rels)
		}
	}
	if rels["dist/bundle.js"] {
		t.Errorf("dist/bundle.js must still be excluded; got %v", rels)
	}
}

// TestS3FSWalk_ExcludesSpecDefaultDirs pins the same §7.1 defaults on the object
// -store backend, where the "directory" is just a key segment (#716). LocalFS and
// S3 must agree on the default ignore set or the same corpus indexes differently
// depending on where it is stored.
func TestS3FSWalk_ExcludesSpecDefaultDirs(t *testing.T) {
	objs := map[string][]byte{
		"corpus/keep.txt":                       []byte("hello"),
		"corpus/dist/bundle.js":                 []byte("bundled"),
		"corpus/build/generated.txt":            []byte("generated"),
		"corpus/.venv/lib/site-packages/pkg.py": []byte("pkg"),
		"corpus/src/dist/nested.js":             []byte("nested"),
		"corpus/dist":                           []byte("a file, not a dir"),
		"corpus/sub/build":                      []byte("also a file"),
		"corpus/sub/.venv":                      []byte("also a file"),
	}
	fsys, _ := newFakeS3FS(t, "corpus/", objs, "")
	got, err := fsys.Walk(context.Background(), "", corpusfs.DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	rels := map[string]bool{}
	for _, f := range got {
		rels[f.RelPath] = true
	}
	for _, want := range []string{"keep.txt", "dist", "sub/build", "sub/.venv"} {
		if !rels[want] {
			t.Errorf("expected %q to be discovered (file, not directory segment); got %v", want, rels)
		}
	}
	for _, bad := range []string{
		"dist/bundle.js",
		"build/generated.txt",
		".venv/lib/site-packages/pkg.py",
		"src/dist/nested.js",
	} {
		if rels[bad] {
			t.Errorf("SPEC §7.1 default ignore list: %q must not be discovered; got %v", bad, rels)
		}
	}
}

// TestDiscoverFiles_ExcludesSpecDefaultDirs checks the ingest-facing entry point
// (the one the scan actually calls) so the fix is pinned at the seam operators
// see, not only inside corpusfs.
func TestDiscoverFiles_ExcludesSpecDefaultDirs(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "keep.txt"), []byte("hello"))
	for _, name := range spec716ExcludedDirs {
		mustWrite(t, filepath.Join(root, name, "artifact.txt"), []byte("artifact"))
	}

	got, err := ingest.DiscoverFiles(context.Background(), root, corpusfs.DefaultMaxFileSizeBytes())
	if err != nil {
		t.Fatalf("DiscoverFiles: %v", err)
	}
	for _, f := range got {
		for _, name := range spec716ExcludedDirs {
			if f.RelPath == name+"/artifact.txt" {
				t.Errorf("DiscoverFiles returned %q; SPEC §7.1 excludes %s/ by default", f.RelPath, name)
			}
		}
	}
	if len(got) != 1 || got[0].RelPath != "keep.txt" {
		t.Errorf("expected only keep.txt to survive discovery; got %+v", got)
	}
}
