package langdetect_test

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/langdetect"
)

// Clear, monolingual sentences long enough for trigram detection.
var knownText = map[string]string{
	"en": "The quick brown fox jumps over the lazy dog and then runs across the open field every single morning before the sun rises.",
	"fr": "Le renard brun et rapide saute par-dessus le chien paresseux puis court à travers le champ ouvert chaque matin avant le lever du soleil.",
	"de": "Der schnelle braune Fuchs springt über den faulen Hund und läuft dann jeden Morgen über das offene Feld bevor die Sonne aufgeht.",
	"es": "El rápido zorro marrón salta sobre el perro perezoso y luego corre por el campo abierto todas las mañanas antes de que salga el sol.",
}

func TestDetect_KnownLanguages(t *testing.T) {
	// Use floor 0 here: this asserts the language MAPPING, not the floor.
	for want, text := range knownText {
		tag, conf, ok := langdetect.Detect(text, 0)
		if !ok {
			t.Errorf("Detect(%s sample) ok=false, want %q", want, want)
			continue
		}
		if tag != want {
			t.Errorf("Detect(%s sample) = %q (conf %.2f), want %q", want, tag, conf, want)
		}
	}
}

func TestDetect_ShortOrLowSignalIsUnknown(t *testing.T) {
	for _, text := range []string{"", "   ", "hi", "a b c", "12345 !!! @@@ ---", "42 + 7 = 49"} {
		if tag, _, ok := langdetect.Detect(text, langdetect.DefaultMinConfidence); ok {
			t.Errorf("Detect(%q) ok=true (tag=%q), want unknown (too little letter signal)", text, tag)
		}
	}
}

func TestDetect_ConfidenceFloor(t *testing.T) {
	text := knownText["en"]
	// An impossible floor rejects everything (left unknown).
	if _, _, ok := langdetect.Detect(text, 1.01); ok {
		t.Error("Detect with floor 1.01 should reject every result")
	}
	// A zero floor accepts a clearly-detectable language.
	if _, _, ok := langdetect.Detect(text, 0); !ok {
		t.Error("Detect with floor 0 should accept a clear language")
	}
}

func TestDetect_Deterministic(t *testing.T) {
	text := knownText["de"]
	t1, c1, ok1 := langdetect.Detect(text, langdetect.DefaultMinConfidence)
	t2, c2, ok2 := langdetect.Detect(text, langdetect.DefaultMinConfidence)
	if t1 != t2 || c1 != c2 || ok1 != ok2 {
		t.Errorf("Detect not deterministic: (%q,%v,%v) vs (%q,%v,%v)", t1, c1, ok1, t2, c2, ok2)
	}
}

func TestDetect_TruncatesLongInputButStillDetects(t *testing.T) {
	// A large natural-English document (well over the detector's internal prefix
	// cap) must still detect correctly. Uses a varied multi-sentence block (not a
	// repeated short phrase, which is degenerate and scores low confidence).
	block := "The committee reviewed the annual report in detail. " +
		"Several members raised concerns about the regional budget allocations. " +
		"After a long discussion, the proposal was approved by a clear majority. " +
		"The findings will be published next quarter alongside the audit summary. " +
		"Stakeholders across every office welcomed the transparent new process. "
	big := strings.Repeat(block, 50) // ~14 KB, far exceeds the internal cap
	tag, _, ok := langdetect.Detect(big, langdetect.DefaultMinConfidence)
	if !ok || tag != "en" {
		t.Errorf("Detect(large en doc) = (%q, ok=%v), want en/true", tag, ok)
	}
}

// Varied multi-sentence blocks per language (a repeated short phrase is
// degenerate and scores low confidence).
var longBlock = map[string]string{
	"en": "The committee reviewed the annual report in detail. " +
		"Several members raised concerns about the regional budget allocations. " +
		"After a long discussion, the proposal was approved by a clear majority. " +
		"The findings will be published next quarter alongside the audit summary. ",
	"de": "Der Ausschuss hat den Jahresbericht ausführlich geprüft. " +
		"Mehrere Mitglieder äußerten Bedenken zur Verteilung der regionalen Mittel. " +
		"Nach einer langen Diskussion wurde der Vorschlag mit klarer Mehrheit angenommen. " +
		"Die Ergebnisse werden im nächsten Quartal zusammen mit dem Prüfbericht veröffentlicht. ",
	"fr": "Le comité a examiné le rapport annuel en détail. " +
		"Plusieurs membres ont exprimé des inquiétudes sur la répartition du budget régional. " +
		"Après une longue discussion, la proposition a été adoptée à une nette majorité. " +
		"Les conclusions seront publiées le trimestre prochain avec le résumé de l'audit. ",
	"uk": "Сьогодні ми говоримо про те, як змінилася країна за останні роки. " +
		"Міністр відповів, що уряд готує нові рішення для підтримки людей. " +
		"Це питання дуже важливе для кожної родини, яка живе в нашому місті. " +
		"Ми продовжимо розмову після короткої перерви, залишайтеся з нами. ",
}

// mixedText returns the lead block repeated to at least leadBytes, then the body
// block repeated until the whole text is at least totalBytes.
func mixedText(lead string, leadBytes int, body string, totalBytes int) string {
	var b strings.Builder
	for b.Len() < leadBytes {
		b.WriteString(lead)
	}
	for b.Len() < totalBytes {
		b.WriteString(body)
	}
	return b.String()
}

// TestDetect_LongTextLeadingPassageDoesNotDecide is the #1135 rule (SPEC §8.8):
// a long text is sampled across its length, so a leading passage in another
// language does not decide the language of the whole text.
func TestDetect_LongTextLeadingPassageDoesNotDecide(t *testing.T) {
	text := mixedText(longBlock["en"], 5*1024, longBlock["de"], 20*1024)
	tag, conf, ok := langdetect.Detect(text, langdetect.DefaultMinConfidence)
	if !ok || tag != "de" {
		t.Fatalf("Detect(5 KiB en + 15 KiB de) = (%q, %.2f, %v), want de", tag, conf, ok)
	}
	if conf <= 0 || conf > 1 {
		t.Errorf("confidence %.3f outside (0,1]", conf)
	}
}

// TestDetect_LongTextMajorityAtStartStillWins: sampling is a vote, not a bias
// against the start. A long text that is mostly English with a German tail is
// English.
func TestDetect_LongTextMajorityAtStartStillWins(t *testing.T) {
	text := mixedText(longBlock["en"], 16*1024, longBlock["de"], 20*1024)
	if tag, _, ok := langdetect.Detect(text, langdetect.DefaultMinConfidence); !ok || tag != "en" {
		t.Fatalf("Detect(16 KiB en + 4 KiB de) = (%q, %v), want en", tag, ok)
	}
}

// TestDetect_LongTextSkipsLowSignalSamples: a long stretch with no letters (a
// timestamp-only or music-only passage) is skipped, not counted as a language.
func TestDetect_LongTextSkipsLowSignalSamples(t *testing.T) {
	noise := strings.Repeat("[00:00] ... 12345 ♪ ♪ ", 600)
	text := noise + mixedText(longBlock["fr"], 0, longBlock["fr"], 8*1024)
	if tag, _, ok := langdetect.Detect(text, langdetect.DefaultMinConfidence); !ok || tag != "fr" {
		t.Fatalf("Detect(noise + fr) = (%q, %v), want fr", tag, ok)
	}
}

// TestDetect_LongTextNoSignalIsUnknown: a long text with no letter signal in
// any sample stays unknown.
func TestDetect_LongTextNoSignalIsUnknown(t *testing.T) {
	text := strings.Repeat("12345 !!! @@@ --- ", 2000)
	if tag, _, ok := langdetect.Detect(text, langdetect.DefaultMinConfidence); ok {
		t.Fatalf("Detect(long noise) ok=true (tag=%q), want unknown", tag)
	}
}

// TestDetect_LongTextDeterministicAndRuneSafe: the sampled vote is stable for
// identical input, and a multi-byte text whose sample cuts land inside runes
// still detects (no sample is cut inside a rune).
func TestDetect_LongTextDeterministicAndRuneSafe(t *testing.T) {
	text := "x" + mixedText(longBlock["uk"], 0, longBlock["uk"], 30*1024)
	t1, c1, ok1 := langdetect.Detect(text, langdetect.DefaultMinConfidence)
	t2, c2, ok2 := langdetect.Detect(text, langdetect.DefaultMinConfidence)
	if t1 != t2 || c1 != c2 || ok1 != ok2 {
		t.Fatalf("Detect not deterministic: (%q,%v,%v) vs (%q,%v,%v)", t1, c1, ok1, t2, c2, ok2)
	}
	if !ok1 || t1 != "uk" {
		t.Fatalf("Detect(long uk) = (%q, %v), want uk", t1, ok1)
	}
}
