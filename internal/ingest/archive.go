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

const (
	// archiveMaxMembers bounds the number of members ingested from a single
	// archive. Without it a tar/zip with thousands of entries is fully buffered
	// in memory before ingestion (#408). Members past the cap are skipped and a
	// truncation flag is returned so the caller can surface a diagnostic instead
	// of a silent drop.
	archiveMaxMembers = 4096
	// archiveMaxTotalBytes bounds the aggregate uncompressed bytes buffered from a
	// single archive — a decompression-bomb guard (a small compressed input
	// expanding into many ~10 MiB members). Extraction stops once adding the next
	// member would exceed this ceiling, bounding peak memory to roughly this value
	// plus one member (#408).
	archiveMaxTotalBytes = 512 * 1024 * 1024 // 512 MiB
)

// memberAccumulator collects archive members while enforcing the member-count
// and aggregate-uncompressed-size caps (#408). A non-positive cap falls back to
// the package default so callers can pass 0 for "use default".
type memberAccumulator struct {
	members      []archiveMember
	total        int64
	maxMembers   int
	maxTotalByte int64
	truncated    bool
}

func newMemberAccumulator(maxMembers int, maxTotalBytes int64) *memberAccumulator {
	if maxMembers <= 0 {
		maxMembers = archiveMaxMembers
	}
	if maxTotalBytes <= 0 {
		maxTotalBytes = archiveMaxTotalBytes
	}
	return &memberAccumulator{maxMembers: maxMembers, maxTotalByte: maxTotalBytes}
}

// add appends a member unless a cap would be exceeded. It returns stop=true when
// a cap is hit: the member is NOT added and extraction should halt, and the
// truncated flag is set so the caller surfaces a diagnostic instead of silently
// dropping the remaining entries.
func (a *memberAccumulator) add(relPath string, content []byte) (stop bool) {
	if len(a.members) >= a.maxMembers {
		a.truncated = true
		return true
	}
	if a.total+int64(len(content)) > a.maxTotalByte {
		a.truncated = true
		return true
	}
	a.total += int64(len(content))
	a.members = append(a.members, archiveMember{RelPath: relPath, Content: content})
	return false
}

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

// isSafeArchiveMemberName reports whether name is safe to use as the final path
// segment of a single-compressed member's rel_path. It rejects empty names, the
// "."/".." traversal segments, embedded path separators, and any ".." sequence
// (mirroring isSafeArchivePath's traversal rejection).
func isSafeArchiveMemberName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	return !strings.Contains(name, "..")
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
//
// maxMembers and maxTotalBytes bound the member-count and aggregate-uncompressed
// fan-out (#408); pass 0 for the package defaults. The returned truncated flag is
// true when extraction stopped early because a cap was hit — the members returned
// before the cap are still valid and should be ingested, and the caller is
// expected to log a warning so the truncation is visible.
func extractArchiveMembers(absPath, archiveRelPath string, maxMembers int, maxTotalBytes int64) (members []archiveMember, truncated bool, err error) {
	switch format := archiveFormat(archiveRelPath); format {
	case "zip":
		return extractZipMembers(absPath, archiveRelPath, maxMembers, maxTotalBytes)
	case "tar", "tar.gz", "tar.bz2":
		return extractTarMembers(absPath, archiveRelPath, maxMembers, maxTotalBytes)
	case "gz", "bz2":
		m, err := extractSingleCompressedMember(absPath, archiveRelPath, format)
		return m, false, err
	default:
		return nil, false, errUnsupportedArchiveFormat
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
	default:
		// Guard against a nil reader: the caller only dispatches "gz"/"bz2" here,
		// but an unexpected value must fail loudly instead of dereferencing nil.
		return nil, fmt.Errorf("unsupported single-compressed format %q", format)
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
	// Guard against edge-case archive names whose stripped member name is a
	// traversal segment (e.g. "..gz" -> "." or "..bz2" -> "."): such a name would
	// yield a "<archive>/.." style rel_path that escapes the archive namespace.
	// Mirror isSafeArchivePath's traversal rejection and fall back to a benign
	// synthetic name.
	if !isSafeArchiveMemberName(memberName) {
		memberName = "member"
	}
	return []archiveMember{{
		RelPath: archiveRelPath + "/" + memberName,
		Content: content,
	}}, nil
}

func extractZipMembers(absPath, archiveRelPath string, maxMembers int, maxTotalBytes int64) ([]archiveMember, bool, error) {
	r, err := zip.OpenReader(absPath)
	if err != nil {
		return nil, false, fmt.Errorf("open zip: %w", err)
	}
	defer func() { _ = r.Close() }()

	acc := newMemberAccumulator(maxMembers, maxTotalBytes)
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
		if acc.add(archiveRelPath+"/"+f.Name, content) {
			break // member-count or aggregate-size cap hit (#408)
		}
	}
	return acc.members, acc.truncated, nil
}

func extractTarMembers(absPath, archiveRelPath string, maxMembers int, maxTotalBytes int64) ([]archiveMember, bool, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, false, fmt.Errorf("open tar: %w", err)
	}
	defer func() { _ = f.Close() }()

	var rd io.Reader = f
	switch archiveFormat(archiveRelPath) {
	case "tar.gz":
		gr, err := gzip.NewReader(f)
		if err != nil {
			return nil, false, fmt.Errorf("gzip reader: %w", err)
		}
		defer func() { _ = gr.Close() }()
		rd = gr
	case "tar.bz2":
		rd = bzip2.NewReader(f)
	}

	tr := tar.NewReader(rd)
	acc := newMemberAccumulator(maxMembers, maxTotalBytes)
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
		if acc.add(archiveRelPath+"/"+hdr.Name, content) {
			break // member-count or aggregate-size cap hit (#408)
		}
	}
	return acc.members, acc.truncated, nil
}
