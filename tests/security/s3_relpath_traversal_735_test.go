package tests

import (
	"errors"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/relpath"
)

// #735: S3FS.Walk accepted object keys whose prefix-relative name was absolute
// or contained traversal segments, because relForKey stripped the configured
// prefix as a raw string and nothing downstream re-checked it. Under prefix
// "corpus/" the bucket key "corpus/../outside.mp4" became rel_path
// "../outside.mp4", and that value reaches code that joins it against the LOCAL
// root: video recognition builds RootDir + rel_path, and TTML/SMIL export
// probes the same way. SPEC §7.8 requires root/prefix isolation on every
// backend, rejected as PATH_OUTSIDE_ROOT.
//
// The pre-existing S3 walk test covered prefix mapping only, with no
// adversarial keys, which is why this shipped.

func TestUnsafeRelPathsAreRejected(t *testing.T) {
	// The exact strings from the issue, plus the neighbours they suggest.
	cases := []struct {
		name string
		rel  string
	}{
		{"parent traversal", "../outside.mp4"},
		{"traversal after a real segment", "a/../../outside.txt"},
		{"absolute, as a doubled prefix separator produces", "/absolute.mp4"},
		{"bare parent", ".."},
		{"bare current", "."},
		{"trailing traversal", "a/b/.."},
		{"interior traversal that cleans to a safe path", "a/../b"},
		{"empty", ""},
		{"whitespace only", "   "},
		{"windows separator", `..\outside.txt`},
		{"windows separator in a plain name", `a\b.txt`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := relpath.Normalize(tc.rel); err == nil {
				t.Fatalf("accepted %q as rel_path %q; SPEC §7.8 requires rejection", tc.rel, got)
			}
			if relpath.Valid(tc.rel) {
				t.Fatalf("Valid(%q) = true", tc.rel)
			}
		})
	}
}

// TestTraversalIsRejectedRatherThanResolved pins a decision the obvious fix
// gets wrong. path.Clean turns "a/../b" into "b", which IS a safe path, so a
// validator that only checks the CLEANED form accepts the key and reports it
// under a name that is not its key. Identity is meant to be the same rel_path
// set on every backend (§7.8), so the key is refused instead.
func TestTraversalIsRejectedRatherThanResolved(t *testing.T) {
	if _, err := relpath.Normalize("a/../b"); !errors.Is(err, relpath.ErrOutsideRoot) {
		t.Fatalf("a/../b: err = %v, want ErrOutsideRoot (not a silent rewrite to \"b\")", err)
	}
}

func TestOrdinaryKeysAreReturnedVerbatim(t *testing.T) {
	// An S3 key is an opaque byte string, so a valid one must come back
	// EXACTLY as it went in. Anything else records a rel_path that resolves to
	// a different object than the bucket listed.
	for _, key := range []string{
		"notes/a.md",
		"a.md",
		"deep/nested/path/x.txt",
		"a..b/c.txt",         // dots inside a name are ordinary bytes
		"...leading.txt",     //
		"weird name .txt",    // interior spaces
		" leading-space.txt", // and leading ones: keyForRel does not trim
		"trailing-space.txt ",
	} {
		got, err := relpath.Normalize(key)
		if err != nil {
			t.Fatalf("Normalize(%q) rejected a legitimate key: %v", key, err)
		}
		if got != key {
			t.Fatalf("Normalize(%q) = %q; a valid key must be returned unchanged", key, got)
		}
	}
}

// TestKeysThatWouldChangeUnderCleaningAreRejected is the other half of that.
// `a//b.txt` and `a/b.txt` are two different S3 objects, so returning the
// cleaned form for the first would have discovery record a rel_path that Open
// then resolves to the second, fetching an object the corpus never saw. The
// safe answer is to refuse the key, not to rewrite it.
func TestKeysThatWouldChangeUnderCleaningAreRejected(t *testing.T) {
	for _, key := range []string{
		"a//b.txt",  // doubled separator
		"a/./b.txt", // a `.` segment
		"a/b.txt/",  // trailing separator on a file key
		"./a.txt",   // leading `.` segment
	} {
		if got, err := relpath.Normalize(key); err == nil {
			t.Fatalf("Normalize(%q) = %q; rewriting it addresses a different object", key, got)
		}
	}
}

// TestALeadingSlashIsOutsideRootNotMerelyOdd: keyForRel strips a leading slash,
// so accepting "/absolute.mp4" would have Open address the object
// "prefix/absolute.mp4" while the corpus recorded "/absolute.mp4". The value
// does not round-trip, which is why it is rejected rather than trimmed.
func TestALeadingSlashIsOutsideRootNotMerelyOdd(t *testing.T) {
	if _, err := relpath.Normalize("/absolute.mp4"); !errors.Is(err, relpath.ErrOutsideRoot) {
		t.Fatalf("/absolute.mp4: err = %v, want ErrOutsideRoot", err)
	}
}

// TestBackslashIsRejectedRatherThanCleaned: a backslash is a legal byte in an
// S3 key and a path separator on Windows. Cleaning it with filepath.Clean would
// rewrite the key and address a DIFFERENT object; leaving it would let
// `..\outside` escape the root on Windows exactly as `../outside` does. So it
// is refused, and the reason is that it cannot round-trip either way.
func TestBackslashIsRejectedRatherThanCleaned(t *testing.T) {
	for _, rel := range []string{`a\b.txt`, `..\outside.txt`, `a/b\c.txt`} {
		if _, err := relpath.Normalize(rel); err == nil {
			t.Fatalf("accepted %q, which cannot round-trip between an S3 key and a local path", rel)
		}
	}
}

// TestRejectionReasonsAreDistinguishable: an operator seeing a rejected key
// needs to know whether the bucket is shaped oddly (not a relative path at all)
// or is trying to escape the corpus (PATH_OUTSIDE_ROOT, §17).
func TestRejectionReasonsAreDistinguishable(t *testing.T) {
	if _, err := relpath.Normalize(""); !errors.Is(err, relpath.ErrNotRelative) {
		t.Fatalf("empty: err = %v, want ErrNotRelative", err)
	}
	if _, err := relpath.Normalize("../x"); !errors.Is(err, relpath.ErrOutsideRoot) {
		t.Fatalf("../x: err = %v, want ErrOutsideRoot", err)
	}
	// The message an operator reads should name the boundary that was crossed.
	_, err := relpath.Normalize("../x")
	if !strings.Contains(err.Error(), "outside") {
		t.Fatalf("ErrOutsideRoot message %q does not mention the root boundary", err)
	}
}
