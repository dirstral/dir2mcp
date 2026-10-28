package tests

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// spanTextStore is a minimal model.Store with the chunk-text capability
// (ChunkTaskByID) that open_file span reads resolve full text through
// (issue #1129). It records which ids were asked for.
type spanTextStore struct {
	text  map[uint64]string
	asked []uint64
}

func (s *spanTextStore) ChunkTaskByID(_ context.Context, chunkID uint64) (model.ChunkTask, string, error) {
	s.asked = append(s.asked, chunkID)
	full, ok := s.text[chunkID]
	if !ok {
		return model.ChunkTask{}, "", model.ErrNotFound
	}
	return model.ChunkTask{Label: chunkID, Text: full, Metadata: model.ChunkMetadata{ChunkID: chunkID}}, "", nil
}

func (s *spanTextStore) Init(context.Context) error                           { return nil }
func (s *spanTextStore) UpsertDocument(context.Context, model.Document) error { return nil }
func (s *spanTextStore) GetDocumentByPath(context.Context, string) (model.Document, error) {
	return model.Document{}, model.ErrNotFound
}
func (s *spanTextStore) ListFiles(context.Context, string, string, int, int) ([]model.Document, int64, error) {
	return nil, 0, nil
}
func (s *spanTextStore) Close() error { return nil }

const (
	// The transcript chunk as the store holds it: the HUD line sits past the
	// 240-rune snippet boundary, which is where the bug hid it.
	fullFirstGoalChunk = "[00:19:58] Thank you. [Scene 00:20:00] Players engaged in soccer match (Outdoor soccer field with trees and parking lot in background) [00:20:01] Let's go, man. [00:20:02] Come on, ladies, let's get back, boys. [00:20:08] Let's go. [00:20:12] Push up. [HUD 00:20:28] GOAL for Air Force, Air Force's first goal of the match: Air Force 1 Navy 0 (match clock 15:55, 1st half)"
	hudLine            = "[HUD 00:20:28] GOAL for Air Force"
)

func snippetOf(text string) string {
	r := []rune(text)
	if len(r) > 240 {
		r = r[:240]
	}
	return string(r)
}

func newSpanService(t *testing.T, store model.Store) *retrieval.Service {
	t.Helper()
	svc := retrieval.NewService(store, nil, nil, nil)
	root := testutil.TempDir(t)
	svc.SetRootDir(root)
	svc.SetStateDir(filepath.Join(root, ".dir2mcp"))
	return svc
}

// TestOpenFile_TimeSpan_ReturnsFullChunkText pins issue #1129: an open_file
// start_ms/end_ms read must return the chunk TEXT, not the ~240-rune store
// snippet the in-memory hit carries, and truncated must follow max_chars.
func TestOpenFile_TimeSpan_ReturnsFullChunkText(t *testing.T) {
	const relPath = "match.mp4"
	store := &spanTextStore{text: map[uint64]string{7: fullFirstGoalChunk}}
	svc := newSpanService(t, store)
	svc.SetChunkMetadata(7, model.SearchHit{
		ChunkID: 7, RelPath: relPath, DocType: "mp4", Snippet: snippetOf(fullFirstGoalChunk),
		Span: model.Span{Kind: "time", StartMS: 1198000, EndMS: 1232000},
	})

	out, truncated, err := svc.OpenFileWithMeta(context.Background(), relPath, model.Span{Kind: "time", StartMS: 1198000, EndMS: 1232000}, 3000)
	if err != nil {
		t.Fatalf("OpenFile time span: %v", err)
	}
	if !strings.Contains(out, hudLine) {
		t.Fatalf("open_file returned the store snippet, not the chunk text: %q", out)
	}
	if truncated {
		t.Fatalf("truncated=true for %d runes under max_chars=3000", len([]rune(out)))
	}
	if len(store.asked) != 1 || store.asked[0] != 7 {
		t.Fatalf("expected one ChunkTaskByID lookup for chunk 7, got %v", store.asked)
	}
}

// TestOpenFile_TimeSpan_MaxCharsStillTruncates: max_chars keeps its contract
// on the full text, and the flag reports it.
func TestOpenFile_TimeSpan_MaxCharsStillTruncates(t *testing.T) {
	const relPath = "match.mp4"
	store := &spanTextStore{text: map[uint64]string{7: fullFirstGoalChunk}}
	svc := newSpanService(t, store)
	svc.SetChunkMetadata(7, model.SearchHit{
		ChunkID: 7, RelPath: relPath, DocType: "mp4", Snippet: snippetOf(fullFirstGoalChunk),
		Span: model.Span{Kind: "time", StartMS: 1198000, EndMS: 1232000},
	})

	out, truncated, err := svc.OpenFileWithMeta(context.Background(), relPath, model.Span{Kind: "time", StartMS: 1198000, EndMS: 1232000}, 200)
	if err != nil {
		t.Fatalf("OpenFile time span: %v", err)
	}
	if !truncated {
		t.Fatalf("truncated=false for a %d-rune chunk under max_chars=200", len([]rune(fullFirstGoalChunk)))
	}
	if got := len([]rune(out)); got != 200 {
		t.Fatalf("content length %d, want 200", got)
	}
}

// TestOpenFile_TimeSpan_FallsBackToSnippet: without the chunk-text capability,
// or when the lookup fails, the snippet is still served, so a store that cannot
// supply text never turns a span read into an empty result.
func TestOpenFile_TimeSpan_FallsBackToSnippet(t *testing.T) {
	const relPath = "match.mp4"
	span := model.Span{Kind: "time", StartMS: 1198000, EndMS: 1232000}
	hit := model.SearchHit{ChunkID: 7, RelPath: relPath, DocType: "mp4", Snippet: snippetOf(fullFirstGoalChunk), Span: span}

	t.Run("nil store", func(t *testing.T) {
		svc := newSpanService(t, nil)
		svc.SetChunkMetadata(7, hit)
		out, err := svc.OpenFile(context.Background(), relPath, span, 3000)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		if out != snippetOf(fullFirstGoalChunk) {
			t.Fatalf("nil store: want the snippet, got %q", out)
		}
	})
	t.Run("lookup fails", func(t *testing.T) {
		store := &spanTextStore{text: map[uint64]string{}}
		svc := newSpanService(t, store)
		svc.SetChunkMetadata(7, hit)
		out, err := svc.OpenFile(context.Background(), relPath, span, 3000)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		if out != snippetOf(fullFirstGoalChunk) {
			t.Fatalf("failed lookup: want the snippet, got %q", out)
		}
	})
}

// TestOpenFile_PageSpan_ReturnsFullChunkText: the page path shares the slice,
// so a page read of an extracted PDF gets the chunk text too, in page order
// with the full text of every chunk on the page.
func TestOpenFile_PageSpan_ReturnsFullChunkText(t *testing.T) {
	const relPath = "report.pdf"
	long1 := "first-chunk " + strings.Repeat("alpha ", 80) + "END-ONE"
	long2 := "second-chunk " + strings.Repeat("beta ", 80) + "END-TWO"
	store := &spanTextStore{text: map[uint64]string{1: long1, 2: long2}}
	svc := newSpanService(t, store)
	svc.SetChunkMetadata(2, model.SearchHit{ChunkID: 2, RelPath: relPath, DocType: "pdf", Snippet: snippetOf(long2),
		Span: model.Span{Kind: "region", Region: &model.RegionSpan{StartPage: 3, EndPage: 3}}})
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: relPath, DocType: "pdf", Snippet: snippetOf(long1),
		Span: model.Span{Kind: "page", Page: 3}})

	out, err := svc.OpenFile(context.Background(), relPath, model.Span{Kind: "page", Page: 3}, 20000)
	if err != nil {
		t.Fatalf("OpenFile page=3: %v", err)
	}
	i, j := strings.Index(out, "END-ONE"), strings.Index(out, "END-TWO")
	if i < 0 || j < 0 {
		t.Fatalf("page read lost the chunk text past the snippet boundary: %q", out)
	}
	if i > j {
		t.Fatalf("page-native chunk must lead the region chunk: %q", out)
	}
}
