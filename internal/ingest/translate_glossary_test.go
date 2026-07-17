package ingest

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

const glossaryGuidanceMarker = "Prefer these renderings for the terms below"

// TestBuildTranslatePrompt_GlossaryInjectedDeterministic asserts the per-line
// prompt (buildTranslatePrompt) injects the current target language's glossary as
// guidance, in sorted source-term order (never Go map-iteration order), and still
// carries the source text (SPEC §8.6.2, issue #574).
func TestBuildTranslatePrompt_GlossaryInjectedDeterministic(t *testing.T) {
	glossary := map[string]string{"Zephyr": "Zefiro", "Aegis": "Egida", "Mistral": "Mistral"}
	got := buildTranslatePrompt("The Aegis holds.", "es", glossary)

	if !strings.Contains(got, glossaryGuidanceMarker) {
		t.Fatalf("glossary guidance missing:\n%s", got)
	}
	for _, want := range []string{"Aegis => Egida", "Mistral => Mistral", "Zephyr => Zefiro"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing glossary entry %q in:\n%s", want, got)
		}
	}
	// Deterministic sorted order: Aegis < Mistral < Zephyr.
	ai, mi, zi := strings.Index(got, "Aegis =>"), strings.Index(got, "Mistral =>"), strings.Index(got, "Zephyr =>")
	if ai >= mi || mi >= zi {
		t.Errorf("glossary entries not in sorted order (Aegis<Mistral<Zephyr): %d,%d,%d\n%s", ai, mi, zi, got)
	}
	if !strings.HasSuffix(got, "The Aegis holds.") {
		t.Errorf("source text lost from prompt:\n%s", got)
	}
}

// TestBuildTranslatePrompt_NoGlossaryUnchanged asserts an empty/nil glossary
// injects nothing and reproduces the historical prompt (blank line before the
// source text) byte-for-byte, so today's behaviour is preserved.
func TestBuildTranslatePrompt_NoGlossaryUnchanged(t *testing.T) {
	// The exact pre-#574 prompt: the fixed instruction line, a blank line, then the
	// source text — with NO glossary block. Compared in full so a change anywhere in
	// the legacy prompt (not just the tail) fails the compat guarantee.
	const historical = "Translate the following text into es. Preserve meaning faithfully. " +
		"Return only the translated text, with no preamble, quotes, or explanation.\n\nhello"
	for _, g := range []map[string]string{nil, {}} {
		got := buildTranslatePrompt("hello", "es", g)
		if got != historical {
			t.Errorf("empty-glossary prompt diverged from the historical form byte-for-byte:\n got=%q\nwant=%q", got, historical)
		}
	}
}

// TestBuildWindowTranslatePrompt_Glossary asserts the windowed prompt
// (buildWindowTranslatePrompt, #573/#599) injects the same guidance in sorted
// order and omits it when the glossary is empty.
func TestBuildWindowTranslatePrompt_Glossary(t *testing.T) {
	cells := []translateCell{{body: "one", translatable: true}, {body: "two", translatable: true}}
	targets := []int{0, 1}

	withG := buildWindowTranslatePrompt(cells, nil, targets, nil, "de", map[string]string{"beta": "B", "alpha": "A"})
	if !strings.Contains(withG, glossaryGuidanceMarker) {
		t.Fatalf("glossary guidance missing from windowed prompt:\n%s", withG)
	}
	if ai, bi := strings.Index(withG, "alpha => A"), strings.Index(withG, "beta => B"); ai < 0 || ai >= bi {
		t.Errorf("windowed glossary entries not in sorted order: %d,%d\n%s", ai, bi, withG)
	}

	withoutG := buildWindowTranslatePrompt(cells, nil, targets, nil, "de", nil)
	if strings.Contains(withoutG, glossaryGuidanceMarker) {
		t.Errorf("guidance injected for nil glossary in windowed prompt:\n%s", withoutG)
	}
}

// TestTranslateGlossaryFor_CurrentTargetOnly asserts only the current target
// language's entries are selected (never another language's), that the language
// match is case-insensitive, and that an unconfigured target yields nil.
func TestTranslateGlossaryFor_CurrentTargetOnly(t *testing.T) {
	svc := &Service{cfg: config.Config{MediaTranslateGlossary: map[string]map[string]string{
		"es": {"Sun": "Sol"},
		"fr": {"Sun": "Soleil"},
	}}}

	if es := svc.translateGlossaryFor("es"); len(es) != 1 || es["Sun"] != "Sol" {
		t.Fatalf("es glossary wrong: %#v", es)
	}
	if es := svc.translateGlossaryFor("ES"); es == nil || es["Sun"] != "Sol" {
		t.Errorf("expected case-insensitive \"ES\" to resolve to the es map (Sun=>Sol), got %#v", es)
	}
	if g := svc.translateGlossaryFor("de"); g != nil {
		t.Errorf("expected nil for a target with no glossary, got %#v", g)
	}

	// The es prompt must show only the es rendering, never the fr one.
	prompt := buildTranslatePrompt("The Sun", "es", svc.translateGlossaryFor("es"))
	if !strings.Contains(prompt, "Sun => Sol") {
		t.Errorf("es rendering missing:\n%s", prompt)
	}
	if strings.Contains(prompt, "Soleil") {
		t.Errorf("fr rendering leaked into es prompt:\n%s", prompt)
	}
}
