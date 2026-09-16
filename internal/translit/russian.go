package translit

import (
	"regexp"
	"strings"
)

// The Russian convention is BGN/PCGN-style, matching how Russian names are
// conventionally rendered in English-language copy (Сеченов -> Sechenov,
// Щербак -> Shcherbak).

// ruLetters is applied longest-output first, because the replacements run as a
// sequence of whole-string substitutions: щ must be consumed before ш, or "shch"
// can never be produced. It includes the Ukrainian and Kazakh/Kyrgyz letters the
// corpus also carries, so a stray letter in a Russian-tagged line transliterates
// rather than surviving as raw Cyrillic. A line whose SOURCE is Ukrainian is
// routed to the Ukrainian convention instead; see ukrainian.go.
var ruLetters = []replacement{
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

// ruAdjectivalEndings render the Russian adjectival name endings the way
// English-language copy conventionally does: Достоевский is "Dostoevsky", not
// "Dostoevskiy". Applied before the letter-by-letter table, which would otherwise
// produce the -iy form and fight editorial style.
//
// Ukrainian needs no counterpart: its national system renders -ський as "-skyi"
// through the plain letter rules, and that IS the conventional form there
// (Рибчинський is "Rybchynskyi").
var ruAdjectivalEndings = []replacement{
	{"цкий", "tsky"}, {"ский", "sky"}, {"цкая", "tskaya"}, {"ская", "skaya"},
	{"ый", "y"}, {"ий", "y"},
}

// transliterateRussian renders one lower-cased Russian word in Latin script.
func transliterateRussian(word string) string {
	for _, ae := range ruAdjectivalEndings {
		if strings.HasSuffix(word, ae.from) {
			base := strings.TrimSuffix(word, ae.from)
			for _, r := range ruLetters {
				base = strings.ReplaceAll(base, r.from, r.to)
			}
			return base + ae.to
		}
	}
	for _, r := range ruLetters {
		word = strings.ReplaceAll(word, r.from, r.to)
	}
	return word
}

// russian is the Russian source convention.
//
// obliqueForms maps an UNAMBIGUOUS oblique (non-nominative) ending on a personal
// name to the nominative ending. English does not inflect, so a hint must carry
// the nominative or the case ending leaks into the translation: "Тикебаеву"
// became "Tikebaevu", "Арнабаевича" became "Arnabaevicha".
//
// Adjectival surnames are RESTORED rather than truncated: dropping "-ского"
// outright yields "Зеленск", and because the prompt says "use exactly these
// spellings" the model is then pushed to write "Zelensk". Longest suffixes first,
// since HasSuffix is checked in order.
//
// Deliberately EXCLUDES -ова/-ева/-ина: genitive for a man's surname but
// NOMINATIVE for a woman's, and guessing wrong renames the person. Such forms are
// left unhinted, except after "имени", where the construction guarantees the
// genitive.
var russian = &convention{
	name:          "ru",
	transliterate: transliterateRussian,
	obliqueForms: []replacement{
		{"овичем", "ович"}, {"евичем", "евич"},
		{"овича", "ович"}, {"евича", "евич"}, {"овичу", "ович"}, {"евичу", "евич"},
		{"овной", "овна"}, {"евной", "евна"}, {"овне", "овна"}, {"евне", "евна"},
		{"ского", "ский"}, {"скому", "ский"}, {"ским", "ский"}, {"ском", "ский"},
		{"цкого", "цкий"}, {"цкому", "цкий"}, {"цким", "цкий"}, {"цком", "цкий"},
		{"овым", "ов"}, {"евым", "ев"}, {"иным", "ин"}, {"ыным", "ын"},
		{"ову", "ов"}, {"еву", "ев"}, {"ину", "ин"}, {"ыну", "ын"},
		{"ове", "ов"}, {"еве", "ев"}, {"ине", "ин"}, {"ыне", "ын"},
	},
	// Endings that mark a form as already nominative, and that prove a stripped
	// -а under the "имени" trigger was an inflection.
	nominativeSuffixes: []string{
		"ов", "ев", "ин", "ын", "ий", "ый", "ич", "ко", "ук", "юк", "ян", "дзе", "швили",
	},
	exonymStems: []string{
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
	},
	vowelRunes:      "аеёиоуыэюяәөүұ",
	caseEndingRunes: "аеёиоуыэюяьйм",
	// An apostrophe in Russian-tagged text is a compound boundary or a quote
	// artifact, not a letter, so a name carrying one is refused rather than
	// pinned; see notAWholePlainName.
	apostropheIsInternal: false,
	// genitiveTrigger matches the "имени X" / "им. X" construction, which forces
	// X into the genitive. The English form is then the nominative: "имени
	// Сеченова" is "Sechenov", not "Sechenova" -- the single most visible case,
	// since it names institutions.
	//
	// Deliberately NOT anchored with \b: Go's RE2 defines word boundaries over
	// ASCII word characters only, so \b never matches against a Cyrillic letter
	// and the pattern would silently never fire.
	genitiveTrigger: regexp.MustCompile(`(?i)(^|\s)(имени|им\.)\s*$`),
}
