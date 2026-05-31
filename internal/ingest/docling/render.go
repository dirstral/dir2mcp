package docling

import (
	"strings"
)

// RenderMarkdown linearizes the document to Markdown in reading order. This is
// the text persisted as the extracted_markdown representation (dirstral-spec
// §7.4.B "What is persisted"): structure is rendered to Markdown here, while
// page/bbox/section provenance travels separately on the blocks (and onward to
// region spans). Headings render as ATX (`#`) by level, tables as Markdown
// tables, everything else as paragraphs.
func RenderMarkdown(blocks []Block) string {
	var b strings.Builder
	for i, blk := range blocks {
		if i > 0 {
			b.WriteString("\n\n")
		}
		switch blk.Label {
		case LabelTitle:
			b.WriteString("# ")
			b.WriteString(blk.Text)
		case LabelSectionHeader:
			level := blk.Level
			if level < 1 {
				level = 1
			}
			if level > 6 {
				level = 6
			}
			b.WriteString(strings.Repeat("#", level))
			b.WriteString(" ")
			b.WriteString(blk.Text)
		case LabelListItem:
			b.WriteString("- ")
			b.WriteString(blk.Text)
		case LabelCode:
			b.WriteString("```\n")
			b.WriteString(strings.TrimRight(blk.Text, "\n"))
			b.WriteString("\n```")
		default:
			// paragraph, table (already Markdown), caption, formula, picture
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// renderTableMarkdown converts docling TableData into a GitHub-flavored
// Markdown table. It reconstructs a row/column grid from the cells, honoring
// row/column offsets, and treats the first row as the header when any cell in
// it is flagged as a column header. Empty or malformed table data yields "".
func renderTableMarkdown(d *tableData) string {
	if d == nil {
		return ""
	}
	cells := flattenCells(d)
	if len(cells) == 0 {
		return ""
	}

	nRows, nCols := d.NumRows, d.NumCols
	for _, c := range cells {
		if c.EndRowOffset > nRows {
			nRows = c.EndRowOffset
		}
		if c.EndColOffset > nCols {
			nCols = c.EndColOffset
		}
	}
	if nRows <= 0 || nCols <= 0 {
		return ""
	}

	grid := make([][]string, nRows)
	for r := range grid {
		grid[r] = make([]string, nCols)
	}
	headerRow := -1
	for _, c := range cells {
		r := c.StartRowOffset
		col := c.StartColOffset
		if r < 0 || r >= nRows || col < 0 || col >= nCols {
			continue
		}
		grid[r][col] = sanitizeCell(c.Text)
		if c.ColumnHeader && (headerRow == -1 || r < headerRow) {
			headerRow = r
		}
	}
	if headerRow == -1 {
		headerRow = 0
	}

	var b strings.Builder
	writeRow := func(row []string) {
		b.WriteString("|")
		for _, cell := range row {
			b.WriteString(" ")
			b.WriteString(cell)
			b.WriteString(" |")
		}
		b.WriteString("\n")
	}
	writeRow(grid[headerRow])
	// separator
	b.WriteString("|")
	for c := 0; c < nCols; c++ {
		b.WriteString(" --- |")
	}
	b.WriteString("\n")
	for r := 0; r < nRows; r++ {
		if r == headerRow {
			continue
		}
		writeRow(grid[r])
	}
	return strings.TrimRight(b.String(), "\n")
}

// flattenCells returns the table cells from whichever representation docling
// populated: the flat table_cells list, or the 2D grid.
func flattenCells(d *tableData) []tableCell {
	if len(d.TableCells) > 0 {
		return d.TableCells
	}
	var out []tableCell
	for _, row := range d.Grid {
		out = append(out, row...)
	}
	return out
}

// sanitizeCell makes cell text safe for a single Markdown table cell: newlines
// become spaces and pipes are escaped so they don't break the column layout.
func sanitizeCell(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.TrimSpace(s)
}
