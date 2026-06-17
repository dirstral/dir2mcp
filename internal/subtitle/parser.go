package subtitle

import (
	"regexp"
	"strconv"
	"strings"
)

// ParseError is returned when a subtitle document cannot be parsed into any
// cues. It is intentionally lightweight (a message string) so callers can log a
// content-free diagnostic without coupling to a richer error type.
type ParseError struct{ Msg string }

func (e *ParseError) Error() string { return e.Msg }

// cueTimingRe matches a "start --> end" line in WebVTT/SRT. The timestamps may
// use either a dot (VTT) or a comma (SRT) before the millisecond field, and the
// hours component is optional (VTT permits "MM:SS.mmm"). Trailing cue settings
// after the end timestamp (VTT positioning, e.g. "align:start") are ignored.
var cueTimingRe = regexp.MustCompile(`^\s*(\d{1,2}:)?(\d{1,2}):(\d{2})[.,](\d{1,3})\s*-->\s*(\d{1,2}:)?(\d{1,2}):(\d{2})[.,](\d{1,3})`)

// ParseVTT parses a WebVTT document into ordered cues. It is robust to common
// real-world variation: an optional "WEBVTT" header line, optional cue
// identifier lines preceding a timing line, NOTE and STYLE/REGION blocks (which
// are skipped), and either dot or comma millisecond separators. Cues are
// returned in document order with 1-based Index values; cues whose text is empty
// after trimming are dropped. A document with no parseable cue yields a
// *ParseError so the caller can fall back to STT rather than ingesting nothing.
func ParseVTT(content string) ([]Cue, error) {
	return parseVTTSRT(content)
}

// ParseSRT parses a SubRip (SRT) document into ordered cues. SRT blocks are an
// optional numeric index line, a "HH:MM:SS,mmm --> HH:MM:SS,mmm" timing line,
// and one or more text lines, separated by blank lines. The parser is tolerant
// of dot or comma separators and of a missing index line. See ParseVTT for the
// shared return contract.
func ParseSRT(content string) ([]Cue, error) {
	return parseVTTSRT(content)
}

// parseVTTSRT parses the shared block structure of WebVTT and SRT. Because the
// two formats differ only in header/metadata conventions and the millisecond
// separator (both of which this parser already tolerates), a single
// line-oriented scanner handles both: it walks lines, recognises a timing line
// by its "-->" shape, accumulates the following non-empty lines as cue text, and
// flushes on a blank line or the next timing line. Identifier/index lines,
// the WEBVTT header, and NOTE/STYLE/REGION blocks are ignored.
func parseVTTSRT(content string) ([]Cue, error) {
	sc := &cueScanner{}
	skipBlock := false
	for _, raw := range splitLines(content) {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)

		if trimmed == "" {
			sc.flush()
			skipBlock = false
			continue
		}
		if skipBlock {
			continue
		}
		if isMetadataBlockStart(trimmed) {
			skipBlock = true
			continue
		}
		if s, e, ok := parseTimingLine(trimmed); ok {
			sc.startCue(s, e)
			continue
		}
		if isVTTHeader(trimmed) {
			continue
		}
		sc.addLine(line)
	}
	sc.flush()

	if len(sc.cues) == 0 {
		return nil, &ParseError{Msg: "subtitle: no cues parsed"}
	}
	return sc.cues, nil
}

// cueScanner accumulates the parser's per-cue state. Splitting it out of
// parseVTTSRT keeps that function's branch count low (gocyclo) while making the
// "timing line starts a cue, blank line flushes it" lifecycle explicit.
type cueScanner struct {
	cues       []Cue
	haveTiming bool
	start, end int
	textLines  []string
}

// startCue begins a new cue at the given bounds, first flushing any in-progress
// cue. Any text accumulated before the timing line is a cue identifier (VTT) or
// index line (SRT), not cue text, so flush discards it.
func (c *cueScanner) startCue(start, end int) {
	c.flush()
	c.haveTiming = true
	c.start, c.end = start, end
}

// addLine accumulates a content line. Before a timing line is seen the line is an
// identifier/index/header candidate, so only the most recent is kept (and later
// discarded by flush); after a timing line it is cue text.
func (c *cueScanner) addLine(line string) {
	if c.haveTiming {
		c.textLines = append(c.textLines, line)
		return
	}
	c.textLines = c.textLines[:0]
	c.textLines = append(c.textLines, line)
}

// flush emits the in-progress cue (if any) and resets the buffer. A cue with
// empty text after trimming/tag-stripping is dropped. The cue's speaker is the
// first WebVTT <v Name> voice tag found in its text lines (SPEC §8.6.8); empty
// when the cue carries no voice markup, preserving prior behaviour.
func (c *cueScanner) flush() {
	if c.haveTiming {
		if text := joinCueText(c.textLines); text != "" {
			speaker := voiceTagName(c.textLines)
			c.cues = append(c.cues, Cue{Index: len(c.cues) + 1, StartMS: c.start, EndMS: c.end, Text: text, Speaker: speaker})
		}
	}
	c.haveTiming = false
	c.textLines = c.textLines[:0]
}

// joinCueText joins the accumulated text lines of a cue with newlines, trims the
// result, and strips inline tags. Empty lines are dropped so a trailing blank
// does not introduce spurious whitespace.
func joinCueText(lines []string) string {
	parts := make([]string, 0, len(lines))
	for _, l := range lines {
		l = stripInlineTags(strings.TrimSpace(l))
		if l != "" {
			parts = append(parts, l)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// parseTimingLine parses a "start --> end" line into millisecond bounds. It
// returns ok=false when the line is not a timing line. end is clamped to be ≥
// start so a malformed cue never produces a negative-length window.
func parseTimingLine(line string) (startMS, endMS int, ok bool) {
	m := cueTimingRe.FindStringSubmatch(line)
	if m == nil {
		return 0, 0, false
	}
	startMS = timestampToMS(m[1], m[2], m[3], m[4])
	endMS = timestampToMS(m[5], m[6], m[7], m[8])
	if endMS < startMS {
		endMS = startMS
	}
	return startMS, endMS, true
}

// timestampToMS converts captured "[HH:]MM:SS" + millis fields into total
// milliseconds. The hours group includes its trailing colon (e.g. "01:") or is
// empty; the millis string is right-padded to three digits so ".5" means 500ms.
func timestampToMS(hh, mm, ss, ms string) int {
	hours := 0
	if hh != "" {
		hours, _ = strconv.Atoi(strings.TrimSuffix(hh, ":"))
	}
	minutes, _ := strconv.Atoi(mm)
	seconds, _ := strconv.Atoi(ss)
	millis, _ := strconv.Atoi(padMillis(ms))
	total := ((hours*60+minutes)*60+seconds)*1000 + millis
	if total < 0 {
		total = 0
	}
	return total
}

// padMillis right-pads a millisecond field to exactly three digits so a
// fractional second written with fewer digits (".5" / ".05") scales correctly,
// and truncates any excess so a four-digit field cannot overflow the field.
func padMillis(ms string) string {
	switch {
	case len(ms) == 0:
		return "000"
	case len(ms) >= 3:
		return ms[:3]
	default:
		return ms + strings.Repeat("0", 3-len(ms))
	}
}

func isVTTHeader(line string) bool {
	return strings.HasPrefix(line, "WEBVTT")
}

// isMetadataBlockStart reports whether a line begins a WebVTT NOTE, STYLE, or
// REGION block (all of which carry no cue text and run to the next blank line).
func isMetadataBlockStart(line string) bool {
	switch {
	case line == "NOTE" || strings.HasPrefix(line, "NOTE "):
		return true
	case line == "STYLE":
		return true
	case line == "REGION":
		return true
	default:
		return false
	}
}

// inlineTagRe matches WebVTT/TTML inline markup (e.g. <c.color>, <v Speaker>,
// <00:00:01.000>, </i>) that should not appear in indexed cue text.
var inlineTagRe = regexp.MustCompile(`</?[^>]+>`)

// voiceTagRe matches a WebVTT voice span start tag and captures the speaker
// name (SPEC §8.6.8). The tag is `<v Name>` or `<v.class1.class2 Name>`: the
// `v` tag name is optionally followed by `.`-separated classes, then required
// whitespace, then the annotation (speaker name) up to the closing `>`. The
// name capture is non-greedy and trimmed by the caller. Case-insensitive on the
// tag name only.
var voiceTagRe = regexp.MustCompile(`(?i)<v(?:\.[^\s.>]+)*\s+([^>]*)>`)

// voiceTagName returns the speaker name from the first WebVTT <v Name> voice tag
// across a cue's text lines, or "" when none is present. Voice tags without a
// name (`<v>`) yield "". The returned name is trimmed; it is metadata only and
// never alters the cue text (which has all inline tags stripped separately).
func voiceTagName(lines []string) string {
	for _, l := range lines {
		if !strings.Contains(l, "<v") && !strings.Contains(l, "<V") {
			continue
		}
		if m := voiceTagRe.FindStringSubmatch(l); m != nil {
			if name := strings.TrimSpace(m[1]); name != "" {
				return name
			}
		}
	}
	return ""
}

// stripInlineTags removes inline markup from cue text so the indexed transcript
// is plain text. It is deterministic and leaves non-tag angle content untouched
// only when it is not a well-formed tag.
func stripInlineTags(s string) string {
	if !strings.ContainsRune(s, '<') {
		return s
	}
	return strings.TrimSpace(inlineTagRe.ReplaceAllString(s, ""))
}

// splitLines splits content on LF after stripping a leading UTF-8 BOM and
// normalising CRLF and lone CR to LF so the scanner sees uniform line breaks
// regardless of the file's origin OS. Windows-authored subtitle files commonly
// carry a BOM; left in place it would prevent the first line from matching the
// WEBVTT header or a timing line.
func splitLines(content string) []string {
	content = stripBOM(content)
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	return strings.Split(content, "\n")
}

// stripBOM removes a single leading UTF-8 BOM (U+FEFF), if present. Such a mark
// is non-whitespace, so left in place it would block the WEBVTT-header / timing
// line match on the first line of a Windows-authored subtitle file.
func stripBOM(content string) string {
	return strings.TrimPrefix(content, "\uFEFF")
}
