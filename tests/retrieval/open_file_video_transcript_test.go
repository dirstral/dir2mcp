package tests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// videoDocStore is a minimal model.Store that reports a stored doc_type per
// path and a transcript-only modality (text chunks, no media chunks), so
// open_file does not take the MEDIA_NO_TEXT guard.
type videoDocStore struct {
	docTypes map[string]string
}

func (f *videoDocStore) Init(context.Context) error { return nil }
func (f *videoDocStore) UpsertDocument(context.Context, model.Document) error {
	return nil
}
func (f *videoDocStore) GetDocumentByPath(_ context.Context, relPath string) (model.Document, error) {
	docType, ok := f.docTypes[relPath]
	if !ok {
		return model.Document{}, model.ErrNotFound
	}
	return model.Document{RelPath: relPath, DocType: docType, Status: "ok"}, nil
}
func (f *videoDocStore) ListFiles(context.Context, string, string, int, int) ([]model.Document, int64, error) {
	return nil, 0, nil
}
func (f *videoDocStore) Close() error { return nil }
func (f *videoDocStore) ChunkModalityPresence(context.Context, string) (bool, bool, error) {
	return false, true, nil
}

// rawVideoBytes looks like a container: it holds NUL bytes and a marker that
// must never reach an open_file answer.
var rawVideoBytes = []byte("\x1aE\xdf\xa3\x00\x00RAWCONTAINER\x00\x01\x02")

// writeVideoWithTranscript writes a video at rel and, when transcript is not
// empty, seeds the transcript cache keyed by sha256 of its bytes (no STT
// identity is configured, so ingest writes the bytes-only key).
func writeVideoWithTranscript(t *testing.T, root, stateDir, rel, transcript string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, rawVideoBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if transcript == "" {
		return
	}
	dir := filepath.Join(stateDir, "cache", "transcribe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(rawVideoBytes)
	if err := os.WriteFile(filepath.Join(dir, hex.EncodeToString(sum[:])+".txt"), []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newVideoOpenService(root, stateDir string, docTypes map[string]string) *retrieval.Service {
	svc := retrieval.NewService(&videoDocStore{docTypes: docTypes}, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(stateDir)
	return svc
}

// TestOpenFile_TranscriptVideo_NeverServesContainerBytes pins SPEC §15.4 for
// a video with transcript chunks and no direct media chunks (any container
// outside MP4/MOV, or MP4/MOV with multimodal embedding off). open_file serves
// the transcript for a default or line span, and a span the transcript cannot
// answer is DOC_TYPE_UNSUPPORTED. It never returns the container bytes.
func TestOpenFile_TranscriptVideo_NeverServesContainerBytes(t *testing.T) {
	const transcript = "[00:00] archive footage\n[00:03] second line"
	for _, rel := range []string{"clip.mkv", "clip.mp4", "tape.mpg", "stream/news.ts"} {
		t.Run(rel, func(t *testing.T) {
			root := testutil.TempDir(t)
			stateDir := filepath.Join(root, ".dir2mcp")
			writeVideoWithTranscript(t, root, stateDir, rel, transcript)
			svc := newVideoOpenService(root, stateDir, map[string]string{rel: "video"})

			for _, span := range []model.Span{{}, {Kind: "lines", StartLine: 1, EndLine: 1}} {
				out, err := svc.OpenFile(context.Background(), rel, span, 20000)
				if err != nil {
					t.Fatalf("span %+v: err %v", span, err)
				}
				if !strings.Contains(out, "archive footage") || strings.Contains(out, "RAWCONTAINER") {
					t.Fatalf("span %+v: want the transcript, got %q", span, out)
				}
			}
			for _, span := range []model.Span{{Kind: "time", StartMS: 1000, EndMS: 2000}, {Kind: "page", Page: 1}} {
				out, err := svc.OpenFile(context.Background(), rel, span, 20000)
				if !errors.Is(err, model.ErrDocTypeUnsupported) {
					t.Fatalf("span %+v with no matching chunk: want ErrDocTypeUnsupported, got err=%v out=%q", span, err, out)
				}
			}
		})
	}
}

// TestOpenFile_VideoWithoutTranscriptCache_IsNotReady checks that a video
// whose transcript is not cached yet answers the retryable OCR_NOT_READY, not
// the container bytes.
func TestOpenFile_VideoWithoutTranscriptCache_IsNotReady(t *testing.T) {
	root := testutil.TempDir(t)
	stateDir := filepath.Join(root, ".dir2mcp")
	writeVideoWithTranscript(t, root, stateDir, "clip.mkv", "")
	svc := newVideoOpenService(root, stateDir, map[string]string{"clip.mkv": "video"})

	out, err := svc.OpenFile(context.Background(), "clip.mkv", model.Span{}, 20000)
	if !errors.Is(err, model.ErrOCRNotReady) {
		t.Fatalf("want ErrOCRNotReady, got err=%v out=%q", err, out)
	}
}

// TestOpenFile_TypeScriptTs_StaysRawText checks that the store lookup for a
// content-sniffed extension keeps a TypeScript .ts on its raw-text read: a row
// stored as code, or no row at all, serves the source.
func TestOpenFile_TypeScriptTs_StaysRawText(t *testing.T) {
	const src = "export const x: number = 1\n"
	for name, docTypes := range map[string]map[string]string{
		"stored as code": {"app.ts": "code"},
		"no row":         {},
	} {
		t.Run(name, func(t *testing.T) {
			root := testutil.TempDir(t)
			if err := os.WriteFile(filepath.Join(root, "app.ts"), []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
			svc := newVideoOpenService(root, filepath.Join(root, ".dir2mcp"), docTypes)
			out, err := svc.OpenFile(context.Background(), "app.ts", model.Span{}, 20000)
			if err != nil {
				t.Fatalf("err %v", err)
			}
			if out != src {
				t.Fatalf("want the TypeScript source, got %q", out)
			}
		})
	}
}
