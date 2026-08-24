package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// TestScriptGuardForeignDetection pins the wrong-script verdict for a Cyrillic
// track: all-Latin gibberish is foreign; cues with any Cyrillic letter, any
// digit, or no letters at all are not.
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
		"Обычная речь.",         // expected script
		"Смотрите на YouTube.",  // mixed scripts: one Cyrillic letter clears the cue
		"COVID-19.",             // digit guard: never dropped
		"127.0.0.1",             // digits only
		"— …!?",                 // punctuation only: no letters, not foreign
		"",                      // empty
	}
	for _, s := range keep {
		if g.IsForeign(s) {
			t.Errorf("IsForeign(%q) = true, want false", s)
		}
	}
}

// TestScriptGuardInactiveAndErrors pins that an empty name yields an inactive
// guard (never foreign), name matching is case/whitespace tolerant, and an
// unknown name is rejected at construction.
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
	if _, err := subtitle.NewScriptGuard("  Cyrillic "); err != nil {
		t.Fatalf("name should be trimmed and lowercased: %v", err)
	}
	if _, err := subtitle.NewScriptGuard("klingon"); err == nil {
		t.Fatalf("unknown script name should error")
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
}
