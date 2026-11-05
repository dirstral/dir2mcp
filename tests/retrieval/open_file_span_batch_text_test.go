package tests

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// spanBatchTextStore adds the batch capability (ChunkTextsByIDs) to the
// per-chunk store, so a span read over many chunks can be resolved in one
// round trip (PR #1130 review). It records every batch and can be told to fail.
type spanBatchTextStore struct {
	*spanTextStore
	batches [][]uint64
	fail    error
}

func (s *spanBatchTextStore) ChunkTextsByIDs(_ context.Context, ids []uint64) (map[uint64]string, error) {
	s.batches = append(s.batches, append([]uint64(nil), ids...))
	if s.fail != nil {
		return nil, s.fail
	}
	out := make(map[uint64]string, len(ids))
	for _, id := range ids {
		if text, ok := s.text[id]; ok {
			out[id] = text
		}
	}
	return out, nil
}

const spanChunks = 60

// seedSpanChunks registers spanChunks consecutive 10 s transcript chunks of one
// film, each with a full text past the snippet boundary and an END-n marker, and
// returns the span that covers all of them. Chunk 7 holds whitespace only, so
// its snippet must come back instead.
func seedSpanChunks(t *testing.T, svc *retrieval.Service, store *spanBatchTextStore) model.Span {
	t.Helper()
	for i := 1; i <= spanChunks; i++ {
		id := uint64(i)
		text := fmt.Sprintf("chunk-%02d ", i) + strings.Repeat("word ", 70) + fmt.Sprintf("END-%02d", i)
		if i == 7 {
			text = "   \n"
		}
		store.text[id] = text
		snippet := snippetOf(text)
		if i == 7 {
			snippet = "snippet-07"
		}
		svc.SetChunkMetadata(id, model.SearchHit{
			ChunkID: id, RelPath: "film.mp4", DocType: "mp4", Snippet: snippet,
			Span: model.Span{Kind: "time", StartMS: (i - 1) * 10000, EndMS: i * 10000},
		})
	}
	return model.Span{Kind: "time", StartMS: 0, EndMS: spanChunks * 10000}
}

// TestOpenFile_TimeSpan_BatchResolvesEveryChunkInOrder: with the batch
// capability a broad span costs one store call, not one per chunk, and the text
// comes back in reading order with the whitespace-only chunk on its snippet.
func TestOpenFile_TimeSpan_BatchResolvesEveryChunkInOrder(t *testing.T) {
	store := &spanBatchTextStore{spanTextStore: &spanTextStore{text: map[uint64]string{}}}
	svc := newSpanService(t, store)
	span := seedSpanChunks(t, svc, store)

	out, truncated, err := svc.OpenFileWithMeta(context.Background(), "film.mp4", span, 1<<20)
	if err != nil {
		t.Fatalf("OpenFile time span: %v", err)
	}
	if truncated {
		t.Fatal("truncated=true under a 1 MiB max_chars")
	}
	if len(store.batches) != 1 || len(store.batches[0]) != spanChunks {
		t.Fatalf("want one batch of %d ids, got %d batches %v", spanChunks, len(store.batches), store.batches)
	}
	if len(store.asked) != 0 {
		t.Fatalf("per-chunk lookups ran beside the batch: %v", store.asked)
	}
	last := -1
	for i := 1; i <= spanChunks; i++ {
		if i == 7 {
			continue
		}
		pos := strings.Index(out, fmt.Sprintf("END-%02d", i))
		if pos < 0 {
			t.Fatalf("chunk %d text missing from the span read", i)
		}
		if pos < last {
			t.Fatalf("chunk %d out of reading order", i)
		}
		last = pos
	}
	if !strings.Contains(out, "snippet-07") {
		t.Fatal("the whitespace-only chunk did not fall back to its snippet")
	}
}

// TestOpenFile_TimeSpan_BatchFailureFallsBackPerChunk: a failed batch is not a
// failed read; the span resolves chunk by chunk and still returns the full text.
func TestOpenFile_TimeSpan_BatchFailureFallsBackPerChunk(t *testing.T) {
	store := &spanBatchTextStore{spanTextStore: &spanTextStore{text: map[uint64]string{}}, fail: errors.New("store down")}
	svc := newSpanService(t, store)
	span := seedSpanChunks(t, svc, store)

	out, _, err := svc.OpenFileWithMeta(context.Background(), "film.mp4", span, 1<<20)
	if err != nil {
		t.Fatalf("OpenFile time span: %v", err)
	}
	if len(store.batches) != 1 {
		t.Fatalf("want one failed batch attempt, got %d", len(store.batches))
	}
	if len(store.asked) != spanChunks {
		t.Fatalf("want %d per-chunk lookups after the batch failed, got %d", spanChunks, len(store.asked))
	}
	if !strings.Contains(out, "END-01") || !strings.Contains(out, fmt.Sprintf("END-%02d", spanChunks)) {
		t.Fatalf("fallback lost chunk text: %q", out[:120])
	}
}

// TestOpenFile_TimeSpan_SingleChunkSkipsTheBatch: one matching chunk goes
// through the per-chunk lookup; a batch of one buys nothing.
func TestOpenFile_TimeSpan_SingleChunkSkipsTheBatch(t *testing.T) {
	store := &spanBatchTextStore{spanTextStore: &spanTextStore{text: map[uint64]string{7: fullFirstGoalChunk}}}
	svc := newSpanService(t, store)
	svc.SetChunkMetadata(7, model.SearchHit{
		ChunkID: 7, RelPath: "match.mp4", DocType: "mp4", Snippet: snippetOf(fullFirstGoalChunk),
		Span: model.Span{Kind: "time", StartMS: 1198000, EndMS: 1232000},
	})
	out, err := svc.OpenFile(context.Background(), "match.mp4", model.Span{Kind: "time", StartMS: 1198000, EndMS: 1232000}, 3000)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if !strings.Contains(out, hudLine) {
		t.Fatalf("single chunk lost its full text: %q", out)
	}
	if len(store.batches) != 0 || len(store.asked) != 1 {
		t.Fatalf("want no batch and one lookup, got batches=%v asked=%v", store.batches, store.asked)
	}
}
