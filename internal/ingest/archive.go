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
	"path"
	"path/filepath"
	"strings"

	"github.com/dirstral/dir2mcp/internal/relpath"
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
	// archiveMaxReportedSkips bounds how many individual refused/dropped members
	// are retained for the caller's diagnostic (#718). The count is always exact;
	// only the per-member sample is capped, so a hostile archive with 100k bad
	// entries cannot turn an observability record into a memory problem.
	archiveMaxReportedSkips = 20
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
	// seen tracks the rel_paths already accumulated so two members that normalize
	// onto the same rel_path are reported instead of one silently overwriting the
	// other in the store (#718). See add.
	seen map[string]struct{}
	// skips is the observable record of members this archive did not yield.
	skips archiveSkips
}

func newMemberAccumulator(maxMembers int, maxTotalBytes int64) *memberAccumulator {
	if maxMembers <= 0 {
		maxMembers = archiveMaxMembers
	}
	if maxTotalBytes <= 0 {
		maxTotalBytes = archiveMaxTotalBytes
	}
	return &memberAccumulator{
		maxMembers:   maxMembers,
		maxTotalByte: maxTotalBytes,
		seen:         make(map[string]struct{}),
	}
}

// add appends a member unless a cap would be exceeded. It returns stop=true when
// a cap is hit: the member is NOT added and extraction should halt, and the
// truncated flag is set so the caller surfaces a diagnostic instead of silently
// dropping the remaining entries.
//
// A rel_path already accumulated is recorded as a collision. Both members are
// kept (rel_path is the store's document key, so the later upsert wins, matching
// tar's own last-entry-wins extraction semantics), but the earlier member's
// content stops being reachable, so the collapse is reported rather than silent.
func (a *memberAccumulator) add(relPath string, content []byte) (stop bool) {
	if len(a.members) >= a.maxMembers {
		a.truncated = true
		return true
	}
	if a.total+int64(len(content)) > a.maxTotalByte {
		a.truncated = true
		return true
	}
	if _, dup := a.seen[relPath]; dup {
		a.skips.record(relPath, "collides with an earlier member's rel_path; the later member wins")
	}
	a.seen[relPath] = struct{}{}
	a.total += int64(len(content))
	a.members = append(a.members, archiveMember{RelPath: relPath, Content: content})
	return false
}

// archiveMemberSkip names one member an archive declared that did not become a
// document, and why.
type archiveMemberSkip struct {
	// Name is the member name exactly as the archive declared it, never a
	// rewritten form: the point of the record is what the archive actually said.
	// (The one exception is a rel_path collision, which by definition is about
	// the shared normalized path rather than either declared name.)
	Name string
	// Reason is a short operator-facing explanation.
	Reason string
}

// archiveSkips is the extraction's admission record: how many members were
// refused or dropped, plus a bounded sample naming them.
//
// It exists because a refused member is otherwise invisible: the archive is
// indexed, the member simply is not there, and nothing distinguishes that from
// an archive that never contained the file (#718). Options.OnUnsafeKey (#735)
// and OnOversize (#497) are the same idea at discovery: an admission decision
// must leave a trace.
type archiveSkips struct {
	Entries []archiveMemberSkip
	Total   int
}

func (s *archiveSkips) record(name, reason string) {
	s.Total++
	if len(s.Entries) < archiveMaxReportedSkips {
		s.Entries = append(s.Entries, archiveMemberSkip{Name: name, Reason: reason})
	}
}

// archiveExtraction is the result of reading one archive: the members to ingest,
// the record of what was not ingested, and whether a fan-out cap stopped the
// read early (#408).
type archiveExtraction struct {
	Members   []archiveMember
	Skips     archiveSkips
	Truncated bool
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

// errArchiveMemberTraversal is the refusal for a member name that carries a `..`
// segment, before or after normalization.
var errArchiveMemberTraversal = errors.New("member name has a `..` segment")

// errArchiveMemberBackslash is the refusal for a member name containing a
// backslash. See archiveMemberRelPath for why it is refused rather than mapped.
var errArchiveMemberBackslash = errors.New("member name contains a backslash, which is a path separator on Windows")

// archiveMemberRelPath validates an archive member's declared name and returns
// the corpus-relative path the member is stored under, or an error explaining
// the refusal.
//
// # Containment
//
// internal/relpath is the one containment rule for a corpus-relative path
// (SPEC §7.8), and it has the final word here too: whatever this function
// returns must be a path relpath.Normalize accepts, so an archive member can
// never be admitted under a name that S3 discovery or the local walker would
// refuse. Membership in `<archive>/...` is then guaranteed by construction:
// caller prefixes the archive's own (already valid) rel_path.
//
// # Why this is not relpath.Normalize on its own
//
// relpath deliberately REJECTS rather than cleans a path that would change under
// normalization: an S3 key is the address used to re-fetch the object, so
// rewriting `a//b.txt` to `a/b.txt` would make discovery record a rel_path that
// Open later resolves to a DIFFERENT object.
//
// An archive member is not an address. Its bytes are read from the archive
// stream by position, never re-resolved by name, so normalizing the name cannot
// retarget anything. And the difference is not academic. `tar -czf x.tgz .`,
// the most common way a tarball is produced, names every member `./file.txt`,
// which relpath.Normalize refuses. Applying the S3 rule verbatim would refuse
// entire ordinary tarballs, trading one silent-drop bug for a much larger one.
//
// So pure normalization noise (`./`, `//`, a trailing `/`) is cleaned. What
// cleaning CAN still do is collapse two distinct members onto one rel_path; that
// is bounded to inside the archive's namespace and is reported by
// memberAccumulator.add rather than left silent.
//
// # Refused
//
//   - an empty or whitespace-only name
//   - a backslash anywhere: it is a legal byte in a POSIX member name and a path
//     separator on Windows, where `..\escape` escapes exactly as `../escape`
//     does. We cannot tell the two apart from the name alone, and guessing wrong
//     in the unsafe direction is a traversal. Same call relpath makes.
//   - an absolute name (`/etc/passwd`, `//etc/passwd`)
//   - any `..` segment, checked BEFORE cleaning as well as after. `a/../b` does
//     not escape, but cleaning it to `b` would file the member under a name the
//     archive gave to a different entry, so it is refused rather than guessed at.
func archiveMemberRelPath(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", relpath.ErrNotRelative
	}
	if strings.Contains(name, `\`) {
		return "", errArchiveMemberBackslash
	}
	if strings.HasPrefix(name, "/") {
		return "", relpath.ErrOutsideRoot
	}
	if relpath.HasDotDotSegment(name) {
		return "", errArchiveMemberTraversal
	}
	// path.Clean (not filepath.Clean): a member path is slash-separated by
	// definition, and on Windows filepath.Clean would treat a backslash as a
	// separator (already refused above, but the distinction is load-bearing).
	cleaned := path.Clean(name)
	if relpath.HasDotDotSegment(cleaned) {
		return "", errArchiveMemberTraversal
	}
	// relpath has the final word: `.`, an absolute result, or anything else that
	// is not a usable rel_path is refused here, by the same rule every backend
	// uses.
	return relpath.Normalize(cleaned)
}

// isSafeArchiveMemberName reports whether name is safe to use as the final path
// segment of a single-compressed member's rel_path. It rejects empty/blank
// names, the "."/".." traversal segments, and embedded path separators, but NOT
// a name that merely contains consecutive dots, which is an ordinary filename
// (`report..final.txt`).
func isSafeArchiveMemberName(name string) bool {
	if strings.TrimSpace(name) == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, `/\`)
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
// Members that fail the path rule or exceed archiveMemberMaxBytes are dropped and
// recorded in the result's Skips so the caller can surface them (#718); corrupted
// archives return whatever members were read before the error. An unrecognised
// format returns errUnsupportedArchiveFormat so the caller can surface a
// diagnostic instead of silently dropping the document.
//
// maxMembers and maxTotalBytes bound the member-count and aggregate-uncompressed
// fan-out (#408); pass 0 for the package defaults. The returned Truncated flag is
// true when extraction stopped early because a cap was hit — the members returned
// before the cap are still valid and should be ingested, and the caller is
// expected to log a warning so the truncation is visible.
func extractArchiveMembers(absPath, archiveRelPath string, maxMembers int, maxTotalBytes int64) (archiveExtraction, error) {
	switch format := archiveFormat(archiveRelPath); format {
	case "zip":
		return extractZipMembers(absPath, archiveRelPath, maxMembers, maxTotalBytes)
	case "tar", "tar.gz", "tar.bz2":
		return extractTarMembers(absPath, archiveRelPath, maxMembers, maxTotalBytes)
	case "gz", "bz2":
		return extractSingleCompressedMember(absPath, archiveRelPath, format, maxMembers, maxTotalBytes)
	default:
		return archiveExtraction{}, errUnsupportedArchiveFormat
	}
}

// extractSingleCompressedMember handles bare single-file compressors (gzip,
// bzip2) that wrap exactly one payload with no internal file tree. The decoded
// member's virtual path is "<archiveRelPath>/<base name minus the compression
// suffix>". A payload larger than archiveMemberMaxBytes is skipped (returns no
// members) rather than truncated. format is the canonical archiveFormat value
// already resolved by the caller ("gz" or "bz2"), passed through to avoid a
// duplicate path walk and keep the dispatch decision in one place.
//
// maxMembers/maxTotalBytes are the same fan-out caps applied on the zip/tar
// paths (#408); they run through the shared memberAccumulator so the aggregate
// -size cap is honored consistently. The member-count cap is effectively 1 here
// (a single payload), but the size cap must still be respected: a decoded member
// larger than maxTotalBytes is refused and truncated=true is returned, matching
// how the zip/tar paths signal truncation.
func extractSingleCompressedMember(absPath, archiveRelPath, format string, maxMembers int, maxTotalBytes int64) (archiveExtraction, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return archiveExtraction{}, fmt.Errorf("open compressed file: %w", err)
	}
	defer func() { _ = f.Close() }()

	var rd io.Reader
	switch format {
	case "gz":
		gr, err := gzip.NewReader(f)
		if err != nil {
			return archiveExtraction{}, fmt.Errorf("gzip reader: %w", err)
		}
		defer func() { _ = gr.Close() }()
		rd = gr
	case "bz2":
		rd = bzip2.NewReader(f)
	default:
		// Guard against a nil reader: the caller only dispatches "gz"/"bz2" here,
		// but an unexpected value must fail loudly instead of dereferencing nil.
		return archiveExtraction{}, fmt.Errorf("unsupported single-compressed format %q", format)
	}

	content, err := io.ReadAll(io.LimitReader(rd, archiveMemberMaxBytes+1))
	if err != nil {
		return archiveExtraction{}, fmt.Errorf("decompress %s: %w", archiveRelPath, err)
	}
	acc := newMemberAccumulator(maxMembers, maxTotalBytes)
	if int64(len(content)) > archiveMemberMaxBytes {
		// Payload exceeds the per-member cap: skipped, but recorded so the drop is
		// visible rather than an empty archive with no explanation (#718).
		acc.skips.record(filepath.Base(archiveRelPath), fmt.Sprintf("decompressed payload exceeds the %d-byte per-member cap", int64(archiveMemberMaxBytes)))
		return archiveExtraction{Skips: acc.skips}, nil
	}

	base := filepath.Base(archiveRelPath)
	memberName := strings.TrimSuffix(base, filepath.Ext(base))
	if memberName == "" || memberName == base {
		memberName = base + ".out"
	}
	// Guard against edge-case archive names whose stripped member name is a
	// traversal segment (e.g. "..gz" -> "." or "..bz2" -> "."): such a name would
	// yield a "<archive>/.." style rel_path that escapes the archive namespace.
	// Fall back to a benign synthetic name, and record the substitution so the
	// stored name is not silently unrelated to the file on disk.
	if !isSafeArchiveMemberName(memberName) {
		acc.skips.record(memberName, "derived member name is not a usable path segment; stored as \"member\"")
		memberName = "member"
	}
	// Route through the shared accumulator so the aggregate-size cap is enforced
	// consistently with the zip/tar paths: if the single decoded member exceeds
	// maxTotalBytes, it is refused and truncated=true is surfaced (#408).
	acc.add(archiveRelPath+"/"+memberName, content)
	return archiveExtraction{Members: acc.members, Skips: acc.skips, Truncated: acc.truncated}, nil
}

func extractZipMembers(absPath, archiveRelPath string, maxMembers int, maxTotalBytes int64) (archiveExtraction, error) {
	r, err := zip.OpenReader(absPath)
	if err != nil {
		return archiveExtraction{}, fmt.Errorf("open zip: %w", err)
	}
	defer func() { _ = r.Close() }()

	acc := newMemberAccumulator(maxMembers, maxTotalBytes)
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		memberRel, err := archiveMemberRelPath(f.Name)
		if err != nil {
			acc.skips.record(f.Name, err.Error()) // zip-slip and friends
			continue
		}
		if f.UncompressedSize64 > archiveMemberMaxBytes {
			acc.skips.record(f.Name, fmt.Sprintf("member exceeds the %d-byte per-member cap", int64(archiveMemberMaxBytes)))
			continue
		}
		rc, err := f.Open()
		if err != nil {
			acc.skips.record(f.Name, fmt.Sprintf("open failed: %v", err))
			continue
		}
		content, readErr := io.ReadAll(io.LimitReader(rc, archiveMemberMaxBytes+1))
		_ = rc.Close()
		if readErr != nil {
			acc.skips.record(f.Name, fmt.Sprintf("read failed: %v", readErr))
			continue
		}
		if int64(len(content)) > archiveMemberMaxBytes {
			acc.skips.record(f.Name, fmt.Sprintf("member exceeds the %d-byte per-member cap", int64(archiveMemberMaxBytes)))
			continue
		}
		if acc.add(archiveRelPath+"/"+memberRel, content) {
			break // member-count or aggregate-size cap hit (#408)
		}
	}
	return archiveExtraction{Members: acc.members, Skips: acc.skips, Truncated: acc.truncated}, nil
}

func extractTarMembers(absPath, archiveRelPath string, maxMembers int, maxTotalBytes int64) (archiveExtraction, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return archiveExtraction{}, fmt.Errorf("open tar: %w", err)
	}
	defer func() { _ = f.Close() }()

	var rd io.Reader = f
	switch archiveFormat(archiveRelPath) {
	case "tar.gz":
		gr, err := gzip.NewReader(f)
		if err != nil {
			return archiveExtraction{}, fmt.Errorf("gzip reader: %w", err)
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
		memberRel, err := archiveMemberRelPath(hdr.Name)
		if err != nil {
			acc.skips.record(hdr.Name, err.Error())
			continue
		}
		if hdr.Size > archiveMemberMaxBytes {
			acc.skips.record(hdr.Name, fmt.Sprintf("member exceeds the %d-byte per-member cap", int64(archiveMemberMaxBytes)))
			continue
		}
		content, readErr := io.ReadAll(io.LimitReader(tr, archiveMemberMaxBytes+1))
		if readErr != nil {
			acc.skips.record(hdr.Name, fmt.Sprintf("read failed: %v", readErr))
			continue
		}
		if int64(len(content)) > archiveMemberMaxBytes {
			acc.skips.record(hdr.Name, fmt.Sprintf("member exceeds the %d-byte per-member cap", int64(archiveMemberMaxBytes)))
			continue
		}
		if acc.add(archiveRelPath+"/"+memberRel, content) {
			break // member-count or aggregate-size cap hit (#408)
		}
	}
	return archiveExtraction{Members: acc.members, Skips: acc.skips, Truncated: acc.truncated}, nil
}
