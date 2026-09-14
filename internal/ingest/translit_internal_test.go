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
	without := nameHints("в клинике Сеченова сегодня")
	if len(without) != 1 || without[0] != "Сеченова -> Sechenova" {
		t.Fatalf("without \"имени\": %v, want the surface form kept", without)
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
