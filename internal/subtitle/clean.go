package subtitle

import (
	"fmt"
	"regexp"
	"strings"
)

// Glossary applies editorial term replacements to cue text: whole-word,
// case-insensitive substitutions that fix correct-but-misspelled entities (for
// example a transliteration "Ajubei" -> "Adzhubei"). Unlike WordFilter, which
// DELETES configured phrases, a glossary REWRITES text and never drops a cue.
// Rules come entirely from configuration (media.subtitles.glossary); an empty
// glossary is a no-op. Each rule's match side is a regular expression (so a
// single entry can absorb spelling variants, e.g. "Aju?bei"), wrapped in ASCII
// word boundaries and matched case-insensitively.
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
// skipped. The pattern is compiled as a case-insensitive, ASCII-word-bounded
// regular expression, so an invalid pattern is a configuration error (returned
// here) rather than a silent no-op. A nil/empty list yields an inactive glossary.
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
		re, err := regexp.Compile(`(?i)\b` + from + `\b`)
		if err != nil {
			return nil, fmt.Errorf("glossary entry %q: invalid match pattern: %w", e, err)
		}
		g.rules = append(g.rules, glossaryRule{re: re, repl: to})
	}
	return g, nil
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
		text = r.re.ReplaceAllLiteralString(text, r.repl)
	}
	return text
}

// CleanOptions configures the export-time cue-cleaning pass (port of the pilot's
// clean_srt.py). All fields are additive and off by default, so a zero
// CleanOptions leaves cues unchanged. It is applied AFTER WordFilter on export,
// so filter_words removal and this cleanup compose.
type CleanOptions struct {
	// DropURLs drops any cue whose text looks like a hallucinated URL / domain /
	// credit line (whisper emits these over silence or music).
	DropURLs bool
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
	return o.DropURLs || o.CollapseRepeats >= 2 || o.Glossary.Active()
}

// urlCueRE matches a hallucinated URL / bare domain in cue text. It mirrors the
// pilot's clean_srt URL rule: an http(s):// or www. prefix, or a token ending in
// a common TLD. ASCII word boundaries are sufficient for URLs.
var urlCueRE = regexp.MustCompile(`(?i)(https?://|www\.|\b\S+\.(com|org|net|ru|ua)\b)`)

// CleanCues applies the configured cleanup passes to cues in order: drop empty
// cues, drop URL/credit hallucinations, collapse identical-consecutive runs, and
// rewrite text via the glossary. Surviving cues are re-indexed gap-free (as
// FilterCues does). Timing is preserved verbatim. A zero/empty CleanOptions
// returns cues unchanged.
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
			text = opts.Glossary.Apply(text)
		}
		cue.Index = len(out) + 1
		cue.Text = text
		out = append(out, cue)
	}
	return out
}
