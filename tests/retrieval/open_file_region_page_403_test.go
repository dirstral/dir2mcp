package tests

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// TestOpenFile_RegionPage_AttributedToRequestedPage pins issue #403 F7: an
// open_file page=N read that resolves a structured (docling) region chunk must
// attribute the slice to the page the caller asked about — a page the region
// genuinely covers — not to the region's primary (start) page. A multi-page
// region's cited text can sit on a later page than its primary, so localizing
// the read to the primary page mis-reports where the text is.
//
// The attribution is observed through slice ordering: matches sort by page and
// then by document reading order. Before the fix a region spanning pages 1..N
// was attributed to page 1, so it sorted ahead of a page-N-native chunk for a
// page=N read (it looked like earlier-page content). After the fix both localize
// to page N and the page-native span (sort key 0) leads the region that merely
// spans into that page.
func TestOpenFile_RegionPage_AttributedToRequestedPage(t *testing.T) {
	svc := retrieval.NewService(nil, nil, nil, nil)
	root := t.TempDir()
	svc.SetRootDir(root)
	svc.SetStateDir(filepath.Join(root, ".dir2mcp"))

	const relPath = "report.pdf"
	// A region chunk whose primary page is 1 but which spans through page 5.
	svc.SetChunkMetadata(1, model.SearchHit{
		RelPath: relPath,
		Snippet: "region-spanning-1-to-5",
		Span: model.Span{Kind: "region", Region: &model.RegionSpan{
			StartPage: 1, EndPage: 5,
			BBox: &model.BBox{Page: 1, L: 1, T: 2, R: 3, B: 4, CoordOrigin: "TOPLEFT"},
		}},
	})
	// A chunk whose span is exactly page 5.
	svc.SetChunkMetadata(2, model.SearchHit{
		RelPath: relPath,
		Snippet: "page-5-native",
		Span:    model.Span{Kind: "page", Page: 5},
	})

	out, err := svc.OpenFile(context.Background(), relPath, model.Span{Kind: "page", Page: 5}, 20000)
	if err != nil {
		t.Fatalf("OpenFile page=5: %v", err)
	}

	// Both the page-5-native chunk and the region that covers page 5 are returned.
	if !strings.Contains(out, "page-5-native") {
		t.Fatalf("page-5-native chunk missing from page=5 read: %q", out)
	}
	if !strings.Contains(out, "region-spanning-1-to-5") {
		t.Fatalf("region covering page 5 missing from page=5 read: %q", out)
	}

	// The region is now localized to the requested page 5 (not its primary page
	// 1), so the page-5-native span leads it. Before the fix the region was
	// attributed to page 1 and sorted first.
	if got := strings.Index(out, "page-5-native"); got == -1 || got > strings.Index(out, "region-spanning-1-to-5") {
		t.Fatalf("expected page-5-native content before the spanning region (region localized to primary page 1?); got %q", out)
	}
}
