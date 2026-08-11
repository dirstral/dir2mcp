package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/corpusfs"
)

// ResolvedMaxFileBytes returns the effective `ingest.max_file_mb` cap in bytes.
//
// It is the single resolver for that cap. Discovery, every source-byte read, the
// object-store backend, and the on-demand MCP tool paths all read it here, so the
// four cannot drift into enforcing different numbers for the same setting. A
// zero or negative configured value selects the 10 MiB default rather than
// disabling the cap.
func ResolvedMaxFileBytes(cfg config.Config) int64 {
	if cfg.IngestMaxFileMB > 0 {
		return int64(cfg.IngestMaxFileMB) * 1024 * 1024
	}
	return corpusfs.DefaultMaxFileSizeBytes()
}

// sourceReadCapBytes is ResolvedMaxFileBytes for this service's configuration.
func (s *Service) sourceReadCapBytes() int64 {
	return ResolvedMaxFileBytes(s.cfg)
}

// readSourceBytes reads a corpus file's bytes through the corpus filesystem under
// a hard byte bound (#682).
//
// The bound is on the READ. Discovery measures a file with a stat (local) or a
// ListObjectsV2 entry (S3) and admits it when that number is under the cap, but
// the bytes arrive from a later, separate operation. A local file can grow between
// the two. A bucket can serve an object that no longer matches the size it was
// listed with, or that never did. So a size check, however it is repeated, only
// ever describes a measurement; it cannot constrain a subsequent read. Only a
// limit applied to the read itself can, and this is that limit.
//
// It reads at most cap+1 bytes. The extra byte is what separates "exactly at the
// cap" from "past it", so the caller can refuse the second case without having
// read a second file's worth of it.
//
// overCap=true is returned with a nil error, and with nil content. It is not a
// failure: the file is over the cap the operator configured, which is the
// `size_cap` skip of SPEC §15.2, and the caller records it as one. The truncated
// prefix is dropped rather than returned, so no path downstream can mistake part
// of a file for the whole of it.
//
// This limit is what decides in practice, and it is the only thing that can catch
// a LOCAL file that grew. The two corpusfs.ErrObjectTooLarge branches are belt and
// braces: an object-store backend bounds its own transport as well, and although
// its sentinel cannot surface through THIS path (the limit reader stops asking at
// cap+1, so the backend is never pushed past its own bound), a backend that
// refused earlier must produce the same answer rather than a second one the caller
// would have to classify.
func (s *Service) readSourceBytes(ctx context.Context, relPath string) (content []byte, overCap bool, err error) {
	capBytes := s.sourceReadCapBytes()
	rc, err := s.corpusFS().Open(ctx, relPath)
	if err != nil {
		if errors.Is(err, corpusfs.ErrObjectTooLarge) {
			return nil, true, nil
		}
		return nil, false, err
	}
	defer func() { _ = rc.Close() }()

	buf, err := io.ReadAll(io.LimitReader(rc, capBytes+1))
	if err != nil {
		if errors.Is(err, corpusfs.ErrObjectTooLarge) {
			return nil, true, nil
		}
		return nil, false, err
	}
	if int64(len(buf)) > capBytes {
		return nil, true, nil
	}
	return buf, false, nil
}

// sourceOverCapError renders the over-cap verdict as an error for the read paths
// whose caller has no document row to record a skip on (the two-phase derivation
// pass and the sidecar read). It wraps ErrFileTooLarge so §14.4 classification
// keeps reporting FILE_TOO_LARGE.
func (s *Service) sourceOverCapError(relPath string) error {
	return fmt.Errorf("%w: %s passed the ingest.max_file_mb cap (%d bytes) while being read", ErrFileTooLarge, relPath, s.sourceReadCapBytes())
}
