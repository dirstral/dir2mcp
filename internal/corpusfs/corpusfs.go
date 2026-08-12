// Package corpusfs abstracts corpus discovery and media reads behind a small
// interface so callers (ingest scanning, multimodal embedding) work the same
// against a local filesystem (an NFS mount is just a local path) or a remote
// object store such as S3, without knowing which.
//
// The interface deliberately exposes three operations rather than the usual
// Walk/Open pair: ffmpeg (avutil.ExtractSegment) and archive extraction need a
// real filesystem path, not an io.Reader, so Localize materializes a backend
// object to a local path (a no-op for LocalFS; a temp download for S3FS).
package corpusfs

import (
	"context"
	"io"
	"io/fs"
	"sort"
)

// defaultMaxFileSizeBytes mirrors the ingest discovery cap so a zero/negative
// Options.MaxSizeBytes resolves to the historical 10 MiB default.
const defaultMaxFileSizeBytes int64 = 10 * 1024 * 1024

// DefaultMaxFileSizeBytes returns the default discovery file-size cap.
func DefaultMaxFileSizeBytes() int64 {
	return defaultMaxFileSizeBytes
}

// StateDirName is the name of the state directory dir2mcp writes under the
// corpus root. It is always part of the resolved directory ignore list: the
// walk would otherwise read the index that the same run writes, and watch mode
// would feed its own writes back into ingest (SPEC §7.1).
const StateDirName = ".dir2mcp"

// defaultExcludedDirs are the directory names skipped during discovery when the
// operator sets no list. For S3 these are matched against path segments of the
// object key.
//
// SPEC §7.1 names these eight directories as the default ignore list. `dist`,
// `build`, and `.venv` were missing until #716, so a default scan indexed
// generated bundles, build output, and entire Python virtualenvs that operators
// had been told were ignored.
//
// This list is the DEFAULT, not a floor. `ingest.exclude_dirs` replaces it in
// full (#773), so adding a name here changes only the corpora that keep the
// default. It still removes documents from each of those corpora on the next
// reindex, so the list stays anchored to the spec rather than growing by taste.
var defaultExcludedDirs = map[string]struct{}{
	".git":         {},
	StateDirName:   {},
	"node_modules": {},
	"vendor":       {},
	"__pycache__":  {},
	"dist":         {},
	"build":        {},
	".venv":        {},
}

// DefaultExcludedDirs returns the default directory ignore list in sorted
// order. It is the value `internal/config` uses when `ingest.exclude_dirs` is
// absent, so the default lives in one place only.
func DefaultExcludedDirs() []string {
	names := make([]string, 0, len(defaultExcludedDirs))
	for name := range defaultExcludedDirs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ExcludedDirSet is a resolved directory ignore list: the names discovery does
// not descend into. Build one with ResolveExcludedDirs.
//
// The zero value resolves to the default list, so a caller that forgets to pass
// a set still excludes `.git/` and `.dir2mcp/` instead of indexing them.
//
// The rule is directory-only. A regular FILE named `dist` or `vendor` is an
// ordinary corpus document and must still be discovered, so callers must only
// consult the set for entries they already know are directories (the S3 backend
// tests it against ancestor key segments only).
//
// The match is exact. The name is a real directory entry or object key segment,
// and ` dist ` is a different directory than `dist`: an old TrimSpace here made
// discovery skip a legitimately-named tree whose documents nothing else would
// have excluded.
type ExcludedDirSet struct {
	names map[string]struct{}
}

// ResolveExcludedDirs returns the ignore set for the supplied names.
//
// A nil slice keeps the default list. A non-nil slice REPLACES the default list
// in full; it does not add to it (SPEC §7.1). `.dir2mcp` is always added back,
// because the state directory lives under the corpus root and to index it is
// self-referential. Blank names are dropped.
//
// This is the single resolver. The local walker, the S3 lister, and the ingest
// file watcher all read the set it returns, so the three cannot drift into
// judging the same directory differently, which is what #716 fixed and what
// keeping a second copy of the list would undo.
func ResolveExcludedDirs(names []string) ExcludedDirSet {
	if names == nil {
		return ExcludedDirSet{}
	}
	resolved := make(map[string]struct{}, len(names)+1)
	for _, name := range names {
		if name == "" {
			continue
		}
		resolved[name] = struct{}{}
	}
	resolved[StateDirName] = struct{}{}
	return ExcludedDirSet{names: resolved}
}

// Has reports whether a DIRECTORY with this name is skipped by discovery.
func (s ExcludedDirSet) Has(name string) bool {
	if s.names == nil {
		_, ok := defaultExcludedDirs[name]
		return ok
	}
	_, ok := s.names[name]
	return ok
}

// Names returns the resolved directory names in sorted order.
func (s ExcludedDirSet) Names() []string {
	if s.names == nil {
		return DefaultExcludedDirs()
	}
	names := make([]string, 0, len(s.names))
	for name := range s.names {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// DiscoveredFile holds metadata collected during corpus discovery.
//
// AbsPath is the local filesystem path for LocalFS; for object-store backends
// (S3FS) it is empty because there is no local path until Localize is called.
// ETag is the backend's content identifier when available (S3 object ETag) and
// empty for LocalFS.
type DiscoveredFile struct {
	AbsPath   string
	RelPath   string
	SizeBytes int64
	MTimeUnix int64
	Mode      fs.FileMode
	ETag      string
}

// Options controls discovery behavior. The fields mirror ingest.DiscoverOptions
// so the two can be converted without loss.
type Options struct {
	MaxSizeBytes   int64
	UseGitIgnore   bool
	FollowSymlinks bool
	// ExcludeDirs is the directory ignore list (config `ingest.exclude_dirs`,
	// SPEC §7.1). nil keeps the default list. A non-nil value REPLACES the
	// default list in full; it does not add to it. `.dir2mcp` is always kept.
	// See ResolveExcludedDirs, which every backend calls to read this field.
	ExcludeDirs []string
	// ScanCache, when non-nil, is an optional directory-discovery cache (issue
	// #267 item 5) consulted by the LocalFS walker. It lets an unchanged
	// directory skip re-reading and re-sorting its entries while still detecting
	// added/removed/modified files. nil disables it (a full re-walk every run);
	// only the local-filesystem backend honors it.
	ScanCache ScanCache
	// OnOversize, when non-nil, is invoked once for every regular file excluded
	// from discovery solely because its size exceeds MaxSizeBytes. It makes the
	// otherwise-silent size-cap drop observable to callers (issue #497): the file
	// is still excluded, but the caller can log/count it instead of the exclusion
	// vanishing without a trace. relPath is the corpus-root-relative slash path
	// and size is the file's size in bytes. It must not block or panic; the walker
	// calls it inline during the walk.
	OnOversize func(relPath string, size int64)
	// OnUnsafeKey, when non-nil, is invoked once for every object key excluded
	// because its prefix-relative name is not a usable corpus-relative path:
	// absolute, or carrying a `..` segment, or otherwise unable to round-trip
	// (#735). SPEC §7.8 requires such keys to be rejected on every backend, and
	// this makes the rejection observable instead of a silent skip, which is
	// what an operator needs to notice a bucket that is not shaped the way they
	// think it is. The key is the FULL object key, because the prefix-relative
	// form is exactly what could not be trusted. It must not block or panic;
	// the walker calls it inline.
	OnUnsafeKey func(key string, err error)
	// OnSkippedSymlink, when non-nil, is invoked once for every directory entry
	// dropped because it is a symlink and FollowSymlinks is false (#781).
	//
	// The refusal to follow links by default is not in question (#717 tightened
	// containment for the case where following IS enabled); its invisibility was.
	// A corpus populated with links into a media library walked to nothing and
	// reported `scanned: 0, skipped: 0, errors: 0`, which is indistinguishable
	// from an empty directory or a wrong root_dir. Same reasoning as OnOversize
	// (#497) and OnUnsafeKey (#735): a policy drop the caller cannot see is a
	// policy the operator cannot diagnose.
	//
	// relPath is the corpus-root-relative slash path of the LINK ITSELF, and it
	// fires for links to directories as well as to files: with following
	// disabled the walker never resolves the target, so it cannot tell the two
	// apart without doing the very thing it was told not to do. It must not
	// block or panic; the walker calls it inline during the walk.
	OnSkippedSymlink func(relPath string)
}

// CachedDirEntry is a directory child's identity recorded in the scan cache: its
// name, whether it is a directory, and (for regular files) the size/mtime used
// to detect an in-place modification. It is the minimal stat fingerprint the
// walker compares against the live filesystem on a cache hit.
type CachedDirEntry struct {
	Name      string
	IsDir     bool
	SizeBytes int64
	// MTimeUnixNano is the child's modification time in NANOSECONDS (#667).
	//
	// It was seconds. A same-size in-place edit inside one Unix second therefore
	// matched the cached entry, and the directory was served from cache instead of
	// being re-read. Nanoseconds separate those two writes on every filesystem
	// that keeps a sub-second stamp (ext4, APFS, XFS, btrfs, NTFS).
	//
	// Note this field is a fallback TRIGGER, not the change signal a caller acts
	// on: a validated child is emitted with its LIVE stat, so the file's reported
	// size and mtime are always current. See CachedDirSignature for the field that
	// carries the correctness weight.
	MTimeUnixNano int64
	// Mode is the file mode bits recorded for a regular file so a cache hit can
	// reconstruct the DiscoveredFile.Mode without re-stat beyond the size/mtime
	// confirmation the walker already performs.
	Mode uint32
}

// CachedDirSignature is the persisted fingerprint of a single directory: the
// directory's own mtime (which POSIX bumps on any add/remove/rename of a direct
// child) plus the sorted list of its direct children. A live directory whose
// mtime equals DirMTimeUnixNano has the same set of children as when the
// signature was stored, so the walker can validate the cached children with
// per-file stats instead of re-reading the directory.
//
// That deduction only holds when the recorded mtime is OLDER than the moment the
// child list was read, by more than the filesystem's timestamp granularity
// (#667). A child added in the same timestamp tick the walk read the directory in
// leaves the mtime equal to the recorded one, so the walk would keep serving a
// child list the new file is not in. The walker therefore refuses to store a
// signature for a directory whose mtime is not yet settled; see
// dirSignatureIsSettled in local.go for the rule and the proof.
type CachedDirSignature struct {
	DirMTimeUnixNano int64
	Entries          []CachedDirEntry
}

// ScanCache persists per-directory discovery signatures keyed by a directory's
// rel path (the corpus-root-relative slash path; "" is the root). It is a cheap,
// correctness-preserving optimization: the walker only ever trusts it after
// confirming the directory's own mtime is unchanged AND re-stat'ing every cached
// child, so a stale or wrong cache can never cause a changed file to be missed —
// at worst it triggers a full re-walk of the affected directory.
//
// Implementations must be safe for the walker's usage (sequential within one
// Walk). Lookup returning ok=false (or any inconsistency) must make the walker
// fall back to a full directory read.
//
// Timestamps in a signature are NANOSECONDS (#667). An implementation that
// persists them must keep the full precision; truncating to seconds reopens the
// window this cache used to have.
type ScanCache interface {
	// LookupDir returns the cached signature for relDir, or ok=false when none is
	// recorded. An error is treated by the walker as a cache miss (full re-walk).
	LookupDir(relDir string) (sig CachedDirSignature, ok bool, err error)
	// StoreDir records the freshly observed signature for relDir, replacing any
	// previous entry. Errors are non-fatal to discovery (the walk still returns
	// correct results; the cache simply does not improve next time).
	StoreDir(relDir string, sig CachedDirSignature) error
}

// DefaultOptions returns discovery defaults: the 10 MiB cap, gitignore off, and
// symlink following off (matching the historical ingest.DefaultDiscoverOptions).
func DefaultOptions() Options {
	return Options{
		MaxSizeBytes:   defaultMaxFileSizeBytes,
		UseGitIgnore:   false,
		FollowSymlinks: false,
	}
}

// CorpusFS abstracts corpus discovery and byte reads across local and remote
// backends.
type CorpusFS interface {
	// Walk enumerates the regular files under root that pass the supplied
	// discovery policies, sorted by RelPath. For object stores root is ignored
	// (the backend's configured prefix is the corpus root).
	Walk(ctx context.Context, root string, opts Options) ([]DiscoveredFile, error)

	// Open returns a seekable reader over the whole object at relPath. Backends
	// that support ranged reads (S3) satisfy seeks via range GETs so callers can
	// read a slice without downloading everything.
	Open(ctx context.Context, relPath string) (io.ReadSeekCloser, error)

	// Localize returns a real local filesystem path for relPath plus a cleanup
	// func the caller must invoke when done. For LocalFS this is the resolved
	// in-root path and a no-op cleanup; for object stores it downloads to a temp
	// file (extension preserved for muxer inference) and cleanup removes it.
	Localize(ctx context.Context, relPath string) (localPath string, cleanup func(), err error)
}

// MediaURLProvider is an optional capability a CorpusFS backend may implement to
// hand out a short-lived http(s) URL for an object so a range-seeking consumer
// (notably ffmpeg via avutil.ExtractSegmentURL) can read only the bytes it needs
// over HTTP instead of forcing a whole-object Localize download.
//
// It is deliberately separate from CorpusFS: only object stores that can mint a
// presigned URL (S3FS) implement it. LocalFS does not — a local file has no URL,
// so callers must type-assert and fall back to Localize when the assertion fails.
// This keeps LocalFS behavior byte-for-byte unchanged.
type MediaURLProvider interface {
	// MediaURL returns a time-limited http(s) URL granting read access to the
	// object at relPath. ok=false (with a nil error) means this backend cannot
	// produce a URL for the object and the caller should fall back to Localize;
	// a non-nil error means producing the URL failed and should be surfaced.
	MediaURL(ctx context.Context, relPath string) (url string, ok bool, err error)
}
