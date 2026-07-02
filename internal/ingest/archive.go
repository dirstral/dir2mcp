package ingest

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const archiveMemberMaxBytes = 10 * 1024 * 1024 // 10 MiB

// errUnsupportedArchiveFormat is returned by extractArchiveMembers when a path
// classified as an "archive" (classify.go) maps to a container the stdlib
// extractor cannot open (e.g. .xz/.7z/.rar). The caller surfaces it as a durable
// per-document diagnostic instead of silently ingesting an empty skipped
// document (#398).
var errUnsupportedArchiveFormat = errors.New("unsupported archive format")

// archiveMember holds extracted content from a single archive entry.
// RelPath is the virtual path "<archiveRelPath>/<memberPath>" used as the
// document's rel_path in the store.
type archiveMember struct {
	RelPath string
	Content []byte
}

// isSafeArchivePath returns true when the member path contains no traversal
// sequences that could escape the extraction root (zip-slip prevention).
func isSafeArchivePath(p string) bool {
	if strings.Contains(p, "..") {
		return false
	}
	// filepath.Clean normalises slashes; reject anything that escapes root.
	cleaned := filepath.ToSlash(filepath.Clean("/" + p))
	return strings.HasPrefix(cleaned, "/") && !strings.HasPrefix(cleaned, "/..")
}

// archiveFormat returns a canonical format string for the archive at relPath,
// or "" if the format is unsupported by the stdlib extractor.
func archiveFormat(relPath string) string {
	name := strings.ToLower(filepath.Base(relPath))
	switch {
	case strings.HasSuffix(name, ".tar.gz"), strings.HasSuffix(name, ".tgz"):
		return "tar.gz"
	case strings.HasSuffix(name, ".tar.bz2"):
		return "tar.bz2"
	case strings.HasSuffix(name, ".tar"):
		return "tar"
	case strings.HasSuffix(name, ".zip"):
		return "zip"
	// Bare single-file compressors (checked AFTER the .tar.* variants above so a
	// tarball is never misclassified as a single member).
	case strings.HasSuffix(name, ".gz"):
		return "gz"
	case strings.HasSuffix(name, ".bz2"):
		return "bz2"
	default:
		return ""
	}
}

// extractArchiveMembers dispatches to the correct extractor based on archiveFormat.
// Members that fail path safety checks or exceed archiveMemberMaxBytes are
// silently skipped; corrupted archives return whatever members were read before
// the error. An unrecognised format returns errUnsupportedArchiveFormat so the
// caller can surface a diagnostic instead of silently dropping the document.
func extractArchiveMembers(absPath, archiveRelPath string) ([]archiveMember, error) {
	switch format := archiveFormat(archiveRelPath); format {
	case "zip":
		return extractZipMembers(absPath, archiveRelPath)
	case "tar", "tar.gz", "tar.bz2":
		return extractTarMembers(absPath, archiveRelPath)
	case "gz", "bz2":
		return extractSingleCompressedMember(absPath, archiveRelPath, format)
	default:
		return nil, errUnsupportedArchiveFormat
	}
}

// extractSingleCompressedMember handles bare single-file compressors (gzip,
// bzip2) that wrap exactly one payload with no internal file tree. The decoded
// member's virtual path is "<archiveRelPath>/<base name minus the compression
// suffix>". A payload larger than archiveMemberMaxBytes is skipped (returns no
// members) rather than truncated. format is the canonical archiveFormat value
// already resolved by the caller ("gz" or "bz2"), passed through to avoid a
// duplicate path walk and keep the dispatch decision in one place.
func extractSingleCompressedMember(absPath, archiveRelPath, format string) ([]archiveMember, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("open compressed file: %w", err)
	}
	defer func() { _ = f.Close() }()

	var rd io.Reader
	switch format {
	case "gz":
		gr, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		defer func() { _ = gr.Close() }()
		rd = gr
	case "bz2":
		rd = bzip2.NewReader(f)
	}

	content, err := io.ReadAll(io.LimitReader(rd, archiveMemberMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("decompress %s: %w", archiveRelPath, err)
	}
	if int64(len(content)) > archiveMemberMaxBytes {
		return nil, nil // payload exceeds per-member cap; skip
	}

	base := filepath.Base(archiveRelPath)
	memberName := strings.TrimSuffix(base, filepath.Ext(base))
	if memberName == "" || memberName == base {
		memberName = base + ".out"
	}
	return []archiveMember{{
		RelPath: archiveRelPath + "/" + memberName,
		Content: content,
	}}, nil
}

func extractZipMembers(absPath, archiveRelPath string) ([]archiveMember, error) {
	r, err := zip.OpenReader(absPath)
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	defer func() { _ = r.Close() }()

	var members []archiveMember
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if !isSafeArchivePath(f.Name) {
			continue // zip-slip: skip silently
		}
		if f.UncompressedSize64 > archiveMemberMaxBytes {
			continue // member too large
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		content, readErr := io.ReadAll(io.LimitReader(rc, archiveMemberMaxBytes+1))
		_ = rc.Close()
		if readErr != nil || int64(len(content)) > archiveMemberMaxBytes {
			continue
		}
		members = append(members, archiveMember{
			RelPath: archiveRelPath + "/" + f.Name,
			Content: content,
		})
	}
	return members, nil
}

func extractTarMembers(absPath, archiveRelPath string) ([]archiveMember, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("open tar: %w", err)
	}
	defer func() { _ = f.Close() }()

	var rd io.Reader = f
	switch archiveFormat(archiveRelPath) {
	case "tar.gz":
		gr, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		defer func() { _ = gr.Close() }()
		rd = gr
	case "tar.bz2":
		rd = bzip2.NewReader(f)
	}

	tr := tar.NewReader(rd)
	var members []archiveMember
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			break // corrupted entry: return what we have
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if !isSafeArchivePath(hdr.Name) {
			continue
		}
		if hdr.Size > archiveMemberMaxBytes {
			continue
		}
		content, readErr := io.ReadAll(io.LimitReader(tr, archiveMemberMaxBytes+1))
		if readErr != nil || int64(len(content)) > archiveMemberMaxBytes {
			continue
		}
		members = append(members, archiveMember{
			RelPath: archiveRelPath + "/" + hdr.Name,
			Content: content,
		})
	}
	return members, nil
}
