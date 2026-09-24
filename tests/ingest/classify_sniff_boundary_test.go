package tests

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// sniffSample is the 8 KiB window SniffTextDocType reads.
const sniffSample = 8 << 10

// TestSniffTextDocType_SampleEdge pins the trim at the 8 KiB sample edge. It
// may drop a multi-byte rune that the edge cut in two, and nothing else. The
// old loop trimmed while the sample was invalid, so an invalid byte followed by
// NULs at the edge was trimmed away and a binary file became "text".
func TestSniffTextDocType_SampleEdge(t *testing.T) {
	// ASCII up to byte 8189, then 0xff and three NULs across the edge.
	binary := append(bytes.Repeat([]byte("a"), sniffSample-3), 0xff, 0, 0, 0)
	if got := ingest.SniffTextDocType("binary_ignored", "blob.dat", binary); got != "binary_ignored" {
		t.Errorf("invalid byte + NULs at the edge: got %q, want binary_ignored", got)
	}

	// A lone invalid byte as the last sampled byte, with valid text after it.
	lone := append(bytes.Repeat([]byte("a"), sniffSample-1), 0xff)
	lone = append(lone, []byte("tail")...)
	if got := ingest.SniffTextDocType("binary_ignored", "blob.dat", lone); got != "binary_ignored" {
		t.Errorf("invalid byte at the edge: got %q, want binary_ignored", got)
	}

	// A valid 3-byte rune ("€" = e2 82 ac) cut after its first two bytes.
	cut := append(bytes.Repeat([]byte("a"), sniffSample-2), []byte("€ and more text")...)
	if got := ingest.SniffTextDocType("binary_ignored", "notes", cut); got != "text" {
		t.Errorf("valid rune cut at the edge: got %q, want text", got)
	}

	// A valid 4-byte rune cut after its first byte.
	cut4 := append(bytes.Repeat([]byte("a"), sniffSample-1), []byte("😀 more")...)
	if got := ingest.SniffTextDocType("binary_ignored", "notes", cut4); got != "text" {
		t.Errorf("valid 4-byte rune cut at the edge: got %q, want text", got)
	}
}

// TestRunScan_S3ClassifierUpgradeReReadsObject pins that the S3 ETag fast path
// does not keep a stale classification. app.mjs was stored as binary_ignored
// before .mjs joined the code table; its ETag, size and hash are unchanged, and
// the scan must still re-read it so it is indexed as code. A sniffed-text
// object (.gitignore stored as text) matches its path class and must still take
// the fast path, so the fix does not cost a GET per object on every scan.
func TestRunScan_S3ClassifierUpgradeReReadsObject(t *testing.T) {
	const mjs = "export const answer = 42;\n"
	const ignore = "node_modules/\n*.log\n"
	fs := newFakeRemoteFS()
	fs.add("app.mjs", "etag-mjs", mjs)
	fs.add(".gitignore", "etag-ignore", ignore)

	st := newRemoteScanStore()
	st.seed(model.Document{
		DocID:       1,
		RelPath:     "app.mjs",
		DocType:     "binary_ignored",
		SizeBytes:   int64(len(mjs)),
		ContentHash: ingest.ComputeContentHash([]byte(mjs)),
		ETag:        "etag-mjs",
		Status:      "skipped",
	})
	st.seed(model.Document{
		DocID:       2,
		RelPath:     ".gitignore",
		DocType:     "text",
		SizeBytes:   int64(len(ignore)),
		ContentHash: ingest.ComputeContentHash([]byte(ignore)),
		ETag:        "etag-ignore",
		Status:      "ok",
	})

	svc := mustNewIngestService(t, config.Config{RootDir: "/corpus"}, st)
	svc.SetCorpusFS(fs)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if got := fs.openCount("app.mjs"); got == 0 {
		t.Fatal("app.mjs took the ETag fast path; a stored binary_ignored row for a code extension must be re-read")
	}
	doc, ok := st.get("app.mjs")
	if !ok {
		t.Fatal("app.mjs missing from the store after the scan")
	}
	if doc.DocType != "code" {
		t.Errorf("app.mjs doc_type = %q, want code", doc.DocType)
	}

	if got := fs.openCount(".gitignore"); got != 0 {
		t.Errorf(".gitignore was re-read %d time(s); a sniffed-text row with an unchanged ETag must take the fast path", got)
	}
	if got := st.upsertCalls[".gitignore"]; got != 0 {
		t.Errorf(".gitignore was upserted %d time(s); want 0", got)
	}
	if strings.TrimSpace(doc.ContentHash) == "" && doc.Status == "ok" {
		t.Errorf("app.mjs is ok with an empty content_hash (done marker never stamped)")
	}
}
