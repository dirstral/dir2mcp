package ingest

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Proper nouns are the weak point of LLM translation: the model REGENERATES a name
// rather than transliterating it, so "университета имени Сеченова" comes back as
// "Sheremetyev University". Named-entity accuracy is a common acceptance criterion for
// broadcast transcripts, and there is no reviewer downstream to catch it.
//
// The fix is prevention, not repair. Repairing after the fact requires knowing which
// English word was supposed to be which source name -- ambiguous, and a wrong swap is
// worse than a visible error. Instead we hand the model the spellings up front.
//
// This is safe in the Cyrillic->English direction specifically: Russian capitalises
// mid-sentence words almost exclusively for proper nouns, so detection is reliable.
// The reverse (guessing which English capital is a mangled name) is not, because
// English capitalises ordinary words mid-sentence constantly.

// cyrTranslit is BGN/PCGN-style, matching how Russian names are conventionally
// rendered in English-language copy (Сеченов->Sechenov, Щербак->Shcherbak).
// Order matters: multi-rune outputs are applied before single-letter ones.
var cyrTranslit = []struct{ from, to string }{
	{"щ", "shch"}, {"ш", "sh"}, {"ч", "ch"}, {"ж", "zh"}, {"ц", "ts"},
	{"ю", "yu"}, {"я", "ya"}, {"х", "kh"}, {"ё", "e"}, {"э", "e"},
	{"ъ", ""}, {"ь", ""},
	{"а", "a"}, {"б", "b"}, {"в", "v"}, {"г", "g"}, {"д", "d"}, {"е", "e"},
	{"з", "z"}, {"и", "i"}, {"й", "y"}, {"к", "k"}, {"л", "l"}, {"м", "m"},
	{"н", "n"}, {"о", "o"}, {"п", "p"}, {"р", "r"}, {"с", "s"}, {"т", "t"},
	{"у", "u"}, {"ф", "f"}, {"ы", "y"},
	// Ukrainian letters, so a mixed-language corpus does not produce mojibake.
	{"і", "i"}, {"ї", "yi"}, {"є", "ye"}, {"ґ", "g"},
}

// translitCyrillic renders one Cyrillic word in Latin script, capitalised.
func translitCyrillic(word string) string {
	w := strings.ToLower(word)
	for _, r := range cyrTranslit {
		w = strings.ReplaceAll(w, r.from, r.to)
	}
	if w == "" {
		return ""
	}
	return strings.ToUpper(w[:1]) + w[1:]
}

// genitiveTrigger matches the "имени X" / "им. X" construction, which forces X into
// the genitive. The English form is then the nominative: "имени Сеченова" is
// "Sechenov", not "Sechenova" -- the single most visible case, since it names
// institutions.
//
// Deliberately NOT anchored with \b: Go's RE2 defines word boundaries over ASCII word
// characters only, so \b never matches against a Cyrillic letter and the pattern would
// silently never fire. Use an explicit start-or-space alternation instead.
var genitiveTrigger = regexp.MustCompile(`(?i)(^|\s)(имени|им\.)\s*$`)

// properNounRE matches a capitalised Cyrillic word of three or more letters.
var properNounRE = regexp.MustCompile(`[А-ЯЁЇІЄҐ][а-яёїієґ]{2,}`)

// maxNameHints bounds the prompt growth on a name-dense line. Six covers the
// realistic worst case (a list of officials) without crowding out the text itself.
const maxNameHints = 6

// nameHints returns "<source> -> <english>" spelling hints for the proper nouns in a
// line of Cyrillic text, in first-appearance order. A capital that opens a sentence is
// skipped: there it signals sentence case rather than a name, so treating it as one
// would feed the model a hint for an ordinary word. Returns nil when there is nothing
// to pin, so the prompt is unchanged for the vast majority of lines.
func nameHints(text string) []string {
	var hints []string
	seen := make(map[string]bool)
	for _, loc := range properNounRE.FindAllStringIndex(text, -1) {
		word := text[loc[0]:loc[1]]
		before := strings.TrimRight(text[:loc[0]], " \t ")
		// sentence-initial (or line-initial) capitals are just sentence case
		if before == "" {
			continue
		}
		if last := before[len(before)-1]; last == '.' || last == '!' || last == '?' {
			continue
		}
		if seen[word] {
			continue
		}
		seen[word] = true

		english := translitCyrillic(word)
		if genitiveTrigger.MatchString(before) {
			if stem := strings.TrimSuffix(word, "а"); stem != word && len([]rune(stem)) >= 3 {
				english = translitCyrillic(stem)
			}
		}
		if english == "" {
			continue
		}
		hints = append(hints, word+" -> "+english)
		if len(hints) == maxNameHints {
			break
		}
	}
	return hints
}

// hasCyrillic reports whether text contains any Cyrillic letter, so the hint pass is
// skipped entirely for source languages it does not apply to.
func hasCyrillic(text string) bool {
	for _, r := range text {
		if unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}

// sortedHints is used only by tests, to assert on hint sets without depending on
// first-appearance order.
func sortedHints(h []string) []string {
	out := append([]string(nil), h...)
	sort.Strings(out)
	return out
}
