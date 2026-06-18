package subtitle

import (
	"encoding/xml"
	"fmt"
	"sort"
	"strings"
)

// BilingualCue is a TTML cue carrying one or two language-tagged text runs over
// a single time region (SPEC §8.6.10). PrimaryText/PrimaryLang is always set;
// SecondaryText/SecondaryLang are set when a secondary-language segment aligned
// to this cue's time region within tolerance. Both runs map back to the SAME
// transcript segment span — StartMS/EndMS — so a rendered cue stays traceable to
// its chunk timing (§8.6.1).
type BilingualCue struct {
	StartMS int
	EndMS   int

	PrimaryLang string
	PrimaryText string
	Speaker     string

	SecondaryLang string
	SecondaryText string
}

// DefaultAlignToleranceMS mirrors the spec/config default
// (media.subtitles.ttml.align_tolerance_ms, SPEC §8.6.10) so callers that do not
// thread a configured tolerance still align deterministically.
const DefaultAlignToleranceMS = 2500

// AlignBilingual aligns secondary-language cues onto primary-language cues for
// bilingual TTML export (SPEC §8.6.10). The primary cues define the cue set and
// their time regions are authoritative; each secondary cue is merged into the
// primary cue whose start is closest to the secondary's start within
// toleranceMS. A secondary cue with no primary cue in tolerance is emitted as
// its own secondary-only cue (never dropped). Alignment is deterministic: inputs
// are sorted by (start,end), the nearest-start primary wins, ties break to the
// earlier primary, and a primary already carrying a secondary run keeps its
// first match so a later secondary falls through to its own cue.
//
// A non-positive toleranceMS falls back to DefaultAlignToleranceMS. Passing nil
// secondary cues yields the primaries rendered as monolingual bilingual cues.
func AlignBilingual(primary, secondary []Cue, primaryLang, secondaryLang string, toleranceMS int) []BilingualCue {
	if toleranceMS <= 0 {
		toleranceMS = DefaultAlignToleranceMS
	}

	prim := sortedByStart(primary)
	sec := sortedByStart(secondary)

	out := make([]BilingualCue, len(prim))
	assigned := make([]bool, len(prim))
	for i, p := range prim {
		out[i] = BilingualCue{
			StartMS:     p.StartMS,
			EndMS:       p.EndMS,
			PrimaryLang: primaryLang,
			PrimaryText: strings.TrimSpace(p.Text),
			Speaker:     strings.TrimSpace(p.Speaker),
		}
	}

	var orphans []BilingualCue
	for _, s := range sec {
		idx := nearestPrimary(prim, assigned, s.StartMS, toleranceMS)
		if idx < 0 {
			// No primary within tolerance (or all candidates already carry a
			// secondary run): emit the secondary as its own cue rather than drop
			// it (SPEC §8.6.10).
			orphans = append(orphans, BilingualCue{
				StartMS:       s.StartMS,
				EndMS:         s.EndMS,
				PrimaryLang:   secondaryLang,
				PrimaryText:   strings.TrimSpace(s.Text),
				Speaker:       strings.TrimSpace(s.Speaker),
				SecondaryLang: "",
			})
			continue
		}
		out[idx].SecondaryLang = secondaryLang
		out[idx].SecondaryText = strings.TrimSpace(s.Text)
		assigned[idx] = true
	}

	merged := append(out, orphans...)
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].StartMS != merged[j].StartMS {
			return merged[i].StartMS < merged[j].StartMS
		}
		return merged[i].EndMS < merged[j].EndMS
	})
	return merged
}

// nearestPrimary returns the index of the unassigned primary cue whose start is
// closest to startMS within toleranceMS, or -1 when none qualifies. Ties (equal
// distance) resolve to the earlier (lower-index) primary for determinism.
func nearestPrimary(prim []Cue, assigned []bool, startMS, toleranceMS int) int {
	best := -1
	bestDelta := toleranceMS + 1
	for i, p := range prim {
		if assigned[i] {
			continue
		}
		delta := startMS - p.StartMS
		if delta < 0 {
			delta = -delta
		}
		if delta <= toleranceMS && delta < bestDelta {
			best = i
			bestDelta = delta
		}
	}
	return best
}

// sortedByStart returns a copy of cues ordered by (start, end) so alignment and
// rendering are deterministic regardless of input order. Empty-text cues are
// dropped (nothing to display).
func sortedByStart(cues []Cue) []Cue {
	out := make([]Cue, 0, len(cues))
	for _, c := range cues {
		if strings.TrimSpace(c.Text) == "" {
			continue
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StartMS != out[j].StartMS {
			return out[i].StartMS < out[j].StartMS
		}
		return out[i].EndMS < out[j].EndMS
	})
	return out
}

// MonolingualBilingualCues wraps a single language's cues as BilingualCues with
// no secondary run, the input shape for monolingual TTML export.
func MonolingualBilingualCues(cues []Cue, lang string) []BilingualCue {
	src := sortedByStart(cues)
	out := make([]BilingualCue, 0, len(src))
	for _, c := range src {
		out = append(out, BilingualCue{
			StartMS:     c.StartMS,
			EndMS:       c.EndMS,
			PrimaryLang: lang,
			PrimaryText: strings.TrimSpace(c.Text),
			Speaker:     strings.TrimSpace(c.Speaker),
		})
	}
	return out
}

// RenderTTML serializes bilingual cues as a TTML (Timed Text Markup Language)
// document. Each cue becomes a <p> with begin/end timing; its primary and (when
// present) secondary text runs are emitted as language-tagged <span xml:lang>
// children over the SAME time region (SPEC §8.6.10), so both languages remain
// traceable to the cue's transcript span. Speaker markup is intentionally not
// emitted here: TTML voice markup is optional (§8.6.8) and omitting it keeps the
// text runs clean and the output portable (fail open).
//
// docLang is the document-level xml:lang (typically the primary language).
// Rendering is deterministic for a given cue slice.
func RenderTTML(cues []BilingualCue, docLang string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<tt xmlns="http://www.w3.org/ns/ttml" xml:lang="`)
	b.WriteString(escapeAttr(docLang))
	b.WriteString("\">\n")
	b.WriteString("  <body>\n")
	b.WriteString("    <div>\n")
	for _, c := range cues {
		if strings.TrimSpace(c.PrimaryText) == "" && strings.TrimSpace(c.SecondaryText) == "" {
			continue
		}
		fmt.Fprintf(&b, "      <p begin=%q end=%q>\n",
			formatTimestampTTML(c.StartMS), formatTimestampTTML(c.EndMS))
		writeTTMLRun(&b, c.PrimaryLang, c.PrimaryText)
		if strings.TrimSpace(c.SecondaryText) != "" {
			writeTTMLRun(&b, c.SecondaryLang, c.SecondaryText)
		}
		b.WriteString("      </p>\n")
	}
	b.WriteString("    </div>\n")
	b.WriteString("  </body>\n")
	b.WriteString("</tt>\n")
	return b.String()
}

// writeTTMLRun writes one language-tagged text run as a <span xml:lang="..">
// child. Newlines in the text become <br/> so multi-line cues render. Text and
// the language tag are XML-escaped. A run with an empty language tag omits the
// xml:lang attribute (the document-level lang applies).
func writeTTMLRun(b *strings.Builder, lang, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if lang = strings.TrimSpace(lang); lang != "" {
		fmt.Fprintf(b, "        <span xml:lang=%q>", lang)
	} else {
		b.WriteString("        <span>")
	}
	b.WriteString(escapeTTMLText(text))
	b.WriteString("</span>\n")
}

// escapeTTMLText XML-escapes cue text and converts newlines to <br/> so a
// multi-line cue renders correctly inside a <span>.
func escapeTTMLText(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = escapeText(l)
	}
	return strings.Join(lines, "<br/>")
}

// escapeText XML-escapes character data.
func escapeText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// escapeAttr XML-escapes a value destined for a double-quoted attribute.
func escapeAttr(s string) string {
	return escapeText(s)
}

// formatTimestampTTML renders ms as a TTML clock-time "HH:MM:SS.mmm".
func formatTimestampTTML(ms int) string {
	h, m, s, msec := splitTimestamp(ms)
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, msec)
}
