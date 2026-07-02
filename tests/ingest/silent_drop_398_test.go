package tests

import (
	"bytes"
	"compress/gzip"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// noRepStore implements model.Store but deliberately NOT
// model.RepresentationStore, reproducing the seam #398 targets: an alternate
// backend that fails the `store.(model.RepresentationStore)` assertion in
// ingest.NewService.
type noRepStore struct{}

func (noRepStore) Init(context.Context) error                           { return nil }
func (noRepStore) UpsertDocument(context.Context, model.Document) error { return nil }
func (noRepStore) GetDocumentByPath(context.Context, string) (model.Document, error) {
	return model.Document{}, nil
}
func (noRepStore) ListFiles(context.Context, string, string, int, int) ([]model.Document, int64, error) {
	return nil, 0, nil
}
func (noRepStore) Close() error { return nil }

// TestNewService_RepresentationStoreSeam_WarnsLoudly is the regression guard for
// #398 item 1: when the store does not satisfy model.RepresentationStore, repGen
// stays nil and representation generation is silently disabled corpus-wide (the
// #364 failure mode one layer up). NewService must now warn loudly on that
// negative branch instead of degrading without a trace.
//
// NewService emits the seam warning through the process-wide default logger
// (log.Default()), so this test redirects that logger's output to a buffer to
// observe it. It therefore mutates global state and MUST NOT be parallelised
// (no t.Parallel()); the original writer is captured and restored via
// t.Cleanup so the logger is never left pointing at an unexpected destination.
func TestNewService_RepresentationStoreSeam_WarnsLoudly(t *testing.T) {
	var buf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	cfg := config.Default()
	cfg.RootDir = t.TempDir()
	// Keep transcriber construction credential-free so the test isolates the
	// RepresentationStore seam, not STT provider setup.
	cfg.STTProvider = "off"
	if _, err := ingest.NewService(cfg, noRepStore{}); err != nil {
		t.Fatalf("NewService: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "does not satisfy model.RepresentationStore") {
		t.Fatalf("expected a loud warning about the missing RepresentationStore capability; got: %q", got)
	}
	if !strings.Contains(got, "DISABLED") {
		t.Fatalf("warning should make clear representation generation is disabled; got: %q", got)
	}
}

// buildGzip returns the bytes of a bare (single-file) gzip stream.
func buildGzip(t *testing.T, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write([]byte(content)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// TestArchiveIngest_BareGzipMemberIndexed is the regression guard for #398
// item 2: a bare .gz (not .tar.gz) classifies as "archive" but was previously
// dropped as an empty skipped document because archiveFormat returned "". It must
// now be decompressed and ingested as a single searchable member.
func TestArchiveIngest_BareGzipMemberIndexed(t *testing.T) {
	data := buildGzip(t, "hello from bare gzip")
	st := runArchiveIngest(t, "notes.txt.gz", data)

	paths := docPaths(t, st)
	if !paths["notes.txt.gz/notes.txt"] {
		t.Errorf("want notes.txt.gz/notes.txt ingested as a decompressed member; got %v", paths)
	}

	member := documentByPath(t, st, "notes.txt.gz/notes.txt")
	if member.Status != "ok" {
		t.Errorf("decompressed gzip member status = %q, want \"ok\"", member.Status)
	}
}

// TestArchiveIngest_EdgeCaseNameNoTraversal is the regression guard for the PR
// #483 path-traversal finding: a bare single-compressed archive whose name
// reduces to a traversal segment once the compression suffix is stripped (e.g.
// "..gz" -> ".") must NOT yield a member rel_path containing a "."/".." traversal
// segment. End-to-end, no such path may ever be persisted; the precise synthetic
// member name is asserted at the extraction layer in the internal package test
// TestExtractSingleCompressedMember_EdgeCaseNameSanitised.
func TestArchiveIngest_EdgeCaseNameNoTraversal(t *testing.T) {
	data := buildGzip(t, "edge case payload")
	st := runArchiveIngest(t, "..gz", data)

	for p := range docPaths(t, st) {
		if strings.HasPrefix(p, "..gz/") && (strings.HasSuffix(p, "/..") || strings.HasSuffix(p, "/.") || strings.Contains(p[len("..gz/"):], "..")) {
			t.Errorf("member rel_path %q contains a traversal segment; edge-case name was not sanitised", p)
		}
	}
}

// TestArchiveIngest_UnsupportedFormatMarkedError is the regression guard for
// #398 item 2: a classified-but-unextractable archive (.7z/.xz/.rar) was silently
// ingested as an empty skipped document with zero diagnostics. It must now be
// recorded as status="error" so the gap is visible and retriable — without
// hard-failing the run.
func TestArchiveIngest_UnsupportedFormatMarkedError(t *testing.T) {
	// Arbitrary bytes: classification is by extension, so the content is never
	// read for an unsupported format.
	st := runArchiveIngest(t, "bundle.7z", []byte("not a real 7z archive"))

	doc := documentByPath(t, st, "bundle.7z")
	if doc.Status != "error" {
		t.Fatalf("unsupported archive status = %q, want \"error\" (a silent empty-skip is the #398 bug)", doc.Status)
	}
	if doc.ErrorMessage == "" {
		t.Fatal("unsupported archive carries no error_message; the diagnostic is empty")
	}
	if !strings.Contains(strings.ToLower(doc.ErrorMessage), "unsupported archive format") {
		t.Errorf("error_message = %q, want it to mention the unsupported archive format", doc.ErrorMessage)
	}
}

// runArchiveIngestSnapshot mirrors runArchiveIngest but attaches an
// IndexingState so the in-memory run counters can be asserted, and returns the
// snapshot taken after the run.
func runArchiveIngestSnapshot(t *testing.T, archiveName string, archiveData []byte) appstate.IndexingSnapshot {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, archiveName), archiveData, 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	cfg := config.Default()
	cfg.RootDir = root
	svc := mustNewIngestService(t, cfg, st)
	state := appstate.NewIndexingState(appstate.ModeIncremental)
	svc.SetIndexingState(state)
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return state.Snapshot()
}

// TestUnsupportedArchive_CountedOnceAsError guards the #398 review fix: an
// unsupported archive builds as status="skipped" (archive containers hold no
// direct text) and is then flipped to status="error" when extraction fails. The
// run counters must record it as exactly one error and zero skips — not
// double-counted as both, which previously inflated CorpusStats totals.
func TestUnsupportedArchive_CountedOnceAsError(t *testing.T) {
	snap := runArchiveIngestSnapshot(t, "bundle.7z", []byte("not a real 7z archive"))
	if snap.Errors != 1 {
		t.Errorf("snapshot.Errors = %d, want 1", snap.Errors)
	}
	if snap.Skipped != 0 {
		t.Errorf("snapshot.Skipped = %d, want 0 (an errored archive must not be counted as skipped too)", snap.Skipped)
	}
}

// TestVideoWithoutSidecar_MarkedError is the regression guard for #398 item 3:
// a default-config video (.mp4) with no subtitle sidecar and multimodal keyframe
// embedding off produces zero representations. That was a silent no-op; it must
// now surface as status="error" so the unsearchable video is durably visible.
func TestVideoWithoutSidecar_MarkedError(t *testing.T) {
	st := runArchiveIngest(t, "clip.mp4", []byte("fake-video-bytes"))

	doc := documentByPath(t, st, "clip.mp4")
	if doc.Status != "error" {
		t.Fatalf("sidecar-less video status = %q, want \"error\" (a zero-representation video must not stay ok)", doc.Status)
	}
	if doc.ErrorMessage == "" {
		t.Fatal("sidecar-less video carries no error_message; the diagnostic is empty")
	}
	if !strings.Contains(strings.ToLower(doc.ErrorMessage), "no representation") {
		t.Errorf("error_message = %q, want it to explain the video produced no representation", doc.ErrorMessage)
	}
}

// TestBinaryParquet_NotIndexedAsText is the regression guard for #398 item 4:
// a .parquet file classifies as "data" and was run through the raw-text path,
// where invalid bytes became U+FFFD soup that was chunked and embedded. The binary
// guard must skip it and record a durable diagnostic instead.
func TestBinaryParquet_NotIndexedAsText(t *testing.T) {
	// Parquet magic + a NUL byte: the same shape looksLikeBinaryContent detects.
	parquet := append([]byte("PAR1"), 0x00, 0x01, 0x02, 0x00, 0xff, 0xfe)
	st := runArchiveIngest(t, "data.parquet", parquet)

	doc := documentByPath(t, st, "data.parquet")
	if doc.Status != "error" {
		t.Fatalf("binary parquet status = %q, want \"error\" (binary must not be indexed as text soup)", doc.Status)
	}
	if doc.ErrorMessage == "" {
		t.Fatal("binary parquet carries no error_message; the diagnostic is empty")
	}
	if !strings.Contains(strings.ToLower(doc.ErrorMessage), "binary content") {
		t.Errorf("error_message = %q, want it to mention binary content on the raw-text path", doc.ErrorMessage)
	}
}

// TestBinaryContent_CountedOnceAsError_NotIndexed is the regression guard for the
// PR #483 double-count finding (thread PRRT_kwDORa91686N7L64, service.go:2049): a
// binary payload on the raw-text path (a .parquet here) persists status="error"
// and increments the error counter, but the diagnostic returned nil, so
// processDocument ALSO credited the document as indexed. That double-counted it as
// both indexed and error, so indexed+skipped+errors exceeded scanned — violating
// the issue #426 invariant. The soft-error must now suppress the indexed credit so
// the document counts solely as an error.
func TestBinaryContent_CountedOnceAsError_NotIndexed(t *testing.T) {
	// Parquet magic + a NUL byte: the same shape looksLikeBinaryContent detects.
	parquet := append([]byte("PAR1"), 0x00, 0x01, 0x02, 0x00, 0xff, 0xfe)
	snap := runArchiveIngestSnapshot(t, "data.parquet", parquet)

	if snap.Errors != 1 {
		t.Errorf("snapshot.Errors = %d, want 1 (a binary-on-raw-text doc is exactly one error)", snap.Errors)
	}
	if snap.Indexed != 0 {
		t.Errorf("snapshot.Indexed = %d, want 0 (a soft-errored doc must not also be credited as indexed)", snap.Indexed)
	}
	if snap.Skipped != 0 {
		t.Errorf("snapshot.Skipped = %d, want 0", snap.Skipped)
	}
	if snap.Indexed+snap.Skipped+snap.Errors > snap.Scanned {
		t.Errorf("indexed(%d)+skipped(%d)+errors(%d) = %d exceeds scanned(%d); the doc was double-counted (issue #426)",
			snap.Indexed, snap.Skipped, snap.Errors, snap.Indexed+snap.Skipped+snap.Errors, snap.Scanned)
	}
}

// TestVideoWithoutSidecar_CountedOnceAsError_NotIndexed is the sibling guard for
// the second PR #483 soft-error path: a sidecar-less, multimodal-off video
// produces zero representations and is persisted status="error" with addErrors(1),
// but the diagnostic returned nil so processDocument also credited it as indexed.
// It must count solely as an error. The same suppression mechanism covers both
// this and the binary-content path.
func TestVideoWithoutSidecar_CountedOnceAsError_NotIndexed(t *testing.T) {
	snap := runArchiveIngestSnapshot(t, "clip.mp4", []byte("fake-video-bytes"))

	if snap.Errors != 1 {
		t.Errorf("snapshot.Errors = %d, want 1 (a zero-representation video is exactly one error)", snap.Errors)
	}
	if snap.Indexed != 0 {
		t.Errorf("snapshot.Indexed = %d, want 0 (a soft-errored video must not also be credited as indexed)", snap.Indexed)
	}
	if snap.Indexed+snap.Skipped+snap.Errors > snap.Scanned {
		t.Errorf("indexed(%d)+skipped(%d)+errors(%d) = %d exceeds scanned(%d); the video was double-counted (issue #426)",
			snap.Indexed, snap.Skipped, snap.Errors, snap.Indexed+snap.Skipped+snap.Errors, snap.Scanned)
	}
}

// TestTextData_StillIndexedOnRawTextPath guards against a false positive from the
// #398 item 4 binary heuristic: sibling "data" extensions that are genuine text
// (.json here) must still be indexed as raw text, not misflagged as binary.
func TestTextData_StillIndexedOnRawTextPath(t *testing.T) {
	st := runArchiveIngest(t, "config.json", []byte(`{"name":"dir2mcp","ok":true}`))

	doc := documentByPath(t, st, "config.json")
	if doc.Status != "ok" {
		t.Fatalf("text JSON status = %q, want \"ok\" (the binary guard must not flag plain text)", doc.Status)
	}
}
