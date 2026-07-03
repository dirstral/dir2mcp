package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// encodeUTF16 encodes s as UTF-16 with the given endianness and BOM policy. Used
// to build non-UTF-8 fixtures for the charset-detection tests.
func encodeUTF16(t *testing.T, e unicode.Endianness, bom unicode.BOMPolicy, s string) []byte {
	t.Helper()
	out, _, err := transform.Bytes(unicode.UTF16(e, bom).NewEncoder(), []byte(s))
	if err != nil {
		t.Fatalf("encode UTF-16: %v", err)
	}
	return out
}

// TestNormalizeUTF8_StripsUTF8BOM pins that a leading UTF-8 BOM (EF BB BF) is
// removed so it neither becomes a junk first token nor hides a markdown heading
// (#417 item 6).
func TestNormalizeUTF8_StripsUTF8BOM(t *testing.T) {
	in := append([]byte{0xEF, 0xBB, 0xBF}, []byte("# Heading\nbody")...)
	got := ingest.NormalizeUTF8(in)
	want := []byte("# Heading\nbody")
	if !bytes.Equal(got, want) {
		t.Fatalf("NormalizeUTF8 = %q, want %q (BOM not stripped)", got, want)
	}
}

// TestNormalizeUTF8_DecodesUTF16 pins that UTF-16 text — which is "valid UTF-8"
// only because of its NUL padding — is transcoded to real UTF-8 rather than
// indexed as NUL-interleaved garbage (#417 item 3). Covers BOM LE/BE and the
// BOM-less LE heuristic; script-agnostic (accented Latin + Cyrillic).
func TestNormalizeUTF8_DecodesUTF16(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"le-bom", encodeUTF16(t, unicode.LittleEndian, unicode.UseBOM, "Héllo wörld"), "Héllo wörld"},
		{"be-bom", encodeUTF16(t, unicode.BigEndian, unicode.UseBOM, "Café — Привет"), "Café — Привет"},
		{"le-nobom", encodeUTF16(t, unicode.LittleEndian, unicode.IgnoreBOM, "plain ascii sentence long enough"), "plain ascii sentence long enough"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(ingest.NormalizeUTF8(tc.in))
			if got != tc.want {
				t.Fatalf("NormalizeUTF8 = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNormalizeUTF8_UTF16BOMOddTailYieldsReplacement pins that once a UTF-16 BOM
// has classified the stream, a stray trailing byte (odd payload length) is
// salvaged as U+FFFD rather than surviving as a raw interleaved NUL. NUL padding
// is itself valid UTF-8, so returning the raw bytes would let them masquerade as
// valid text; the BOM's authority means the output must be UTF-8 text or a
// replacement char (Copilot finding on transcodeUTF16).
func TestNormalizeUTF8_UTF16BOMOddTailYieldsReplacement(t *testing.T) {
	// UTF-16LE BOM (FF FE) followed by a lone 0x00: an incomplete final code
	// unit. Must not yield the raw 0x00 byte.
	in := []byte{0xFF, 0xFE, 0x00}
	got := ingest.NormalizeUTF8(in)
	if bytes.IndexByte(got, 0x00) != -1 {
		t.Fatalf("NormalizeUTF8 = %q, must not contain a raw NUL byte", got)
	}
	if want := []byte("�"); !bytes.Equal(got, want) {
		t.Fatalf("NormalizeUTF8 = %q, want %q (U+FFFD replacement)", got, want)
	}
}

// TestNormalizeUTF8_DecodesBOMlessUTF16OddTail pins that a BOM-less UTF-16 body
// with a trailing odd byte is still detected and decoded, rather than being
// rejected outright by an even-length gate (optibot finding on sniffUTF16). The
// stray byte is salvaged as U+FFFD; the real text must survive.
func TestNormalizeUTF8_DecodesBOMlessUTF16OddTail(t *testing.T) {
	body := encodeUTF16(t, unicode.LittleEndian, unicode.IgnoreBOM, "plain ascii sentence long enough")
	in := append(append([]byte{}, body...), 0x41) // stray trailing odd byte
	got := string(ingest.NormalizeUTF8(in))
	if !strings.Contains(got, "plain ascii sentence long enough") {
		t.Fatalf("NormalizeUTF8 = %q, want decoded UTF-16 text (odd-tail body not detected)", got)
	}
	if strings.ContainsRune(got, 0x00) {
		t.Fatalf("NormalizeUTF8 = %q, must not contain a raw NUL byte", got)
	}
}

// TestNormalizeUTF8_DecodesLegacySingleByte pins that invalid-UTF-8 legacy
// single-byte text (Latin-1 / Windows-1252) is decoded to real characters
// instead of every accented byte being destroyed into U+FFFD (#417 item 3).
func TestNormalizeUTF8_DecodesLegacySingleByte(t *testing.T) {
	// "café résumé — ü" in Windows-1252: é=0xE9, ü=0xFC, em dash=0x97.
	in := []byte{'c', 'a', 'f', 0xE9, ' ', 'r', 0xE9, 's', 'u', 'm', 0xE9, ' ', 0x97, ' ', 0xFC}
	want := "café résumé — ü"
	got := string(ingest.NormalizeUTF8(in))
	if got != want {
		t.Fatalf("NormalizeUTF8 = %q, want %q", got, want)
	}
}

// TestNormalizeUTF8_KeepsValidUTF8NonLatin pins that valid multi-byte UTF-8
// (Cyrillic here) is NOT mistaken for a legacy single-byte encoding and passes
// through unchanged — the detector must be script-agnostic, not Latin-biased.
func TestNormalizeUTF8_KeepsValidUTF8NonLatin(t *testing.T) {
	for _, s := range []string{"Привет, мир!", "こんにちは世界", "Hello 世界 🌍"} {
		if got := string(ingest.NormalizeUTF8([]byte(s))); got != s {
			t.Errorf("NormalizeUTF8(%q) = %q, want unchanged", s, got)
		}
	}
}

// TestExtractTitle_StripsLeadingBOM pins that a BOM-prefixed markdown file still
// yields its heading as the title (#417 item 6): the BOM must not shadow the
// leading `#`.
func TestExtractTitle_StripsLeadingBOM(t *testing.T) {
	if got := ingest.ExtractTitle("\uFEFF# Real Heading\nbody text"); got != "Real Heading" {
		t.Fatalf("ExtractTitle with BOM = %q, want %q", got, "Real Heading")
	}
}

// TestGenerateRawText_DetectsNonLatinLanguage pins that a raw_text rep in a
// non-Latin script is language-tagged when detection is enabled (#417 item 4),
// so the `languages` filter no longer silently excludes it. Script-agnostic:
// uses Russian to prove the tagging is not Latin-only.
func TestGenerateRawText_DetectsNonLatinLanguage(t *testing.T) {
	st := &fakeRepStore{failAfter: -1}
	rg := ingest.NewRepresentationGenerator(st)
	rg.SetLanguageDetection(true)
	doc := model.Document{DocID: 1, RelPath: "заметки.txt", DocType: "text"}
	content := []byte("Совет директоров внимательно рассмотрел каждое заявление и затем утвердил бюджет на предстоящий финансовый год для всех региональных отделений компании.")
	if err := rg.GenerateRawTextFromContent(context.Background(), doc, content); err != nil {
		t.Fatalf("GenerateRawTextFromContent failed: %v", err)
	}
	if len(st.reps) == 0 {
		t.Fatal("no representation recorded")
	}
	var meta struct {
		Language       string `json:"language"`
		LanguageSource string `json:"language_source"`
	}
	if err := json.Unmarshal([]byte(st.reps[0].MetaJSON), &meta); err != nil {
		t.Fatalf("meta_json %q is not valid JSON: %v", st.reps[0].MetaJSON, err)
	}
	if meta.Language != "ru" {
		t.Errorf("detected language = %q, want ru (meta=%q)", meta.Language, st.reps[0].MetaJSON)
	}
	if meta.LanguageSource != "detected" {
		t.Errorf("language_source = %q, want detected", meta.LanguageSource)
	}
}
