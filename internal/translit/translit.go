package translit

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
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
// This is safe in the Cyrillic->English direction specifically: the source languages
// here capitalise mid-sentence words almost exclusively for proper nouns, so detection
// is reliable. The reverse (guessing which English capital is a mangled name) is not,
// because English capitalises ordinary words mid-sentence constantly.
//
// Everything that differs BETWEEN source languages lives in a convention
// (russian.go, ukrainian.go); this file holds what they share. A language with no
// convention gets no hints at all: applying one language's table to another gives
// a confidently wrong spelling, and the prompt says "use exactly these".

// replacement is one ordered source-ending/rendering pair.
type replacement struct{ from, to string }

// convention is one source language's transliteration system and name grammar.
// Every field is language-specific on purpose: a shared default would be one
// language's rule silently applied to another, which is the bug this type exists
// to prevent.
type convention struct {
	// name is the short tag recorded in the translate derivation identity, so a
	// cached translation produced under one convention cannot be served after
	// the source language changes to another.
	name string
	// transliterate renders one LOWER-CASED word; the caller capitalises.
	transliterate func(word string) string
	// obliqueForms map an unambiguous oblique ending to its nominative.
	obliqueForms []replacement
	// nominativeSuffixes mark a form that already needs no adjustment, and prove
	// that a stripped genitive -а was an inflection.
	nominativeSuffixes []string
	// triggerForms restore a genitive that only the genitiveTrigger construction
	// makes unambiguous. Checked after the strip; see genitiveStem.
	triggerForms []replacement
	// exonymStems are source words with an established English form.
	exonymStems []string
	// vowelRunes decide whether a word is consonant-final, hence nominative.
	vowelRunes string
	// caseEndingRunes are the letters a case ending is built from.
	caseEndingRunes string
	// apostropheIsInternal is true where an apostrophe is a phonetic separator
	// inside one word rather than a compound boundary.
	apostropheIsInternal bool
	// genitiveTrigger matches the construction that forces the next word into
	// the genitive.
	genitiveTrigger *regexp.Regexp
}

// conventionFor resolves a BCP-47 source-language tag to its convention. The
// region subtag is dropped ("ru-RU" is Russian), and an unknown or empty tag
// resolves to nothing: unknown is first class (SPEC §8.8) and is never assumed
// to be a language we have a table for.
func conventionFor(lang string) (*convention, bool) {
	l := strings.ToLower(strings.TrimSpace(lang))
	if i := strings.IndexAny(l, "-_"); i >= 0 {
		l = l[:i]
	}
	switch l {
	case "ru", "rus":
		return russian, true
	case "uk", "ukr":
		return ukrainian, true
	}
	return nil, false
}

// SupportsSource reports whether name hints exist for a source language. Hints
// are gated on it because a table is specific to the language it was written
// for: the Russian table applied to a Ukrainian name yields Володимир ->
// Volodimir and Гриценко -> Gritsenko, where the Ukrainian rules give Volodymyr
// and Hrytsenko. A pinned wrong spelling is worse than no hint.
func SupportsSource(lang string) bool {
	_, ok := conventionFor(lang)
	return ok
}

// SourceConvention returns the short name of the convention a source language
// resolves to, or "" when there is none. It is folded into the translate
// derivation identity: two languages that BOTH have hints produce different
// spellings for the same text, so a cached translation must not be served across
// them.
func SourceConvention(lang string) string {
	conv, ok := conventionFor(lang)
	if !ok {
		return ""
	}
	return conv.name
}

// capitalise upper-cases the first RUNE. Byte-slicing would corrupt any name whose
// first character survives transliteration as a multi-byte rune.
func capitalise(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// Transliterate renders one Cyrillic word in Latin script, capitalised, under the
// convention of sourceLang. A language with no convention returns "".
func Transliterate(word, sourceLang string) string {
	conv, ok := conventionFor(sourceLang)
	if !ok {
		return ""
	}
	return capitalise(conv.transliterate(strings.ToLower(word)))
}

// nominalize returns the nominative form of a personal name plus whether the
// conversion is trustworthy. Anything ambiguous reports false so no hint is emitted --
// a wrong pin is worse than no pin, since the model may well have had it right.
func nominalize(word string, conv *convention) (string, bool) {
	lw := strings.ToLower(word)
	for _, of := range conv.obliqueForms {
		if strings.HasSuffix(lw, of.from) {
			stem := []rune(word)[:len([]rune(word))-len([]rune(of.from))]
			if len(stem) < 3 {
				return "", false
			}
			return string(stem) + of.to, true
		}
	}
	for _, nom := range conv.nominativeSuffixes {
		if strings.HasSuffix(lw, nom) {
			return word, true
		}
	}
	r := []rune(lw)
	if !strings.ContainsRune(conv.vowelRunes, r[len(r)-1]) {
		return word, true // consonant-final: already nominative
	}
	return "", false
}

// hasExonym reports whether word already has a conventional English rendering.
// Pinning a literal transliteration for these makes the translation WORSE --
// measured: "России" pinned to "Rossii" turned "in Russia" into "in Rossii", and
// "Бог" pinned to "Bog" turned "May God grant" into "May Bog indeed enable".
//
// The stem must be followed only by a plausible case ending. A bare prefix test
// swallows real surnames that merely start the same way -- "вен" ate Венедиктов,
// "литв" ate Литвиненко, "бог" ate Богданов -- silently removing the feature for names
// it exists to fix.
func hasExonym(word string, conv *convention) bool {
	lw := strings.ToLower(word)
	for _, stem := range conv.exonymStems {
		if !strings.HasPrefix(lw, stem) {
			continue
		}
		rest := []rune(strings.TrimPrefix(lw, stem))
		if len(rest) > 3 {
			continue
		}
		ok := true
		for _, r := range rest {
			if !strings.ContainsRune(conv.caseEndingRunes, r) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// properNounRE matches a capitalised Cyrillic word of three or more letters. The
// lowercase class must cover every alphabet in the corpus: omitting the Kazakh/Kyrgyz
// letters made the match stop at the first one, pinning a TRUNCATED name
// ("Айдарқұла" -> "Айдар").
var properNounRE = regexp.MustCompile(`[А-ЯЁЄІЇҐӘҒҚҢӨҰҮҺ][а-яёєіїґәғқңөұүһ]{2,}(?:['’][а-яёєіїґәғқңөұүһ]+|-[А-ЯЁЄІЇҐӘҒҚҢӨҰҮҺа-яёєіїґәғқңөұүһ][а-яёєіїґәғқңөұүһ]+)*`)

// nameJoiners are the characters that continue a single surname across a boundary
// the plain letter class would stop at: the Ukrainian/Belarusian apostrophe in both
// its straight and curly forms, and the hyphen of a compound surname.
const nameJoiners = "'’-"

// sentenceEnders are the characters after which a capital signals sentence case rather
// than a name. Dash-led dialogue is routine in subtitles (a cue that opens with a
// dash, then "Привет, Иван"), and
// quotation marks, colons and ellipses open sentences too; without them an ordinary
// word gets pinned as a name. The closing bracket ends a "[00:00]" timestamp
// marker, after which the line starts. A list number ("1. Студентам") ends on its
// own period and needs nothing extra here. The comma is deliberately absent:
// "Привет, Иван" continues the sentence.
// A line break ends a sentence too: the boundary check trims spaces and tabs
// only, so the newline itself is the last rune before a word that opens the
// next line.
const sentenceEnders = ".!?…:;—–«»\"'()]\r\n"

// bareTimestampMarker matches an UNBRACKETED "mm:ss" / "hh:mm:ss" / "mm:ss.mmm"
// transcript marker that occupies the whole text before a candidate name, i.e.
// the marker opens the line and the capital after it is sentence case.
//
// The bracketed form ends in ']' and is handled by sentenceEnders. This one ends
// in a digit, and treating ANY trailing digit as a boundary was too broad: it hid
// a genuine name after an ordinary number ("В 2024 Иванов" dropped Иванов).
// Requiring the full marker shape keeps the boundary while leaving such names
// pinnable. It mirrors the bare form of the ingest marker grammar
// (transcriptTimestampBareRe), restated because ingest imports this package.
var bareTimestampMarker = regexp.MustCompile(`(?:\A|\n)[ \t]*\d+:\d{2}(?::\d{2})?(?:\.\d{1,3})?\z`)

// maxNameHints bounds the prompt growth on a name-dense line. Six covers the
// realistic worst case (a list of officials) without crowding out the text itself.
const maxNameHints = 6

// notAWholePlainName reports whether the match at loc must be refused: it is a
// name a joiner continues that this convention cannot render, or it is the tail
// of a longer token.
//
// A HYPHENATED name -- Римский-Корсаков -- is matched as ONE token by
// properNounRE, so it can no longer pin a fragment or split into two hints. It is
// then REFUSED rather than pinned, for the same reason an ambiguous -ова is: a
// correct rendering of a compound needs per-part rules that do not exist yet
// (the Russian transliterator gives "Rimskiy-korsakov": the second half is
// lowercased and the adjectival -sky rule only fires at word end), and no hint
// leaves the model its own rendering where a wrong hint overrides it.
//
// An APOSTROPHE is refused only where the convention treats it as a boundary. In
// Ukrainian it is a phonetic separator inside one word and the national system
// simply drops it, so Лук'яненко renders "Lukianenko" as one name; refusing it
// there would drop a whole class of Ukrainian surnames.
//
// A match that starts right after a letter or a joiner is the tail of a token
// whose head failed the capital rule (or a lowercase-led compound such as
// де-Голль). Go's regexp has no lookbehind, so that boundary is enforced here.
func notAWholePlainName(text string, loc []int, word string, conv *convention) bool {
	refused := nameJoiners
	if conv.apostropheIsInternal {
		refused = "-"
	}
	if strings.ContainsAny(word, refused) {
		return true
	}
	if loc[0] == 0 {
		return false
	}
	prev, _ := utf8.DecodeLastRuneInString(text[:loc[0]])
	return unicode.IsLetter(prev) || strings.ContainsRune(nameJoiners, prev)
}

// genitiveStem restores the nominative of a surname that the genitive trigger has
// put in the genitive: имени Сеченова -> Сеченов, імені Шевченка -> Шевченко.
//
// The trailing -а is stripped only when the stripped stem ends in a suffix
// nominativeSuffixes recognises, which is what proves the -а was an inflection at
// all. Without that proof the strip corrupts indeclinable names: Дюма is French
// and does not inflect, so "имени Дюма" is still Дюма, and stripping gave "Дюма
// -> Dyum" -- pinned into a prompt that says "use exactly this".
//
// A convention's triggerForms are checked AFTER the strip, for a genitive whose
// nominative is a different ending rather than a shorter one. They are safe only
// under the trigger, which guarantees the case: Ukrainian -ка restores to -ко
// (Франка -> Франко) because no feminine nominative in -ка has a genitive in -ка,
// but outside the construction the same ending is ordinary.
//
// An unproven stem falls through to nominalize, which refuses a vowel-final form
// as ambiguous, so no hint is emitted and the model keeps its own rendering.
func genitiveStem(word string, genitive bool, conv *convention) (string, bool) {
	if !genitive {
		return "", false
	}
	if strings.HasSuffix(word, "а") {
		stem := strings.TrimSuffix(word, "а")
		lw := strings.ToLower(stem)
		for _, suf := range conv.nominativeSuffixes {
			if strings.HasSuffix(lw, suf) {
				return stem, true
			}
		}
	}
	lw := strings.ToLower(word)
	for _, tf := range conv.triggerForms {
		if !strings.HasSuffix(lw, tf.from) {
			continue
		}
		stem := []rune(word)[:len([]rune(word))-len([]rune(tf.from))]
		if len(stem) < 3 {
			return "", false
		}
		return string(stem) + tf.to, true
	}
	return "", false
}

// opensASentence reports whether a capital placed right after before is sentence
// case rather than a name: nothing precedes it, the text ends in a sentence
// ender, or the text is only a bare transcript timestamp marker.
func opensASentence(before string) bool {
	if before == "" {
		return true
	}
	last := []rune(before)[len([]rune(before))-1]
	return strings.ContainsRune(sentenceEnders, last) || bareTimestampMarker.MatchString(before)
}

// Hint is one derived spelling: the Word exactly as it appears in the source,
// the English rendering the prompt should pin for it, and Key -- the lower-cased
// NOMINATIVE the rendering was derived from. Key is what identifies the name:
// "Иванову" and "Иванов" share the key "иванов". Callers that reconcile hints
// against another per-name table (the operator glossary) must match on Key, or an
// oblique form in the text slips past a nominative entry in the table.
type Hint struct {
	Word    string
	Key     string
	English string
}

// Hints renders HintPairs as "<word> -> <english>" strings, the form the prompt
// carries. See HintPairs for the derivation.
func Hints(text, sourceLang string) []string {
	pairs := HintPairs(text, sourceLang)
	if len(pairs) == 0 {
		return nil
	}
	out := make([]string, 0, len(pairs))
	for _, h := range pairs {
		out = append(out, h.Word+" -> "+h.English)
	}
	return out
}

// HintPairs returns spelling hints for the proper nouns in a line of Cyrillic
// text, in first-appearance order, under the convention of sourceLang. It returns
// nil when there is nothing to pin or the language has no convention, so the
// prompt is unchanged for the vast majority of lines.
func HintPairs(text, sourceLang string) []Hint {
	conv, ok := conventionFor(sourceLang)
	if !ok {
		return nil
	}
	var hints []Hint
	seen := make(map[string]bool)
	for _, loc := range properNounRE.FindAllStringIndex(text, -1) {
		word := text[loc[0]:loc[1]]
		before := strings.TrimRight(text[:loc[0]], " \t")
		genitive := conv.genitiveTrigger.MatchString(before)

		// A capital opening a sentence is sentence case, not a name. Checked AFTER the
		// genitive trigger: "им. Сеченова" ends in '.', so testing the boundary first
		// made the abbreviated form unreachable -- and it is the commoner one in copy.
		if !genitive && opensASentence(before) {
			continue
		}
		if notAWholePlainName(text, loc, word, conv) {
			continue
		}
		if hasExonym(word, conv) {
			continue
		}

		var nom string
		if stem, ok := genitiveStem(word, genitive, conv); ok {
			nom = stem
		} else {
			var ok bool
			if nom, ok = nominalize(word, conv); !ok {
				continue
			}
		}
		english := capitalise(conv.transliterate(strings.ToLower(nom)))
		if english == "" {
			continue
		}
		// Keyed on the NORMALISED name, and only once normalisation has succeeded: an
		// ambiguous occurrence that gets rejected must not block a later valid one, and
		// two inflections of the same name should not consume two hint slots.
		key := strings.ToLower(nom)
		if seen[key] {
			continue
		}
		seen[key] = true
		hints = append(hints, Hint{Word: word, Key: key, English: english})
		if len(hints) == maxNameHints {
			break
		}
	}
	return hints
}

// HasCyrillic reports whether text contains any Cyrillic letter, so the hint pass is
// skipped entirely for source languages it does not apply to.
func HasCyrillic(text string) bool {
	for _, r := range text {
		if unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}

// IsEnglishTarget reports whether the translation target is English. The hints are
// English transliterations, so pinning them for a French or German target would
// override that language's own convention for the same name.
func IsEnglishTarget(lang string) bool {
	l := strings.ToLower(strings.TrimSpace(lang))
	return l == "en" || l == "eng" || l == "english" || strings.HasPrefix(l, "en-") || strings.HasPrefix(l, "en_")
}
