package docling

import "strings"

// BBox is a bounding box in the source document's point space. CoordOrigin
// records the origin actually stored ("TOPLEFT" or "BOTTOMLEFT"), matching
// dirstral-spec §5.4.
type BBox struct {
	Page        int     `json:"page"`
	L           float64 `json:"l"`
	T           float64 `json:"t"`
	R           float64 `json:"r"`
	B           float64 `json:"b"`
	CoordOrigin string  `json:"coord_origin"`
}

// Block is one element in reading order with its provenance and the active
// section breadcrumb. Label is normalized to the spec's discrete set:
// paragraph, section_header, list_item, table, caption, code, formula,
// picture, title.
type Block struct {
	Label    string
	Text     string
	Level    int      // heading level for section_header/title
	Page     int      // primary page (first prov page); 0 when unknown
	BBox     *BBox    // union on the primary page; nil when no provenance
	Section  []string // heading breadcrumb (outermost first), excludes self
	CharSpan []int    // optional char offsets into the source element
}

const (
	LabelParagraph     = "paragraph"
	LabelSectionHeader = "section_header"
	LabelListItem      = "list_item"
	LabelTable         = "table"
	LabelCaption       = "caption"
	LabelCode          = "code"
	LabelFormula       = "formula"
	LabelPicture       = "picture"
	LabelTitle         = "title"
)

// normalizeLabel maps docling's DocItemLabel values onto the spec's discrete
// label set. Unknown labels degrade to paragraph so their text is still
// indexed.
func normalizeLabel(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "title":
		return LabelTitle
	case "section_header", "subtitle-level-1":
		return LabelSectionHeader
	case "list_item":
		return LabelListItem
	case "table":
		return LabelTable
	case "caption":
		return LabelCaption
	case "code":
		return LabelCode
	case "formula":
		return LabelFormula
	case "picture":
		return LabelPicture
	case "paragraph", "text", "":
		return LabelParagraph
	default:
		return LabelParagraph
	}
}

// Title returns the document title from the first title-labeled text element,
// or "" when the document has no title element.
//
// It deliberately does NOT fall back to the document name: docling derives that
// name from the input filename, and dir2mcp feeds docling a temp file, so the
// fallback surfaced the temp filename (e.g. "dir2mcp-docling-1234") as the
// document title (issue #383). Returning "" instead lets the ingest service run
// its content-based title heuristic (ExtractTitle) — the same path Mistral OCR
// uses — yielding a real title like the document's heading.
func (d *Document) Title() string {
	for _, t := range d.Texts {
		if t != nil && normalizeLabel(t.Label) == LabelTitle {
			if s := strings.TrimSpace(t.Text); s != "" {
				return s
			}
		}
	}
	return ""
}

// headingFrame tracks one open section in the breadcrumb stack.
type headingFrame struct {
	level int
	text  string
}

// Linearize walks the body tree in reading order, resolving references and
// recursing into groups, and returns the ordered blocks. It threads the
// section-heading breadcrumb so every block beneath a heading carries it, and
// attaches per-element provenance (primary page + union bbox).
func (d *Document) Linearize() []Block {
	w := &walker{doc: d, seen: map[string]bool{}}
	w.walk(d.Body)
	return w.blocks
}

type walker struct {
	doc    *Document
	blocks []Block
	stack  []headingFrame // open headings, shallowest first
	seen   map[string]bool
}

func (w *walker) breadcrumb() []string {
	if len(w.stack) == 0 {
		return nil
	}
	out := make([]string, 0, len(w.stack))
	for _, f := range w.stack {
		out = append(out, f.text)
	}
	return out
}

// pushHeading updates the breadcrumb stack: a heading at level L closes any
// open headings at level >= L before becoming the new deepest frame.
func (w *walker) pushHeading(level int, text string) {
	if level <= 0 {
		level = 1
	}
	for len(w.stack) > 0 && w.stack[len(w.stack)-1].level >= level {
		w.stack = w.stack[:len(w.stack)-1]
	}
	w.stack = append(w.stack, headingFrame{level: level, text: text})
}

func (w *walker) walk(n *node) {
	if n == nil {
		return
	}
	for _, child := range n.Children {
		w.visit(child.target())
	}
}

func (w *walker) visit(target string) {
	if target == "" || w.seen[target] {
		return
	}
	coll, idx := refCollection(target)
	switch coll {
	case "texts":
		if idx >= 0 && idx < len(w.doc.Texts) {
			w.seen[target] = true
			w.emitText(w.doc.Texts[idx])
		}
	case "tables":
		if idx >= 0 && idx < len(w.doc.Tables) {
			w.seen[target] = true
			w.emitTable(w.doc.Tables[idx])
		}
	case "pictures":
		if idx >= 0 && idx < len(w.doc.Pictures) {
			w.seen[target] = true
			w.emitPicture(w.doc.Pictures[idx])
		}
	case "groups":
		if idx >= 0 && idx < len(w.doc.Groups) {
			w.seen[target] = true
			w.walk(w.doc.Groups[idx]) // groups hold no content; recurse for order
		}
	}
}

func (w *walker) emitText(t *textItem) {
	if t == nil {
		return
	}
	label := normalizeLabel(t.Label)
	text := strings.TrimSpace(t.Text)
	if text == "" {
		text = strings.TrimSpace(t.Orig)
	}

	if label == LabelTitle {
		// The document title is metadata, not a section: it is emitted as a
		// block (and feeds documents.title) but does NOT enter the section
		// breadcrumb stack, so content beneath it is not nested under the title.
		page, bbox := primaryProv(t.Prov, w.doc)
		w.blocks = append(w.blocks, Block{
			Label:    LabelTitle,
			Text:     text,
			Level:    t.Level,
			Page:     page,
			BBox:     bbox,
			Section:  w.breadcrumb(),
			CharSpan: charSpan(t.Prov),
		})
		return
	}

	if label == LabelSectionHeader {
		// Section headers update the breadcrumb using their declared level.
		level := t.Level
		page, bbox := primaryProv(t.Prov, w.doc)
		// The heading's own breadcrumb is the context it sits under (computed
		// before it is pushed), so it is not self-referential.
		w.blocks = append(w.blocks, Block{
			Label:    LabelSectionHeader,
			Text:     text,
			Level:    level,
			Page:     page,
			BBox:     bbox,
			Section:  w.breadcrumb(),
			CharSpan: charSpan(t.Prov),
		})
		if text != "" {
			w.pushHeading(level, text)
		}
		return
	}

	if text == "" {
		return
	}
	page, bbox := primaryProv(t.Prov, w.doc)
	w.blocks = append(w.blocks, Block{
		Label:    label,
		Text:     text,
		Page:     page,
		BBox:     bbox,
		Section:  w.breadcrumb(),
		CharSpan: charSpan(t.Prov),
	})
}

func (w *walker) emitTable(t *tableItem) {
	if t == nil {
		return
	}
	md := renderTableMarkdown(t.Data)
	if cap := w.captionText(t.Captions); cap != "" {
		md = strings.TrimRight(md, "\n") + "\n\n" + cap
	}
	if strings.TrimSpace(md) == "" {
		return
	}
	page, bbox := primaryProv(t.Prov, w.doc)
	w.blocks = append(w.blocks, Block{
		Label:   LabelTable,
		Text:    md,
		Page:    page,
		BBox:    bbox,
		Section: w.breadcrumb(),
	})
}

func (w *walker) emitPicture(p *pictureItem) {
	if p == nil {
		return
	}
	var parts []string
	if cap := w.captionText(p.Captions); cap != "" {
		parts = append(parts, cap)
	}
	for _, a := range p.Annotations {
		if d := a.describe(); d != "" {
			parts = append(parts, d)
		}
	}
	if len(parts) == 0 {
		return // a figure with no caption/annotation carries no searchable text
	}
	page, bbox := primaryProv(p.Prov, w.doc)
	w.blocks = append(w.blocks, Block{
		Label:   LabelPicture,
		Text:    strings.Join(parts, "\n\n"),
		Page:    page,
		BBox:    bbox,
		Section: w.breadcrumb(),
	})
}

func (w *walker) captionText(refs []*refItem) string {
	var parts []string
	for _, r := range refs {
		if t := w.doc.resolveText(r.target()); t != nil {
			// Mirror emitText's Text-or-Orig fallback so captions are not lost
			// when docling populates only orig.
			s := strings.TrimSpace(t.Text)
			if s == "" {
				s = strings.TrimSpace(t.Orig)
			}
			if s != "" {
				parts = append(parts, s)
				w.seen[r.target()] = true // don't also emit the caption standalone
			}
		}
	}
	return strings.Join(parts, " ")
}

// primaryProv returns the primary page (first provenance entry's page) and the
// union bbox of all provenance entries on that page, normalized to TOPLEFT when
// the page height is known. Returns (0, nil) when there is no provenance.
func primaryProv(provs []prov, d *Document) (int, *BBox) {
	first := -1
	for _, p := range provs {
		if p.PageNo > 0 {
			first = p.PageNo
			break
		}
	}
	if first < 0 {
		return 0, nil
	}
	var union *BBox
	for _, p := range provs {
		if p.PageNo != first || p.BBox == nil {
			continue
		}
		b := normalizeBBox(first, p.BBox, d.pageHeight(first))
		if union == nil {
			union = &b
			continue
		}
		union.L = min64(union.L, b.L)
		union.T = min64(union.T, b.T)
		union.R = max64(union.R, b.R)
		union.B = max64(union.B, b.B)
	}
	return first, union
}

// normalizeBBox converts a docling bbox to TOPLEFT origin when pageHeight is
// known; otherwise it preserves the reported origin (per spec: SHOULD
// normalize, MUST record the origin actually stored). The returned box always
// has L<=R and T<=B in the stored coordinate space.
func normalizeBBox(page int, raw *rawBBox, pageHeight float64) BBox {
	origin := strings.ToUpper(strings.TrimSpace(raw.CoordOrigin))
	if origin == "" {
		origin = "TOPLEFT"
	}
	l, r := minmax(raw.L, raw.R)
	if origin == "BOTTOMLEFT" && pageHeight > 0 {
		// Flip vertical axis: top = H - b, bottom = H - t.
		top := pageHeight - max64(raw.T, raw.B)
		bot := pageHeight - min64(raw.T, raw.B)
		return BBox{Page: page, L: l, T: top, R: r, B: bot, CoordOrigin: "TOPLEFT"}
	}
	t, b := minmax(raw.T, raw.B)
	return BBox{Page: page, L: l, T: t, R: r, B: b, CoordOrigin: origin}
}

func charSpan(provs []prov) []int {
	for _, p := range provs {
		if len(p.CharSpan) == 2 {
			return []int{p.CharSpan[0], p.CharSpan[1]}
		}
	}
	return nil
}

func minmax(a, b float64) (float64, float64) {
	if a <= b {
		return a, b
	}
	return b, a
}

func min64(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
