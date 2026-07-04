package tests

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/ingest/docling"
)

// approxCharsPerToken mirrors the ingest package's token->rune conversion factor
// used by ConfigureChunking. Kept in sync with internal/ingest/represent.go.
const approxCharsPerToken = 4

// TestConfigureChunking_MaxTokensCapsChunkSize pins the remaining half of #405:
// chunking.max_tokens / chunking.overlap_tokens must actually reach the chunker
// (they were silently ignored — the sizes were hardcoded). A smaller configured
// window must produce smaller, more numerous chunks, every one within the
// configured cap, while an unset window reproduces the historical default sizes.
func TestConfigureChunking_MaxTokensCapsChunkSize(t *testing.T) {
	// The chunker reads process-level effective sizes; restore the defaults so
	// this test does not perturb any sibling test sharing the same binary.
	defer ingest.ConfigureChunking(0, 0)

	long := strings.Repeat("alpha beta gamma delta ", 3000) // ~69k runes
	block := docling.Block{Label: docling.LabelParagraph, Text: long, Page: 1}

	// Unset window (chunking.max_tokens = 0): historical default sizing.
	ingest.ConfigureChunking(0, 0)
	defaultSegs := ingest.ChunkStructuredBlocks([]docling.Block{block})
	if len(defaultSegs) == 0 {
		t.Fatal("default chunking produced no segments")
	}

	// A small explicit window: 100 tokens ~= 400 runes.
	const maxTokens = 100
	const overlapTokens = 20
	ingest.ConfigureChunking(maxTokens, overlapTokens)
	smallSegs := ingest.ChunkStructuredBlocks([]docling.Block{block})
	if len(smallSegs) == 0 {
		t.Fatal("configured chunking produced no segments")
	}

	capRunes := maxTokens * approxCharsPerToken
	for i, s := range smallSegs {
		if n := utf8.RuneCountInString(s.Text); n > capRunes {
			t.Errorf("chunk %d has %d runes, exceeds configured cap %d runes", i, n, capRunes)
		}
	}

	// The smaller window must fragment the same text into more chunks than the
	// default — proof the config took effect rather than being ignored.
	if len(smallSegs) <= len(defaultSegs) {
		t.Errorf("configured max_tokens=%d did not shrink chunks: small=%d default=%d",
			maxTokens, len(smallSegs), len(defaultSegs))
	}

	// Sanity: with no content lost, resetting to defaults restores the default
	// sizing (idempotent), so a re-run matches the first default run.
	ingest.ConfigureChunking(0, 0)
	if got := len(ingest.ChunkStructuredBlocks([]docling.Block{block})); got != len(defaultSegs) {
		t.Errorf("resetting chunking to defaults not idempotent: %d != %d", got, len(defaultSegs))
	}
}

// TestConfigureChunking_ClampsOverlapForRawText pins that an overlap >= the
// window (e.g. from a direct ConfigureChunking call that bypasses Validate) is
// clamped, so the raw-text path can't stall: chunking must terminate and emit
// bounded chunks. Regression for #405/#532 review.
func TestConfigureChunking_ClampsOverlapForRawText(t *testing.T) {
	defer ingest.ConfigureChunking(0, 0)
	// overlap (1000 tokens) far exceeds the window (10 tokens); without the clamp
	// chunkTextByChars would never advance.
	ingest.ConfigureChunking(10, 1000)
	long := strings.Repeat("alpha beta gamma delta ", 500)
	segs := ingest.ChunkRawText("text", long)
	if len(segs) == 0 {
		t.Fatal("clamped raw-text chunking produced no segments")
	}
	capRunes := 10 * approxCharsPerToken
	for i, s := range segs {
		if n := utf8.RuneCountInString(s.Text); n > capRunes {
			t.Errorf("chunk %d has %d runes, exceeds cap %d", i, n, capRunes)
		}
	}
}
