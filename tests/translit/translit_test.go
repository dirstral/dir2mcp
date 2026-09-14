package translit_test

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/translit"
)

// TestTransliterate pins the BGN/PCGN-style renderings that matter for names.
func TestTransliterate(t *testing.T) {
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
		if got := translit.Transliterate(in); got != want {
			t.Errorf("Transliterate(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTransliterateAdjectivalEndings pins the conventional English rendering of
// adjectival name endings: -ский is "-sky" in English-language copy, not the
// letter-wise "-skiy", which would fight glossaries standardising on "-sky".
func TestTransliterateAdjectivalEndings(t *testing.T) {
	for in, want := range map[string]string{
		"Зеленский":   "Zelensky",
		"Достоевский": "Dostoevsky",
		"Троцкий":     "Trotsky",
		"Маяковский":  "Mayakovsky",
		"Грозный":     "Grozny",
	} {
		if got := translit.Transliterate(in); got != want {
			t.Errorf("Transliterate(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestHintsSkipsSentenceInitial pins that a capital opening a sentence is treated as
// sentence case, not a name -- otherwise every sentence's first word would be pinned.
func TestHintsSkipsSentenceInitial(t *testing.T) {
	got := translit.Hints("Студентам университета имени Сеченова об этом объявили.")
	if len(got) != 1 || got[0] != "Сеченова -> Sechenov" {
		t.Fatalf("Hints = %v, want [Сеченова -> Sechenov]", got)
	}
}

// TestHintsSkipsNonPeriodSentenceStarts pins that a capital after a dash, quote, colon
// or ellipsis is sentence case too. Dash-led dialogue is routine in subtitles, and
// without this an ordinary word is pinned as a name.
func TestHintsSkipsNonPeriodSentenceStarts(t *testing.T) {
	for _, s := range []string{
		"— Привет, как дела?",
		"«Привет», сказал он",
		"Он ушёл… Потом вернулся",
		"Вопрос: Почему так вышло",
	} {
		for _, h := range translit.Hints(s) {
			for _, bad := range []string{"Привет", "Потом", "Почему"} {
				if strings.HasPrefix(h, bad) {
					t.Errorf("Hints(%q) pinned ordinary sentence-initial word: %v", s, h)
				}
			}
		}
	}
	got := translit.Hints("— Привет, Иванов, как дела?")
	if len(got) != 1 || !strings.HasPrefix(got[0], "Иванов") {
		t.Errorf("expected only the surname hinted, got %v", got)
	}
}

// TestHintsGenitiveAfterImeni pins the "имени X" / "им. X" construction: it forces the
// genitive, so the English form is the nominative (Сеченова -> Sechenov). The
// abbreviated form ends in '.', which previously made it unreachable behind the
// sentence-boundary check -- and it is the commoner form in broadcast copy.
func TestHintsGenitiveAfterImeni(t *testing.T) {
	for _, s := range []string{
		"в клинике имени Сеченова сегодня",
		"в клинике им. Сеченова сегодня",
	} {
		got := translit.Hints(s)
		if len(got) != 1 || got[0] != "Сеченова -> Sechenov" {
			t.Errorf("Hints(%q) = %v, want [Сеченова -> Sechenov]", s, got)
		}
	}
	// Without the trigger a bare -ова is ambiguous: genitive of a man's surname or
	// nominative of a woman's. Guessing renames the person, so no hint is emitted.
	if got := translit.Hints("в клинике Сеченова сегодня"); len(got) != 0 {
		t.Errorf("ambiguous -ова should yield no hint, got %v", got)
	}
}

// TestHintsAmbiguousThenValidOccurrence pins that a rejected ambiguous occurrence does
// not suppress a later valid one: de-duplication keys on the normalised name and only
// applies once normalisation has succeeded.
func TestHintsAmbiguousThenValidOccurrence(t *testing.T) {
	got := translit.Hints("в клинике Сеченова, затем в университете имени Сеченова")
	if len(got) != 1 || got[0] != "Сеченова -> Sechenov" {
		t.Errorf("Hints = %v, want the later valid occurrence to be hinted", got)
	}
}

// TestHintsAdjectivalObliqueRestored pins that an oblique adjectival surname is
// RESTORED to its nominative, not truncated. Dropping "-ского" outright yielded
// "Зеленск" -> "Zelensk", and since the prompt says "use exactly these spellings" the
// model was then pushed to write it.
func TestHintsAdjectivalObliqueRestored(t *testing.T) {
	for src, want := range map[string]string{
		"встретил Зеленского вчера":   "Zelensky",
		"читал Достоевского всю ночь": "Dostoevsky",
		"говорил с Маяковским тогда":  "Mayakovsky",
	} {
		got := translit.Hints(src)
		if len(got) != 1 || !strings.HasSuffix(got[0], "-> "+want) {
			t.Errorf("Hints(%q) = %v, want a hint ending %q", src, got, want)
		}
	}
}

// TestHintsSkipsExonymsAndCommonNouns pins that words with an established English form
// get NO hint. Measured harm: "России -> Rossii" made the model write "in Rossii" where
// it had correctly written "in Russia"; "Бог -> Bog" turned "May God grant" into
// "May Bog indeed enable".
func TestHintsSkipsExonymsAndCommonNouns(t *testing.T) {
	for _, s := range []string{
		"это запрещено в России сегодня",
		"поехали в Харьков за деньги",
		"живёт в Москве уже год",
		"вступление в НАТО обсуждали",
		"дай Бог здоровья всем",
		"нашёл в Интернете вчера",
	} {
		if got := translit.Hints(s); len(got) != 0 {
			t.Errorf("Hints(%q) = %v, want none", s, got)
		}
	}
	// a real surname that merely starts like an exonym is still hinted: matching on a
	// bare prefix silently ate Венедиктов ("вен"), Литвиненко ("литв"), Богданов ("бог")
	for _, s := range []string{"сказал Венедиктов вчера", "сказал Литвиненко вчера", "сказал Богданов вчера"} {
		if got := translit.Hints(s); len(got) != 1 {
			t.Errorf("Hints(%q) = %v, want the surname hinted", s, got)
		}
	}
}

// TestHintsExtendedCyrillic pins that Kazakh/Kyrgyz letters count as part of a word.
// Omitting them made the match stop at the first one and pin a TRUNCATED name.
func TestHintsExtendedCyrillic(t *testing.T) {
	for _, h := range translit.Hints("встретил Айдарқұла вчера") {
		if strings.HasPrefix(h, "Айдар ->") {
			t.Errorf("pinned a truncated name: %v", h)
		}
	}
	if s := translit.Transliterate("Өмүрбек"); strings.ContainsAny(s, "өүқғңәұһ") {
		t.Errorf("Transliterate(Өмүрбек) = %q, want fully transliterated", s)
	}
}

// TestHintsEmptyForOrdinaryText pins that text with no mid-sentence proper noun yields
// no hints, so the prompt is unchanged for most lines.
func TestHintsEmptyForOrdinaryText(t *testing.T) {
	for _, s := range []string{
		"Он лежит в больнице, вот справка.",
		"Сегодня очень холодно.",
		"",
		"The meeting is over.",
	} {
		if got := translit.Hints(s); len(got) != 0 {
			t.Errorf("Hints(%q) = %v, want none", s, got)
		}
	}
}

// TestHintsDedupesAndCaps pins de-duplication and the per-line cap, so a name-dense
// line cannot crowd the text out of the prompt.
func TestHintsDedupesAndCaps(t *testing.T) {
	if got := translit.Hints("сказал Иванов, а потом Иванов снова сказал"); len(got) != 1 {
		t.Errorf("repeated name should yield one hint, got %v", got)
	}
	many := "перечислим: Иванов, Петров, Сидоров, Кузнецов, Смирнов, Попов, Волков, Лебедев"
	if got := translit.Hints(many); len(got) > 6 {
		t.Errorf("hints = %d, want capped at 6", len(got))
	}
}

// TestHasCyrillic pins the guard that keeps the hint pass off non-Cyrillic sources.
func TestHasCyrillic(t *testing.T) {
	if !translit.HasCyrillic("привет") || translit.HasCyrillic("hello world") || translit.HasCyrillic("") {
		t.Fatal("HasCyrillic misclassified")
	}
}

// TestIsEnglishTarget pins that hints apply only to an English target; the
// transliterations are BGN/PCGN and would override another language's convention.
func TestIsEnglishTarget(t *testing.T) {
	for _, l := range []string{"en", "EN", "eng", "English", "en-GB", "en_US"} {
		if !translit.IsEnglishTarget(l) {
			t.Errorf("IsEnglishTarget(%q) = false", l)
		}
	}
	for _, l := range []string{"de", "fr", "ru", "es", ""} {
		if translit.IsEnglishTarget(l) {
			t.Errorf("IsEnglishTarget(%q) = true", l)
		}
	}
}
