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
// This is safe in the Cyrillic->English direction specifically: Russian capitalises
// mid-sentence words almost exclusively for proper nouns, so detection is reliable.
// The reverse (guessing which English capital is a mangled name) is not, because
// English capitalises ordinary words mid-sentence constantly.

// cyrTranslit is BGN/PCGN-style, matching how Russian names are conventionally
// rendered in English-language copy (Сеченов->Sechenov, Щербак->Shcherbak).
// Order matters: multi-rune outputs are applied before single-letter ones.
// Includes the Kazakh/Kyrgyz/Ukrainian letters this corpus also carries, so a name in
// those alphabets transliterates rather than surviving as raw Cyrillic.
var cyrTranslit = []struct{ from, to string }{
	{"щ", "shch"}, {"ш", "sh"}, {"ч", "ch"}, {"ж", "zh"}, {"ц", "ts"},
	{"ю", "yu"}, {"я", "ya"}, {"х", "kh"}, {"ё", "e"}, {"э", "e"},
	{"ъ", ""}, {"ь", ""},
	{"а", "a"}, {"б", "b"}, {"в", "v"}, {"г", "g"}, {"д", "d"}, {"е", "e"},
	{"з", "z"}, {"и", "i"}, {"й", "y"}, {"к", "k"}, {"л", "l"}, {"м", "m"},
	{"н", "n"}, {"о", "o"}, {"п", "p"}, {"р", "r"}, {"с", "s"}, {"т", "t"},
	{"у", "u"}, {"ф", "f"}, {"ы", "y"},
	// Ukrainian
	{"і", "i"}, {"ї", "yi"}, {"є", "ye"}, {"ґ", "g"},
	// Kazakh / Kyrgyz
	{"ә", "a"}, {"ғ", "g"}, {"қ", "k"}, {"ң", "ng"}, {"ө", "o"},
	{"ұ", "u"}, {"ү", "u"}, {"һ", "h"},
}

// adjectivalEndings render the Russian adjectival name endings the way English-language
// copy conventionally does: Достоевский is "Dostoevsky", not "Dostoevskiy". Applied
// before the letter-by-letter table, which would otherwise produce the -iy form and
// fight editorial style.
var adjectivalEndings = []struct{ from, to string }{
	{"цкий", "tsky"}, {"ский", "sky"}, {"цкая", "tskaya"}, {"ская", "skaya"},
	{"ый", "y"}, {"ий", "y"},
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

// Transliterate renders one Cyrillic word in Latin script, capitalised.
func Transliterate(word string) string {
	w := strings.ToLower(word)
	for _, ae := range adjectivalEndings {
		if strings.HasSuffix(w, ae.from) {
			base := strings.TrimSuffix(w, ae.from)
			for _, r := range cyrTranslit {
				base = strings.ReplaceAll(base, r.from, r.to)
			}
			return capitalise(base + ae.to)
		}
	}
	for _, r := range cyrTranslit {
		w = strings.ReplaceAll(w, r.from, r.to)
	}
	return capitalise(w)
}

// obliqueForms maps an UNAMBIGUOUS oblique (non-nominative) ending on a Russian
// personal name to the nominative ending. English does not inflect, so a hint must
// carry the nominative or the case ending leaks into the translation: "Тикебаеву"
// became "Tikebaevu", "Арнабаевича" became "Arnabaevicha".
//
// Adjectival surnames are RESTORED rather than truncated -- dropping "-ского" outright
// yields "Зеленск", and because the prompt says "use exactly these spellings" the model
// is then pushed to write "Zelensk". Longest suffixes first, since HasSuffix is checked
// in order.
//
// Deliberately EXCLUDES -ова/-ева/-ина: genitive for a man's surname but NOMINATIVE for
// a woman's, and guessing wrong renames the person. Such forms are left unhinted,
// except after "имени", where the construction guarantees the genitive.
var obliqueForms = []struct{ from, to string }{
	{"овичем", "ович"}, {"евичем", "евич"},
	{"овича", "ович"}, {"евича", "евич"}, {"овичу", "ович"}, {"евичу", "евич"},
	{"овной", "овна"}, {"евной", "евна"}, {"овне", "овна"}, {"евне", "евна"},
	{"ского", "ский"}, {"скому", "ский"}, {"ским", "ский"}, {"ском", "ский"},
	{"цкого", "цкий"}, {"цкому", "цкий"}, {"цким", "цкий"}, {"цком", "цкий"},
	{"овым", "ов"}, {"евым", "ев"}, {"иным", "ин"}, {"ыным", "ын"},
	{"ову", "ов"}, {"еву", "ев"}, {"ину", "ин"}, {"ыну", "ын"},
	{"ове", "ов"}, {"еве", "ев"}, {"ине", "ин"}, {"ыне", "ын"},
}

// nominativeSuffixes mark a form that already needs no adjustment.
var nominativeSuffixes = []string{
	"ов", "ев", "ин", "ын", "ий", "ый", "ич", "ко", "ук", "юк", "ян", "дзе", "швили",
}

// nominalize returns the nominative form of a Russian personal name plus whether the
// conversion is trustworthy. Anything ambiguous reports false so no hint is emitted --
// a wrong pin is worse than no pin, since the model may well have had it right.
func nominalize(word string) (string, bool) {
	lw := strings.ToLower(word)
	for _, of := range obliqueForms {
		if strings.HasSuffix(lw, of.from) {
			stem := []rune(word)[:len([]rune(word))-len([]rune(of.from))]
			if len(stem) < 3 {
				return "", false
			}
			return string(stem) + of.to, true
		}
	}
	for _, nom := range nominativeSuffixes {
		if strings.HasSuffix(lw, nom) {
			return word, true
		}
	}
	r := []rune(lw)
	if !strings.ContainsRune("аеёиоуыэюяәөүұ", r[len(r)-1]) {
		return word, true // consonant-final: already nominative
	}
	return "", false
}

// exonymStems lists source words that already have an established English form the
// model knows (countries, major cities, well-known bodies, and capitalised common
// nouns). Pinning a literal transliteration for these makes the translation WORSE --
// measured: "России" pinned to "Rossii" turned "in Russia" into "in Rossii", and "Бог"
// pinned to "Bog" turned "May God grant" into "May Bog indeed enable".
var exonymStems = []string{
	// countries / regions
	"росси", "украин", "беларус", "белорус", "казахстан", "киргиз", "кыргыз",
	"грузи", "армени", "азербайджан", "узбекистан", "таджикистан", "туркмен",
	"молдов", "литв", "латви", "эстони", "польш", "германи", "франци", "англи",
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
	// capitalised COMMON nouns
	"бог", "господ", "земл", "интернет", "родин", "отечеств",
}

// caseEndingRunes are the letters a Russian case ending is built from. Used to decide
// whether the text after an exonym stem is an inflection of that word or the rest of a
// different, longer word.
const caseEndingRunes = "аеёиоуыэюяьйм"

// hasExonym reports whether word already has a conventional English rendering.
//
// The stem must be followed only by a plausible case ending. A bare prefix test
// swallows real surnames that merely start the same way -- "вен" ate Венедиктов,
// "литв" ate Литвиненко, "бог" ate Богданов -- silently removing the feature for names
// it exists to fix.
func hasExonym(word string) bool {
	lw := strings.ToLower(word)
	for _, stem := range exonymStems {
		if !strings.HasPrefix(lw, stem) {
			continue
		}
		rest := []rune(strings.TrimPrefix(lw, stem))
		if len(rest) > 3 {
			continue
		}
		ok := true
		for _, r := range rest {
			if !strings.ContainsRune(caseEndingRunes, r) {
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

// genitiveTrigger matches the "имени X" / "им. X" construction, which forces X into
// the genitive. The English form is then the nominative: "имени Сеченова" is
// "Sechenov", not "Sechenova" -- the single most visible case, since it names
// institutions.
//
// Deliberately NOT anchored with \b: Go's RE2 defines word boundaries over ASCII word
// characters only, so \b never matches against a Cyrillic letter and the pattern would
// silently never fire.
var genitiveTrigger = regexp.MustCompile(`(?i)(^|\s)(имени|им\.)\s*$`)

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
// than a name. Dash-led dialogue is routine in subtitles ("— Привет, Иван"), and
// quotation marks, colons and ellipses open sentences too; without them an ordinary
// word gets pinned as a name.
const sentenceEnders = ".!?…:;—–«»\"'()"

// maxNameHints bounds the prompt growth on a name-dense line. Six covers the
// realistic worst case (a list of officials) without crowding out the text itself.
const maxNameHints = 6

// Hints returns "<source> -> <english>" spelling hints for the proper nouns in a
// line of Cyrillic text, in first-appearance order. Returns nil when there is nothing
// to pin, so the prompt is unchanged for the vast majority of lines.
// notAWholePlainName reports whether the match at loc must be refused: it is a
// name a joiner continues, or it is the tail of a longer token.
//
// A name that a joiner continues -- Лук'яненко, Дем'янюк, Римский-Корсаков -- is
// matched as ONE token by properNounRE, so it can no longer pin a fragment ("Лук
// -> Luk") or split into two hints ("Римский -> Rimsky", "Корсаков -> Korsakov").
// Both put a wrong spelling into a prompt that says "use exactly these". It is
// then REFUSED rather than pinned, for the same reason an ambiguous -ова is: a
// correct rendering of a compound needs per-part rules that do not exist yet
// (Transliterate gives "Rimskiy-korsakov": the second half is lowercased and the
// adjectival -sky rule only fires at word end), and no hint leaves the model its
// own rendering where a wrong hint overrides it. Splitting on the joiner and
// transliterating each part is the follow-up once those rules exist.
//
// A match that starts right after a letter or a joiner is the tail of a token
// whose head failed the capital rule (or a lowercase-led compound such as
// де-Голль). Go's regexp has no lookbehind, so that boundary is enforced here.
func notAWholePlainName(text string, loc []int, word string) bool {
	if strings.ContainsAny(word, nameJoiners) {
		return true
	}
	if loc[0] == 0 {
		return false
	}
	prev, _ := utf8.DecodeLastRuneInString(text[:loc[0]])
	return unicode.IsLetter(prev) || strings.ContainsRune(nameJoiners, prev)
}

func Hints(text string) []string {
	var hints []string
	seen := make(map[string]bool)
	for _, loc := range properNounRE.FindAllStringIndex(text, -1) {
		word := text[loc[0]:loc[1]]
		before := strings.TrimRight(text[:loc[0]], " \t")
		genitive := genitiveTrigger.MatchString(before)

		// A capital opening a sentence is sentence case, not a name. Checked AFTER the
		// genitive trigger: "им. Сеченова" ends in '.', so testing the boundary first
		// made the abbreviated form unreachable -- and it is the commoner one in copy.
		if !genitive {
			if before == "" {
				continue
			}
			last := []rune(before)[len([]rune(before))-1]
			if strings.ContainsRune(sentenceEnders, last) {
				continue
			}
		}
		if notAWholePlainName(text, loc, word) {
			continue
		}
		if hasExonym(word) {
			continue
		}

		var nom string
		if genitive && strings.HasSuffix(word, "а") {
			nom = strings.TrimSuffix(word, "а")
		} else {
			var ok bool
			if nom, ok = nominalize(word); !ok {
				continue
			}
		}
		english := Transliterate(nom)
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
		hints = append(hints, word+" -> "+english)
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
// English transliterations (BGN/PCGN), so pinning them for a French or German target
// would override that language's own convention for the same name.
func IsEnglishTarget(lang string) bool {
	l := strings.ToLower(strings.TrimSpace(lang))
	return l == "en" || l == "eng" || l == "english" || strings.HasPrefix(l, "en-") || strings.HasPrefix(l, "en_")
}
