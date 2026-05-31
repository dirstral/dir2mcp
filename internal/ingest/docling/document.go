// Package docling parses the structured DoclingDocument JSON emitted by
// `docling --to json` and linearizes it into an ordered sequence of blocks
// that preserve reading order, the section-heading hierarchy, and per-element
// provenance (page + bounding box). This is the structure that flat Markdown
// extraction discards; dir2mcp carries it through to `region` spans and
// region-accurate citations (see dirstral-spec §5.4, §7.4.B).
package docling

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Document is the subset of the DoclingDocument model that dir2mcp consumes.
// Unknown fields are tolerated (docling evolves its schema): only the parts we
// linearize are modeled, and the document version/schema name are retained so
// callers can guard against incompatible drift.
type Document struct {
	SchemaName string              `json:"schema_name"`
	Version    string              `json:"version"`
	Name       string              `json:"name"`
	Origin     *origin             `json:"origin"`
	Body       *node               `json:"body"`
	Furniture  *node               `json:"furniture"`
	Groups     []*node             `json:"groups"`
	Texts      []*textItem         `json:"texts"`
	Tables     []*tableItem        `json:"tables"`
	Pictures   []*pictureItem      `json:"pictures"`
	Pages      map[string]pageInfo `json:"pages"`
}

type origin struct {
	Filename string `json:"filename"`
	MIMEType string `json:"mimetype"`
}

type pageInfo struct {
	Size *struct {
		Width  float64 `json:"width"`
		Height float64 `json:"height"`
	} `json:"size"`
	PageNo int `json:"page_no"`
}

// node is the common shape of body/furniture/group containers: a self
// reference plus an ordered list of child references that drive reading order.
type node struct {
	SelfRef  string     `json:"self_ref"`
	Label    string     `json:"label"`
	Children []*refItem `json:"children"`
}

// refItem is an internal reference. Docling has used both "$ref" and "cref"
// across versions; we accept either.
type refItem struct {
	Ref  string `json:"$ref"`
	CRef string `json:"cref"`
}

func (r *refItem) target() string {
	if r == nil {
		return ""
	}
	if strings.TrimSpace(r.Ref) != "" {
		return r.Ref
	}
	return r.CRef
}

// textItem covers paragraphs, headings, list items, code, formulas, captions —
// anything with a textual representation.
type textItem struct {
	SelfRef string `json:"self_ref"`
	Label   string `json:"label"`
	Text    string `json:"text"`
	Orig    string `json:"orig"`
	Level   int    `json:"level"`
	Prov    []prov `json:"prov"`
}

type tableItem struct {
	SelfRef  string     `json:"self_ref"`
	Label    string     `json:"label"`
	Data     *tableData `json:"data"`
	Captions []*refItem `json:"captions"`
	Prov     []prov     `json:"prov"`
}

type tableData struct {
	NumRows    int           `json:"num_rows"`
	NumCols    int           `json:"num_cols"`
	Grid       [][]tableCell `json:"grid"`
	TableCells []tableCell   `json:"table_cells"`
}

type tableCell struct {
	Text           string `json:"text"`
	RowSpan        int    `json:"row_span"`
	ColSpan        int    `json:"col_span"`
	StartRowOffset int    `json:"start_row_offset_idx"`
	EndRowOffset   int    `json:"end_row_offset_idx"`
	StartColOffset int    `json:"start_col_offset_idx"`
	EndColOffset   int    `json:"end_col_offset_idx"`
	ColumnHeader   bool   `json:"column_header"`
	RowHeader      bool   `json:"row_header"`
}

type pictureItem struct {
	SelfRef     string       `json:"self_ref"`
	Label       string       `json:"label"`
	Captions    []*refItem   `json:"captions"`
	Annotations []annotation `json:"annotations"`
	Prov        []prov       `json:"prov"`
}

// annotation carries enrichment output (e.g. picture classification or a
// generated description). Docling shapes vary by annotation kind; we read the
// fields commonly present and ignore the rest.
type annotation struct {
	Kind           string `json:"kind"`
	Text           string `json:"text"`
	PredictedClass string `json:"predicted_class"`
}

func (a annotation) describe() string {
	if t := strings.TrimSpace(a.Text); t != "" {
		return t
	}
	return strings.TrimSpace(a.PredictedClass)
}

type prov struct {
	PageNo   int      `json:"page_no"`
	BBox     *rawBBox `json:"bbox"`
	CharSpan []int    `json:"charspan"`
}

type rawBBox struct {
	L           float64 `json:"l"`
	T           float64 `json:"t"`
	R           float64 `json:"r"`
	B           float64 `json:"b"`
	CoordOrigin string  `json:"coord_origin"`
}

// Parse decodes DoclingDocument JSON. It returns an error for malformed JSON or
// for a payload that lacks the structural minimum (a body with children); the
// caller falls back to flat extraction in that case.
func Parse(data []byte) (*Document, error) {
	var doc Document
	dec := json.NewDecoder(strings.NewReader(string(data)))
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse docling document: %w", err)
	}
	if doc.Body == nil || len(doc.Body.Children) == 0 {
		return nil, fmt.Errorf("docling document has no body content")
	}
	return &doc, nil
}

// resolveText returns the textItem for a "#/texts/N" reference, or nil.
func (d *Document) resolveText(target string) *textItem {
	if i, ok := refIndex(target, "texts"); ok && i >= 0 && i < len(d.Texts) {
		return d.Texts[i]
	}
	return nil
}

// pageHeight returns the height of a page (for TOPLEFT normalization), or 0 if
// unknown. Docling keys the pages map by stringified page number.
func (d *Document) pageHeight(pageNo int) float64 {
	if d.Pages == nil {
		return 0
	}
	if p, ok := d.Pages[strconv.Itoa(pageNo)]; ok && p.Size != nil {
		return p.Size.Height
	}
	return 0
}

// refIndex parses a reference like "#/texts/3" into (3, true) when its
// collection matches want.
func refIndex(target, want string) (int, bool) {
	parts := strings.Split(strings.TrimPrefix(target, "#/"), "/")
	if len(parts) != 2 || parts[0] != want {
		return 0, false
	}
	i, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, false
	}
	return i, true
}

// refCollection returns the collection name of a reference ("texts", "tables",
// "pictures", "groups", "body", …) and its index.
func refCollection(target string) (string, int) {
	parts := strings.Split(strings.TrimPrefix(target, "#/"), "/")
	if len(parts) == 0 {
		return "", -1
	}
	if len(parts) == 1 {
		return parts[0], -1
	}
	i, err := strconv.Atoi(parts[1])
	if err != nil {
		return parts[0], -1
	}
	return parts[0], i
}
