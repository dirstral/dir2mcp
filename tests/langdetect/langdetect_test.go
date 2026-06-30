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
