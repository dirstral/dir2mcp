package store

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// TestRegionSpanRoundTrip pins that a region span survives spanToRow ->
// spanFromRow: page range comes back from start/end, and bbox/section/label
// come back from the extra_json payload (spec 0.9.0 §5.4).
func TestRegionSpanRoundTrip(t *testing.T) {
	in := model.Span{
		Kind: "region",
		Region: &model.RegionSpan{
			StartPage: 3,
			EndPage:   3,
			BBox: &model.BBox{
				Page: 3, L: 72, T: 90.5, R: 523, B: 410.2, CoordOrigin: "TOPLEFT",
			},
			Section: []string{"Results", "3.1 Revenue"},
			Label:   "paragraph",
		},
	}

	kind, start, end, extra, err := spanToRow(in)
	if err != nil {
		t.Fatalf("spanToRow: %v", err)
	}
	if kind != "region" || start != 3 || end != 3 {
		t.Fatalf("row = (%q,%d,%d), want (region,3,3)", kind, start, end)
	}
	if extra == "" {
		t.Fatal("region span must produce a non-empty extra_json payload")
	}

	out := spanFromRow(kind, start, end, extra)
	if out.Kind != "region" || out.Region == nil {
		t.Fatalf("round-trip lost region: %+v", out)
	}
	r := out.Region
	if r.StartPage != 3 || r.EndPage != 3 {
		t.Errorf("page range = %d-%d, want 3-3", r.StartPage, r.EndPage)
	}
	if r.BBox == nil || r.BBox.L != 72 || r.BBox.T != 90.5 || r.BBox.R != 523 || r.BBox.B != 410.2 {
		t.Errorf("bbox lost: %+v", r.BBox)
	}
	if r.BBox != nil && r.BBox.CoordOrigin != "TOPLEFT" {
		t.Errorf("coord_origin = %q, want TOPLEFT", r.BBox.CoordOrigin)
	}
	if len(r.Section) != 2 || r.Section[0] != "Results" || r.Section[1] != "3.1 Revenue" {
		t.Errorf("section = %v, want [Results 3.1 Revenue]", r.Section)
	}
	if r.Label != "paragraph" {
		t.Errorf("label = %q, want paragraph", r.Label)
	}
}

// TestRegionSpanMultiPage covers a region whose elements span more than one
// page: start/end carry the full range while bbox stays on the primary page.
func TestRegionSpanMultiPage(t *testing.T) {
	in := model.Span{
		Kind: "region",
		Region: &model.RegionSpan{
			StartPage: 4,
			EndPage:   5,
			BBox:      &model.BBox{Page: 4, L: 10, T: 20, R: 30, B: 40, CoordOrigin: "TOPLEFT"},
		},
	}
	kind, start, end, extra, err := spanToRow(in)
	if err != nil {
		t.Fatalf("spanToRow: %v", err)
	}
	out := spanFromRow(kind, start, end, extra)
	if out.Region == nil || out.Region.StartPage != 4 || out.Region.EndPage != 5 {
		t.Fatalf("page range lost: %+v", out.Region)
	}
	if out.Region.BBox == nil || out.Region.BBox.Page != 4 {
		t.Errorf("primary bbox page = %v, want 4", out.Region.BBox)
	}
}

// TestRegionSpanToRow_Rejects pins the validation guards in spanToRow.
func TestRegionSpanToRow_Rejects(t *testing.T) {
	cases := map[string]model.Span{
		"missing region payload": {Kind: "region"},
		"missing bbox":           {Kind: "region", Region: &model.RegionSpan{StartPage: 1, EndPage: 1}},
		"bad page range":         {Kind: "region", Region: &model.RegionSpan{StartPage: 5, EndPage: 2, BBox: &model.BBox{}}},
		"zero start page":        {Kind: "region", Region: &model.RegionSpan{StartPage: 0, EndPage: 1, BBox: &model.BBox{}}},
	}
	for name, span := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, _, _, err := spanToRow(span); err == nil {
				t.Errorf("expected error for %q", name)
			}
		})
	}
}

// TestRegionSpanFromRow_DegradesToPage pins the read-side fallback: a region
// row whose extra_json is missing or malformed degrades to a page span on the
// start page (never a region span with a nil bbox), so clients still get a
// usable citation. Mirrors the spec's "treat as a page-level citation" rule.
func TestRegionSpanFromRow_DegradesToPage(t *testing.T) {
	for name, extra := range map[string]string{
		"empty extra":   "",
		"malformed json": "{not json",
		"no bbox":        `{"section":["A"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := spanFromRow("region", 7, 7, extra)
			if got.Kind != "page" || got.Page != 7 {
				t.Errorf("degraded span = %+v, want page span on page 7", got)
			}
		})
	}

	// A region row with an unusable page range degrades to lines.
	if got := spanFromRow("region", 0, 0, `{"bbox":{"page":1}}`); got.Kind != "lines" {
		t.Errorf("zero page range should degrade to lines, got %+v", got)
	}
}

// TestScalarSpansUnaffected pins that the existing kinds still round-trip with
// an empty extra_json (stored as NULL) and no Region payload.
func TestScalarSpansUnaffected(t *testing.T) {
	cases := []model.Span{
		{Kind: "lines", StartLine: 12, EndLine: 48},
		{Kind: "page", Page: 3},
		{Kind: "time", StartMS: 1000, EndMS: 5000},
	}
	for _, in := range cases {
		kind, start, end, extra, err := spanToRow(in)
		if err != nil {
			t.Fatalf("spanToRow(%+v): %v", in, err)
		}
		if extra != "" {
			t.Errorf("%s span must have empty extra_json, got %q", kind, extra)
		}
		out := spanFromRow(kind, start, end, extra)
		if out.Kind != in.Kind || out.Region != nil {
			t.Errorf("round-trip changed %+v -> %+v", in, out)
		}
	}
}
