package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
)

// Issue #886: on S3 the size cap ran during the listing and the gitignore
// filter afterwards, so a gitignored over-cap object fired OnOversize. The hook
// is not passive: ingest counts the file as scanned+skipped, persists a
// skip_reason=size_cap document row and emits a FILE_TOO_LARGE manifest entry,
// so the honest-coverage aggregate reported a gap that was not a gap and told
// the operator to raise a cap that would have changed nothing. The local walker
// drops an ignored path before it ever looks at the size; these tests pin the
// same order on S3.

// walk886 runs a Walk with a 1 KiB cap, collecting OnOversize calls.
func walk886(t *testing.T, objs map[string][]byte, useGitIgnore bool) (paths []string, oversize []string) {
	t.Helper()
	fsys, _ := newFakeS3FS(t, "", objs, "")
	got, err := fsys.Walk(context.Background(), "", corpusfs.Options{
		UseGitIgnore: useGitIgnore,
		MaxSizeBytes: 1024,
		OnOversize:   func(rel string, _ int64) { oversize = append(oversize, rel) },
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	for _, f := range got {
		paths = append(paths, f.RelPath)
	}
	return paths, oversize
}

// TestS3Walk886_GitignoredOversizeObjectIsNotASizeCapSkip is the defect. The
// over-cap object is excluded by .gitignore, so it must vanish silently: not
// discovered, and no OnOversize call.
func TestS3Walk886_GitignoredOversizeObjectIsNotASizeCapSkip(t *testing.T) {
	objs := map[string][]byte{
		".gitignore": []byte("*.iso\n"),
		"keep.txt":   []byte("small"),
		"backup.iso": []byte(strings.Repeat("x", 4096)), // gitignored AND over-cap
	}
	paths, oversize := walk886(t, objs, true)
	if len(oversize) != 0 {
		t.Fatalf("gitignored object reported as size_cap skip: OnOversize(%v); the coverage report would name a gap that is not a gap", oversize)
	}
	for _, p := range paths {
		if p == "backup.iso" {
			t.Fatalf("gitignored object was discovered: %v", paths)
		}
	}
}

// TestS3Walk886_PlainOversizeObjectStillReports is the control: the same
// over-cap object without a .gitignore rule keeps today's behaviour, one
// OnOversize call and no discovery.
func TestS3Walk886_PlainOversizeObjectStillReports(t *testing.T) {
	objs := map[string][]byte{
		"keep.txt":   []byte("small"),
		"backup.iso": []byte(strings.Repeat("x", 4096)),
	}
	for _, useGitIgnore := range []bool{true, false} {
		paths, oversize := walk886(t, objs, useGitIgnore)
		if len(oversize) != 1 || oversize[0] != "backup.iso" {
			t.Fatalf("UseGitIgnore=%v: OnOversize = %v, want exactly [backup.iso]", useGitIgnore, oversize)
		}
		for _, p := range paths {
			if p == "backup.iso" {
				t.Fatalf("UseGitIgnore=%v: over-cap object was discovered: %v", useGitIgnore, paths)
			}
		}
	}
}

// TestS3Walk886_KeptObjectsSurviveBothFilters pins that moving the size gate
// did not change what a normal walk returns.
func TestS3Walk886_KeptObjectsSurviveBothFilters(t *testing.T) {
	objs := map[string][]byte{
		".gitignore": []byte("*.log\n"),
		"keep.txt":   []byte("small"),
		"app.log":    []byte("gitignored, small"),
		"big.bin":    []byte(strings.Repeat("x", 4096)), // over-cap, not ignored
	}
	paths, oversize := walk886(t, objs, true)
	want := map[string]bool{".gitignore": true, "keep.txt": true}
	if len(paths) != len(want) {
		t.Fatalf("discovered %v, want exactly the keys of %v", paths, want)
	}
	for _, p := range paths {
		if !want[p] {
			t.Fatalf("unexpected discovery %q in %v", p, paths)
		}
	}
	if len(oversize) != 1 || oversize[0] != "big.bin" {
		t.Fatalf("OnOversize = %v, want [big.bin]", oversize)
	}
}
