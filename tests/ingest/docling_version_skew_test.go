package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest/docling"
)

// The docling DoclingDocument schema evolves across releases; the adapter is
// built to tolerate that drift (unknown top-level fields ignored, `cref` and
// `$ref` reference aliases, label aliases, unknown labels degrading to
// paragraph) so a docling upgrade does not silently break extraction. These
// tests pin that version-skew contract directly against the parser/linearizer,
// with no docling binary, so CI catches adapter-protocol regressions (#145).

// TestDoclingParse_RejectsMalformedJSON pins the contract that drives the flat
// fallback: malformed JSON yields an error (the caller then returns the raw
// command output verbatim rather than fabricating structure).
func TestDoclingParse_RejectsMalformedJSON(t *testing.T) {
	if _, err := docling.Parse([]byte("{not valid json")); err == nil {
		t.Fatal("Parse(malformed) = nil error, want a parse error so the caller can fall back to flat extraction")
	}
}

// TestDoclingParse_RejectsEmptyBody pins that a structurally-empty document
// (no body, or a body with no children) is rejected, so non-DoclingDocument
// output (e.g. a plain-Markdown custom command) is not misread as structured.
func TestDoclingParse_RejectsEmptyBody(t *testing.T) {
	cases := map[string]string{
		"no body":            `{"schema_name":"DoclingDocument","version":"1.2.0"}`,
		"body no children":   `{"schema_name":"DoclingDocument","version":"1.2.0","body":{"self_ref":"#/body","children":[]}}`,
		"flat markdown text": "# Just markdown\n\nnot json at all",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := docling.Parse([]byte(in)); err == nil {
				t.Errorf("Parse(%q) = nil error, want rejection so the caller falls back to flat extraction", name)
			}
		})
	}
}

// TestDoclingParse_ToleratesUnknownTopLevelFields pins forward-compat: a future
// docling release that adds top-level fields the adapter does not model must
// still parse and linearize cleanly (unknown fields ignored, not fatal).
func TestDoclingParse_ToleratesUnknownTopLevelFields(t *testing.T) {
	in := `{
	  "schema_name": "DoclingDocument",
	  "version": "9.9.0-future",
	  "name": "Forward Compat",
	  "some_new_field": {"nested": [1, 2, 3]},
	  "key_value_items": [],
	  "body": {"self_ref": "#/body", "children": [{"$ref": "#/texts/0"}]},
	  "texts": [
	    {"self_ref": "#/texts/0", "label": "paragraph", "text": "Survives schema drift.",
	     "unknown_per_text_field": true}
	  ]
	}`
	doc, err := docling.Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse with unknown fields: %v", err)
	}
	if doc.Version != "9.9.0-future" {
		t.Errorf("Version = %q, want it retained for drift guards", doc.Version)
	}
	blocks := doc.Linearize()
	if _, ok := findBlock(blocks, docling.LabelParagraph, "Survives schema drift."); !ok {
		t.Errorf("paragraph lost when unknown fields are present: %+v", blocks)
	}
}

// TestDoclingLinearize_AcceptsCrefReferenceAlias pins that the adapter follows
// the `cref` reference key as well as `$ref`. Docling has emitted both across
// versions; a release that switches keys must not drop body content.
func TestDoclingLinearize_AcceptsCrefReferenceAlias(t *testing.T) {
	in := `{
	  "schema_name": "DoclingDocument", "version": "1.0.0", "name": "Cref Doc",
	  "body": {"self_ref": "#/body", "children": [{"cref": "#/texts/0"}]},
	  "texts": [
	    {"self_ref": "#/texts/0", "label": "paragraph", "text": "Reached via cref."}
	  ]
	}`
	doc, err := docling.Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	blocks := doc.Linearize()
	if _, ok := findBlock(blocks, docling.LabelParagraph, "Reached via cref."); !ok {
		t.Errorf("cref reference not followed; body content dropped: %+v", blocks)
	}
}

// TestDoclingLinearize_LabelAliasesAndDegradation pins the label-normalization
// contract: known aliases (`subtitle-level-1` -> section_header) map onto the
// spec label set, and an unknown/future label degrades to paragraph so its text
// is still indexed rather than discarded.
func TestDoclingLinearize_LabelAliasesAndDegradation(t *testing.T) {
	in := `{
	  "schema_name": "DoclingDocument", "version": "1.0.0", "name": "Labels",
	  "body": {"self_ref": "#/body", "children": [
	    {"$ref": "#/texts/0"}, {"$ref": "#/texts/1"}, {"$ref": "#/texts/2"}
	  ]},
	  "texts": [
	    {"self_ref": "#/texts/0", "label": "subtitle-level-1", "level": 1, "text": "Legacy Heading"},
	    {"self_ref": "#/texts/1", "label": "footnote", "text": "Unknown label body."},
	    {"self_ref": "#/texts/2", "label": "TITLE", "text": "Mixed Case Title"}
	  ]
	}`
	doc, err := docling.Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	blocks := doc.Linearize()

	if _, ok := findBlock(blocks, docling.LabelSectionHeader, "Legacy Heading"); !ok {
		t.Error("subtitle-level-1 should normalize to section_header")
	}
	// An unknown label must still surface its text, under the paragraph label.
	if _, ok := findBlock(blocks, docling.LabelParagraph, "Unknown label body."); !ok {
		t.Error("unknown label should degrade to paragraph (text retained, not dropped)")
	}
	// Label matching is case-insensitive, so "TITLE" is a title.
	if _, ok := findBlock(blocks, docling.LabelTitle, "Mixed Case Title"); !ok {
		t.Error("upper-case TITLE should normalize to the title label")
	}
}

// TestDoclingLinearize_OrigFallbackWhenTextEmpty pins that an element carrying
// only `orig` (some docling enrichments populate orig, not text) still yields a
// block, so its content is not lost.
func TestDoclingLinearize_OrigFallbackWhenTextEmpty(t *testing.T) {
	in := `{
	  "schema_name": "DoclingDocument", "version": "1.0.0", "name": "Orig",
	  "body": {"self_ref": "#/body", "children": [{"$ref": "#/texts/0"}]},
	  "texts": [
	    {"self_ref": "#/texts/0", "label": "paragraph", "text": "", "orig": "From orig only."}
	  ]
	}`
	doc, err := docling.Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	blocks := doc.Linearize()
	if _, ok := findBlock(blocks, docling.LabelParagraph, "From orig only."); !ok {
		t.Errorf("orig fallback lost: %+v", blocks)
	}
}

// TestDoclingLinearize_BottomLeftBBoxNormalizedToTopLeft pins the provenance
// normalization contract (spec §5.4): a BOTTOMLEFT-origin bbox is flipped to
// TOPLEFT when the page height is known, with the origin recorded as TOPLEFT.
// This is what keeps region citations accurate regardless of docling's stored
// coordinate convention.
func TestDoclingLinearize_BottomLeftBBoxNormalizedToTopLeft(t *testing.T) {
	in := `{
	  "schema_name": "DoclingDocument", "version": "1.0.0", "name": "BBox",
	  "pages": {"1": {"size": {"width": 612, "height": 792}, "page_no": 1}},
	  "body": {"self_ref": "#/body", "children": [{"$ref": "#/texts/0"}]},
	  "texts": [
	    {"self_ref": "#/texts/0", "label": "paragraph", "text": "Bottom-left box.",
	     "prov": [{"page_no": 1, "bbox": {"l": 72, "t": 100, "r": 540, "b": 60, "coord_origin": "BOTTOMLEFT"}}]}
	  ]
	}`
	doc, err := docling.Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b, ok := findBlock(doc.Linearize(), docling.LabelParagraph, "Bottom-left box.")
	if !ok || b.BBox == nil {
		t.Fatalf("expected a paragraph block with a bbox, got %+v ok=%v", b, ok)
	}
	if b.BBox.CoordOrigin != "TOPLEFT" {
		t.Errorf("coord_origin = %q, want TOPLEFT after normalization", b.BBox.CoordOrigin)
	}
	// height=792, BOTTOMLEFT t=100 b=60 -> TOPLEFT top = 792-100 = 692, bottom = 792-60 = 732.
	if b.BBox.T != 692 || b.BBox.B != 732 {
		t.Errorf("normalized bbox T/B = %v/%v, want 692/732", b.BBox.T, b.BBox.B)
	}
	if b.BBox.T > b.BBox.B {
		t.Errorf("normalized bbox must satisfy T<=B, got T=%v B=%v", b.BBox.T, b.BBox.B)
	}
}

// TestDoclingRenderMarkdown_EscapesTablePipes pins that cell text containing a
// pipe is escaped so it cannot break the Markdown table column layout (a common
// source of corrupted structured output on real documents).
func TestDoclingRenderMarkdown_EscapesTablePipes(t *testing.T) {
	in := `{
	  "schema_name": "DoclingDocument", "version": "1.0.0", "name": "Pipes",
	  "body": {"self_ref": "#/body", "children": [{"$ref": "#/tables/0"}]},
	  "tables": [
	    {"self_ref": "#/tables/0", "label": "table", "data": {
	      "num_rows": 2, "num_cols": 1, "table_cells": [
	        {"text": "Header", "start_row_offset_idx": 0, "end_row_offset_idx": 1, "start_col_offset_idx": 0, "end_col_offset_idx": 1, "column_header": true},
	        {"text": "a|b", "start_row_offset_idx": 1, "end_row_offset_idx": 2, "start_col_offset_idx": 0, "end_col_offset_idx": 1}
	      ]
	    }}
	  ]
	}`
	doc, err := docling.Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	md := docling.RenderMarkdown(doc.Linearize())
	if !strings.Contains(md, `a\|b`) {
		t.Errorf("table cell pipe not escaped, layout would break:\n%s", md)
	}
}
