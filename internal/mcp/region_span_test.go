package mcp

import (
	"reflect"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// TestBuildOpenFileSpan_Region pins the region span rendering in MCP tool
// output (spec §15.1.1): page range + primary-page bbox + section breadcrumb.
func TestBuildOpenFileSpan_Region(t *testing.T) {
	span := model.Span{
		Kind: "region",
		Region: &model.RegionSpan{
			StartPage: 3,
			EndPage:   3,
			BBox:      &model.BBox{Page: 3, L: 72, T: 90.5, R: 523, B: 410.2, CoordOrigin: "TOPLEFT"},
			Section:   []string{"Results", "3.1 Revenue"},
			Label:     "paragraph",
		},
	}
	got := buildOpenFileSpan(span)

	if got["kind"] != "region" {
		t.Fatalf("kind = %v, want region", got["kind"])
	}
	if got["start_page"] != 3 || got["end_page"] != 3 {
		t.Errorf("page range = %v-%v, want 3-3", got["start_page"], got["end_page"])
	}
	bbox, ok := got["bbox"].(map[string]interface{})
	if !ok {
		t.Fatalf("bbox missing or wrong type: %T", got["bbox"])
	}
	if bbox["page"] != 3 || bbox["l"] != 72.0 || bbox["coord_origin"] != "TOPLEFT" {
		t.Errorf("bbox fields wrong: %+v", bbox)
	}
	if !reflect.DeepEqual(got["section"], []string{"Results", "3.1 Revenue"}) {
		t.Errorf("section = %v, want [Results 3.1 Revenue]", got["section"])
	}
	// label is internal provenance, not part of the wire Span shape.
	if _, present := got["label"]; present {
		t.Errorf("label must not be emitted in the wire span: %+v", got)
	}
}

// TestBuildOpenFileSpan_RegionDegrades pins that a malformed region span (no
// bbox) degrades to a page span on the start page, and to the document variant
// when no page is available, so clients always receive a usable citation.
func TestBuildOpenFileSpan_RegionDegrades(t *testing.T) {
	noBBox := buildOpenFileSpan(model.Span{
		Kind:   "region",
		Region: &model.RegionSpan{StartPage: 5, EndPage: 5},
	})
	if noBBox["kind"] != "page" || noBBox["page"] != 5 {
		t.Errorf("no-bbox region = %+v, want page span on 5", noBBox)
	}

	empty := buildOpenFileSpan(model.Span{Kind: "region"})
	if empty["kind"] != "document" {
		t.Errorf("payload-less region = %+v, want document variant", empty)
	}
}

// TestBuildOpenFileSpan_ScalarKindsUnchanged guards that adding region did not
// disturb the existing kinds.
func TestBuildOpenFileSpan_ScalarKindsUnchanged(t *testing.T) {
	lines := buildOpenFileSpan(model.Span{Kind: "lines", StartLine: 12, EndLine: 48})
	if lines["kind"] != "lines" || lines["start_line"] != 12 || lines["end_line"] != 48 {
		t.Errorf("lines span wrong: %+v", lines)
	}
	page := buildOpenFileSpan(model.Span{Kind: "page", Page: 7})
	if page["kind"] != "page" || page["page"] != 7 {
		t.Errorf("page span wrong: %+v", page)
	}
	tm := buildOpenFileSpan(model.Span{Kind: "time", StartMS: 1000, EndMS: 5000})
	if tm["kind"] != "time" || tm["start_ms"] != 1000 || tm["end_ms"] != 5000 {
		t.Errorf("time span wrong: %+v", tm)
	}
}
