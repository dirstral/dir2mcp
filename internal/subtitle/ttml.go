package subtitle

import (
	"encoding/xml"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// LangCues pairs a (possibly empty) language tag with the cues authored in that
// language. A monolingual TTML file yields a single LangCues with Lang == "";
// a bilingual file (per-language <div>/<p> via xml:lang) yields one per language.
type LangCues struct {
	Lang string
	Cues []Cue
}

// ParseTTML parses a TTML (Timed Text Markup Language) document into ordered
// cues, flattening every language present into one cue stream in document order.
// Use ParseTTMLByLang when per-language separation is required (bilingual
// subtitles). Returns a *ParseError when no timed paragraph is found.
func ParseTTML(content string) ([]Cue, error) {
	groups, err := ParseTTMLByLang(content)
	if err != nil {
		return nil, err
	}
	var all []Cue
	for _, g := range groups {
		all = append(all, g.Cues...)
	}
	if len(all) == 0 {
		return nil, &ParseError{Msg: "subtitle: no TTML cues parsed"}
	}
	// ParseTTMLByLang returns language-sorted groups, so the naive flatten above
	// is language-grouped, not document (time) order. Sort by start time
	// (tie-break end time) so the flattened stream matches the doc-order contract.
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].StartMS != all[j].StartMS {
			return all[i].StartMS < all[j].StartMS
		}
		return all[i].EndMS < all[j].EndMS
	})
	reindex(all)
	return all, nil
}

// ParseTTMLByLang parses a TTML document into per-language cue sets. The
// language for a <p> is the nearest xml:lang in scope (on the <p>, an ancestor
// <div>/<body>, or the root <tt>), defaulting to "" when none is declared.
// Groups are returned sorted by language tag for determinism (empty language
// first), each with 1-based cue indices. Bilingual files thus produce distinct
// per-language transcript inputs (spec §8.6.4).
func ParseTTMLByLang(content string) ([]LangCues, error) {
	byLang := map[string][]Cue{}
	dec := xml.NewDecoder(strings.NewReader(content))
	langStack := []string{""}
	tickRate := 0 // ticks per second, if a tt@ttp:tickRate is declared

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, &ParseError{Msg: "subtitle: TTML parse error: " + err.Error()}
		}
		switch el := tok.(type) {
		case xml.StartElement:
			lang := inheritedLang(langStack, el)
			langStack = append(langStack, lang)
			if local(el.Name) == "tt" {
				if tr := attrValue(el, "tickRate"); tr != "" {
					tickRate, _ = strconv.Atoi(tr)
				}
			}
			if local(el.Name) == "p" {
				cue, ok := decodeTTMLParagraph(dec, el, tickRate)
				// The paragraph's own content was consumed by decodeTTMLParagraph
				// up to its EndElement, so pop the lang we just pushed for it.
				langStack = langStack[:len(langStack)-1]
				if ok {
					byLang[lang] = append(byLang[lang], cue)
				}
			}
		case xml.EndElement:
			if len(langStack) > 1 {
				langStack = langStack[:len(langStack)-1]
			}
		}
	}

	groups := make([]LangCues, 0, len(byLang))
	langs := make([]string, 0, len(byLang))
	for l := range byLang {
		langs = append(langs, l)
	}
	sort.Strings(langs)
	for _, l := range langs {
		cues := byLang[l]
		reindex(cues)
		groups = append(groups, LangCues{Lang: l, Cues: cues})
	}
	if len(groups) == 0 {
		return nil, &ParseError{Msg: "subtitle: no TTML cues parsed"}
	}
	return groups, nil
}

// decodeTTMLParagraph reads a <p> element's begin/end timing and inner text
// (flattening nested spans and converting <br/> to newlines), returning the cue
// and whether it carries usable timing and text. The decoder is advanced to the
// paragraph's EndElement.
func decodeTTMLParagraph(dec *xml.Decoder, start xml.StartElement, tickRate int) (Cue, bool) {
	beginMS, hasBegin := parseTTMLTime(attrValue(start, "begin"), tickRate)
	endMS, hasEnd := parseTTMLTime(attrValue(start, "end"), tickRate)

	var b strings.Builder
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch el := tok.(type) {
		case xml.StartElement:
			depth++
			if local(el.Name) == "br" {
				b.WriteByte('\n')
			}
		case xml.EndElement:
			depth--
		case xml.CharData:
			b.Write(el)
		}
	}

	text := normalizeTTMLText(b.String())
	if text == "" || !hasBegin {
		return Cue{}, false
	}
	if !hasEnd || endMS < beginMS {
		endMS = beginMS
	}
	return Cue{StartMS: beginMS, EndMS: endMS, Text: text}, true
}

// ttmlWhitespaceRe collapses runs of spaces/tabs within a line; TTML treats
// inter-element whitespace loosely and authored files often indent paragraph
// text.
var ttmlWhitespaceRe = regexp.MustCompile(`[ \t]+`)

// normalizeTTMLText trims and collapses whitespace per line while preserving
// explicit <br/>-derived newlines, yielding clean plain text for indexing.
func normalizeTTMLText(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(ttmlWhitespaceRe.ReplaceAllString(l, " "))
		if l != "" {
			out = append(out, l)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// parseTTMLTime parses a TTML time expression into milliseconds. It supports the
// two common forms: clock time "HH:MM:SS[.mmm]" / "HH:MM:SS:frames" and offset
// time with a unit suffix ("12.5s", "500ms", "1500t" ticks). Returns ok=false
// for an empty or unrecognised expression.
func parseTTMLTime(v string, tickRate int) (ms int, ok bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if strings.Contains(v, ":") {
		return parseTTMLClock(v)
	}
	return parseTTMLOffset(v, tickRate)
}

// parseTTMLClock parses "HH:MM:SS[.fraction]" (a trailing ":frames" component,
// if present, is ignored as we lack frame-rate context). Returns ok=false on a
// malformed clock value.
func parseTTMLClock(v string) (int, bool) {
	parts := strings.Split(v, ":")
	if len(parts) < 3 {
		return 0, false
	}
	hours, err1 := strconv.Atoi(parts[0])
	minutes, err2 := strconv.Atoi(parts[1])
	secField := parts[2]
	frac := 0
	if dot := strings.IndexByte(secField, '.'); dot >= 0 {
		frac, _ = strconv.Atoi(padMillis(secField[dot+1:]))
		secField = secField[:dot]
	}
	seconds, err3 := strconv.Atoi(secField)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, false
	}
	total := ((hours*60+minutes)*60+seconds)*1000 + frac
	if total < 0 {
		return 0, false
	}
	return total, true
}

// parseTTMLOffset parses an offset-time value with a unit suffix: "s"
// (seconds), "ms" (milliseconds), or "t" (ticks, scaled by tickRate when
// declared). Returns ok=false for an unsupported unit or unparseable number.
func parseTTMLOffset(v string, tickRate int) (int, bool) {
	switch {
	case strings.HasSuffix(v, "ms"):
		n, err := strconv.ParseFloat(strings.TrimSuffix(v, "ms"), 64)
		if err != nil {
			return 0, false
		}
		return int(n), true
	case strings.HasSuffix(v, "s"):
		n, err := strconv.ParseFloat(strings.TrimSuffix(v, "s"), 64)
		if err != nil {
			return 0, false
		}
		return int(n * 1000), true
	case strings.HasSuffix(v, "t"):
		n, err := strconv.ParseFloat(strings.TrimSuffix(v, "t"), 64)
		if err != nil || tickRate <= 0 {
			return 0, false
		}
		return int(n / float64(tickRate) * 1000), true
	default:
		return 0, false
	}
}

// inheritedLang returns the effective xml:lang for an element: its own xml:lang
// when present, otherwise the language inherited from the enclosing scope.
func inheritedLang(stack []string, el xml.StartElement) string {
	for _, a := range el.Attr {
		if a.Name.Local == "lang" && (a.Name.Space == "xml" || strings.HasSuffix(a.Name.Space, "XML/1998/namespace")) {
			return strings.TrimSpace(a.Value)
		}
	}
	if len(stack) > 0 {
		return stack[len(stack)-1]
	}
	return ""
}

// attrValue returns the value of the named (local) attribute, ignoring
// namespace, or "" when absent.
func attrValue(el xml.StartElement, name string) string {
	for _, a := range el.Attr {
		if a.Name.Local == name {
			return strings.TrimSpace(a.Value)
		}
	}
	return ""
}

func local(n xml.Name) string { return n.Local }

// reindex assigns contiguous 1-based Index values to cues in order.
func reindex(cues []Cue) {
	for i := range cues {
		cues[i].Index = i + 1
	}
}
