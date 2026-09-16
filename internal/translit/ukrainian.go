package translit

import "regexp"

// The Ukrainian convention is the national system: Cabinet of Ministers
// resolution 55 of 2010-01-27, recommended for international use by resolution
// X/9 of the Tenth United Nations Conference on the Standardization of
// Geographical Names in 2012, and adopted by BGN/PCGN in its 2019 Agreement. It
// is what Ukrainian passports, road signs and official English-language copy
// use, so it is the spelling an English reader meets elsewhere.
//
// It is NOT the Russian table with Ukrainian letters added, which is why a
// Ukrainian source was refused until this convention existed: the Russian table
// gave Володимир -> Volodimir and Гриценко -> Gritsenko, where the Ukrainian
// rules give Volodymyr and Hrytsenko. The differences that matter for names are
// г -> h (not g), и -> y (not i), the position-dependent iotated letters below,
// and зг -> zgh.

// ukInitialForms are the renderings the iotated letters take at the START of a
// word. Elsewhere they take the ukLetters form: Юрій is "Yurii" (initial ю) and
// Коріння is "Korinnia" (medial я), so the same letter renders two ways in one
// text. A flat replacement table cannot express this, which is why the Ukrainian
// transliterator walks runes.
var ukInitialForms = map[rune]string{
	'є': "ye", 'ї': "yi", 'й': "y", 'ю': "yu", 'я': "ya",
}

// ukLetters is the letter-by-letter table, in the medial (non-word-initial)
// form for the five iotated letters.
//
// The soft sign and the apostrophe render as NOTHING, per the system's own note:
// Лук'яненко is "Lukianenko" and Рибчинський is "Rybchynskyi". The Russian
// letters at the end are not part of the system; they are here because a line
// tagged Ukrainian can still carry a Russian quotation, and leaving raw Cyrillic
// in a hint would be worse than transliterating it approximately.
var ukLetters = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "h", 'ґ': "g", 'д': "d", 'е': "e",
	'є': "ie", 'ж': "zh", 'з': "z", 'и': "y", 'і': "i", 'ї': "i", 'й': "i",
	'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o", 'п': "p", 'р': "r",
	'с': "s", 'т': "t", 'у': "u", 'ф': "f", 'х': "kh", 'ц': "ts", 'ч': "ch",
	'ш': "sh", 'щ': "shch", 'ю': "iu", 'я': "ia",
	// Every apostrophe form Ukrainian text uses; see nameJoiners.
	'ь': "", '\'': "", '’': "", 'ʼ': "",
	// Not Ukrainian letters; see the type comment.
	'ы': "y", 'э': "e", 'ё': "e", 'ъ': "",
}

// transliterateUkrainian renders one lower-cased Ukrainian word in Latin script.
//
// Two rules need more than a table. The pair зг renders "zgh", not "zh", so
// Згурський is "Zghurskyi" and Розгон is "Rozghon" rather than colliding with
// ж. And the iotated letters take their ukInitialForms rendering only at the
// start of a word: after the apostrophe they do NOT, because the apostrophe is a
// phonetic separator inside one word (В'ячеслав is "Viacheslav"), while after a
// hyphen they do, because each part of a compound is its own word.
func transliterateUkrainian(word string) string {
	runes := []rune(word)
	var b []byte
	atWordStart := true
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == 'з' && i+1 < len(runes) && runes[i+1] == 'г' {
			b = append(b, "zgh"...)
			i++
			atWordStart = false
			continue
		}
		if atWordStart {
			if s, ok := ukInitialForms[r]; ok {
				b = append(b, s...)
				atWordStart = false
				continue
			}
		}
		if s, ok := ukLetters[r]; ok {
			b = append(b, s...)
			// The apostrophe separates sounds inside one word, so what follows
			// it is not word-initial.
			atWordStart = false
			continue
		}
		b = append(b, string(r)...)
		atWordStart = r == '-'
	}
	return string(b)
}

// ukrainian is the Ukrainian source convention.
//
// obliqueForms carries ONLY the adjectival and patronymic endings, whose
// nominative is unambiguous. The -а genitive of the commonest surname shapes
// (Бондарчука, Франка, Шевченка) is deliberately absent here and handled only
// under the "імені" trigger, where the construction guarantees the genitive:
// outside it, restoring an -а form would have to guess, and a wrong pin is worse
// than none. Surnames in -ко are treated as indeclinable in standard usage, so a
// bare Шевченка is rare and not worth the risk.
var ukrainian = &convention{
	name:          "uk",
	transliterate: transliterateUkrainian,
	obliqueForms: []replacement{
		{"овичем", "ович"}, {"євичем", "євич"},
		{"ського", "ський"}, {"ському", "ський"}, {"ським", "ський"},
		{"цького", "цький"}, {"цькому", "цький"}, {"цьким", "цький"},
		{"ської", "ська"}, {"ською", "ська"}, {"ській", "ська"},
		{"цької", "цька"}, {"цькою", "цька"}, {"цькій", "цька"},
		{"овича", "ович"}, {"євича", "євич"},
		{"овичу", "ович"}, {"євичу", "євич"},
	},
	// Endings that mark a form as already nominative, and that prove a stripped
	// -а under the "імені" trigger was an inflection.
	nominativeSuffixes: []string{
		"ко", "ук", "юк", "ак", "ич", "ів", "ий", "ій", "ець", "ян", "дзе", "швілі",
	},
	// triggerForms restore a genitive that the "імені" construction guarantees
	// but a strip cannot reach: Франка and Шевченка end in -ка because the
	// nominative ends in -ко, and no feminine nominative gives a genitive in -ка
	// (a feminine -ка declines to -ки). They are checked AFTER the strip, so
	// Бондарчука still restores through its proven -ук rather than becoming
	// "Бондарчуко".
	triggerForms: []replacement{
		{"ка", "ко"},
	},
	exonymStems: []string{
		// countries / regions
		"україн", "росі", "білорус", "казахстан", "киргиз", "киргизстан",
		"грузі", "вірмені", "азербайджан", "узбекистан", "таджикистан",
		"туркмен", "молдов", "литв", "латві", "естоні", "польщ", "німеччин",
		"франці", "англі", "британі", "америк", "європ", "китай", "японі",
		"туреччин", "ізраїл", "іран", "ірак", "сирі", "інді", "афганістан",
		"чечн", "сибір", "кавказ", "урал", "крим", "донбас", "прибалтик",
		// cities
		"москв", "київ", "харків", "одес", "львів", "дніпр", "запоріж",
		"донецьк", "луганськ", "маріупол", "чорноб", "петербург", "мінськ",
		"бішкек", "астан", "алмат", "тбілісі", "єреван", "баку", "ташкент",
		"душанбе", "вільнюс", "риг", "таллінн", "варшав", "берлін", "париж",
		"лондон", "вашингтон", "брюссел", "праг", "відн", "рим", "стамбул",
		// bodies / institutions with standard English names
		"нато", "оон", "євросоюз", "кремл", "держдум", "юнеско", "інтерпол",
		// capitalised COMMON nouns
		"бог", "господ", "земл", "інтернет", "батьківщин", "вітчизн",
	},
	// Ukrainian has no ы/э and adds є/і/ї, so the vowel set that decides
	// "consonant-final, already nominative" differs from the Russian one.
	vowelRunes:      "аеєиіїоуюя",
	caseEndingRunes: "аеєиіїоуюяьйм",
	// The apostrophe is a phonetic separator INSIDE a Ukrainian word, not a
	// compound boundary, and the system drops it. Дем'янюк is one name and
	// renders "Demianiuk", so it is pinned rather than refused.
	apostropheIsInternal: true,
	genitiveTrigger:      regexp.MustCompile(`(?i)(^|\s)(імені|ім\.)\s*$`),
}
