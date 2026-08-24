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
		// Wrap the pattern in a non-capturing group so the word boundaries anchor
		// the WHOLE pattern: without it a top-level alternation ("Aju|Adju") would
		// compile to \bAju|Adju\b, which RE2 reads as (\bAju)|(Adju\b) — each
		// alternative keeping only one boundary — silently allowing partial-word
		// rewrites and breaking the documented word-bounded guarantee.
		re, err := regexp.Compile(`(?i)\b(?:` + from + `)\b`)
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

// scrubSpaceRE collapses runs of spaces/tabs left behind after a phrase is
// excised. It deliberately excludes '\n' so an intentional two-line wrap survives
// the scrub (CleanCues runs after wrapping).
var scrubSpaceRE = regexp.MustCompile(`[ \t]+`)

// Scrub removes every configured phrase match from text and tidies the result.
// Unlike IsSpam (a whole-cue verdict), Scrub is used when a hallucinated phrase is
// glued to REAL speech in the same cue — it excises just the phrase and keeps the
// sentence. It is a strict no-op when no phrase matches (so the vast majority of
// cues, including their line-wrap newline, pass through byte-for-byte). Because a
// Scrub set is configured with the FULL contiguous hallucination phrase (not a
// word alternation), it never touches a legitimate mention of one of the words.
func (d *DropSet) Scrub(text string) string {
	if !d.Active() {
		return text
	}
	matched := false
	for _, re := range d.res {
		if re.MatchString(text) {
			text = re.ReplaceAllString(text, " ")
			matched = true
		}
	}
	if !matched {
		return text
	}
	// Collapse only spaces/tabs (never the wrap newline), then strip connective
	// punctuation orphaned at either end by the excision (a leading ", " or a
	// dangling "—"), keeping sentence-final "." "!" "?" so the surviving sentence
	// is not de-punctuated.
	text = scrubSpaceRE.ReplaceAllString(text, " ")
	text = strings.TrimSpace(text)
	return strings.TrimSpace(strings.Trim(text, ",;:—- "))
}

// CleanOptions configures the export-time cue-cleaning pass (port of the pilot's
// clean_srt.py). All fields are additive and off by default, so a zero
// CleanOptions leaves cues unchanged. It is applied AFTER WordFilter on export,
// so filter_words removal and this cleanup compose.
type CleanOptions struct {
	// DropURLs drops any cue whose text looks like a hallucinated URL / domain /
	// credit line (whisper emits these over silence or music).
	DropURLs bool
	// Script drops any cue whose letters lie entirely outside the track's
	// expected script (wrong-script STT gibberish hallucinated over non-speech;
	// see ScriptGuard). Cues with any digit or any expected-script letter
	// survive. Nil/inactive = no drops.
	Script *ScriptGuard
	// Drop drops any cue composed entirely of configured hallucination phrases
	// (whisper keyword-spam over non-speech). Nil/inactive = no drops.
	Drop *DropSet
	// Scrub excises configured hallucination phrases from cues that ALSO carry
	// real speech (the phrase leaked into the same cue), keeping the sentence.
	// Configure with the full contiguous phrase. Nil/inactive = no scrubbing.
	Scrub *DropSet
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
	return o.DropURLs || o.Script.Active() || o.Drop.Active() || o.Scrub.Active() || o.CollapseRepeats >= 2 || o.Glossary.Active()
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
		if opts.Script.IsForeign(text) {
			continue
		}
		if opts.Drop.IsSpam(text) {
			continue
		}
		// Excise a hallucinated phrase that leaked into an otherwise-real cue,
		// then re-check for emptiness (a cue that was ALL phrase plus punctuation
		// scrubs to nothing and is dropped).
		if opts.Scrub.Active() {
			text = opts.Scrub.Scrub(text)
			if text == "" {
				continue
			}
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
