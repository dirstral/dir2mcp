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
)

// defaultMaxFileSizeBytes mirrors the ingest discovery cap so a zero/negative
// Options.MaxSizeBytes resolves to the historical 10 MiB default.
const defaultMaxFileSizeBytes int64 = 10 * 1024 * 1024

// DefaultMaxFileSizeBytes returns the default discovery file-size cap.
func DefaultMaxFileSizeBytes() int64 {
	return defaultMaxFileSizeBytes
}

// defaultExcludedDirs are directory names skipped during discovery regardless of
// backend. For S3 these are matched against path segments of the object key.
var defaultExcludedDirs = map[string]struct{}{
	".git":         {},
	".dir2mcp":     {},
	"node_modules": {},
	"vendor":       {},
	"__pycache__":  {},
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
