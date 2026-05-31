package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/ingest/docling"
)

func bbox(page int) *docling.BBox {
	return &docling.BBox{Page: page, L: 10, T: 20, R: 30, B: 40, CoordOrigin: "TOPLEFT"}
}

// TestChunkStructuredBlocks_GroupsBySectionWithRegionSpans pins that
// consecutive blocks in the same section are grouped into one chunk carrying a
// region span with that section breadcrumb, and a new section starts a new
// chunk.
func TestChunkStructuredBlocks_GroupsBySectionWithRegionSpans(t *testing.T) {
	blocks := []docling.Block{
		{Label: "section_header", Text: "Results", Page: 1, BBox: bbox(1)},
		{Label: "paragraph", Text: "First point.", Page: 1, BBox: bbox(1), Section: []string{"Results"}},
		{Label: "paragraph", Text: "Second point.", Page: 1, BBox: bbox(1), Section: []string{"Results"}},
		{Label: "section_header", Text: "Outlook", Page: 2, BBox: bbox(2), Section: []string{}},
		{Label: "paragraph", Text: "Future stuff.", Page: 2, BBox: bbox(2), Section: []string{"Outlook"}},
	}
	segs := ingest.ChunkStructuredBlocks(blocks)
	if len(segs) == 0 {
		t.Fatal("no segments produced")
	}

	// Every segment must carry a region span with a bbox.
	for i, s := range segs {
		if s.Span.Kind != "region" {
			t.Errorf("segment %d kind = %q, want region", i, s.Span.Kind)
			continue
		}
		if s.Span.Region == nil || s.Span.Region.BBox == nil {
			t.Errorf("segment %d missing region/bbox: %+v", i, s.Span)
		}
	}

	// The two "Results" paragraphs should land in a chunk whose span breadcrumb
	// is [Results].
	var resultsSeg *ingest.ChunkSegment
	for i := range segs {
		if strings.Contains(segs[i].Text, "First point.") {
			resultsSeg = &segs[i]
		}
	}
	if resultsSeg == nil {
		t.Fatal("Results content not found in any segment")
	}
	if !strings.Contains(resultsSeg.Text, "Second point.") {
		t.Errorf("expected both Results paragraphs grouped, got: %q", resultsSeg.Text)
	}
	if r := resultsSeg.Span.Region; r == nil || len(r.Section) != 1 || r.Section[0] != "Results" {
		t.Errorf("Results chunk section = %v, want [Results]", sectionOf(resultsSeg))
	}
}

// TestChunkStructuredBlocks_TableIsAtomic pins that a table block becomes its
// own chunk, not merged with surrounding paragraphs.
func TestChunkStructuredBlocks_TableIsAtomic(t *testing.T) {
	blocks := []docling.Block{
		{Label: "paragraph", Text: "Intro paragraph.", Page: 1, BBox: bbox(1), Section: []string{"S"}},
		{Label: "table", Text: "| A | B |\n| --- | --- |\n| 1 | 2 |", Page: 1, BBox: bbox(1), Section: []string{"S"}},
		{Label: "paragraph", Text: "Trailing paragraph.", Page: 1, BBox: bbox(1), Section: []string{"S"}},
	}
	segs := ingest.ChunkStructuredBlocks(blocks)

	var tableSeg *ingest.ChunkSegment
	for i := range segs {
		if strings.Contains(segs[i].Text, "| A | B |") {
			tableSeg = &segs[i]
		}
	}
	if tableSeg == nil {
		t.Fatal("table segment not found")
	}
	if strings.Contains(tableSeg.Text, "Intro paragraph.") || strings.Contains(tableSeg.Text, "Trailing paragraph.") {
		t.Errorf("table not atomic, merged with prose: %q", tableSeg.Text)
	}
}

// TestChunkStructuredBlocks_PageSpanFallback pins that a block without a
// bounding box degrades to a page span rather than a region span.
func TestChunkStructuredBlocks_PageSpanFallback(t *testing.T) {
	blocks := []docling.Block{
		{Label: "paragraph", Text: "No geometry here.", Page: 4, BBox: nil, Section: []string{"X"}},
	}
	segs := ingest.ChunkStructuredBlocks(blocks)
	if len(segs) != 1 {
		t.Fatalf("segments = %d, want 1", len(segs))
	}
	if segs[0].Span.Kind != "page" || segs[0].Span.Page != 4 {
		t.Errorf("span = %+v, want page span on page 4", segs[0].Span)
	}
}

func sectionOf(s *ingest.ChunkSegment) []string {
	if s == nil || s.Span.Region == nil {
		return nil
	}
	return s.Span.Region.Section
}
