package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// TestScriptGuardForeignDetection pins the wrong-script verdict for a Cyrillic
// track: all-Latin gibberish is foreign; a cue with any Cyrillic letter, any
// digit, or no letters at all is not.
func TestScriptGuardForeignDetection(t *testing.T) {
	g, err := subtitle.NewScriptGuard("cyrillic")
	if err != nil {
		t.Fatalf("NewScriptGuard: %v", err)
	}
	if !g.Active() {
		t.Fatalf("guard with a script should be active")
	}
	foreign := []string{
		"Elola alolo.",     // Latin vowel-run gibberish
		"Iiiiiiii.",        // letter-run gibberish
		"M u l i d.",       // spaced Latin letters
		"lorem ipsum, sic", // multi-word Latin, punctuation ignored
	}
	for _, s := range foreign {
		if !g.IsForeign(s) {
			t.Errorf("IsForeign(%q) = false, want true", s)
		}
	}
	keep := []string{
		"Обычная речь.",        // expected script
		"Смотрите на YouTube.", // mixed scripts: one Cyrillic letter clears the cue
		"COVID-19.",            // digit guard: never dropped
		"127.0.0.1",            // digits only
		"«…»!?",                // punctuation only: no letters, not foreign
		"",                     // empty
	}
	for _, s := range keep {
		if g.IsForeign(s) {
			t.Errorf("IsForeign(%q) = true, want false", s)
		}
	}
}

// TestScriptGuardEveryTableIsScriptLevel pins that the guard is script-level,
// not language-level: each table in the accepted list judges its own script's
// text as native and another script's text as foreign, with no per-language
// list involved. A table that accepted nothing (a typo in the table name) would
// drop every cue on that track.
func TestScriptGuardEveryTableIsScriptLevel(t *testing.T) {
	native := map[string]string{
		"cyrillic":   "речь",
		"latin":      "speech",
		"greek":      "λόγος",
		"arabic":     "كلام",
		"hebrew":     "דיבור",
		"georgian":   "სიტყვა",
		"armenian":   "խոսք",
		"han":        "话语",
		"hangul":     "말",
		"devanagari": "भाषण",
	}
	for name, text := range native {
		g, err := subtitle.NewScriptGuard(name)
		if err != nil {
			t.Fatalf("NewScriptGuard(%q): %v", name, err)
		}
		if g.IsForeign(text) {
			t.Errorf("%s: native text %q judged foreign", name, text)
		}
		other := "речь"
		if name == "cyrillic" {
			other = "speech"
		}
		if !g.IsForeign(other) {
			t.Errorf("%s: other-script text %q not judged foreign", name, other)
		}
	}
}

// TestScriptGuardInactiveAndErrors pins that an empty name yields an inactive
// guard (never foreign), name matching is case and whitespace tolerant, and an
// unknown name is rejected at construction with the accepted names listed.
func TestScriptGuardInactiveAndErrors(t *testing.T) {
	off, err := subtitle.NewScriptGuard("")
	if err != nil {
		t.Fatalf("NewScriptGuard(\"\"): %v", err)
	}
	if off.Active() {
		t.Fatalf("empty script name should be inactive")
	}
	if off.IsForeign("Elola alolo.") {
		t.Fatalf("inactive guard flagged a cue")
	}
	var nilGuard *subtitle.ScriptGuard
	if nilGuard.Active() || nilGuard.IsForeign("Elola alolo.") {
		t.Fatalf("a nil guard must be inactive and never foreign")
	}
	if _, err := subtitle.NewScriptGuard("  Cyrillic "); err != nil {
		t.Fatalf("name should be trimmed and lowercased: %v", err)
	}
	_, err = subtitle.NewScriptGuard("klingon")
	if err == nil {
		t.Fatalf("unknown script name should error")
	}
	for _, want := range []string{"klingon", "cyrillic", "latin", "devanagari"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must name the bad value and list the accepted names (missing %q)", err, want)
		}
	}
}

// TestCleanCuesDropsForeignScript pins the CleanCues wiring: with an expected
// script configured, wrong-script cues are dropped and survivors re-indexed
// gap-free, while digit-bearing and mixed-script cues stay.
func TestCleanCuesDropsForeignScript(t *testing.T) {
	g, err := subtitle.NewScriptGuard("cyrillic")
	if err != nil {
		t.Fatalf("NewScriptGuard: %v", err)
	}
	cues := []subtitle.Cue{
		{Index: 1, StartMS: 0, EndMS: 1000, Text: "Elola alolo."},
		{Index: 2, StartMS: 1000, EndMS: 2000, Text: "Настоящая речь."},
		{Index: 3, StartMS: 2000, EndMS: 3000, Text: "Iiiiiiii."},
		{Index: 4, StartMS: 3000, EndMS: 4000, Text: "COVID-19."},
		{Index: 5, StartMS: 4000, EndMS: 5000, Text: "Ещё речь про YouTube."},
	}
	got := subtitle.CleanCues(cues, subtitle.CleanOptions{Script: g})
	if len(got) != 3 {
		t.Fatalf("expected 3 surviving cues, got %d: %+v", len(got), got)
	}
	if got[0].Text != "Настоящая речь." || got[1].Text != "COVID-19." || got[2].Text != "Ещё речь про YouTube." {
		t.Fatalf("wrong cues survived: %+v", got)
	}
	for i := range got {
		if got[i].Index != i+1 {
			t.Errorf("survivor %d has Index %d, want %d", i, got[i].Index, i+1)
		}
	}
	// The guard alone must make the options active; otherwise CleanCues
	// short-circuits and exports the gibberish unchanged.
	if !(subtitle.CleanOptions{Script: g}).Active() {
		t.Fatalf("CleanOptions with only a script guard must be active")
	}
}
