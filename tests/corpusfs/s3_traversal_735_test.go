package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
)

// #735: S3FS.Walk accepted object keys whose prefix-relative name was absolute
// or contained traversal segments. relForKey stripped the configured prefix as
// a raw string and nothing re-checked the result, so under prefix "corpus/" the
// key "corpus/../outside.mp4" became rel_path "../outside.mp4".
//
// That is not merely malformed metadata. The value reaches code that joins it
// against the LOCAL root: video recognition builds RootDir + rel_path, and
// TTML/SMIL export probes filepath.Join(RootDir, relPath). A bucket is
// untrusted input, so a key could steer either at a file outside the corpus.
// SPEC §7.8 requires root/prefix isolation on every backend.
//
// The pre-existing walk test (TestS3FSWalk_KeyMappingExcludeAndSizeCap) covered
// prefix mapping with no adversarial keys, which is why this shipped.

func TestS3FSWalk_RejectsTraversalAndAbsoluteKeys(t *testing.T) {
	objs := map[string][]byte{
		// The three keys named in the issue.
		"corpus/../outside.mp4":      []byte("escapes the prefix"),
		"corpus/a/../../outside.txt": []byte("escapes after a real segment"),
		"corpus//absolute.mp4":       []byte("strips to a leading slash"),
		// Neighbours worth pinning alongside them.
		"corpus/..":                  []byte("bare parent"),
		"corpus/sub/../still-inside": []byte("cleans to a safe path, still refused"),
		// And an ordinary key, so the test fails loudly if the guard is
		// over-broad rather than silently discovering nothing.
		"corpus/keep.txt": []byte("hello"),
	}
	fsys, _ := newFakeS3FS(t, "corpus/", objs, "")

	var rejected []string
	got, err := fsys.Walk(context.Background(), "", corpusfs.Options{
		MaxSizeBytes: 1 << 20,
		OnUnsafeKey: func(key string, _ error) {
			rejected = append(rejected, key)
		},
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	for _, f := range got {
		if f.RelPath != "keep.txt" {
			t.Fatalf("discovered %q; every other key in this bucket escapes the prefix and must be rejected (#735)", f.RelPath)
		}
		// The specific shapes, named so a regression says which one came back.
		if strings.HasPrefix(f.RelPath, "/") {
			t.Fatalf("absolute rel_path %q survived discovery", f.RelPath)
		}
		if strings.Contains(f.RelPath, "..") {
			t.Fatalf("traversal rel_path %q survived discovery", f.RelPath)
		}
	}
	if len(got) != 1 {
		t.Fatalf("discovered %d files, want only keep.txt: %v", len(got), got)
	}

	// Rejection must be observable, not a silent skip: an operator needs to see
	// that the bucket carries keys dir2mcp refuses.
	// Exactly, not at least: a regression that silently drops one callback
	// would otherwise still pass.
	if len(rejected) != 5 {
		t.Fatalf("OnUnsafeKey saw %d keys (%v); the bucket holds exactly five unsafe ones", len(rejected), rejected)
	}
	for _, key := range rejected {
		if key == "corpus/keep.txt" {
			t.Fatal("the ordinary key was reported as unsafe")
		}
	}
}

// TestS3FSWalk_UnsafeKeysAreSkippedWithoutACallback pins that the callback is
// optional: a caller that does not pass one still gets a safe listing rather
// than a panic or an unsafe rel_path.
func TestS3FSWalk_UnsafeKeysAreSkippedWithoutACallback(t *testing.T) {
	objs := map[string][]byte{
		"corpus/../outside.mp4": []byte("x"),
		"corpus/keep.txt":       []byte("hello"),
	}
	fsys, _ := newFakeS3FS(t, "corpus/", objs, "")

	got, err := fsys.Walk(context.Background(), "", corpusfs.Options{MaxSizeBytes: 1 << 20})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(got) != 1 || got[0].RelPath != "keep.txt" {
		t.Fatalf("Walk without OnUnsafeKey = %v, want only keep.txt", got)
	}
}

// TestS3FSOpen_RejectsAPathOutsideTheCorpus: §7.8 applies to open and stat, not
// only to list. A rel_path arriving from the store, an MCP argument or a config
// file has not been through discovery, and keyForRel would strip a leading
// slash and address a different object than the caller named.
func TestS3FSOpen_RejectsAPathOutsideTheCorpus(t *testing.T) {
	fsys, _ := newFakeS3FS(t, "corpus/", map[string][]byte{"corpus/keep.txt": []byte("hello")}, "")

	for _, rel := range []string{"../outside.mp4", "/absolute.mp4", "a/../../outside.txt"} {
		if _, err := fsys.Open(context.Background(), rel); err == nil {
			t.Fatalf("Open(%q) succeeded; it must be refused (#735)", rel)
		}
	}
	// The legitimate path still opens.
	rc, err := fsys.Open(context.Background(), "keep.txt")
	if err != nil {
		t.Fatalf("Open(keep.txt): %v", err)
	}
	_ = rc.Close()
}
