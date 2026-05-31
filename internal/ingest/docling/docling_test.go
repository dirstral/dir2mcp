package docling

import (
	"strings"
	"testing"
)

// sampleDoc is a small DoclingDocument exercising the structures dir2mcp
// linearizes: a title, nested section headers, paragraphs with provenance, a
// list item, a table with a caption, and a figure with a caption + annotation.
// Reading order is driven by body.children (intentionally not the order of the
// texts array) so the test pins that references — not array order — win.
const sampleDoc = `{
  "schema_name": "DoclingDocument",
  "version": "1.2.0",
  "name": "Quarterly Report",
  "pages": { "1": { "size": { "width": 612, "height": 792 }, "page_no": 1 } },
  "body": { "self_ref": "#/body", "children": [
    { "$ref": "#/texts/0" },
    { "$ref": "#/texts/1" },
    { "$ref": "#/texts/2" },
    { "$ref": "#/texts/3" },
    { "$ref": "#/texts/4" },
    { "$ref": "#/tables/0" },
    { "$ref": "#/pictures/0" }
  ]},
  "texts": [
    { "self_ref": "#/texts/0", "label": "title", "text": "Quarterly Report",
      "prov": [{ "page_no": 1, "bbox": { "l": 72, "t": 700, "r": 540, "b": 720, "coord_origin": "BOTTOMLEFT" }, "charspan": [0, 16] }] },
    { "self_ref": "#/texts/1", "label": "section_header", "level": 1, "text": "Results",
      "prov": [{ "page_no": 1, "bbox": { "l": 72, "t": 650, "r": 300, "b": 670, "coord_origin": "BOTTOMLEFT" }}] },
    { "self_ref": "#/texts/2", "label": "section_header", "level": 2, "text": "Revenue",
      "prov": [{ "page_no": 1, "bbox": { "l": 72, "t": 620, "r": 280, "b": 635, "coord_origin": "BOTTOMLEFT" }}] },
    { "self_ref": "#/texts/3", "label": "paragraph", "text": "Revenue grew 20% year over year.",
      "prov": [{ "page_no": 1, "bbox": { "l": 72, "t": 590, "r": 500, "b": 610, "coord_origin": "BOTTOMLEFT" }}] },
    { "self_ref": "#/texts/4", "label": "list_item", "text": "North America led growth.",
      "prov": [{ "page_no": 1, "bbox": { "l": 90, "t": 560, "r": 480, "b": 575, "coord_origin": "BOTTOMLEFT" }}] },
    { "self_ref": "#/texts/5", "label": "caption", "text": "Table 1: Revenue by region." }
  ],
  "tables": [
    { "self_ref": "#/tables/0", "label": "table",
      "captions": [{ "$ref": "#/texts/5" }],
      "prov": [{ "page_no": 1, "bbox": { "l": 72, "t": 400, "r": 540, "b": 540, "coord_origin": "BOTTOMLEFT" }}],
      "data": { "num_rows": 2, "num_cols": 2, "table_cells": [
        { "text": "Region", "start_row_offset_idx": 0, "end_row_offset_idx": 1, "start_col_offset_idx": 0, "end_col_offset_idx": 1, "column_header": true },
        { "text": "Revenue", "start_row_offset_idx": 0, "end_row_offset_idx": 1, "start_col_offset_idx": 1, "end_col_offset_idx": 2, "column_header": true },
        { "text": "NA", "start_row_offset_idx": 1, "end_row_offset_idx": 2, "start_col_offset_idx": 0, "end_col_offset_idx": 1 },
        { "text": "$5M", "start_row_offset_idx": 1, "end_row_offset_idx": 2, "start_col_offset_idx": 1, "end_col_offset_idx": 2 }
      ]}}
  ],
  "pictures": [
    { "self_ref": "#/pictures/0", "label": "picture",
      "captions": [],
      "annotations": [{ "kind": "classification", "predicted_class": "bar_chart" }],
      "prov": [{ "page_no": 1, "bbox": { "l": 72, "t": 200, "r": 540, "b": 380, "coord_origin": "BOTTOMLEFT" }}] }
  ]
}`

func TestParse_RejectsMalformed(t *testing.T) {
	if _, err := Parse([]byte("not json")); err == nil {
		t.Fatal("expected error for non-JSON input")
	}
	if _, err := Parse([]byte(`{"body": {"children": []}}`)); err == nil {
		t.Fatal("expected error for empty body")
	}
}

func TestLinearize_ReadingOrderAndBlocks(t *testing.T) {
	doc, err := Parse([]byte(sampleDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	blocks := doc.Linearize()

	// title, Results, Revenue, paragraph, list_item, table, picture = 7
	if len(blocks) != 7 {
		t.Fatalf("expected 7 blocks, got %d: %+v", len(blocks), labels(blocks))
	}
	wantLabels := []string{
		LabelTitle, LabelSectionHeader, LabelSectionHeader,
		LabelParagraph, LabelListItem, LabelTable, LabelPicture,
	}
	for i, want := range wantLabels {
		if blocks[i].Label != want {
			t.Errorf("block %d label = %q, want %q", i, blocks[i].Label, want)
		}
	}
}

func TestLinearize_SectionBreadcrumb(t *testing.T) {
	doc, _ := Parse([]byte(sampleDoc))
	blocks := doc.Linearize()

	// The paragraph sits under Results > Revenue.
	para := blockByText(blocks, "Revenue grew 20% year over year.")
	if para == nil {
		t.Fatal("paragraph block not found")
	}
	got := strings.Join(para.Section, " > ")
	if got != "Results > Revenue" {
		t.Errorf("paragraph breadcrumb = %q, want %q", got, "Results > Revenue")
	}

	// The level-2 header itself sits under Results (its breadcrumb excludes
	// itself).
	rev := blockByText(blocks, "Revenue")
	if rev == nil || len(rev.Section) != 1 || rev.Section[0] != "Results" {
		t.Errorf("Revenue header breadcrumb = %v, want [Results]", sectionOf(rev))
	}

	// The level-1 header has no parent section.
	res := blockByText(blocks, "Results")
	if res == nil || len(res.Section) != 0 {
		t.Errorf("Results header breadcrumb = %v, want []", sectionOf(res))
	}
}

func TestLinearize_ProvenanceNormalizedToTopLeft(t *testing.T) {
	doc, _ := Parse([]byte(sampleDoc))
	blocks := doc.Linearize()
	para := blockByText(blocks, "Revenue grew 20% year over year.")
	if para == nil || para.BBox == nil {
		t.Fatal("paragraph bbox missing")
	}
	if para.Page != 1 {
		t.Errorf("page = %d, want 1", para.Page)
	}
	// Source was BOTTOMLEFT t=590,b=610 on a 792-tall page → TOPLEFT
	// top = 792-610 = 182, bottom = 792-590 = 202.
	if para.BBox.CoordOrigin != "TOPLEFT" {
		t.Errorf("coord_origin = %q, want TOPLEFT", para.BBox.CoordOrigin)
	}
	if para.BBox.T != 182 || para.BBox.B != 202 {
		t.Errorf("normalized bbox T/B = %v/%v, want 182/202", para.BBox.T, para.BBox.B)
	}
	if para.BBox.L != 72 || para.BBox.R != 500 {
		t.Errorf("bbox L/R = %v/%v, want 72/500", para.BBox.L, para.BBox.R)
	}
}

func TestTitle(t *testing.T) {
	doc, _ := Parse([]byte(sampleDoc))
	if got := doc.Title(); got != "Quarterly Report" {
		t.Errorf("Title() = %q, want %q", got, "Quarterly Report")
	}
}

func TestTableRendersAtomicallyWithCaption(t *testing.T) {
	doc, _ := Parse([]byte(sampleDoc))
	blocks := doc.Linearize()
	var table *Block
	for i := range blocks {
		if blocks[i].Label == LabelTable {
			table = &blocks[i]
		}
	}
	if table == nil {
		t.Fatal("table block not found")
	}
	for _, want := range []string{"| Region | Revenue |", "| --- | --- |", "| NA | $5M |", "Table 1: Revenue by region."} {
		if !strings.Contains(table.Text, want) {
			t.Errorf("table text missing %q:\n%s", want, table.Text)
		}
	}
	// The caption must not also appear as its own standalone block.
	if blockByText(blocks, "Table 1: Revenue by region.") != nil {
		t.Error("caption leaked as a standalone block; should be folded into the table")
	}
}

func TestPictureAnnotationIndexed(t *testing.T) {
	doc, _ := Parse([]byte(sampleDoc))
	blocks := doc.Linearize()
	pic := blockByLabel(blocks, LabelPicture)
	if pic == nil {
		t.Fatal("picture block not found")
	}
	if !strings.Contains(pic.Text, "bar_chart") {
		t.Errorf("picture text missing classification: %q", pic.Text)
	}
}

func TestRenderMarkdown(t *testing.T) {
	doc, _ := Parse([]byte(sampleDoc))
	md := RenderMarkdown(doc.Linearize())
	for _, want := range []string{
		"# Quarterly Report",
		"# Results",
		"## Revenue",
		"Revenue grew 20% year over year.",
		"- North America led growth.",
		"| Region | Revenue |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("rendered markdown missing %q:\n%s", want, md)
		}
	}
}

// --- helpers ---

func labels(bs []Block) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Label
	}
	return out
}

func blockByText(bs []Block, text string) *Block {
	for i := range bs {
		if strings.TrimSpace(bs[i].Text) == text {
			return &bs[i]
		}
	}
	return nil
}

func blockByLabel(bs []Block, label string) *Block {
	for i := range bs {
		if bs[i].Label == label {
			return &bs[i]
		}
	}
	return nil
}

func sectionOf(b *Block) []string {
	if b == nil {
		return nil
	}
	return b.Section
}
