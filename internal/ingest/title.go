package ingest

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// titleScanLimit caps how much of the document body the title heuristic
// inspects. The title nearly always appears within the first kilobyte of an
// OCR'd act or markdown document.
const titleScanLimit = 4000

// titleMaxLen is the longest title we will surface; longer first-lines are
// almost certainly body text mistakenly capitalized.
const titleMaxLen = 200

// ExtractTitle attempts to derive a human-readable document title from the
// beginning of a text/markdown/OCR body. Used to humanize citations whose
// rel_path is opaque (e.g. Windows 8.3-truncated PDF filenames).
//
// Heuristic, in order of preference (earlier wins):
//  1. A `title:` key inside a leading YAML front-matter block (the author's
//     explicitly declared title).
//  2. The first markdown ATX heading line (`# Title`, `## Title`, ...) or a
//     setext H1 (a line followed by a `=====` underline). Code-fenced lines are
//     skipped so a `#` comment inside a fence is not mistaken for a heading.
//  3. The first line that is mostly uppercase and looks title-like (not a
//     digit-heavy page/running header or a colon-terminated label).
//
// Returns an empty string if no candidate is found. Callers should treat the
// empty result as "no title available" and fall back to rel_path themselves.
func ExtractTitle(body string) string {
	if body == "" {
		return ""
	}
	// Strip a leading UTF-8 BOM (U+FEFF): it is valid UTF-8 and unicode.IsSpace
	// does not treat it as space, so TrimSpace leaves it in front of a `# Heading`
	// line — hiding the heading from titleFromMarkdownHeading (#417).
	body = strings.TrimPrefix(body, "\uFEFF")
	if body == "" {
		return ""
	}
	body = truncateOnRuneBoundary(body, titleScanLimit)
	lines := strings.Split(body, "\n")

	// 1. YAML front-matter title wins outright when declared.
	if title, end, present := frontMatterTitle(lines); present {
		if title != "" {
			return title
		}
		// Front-matter block present but no title key: skip its lines so the
		// body scan does not treat front-matter values as title candidates.
		lines = lines[end:]
	}

	return titleFromBody(lines)
}

// titleFromBody scans the (post front-matter) lines for a markdown heading,
// setext H1, or an uppercase-line fallback. Markdown headings win even when an
// earlier uppercase-line candidate exists; the uppercase fallback is only
// returned if no heading is found anywhere in the scanned window.
func titleFromBody(lines []string) string {
	var uppercaseFallback string
	var prev string // previous non-blank, non-fence line (for setext underline)
	inFence := false
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if isFenceDelimiter(line) {
			inFence = !inFence
			prev = ""
			continue
		}
		if inFence || line == "" {
			prev = ""
			continue
		}
		if title := titleFromMarkdownHeading(line); title != "" {
			return title
		}
		if prev != "" && isSetextUnderline(line) {
			return clampTitle(prev)
		}
		if uppercaseFallback == "" {
			if title := titleFromUppercaseLine(line); title != "" {
				uppercaseFallback = title
			}
		}
		prev = line
	}
	return uppercaseFallback
}

// truncateOnRuneBoundary returns s truncated to at most maxBytes bytes, but
// never splits a UTF-8 rune. Slicing by raw byte offset can produce invalid
// UTF-8 (e.g. cutting a multi-byte rune in half), which would break later
// rune-aware passes. Walk back from the cut point until we find a valid
// rune boundary.
func truncateOnRuneBoundary(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// frontMatterTitle parses a leading YAML front-matter block delimited by `---`
// lines and returns its `title:` value. present is true only when a complete
// block (opening and closing delimiter) is found; end is the index of the first
// body line after the closing delimiter. A bare leading `---` with no closing
// delimiter is treated as not-a-block (it may be a thematic break or setext
// rule) so the body scan still runs over it.
func frontMatterTitle(lines []string) (title string, end int, present bool) {
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", 0, false
	}
	for i := 1; i < len(lines); i++ {
		switch strings.TrimSpace(lines[i]) {
		case "---", "...":
			return title, i + 1, true
		}
		if title == "" {
			if v, ok := frontMatterValue(lines[i], "title"); ok {
				title = v
			}
		}
	}
	return "", 0, false
}

// frontMatterValue extracts the value of a `key:` mapping line (case-insensitive
// key), stripping a single layer of matching surrounding quotes and clamping to
// titleMaxLen. ok is false when the line is not the requested key or the value
// is empty.
func frontMatterValue(line, key string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	prefix := key + ":"
	if !strings.HasPrefix(strings.ToLower(trimmed), prefix) {
		return "", false
	}
	val := strings.TrimSpace(trimmed[len(prefix):])
	val = trimMatchingQuotes(val)
	val = clampTitle(val)
	if val == "" {
		return "", false
	}
	return val, true
}

func trimMatchingQuotes(s string) string {
	if len(s) >= 2 {
		first, last := s[0], s[len(s)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// isFenceDelimiter reports whether a trimmed line opens or closes a fenced code
// block (``` or ~~~, at least three of the same character, optionally with an
// info string).
func isFenceDelimiter(line string) bool {
	return strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~")
}

// isSetextUnderline reports whether a trimmed line is a setext H1 underline: a
// non-empty run consisting solely of `=` characters.
func isSetextUnderline(line string) bool {
	if line == "" {
		return false
	}
	for _, r := range line {
		if r != '=' {
			return false
		}
	}
	return true
}

// titleFromMarkdownHeading returns the text of an ATX heading line. Per
// CommonMark the opening run of 1-6 `#` must be followed by a space/tab (or end
// of line); this rejects shebangs (`#!/usr/bin/env python`), hashtags
// (`#nowplaying`), and `#`-prefixed code comments that are not headings.
func titleFromMarkdownHeading(line string) string {
	hashes := 0
	for hashes < len(line) && line[hashes] == '#' {
		hashes++
	}
	if hashes == 0 || hashes > 6 {
		return ""
	}
	rest := line[hashes:]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return ""
	}
	stripped := strings.TrimSpace(rest)
	if stripped == "" {
		return ""
	}
	return clampTitle(stripped)
}

// titleFromUppercaseLine returns the line if it looks like an act/document
// title: at least 3 letters, mostly uppercase, not digit-heavy (a page or
// running header such as `PAGE 1 OF 10` / `JANUARY 2026`), and not terminated
// by sentence punctuation or a label colon (`ABSTRACT:`).
func titleFromUppercaseLine(line string) string {
	letters, upper, digits := 0, 0, 0
	for _, r := range line {
		switch {
		case unicode.IsLetter(r):
			letters++
			if unicode.IsUpper(r) {
				upper++
			}
		case unicode.IsDigit(r):
			digits++
		}
	}
	if letters < 3 {
		return ""
	}
	if float64(upper)/float64(letters) < 0.7 {
		return ""
	}
	// Digit-heavy lines are page numbers / running headers, not titles. A lone
	// year embedded in a long title (e.g. "... ACT, 2021") stays under this bar.
	if digits*2 >= letters {
		return ""
	}
	switch lastNonSpace(line) {
	case '.', ':':
		return ""
	}
	return clampTitle(line)
}

func clampTitle(s string) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) > titleMaxLen {
		s = strings.TrimSpace(string(runes[:titleMaxLen]))
	}
	return s
}

func lastNonSpace(s string) rune {
	runes := []rune(s)
	for i := len(runes) - 1; i >= 0; i-- {
		if !unicode.IsSpace(runes[i]) {
			return runes[i]
		}
	}
	return 0
}
