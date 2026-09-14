package ingest

import (
	"strings"
	"testing"
)

// TestTranslitCyrillic pins the BGN/PCGN-style renderings that matter for names.
func TestTranslitCyrillic(t *testing.T) {
	for in, want := range map[string]string{
		"Сеченов":  "Sechenov",
		"Пирогов":  "Pirogov",
		"Собянин":  "Sobyanin",
		"Плющ":     "Plyushch",
		"Щербак":   "Shcherbak",
		"Хрущев":   "Khrushchev",
		"Жуков":    "Zhukov",
		"Цветаева": "Tsvetaeva",
	} {
		if got := translitCyrillic(in); got != want {
			t.Errorf("translitCyrillic(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNameHintsSkipsSentenceInitial pins that a capital opening a sentence is treated
// as sentence case, not a name -- otherwise every sentence's first word would be
// pinned as a proper noun.
func TestNameHintsSkipsSentenceInitial(t *testing.T) {
	// "Студентам" opens the sentence; only "Сеченова" is a name.
	got := nameHints("Студентам университета имени Сеченова об этом объявили.")
	if len(got) != 1 {
		t.Fatalf("expected 1 hint, got %d: %v", len(got), got)
	}
	if got[0] != "Сеченова -> Sechenov" {
		t.Errorf("hint = %q, want the NOMINATIVE after \"имени\"", got[0])
	}
}

// TestNameHintsGenitiveAfterImeni pins the "имени X" case: the construction forces the
// genitive, so the English form is the nominative (Сеченова -> Sechenov, not Sechenova).
// Without "имени" the surface form is kept, since a bare -ова may be a woman's surname.
func TestNameHintsGenitiveAfterImeni(t *testing.T) {
	with := nameHints("в университете имени Сеченова сегодня")
	if len(with) != 1 || with[0] != "Сеченова -> Sechenov" {
		t.Fatalf("after \"имени\": %v, want [Сеченова -> Sechenov]", with)
	}
	// Without the "имени" trigger a bare -ова is ambiguous: genitive of a man's
	// surname (Сеченов) or nominative of a woman's (Сеченова). Guessing renames the
	// person, so no hint is emitted and the model keeps its own rendering.
	without := nameHints("в клинике Сеченова сегодня")
	if len(without) != 0 {
		t.Fatalf("ambiguous -ова should yield no hint, got %v", without)
	}
}

// TestNameHintsEmptyForOrdinaryText pins that text with no mid-sentence proper noun
// produces no hints at all, so the prompt is unchanged for most lines.
func TestNameHintsEmptyForOrdinaryText(t *testing.T) {
	for _, s := range []string{
		"Он лежит в больнице, вот справка.",
		"Сегодня очень холодно.",
		"",
		"The meeting is over.",
	} {
		if got := nameHints(s); len(got) != 0 {
			t.Errorf("nameHints(%q) = %v, want none", s, got)
		}
	}
}

// TestNameHintsDedupesAndCaps pins de-duplication and the per-line cap, so a
// name-dense line cannot crowd the text out of the prompt.
func TestNameHintsDedupesAndCaps(t *testing.T) {
	dup := nameHints("сказал Иванов, а потом Иванов снова сказал")
	if len(dup) != 1 {
		t.Errorf("repeated name should yield one hint, got %v", dup)
	}
	many := "перечислим: Иванов, Петров, Сидоров, Кузнецов, Смирнов, Попов, Волков, Лебедев"
	if got := nameHints(many); len(got) != maxNameHints {
		t.Errorf("hints = %d, want capped at %d", len(got), maxNameHints)
	}
}

// TestHasCyrillic pins the guard that keeps the hint pass off non-Cyrillic sources.
func TestHasCyrillic(t *testing.T) {
	if !hasCyrillic("привет") || hasCyrillic("hello world") || hasCyrillic("") {
		t.Fatal("hasCyrillic misclassified")
	}
}

// TestBuildTranslatePromptHints pins that hints appear before the text when present,
// and that the prompt is byte-identical to the un-hinted form when absent.
func TestBuildTranslatePromptHints(t *testing.T) {
	plain := buildTranslatePrompt("привет", "en", nil)
	if strings.Contains(plain, "spellings") {
		t.Errorf("no hints should mean no spelling block:\n%s", plain)
	}
	hinted := buildTranslatePrompt("привет", "en", []string{"Сеченова -> Sechenov"})
	if !strings.Contains(hinted, "Сеченова -> Sechenov") {
		t.Errorf("hint missing from prompt:\n%s", hinted)
	}
	if strings.Index(hinted, "Sechenov") > strings.Index(hinted, "привет") {
		t.Errorf("hints must precede the text so the model reads them first:\n%s", hinted)
	}
}

// TestNameHintsSkipsExonyms pins that words with an established English form get NO
// hint. Measured harm: pinning "России -> Rossii" made the model write "in Rossii"
// where it had correctly written "in Russia", and pinning "Харьков -> Kharkov" fought
// the house-style "Kharkiv". Hints are only for names the model would otherwise invent.
func TestNameHintsSkipsExonyms(t *testing.T) {
	for _, s := range []string{
		"это запрещено в России сегодня",
		"поехали в Харьков за деньги",
		"живёт в Москве уже год",
		"вступление в НАТО обсуждали",
		"отношения с Украиной сложные",
	} {
		if got := nameHints(s); len(got) != 0 {
			t.Errorf("nameHints(%q) = %v, want none (exonym)", s, got)
		}
	}
	// a person's surname in the same sentence is still pinned
	got := nameHints("в Москве выступил Сеченов вчера")
	if len(got) != 1 || !strings.HasPrefix(got[0], "Сеченов") {
		t.Errorf("person name alongside an exonym should still be hinted, got %v", got)
	}
}

// TestNominalize pins that oblique forms are reduced to the nominative (English does
// not inflect, so a case ending would leak into the translation) and that ambiguous
// forms are refused rather than guessed.
func TestNominalize(t *testing.T) {
	for in, want := range map[string]string{
		"Тикебаеву":   "Тикебаев",   // dative
		"Арнабаевича": "Арнабаевич", // genitive patronymic
		"Лобанов":     "Лобанов",    // already nominative
		"Кобахидзе":   "Кобахидзе",  // Georgian -дзе
		"Петровым":    "Петров",     // instrumental
	} {
		got, ok := nominalize(in)
		if !ok || got != want {
			t.Errorf("nominalize(%q) = (%q,%v), want (%q,true)", in, got, ok, want)
		}
	}
	for _, ambiguous := range []string{"Сеченова", "Иванова", "Пушкина"} {
		if _, ok := nominalize(ambiguous); ok {
			t.Errorf("nominalize(%q) should refuse: -ова/-ина is genitive-masculine OR nominative-feminine", ambiguous)
		}
	}
}

// TestAdjectivalEndings pins the conventional English rendering of adjectival name
// endings: -ский is "-sky" in English-language copy, not the letter-wise "-skiy",
// which would fight editorial glossaries that standardise on "-sky".
func TestAdjectivalEndings(t *testing.T) {
	for in, want := range map[string]string{
		"Зеленский":   "Zelensky",
		"Достоевский": "Dostoevsky",
		"Троцкий":     "Trotsky",
		"Маяковский":  "Mayakovsky",
		"Грозный":     "Grozny",
	} {
		if got := translitCyrillic(in); got != want {
			t.Errorf("translitCyrillic(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNameHintsSkipsCapitalisedCommonNouns pins that Russian's capitalised common
// nouns are not mistaken for names.
func TestNameHintsSkipsCapitalisedCommonNouns(t *testing.T) {
	for _, s := range []string{
		"дай Бог здоровья всем",
		"вся Земля это знает",
		"нашёл в Интернете вчера",
	} {
		if got := nameHints(s); len(got) != 0 {
			t.Errorf("nameHints(%q) = %v, want none", s, got)
		}
	}
}

// --- regression tests for review findings on PR #985 ---

// TestNominalizeAdjectivalRestoresEnding pins that an oblique adjectival surname is
// RESTORED to its nominative, not truncated. Dropping "-ского" outright yielded
// "Зеленск" -> "Zelensk", and since the prompt says "use exactly these spellings" the
// model was then pushed to write it.
func TestNominalizeAdjectivalRestoresEnding(t *testing.T) {
	for in, want := range map[string]string{
		"Зеленского":   "Zelensky",
		"Достоевского": "Dostoevsky",
		"Маяковским":   "Mayakovsky",
		"Троцкому":     "Trotsky",
	} {
		nom, ok := nominalize(in)
		if !ok {
			t.Errorf("nominalize(%q) refused", in)
			continue
		}
		if got := translitCyrillic(nom); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

// TestNameHintsSkipsNonPeriodSentenceStarts pins that a capital after a dash, quote,
// colon or ellipsis is treated as sentence case. Dash-led dialogue is routine in
// subtitles, and without this an ordinary word is pinned as a name.
func TestNameHintsSkipsNonPeriodSentenceStarts(t *testing.T) {
	for _, s := range []string{
		"— Привет, как дела?",
		"«Привет», сказал он",
		"Он ушёл… Потом вернулся",
		"Вопрос: Почему так вышло",
	} {
		for _, h := range nameHints(s) {
			if strings.HasPrefix(h, "Привет") || strings.HasPrefix(h, "Потом") || strings.HasPrefix(h, "Почему") {
				t.Errorf("nameHints(%q) pinned an ordinary sentence-initial word: %v", s, h)
			}
		}
	}
	// a genuine name later in the same dash-led line is still pinned
	got := nameHints("— Привет, Иванов, как дела?")
	if len(got) != 1 || !strings.HasPrefix(got[0], "Иванов") {
		t.Errorf("expected only the surname to be hinted, got %v", got)
	}
}

// TestNameHintsAbbreviatedImeni pins that "им. X" works as well as "имени X". The
// abbreviated form ends in '.', so testing the sentence boundary first made it
// unreachable -- and it is the commoner form in broadcast copy.
func TestNameHintsAbbreviatedImeni(t *testing.T) {
	for _, s := range []string{"в клинике им. Сеченова сегодня", "в клинике имени Сеченова сегодня"} {
		got := nameHints(s)
		if len(got) != 1 || got[0] != "Сеченова -> Sechenov" {
			t.Errorf("nameHints(%q) = %v, want [Сеченова -> Sechenov]", s, got)
		}
	}
}

// TestNameHintsExtendedCyrillic pins that Kazakh/Kyrgyz letters are part of a word.
// Omitting them from the lowercase class made the match stop at the first one and pin
// a TRUNCATED name.
func TestNameHintsExtendedCyrillic(t *testing.T) {
	got := nameHints("встретил Айдарқұла вчера")
	for _, h := range got {
		if strings.HasPrefix(h, "Айдар ->") {
			t.Errorf("pinned a truncated name: %v", got)
		}
	}
	if s := translitCyrillic("Өмүрбек"); !strings.HasPrefix(s, "O") || strings.ContainsAny(s, "өүқғңәұһ") {
		t.Errorf("translitCyrillic(Өмүрбек) = %q, want fully transliterated", s)
	}
}

// TestHasExonymRequiresCaseEndingOnly pins that the exonym list matches a whole word
// plus an inflection, not any word sharing a prefix -- "вен" was eating Венедиктов,
// "литв" Литвиненко, "бог" Богданов, silently removing the feature for real names.
func TestHasExonymRequiresCaseEndingOnly(t *testing.T) {
	for _, w := range []string{"Венедиктов", "Литвиненко", "Богданов", "Казанцев", "Германом"} {
		if hasExonym(w) {
			t.Errorf("hasExonym(%q) = true, want false (real surname)", w)
		}
	}
	for _, w := range []string{"России", "Москве", "Крыма", "Бог", "Украиной"} {
		if !hasExonym(w) {
			t.Errorf("hasExonym(%q) = false, want true (exonym)", w)
		}
	}
}

// TestIsEnglishTarget pins that hints apply only to an English target; the
// transliterations are BGN/PCGN and would override another language's convention.
func TestIsEnglishTarget(t *testing.T) {
	for _, l := range []string{"en", "EN", "eng", "English", "en-GB", "en_US"} {
		if !isEnglishTarget(l) {
			t.Errorf("isEnglishTarget(%q) = false", l)
		}
	}
	for _, l := range []string{"de", "fr", "ru", "es", ""} {
		if isEnglishTarget(l) {
			t.Errorf("isEnglishTarget(%q) = true", l)
		}
	}
}
