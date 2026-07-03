package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
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
