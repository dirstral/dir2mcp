package ingest

import (
	"context"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
)

// DefaultMaxFileSizeBytes returns the default ingest file-size cap.
func DefaultMaxFileSizeBytes() int64 {
	return corpusfs.DefaultMaxFileSizeBytes()
}

// DiscoveredFile holds metadata collected during file system discovery. It is
// an alias of corpusfs.DiscoveredFile so discovery can run against any CorpusFS
// backend (local filesystem, NFS path, or S3) without callers changing.
type DiscoveredFile = corpusfs.DiscoveredFile

// DiscoverOptions controls optional discovery behavior.
type DiscoverOptions struct {
	MaxSizeBytes   int64
	UseGitIgnore   bool
	FollowSymlinks bool
}

// DefaultDiscoverOptions returns discovery defaults used by ingestion.
func DefaultDiscoverOptions() DiscoverOptions {
	return DiscoverOptions{
		MaxSizeBytes:   corpusfs.DefaultMaxFileSizeBytes(),
		UseGitIgnore:   false,
		FollowSymlinks: false,
	}
}

// corpusfsOptions converts ingest DiscoverOptions to corpusfs.Options.
func (o DiscoverOptions) corpusfsOptions() corpusfs.Options {
	return corpusfs.Options{
		MaxSizeBytes:   o.MaxSizeBytes,
		UseGitIgnore:   o.UseGitIgnore,
		FollowSymlinks: o.FollowSymlinks,
	}
}

// DiscoverFiles walks rootDir and returns regular files that pass default
// discovery policies (skip symlinks, known heavy dirs, and over-limit files).
//
// It runs against a local-filesystem CorpusFS so existing callers keep working
// unchanged; the walk logic itself now lives in internal/corpusfs.
func DiscoverFiles(ctx context.Context, rootDir string, maxSizeBytes int64) ([]DiscoveredFile, error) {
	options := DefaultDiscoverOptions()
	options.MaxSizeBytes = maxSizeBytes
	return DiscoverFilesWithOptions(ctx, rootDir, options)
}

// DiscoverFilesWithOptions walks rootDir and returns regular files that match
// discovery policies and caller-provided options, via a local CorpusFS backend.
func DiscoverFilesWithOptions(ctx context.Context, rootDir string, options DiscoverOptions) ([]DiscoveredFile, error) {
	return corpusfs.NewLocalFS(rootDir).Walk(ctx, rootDir, options.corpusfsOptions())
}
