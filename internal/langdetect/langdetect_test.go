package langdetect

import "testing"

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
		tag, conf, ok := Detect(text, 0)
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
		if tag, _, ok := Detect(text, DefaultMinConfidence); ok {
			t.Errorf("Detect(%q) ok=true (tag=%q), want unknown (too little letter signal)", text, tag)
		}
	}
}

func TestDetect_ConfidenceFloor(t *testing.T) {
	text := knownText["en"]
	// An impossible floor rejects everything (left unknown).
	if _, _, ok := Detect(text, 1.01); ok {
		t.Error("Detect with floor 1.01 should reject every result")
	}
	// A zero floor accepts a clearly-detectable language.
	if _, _, ok := Detect(text, 0); !ok {
		t.Error("Detect with floor 0 should accept a clear language")
	}
}

func TestDetect_Deterministic(t *testing.T) {
	text := knownText["de"]
	t1, c1, ok1 := Detect(text, DefaultMinConfidence)
	t2, c2, ok2 := Detect(text, DefaultMinConfidence)
	if t1 != t2 || c1 != c2 || ok1 != ok2 {
		t.Errorf("Detect not deterministic: (%q,%v,%v) vs (%q,%v,%v)", t1, c1, ok1, t2, c2, ok2)
	}
}
