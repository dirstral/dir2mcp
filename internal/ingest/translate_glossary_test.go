package ingest

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/promptfence"

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
	// The prompt no longer ENDS with the source: since #888 the source sits
	// inside the untrusted-data fence and the instruction is restated after it.
	payload, ok := promptfence.Payload(got)
	if !ok {
		t.Fatalf("source text is not fenced:\n%s", got)
	}
	if payload != "The Aegis holds." {
		t.Errorf("fenced source = %q, want the source text", payload)
	}
}

// TestBuildTranslatePrompt_NoGlossaryUnchanged asserts an empty/nil glossary
// injects nothing, comparing the whole prompt byte-for-byte so a change
// anywhere in it (not just the tail) is caught.
//
// The expected string changed with #888: the prompt now carries the
// untrusted-data guard, fences the source, and restates the output rule after
// it. That is a DELIBERATE break of the previous byte-for-byte guarantee, and
// the derivation identity carries a fence tag so no cached translation
// produced by the old prompt is served as if it came from this one. The
// assertion stays a full comparison rather than relaxing to Contains, so the
// new form is pinned exactly as tightly as the old one was.
func TestBuildTranslatePrompt_NoGlossaryUnchanged(t *testing.T) {
	want := "Translate the following text into es. Preserve meaning faithfully. " +
		"Return only the translated text, with no preamble, quotes, or explanation.\n" +
		"\n" + promptfence.Guard("translate") + "\n" +
		promptfence.Wrap("", "hello") +
		"\nReturn only the translated text."
	for _, g := range []map[string]string{nil, {}} {
		got := buildTranslatePrompt("hello", "es", g)
		if got != want {
			t.Errorf("prompt diverged byte-for-byte:\n got=%q\nwant=%q", got, want)
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
