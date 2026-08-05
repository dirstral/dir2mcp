//go:build unix

// POSIX permission bits and syscall.Umask. Windows has no umask and its
// owner-only guarantee is an ACL contract, which is a separate test.

package tests

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/dirstral/dir2mcp/internal/statefs"
)

// #726: dir2mcp keeps corpus-derived plaintext under StateDir (OCR output,
// transcripts, translations, summaries, contextual text) plus index payloads
// carrying titles, snippets, speakers and media references, and several normal
// startup and ingest paths created that hierarchy 0755/0644. Under the usual
// 022 umask any other local account could read the corpus in derived form even
// when the corpus directory itself was private, which contradicted the
// owner-only posture already applied to meta.sqlite, tokens and support
// bundles.
//
// These tests run under a deliberately permissive umask, because the defect was
// invisible under a restrictive one.

// withPermissiveUmask forces 022 for the duration of a test, which is the
// setting the reported defect needs and is NOT what a hardened CI box
// necessarily has.
func withPermissiveUmask(t *testing.T) {
	t.Helper()
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
}

func modeOf(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

func TestStateDirectoriesAreOwnerOnlyUnderAPermissiveUmask(t *testing.T) {
	withPermissiveUmask(t)
	root := filepath.Join(t.TempDir(), "state")

	if err := statefs.MkdirAll(filepath.Join(root, "cache", "ocr")); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for _, dir := range []string{root, filepath.Join(root, "cache"), filepath.Join(root, "cache", "ocr")} {
		if got := modeOf(t, dir); got != statefs.DirMode {
			t.Fatalf("%s is %04o, want %04o: another local account can traverse the state tree", dir, got, statefs.DirMode)
		}
	}
}

func TestDerivedStateFilesAreOwnerOnlyUnderAPermissiveUmask(t *testing.T) {
	withPermissiveUmask(t)
	root := t.TempDir()

	transcript := filepath.Join(root, "transcript.json")
	if err := statefs.WriteFile(transcript, []byte(`{"text":"corpus content"}`)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := modeOf(t, transcript); got != statefs.FileMode {
		t.Fatalf("derived file is %04o, want %04o", got, statefs.FileMode)
	}

	snapshot := filepath.Join(root, "snapshot.bin")
	file, err := statefs.Create(snapshot)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := file.WriteString("payload"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := modeOf(t, snapshot); got != statefs.FileMode {
		t.Fatalf("index snapshot is %04o, want %04o", got, statefs.FileMode)
	}
}

// TestAnExistingPermissiveStateTreeIsTightened pins the half a mode argument
// could never fix: MkdirAll and WriteFile leave an EXISTING path's mode alone,
// so a tree created 0755 by an older build stayed 0755 through every later run
// no matter what mode the new code passed.
func TestAnExistingPermissiveStateTreeIsTightened(t *testing.T) {
	withPermissiveUmask(t)
	root := filepath.Join(t.TempDir(), "legacy")

	// A state tree exactly as an older build left it.
	if err := os.MkdirAll(filepath.Join(root, "cache", "docling"), 0o755); err != nil {
		t.Fatalf("seed dirs: %v", err)
	}
	derived := filepath.Join(root, "cache", "docling", "doc.md")
	if err := os.WriteFile(derived, []byte("# extracted corpus text"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if modeOf(t, root) != 0o755 || modeOf(t, derived) != 0o644 {
		t.Fatalf("seed did not reproduce the reported state: dir %04o file %04o",
			modeOf(t, root), modeOf(t, derived))
	}

	if err := statefs.HardenTree(root); err != nil {
		t.Fatalf("HardenTree: %v", err)
	}
	for _, dir := range []string{root, filepath.Join(root, "cache"), filepath.Join(root, "cache", "docling")} {
		if got := modeOf(t, dir); got != statefs.DirMode {
			t.Fatalf("pre-existing dir %s left at %04o", dir, got)
		}
	}
	if got := modeOf(t, derived); got != statefs.FileMode {
		t.Fatalf("pre-existing derived file left at %04o, want %04o", got, statefs.FileMode)
	}
}

// TestRewritingAFilePreviouslyWorldReadableTightensIt covers the ingest caches,
// which rewrite the same path on every re-derivation: os.WriteFile keeps the
// existing mode, so a cache first written by an older build would otherwise
// stay 0644 forever even as the content was refreshed.
func TestRewritingAFilePreviouslyWorldReadableTightensIt(t *testing.T) {
	withPermissiveUmask(t)
	path := filepath.Join(t.TempDir(), "summary.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := statefs.WriteFile(path, []byte("refreshed summary")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := modeOf(t, path); got != statefs.FileMode {
		t.Fatalf("rewritten cache is %04o, want %04o", got, statefs.FileMode)
	}
}

// TestHardeningNeverWidens pins that this only ever removes bits: an operator
// who made a cache read-only keeps it read-only.
func TestHardeningNeverWidens(t *testing.T) {
	withPermissiveUmask(t)
	root := t.TempDir()
	path := filepath.Join(root, "readonly.bin")
	if err := os.WriteFile(path, []byte("x"), 0o400); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := statefs.HardenTree(root); err != nil {
		t.Fatalf("HardenTree: %v", err)
	}
	if got := modeOf(t, path); got != 0o400 {
		t.Fatalf("hardening widened a deliberately restrictive file to %04o", got)
	}
}

// TestHardenTreeSkipsAVanishedPath: a live daemon rotates temporaries under the
// state directory, so a walk that raced one must not fail the startup it is
// protecting.
func TestHardenTreeSkipsAVanishedPath(t *testing.T) {
	withPermissiveUmask(t)
	if err := statefs.HardenTree(filepath.Join(t.TempDir(), "never-created")); err != nil {
		t.Fatalf("HardenTree on a missing root: %v", err)
	}
}

// TestHardenTreeDoesNotFollowSymlinks: the mode that matters is the target's,
// and a target may deliberately live outside the tree (an index moved to
// another volume). Chmod'ing through the link would reach outside the state
// directory entirely.
func TestHardenTreeDoesNotFollowSymlinks(t *testing.T) {
	withPermissiveUmask(t)
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("not ours"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := statefs.HardenTree(root); err != nil {
		t.Fatalf("HardenTree: %v", err)
	}
	if got := modeOf(t, outside); got != 0o644 {
		t.Fatalf("hardening reached through a symlink and changed %s to %04o", outside, got)
	}
}
