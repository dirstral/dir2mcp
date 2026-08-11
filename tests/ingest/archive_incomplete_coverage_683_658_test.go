package tests

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// archiveMemberCapBytes683 mirrors internal/ingest.archiveMemberMaxBytes, the
// fixed per-member cap. The suite is an external black-box package, so the value
// is restated here rather than imported.
const archiveMemberCapBytes683 = 10 * 1024 * 1024

// oversizePayload683 returns a payload one byte past the per-member cap. The
// bytes repeat, so every archive format compresses it to a few kilobytes.
func oversizePayload683() []byte {
	return payload683(archiveMemberCapBytes683 + 1)
}

func payload683(n int) []byte {
	unit := []byte("dir2mcp oversized archive member payload\n")
	out := bytes.Repeat(unit, n/len(unit)+1)
	return out[:n]
}

// sizedMember683 is one archive entry with an explicit byte count.
type sizedMember683 struct {
	name string
	body []byte
}

func buildZipSized683(t *testing.T, members []sizedMember683) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, m := range members {
		fw, err := w.Create(m.name)
		if err != nil {
			t.Fatalf("zip create %q: %v", m.name, err)
		}
		if _, err := fw.Write(m.body); err != nil {
			t.Fatalf("zip write %q: %v", m.name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func buildTarGzSized683(t *testing.T, members []sizedMember683) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, m := range members {
		hdr := &tar.Header{Name: m.name, Size: int64(len(m.body)), Typeflag: tar.TypeReg, Mode: 0o644}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %q: %v", m.name, err)
		}
		if _, err := tw.Write(m.body); err != nil {
			t.Fatalf("tar write %q: %v", m.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func buildGz683(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(body); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// oversizeBz2Fixture683 is a checked-in bzip2 stream that decodes to exactly
// oversizePayload683(). Go ships no bzip2 writer and this repository pulls in no
// third-party compressor, so the alternative was to shell out to a system
// bzip2. That would let CI pass while silently skipping the only bare-bzip2
// case. The fixture is 2 KB and regenerates with:
//
//	python3 -c 'import bz2,sys; u=b"dir2mcp oversized archive member payload\n"; \
//	  n=10*1024*1024+1; sys.stdout.buffer.write(bz2.compress((u*(n//len(u)+1))[:n],9))' \
//	  > tests/ingest/testdata/oversize_member_683.bz2
const oversizeBz2Fixture683 = "testdata/oversize_member_683.bz2"

func readOversizeBz2Fixture683(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(oversizeBz2Fixture683)
	if err != nil {
		t.Fatalf("read bzip2 fixture: %v", err)
	}
	// Guard the fixture itself: a truncated or regenerated file that no longer
	// decodes past the cap would make the test vacuous.
	decoded, err := io.ReadAll(bzip2.NewReader(bytes.NewReader(data)))
	if err != nil {
		t.Fatalf("decode bzip2 fixture: %v", err)
	}
	if len(decoded) <= archiveMemberCapBytes683 {
		t.Fatalf("bzip2 fixture decodes to %d bytes, want more than the %d-byte cap", len(decoded), archiveMemberCapBytes683)
	}
	return data
}

// assertSizeCapSkip683 asserts that relPath exists as a durable skipped document
// carrying the spec's size_cap reason.
func assertSizeCapSkip683(t *testing.T, st *store.SQLiteStore, relPath string) {
	t.Helper()
	doc := documentByPath(t, st, relPath)
	if doc.Status != "skipped" {
		t.Errorf("%s status = %q, want \"skipped\" (an omitted member must be durable coverage, not an absence)", relPath, doc.Status)
	}
	if doc.SkipReason != model.SkipReasonSizeCap {
		t.Errorf("%s skip_reason = %q, want %q", relPath, doc.SkipReason, model.SkipReasonSizeCap)
	}
}

// TestArchiveOversizeMember683_ZipRecordsSizeCapSkip is the #683 regression
// guard for zip. Before the fix the oversized member was dropped with a log line
// and no document row, while the container was finalized as fully processed, so
// the corpus reported coverage it did not have.
func TestArchiveOversizeMember683_ZipRecordsSizeCapSkip(t *testing.T) {
	data := buildZipSized683(t, []sizedMember683{
		{name: "small.txt", body: []byte("a small member")},
		{name: "huge.txt", body: oversizePayload683()},
	})
	st := runArchiveIngest(t, "docs.zip", data)

	if !docPaths(t, st)["docs.zip/small.txt"] {
		t.Error("the under-cap member must still be indexed (best-effort ingestion is preserved)")
	}
	assertSizeCapSkip683(t, st, "docs.zip/huge.txt")
}

// TestArchiveOversizeMember683_TarRecordsSizeCapSkip is the #683 guard for the
// tar path, which took its own branch past the same cap.
func TestArchiveOversizeMember683_TarRecordsSizeCapSkip(t *testing.T) {
	data := buildTarGzSized683(t, []sizedMember683{
		{name: "small.txt", body: []byte("a small member")},
		{name: "huge.txt", body: oversizePayload683()},
	})
	st := runArchiveIngest(t, "bundle.tar.gz", data)

	if !docPaths(t, st)["bundle.tar.gz/small.txt"] {
		t.Error("the under-cap member must still be indexed (best-effort ingestion is preserved)")
	}
	assertSizeCapSkip683(t, st, "bundle.tar.gz/huge.txt")
}

// TestArchiveOversizeMember683_GzipRecordsSizeCapSkip is the #683 guard for a
// bare gzip payload. This is the worst case in the issue: the archive holds one
// member, so an over-cap payload made the whole container look empty.
func TestArchiveOversizeMember683_GzipRecordsSizeCapSkip(t *testing.T) {
	st := runArchiveIngest(t, "report.txt.gz", buildGz683(t, oversizePayload683()))
	assertSizeCapSkip683(t, st, "report.txt.gz/report.txt")
}

// TestArchiveOversizeMember683_Bzip2RecordsSizeCapSkip is the #683 guard for a
// bare bzip2 payload.
func TestArchiveOversizeMember683_Bzip2RecordsSizeCapSkip(t *testing.T) {
	st := runArchiveIngest(t, "report.txt.bz2", readOversizeBz2Fixture683(t))
	assertSizeCapSkip683(t, st, "report.txt.bz2/report.txt")
}

// TestArchiveOversizeMember683_ContainerStaysFinalized pins the other half of
// the #683 contract. The per-member cap is deterministic, so re-extraction can
// never recover the member. Once the omission is durably recorded the container
// IS done, and it must keep its content_hash so incremental scans do not
// re-extract it forever.
func TestArchiveOversizeMember683_ContainerStaysFinalized(t *testing.T) {
	data := buildZipSized683(t, []sizedMember683{
		{name: "small.txt", body: []byte("a small member")},
		{name: "huge.txt", body: oversizePayload683()},
	})
	st := runArchiveIngest(t, "docs.zip", data)

	container := documentByPath(t, st, "docs.zip")
	if container.Status != "skipped" {
		t.Errorf("container status = %q, want \"skipped\" (a size-cap omission is not a container failure)", container.Status)
	}
	if container.ContentHash == "" {
		t.Error("container content_hash is blank; a deterministic cap must not force a re-extract on every scan")
	}
}

// TestArchiveOversizeMember683_SkipRowSurvivesSecondScan pins that the durable
// record is durable. The second scan sees an unchanged archive and re-extracts
// nothing, so the size_cap row must be retained rather than tombstoned as a
// missing document. A coverage gap that only lasts one run is not honest
// coverage.
func TestArchiveOversizeMember683_SkipRowSurvivesSecondScan(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	data := buildZipSized683(t, []sizedMember683{
		{name: "small.txt", body: []byte("a small member")},
		{name: "huge.txt", body: oversizePayload683()},
	})
	if err := os.WriteFile(filepath.Join(root, "docs.zip"), data, 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Default()
	cfg.RootDir = root
	svc := mustNewIngestService(t, cfg, st)
	for i := 1; i <= 2; i++ {
		if err := svc.Run(ctx); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	doc := documentByPath(t, st, "docs.zip/huge.txt")
	if doc.Deleted {
		t.Error("the size_cap row was tombstoned on the second scan; the coverage gap must outlive the run that found it")
	}
	assertSizeCapSkip683(t, st, "docs.zip/huge.txt")
}

// TestArchiveOversizeMember683_LastEntryWinsOnCollision covers the collision
// case. Two entries can normalize onto one rel_path, and rel_path is the store's
// document key, so only the last one is reachable. The final row must therefore
// describe the LAST entry.
//
// Both directions matter. A small member followed by an oversized twin must end
// as a size_cap skip, not as an indexed document that claims coverage the corpus
// does not have. An oversized member followed by a small twin must end indexed,
// not as a skip that hides a member which is really there.
func TestArchiveOversizeMember683_LastEntryWinsOnCollision(t *testing.T) {
	small := []byte("a small member")

	t.Run("oversized entry last", func(t *testing.T) {
		data := buildZipSized683(t, []sizedMember683{
			{name: "./notes.txt", body: small},
			{name: "notes.txt", body: oversizePayload683()},
		})
		st := runArchiveIngest(t, "docs.zip", data)
		assertSizeCapSkip683(t, st, "docs.zip/notes.txt")
	})

	t.Run("under-cap entry last", func(t *testing.T) {
		data := buildZipSized683(t, []sizedMember683{
			{name: "./notes.txt", body: oversizePayload683()},
			{name: "notes.txt", body: small},
		})
		st := runArchiveIngest(t, "docs.zip", data)
		doc := documentByPath(t, st, "docs.zip/notes.txt")
		if doc.Status != "ok" {
			t.Errorf("status = %q, want \"ok\" (the last entry is under the cap and IS indexed)", doc.Status)
		}
	})
}

// TestArchiveOversizeMember683_ExclusionsShareTheMemberBudget pins that naming
// an exclusion draws on the same member-count budget as ingesting a member.
// An exclusion costs memory that a hostile archive controls, so it cannot have
// its own unbounded allowance. Once the budget is spent, extraction stops and
// reports the #408 truncation, rather than dropping the remaining exclusions in
// silence.
func TestArchiveOversizeMember683_ExclusionsShareTheMemberBudget(t *testing.T) {
	data := buildZipSized683(t, []sizedMember683{
		{name: "huge-a.txt", body: oversizePayload683()},
		{name: "huge-b.txt", body: oversizePayload683()},
		{name: "small.txt", body: []byte("a small member")},
	})
	st, logs := runArchiveIngestCapped(t, "docs.zip", data, 1, 0)

	paths := docPaths(t, st)
	if !paths["docs.zip/huge-a.txt"] {
		t.Error("the first exclusion fits the budget and must be recorded")
	}
	if paths["docs.zip/huge-b.txt"] || paths["docs.zip/small.txt"] {
		t.Errorf("extraction must stop once the budget is spent; got %v", paths)
	}
	if !strings.Contains(logs, "#408") {
		t.Errorf("want the truncation warning so the stop is visible; got logs:\n%s", logs)
	}
}

// failingSkipRowStore683 fails the upsert of one rel_path and delegates
// everything else. It models the store rejecting exactly the size_cap row.
type failingSkipRowStore683 struct {
	*memoryStore
	failPath string
}

func (s *failingSkipRowStore683) UpsertDocument(ctx context.Context, doc model.Document) error {
	if doc.RelPath == s.failPath {
		return errors.New("simulated store failure")
	}
	return s.memoryStore.UpsertDocument(ctx, doc)
}

// TestArchiveOversizeMember683_UnpersistedSkipRowBlocksFinalize pins that the
// container is finalized on the RECORD, not on the attempt. If the size_cap row
// does not land, the omission has no durable trace, so the container must keep
// an empty content_hash and re-extract on the next scan.
func TestArchiveOversizeMember683_UnpersistedSkipRowBlocksFinalize(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	data := buildZipSized683(t, []sizedMember683{
		{name: "small.txt", body: []byte("a small member")},
		{name: "huge.txt", body: oversizePayload683()},
	})
	if err := os.WriteFile(filepath.Join(root, "docs.zip"), data, 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	st := &failingSkipRowStore683{memoryStore: newMemoryStore(), failPath: "docs.zip/huge.txt"}
	cfg := config.Default()
	cfg.RootDir = root
	svc := mustNewIngestService(t, cfg, st)
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, wrote := st.docs["docs.zip/huge.txt"]; wrote {
		t.Fatal("the fixture did not block the size_cap row; the test proves nothing")
	}
	if hash := st.docs["docs.zip"].ContentHash; hash != "" {
		t.Errorf("container content_hash = %q, want empty (the omission was recorded nowhere, so the archive is not done)", hash)
	}
}

// corruptStoredZip658 builds a zip whose single member is stored uncompressed
// and then flips one payload byte. The stored bytes appear literally in the
// archive, so the corruption is deterministic: the reader's CRC check fails when
// the member is read.
func corruptStoredZip658(t *testing.T) []byte {
	t.Helper()
	body := []byte("MEMBER-PAYLOAD-MARKER-0123456789")
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	fw, err := w.CreateHeader(&zip.FileHeader{Name: "corrupt.txt", Method: zip.Store})
	if err != nil {
		t.Fatalf("zip create header: %v", err)
	}
	if _, err := fw.Write(body); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	data := buf.Bytes()
	at := bytes.Index(data, body)
	if at < 0 {
		t.Fatal("stored payload not found in the zip; the test fixture is wrong")
	}
	data[at] ^= 0xFF
	return data
}

// TestArchiveUnreadableMember658_ZipMarksContainerError is the #658 regression
// guard. A member whose bytes do not read was skipped in silence and the
// container was still stamped done, so the missing content never came back.
func TestArchiveUnreadableMember658_ZipMarksContainerError(t *testing.T) {
	st := runArchiveIngest(t, "broken.zip", corruptStoredZip658(t))

	container := documentByPath(t, st, "broken.zip")
	if container.Status != "error" {
		t.Fatalf("container status = %q, want \"error\" (an unreadable member must not look healthy)", container.Status)
	}
	if container.ErrorMessage == "" {
		t.Error("container error_message is empty; the failure must carry a durable diagnostic")
	}
	if container.ContentHash != "" {
		t.Error("container content_hash was stamped; the next incremental scan will never retry the unread member")
	}
}

// TestArchiveUnreadableMember658_TruncatedTarMarksContainerError covers the tar
// path, where a corrupt entry header ended the read with a bare break and a nil
// error. Members read before the break must survive; the container must not.
func TestArchiveUnreadableMember658_TruncatedTarMarksContainerError(t *testing.T) {
	full := buildTarGzSized683(t, []sizedMember683{
		{name: "good.txt", body: []byte("this member reads fine")},
		{name: "later.txt", body: payload683(4096)},
	})
	// Cut the gzip stream short so the tar reader hits a truncated entry.
	truncated := full[:len(full)-64]
	st := runArchiveIngest(t, "broken.tar.gz", truncated)

	if !docPaths(t, st)["broken.tar.gz/good.txt"] {
		t.Error("the member read before the break must still be indexed (best-effort ingestion is preserved)")
	}
	container := documentByPath(t, st, "broken.tar.gz")
	if container.Status != "error" {
		t.Fatalf("container status = %q, want \"error\" (a truncated tar is an incomplete read)", container.Status)
	}
	if container.ContentHash != "" {
		t.Error("container content_hash was stamped; a truncated archive must be retried")
	}
}

// TestArchiveUnreadableMember658_UnopenableArchiveMarksError covers the
// empty-corrupt case: the container cannot be opened at all, so it yields no
// members. It used to be indistinguishable from an archive that held nothing.
func TestArchiveUnreadableMember658_UnopenableArchiveMarksError(t *testing.T) {
	st := runArchiveIngest(t, "garbage.zip", []byte("this is not a zip file at all"))

	container := documentByPath(t, st, "garbage.zip")
	if container.Status != "error" {
		t.Fatalf("container status = %q, want \"error\" (an unopenable archive must not pass as empty)", container.Status)
	}
	if container.ErrorMessage == "" {
		t.Error("container error_message is empty; the failure must carry a durable diagnostic")
	}
	if container.ContentHash != "" {
		t.Error("container content_hash was stamped; an unopenable archive must be retried")
	}
}
