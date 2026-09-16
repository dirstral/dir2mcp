package tests

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
)

// recordingTranslator keeps every prompt it receives and answers both the
// per-line and the windowed contract, so one fake drives either path.
type recordingTranslator struct {
	mu      sync.Mutex
	prompts []string
}

func (r *recordingTranslator) Generate(_ context.Context, prompt string) (string, error) {
	r.mu.Lock()
	r.prompts = append(r.prompts, prompt)
	r.mu.Unlock()
	if strings.Contains(prompt, "Lines to translate:") {
		return numberedEcho(fencedPayload(prompt)), nil
	}
	return "translated:" + fencedPayload(prompt), nil
}

func (r *recordingTranslator) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.prompts, "\n---\n")
}

const hintLine = "[00:00] Студентам университета имени Сеченова об этом объявили"

// runNameHintTranslation transcribes one line and translates it into "en" under
// cfg, returning every prompt the translator saw.
func runNameHintTranslation(t *testing.T, cfg config.Config, sourceLang, source string) string {
	t.Helper()
	cfg.StateDir = t.TempDir()
	cfg.MediaTranslateEnabled = true
	cfg.MediaTranslateTargetLangs = []string{"en"}
	svc := mustNewIngestService(t, cfg, &fakeIngestStore{})
	svc.SetTranscriber(&fakeTranscriber{text: source})
	svc.SetTranscriptLanguage(sourceLang)
	tr := &recordingTranslator{}
	svc.SetTranslator(tr, "mistral", "m", []string{"en"})
	doc := model.Document{DocID: 11, RelPath: "audio/ru.mp3", DocType: "audio"}
	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, []byte("audio")); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation: %v", err)
	}
	got := tr.all()
	if got == "" {
		t.Fatal("translator was never called")
	}
	return got
}

// TestTranscriptTranslation_NameHintsPerLinePrompt pins the end-to-end effect of
// media.translate.name_hints on the per-line prompt (SPEC §8.6.2): ON with a
// Russian source pins the spelling; OFF, a Ukrainian source, or an unknown
// source leave the prompt without a hint. The tables are Russian BGN/PCGN, so
// a Ukrainian source would get Volodimir pinned where the Ukrainian rules give
// Volodymyr; unknown is not assumed Russian.
func TestTranscriptTranslation_NameHintsPerLinePrompt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		enabled    bool
		sourceLang string
		wantHint   bool
	}{
		{"on, Russian source", true, "ru", true},
		{"on, regional Russian tag", true, "ru-RU", true},
		{"off", false, "ru", false},
		{"on, Ukrainian source", true, "uk", false},
		{"on, unknown source", true, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runNameHintTranslation(t, config.Config{
				MediaTranslateNameHints:   tc.enabled,
				MediaTranslateWindowLines: 1, // the per-line path
			}, tc.sourceLang, hintLine)
			if has := strings.Contains(got, "Сеченова -> Sechenov"); has != tc.wantHint {
				t.Errorf("hint present = %v, want %v\nprompt:\n%s", has, tc.wantHint, got)
			}
			if !tc.wantHint && strings.Contains(got, "spellings") {
				t.Errorf("a prompt without hints must not carry the hint line:\n%s", got)
			}
			// The sentence-initial word is sentence case, not a name.
			if strings.Contains(got, "Студентам ->") {
				t.Errorf("the sentence-initial word was pinned as a name:\n%s", got)
			}
		})
	}
}

// TestTranscriptTranslation_NameHintsWindowedPrompt pins that the windowed
// prompt (the default path since #573) carries the hints of its TARGET cues,
// merged and deduplicated, and none for a context-only cue: the model must not
// return the context, so a spelling pinned for it would be an instruction about
// text it must not produce.
func TestTranscriptTranslation_NameHintsWindowedPrompt(t *testing.T) {
	t.Parallel()
	source := "[00:00] Выступил Сеченов\n" +
		"[00:03] Потом снова Сеченов\n" +
		"[00:06] Ответил Щербак\n" +
		"[00:09] Все ушли"
	got := runNameHintTranslation(t, config.Config{
		MediaTranslateNameHints:    true,
		MediaTranslateWindowLines:  2,
		MediaTranslateContextLines: 1,
	}, "ru", source)
	windows := strings.Split(got, "\n---\n")
	if len(windows) != 2 {
		t.Fatalf("expected 2 window prompts, got %d:\n%s", len(windows), got)
	}
	first, second := windows[0], windows[1]
	if !strings.Contains(first, "Сеченов -> Sechenov") {
		t.Errorf("window 1 must pin its target's name:\n%s", first)
	}
	if strings.Count(first, "Sechenov") != 1 {
		t.Errorf("a name that recurs across a window's targets must be pinned once:\n%s", first)
	}
	if strings.Contains(first, "Щербак ->") {
		t.Errorf("window 1 must not pin the name of its context-only cue (Щербак):\n%s", first)
	}
	if !strings.Contains(second, "Щербак -> Shcherbak") {
		t.Errorf("window 2 must pin its target's name:\n%s", second)
	}
	if strings.Contains(second, "Сеченов ->") {
		t.Errorf("window 2 must not pin the name of its context-only cue (Сеченов):\n%s", second)
	}
}

// TestTranscriptTranslation_GlossaryWinsOverNameHint pins the precedence rule
// (SPEC §8.6.2): an operator glossary entry for a name suppresses the derived
// hint for that name, so the prompt carries one instruction for it, not two.
func TestTranscriptTranslation_GlossaryWinsOverNameHint(t *testing.T) {
	t.Parallel()
	got := runNameHintTranslation(t, config.Config{
		MediaTranslateNameHints:   true,
		MediaTranslateWindowLines: 1,
		MediaTranslateGlossary:    map[string]map[string]string{"en": {"сеченов": "Setchenov"}},
	}, "ru", "[00:00] Выступил Сеченов и Щербак")
	if strings.Contains(got, "Сеченов -> Sechenov") {
		t.Errorf("the glossary entry must suppress the derived hint for the same name:\n%s", got)
	}
	if !strings.Contains(got, "сеченов => Setchenov") {
		t.Errorf("the glossary guidance must still be present:\n%s", got)
	}
	if !strings.Contains(got, "Щербак -> Shcherbak") {
		t.Errorf("a name the glossary does not cover keeps its hint:\n%s", got)
	}
}

// TestTranscriptTranslation_GlossaryCoversObliqueForms pins that precedence is
// decided on the NAME, not on the word as written. An operator writes the
// glossary key once, in the nominative; the text carries whatever case the
// sentence needs. "имени Сеченова" must count as covered by "сеченов", or the
// prompt carries both the operator's spelling and a derived one for the same
// person and leaves the model to choose.
func TestTranscriptTranslation_GlossaryCoversObliqueForms(t *testing.T) {
	t.Parallel()
	got := runNameHintTranslation(t, config.Config{
		MediaTranslateNameHints:   true,
		MediaTranslateWindowLines: 1,
		MediaTranslateGlossary:    map[string]map[string]string{"en": {"сеченов": "Setchenov"}},
	}, "ru", "[00:00] Мы были в университете имени Сеченова, сказал Щербак")
	if strings.Contains(got, "Сеченова -> Sechenov") {
		t.Errorf("an oblique form of a glossary-covered name must not get a derived hint:\n%s", got)
	}
	if !strings.Contains(got, "сеченов => Setchenov") {
		t.Errorf("the glossary guidance must still be present:\n%s", got)
	}
	if !strings.Contains(got, "Щербак -> Shcherbak") {
		t.Errorf("an uncovered name keeps its hint:\n%s", got)
	}
}

// TestTranscriptTranslation_UkrainianSourceUsesTheUkrainianTable pins the
// end-to-end effect of the second convention (SPEC §8.6.2): a Ukrainian source
// now carries hints, and they are the national-system spellings, not the Russian
// table's. Before the Ukrainian table existed this source was refused outright,
// because the Russian table would have pinned "Volodimir" for Volodymyr.
func TestTranscriptTranslation_UkrainianSourceUsesTheUkrainianTable(t *testing.T) {
	t.Parallel()
	const line = "[00:00] вчора сказав Володимир Гриценко про це"
	for _, tc := range []struct {
		name       string
		sourceLang string
		want       []string
		absent     []string
	}{
		{"Ukrainian", "uk", []string{"Володимир -> Volodymyr", "Гриценко -> Hrytsenko"}, []string{"Volodimir", "Gritsenko"}},
		{"regional Ukrainian tag", "uk-UA", []string{"Володимир -> Volodymyr"}, []string{"Volodimir"}},
		{"Russian", "ru", []string{"Володимир -> Volodimir", "Гриценко -> Gritsenko"}, []string{"Volodymyr", "Hrytsenko"}},
		{"no convention", "be", nil, []string{"Volodymyr", "Volodimir", "spellings"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runNameHintTranslation(t, config.Config{
				MediaTranslateNameHints:   true,
				MediaTranslateWindowLines: 1,
			}, tc.sourceLang, line)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("prompt is missing %q:\n%s", want, got)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(got, absent) {
					t.Errorf("prompt must not carry %q:\n%s", absent, got)
				}
			}
		})
	}
}
