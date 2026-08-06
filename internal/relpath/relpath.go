// Package relpath is the one rule for what a corpus-relative path may be.
//
// SPEC §7.8 requires root/prefix isolation on EVERY backend: a `rel_path` or
// object key that resolves outside the configured root must be rejected with
// `PATH_OUTSIDE_ROOT`. The local walker got that from the filesystem, which
// cannot hand back a path above the directory it was asked to walk. S3
// discovery had no such floor: `relForKey` stripped the configured prefix as a
// raw string, so a bucket containing `corpus/../outside.mp4` produced
// `rel_path="../outside.mp4"` and it flowed on into code that joins it against
// the local root (#735).
//
// # Slashes, not separators
//
// A rel_path is slash-separated by definition, so this cleans with `path`
// rather than `filepath`. The difference matters at the S3 boundary in both
// directions:
//
//   - `filepath.Clean` on Windows treats `\` as a separator, so it would
//     rewrite an S3 key that legitimately contains a backslash and address a
//     DIFFERENT object than the one the bucket listed.
//   - a rel_path that still contains `\` cannot round-trip safely to a local
//     path on Windows, where `..\outside` escapes exactly as `../outside`
//     does. So a backslash is rejected here rather than cleaned, since the
//     byte is meaningful to S3 and dangerous locally, and silently rewriting
//     it would retarget the object.
package relpath

import (
	"errors"
	"path"
	"strings"
)

// ErrOutsideRoot is the sentinel for a path that is not inside the corpus
// root. Callers map it to the canonical `PATH_OUTSIDE_ROOT` (SPEC §17).
var ErrOutsideRoot = errors.New("path resolves outside the corpus root")

// ErrNotRelative is for input that is not a usable corpus-relative path at all
// (empty, or carrying a separator that cannot round-trip).
var ErrNotRelative = errors.New("not a corpus-relative path")

// Normalize validates rel and returns it UNCHANGED, or an error.
//
// It does not clean and it does not trim. An S3 key is an opaque byte string,
// so `a//b.txt` and `a/b.txt` name two different objects: returning the cleaned
// form for a key the bucket listed as the former would have discovery record a
// rel_path that `Open` then resolves to the latter, fetching an object the
// corpus never saw. Leading and trailing spaces are the same problem, which is
// why keyForRel deliberately does not trim either.
//
// So a path that WOULD change under cleaning is rejected rather than rewritten.
// That is stricter than it needs to be for safety alone (`a//b.txt` escapes
// nothing), and it is what keeps the §7.8 promise that the rel_path set is the
// same on every backend: a local walk can never produce `a//b.txt`, so an S3
// corpus must not either.
//
// Rejects:
//
//   - empty or whitespace-only input
//   - an absolute path (`/x`), including one produced by a doubled prefix
//     separator, which is how `corpus//absolute.mp4` arrived
//   - `.` and `..`, and any path with a `..` segment, before OR after cleaning
//   - a backslash, which is a legal S3 key byte and a separator on Windows
//   - anything whose cleaned form differs from itself: `a//b`, `a/./b`,
//     `a/b/`, and so on
func Normalize(rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", ErrNotRelative
	}
	if strings.Contains(rel, `\`) {
		return "", ErrNotRelative
	}
	if strings.HasPrefix(rel, "/") {
		return "", ErrOutsideRoot
	}
	if HasDotDotSegment(rel) {
		return "", ErrOutsideRoot
	}
	cleaned := path.Clean(rel)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "/") || HasDotDotSegment(cleaned) {
		return "", ErrOutsideRoot
	}
	if cleaned != rel {
		// Redundant separators, a `.` segment, a trailing slash: all name a
		// different byte string than the cleaned form, and S3 keys are bytes.
		return "", ErrNotRelative
	}
	return rel, nil
}

// Valid reports whether rel is already a usable corpus-relative path.
func Valid(rel string) bool {
	_, err := Normalize(rel)
	return err == nil
}

// HasDotDotSegment reports whether p has a `..` path SEGMENT.
//
// It is exported because "is this traversal?" kept being re-implemented as a
// substring test, and a substring test is wrong in the dangerous direction of
// dropping data: `v1..v2.txt`, `draft..final/report.md`, and `sub/...notes.md`
// are ordinary filenames, not traversal. Only a segment that is exactly `..`
// walks up a level. Callers that need the full containment rule should use
// Normalize; this is for the ones that clean first and only need the traversal
// question answered (archive members, the store's document key check; #718).
//
// p is slash-separated. A caller holding a path that may contain backslashes
// must reject or convert them first: on Windows `..\x` is traversal, and this
// function will not say so.
func HasDotDotSegment(p string) bool {
	for _, segment := range strings.Split(p, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}
