package tests

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
)

// Issue #773 / SPEC §7.1: `ingest.exclude_dirs` lets an operator replace the
// directory ignore list. A present list replaces the default list in full; it
// does not add to it. `.dir2mcp` stays in the resolved list either way.

// walkRels walks root with opts and returns the discovered rel paths as a set.
func walkRels(t *testing.T, root string, opts corpusfs.Options) map[string]bool {
	t.Helper()
	files, err := corpusfs.NewLocalFS(root).Walk(context.Background(), root, opts)
	if err != nil {
		t.Fatalf("LocalFS.Walk: %v", err)
	}
	rels := map[string]bool{}
	for _, f := range files {
		rels[f.RelPath] = true
	}
	return rels
}

// excludeDirsCorpus writes one file under each directory the tests care about.
func excludeDirsCorpus(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "keep.txt"), []byte("keep"))
	mustWrite(t, filepath.Join(root, "dist", "bundle.js"), []byte("bundled"))
	mustWrite(t, filepath.Join(root, "node_modules", "lib.js"), []byte("dep"))
	mustWrite(t, filepath.Join(root, ".dir2mcp", "index.db"), []byte("state"))
	mustWrite(t, filepath.Join(root, "notes", "memo.md"), []byte("memo"))
	return root
}

// TestLocalFSWalk_ExcludeDirsDropsANameFromTheList is the operator case in
// #773: a corpus of published static-site output really does live in `dist/`.
// Dropping `dist` from the list must index it.
func TestLocalFSWalk_ExcludeDirsDropsANameFromTheList(t *testing.T) {
	root := excludeDirsCorpus(t)

	opts := corpusfs.DefaultOptions()
	opts.ExcludeDirs = []string{".git", "node_modules", "vendor", "__pycache__", "build", ".venv"}
	rels := walkRels(t, root, opts)

	if !rels["dist/bundle.js"] {
		t.Errorf("dist/bundle.js must be discovered once `dist` leaves ingest.exclude_dirs; got %v", rels)
	}
	if !rels["keep.txt"] {
		t.Errorf("keep.txt must still be discovered; got %v", rels)
	}
	if rels["node_modules/lib.js"] {
		t.Errorf("node_modules is still listed, so it must stay excluded; got %v", rels)
	}
}

// TestLocalFSWalk_ExcludeDirsReplacesTheDefaultList pins the replace semantics.
// A present key does not ADD to the default list. A list of one name leaves
// every other default name indexable.
func TestLocalFSWalk_ExcludeDirsReplacesTheDefaultList(t *testing.T) {
	root := excludeDirsCorpus(t)

	opts := corpusfs.DefaultOptions()
	opts.ExcludeDirs = []string{"notes"}
	rels := walkRels(t, root, opts)

	if rels["notes/memo.md"] {
		t.Errorf("notes is the only listed name, so it must be excluded; got %v", rels)
	}
	for _, want := range []string{"dist/bundle.js", "node_modules/lib.js"} {
		if !rels[want] {
			t.Errorf("%q must be discovered: a present list replaces the default list, it does not add to it; got %v", want, rels)
		}
	}
}

// TestLocalFSWalk_ExcludeDirsAlwaysKeepsTheStateDir pins the one name an
// operator cannot drop. The state directory lives under the corpus root, so to
// index it is self-referential: the walk would read the index the same run
// writes.
func TestLocalFSWalk_ExcludeDirsAlwaysKeepsTheStateDir(t *testing.T) {
	root := excludeDirsCorpus(t)

	opts := corpusfs.DefaultOptions()
	opts.ExcludeDirs = []string{"notes"} // omits .dir2mcp on purpose
	rels := walkRels(t, root, opts)

	if rels[".dir2mcp/index.db"] {
		t.Errorf(".dir2mcp must stay excluded even when the list omits it; got %v", rels)
	}
}

// TestLocalFSWalk_ExcludeDirsEmptyListKeepsOnlyTheStateDir covers the explicit
// `exclude_dirs: []`. It is a present key, so it replaces the default list with
// nothing, and only the forced state directory remains.
func TestLocalFSWalk_ExcludeDirsEmptyListKeepsOnlyTheStateDir(t *testing.T) {
	root := excludeDirsCorpus(t)

	opts := corpusfs.DefaultOptions()
	opts.ExcludeDirs = []string{}
	rels := walkRels(t, root, opts)

	for _, want := range []string{"dist/bundle.js", "node_modules/lib.js", "notes/memo.md"} {
		if !rels[want] {
			t.Errorf("%q must be discovered under an empty list; got %v", want, rels)
		}
	}
	if rels[".dir2mcp/index.db"] {
		t.Errorf(".dir2mcp must stay excluded under an empty list; got %v", rels)
	}
}

// TestLocalFSWalk_ExcludeDirsAbsentKeepsTheDefaults guards the existing corpus:
// nil means the operator set nothing, so the default list still applies.
func TestLocalFSWalk_ExcludeDirsAbsentKeepsTheDefaults(t *testing.T) {
	root := excludeDirsCorpus(t)

	rels := walkRels(t, root, corpusfs.DefaultOptions())

	if !rels["keep.txt"] || !rels["notes/memo.md"] {
		t.Errorf("ordinary files must be discovered; got %v", rels)
	}
	for _, bad := range []string{"dist/bundle.js", "node_modules/lib.js", ".dir2mcp/index.db"} {
		if rels[bad] {
			t.Errorf("%q must stay excluded when no list is set; got %v", bad, rels)
		}
	}
}

// TestS3FSWalk_ExcludeDirsMatchesLocal pins backend parity: one list governs a
// bucket exactly as it governs a local corpus.
func TestS3FSWalk_ExcludeDirsMatchesLocal(t *testing.T) {
	objs := map[string][]byte{
		"keep.txt":            []byte("keep"),
		"dist/bundle.js":      []byte("bundled"),
		"node_modules/lib.js": []byte("dep"),
		".dir2mcp/index.db":   []byte("state"),
		"notes/memo.md":       []byte("memo"),
	}
	fsys, _ := newFakeS3FS(t, "", objs, "")

	opts := corpusfs.DefaultOptions()
	opts.ExcludeDirs = []string{"notes"}
	got, err := fsys.Walk(context.Background(), "", opts)
	if err != nil {
		t.Fatalf("S3FS.Walk: %v", err)
	}
	rels := map[string]bool{}
	for _, f := range got {
		rels[f.RelPath] = true
	}

	for _, want := range []string{"keep.txt", "dist/bundle.js", "node_modules/lib.js"} {
		if !rels[want] {
			t.Errorf("%q must be discovered: the list replaces the default list on S3 too; got %v", want, rels)
		}
	}
	for _, bad := range []string{"notes/memo.md", ".dir2mcp/index.db"} {
		if rels[bad] {
			t.Errorf("%q must stay excluded on S3; got %v", bad, rels)
		}
	}
}

// TestResolveExcludedDirs pins the resolver the three consumers share.
func TestResolveExcludedDirs(t *testing.T) {
	want := []string{".dir2mcp", ".git", "__pycache__", ".venv", "build", "dist", "node_modules", "vendor"}
	got := corpusfs.DefaultExcludedDirs()
	if len(got) != len(want) {
		t.Fatalf("the default list has %d names, want %d: %v", len(got), len(want), got)
	}
	for _, name := range want {
		if !corpusfs.ResolveExcludedDirs(nil).Has(name) {
			t.Errorf("a nil list must keep the default name %q", name)
		}
	}

	// The zero value is the default list, so a consumer that forgets to resolve
	// still excludes .git and .dir2mcp instead of indexing them.
	var zero corpusfs.ExcludedDirSet
	if !zero.Has(".git") || !zero.Has(".dir2mcp") {
		t.Errorf("the zero value must fall back to the default list")
	}

	resolved := corpusfs.ResolveExcludedDirs([]string{"notes", "notes", ""})
	if !reflect.DeepEqual(resolved.Names(), []string{".dir2mcp", "notes"}) {
		t.Errorf("a present list must replace the defaults, drop duplicates and blanks, and keep .dir2mcp; got %v", resolved.Names())
	}
	if resolved.Has("dist") {
		t.Errorf("a present list must not keep default names it omits")
	}
}
