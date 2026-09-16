package translit_test

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/translit"
)

// The Ukrainian convention is the national system (Cabinet of Ministers
// resolution 55 of 2010, UNGEGN 2012, BGN/PCGN 2020). Every expectation below
// that carries a "(official)" note is an example from the published table, so a
// change to the table is measured against the source, not against itself.

func TestTransliterateUkrainian_OfficialExamples(t *testing.T) {
	for in, want := range map[string]string{
		// The three letters that differ most from the Russian table.
		"Гадяч":   "Hadiach",     // official: г -> h
		"Медвин":  "Medvyn",      // official: и -> y
		"Ґалаґан": "Galagan",     // official: ґ -> g
		"Харків":  "Kharkiv",     // х -> kh
		"Гоща":    "Hoshcha",     // official
		"Щербухи": "Shcherbukhy", // official
		// Iotated letters: word-initial vs medial, both in one word for Єнакієве.
		"Єнакієве":  "Yenakiieve", // official: є- -> ye, -є- -> ie
		"Гаєвич":    "Haievych",   // official
		"Йосипівка": "Yosypivka",  // official: й- -> y
		"Стрий":     "Stryi",      // official: -й -> i
		"Олексій":   "Oleksii",    // official
		"Кадиївка":  "Kadyivka",   // official: -ї- -> i
		"Юрій":      "Yurii",      // official: ю- -> yu
		"Коріння":   "Korinnia",   // -я -> ia
		"Наталія":   "Nataliia",   // official
		"Яготин":    "Yahotyn",    // official: я- -> ya
		"Костянтин": "Kostiantyn", // official: -я- -> ia
		// зг is "zgh", so it cannot be read back as ж ("zh"). жг is not affected.
		"Згурський": "Zghurskyi", // official
		"Розгон":    "Rozghon",   // official
		"Ужгород":   "Uzhhorod",  // official: ж then г, not зг
		// The adjectival ending needs no special rule here: the plain letter
		// rules already give the conventional form.
		"Рибчинський": "Rybchynskyi", // official
		"Зеленський":  "Zelenskyi",
		"Коцюбинська": "Kotsiubynska",
		// The soft sign and the apostrophe render as nothing.
		"Лук'яненко": "Lukianenko",
		"Дем'янюк":   "Demianiuk",
		"В'ячеслав":  "Viacheslav",
		// The names the Russian table got wrong, which is why this convention exists.
		"Володимир": "Volodymyr",
		"Гриценко":  "Hrytsenko",
		"Шевченко":  "Shevchenko", // official
	} {
		if got := translit.Transliterate(in, "uk"); got != want {
			t.Errorf("Transliterate(%q, uk) = %q, want %q", in, got, want)
		}
	}
}

// The same word under the two conventions must differ where the systems differ.
// One table applied to the other language is the defect this split prevents.
func TestTransliterate_ConventionsDisagreeWhereTheSystemsDo(t *testing.T) {
	for _, tc := range []struct{ word, ru, uk string }{
		{"Володимир", "Volodimir", "Volodymyr"},
		{"Гриценко", "Gritsenko", "Hrytsenko"},
		{"Ігор", "Igor", "Ihor"},
	} {
		if got := translit.Transliterate(tc.word, "ru"); got != tc.ru {
			t.Errorf("Transliterate(%q, ru) = %q, want %q", tc.word, got, tc.ru)
		}
		if got := translit.Transliterate(tc.word, "uk"); got != tc.uk {
			t.Errorf("Transliterate(%q, uk) = %q, want %q", tc.word, got, tc.uk)
		}
	}
	// A language with no convention renders nothing rather than borrowing one.
	for _, lang := range []string{"", "be", "en", "ua"} {
		if got := translit.Transliterate("Гриценко", lang); got != "" {
			t.Errorf("Transliterate(Гриценко, %q) = %q, want empty", lang, got)
		}
	}
}

// End to end: a Ukrainian line yields Ukrainian spellings, and the same line
// declared Russian yields the Russian ones. Both are "correct" for their
// declared source, which is why the source language has to reach this package.
func TestHintsUkrainian_UsesTheUkrainianTable(t *testing.T) {
	const line = "вчора сказав Володимир Гриценко про це"
	got := translit.Hints(line, "uk")
	want := []string{"Володимир -> Volodymyr", "Гриценко -> Hrytsenko"}
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Errorf("Hints(uk) = %v, want %v", got, want)
	}
	ru := translit.Hints(line, "ru")
	if strings.Join(ru, "; ") == strings.Join(got, "; ") {
		t.Errorf("the two conventions must not agree on this line: %v", ru)
	}
	if len(translit.Hints(line, "")) != 0 || len(translit.Hints(line, "be")) != 0 {
		t.Error("a language with no convention must yield no hints")
	}
}

// "імені X" forces the genitive, so the English form is the nominative. The
// Ukrainian restoration has to reach the -ко surnames, which is the commonest
// shape the construction names (Шевченко and Франко both name universities).
func TestHintsUkrainian_GenitiveAfterImeni(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{"університет імені Шевченка сьогодні", "Шевченка -> Shevchenko"},
		{"університет імені Франка сьогодні", "Франка -> Franko"},
		{"премія ім. Бондарчука вручена", "Бондарчука -> Bondarchuk"},
		{"театр імені Заньковецької відкрито", "Заньковецької -> Zankovetska"},
	} {
		got := translit.Hints(tc.line, "uk")
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("Hints(%q, uk) = %v, want [%q]", tc.line, got, tc.want)
		}
	}
	// The -ук restoration must win over the -ка rule, or Бондарчука becomes
	// "Бондарчуко": the strip is proven by a nominative suffix and runs first.
	if got := translit.Hints("премія ім. Бондарчука вручена", "uk"); len(got) == 1 && strings.Contains(got[0], "Bondarchuko") {
		t.Errorf("the proven -ук strip must run before the -ка rule: %v", got)
	}
}

// An indeclinable name after the trigger keeps its ending, exactly as in Russian:
// a strip that cannot be proven emits no hint rather than a truncated one.
func TestHintsUkrainian_AnIndeclinableNameAfterImeniIsNotTruncated(t *testing.T) {
	for _, line := range []string{"у театрі імені Дюма сьогодні", "премія імені Гарсіа вручена"} {
		if got := translit.Hints(line, "uk"); len(got) != 0 {
			t.Errorf("Hints(%q, uk) = %v, want none", line, got)
		}
	}
}

// Outside the trigger an -а form is left alone: it is a man's genitive or a
// woman's nominative depending on the surname, and guessing renames the person.
func TestHintsUkrainian_BareGenitiveIsRefused(t *testing.T) {
	for _, line := range []string{"зустрів Бондарчука вчора", "бачив Шевченка вчора"} {
		if got := translit.Hints(line, "uk"); len(got) != 0 {
			t.Errorf("Hints(%q, uk) = %v, want none outside the імені construction", line, got)
		}
	}
}

// Adjectival surnames are restored to the nominative rather than truncated.
func TestHintsUkrainian_AdjectivalObliqueRestored(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{"виступ Зеленського вчора", "Зеленського -> Zelenskyi"},
		{"розмова із Зеленським вчора", "Зеленським -> Zelenskyi"},
		{"твори Коцюбинського вивчають", "Коцюбинського -> Kotsiubynskyi"},
		{"слова Грушевському передали", "Грушевському -> Hrushevskyi"},
	} {
		got := translit.Hints(tc.line, "uk")
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("Hints(%q, uk) = %v, want [%q]", tc.line, got, tc.want)
		}
	}
}

// An apostrophe is a phonetic separator inside one Ukrainian word, and the
// national system drops it. The name is pinned whole; under Russian, where an
// apostrophe is a compound boundary or a quote artifact, it stays refused.
func TestHintsUkrainian_ApostropheNamesArePinnedWhole(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{"сказали Лук'яненко та інші", "Лук'яненко -> Lukianenko"},
		{"виступив Дем'янюк вчора", "Дем'янюк -> Demianiuk"},
	} {
		got := translit.Hints(tc.line, "uk")
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("Hints(%q, uk) = %v, want [%q]", tc.line, got, tc.want)
		}
		if ru := translit.Hints(tc.line, "ru"); len(ru) != 0 {
			t.Errorf("Russian must still refuse an apostrophe name: %v", ru)
		}
	}
	// A hyphenated compound stays refused under both: rendering it needs
	// per-part rules that do not exist yet.
	if got := translit.Hints("твори Нечуй-Левицького вивчають", "uk"); len(got) != 0 {
		t.Errorf("a hyphenated compound must stay refused: %v", got)
	}
}

// Ukrainian exonyms are the Ukrainian-language spellings, not the Russian ones:
// pinning "Ukraina" for Україна or "Kyiv" as "Kyievi" makes the translation worse.
func TestHintsUkrainian_SkipsExonymsAndCommonNouns(t *testing.T) {
	for _, line := range []string{
		"розмови про Україну тривають",
		"заборонено в Росії сьогодні",
		"живе у Києві давно",
		"поїхав до Львова вчора",
		"вступ до НАТО обговорювали",
		"дай Бог здоров'я всім",
		"знайшов в Інтернеті вчора",
	} {
		if got := translit.Hints(line, "uk"); len(got) != 0 {
			t.Errorf("Hints(%q, uk) = %v, want none", line, got)
		}
	}
	// A real surname that merely starts like an exonym is still hinted: matching
	// on a bare prefix would silently eat it.
	for _, tc := range []struct{ line, word string }{
		{"сказав Богданов вчора", "Богданов"},
		{"сказав Литвиненко вчора", "Литвиненко"},
		{"сказав Римчук вчора", "Римчук"},
	} {
		got := translit.Hints(tc.line, "uk")
		if len(got) != 1 || !strings.HasPrefix(got[0], tc.word+" -> ") {
			t.Errorf("Hints(%q, uk) = %v, want the complete surname %q", tc.line, got, tc.word)
		}
	}
}

// Ordinary Ukrainian prose carries no hints, so the prompt is unchanged for the
// vast majority of lines.
func TestHintsUkrainian_EmptyForOrdinaryText(t *testing.T) {
	for _, line := range []string{
		"сьогодні вранці почався дощ",
		"вони говорили про погоду та новини",
		"Це звичайне речення без імен.",
	} {
		if got := translit.Hints(line, "uk"); len(got) != 0 {
			t.Errorf("Hints(%q, uk) = %v, want none", line, got)
		}
	}
}
