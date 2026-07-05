package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// TestGlossaryReplacesWholeWordCaseInsensitive pins that a glossary rule rewrites
// matching whole words case-insensitively, absorbs regex spelling variants, and
// leaves non-matching substrings alone.
func TestGlossaryReplacesWholeWordCaseInsensitive(t *testing.T) {
	g, err := subtitle.NewGlossary([]string{"Aju?bei=>Adzhubei", "Khruschev=>Khrushchev"})
	if err != nil {
		t.Fatalf("NewGlossary: %v", err)
	}
	cases := map[string]string{
		"signed by Ajubei today":  "signed by Adzhubei today", // variant with 'u'
		"signed by Ajbei today":   "signed by Adzhubei today", // variant without 'u'
		"AJUBEI spoke":            "Adzhubei spoke",           // case-insensitive
		"met Khruschev in Moscow": "met Khrushchev in Moscow",
		"Ajubeivich is untouched": "Ajubeivich is untouched", // word-bounded: no partial rewrite
	}
	for in, want := range cases {
		if got := g.Apply(in); got != want {
			t.Errorf("Apply(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGlossaryAlternationStaysWordBounded pins that a top-level alternation in a
// glossary pattern keeps BOTH word boundaries anchoring the whole pattern (the
// pattern is wrapped in a non-capturing group), so it never rewrites partial
// words like "Ajubeyond" or "xAdju".
func TestGlossaryAlternationStaysWordBounded(t *testing.T) {
	g, err := subtitle.NewGlossary([]string{"Aju|Adju=>Adzhubei"})
	if err != nil {
		t.Fatalf("NewGlossary: %v", err)
	}
	cases := map[string]string{
		"met Aju today":       "met Adzhubei today",  // left alternative, whole word
		"met Adju today":      "met Adzhubei today",  // right alternative, whole word
		"Ajubeyond the fold":  "Ajubeyond the fold",  // left alt must not match a prefix
		"saw xAdju in a note": "saw xAdju in a note", // right alt must not match a suffix
	}
	for in, want := range cases {
		if got := g.Apply(in); got != want {
			t.Errorf("Apply(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGlossaryInactiveAndErrors pins that an empty glossary is a no-op and that
// malformed entries are rejected at construction (so config can fail fast).
func TestGlossaryInactiveAndErrors(t *testing.T) {
	empty, err := subtitle.NewGlossary(nil)
	if err != nil {
		t.Fatalf("NewGlossary(nil): %v", err)
	}
	if empty.Active() {
		t.Fatalf("nil glossary should be inactive")
	}
	if got := empty.Apply("unchanged"); got != "unchanged" {
		t.Fatalf("inactive Apply changed text: %q", got)
	}

	if _, err := subtitle.NewGlossary([]string{"no-arrow-here"}); err == nil {
		t.Fatalf("entry without separator should error")
	}
	if _, err := subtitle.NewGlossary([]string{"=>only-replacement"}); err == nil {
		t.Fatalf("entry with empty pattern should error")
	}
	if _, err := subtitle.NewGlossary([]string{"a(b=>c"}); err == nil {
		t.Fatalf("entry with invalid regexp should error")
	}
}

// TestDropSetSpamDetection pins the drop-phrase semantics: a cue composed
// ENTIRELY of configured phrases (plus punctuation) is spam, while a cue that
// also carries real words survives, and an inactive set never flags spam.
func TestDropSetSpamDetection(t *testing.T) {
	d, err := subtitle.NewDropSet([]string{"Донбасс|Крым|Украина|Иван|Плющ|НАТО"})
	if err != nil {
		t.Fatalf("NewDropSet: %v", err)
	}
	if !d.Active() {
		t.Fatalf("drop set with one rule should be active")
	}
	spam := []string{
		"Донбасс, Крым, Украина, Иван Плющ, НАТО.",
		"НАТО.",
		"Крым, НАТО",
		"украина", // case-insensitive
	}
	for _, s := range spam {
		if !d.IsSpam(s) {
			t.Errorf("IsSpam(%q) = false, want true", s)
		}
	}
	keep := []string{
		"Крым сегодня, нет Крыма.", // real words remain
		"Что дальше будет, никто не знает.",
		"Иван Плющ был депутатом.", // "был депутатом" survives
	}
	for _, s := range keep {
		if d.IsSpam(s) {
			t.Errorf("IsSpam(%q) = true, want false", s)
		}
	}
}

// TestDropSetInactiveAndErrors pins that an empty set is inactive (never spam)
// and that an invalid regexp is rejected at construction.
func TestDropSetInactiveAndErrors(t *testing.T) {
	empty, err := subtitle.NewDropSet(nil)
	if err != nil {
		t.Fatalf("NewDropSet(nil): %v", err)
	}
	if empty.Active() {
		t.Fatalf("nil drop set should be inactive")
	}
	if empty.IsSpam("НАТО.") {
		t.Fatalf("inactive drop set flagged spam")
	}
	if _, err := subtitle.NewDropSet([]string{"a(b"}); err == nil {
		t.Fatalf("invalid regexp should error")
	}
}

// TestCleanCuesDropsPhrases pins that CleanCues drops phrase-only cues while
// keeping cues that mix a phrase word with real speech, and re-indexes gap-free.
func TestCleanCuesDropsPhrases(t *testing.T) {
	d, err := subtitle.NewDropSet([]string{"Крым|НАТО"})
	if err != nil {
		t.Fatalf("NewDropSet: %v", err)
	}
	cues := []subtitle.Cue{
		{Index: 1, StartMS: 0, EndMS: 1000, Text: "Крым, НАТО."},
		{Index: 2, StartMS: 1000, EndMS: 2000, Text: "Крым сегодня наш дом."},
		{Index: 3, StartMS: 2000, EndMS: 3000, Text: "НАТО"},
		{Index: 4, StartMS: 3000, EndMS: 4000, Text: "Real speech here"},
	}
	got := subtitle.CleanCues(cues, subtitle.CleanOptions{Drop: d})
	if len(got) != 2 {
		t.Fatalf("expected 2 surviving cues, got %d: %+v", len(got), got)
	}
	if got[0].Text != "Крым сегодня наш дом." || got[1].Text != "Real speech here" {
		t.Fatalf("wrong cues survived: %+v", got)
	}
	for i := range got {
		if got[i].Index != i+1 {
			t.Errorf("survivor %d has Index %d, want %d", i, got[i].Index, i+1)
		}
	}
}

// TestCleanCuesInactiveIsIdentity pins that a zero CleanOptions returns cues
// unchanged (same order, same content), so the empty-config path is a no-op.
func TestCleanCuesInactiveIsIdentity(t *testing.T) {
	cues := []subtitle.Cue{
		{Index: 1, StartMS: 0, EndMS: 1000, Text: "one"},
		{Index: 2, StartMS: 1000, EndMS: 2000, Text: "two"},
	}
	got := subtitle.CleanCues(cues, subtitle.CleanOptions{})
	if len(got) != 2 || got[0].Text != "one" || got[1].Text != "two" {
		t.Fatalf("inactive CleanCues altered cues: %+v", got)
	}
}

// TestCleanCuesDropsURLs pins that URL/domain/credit cues are dropped when
// enabled, while ordinary speech survives.
func TestCleanCuesDropsURLs(t *testing.T) {
	cues := []subtitle.Cue{
		{Index: 1, StartMS: 0, EndMS: 1000, Text: "Real speech here"},
		{Index: 2, StartMS: 1000, EndMS: 2000, Text: "Subtitles by www.example.com"},
		{Index: 3, StartMS: 2000, EndMS: 3000, Text: "visit https://foo.bar"},
		{Index: 4, StartMS: 3000, EndMS: 4000, Text: "see amara.org for more"},
		{Index: 5, StartMS: 4000, EndMS: 5000, Text: "More real speech"},
	}
	got := subtitle.CleanCues(cues, subtitle.CleanOptions{DropURLs: true})
	if len(got) != 2 {
		t.Fatalf("expected 2 surviving cues, got %d: %+v", len(got), got)
	}
	if got[0].Text != "Real speech here" || got[1].Text != "More real speech" {
		t.Fatalf("wrong cues survived: %+v", got)
	}
	for i := range got {
		if got[i].Index != i+1 {
			t.Errorf("survivor %d has Index %d, want %d", i, got[i].Index, i+1)
		}
	}
}

// TestCleanCuesCollapseRepeats pins the repetition-collapse: a long identical run
// keeps the first (threshold-1) cues and drops the rest, while a short repeat and
// a distinct cue are preserved.
func TestCleanCuesCollapseRepeats(t *testing.T) {
	mk := func(texts ...string) []subtitle.Cue {
		out := make([]subtitle.Cue, 0, len(texts))
		for i, tx := range texts {
			out = append(out, subtitle.Cue{Index: i + 1, StartMS: i * 1000, EndMS: (i + 1) * 1000, Text: tx})
		}
		return out
	}
	// Five identical "No." in a row, then a distinct cue, then two identical.
	cues := mk("No.", "No.", "No.", "No.", "No.", "Yes.", "Ok.", "Ok.")
	got := subtitle.CleanCues(cues, subtitle.CleanOptions{CollapseRepeats: 3})
	var texts []string
	for _, c := range got {
		texts = append(texts, c.Text)
	}
	// Run of 5 "No." -> keep first 2; "Yes." kept; run of 2 "Ok." (< 3) both kept.
	want := []string{"No.", "No.", "Yes.", "Ok.", "Ok."}
	if len(texts) != len(want) {
		t.Fatalf("collapse got %v, want %v", texts, want)
	}
	for i := range want {
		if texts[i] != want[i] {
			t.Fatalf("collapse got %v, want %v", texts, want)
		}
	}
}

// TestCleanCuesGlossaryRewrites pins that glossary rewrites are applied to
// surviving cues after the drop/collapse passes.
func TestCleanCuesGlossaryRewrites(t *testing.T) {
	g, err := subtitle.NewGlossary([]string{"Ajubei=>Adzhubei"})
	if err != nil {
		t.Fatalf("NewGlossary: %v", err)
	}
	cues := []subtitle.Cue{{Index: 1, StartMS: 0, EndMS: 1000, Text: "letter from Ajubei"}}
	got := subtitle.CleanCues(cues, subtitle.CleanOptions{Glossary: g})
	if len(got) != 1 || got[0].Text != "letter from Adzhubei" {
		t.Fatalf("glossary not applied in CleanCues: %+v", got)
	}
}
