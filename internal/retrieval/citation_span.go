package retrieval

import (
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/dirstral/dir2mcp/internal/model"
)

// Issue #1082. stripHallucinatedCitations (#403 F3) removes an inline tag that
// names a document the model was never shown. It reads the path only, so a
// tag that names the right document with a made-up span passed through:
// [Normans.md:L40-L48] for a block the prompt tagged [Normans.md:L25-L33]. A
// client that opens that span shows the user text the model did not read.
//
// SPEC §9.4.1 requires an inline tag to resolve to the in-context set and
// lets the server strip or otherwise neutralize one that does not. This file
// neutralizes by rewriting: a tag resolves to a shown block of its document,
// and the span it names may only narrow that block's span.
//
//   - A span inside the shown range stays as written. The model read those
//     lines, so the narrower claim is its own and is grounded.
//   - A span that reaches outside the shown range is clamped to the part the
//     model saw.
//   - A span with nothing in common with any shown block becomes the nearest
//     block's whole span.
//   - A tag with no span ([path]) names nothing unseen and stays.
//
// The rewritten tag is the §9.3 form the block header carried, so a client
// that matched the header still matches, and the answer keeps the file-level
// attribution the benchmark measured at 98.7% precision rather than losing
// the tag over its span. The structured citations are not touched: they
// already carry each shown block's real span (#403 F1).

// tagSpan is the span an inline tag or a shown block names, reduced to one
// closed interval so the two can be compared: line numbers for a line span,
// the page number for a page span, milliseconds for a time span.
type tagSpan struct {
	kind   string
	lo, hi int
}

// shownBlock is one in-context citation with the span its header rendered.
type shownBlock struct {
	citation model.Citation
	span     tagSpan
	hasSpan  bool
}

var (
	// tagPageRe reads the page suffix: "#p=3" or "#p=3-5".
	tagPageRe = regexp.MustCompile(`^#p=(\d+)(?:-(\d+))?$`)
	// tagLinesRe reads the line suffix in every spelling a model emits: the
	// §9.3 ":L12-L48", the bare "file:12-48" and "file:12", and the legacy
	// "@L12-48" (#942). The parser accepts the legacy writer's form so a tag
	// copied from an older transcript still resolves rather than being read
	// as spanless.
	tagLinesRe = regexp.MustCompile(`^[:@]L?(\d+)(?:-L?(\d+))?$`)
)

// clampCitationTagSpans rewrites every inline citation tag whose span is not
// inside the span of a shown block for that document, and reports how many
// tags it rewrote. A tag whose document is not among the citations is left
// for stripHallucinatedCitations, which runs first.
func clampCitationTagSpans(answer string, citations []model.Citation) (string, int) {
	if strings.TrimSpace(answer) == "" || len(citations) == 0 {
		return answer, 0
	}
	clamped := 0
	out := inlineCitationRe.ReplaceAllStringFunc(answer, func(match string) string {
		p := inlineCitationTagPath(match)
		if p == "" {
			return match
		}
		shown := shownBlocksFor(p, citations)
		if len(shown) == 0 {
			return match
		}
		tag, ok := parseCitationTagSpan(match)
		if !ok {
			return match
		}
		block, inside := resolveTagToBlock(tag, shown)
		if inside {
			return match
		}
		clamped++
		// inlineCitationRe absorbs one leading whitespace character; keep it,
		// so a rewrite never glues the tag to the word before it.
		lead := match[:strings.IndexByte(match, '[')]
		return lead + FormatCitation(block.citation.RelPath, clampedSpan(tag, block))
	})
	return out, clamped
}

// shownBlocksFor returns the in-context citations a tag path names, by full
// rel_path or by basename, the same leniency stripHallucinatedCitations
// applies. Order is the citation order, which is the prompt order.
func shownBlocksFor(p string, citations []model.Citation) []shownBlock {
	var out []shownBlock
	for _, c := range citations {
		rel := strings.TrimSpace(c.RelPath)
		if rel == "" || (rel != p && path.Base(rel) != p && path.Base(rel) != path.Base(p)) {
			continue
		}
		span, ok := shownTagSpan(c.Span)
		out = append(out, shownBlock{citation: c, span: span, hasSpan: ok})
	}
	return out
}

// shownTagSpan reduces a block's span to the interval its header rendered.
// It mirrors FormatCitation: the same conditions that make the header carry
// a span make the block comparable here, so a block whose header was bare
// reports no span.
func shownTagSpan(span model.Span) (tagSpan, bool) {
	switch strings.ToLower(strings.TrimSpace(span.Kind)) {
	case "page":
		if span.Page > 0 {
			return tagSpan{kind: "page", lo: span.Page, hi: span.Page}, true
		}
	case "lines":
		if span.StartLine > 0 && span.EndLine >= span.StartLine {
			return tagSpan{kind: "lines", lo: span.StartLine, hi: span.EndLine}, true
		}
	case "time":
		// The header renders whole seconds, so a model that copies it names a
		// start up to 999 ms before the real one. That copy is the shown span,
		// so the comparison starts at the rendered second.
		lo := max(span.StartMS, 0)
		return tagSpan{kind: "time", lo: lo - lo%1000, hi: max(span.EndMS, 0)}, true
	}
	return tagSpan{}, false
}

// parseCitationTagSpan reads the span an inline tag names. ok is false for a
// bare tag and for a suffix no reader can turn into a span: such a tag names
// nothing the model was not shown, so it is left alone.
func parseCitationTagSpan(match string) (tagSpan, bool) {
	inner := strings.TrimSpace(match)
	if len(inner) < 2 {
		return tagSpan{}, false
	}
	inner = strings.TrimSpace(inner[1 : len(inner)-1])
	// The speaker or breadcrumb suffix (" › S2") is not part of the span.
	if i := strings.Index(inner, " › "); i >= 0 {
		inner = inner[:i]
	}
	// citationTagPath keeps the leading token as the path; the span is the
	// suffix of that same token.
	if i := strings.IndexAny(inner, " \t"); i >= 0 {
		inner = inner[:i]
	}
	if i := strings.Index(inner, "#p="); i >= 0 {
		return parseTagRange("page", tagPageRe.FindStringSubmatch(inner[i:]))
	}
	if i := strings.Index(inner, "@t="); i >= 0 {
		return parseTagTime(inner[i+len("@t="):])
	}
	if loc := citationLineSuffixRe.FindStringIndex(inner); loc != nil {
		return parseTagRange("lines", tagLinesRe.FindStringSubmatch(inner[loc[0]:]))
	}
	if i := strings.Index(inner, "@L"); i >= 0 {
		return parseTagRange("lines", tagLinesRe.FindStringSubmatch(inner[i:]))
	}
	return tagSpan{}, false
}

// parseTagRange turns a two-group numeric submatch ("12", "48") into a span.
// A single number is a one-unit span; reversed bounds are put in order.
func parseTagRange(kind string, m []string) (tagSpan, bool) {
	if len(m) != 3 {
		return tagSpan{}, false
	}
	lo, err := strconv.Atoi(m[1])
	if err != nil {
		return tagSpan{}, false
	}
	hi := lo
	if m[2] != "" {
		if hi, err = strconv.Atoi(m[2]); err != nil {
			return tagSpan{}, false
		}
	}
	if hi < lo {
		lo, hi = hi, lo
	}
	return tagSpan{kind: kind, lo: lo, hi: hi}, true
}

// parseTagTime reads "<start>-<end>" where each bound is mm:ss, hh:mm:ss or
// a millisecond count (SPEC §9.3).
func parseTagTime(s string) (tagSpan, bool) {
	start, end, ok := strings.Cut(s, "-")
	if !ok {
		return tagSpan{}, false
	}
	lo, okLo := parseCitationTime(start)
	hi, okHi := parseCitationTime(end)
	if !okLo || !okHi {
		return tagSpan{}, false
	}
	if hi < lo {
		lo, hi = hi, lo
	}
	return tagSpan{kind: "time", lo: lo, hi: hi}, true
}

// parseCitationTime is the inverse of formatCitationTime, plus the plain
// millisecond form §9.3 allows.
func parseCitationTime(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if !strings.Contains(s, ":") {
		ms, err := strconv.Atoi(s)
		return ms, err == nil && ms >= 0
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	total := 0
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return 0, false
		}
		total = total*60 + n
	}
	return total * 1000, true
}

// resolveTagToBlock picks the shown block a tag resolves to and reports
// whether the tag's span lies inside that block's span. Among blocks of the
// tag's span kind the one with the largest overlap wins; with no overlap the
// nearest wins; with no block of that kind, the first shown block of the
// document wins and the tag is outside by construction. Ties keep prompt
// order, so the choice is deterministic.
func resolveTagToBlock(tag tagSpan, shown []shownBlock) (shownBlock, bool) {
	best, bestOverlap, bestDist := -1, 0, -1
	for i, b := range shown {
		if !b.hasSpan || b.span.kind != tag.kind {
			continue
		}
		if ov := overlapLen(tag, b.span); ov > 0 {
			if ov > bestOverlap {
				best, bestOverlap = i, ov
			}
			continue
		}
		if bestOverlap > 0 {
			continue
		}
		if d := gapLen(tag, b.span); best < 0 || d < bestDist {
			best, bestDist = i, d
		}
	}
	if best < 0 {
		return shown[0], false
	}
	b := shown[best]
	return b, tag.lo >= b.span.lo && tag.hi <= b.span.hi
}

// overlapLen is the length of the intersection of two closed intervals, or
// zero or less when they are disjoint.
func overlapLen(a, b tagSpan) int {
	return min(a.hi, b.hi) - max(a.lo, b.lo) + 1
}

// gapLen is the distance between two disjoint closed intervals.
func gapLen(a, b tagSpan) int {
	if a.hi < b.lo {
		return b.lo - a.hi
	}
	return a.lo - b.hi
}

// clampedSpan is the block's span narrowed to the part the tag overlaps, or
// the block's whole span when the tag overlaps none of it (or names a span of
// another kind). The result is always inside the shown span, and the bounds
// the block's header carried are the ceiling: a start before the real
// StartMS never survives, even though the comparison allowed the rounded one.
func clampedSpan(tag tagSpan, block shownBlock) model.Span {
	span := block.citation.Span
	if !block.hasSpan || tag.kind != block.span.kind || overlapLen(tag, block.span) <= 0 {
		return span
	}
	switch block.span.kind {
	case "lines":
		span.StartLine = max(tag.lo, span.StartLine)
		span.EndLine = min(tag.hi, span.EndLine)
	case "time":
		span.StartMS = max(tag.lo, span.StartMS)
		span.EndMS = min(tag.hi, span.EndMS)
	}
	// A page block is one page; a tag that overlaps it names that page.
	return span
}
