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

// adjectivalEndings render the Russian adjectival name endings the way English-language
// copy conventionally does: Достоевский is "Dostoevsky", not "Dostoevskiy". Applied
// before the letter-by-letter table, which would otherwise produce the -iy form and
// fight editorial style (measured: "Zelenskiy" where house style is "Zelensky").
var adjectivalEndings = []struct{ from, to string }{
	{"цкий", "tsky"}, {"ский", "sky"}, {"цкая", "tskaya"}, {"ская", "skaya"},
	{"ый", "y"}, {"ий", "y"},
}

// translitCyrillic renders one Cyrillic word in Latin script, capitalised.
func translitCyrillic(word string) string {
	w := strings.ToLower(word)
	for _, ae := range adjectivalEndings {
		if strings.HasSuffix(w, ae.from) {
			w = strings.TrimSuffix(w, ae.from)
			for _, r := range cyrTranslit {
				w = strings.ReplaceAll(w, r.from, r.to)
			}
			w += ae.to
			if w == "" {
				return ""
			}
			return strings.ToUpper(w[:1]) + w[1:]
		}
	}
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

// exonymStems lists source words that already have an established English form the
// model knows (countries, major cities, well-known bodies). Pinning a literal
// transliteration for these makes the translation WORSE -- measured: "России" was
// pinned to "Rossii" and the model wrote "prohibited in Rossii" where it had correctly
// written "in Russia", and "Харьков" was pinned to "Kharkov" against the house-style
// "Kharkiv". Hints are for names the model would otherwise invent, not for words it
// already renders correctly.
//
// Matched by PREFIX against the lowercased word, so inflected forms (России, Москве,
// Харькова) are covered without enumerating every case ending.
var exonymStems = []string{
	// countries / regions
	"росси", "украин", "беларус", "белорус", "казахстан", "киргиз", "кыргыз",
	"грузи", "армени", "азербайджан", "узбекистан", "таджикистан", "туркмен",
	"молдов", "литв", "латви", "эстони", "польш", "герман", "франц", "англи",
	"британи", "америк", "европ", "китай", "япони", "турци", "израил", "иран",
	"ирак", "сири", "инди", "афганистан", "чечн", "сибир", "кавказ", "урал",
	"крым", "донбас", "прибалтик",
	// cities
	"москв", "киев", "харьков", "одесс", "львов", "петербург", "минск", "бишкек",
	"астан", "алмат", "тбилиси", "ереван", "баку", "ташкент", "душанбе", "вильнюс",
	"риг", "таллин", "варшав", "берлин", "париж", "лондон", "вашингтон", "брюссел",
	"праг", "вен", "рим", "стамбул", "сочи", "казан", "екатеринбург", "новосибирск",
	// bodies / institutions with standard English names
	"нато", "оон", "евросоюз", "кремл", "госдум", "думе", "юнеско", "интерпол",
	// capitalised COMMON nouns -- Russian capitalises these but they are ordinary
	// words with English equivalents, and pinning a transliteration mangles the
	// sentence (measured: "Бог" pinned as "Bog" turned "May God grant" into
	// "May Bog indeed enable").
	"бог", "господ", "земл", "интернет", "родин", "отечеств",
}

// hasExonym reports whether word already has a conventional English rendering, in
// which case no hint is emitted and the model is left to use it.
func hasExonym(word string) bool {
	lw := strings.ToLower(word)
	for _, stem := range exonymStems {
		if strings.HasPrefix(lw, stem) {
			return true
		}
	}
	return false
}

// obliqueEndings maps an UNAMBIGUOUS oblique (non-nominative) ending on a Russian
// personal name to the number of trailing runes to drop to reach the nominative.
// English does not inflect, so a hint must carry the nominative or the case ending
// leaks into the translation -- measured: "Тикебаеву" pinned as "Tikebaevu" and
// "Арнабаевича" as "Arnabaevicha".
//
// Deliberately EXCLUDES -ова/-ева/-ина: those are genitive for a man's surname but
// NOMINATIVE for a woman's, and guessing wrong renames the person. Such forms are left
// unhinted (the model's own rendering is no worse), except after "имени", where the
// construction guarantees the genitive.
var obliqueEndings = []struct {
	suffix string
	drop   int
}{
	{"овича", 1}, {"евича", 1}, {"овичу", 1}, {"евичу", 1}, {"овичем", 2}, {"евичем", 2},
	{"овной", 2}, {"евной", 2}, {"овне", 1}, {"евне", 1},
	{"овым", 2}, {"евым", 2}, {"иным", 2}, {"ыным", 2},
	{"ову", 1}, {"еву", 1}, {"ину", 1}, {"ыну", 1},
	{"ове", 1}, {"еве", 1}, {"ине", 1}, {"ыне", 1},
	{"ского", 3}, {"скому", 3}, {"ским", 2}, {"ском", 2},
}

// nominalize returns the nominative form of a Russian personal name plus whether the
// conversion is trustworthy. A name already in a nominative-looking form is returned
// as-is; an unambiguous oblique form is reduced; anything else reports false so no hint
// is emitted -- a wrong pin is worse than no pin, since the model may well have had it
// right.
func nominalize(word string) (string, bool) {
	lw := strings.ToLower(word)
	for _, oe := range obliqueEndings {
		if strings.HasSuffix(lw, oe.suffix) {
			r := []rune(word)
			if len(r)-oe.drop >= 4 {
				return string(r[:len(r)-oe.drop]), true
			}
			return "", false
		}
	}
	// nominative-looking: surname/patronymic suffixes, or a bare consonant ending
	for _, nom := range []string{"ов", "ев", "ин", "ын", "ий", "ый", "ич", "ко", "ук", "юк", "ян", "дзе", "швили"} {
		if strings.HasSuffix(lw, nom) {
			return word, true
		}
	}
	last := []rune(lw)[len([]rune(lw))-1]
	if !strings.ContainsRune("аеёиоуыэюя", last) {
		return word, true // ends in a consonant: already nominative
	}
	return "", false
}

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
		// words with an established English form are left alone -- see exonymStems
		if hasExonym(word) {
			continue
		}

		// "имени X" guarantees the genitive, so -а comes off even though a bare
		// -ова/-ева is otherwise ambiguous between a man's genitive and a woman's
		// nominative.
		var nom string
		if genitiveTrigger.MatchString(before) && strings.HasSuffix(word, "а") {
			nom = strings.TrimSuffix(word, "а")
		} else {
			var ok bool
			if nom, ok = nominalize(word); !ok {
				continue
			}
		}
		english := translitCyrillic(nom)
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
