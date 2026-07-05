package subtitle

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Glossary applies editorial term replacements to cue text: whole-word,
// case-insensitive substitutions that fix correct-but-misspelled entities (for
// example a transliteration "Ajubei" -> "Adzhubei"). Unlike WordFilter, which
// DELETES configured phrases, a glossary REWRITES text and never drops a cue.
// Rules come entirely from configuration (media.subtitles.glossary); an empty
// glossary is a no-op. Each rule's match side is a regular expression (so a
// single entry can absorb spelling variants, e.g. "Aju?bei"), matched
// case-insensitively and constrained to whole words. Word boundaries are
// Unicode-aware (letter/digit/underscore in any script), so rules apply to
// Cyrillic, CJK, etc. — not only ASCII/Latin transliterations.
type Glossary struct {
	rules []glossaryRule
}

type glossaryRule struct {
	re   *regexp.Regexp
	repl string
}

// glossarySep separates the match pattern from its replacement in a configured
// glossary entry, e.g. "Aju?bei=>Adzhubei".
const glossarySep = "=>"

// NewGlossary compiles glossary entries of the form "pattern=>replacement".
// Whitespace around the entry and each side is trimmed; blank entries are
// skipped. The pattern is compiled case-insensitively, so an invalid pattern is a
// configuration error (returned here) rather than a silent no-op. Whole-word
// matching is enforced at apply time by Unicode-aware boundary checks (see Apply),
// not by RE2's ASCII-only \b — otherwise glossary rules would silently fail on any
// non-Latin script. A nil/empty list yields an inactive glossary.
func NewGlossary(entries []string) (*Glossary, error) {
	g := &Glossary{}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		i := strings.Index(e, glossarySep)
		if i < 0 {
			return nil, fmt.Errorf("glossary entry %q must be of the form pattern%sreplacement", e, glossarySep)
		}
		from := strings.TrimSpace(e[:i])
		to := strings.TrimSpace(e[i+len(glossarySep):])
		if from == "" {
			return nil, fmt.Errorf("glossary entry %q has an empty match pattern", e)
		}
		// Wrap the pattern in a non-capturing group so Apply's boundary check
		// constrains the WHOLE pattern: without it a top-level alternation
		// ("Aju|Adju") would bind the surrounding context to only one alternative
		// and the boundary check would police just part of the match, silently
		// allowing partial-word rewrites and breaking the whole-word guarantee.
		re, err := regexp.Compile(`(?i)(?:` + from + `)`)
		if err != nil {
			return nil, fmt.Errorf("glossary entry %q: invalid match pattern: %w", e, err)
		}
		g.rules = append(g.rules, glossaryRule{re: re, repl: to})
	}
	return g, nil
}

// isWordRune reports whether r is a word constituent for whole-word matching:
// any Unicode letter or digit, or underscore. This is the Unicode-aware analogue
// of RE2's ASCII-only \w, so word boundaries hold across Cyrillic, CJK, etc.
func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// replaceWholeWord replaces every non-overlapping match of re in text with repl,
// but only where the match is whole-word: the runes immediately flanking the
// match must sit on a word boundary (a \b-equivalent transition between a word
// and a non-word rune), evaluated with Unicode-aware wordness. Non-whole-word
// matches (e.g. "Aju" inside "Ajubeyond") are left untouched. repl is inserted
// verbatim (no capture-group expansion).
func replaceWholeWord(re *regexp.Regexp, text, repl string) string {
	locs := re.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, loc := range locs {
		s, e := loc[0], loc[1]
		leftOK := true
		if s > 0 {
			before, _ := utf8.DecodeLastRuneInString(text[:s])
			first, _ := utf8.DecodeRuneInString(text[s:e])
			leftOK = isWordRune(before) != isWordRune(first)
		}
		rightOK := true
		if e < len(text) {
			after, _ := utf8.DecodeRuneInString(text[e:])
			lastR, _ := utf8.DecodeLastRuneInString(text[s:e])
			rightOK = isWordRune(lastR) != isWordRune(after)
		}
		if leftOK && rightOK {
			b.WriteString(text[last:s])
			b.WriteString(repl)
			last = e
		}
	}
	b.WriteString(text[last:])
	return b.String()
}

// Active reports whether the glossary has any rules to apply. When false, Apply
// is the identity and callers can skip it.
func (g *Glossary) Active() bool {
	return g != nil && len(g.rules) > 0
}

// Apply rewrites text by each glossary rule in configuration order. Replacements
// are literal (a '$' in a replacement is not treated as a capture reference), so
// the replacement text appears verbatim. Rules are applied in order; an earlier
// rewrite can feed a later rule, which is deterministic.
func (g *Glossary) Apply(text string) string {
	if !g.Active() {
		return text
	}
	for _, r := range g.rules {
		text = replaceWholeWord(r.re, text, r.repl)
	}
	return text
}

// DropSet drops a whole cue when its text is composed ENTIRELY of configured
// hallucination phrases (plus punctuation/whitespace). Whisper spams a fixed
// phrase over silence/music/B-roll — e.g. "Донбасс, Крым, Украина, НАТО" — and
// broadcast segmentation fragments it across cue boundaries, so neither the
// URL-drop nor the consecutive-identical CollapseRepeats pass catches it. A drop
// rule removes such cues WITHOUT harming real speech that merely mentions one of
// the words: a cue survives as long as any non-phrase letters/digits remain
// after the phrase matches are stripped (so "Крым сегодня" is kept, "Крым, НАТО"
// is dropped). Rules come entirely from configuration
// (media.subtitles.drop_phrases); an empty set is a no-op. Each entry is a
// case-insensitive regular expression matched anywhere in the cue.
type DropSet struct {
	res []*regexp.Regexp
}

// dropResidualRE matches any letter or digit; a cue with none remaining after
// phrase-stripping is treated as pure hallucination and dropped.
var dropResidualRE = regexp.MustCompile(`[\p{L}\p{N}]`)

// NewDropSet compiles drop-phrase patterns. Whitespace around each entry is
// trimmed and blank entries are skipped. Each pattern is compiled
// case-insensitively, so an invalid pattern is a configuration error (returned
// here) rather than a silent no-op. A nil/empty list yields an inactive set.
func NewDropSet(entries []string) (*DropSet, error) {
	d := &DropSet{}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		re, err := regexp.Compile(`(?i)` + e)
		if err != nil {
			return nil, fmt.Errorf("drop phrase %q: invalid pattern: %w", e, err)
		}
		d.res = append(d.res, re)
	}
	return d, nil
}

// Active reports whether the set has any rules. When false, IsSpam is always
// false and callers can skip it.
func (d *DropSet) Active() bool {
	return d != nil && len(d.res) > 0
}

// IsSpam reports whether text is composed entirely of configured drop phrases:
// removing every phrase match leaves no letters or digits behind. An inactive
// set never reports spam.
func (d *DropSet) IsSpam(text string) bool {
	if !d.Active() {
		return false
	}
	stripped := text
	for _, re := range d.res {
		stripped = re.ReplaceAllString(stripped, " ")
	}
	return !dropResidualRE.MatchString(stripped)
}

// CleanOptions configures the export-time cue-cleaning pass (port of the pilot's
// clean_srt.py). All fields are additive and off by default, so a zero
// CleanOptions leaves cues unchanged. It is applied AFTER WordFilter on export,
// so filter_words removal and this cleanup compose.
type CleanOptions struct {
	// DropURLs drops any cue whose text looks like a hallucinated URL / domain /
	// credit line (whisper emits these over silence or music).
	DropURLs bool
	// Drop drops any cue composed entirely of configured hallucination phrases
	// (whisper keyword-spam over non-speech). Nil/inactive = no drops.
	Drop *DropSet
	// CollapseRepeats drops the Nth-and-later cue in a run of identical
	// consecutive cues (a whisper repetition-collapse artifact), keeping the
	// first N-1. A value < 2 disables the pass, so legitimate short repeats
	// ("Yes." "Yes.") survive — only long identical runs are collapsed.
	CollapseRepeats int
	// Glossary applies editorial term replacements. Nil/inactive = no rewrites.
	Glossary *Glossary
}

// Active reports whether any cleaning is configured. When false, CleanCues
// returns its input unchanged so the empty-config export path is a no-op.
func (o CleanOptions) Active() bool {
	return o.DropURLs || o.Drop.Active() || o.CollapseRepeats >= 2 || o.Glossary.Active()
}

// urlCueRE matches a hallucinated URL / bare domain in cue text (whisper emits
// these over silence or music). It is deliberately locale-agnostic: an http(s)://
// or www. prefix, or a hostname-shaped label ([\w-]+) followed by a generic
// dotted TLD (\.[a-z]{2,24}) at an ASCII word boundary. Requiring a letter
// immediately after the dot and a hostname-shaped label keeps ordinary prose with
// periods out ("end. Next", "3.14", "Mr. Smith" — the dot is followed by a space
// or digits, not a TLD), while catching any real domain (.io, .tv, .co, .de, .uk,
// .gov, .info, …) without hardcoding country-specific ccTLDs. RE2 word boundaries
// and \w are ASCII-only, which is fine here: real domains are ASCII, and prose in
// any script (Cyrillic, CJK, …) whose only dots are sentence periods never has a
// [\w-] label immediately abutting a letter-led TLD.
var urlCueRE = regexp.MustCompile(`(?i)(https?://|www\.|\b[\w-]+\.[a-z]{2,24}\b)`)

// CleanCues applies the configured cleanup passes to cues in order: drop empty
// cues, drop URL/credit hallucinations, collapse identical-consecutive runs, and
// rewrite text via the glossary (dropping any cue the rewrite empties). Surviving
// cues are re-indexed gap-free (as FilterCues does). Timing is preserved verbatim.
// A zero/empty CleanOptions returns cues unchanged.
func CleanCues(cues []Cue, opts CleanOptions) []Cue {
	if !opts.Active() {
		return cues
	}
	out := make([]Cue, 0, len(cues))
	var runText string
	var runLen int
	for _, cue := range cues {
		text := strings.TrimSpace(cue.Text)
		if text == "" {
			continue
		}
		if opts.DropURLs && urlCueRE.MatchString(text) {
			continue
		}
		if opts.Drop.IsSpam(text) {
			continue
		}
		// Repetition-collapse compares the pre-glossary text so a rewrite cannot
		// mask a collapse run. The comparison anchor (runText) updates only when
		// the text changes, so a run of K identical cues keeps the first
		// CollapseRepeats-1 and drops the rest.
		if opts.CollapseRepeats >= 2 {
			if text == runText {
				runLen++
				if runLen >= opts.CollapseRepeats {
					continue
				}
			} else {
				runText = text
				runLen = 1
			}
		}
		if opts.Glossary.Active() {
			// Re-trim after the rewrite: a glossary rule may empty a cue (an empty
			// REPLACEMENT deletes text, e.g. "foo=>") or leave stray leading/
			// trailing whitespace. Trim it and drop a now-empty cue like the
			// entry-time empty/URL passes do. This runs AFTER the collapse
			// bookkeeping (which compares pre-glossary text), so dropping here
			// never corrupts the run counter; survivors are re-indexed gap-free.
			text = strings.TrimSpace(opts.Glossary.Apply(text))
			if text == "" {
				continue
			}
		}
		cue.Index = len(out) + 1
		cue.Text = text
		out = append(out, cue)
	}
	return out
}
