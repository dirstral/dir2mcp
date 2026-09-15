package subtitle

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// ScriptGuard drops a whole cue whose letters lie ENTIRELY outside the track's
// expected script. CTC and whisper-family STT models emit wrong-script gibberish
// over non-speech audio (B-roll, music, crosstalk): Latin vowel-run junk such as
// "Elola alolo." or "Iiiiiiii." inside a Cyrillic-language track. A cue that
// contains letters but not one letter of the expected script is machine output
// by construction (measured on pilot data: every such cue was junk), so no
// per-phrase configuration is needed, unlike DropSet, which matches known
// hallucination phrases. Two safeguards keep real content: a cue that contains
// ANY digit is never dropped (protects "COVID-19" and similar), and a cue that
// mixes scripts survives (a Latin brand name inside a Cyrillic sentence keeps
// the cue). Configured via media.subtitles.expect_script (SPEC §8.6.3); an
// empty name yields an inactive guard.
//
// The guard is script-level, not language-level: it ships no phrase list and
// applies the same way to every track whose script is in scriptTables.
type ScriptGuard struct {
	table *unicode.RangeTable
}

// scriptTables maps a recognized (lowercase) script name to its Unicode range
// table. Extending the guard to another script is a one-line addition here.
var scriptTables = map[string]*unicode.RangeTable{
	"cyrillic":   unicode.Cyrillic,
	"latin":      unicode.Latin,
	"greek":      unicode.Greek,
	"arabic":     unicode.Arabic,
	"hebrew":     unicode.Hebrew,
	"georgian":   unicode.Georgian,
	"armenian":   unicode.Armenian,
	"han":        unicode.Han,
	"hangul":     unicode.Hangul,
	"devanagari": unicode.Devanagari,
}

// NewScriptGuard resolves a configured script name to its Unicode range table.
// The name is trimmed and lowercased; an empty name yields an inactive guard
// (the feature is off by default). An unknown name is a configuration error
// that lists the valid names. It is returned, not swallowed: a silent no-op
// would leave the operator believing the filter is on.
func NewScriptGuard(name string) (*ScriptGuard, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return &ScriptGuard{}, nil
	}
	table, ok := scriptTables[name]
	if !ok {
		names := make([]string, 0, len(scriptTables))
		for n := range scriptTables {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("unknown script %q: valid names are %s", name, strings.Join(names, ", "))
	}
	return &ScriptGuard{table: table}, nil
}

// Active reports whether an expected script is configured. When false,
// IsForeign is always false and callers can skip it.
func (g *ScriptGuard) Active() bool {
	return g != nil && g.table != nil
}

// IsForeign reports whether text is wrong-script gibberish: it contains at
// least one letter and NOT ONE of its letters belongs to the expected script.
// A cue that contains any digit is never foreign (numeric content such as
// "COVID-19" is kept whatever its script), and a single expected-script letter
// clears the whole cue (mixed-script cues survive). An inactive guard never
// reports foreign.
func (g *ScriptGuard) IsForeign(text string) bool {
	if !g.Active() {
		return false
	}
	hasLetter := false
	for _, r := range text {
		if unicode.IsDigit(r) {
			return false
		}
		if !unicode.IsLetter(r) {
			continue
		}
		if unicode.Is(g.table, r) {
			return false
		}
		hasLetter = true
	}
	return hasLetter
}
