package tests

import (
	"bytes"
	"compress/gzip"
	"context"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
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
func TestNewService_RepresentationStoreSeam_WarnsLoudly(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

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
