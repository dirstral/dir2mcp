package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TestProcessDocument_WithholdsContentHashUntilRepsCommit pins the #402 A1 fix:
// the content_hash "done" marker is NOT written on the initial document upsert;
// it is stamped only after the representations/chunks are committed. This is what
// makes an ungraceful crash mid-ingest recoverable — the incremental gate keys
// off content_hash, so a document that never reached the finalize step is
// reprocessed on restart instead of being silently skipped with zero chunks.
func TestProcessDocument_WithholdsContentHashUntilRepsCommit(t *testing.T) {
	root := t.TempDir()
	absPath := filepath.Join(root, "note.txt")
	content := "hello world, this is indexable text"
	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	wantHash := ingest.ComputeContentHash([]byte(content))

	st := &fakeIncrementalStore{reflectUpserts: true} // new document, needs processing
	svc := mustNewIngestService(t, config.Config{RootDir: root}, st)
	df := ingest.DiscoveredFile{AbsPath: absPath, RelPath: "note.txt", SizeBytes: int64(len(content))}

	if err := svc.ProcessDocument(context.Background(), df, nil, false); err != nil {
		t.Fatalf("processDocument failed: %v", err)
	}

	if len(st.upsertedDocs) < 2 {
		t.Fatalf("expected at least two document upserts (withhold + finalize), got %d", len(st.upsertedDocs))
	}
	// First upsert must NOT carry the done marker.
	if st.upsertedDocs[0].ContentHash != "" {
		t.Fatalf("initial upsert leaked the content_hash done marker: %q", st.upsertedDocs[0].ContentHash)
	}
	if st.upsertedDocs[0].Status != "ok" {
		t.Fatalf("initial upsert status = %q, want ok", st.upsertedDocs[0].Status)
	}
	// Reps/chunks must have been committed before the finalize upsert.
	if st.insertChunkCalls == 0 {
		t.Fatalf("expected chunks to be committed")
	}
	// Final upsert stamps the real hash.
	last := st.upsertedDocs[len(st.upsertedDocs)-1]
	if last.ContentHash != wantHash {
		t.Fatalf("finalize upsert content_hash = %q, want %q", last.ContentHash, wantHash)
	}
}

// TestProcessDocument_CrashDuringRepsNeverMarksDone pins that a failure while
// committing representations (a stand-in for a SIGKILL/OOM in that window) never
// leaves the content_hash done marker set. The document must end up
// reprocessable (empty hash, error status), not falsely "indexed" (#402 A1).
func TestProcessDocument_CrashDuringRepsNeverMarksDone(t *testing.T) {
	root := t.TempDir()
	absPath := filepath.Join(root, "note.txt")
	content := "content that would produce at least one chunk"
	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	wantHash := ingest.ComputeContentHash([]byte(content))

	st := &fakeIncrementalStore{insertChunkErr: errors.New("simulated crash committing chunks")}
	svc := mustNewIngestService(t, config.Config{RootDir: root}, st)
	df := ingest.DiscoveredFile{AbsPath: absPath, RelPath: "note.txt", SizeBytes: int64(len(content))}

	if err := svc.ProcessDocument(context.Background(), df, nil, false); err == nil {
		t.Fatalf("expected processDocument to fail when chunk commit fails")
	}

	// The done marker must never have been written for this document.
	for i, d := range st.upsertedDocs {
		if d.ContentHash == wantHash {
			t.Fatalf("upsert #%d wrote the content_hash done marker despite the crash: %q", i, d.ContentHash)
		}
	}
	// The persisted terminal state must force reprocessing on the next scan.
	last := st.upsertedDocs[len(st.upsertedDocs)-1]
	if last.ContentHash != "" {
		t.Fatalf("terminal content_hash = %q, want empty so the doc is reprocessed", last.ContentHash)
	}
	if last.Status != "error" {
		t.Fatalf("terminal status = %q, want error", last.Status)
	}
}

// TestProcessDocument_FinalizePreservesOutOfBandTitle pins the finalizeContentHash
// regression (#402 follow-up): the title is written out-of-band to the DB row by
// persistTitleIfFound DURING representation generation, invisible to the by-value
// doc that finalizeContentHash carries. When finalize stamps the withheld
// content_hash it must upsert the re-read row (title intact), not the by-value doc
// — otherwise the freshly-written title is reverted. This asserts the terminal row
// carries BOTH the extracted title AND the finalized content_hash.
func TestProcessDocument_FinalizePreservesOutOfBandTitle(t *testing.T) {
	root := t.TempDir()
	absPath := filepath.Join(root, "note.txt")
	// A markdown H1 makes ExtractTitle yield a title, so persistTitleIfFound writes
	// documents.title out-of-band during representation generation.
	content := "# Regression Title\n\nsome indexable body text goes here"
	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	wantHash := ingest.ComputeContentHash([]byte(content))
	const wantTitle = "Regression Title"

	// reflectUpserts makes GetDocumentByPath return the most recent upsert, so the
	// out-of-band title write is visible to the finalize read-back — exactly the
	// real store's behavior.
	st := &fakeIncrementalStore{reflectUpserts: true}
	svc := mustNewIngestService(t, config.Config{RootDir: root}, st)
	df := ingest.DiscoveredFile{AbsPath: absPath, RelPath: "note.txt", SizeBytes: int64(len(content))}

	if err := svc.ProcessDocument(context.Background(), df, nil, false); err != nil {
		t.Fatalf("processDocument failed: %v", err)
	}

	// Sanity: the title was in fact written out-of-band before finalize ran.
	sawTitle := false
	for _, d := range st.upsertedDocs {
		if d.Title == wantTitle {
			sawTitle = true
			break
		}
	}
	if !sawTitle {
		t.Fatalf("expected persistTitleIfFound to write title %q out-of-band; upserts=%d", wantTitle, len(st.upsertedDocs))
	}

	// The terminal row must carry BOTH the out-of-band title AND the finalized hash.
	last := st.upsertedDocs[len(st.upsertedDocs)-1]
	if last.ContentHash != wantHash {
		t.Fatalf("finalize upsert content_hash = %q, want %q", last.ContentHash, wantHash)
	}
	if last.Title != wantTitle {
		t.Fatalf("finalize reverted out-of-band title: got %q, want %q", last.Title, wantTitle)
	}
}

// TestProcessDocument_ArchiveWithholdsContentHashUntilMembersExtracted pins the
// #502 fix: an archive container's content_hash "done" marker is NOT written on
// the initial upsert; it is stamped only AFTER processArchiveMembers has committed
// every member. content_hash is the incremental gate for whether member extraction
// re-runs, and archive containers persist as status="skipped" — so an ungraceful
// crash between the initial upsert and completed extraction must leave the hash
// blank and force re-extraction, not skip on a premature marker (the archive-path
// analogue of the #402/#485 representation-commit crash window).
func TestProcessDocument_ArchiveWithholdsContentHashUntilMembersExtracted(t *testing.T) {
	root := t.TempDir()
	archiveData := buildZip(t, map[string]string{"notes.txt": "hello from zip archive"})
	if err := os.WriteFile(filepath.Join(root, "docs.zip"), archiveData, 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	wantHash := ingest.ComputeContentHash(archiveData)

	// reflectByPath so the container's deferred finalize reads back the container
	// row (status "skipped"), not whichever member was upserted last.
	st := &fakeIncrementalStore{reflectByPath: true} // new document, needs extraction
	svc := mustNewIngestService(t, config.Config{RootDir: root}, st)
	df := ingest.DiscoveredFile{RelPath: "docs.zip", SizeBytes: int64(len(archiveData))}

	if err := svc.ProcessDocument(context.Background(), df, nil, false); err != nil {
		t.Fatalf("processDocument failed: %v", err)
	}

	var containerUpserts []model.Document
	lastMemberIdx, containerStampIdx := -1, -1
	for i, d := range st.upsertedDocs {
		switch {
		case d.RelPath == "docs.zip":
			containerUpserts = append(containerUpserts, d)
			if d.ContentHash == wantHash {
				containerStampIdx = i
			}
		case strings.HasPrefix(d.RelPath, "docs.zip/"):
			lastMemberIdx = i
		}
	}

	if lastMemberIdx == -1 {
		t.Fatalf("expected at least one archive member to be upserted; upserts=%v", st.upsertedDocs)
	}
	if len(containerUpserts) < 2 {
		t.Fatalf("expected >=2 container upserts (withhold + finalize), got %d", len(containerUpserts))
	}
	// Initial container upsert must NOT carry the done marker.
	if containerUpserts[0].ContentHash != "" {
		t.Fatalf("initial archive upsert leaked content_hash before members extracted: %q", containerUpserts[0].ContentHash)
	}
	if containerUpserts[0].Status != "skipped" {
		t.Fatalf("initial archive upsert status = %q, want skipped", containerUpserts[0].Status)
	}
	// The done marker must have been stamped, and strictly AFTER the members were
	// committed — this ordering is the crash-safety invariant.
	if containerStampIdx == -1 {
		t.Fatalf("archive content_hash was never finalized; want %q stamped", wantHash)
	}
	if containerStampIdx < lastMemberIdx {
		t.Fatalf("archive content_hash stamped (upsert #%d) BEFORE members committed (last member upsert #%d)", containerStampIdx, lastMemberIdx)
	}
	// Final container upsert carries the real hash and keeps status "skipped".
	last := containerUpserts[len(containerUpserts)-1]
	if last.ContentHash != wantHash {
		t.Fatalf("finalize archive content_hash = %q, want %q", last.ContentHash, wantHash)
	}
	if last.Status != "skipped" {
		t.Fatalf("finalize archive status = %q, want skipped", last.Status)
	}
}

// TestProcessDocument_ArchiveExtractionFailureLeavesContentHashUnstamped pins the
// retry half of the #502 fix: when member extraction does not complete, the
// container's content_hash is never stamped, so the next scan re-extracts rather
// than skipping on a matching-but-premature marker. An unextractable archive
// (.7z — classified as an archive but unsupported by the stdlib extractor) stands
// in for "extraction did not finish".
func TestProcessDocument_ArchiveExtractionFailureLeavesContentHashUnstamped(t *testing.T) {
	root := t.TempDir()
	archiveData := []byte("this is not a real 7z archive; extraction will fail")
	if err := os.WriteFile(filepath.Join(root, "data.7z"), archiveData, 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	wantHash := ingest.ComputeContentHash(archiveData)

	st := &fakeIncrementalStore{reflectByPath: true}
	svc := mustNewIngestService(t, config.Config{RootDir: root}, st)
	df := ingest.DiscoveredFile{RelPath: "data.7z", SizeBytes: int64(len(archiveData))}

	if err := svc.ProcessDocument(context.Background(), df, nil, false); err == nil {
		t.Fatalf("expected processDocument to fail for an unextractable archive")
	}

	// No container upsert may carry the done marker — the members were never
	// extracted, so the marker would falsely skip re-extraction on the next scan.
	var last model.Document
	found := false
	for i, d := range st.upsertedDocs {
		if d.RelPath != "data.7z" {
			continue
		}
		if d.ContentHash == wantHash {
			t.Fatalf("upsert #%d stamped content_hash despite failed extraction: %q", i, d.ContentHash)
		}
		last = d
		found = true
	}
	if !found {
		t.Fatalf("expected the archive container to be upserted")
	}
	// Terminal row: blank hash (reprocessed next scan) + error (#398 diagnostic).
	if last.ContentHash != "" {
		t.Fatalf("terminal archive content_hash = %q, want empty so it is reprocessed", last.ContentHash)
	}
	if last.Status != "error" {
		t.Fatalf("terminal archive status = %q, want error", last.Status)
	}
}

// TestProcessDocument_ArchiveMemberFailureLeavesContentHashUnstamped pins the
// member-granularity half of the #502 fix: when a single archive member fails to
// ingest (a stand-in for a crash mid-member), that member is logged and skipped
// per #398 best-effort, but the CONTAINER's content_hash is NOT stamped — so the
// next incremental scan re-extracts and retries the failed member instead of
// permanently skipping it on a premature done marker. An all-success archive (see
// TestProcessDocument_ArchiveWithholdsContentHashUntilMembersExtracted) DOES stamp
// the marker; this asserts the failing-member counterpart withholds it.
func TestProcessDocument_ArchiveMemberFailureLeavesContentHashUnstamped(t *testing.T) {
	root := t.TempDir()
	archiveData := buildZip(t, map[string]string{"notes.txt": "hello from a zip member whose commit will fail"})
	if err := os.WriteFile(filepath.Join(root, "docs.zip"), archiveData, 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	wantHash := ingest.ComputeContentHash(archiveData)

	// insertChunkErr makes the member's representation commit fail (a stand-in for a
	// per-member crash/outage). The member is logged+skipped (#398 best-effort), so
	// processDocument still succeeds, but the container must NOT be finalized.
	st := &fakeIncrementalStore{reflectByPath: true, insertChunkErr: errors.New("simulated member commit crash")}
	svc := mustNewIngestService(t, config.Config{RootDir: root}, st)
	df := ingest.DiscoveredFile{RelPath: "docs.zip", SizeBytes: int64(len(archiveData))}

	// Best-effort: a member failure is swallowed, so the run itself succeeds.
	if err := svc.ProcessDocument(context.Background(), df, nil, false); err != nil {
		t.Fatalf("processDocument failed: %v", err)
	}

	// Sanity: the failing member WAS attempted (so this is really the member-failure
	// path, not an archive that produced no members).
	if st.insertChunkCalls == 0 {
		t.Fatalf("expected the member's representation commit to be attempted")
	}

	var last model.Document
	found := false
	for i, d := range st.upsertedDocs {
		if d.RelPath != "docs.zip" {
			continue
		}
		if d.ContentHash == wantHash {
			t.Fatalf("upsert #%d stamped the container content_hash despite a failed member: %q", i, d.ContentHash)
		}
		last = d
		found = true
	}
	if !found {
		t.Fatalf("expected the archive container to be upserted")
	}
	// Terminal container row: blank hash so the next scan re-extracts and retries
	// the failed member; status stays "skipped" (archive containers carry no text).
	if last.ContentHash != "" {
		t.Fatalf("terminal container content_hash = %q, want empty so the failed member is retried next scan", last.ContentHash)
	}
	if last.Status != "skipped" {
		t.Fatalf("terminal container status = %q, want skipped", last.Status)
	}
}

// TestNeedsReprocessing_EmptyHashResumesAfterInterruption is the restart half of
// the #402 A1 story: a document left with an empty content_hash by an interrupted
// run (the withheld-marker state) is always reprocessed on the next scan.
func TestNeedsReprocessing_EmptyHashResumesAfterInterruption(t *testing.T) {
	if !ingest.NeedsReprocessing("", "any-new-hash", false) {
		t.Fatalf("a document with an empty (withheld) content_hash must be reprocessed on restart")
	}
	if ingest.NeedsReprocessing("h", "h", false) {
		t.Fatalf("an unchanged, fully-indexed document must not be reprocessed")
	}
}
