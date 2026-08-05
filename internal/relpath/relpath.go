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

// Normalize returns the canonical slash-separated form of rel, or an error.
//
// Accepts an ordinary relative path and collapses redundant separators and
// `.` segments (`a//b` and `a/./b` both become `a/b`). Rejects:
//
//   - empty or whitespace-only input
//   - an absolute path (`/x`), including one produced by a doubled prefix
//     separator, which is how `corpus//absolute.mp4` arrived
//   - `.` and `..`, and any path with a `..` segment, before OR after
//     cleaning, so `a/../../outside` is refused rather than resolved
//   - a backslash, which is a legal S3 key byte and a separator on Windows
func Normalize(rel string) (string, error) {
	trimmed := strings.TrimSpace(rel)
	if trimmed == "" {
		return "", ErrNotRelative
	}
	if strings.Contains(trimmed, `\`) {
		return "", ErrNotRelative
	}
	if strings.HasPrefix(trimmed, "/") {
		return "", ErrOutsideRoot
	}
	// Refuse a traversal segment in the INPUT as well as in the cleaned form.
	// path.Clean resolves `a/../b` to `b`, which is a safe path, but it is not
	// the path the bucket listed: accepting it would let a key address an
	// object under a name that no longer matches its key, and identity is
	// meant to be the same set of rel_paths on every backend (§7.8).
	if hasDotDot(trimmed) {
		return "", ErrOutsideRoot
	}
	cleaned := path.Clean(trimmed)
	if cleaned == "." || cleaned == ".." {
		return "", ErrOutsideRoot
	}
	if strings.HasPrefix(cleaned, "/") || hasDotDot(cleaned) {
		return "", ErrOutsideRoot
	}
	return cleaned, nil
}

// Valid reports whether rel is already a usable corpus-relative path.
func Valid(rel string) bool {
	_, err := Normalize(rel)
	return err == nil
}

func hasDotDot(p string) bool {
	for _, segment := range strings.Split(p, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}
