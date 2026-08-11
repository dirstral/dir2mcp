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
	// excluded lists members the per-member size cap deliberately left out (#683).
	// See excludeOversize.
	excluded []archiveMemberExclusion
	// excludedLast holds the rel_paths whose LATEST decision, in archive order,
	// was an exclusion. Two entries can normalize onto one rel_path, and rel_path
	// is the store's document key, so only the last one is reachable. Without this
	// an earlier accepted member would overwrite the skip row of the later
	// oversized member that actually wins, and the corpus would claim the path is
	// indexed when it is not. See result.
	excludedLast map[string]struct{}
	// unreadable counts members (or whole entry streams) the archive declared but
	// that could not be read (#658). See recordUnreadable/recordStreamFailure.
	unreadable int
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
		excludedLast: make(map[string]struct{}),
	}
}

// entriesTaken is the number of archive entries this accumulator has already
// committed to, whether by ingesting them or by naming them as an exclusion.
// Both consume the same budget: each one costs memory that a hostile archive
// controls, and the member-count cap is the single ceiling on how many entries
// one archive may turn into work (#408).
func (a *memberAccumulator) entriesTaken() int {
	return len(a.members) + len(a.excluded)
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
	if a.entriesTaken() >= a.maxMembers {
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
	// This member is now the latest claim on the rel_path, so it supersedes any
	// earlier exclusion of the same path.
	delete(a.excludedLast, relPath)
	a.total += int64(len(content))
	a.members = append(a.members, archiveMember{RelPath: relPath, Content: content})
	return false
}

// excludeOversize records a member the per-member size cap deliberately left
// out (#683).
//
// It is deliberately NOT the same thing as a refused name. A refused name has
// no usable rel_path, so the unusable name IS the whole finding and a log line
// is all it can be. An oversized member has a perfectly good rel_path: the
// archive named it, the name passed the path rule, and only its size stopped
// it. That means the caller CAN key a durable status=skipped row on it, so the
// corpus reports the member as known-and-not-indexed instead of reporting
// coverage it does not have.
//
// sizeBytes is the size the archive declared, or 0 when the format does not
// declare one (a bare gzip/bzip2 payload is only known to be over the cap).
// The member is also recorded in skips so the operator log names it.
//
// It returns stop=true on exactly the same terms as add: naming an exclusion
// costs memory a hostile archive controls, so it draws on the SAME member-count
// budget, and extraction halts once that budget is spent. An exclusion is
// therefore never quietly dropped for want of room. Either it is recorded, or
// the run stopped at the cap and the caller reports the #408 truncation. The
// member is recorded in skips either way, and skips.Total stays exact.
func (a *memberAccumulator) excludeOversize(declaredName, relPath string, sizeBytes int64) (stop bool) {
	a.skips.record(declaredName, fmt.Sprintf("member exceeds the %d-byte per-member cap; recorded as a size_cap skip", int64(archiveMemberMaxBytes)))
	if a.entriesTaken() >= a.maxMembers {
		a.truncated = true
		return true
	}
	if _, dup := a.seen[relPath]; dup {
		a.skips.record(relPath, "collides with an earlier member's rel_path; the later member wins")
	}
	a.seen[relPath] = struct{}{}
	// This exclusion is now the latest claim on the rel_path. result() uses that
	// to drop any earlier accepted member that would otherwise overwrite the skip
	// row with an "indexed" one.
	a.excludedLast[relPath] = struct{}{}
	a.excluded = append(a.excluded, archiveMemberExclusion{RelPath: relPath, SizeBytes: sizeBytes})
	return false
}

// recordUnreadable records a member the archive declared but that could not be
// opened or read (#658): a corrupt entry, a truncated stream, an encrypted zip
// member. Unlike an oversized member this is a FAILURE, not a policy decision.
// Re-reading the archive later may well succeed (the bytes may be repaired, the
// transfer may be completed), so the caller must leave the container's done
// marker withheld and retry rather than record a permanent skip.
func (a *memberAccumulator) recordUnreadable(name, reason string) {
	a.unreadable++
	a.skips.record(name, reason)
}

// recordStreamFailure records a failure that ended the read of the archive
// itself rather than of one named member: a corrupt tar entry header, for
// example. No member name is available, because the entry the reader choked on
// was never decoded, and every entry after it is unreachable too. The count is
// therefore a floor of at least one, never an exact tally.
func (a *memberAccumulator) recordStreamFailure(reason string) {
	a.recordUnreadable("<archive stream>", reason)
}

// archiveMemberExclusion names one member that has a usable rel_path but that a
// deterministic cap deliberately kept out of the corpus (#683). The caller turns
// it into a durable status=skipped document row.
type archiveMemberExclusion struct {
	// RelPath is the corpus-relative path the member would have been stored
	// under, already prefixed with the archive's own rel_path.
	RelPath string
	// SizeBytes is the size the archive declared for the member, or 0 when the
	// format declares none.
	SizeBytes int64
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
	// Excluded lists members a deterministic cap deliberately kept out (#683).
	// The caller persists one durable status=skipped row per entry, so a
	// container whose largest member was dropped never reports full coverage.
	Excluded []archiveMemberExclusion
	// Unreadable counts members, or whole entry streams, that could not be read
	// (#658). It is a floor, not an exact tally: a corrupt tar header hides every
	// entry behind it. A non-zero value means extraction did NOT complete, so the
	// caller must not stamp the container's done marker.
	Unreadable int
}

// result converts the accumulator into the extractor's return value. Every
// extractor ends the same way, so the field-by-field copy lives in one place:
// a new observable field added to the accumulator cannot then be plumbed
// through one format and forgotten on another.
//
// It also settles collisions between the two lists. rel_path is the store's
// document key, so when several entries normalize onto one path only the last
// one in archive order is reachable, and the caller writes Members and Excluded
// in two separate passes with no ordering between them. Each list therefore
// keeps only the paths its own side claimed LAST. The two are then disjoint, so
// whichever pass runs first, the surviving row is the outcome the archive
// actually ends on: an oversized last entry stays a size_cap skip, and an
// under-cap last entry stays an indexed document.
func (a *memberAccumulator) result() archiveExtraction {
	members, excluded := a.members, a.excluded
	// Gate on the exclusions, never on excludedLast: an exclusion that a later
	// accepted member superseded empties that map while leaving the stale entry in
	// a.excluded, and skipping the filter would then write a skip row over the
	// member that actually wins.
	if len(a.excluded) > 0 {
		members = make([]archiveMember, 0, len(a.members))
		for _, m := range a.members {
			if _, superseded := a.excludedLast[m.RelPath]; !superseded {
				members = append(members, m)
			}
		}
		excluded = make([]archiveMemberExclusion, 0, len(a.excluded))
		for _, e := range a.excluded {
			if _, wins := a.excludedLast[e.RelPath]; wins {
				excluded = append(excluded, e)
			}
		}
	}
	return archiveExtraction{
		Members:    members,
		Skips:      a.skips,
		Truncated:  a.truncated,
		Excluded:   excluded,
		Unreadable: a.unreadable,
	}
}

// errUnsupportedArchiveFormat is returned by extractArchiveMembers when a path
// classified as an "archive" (classify.go) maps to a container the stdlib
// extractor cannot open (e.g. .xz/.7z/.rar). The caller surfaces it as a durable
// per-document diagnostic instead of silently ingesting an empty skipped
// document (#398).
var errUnsupportedArchiveFormat = errors.New("unsupported archive format")

// errArchiveUnreadable is returned by processArchiveMembers when the archive
// could not be read in full: the container itself failed to open or decompress,
// or one or more declared members could not be opened or read (#658).
//
// It is a DURABLE per-document failure, like errUnsupportedArchiveFormat. The
// members that did read are still ingested, but the container is persisted as
// status="error" with a redacted message and keeps an empty content_hash, so the
// gap is visible in status queries and the next incremental scan retries it.
// Without this signal a corrupt archive looked healthy: the good members landed,
// the bad ones vanished, and nothing said so.
var errArchiveUnreadable = errors.New("archive could not be read in full")

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
// Members that fail the path rule are dropped and recorded in the result's Skips
// so the caller can surface them (#718). Members over archiveMemberMaxBytes are
// additionally returned in Excluded, so the caller turns each one into a durable
// size_cap skip row (#683). Corrupted archives return whatever members were read
// before the error, with Unreadable>0 so the caller knows the read did not
// complete (#658). An unrecognised format returns errUnsupportedArchiveFormat so
// the caller can surface a diagnostic instead of silently dropping the document.
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
	memberRelPath := archiveRelPath + "/" + memberName

	if int64(len(content)) > archiveMemberMaxBytes {
		// Payload exceeds the per-member cap. The name is resolved above, so this
		// is an excluded member with a usable rel_path, not an unnameable drop:
		// the caller persists a durable size_cap skip row for it (#683). The
		// declared size is unknown, because the read stopped one byte past the cap
		// (see archiveMemberExclusion.SizeBytes).
		// The stop signal cannot fire here: a bare gzip/bzip2 holds one payload,
		// so no entry budget has been spent yet.
		_ = acc.excludeOversize(base, memberRelPath, 0)
		return acc.result(), nil
	}
	// Route through the shared accumulator so the aggregate-size cap is enforced
	// consistently with the zip/tar paths: if the single decoded member exceeds
	// maxTotalBytes, it is refused and truncated=true is surfaced (#408).
	acc.add(memberRelPath, content)
	return acc.result(), nil
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
		memberRelPath := archiveRelPath + "/" + memberRel
		if f.UncompressedSize64 > archiveMemberMaxBytes {
			if acc.excludeOversize(f.Name, memberRelPath, int64(f.UncompressedSize64)) {
				break // entry budget spent (#408)
			}
			continue
		}
		rc, err := f.Open()
		if err != nil {
			// Encrypted, or a corrupt local header. Not a policy skip: the member
			// may read on a later scan, so it must leave extraction incomplete.
			acc.recordUnreadable(f.Name, fmt.Sprintf("open failed: %v", err))
			continue
		}
		content, readErr := io.ReadAll(io.LimitReader(rc, archiveMemberMaxBytes+1))
		_ = rc.Close()
		if readErr != nil {
			acc.recordUnreadable(f.Name, fmt.Sprintf("read failed: %v", readErr))
			continue
		}
		if int64(len(content)) > archiveMemberMaxBytes {
			// The central directory understated the member. The cap still applies,
			// and the true size is unknown, so report the declared one.
			if acc.excludeOversize(f.Name, memberRelPath, int64(f.UncompressedSize64)) {
				break // entry budget spent (#408)
			}
			continue
		}
		if acc.add(memberRelPath, content) {
			break // member-count or aggregate-size cap hit (#408)
		}
	}
	return acc.result(), nil
}

// openTarStream opens the archive at absPath and returns the raw tar byte
// stream, unwrapping whichever compression the archive's name declares. cleanup
// closes everything the stream holds open and is non-nil whenever err is nil.
// Split out of extractTarMembers to keep that function within the complexity
// budget.
func openTarStream(absPath, archiveRelPath string) (rd io.Reader, cleanup func(), err error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open tar: %w", err)
	}
	closeFile := func() { _ = f.Close() }
	switch archiveFormat(archiveRelPath) {
	case "tar.gz":
		gr, err := gzip.NewReader(f)
		if err != nil {
			closeFile()
			return nil, nil, fmt.Errorf("gzip reader: %w", err)
		}
		return gr, func() { _ = gr.Close(); closeFile() }, nil
	case "tar.bz2":
		return bzip2.NewReader(f), closeFile, nil
	default:
		return f, closeFile, nil
	}
}

func extractTarMembers(absPath, archiveRelPath string, maxMembers int, maxTotalBytes int64) (archiveExtraction, error) {
	rd, cleanup, err := openTarStream(absPath, archiveRelPath)
	if err != nil {
		return archiveExtraction{}, err
	}
	defer cleanup()

	tr := tar.NewReader(rd)
	acc := newMemberAccumulator(maxMembers, maxTotalBytes)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			// A corrupt or truncated entry header ends the stream. Every entry
			// behind it is unreachable, so this is an incomplete read, not a clean
			// end of archive (#658). Record it and return the members read so far.
			acc.recordStreamFailure(fmt.Sprintf("entry stream failed: %v; the remaining entries could not be read", err))
			break
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		memberRel, err := archiveMemberRelPath(hdr.Name)
		if err != nil {
			acc.skips.record(hdr.Name, err.Error())
			continue
		}
		memberRelPath := archiveRelPath + "/" + memberRel
		if hdr.Size > archiveMemberMaxBytes {
			if acc.excludeOversize(hdr.Name, memberRelPath, hdr.Size) {
				break // entry budget spent (#408)
			}
			continue
		}
		content, readErr := io.ReadAll(io.LimitReader(tr, archiveMemberMaxBytes+1))
		if readErr != nil {
			acc.recordUnreadable(hdr.Name, fmt.Sprintf("read failed: %v", readErr))
			continue
		}
		if int64(len(content)) > archiveMemberMaxBytes {
			// The header understated the member. The cap still applies, and the true
			// size is unknown, so report the declared one.
			if acc.excludeOversize(hdr.Name, memberRelPath, hdr.Size) {
				break // entry budget spent (#408)
			}
			continue
		}
		if acc.add(memberRelPath, content) {
			break // member-count or aggregate-size cap hit (#408)
		}
	}
	return acc.result(), nil
}
