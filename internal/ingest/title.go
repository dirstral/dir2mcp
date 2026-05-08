package ingest

import (
	"strings"
	"unicode"
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
// Heuristic, in order of preference:
//  1. The first markdown heading line (`# Title`, `## Title`, ...).
//  2. The first line that is mostly uppercase and looks title-like.
//
// Returns an empty string if no candidate is found. Callers should treat the
// empty result as "no title available" and fall back to rel_path themselves.
func ExtractTitle(body string) string {
	if body == "" {
		return ""
	}
	if len(body) > titleScanLimit {
		body = body[:titleScanLimit]
	}
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if title := titleFromMarkdownHeading(line); title != "" {
			return title
		}
		if title := titleFromUppercaseLine(line); title != "" {
			return title
		}
	}
	return ""
}

func titleFromMarkdownHeading(line string) string {
	if !strings.HasPrefix(line, "#") {
		return ""
	}
	stripped := strings.TrimLeft(line, "#")
	stripped = strings.TrimSpace(stripped)
	if stripped == "" {
		return ""
	}
	return clampTitle(stripped)
}

// titleFromUppercaseLine returns the line if it looks like an act/document
// title: at least 3 letters, mostly uppercase, no trailing sentence punctuation.
func titleFromUppercaseLine(line string) string {
	letters := 0
	upper := 0
	for _, r := range line {
		if unicode.IsLetter(r) {
			letters++
			if unicode.IsUpper(r) {
				upper++
			}
		}
	}
	if letters < 3 {
		return ""
	}
	if float64(upper)/float64(letters) < 0.7 {
		return ""
	}
	if last := lastNonSpace(line); last == '.' {
		return ""
	}
	return clampTitle(line)
}

func clampTitle(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > titleMaxLen {
		s = strings.TrimSpace(s[:titleMaxLen])
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
