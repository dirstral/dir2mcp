// Package subtitle renders WebVTT and SubRip (SRT) subtitle documents from a
// document's stored transcript chunks. dir2mcp already persists transcript
// representations as chunk text plus a time span (model.Span{Kind:"time",
// StartMS, EndMS}); this package is a pure read+render layer over that data,
// turning ordered chunks into cues and serializing them in either format.
package subtitle

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/dirstral/dir2mcp/internal/model"
)

// blankLineRE matches an interior run of blank lines (a newline, optional
// whitespace, and one or more further newlines). Such a run terminates a cue
// block early in both WebVTT and SRT, so renderers collapse it to a single
// newline to keep cue boundaries intact.
var blankLineRE = regexp.MustCompile(`\n[ \t]*\n+`)

// collapseBlankLines replaces any interior run of blank lines with a single
// newline so ordinary transcript text (which may contain paragraph breaks)
// cannot prematurely terminate a cue block.
func collapseBlankLines(text string) string {
	return blankLineRE.ReplaceAllString(text, "\n")
}

// vttCueTextReplacer escapes the characters that are special in WebVTT cue
// payloads. Escaping '>' also neutralises the "-->" timing arrow (it becomes
// "--&gt;"), so a cue line can never be misread as a timing line.
var vttCueTextReplacer = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
)

// escapeVTTText makes arbitrary transcript text safe as a WebVTT cue payload:
// it collapses interior blank lines, escapes '&'/'<'/'>', and (via the '>'
// escape) neutralises any "-->" so the text cannot break the cue structure.
func escapeVTTText(text string) string {
	return vttCueTextReplacer.Replace(collapseBlankLines(text))
}

// srtCueTextReplacer neutralises the "-->" timing arrow in SRT cue text. SRT
// has no markup escaping, so a literal "-->" on its own line could be parsed as
// a timing line; replacing it with "-→" keeps the text readable and unambiguous.
var srtCueTextReplacer = strings.NewReplacer("-->", "-→")

// neutralizeSRTText makes arbitrary transcript text safe as an SRT cue payload:
// it collapses interior blank lines (which would end the cue block early) and
// neutralises any "-->" timing arrow.
func neutralizeSRTText(text string) string {
	return srtCueTextReplacer.Replace(collapseBlankLines(text))
}

// Cue is a single subtitle entry: a [StartMS, EndMS] time window and the text
// shown during it. Index is the 1-based sequence number used by SRT (WebVTT
// does not require numeric identifiers and omits them).
type Cue struct {
	Index   int
	StartMS int
	EndMS   int
	Text    string
	// Speaker is the human-readable voice/speaker name parsed from a WebVTT
	// <v Name> voice tag on the cue (SPEC §8.6.8). It is metadata only: it never
	// appears in Text (voice tags are stripped) and is empty for cues with no
	// voice markup, so a transcript without <v> tags is unchanged. A sidecar
	// that carries voice tags can populate speaker attribution WITHOUT a
	// diarization model.
	Speaker string
}

// TranscriptChunk pairs a transcript chunk's text with its time span. It is the
// builder's input shape so callers (CLI/store) need not depend on the full
// model.Chunk row layout, only the two fields that matter for subtitles.
type TranscriptChunk struct {
	Text string
	Span model.Span
}

// BuildCues turns transcript chunks into ordered, 1-indexed cues. Chunks are
// sorted by span start (then end) so out-of-order storage still renders in
// playback order. Only spans of kind "time" contribute timing; chunks whose
// text is empty after trimming are skipped (an empty cue has nothing to show).
// A chunk whose span is non-time is dropped, since subtitles require timing.
func BuildCues(chunks []TranscriptChunk) []Cue {
	type timed struct {
		start   int
		end     int
		text    string
		speaker string
	}
	timedChunks := make([]timed, 0, len(chunks))
	for _, ch := range chunks {
		if !strings.EqualFold(strings.TrimSpace(ch.Span.Kind), "time") {
			continue
		}
		text := strings.TrimSpace(ch.Text)
		if text == "" {
			continue
		}
		start := ch.Span.StartMS
		end := ch.Span.EndMS
		if start < 0 {
			start = 0
		}
		if end < start {
			end = start
		}
		// Prefer the human-readable label, falling back to the stable id, so a
		// diarized transcript exports voice markup (SPEC §8.6.3); empty for a
		// non-diarized transcript so the export is unchanged.
		speaker := strings.TrimSpace(ch.Span.SpeakerLabel)
		if speaker == "" {
			speaker = strings.TrimSpace(ch.Span.Speaker)
		}
		timedChunks = append(timedChunks, timed{start: start, end: end, text: text, speaker: speaker})
	}

	sort.SliceStable(timedChunks, func(i, j int) bool {
		if timedChunks[i].start != timedChunks[j].start {
			return timedChunks[i].start < timedChunks[j].start
		}
		return timedChunks[i].end < timedChunks[j].end
	})

	cues := make([]Cue, 0, len(timedChunks))
	for i, tc := range timedChunks {
		cues = append(cues, Cue{
			Index:   i + 1,
			StartMS: tc.start,
			EndMS:   tc.end,
			Text:    tc.text,
			Speaker: tc.speaker,
		})
	}
	return cues
}

// RenderVTT serializes cues as a WebVTT document. The output is the literal
// "WEBVTT" header, a blank line, then each non-empty cue as a
// "HH:MM:SS.mmm --> HH:MM:SS.mmm" timing line followed by its text and a
// trailing blank line. Cues with empty text are skipped. Cue indices are not
// emitted (they are optional in WebVTT).
func RenderVTT(cues []Cue) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for _, cue := range cues {
		text := strings.TrimSpace(cue.Text)
		if text == "" {
			continue
		}
		b.WriteString(formatTimestampVTT(cue.StartMS))
		b.WriteString(" --> ")
		b.WriteString(formatTimestampVTT(cue.EndMS))
		b.WriteByte('\n')
		// Carry speaker as a WebVTT <v Name> voice tag when present (SPEC §8.6.3);
		// a cue with no speaker renders exactly as before. The name is sanitised
		// so it can never break out of the tag.
		if speaker := sanitizeVoiceName(cue.Speaker); speaker != "" {
			b.WriteString("<v ")
			b.WriteString(speaker)
			b.WriteByte('>')
		}
		b.WriteString(escapeVTTText(text))
		b.WriteString("\n\n")
	}
	return b.String()
}

// voiceNameReplacer makes a speaker name safe to embed in a WebVTT <v Name>
// tag: it strips '<'/'>' (which would terminate the tag or inject markup),
// escapes '&' (a bare ampersand is an invalid entity in the tag), and replaces
// CR/LF with a space (a newline would leave the <v ...> tag unterminated).
var voiceNameReplacer = strings.NewReplacer(
	"<", "",
	">", "",
	"&", "&amp;",
	"\r", " ",
	"\n", " ",
)

// sanitizeVoiceName makes a speaker name safe to embed in a WebVTT <v Name> tag.
// Returns "" for an empty/blank name so the caller omits the voice tag entirely.
func sanitizeVoiceName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	return strings.TrimSpace(voiceNameReplacer.Replace(name))
}

// RenderSRT serializes cues as a SubRip (SRT) document: a 1-based index line,
// a "HH:MM:SS,mmm --> HH:MM:SS,mmm" timing line (comma decimal separator), the
// text, and a blank separator line between cues. Empty-text cues are skipped,
// and indices are renumbered contiguously over the surviving cues so the output
// is always a valid, gap-free SRT regardless of the Index values on input.
func RenderSRT(cues []Cue) string {
	var b strings.Builder
	idx := 0
	for _, cue := range cues {
		text := strings.TrimSpace(cue.Text)
		if text == "" {
			continue
		}
		idx++
		fmt.Fprintf(&b, "%d\n", idx)
		b.WriteString(formatTimestampSRT(cue.StartMS))
		b.WriteString(" --> ")
		b.WriteString(formatTimestampSRT(cue.EndMS))
		b.WriteByte('\n')
		b.WriteString(neutralizeSRTText(text))
		b.WriteString("\n\n")
	}
	return b.String()
}

// splitTimestamp decomposes a non-negative millisecond value into zero-padded
// hours, minutes, seconds, and milliseconds components. Negative inputs are
// clamped to zero so a malformed span can never produce a negative timestamp.
func splitTimestamp(ms int) (hours, minutes, seconds, millis int) {
	if ms < 0 {
		ms = 0
	}
	millis = ms % 1000
	totalSeconds := ms / 1000
	seconds = totalSeconds % 60
	totalMinutes := totalSeconds / 60
	minutes = totalMinutes % 60
	hours = totalMinutes / 60
	return hours, minutes, seconds, millis
}

// formatTimestampVTT renders ms as a WebVTT timestamp "HH:MM:SS.mmm" with a dot
// before the millisecond field.
func formatTimestampVTT(ms int) string {
	h, m, s, msec := splitTimestamp(ms)
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, msec)
}

// formatTimestampSRT renders ms as an SRT timestamp "HH:MM:SS,mmm" with a comma
// before the millisecond field.
func formatTimestampSRT(ms int) string {
	h, m, s, msec := splitTimestamp(ms)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, msec)
}
