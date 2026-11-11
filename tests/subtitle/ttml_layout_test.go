package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// SPEC §8.6.10 TTML layout: media.subtitles.ttml.layout and
// media.subtitles.ttml.language_codes.

var layoutCues = []subtitle.BilingualCue{
	{StartMS: 0, EndMS: 2000, PrimaryLang: "ru", PrimaryText: "Привет", SecondaryLang: "en", SecondaryText: "Hello"},
	{StartMS: 2000, EndMS: 4500, PrimaryLang: "ru", PrimaryText: "Две\nстроки"},
	{StartMS: 9000, EndMS: 9500, SecondaryLang: "en", SecondaryText: "Tom & Jerry"},
}

// TestRenderTTMLWith_DefaultsKeepTheSpansOutput pins that the zero options and
// the explicit defaults render exactly what RenderTTML always did.
func TestRenderTTMLWith_DefaultsKeepTheSpansOutput(t *testing.T) {
	want := subtitle.RenderTTML(layoutCues, "ru")
	for _, opts := range []subtitle.TTMLOptions{{}, {Layout: "spans", LanguageCodes: "bcp47"}} {
		if got := subtitle.RenderTTMLWith(layoutCues, "ru", opts); got != want {
			t.Fatalf("%+v changed the default output:\n%s\nwant:\n%s", opts, got, want)
		}
	}
}

// TestRenderTTMLWith_DivsIsOneDivPerLanguage pins the layout Wowza needs: one
// <div xml:lang> per language (primary first), plain-text <p> cues with the
// shared time region, ISO 639-2 codes everywhere, and a cue omitted from a
// language it has no text in.
func TestRenderTTMLWith_DivsIsOneDivPerLanguage(t *testing.T) {
	got := subtitle.RenderTTMLWith(layoutCues, "ru", subtitle.TTMLOptions{Layout: "divs", LanguageCodes: "iso639_2"})
	want := `<?xml version="1.0" encoding="UTF-8"?>
<tt xmlns="http://www.w3.org/ns/ttml" xml:lang="rus">
  <body>
    <div xml:lang="rus">
      <p begin="00:00:00.000" end="00:00:02.000">Привет</p>
      <p begin="00:00:02.000" end="00:00:04.500">Две<br/>строки</p>
    </div>
    <div xml:lang="eng">
      <p begin="00:00:00.000" end="00:00:02.000">Hello</p>
      <p begin="00:00:09.000" end="00:00:09.500">Tom &amp; Jerry</p>
    </div>
  </body>
</tt>
`
	if got != want {
		t.Fatalf("divs layout:\n%s\nwant:\n%s", got, want)
	}

	// It reads back as two languages with their own cues.
	langs, err := subtitle.ParseTTMLByLang(got)
	if err != nil {
		t.Fatal(err)
	}
	perLang := map[string]int{}
	for _, l := range langs {
		perLang[l.Lang] = len(l.Cues)
	}
	if len(langs) != 2 || perLang["rus"] != 2 || perLang["eng"] != 2 {
		t.Fatalf("parsed back as %+v", langs)
	}

	// Monolingual: one div.
	mono := subtitle.RenderTTMLWith(layoutCues[1:2], "ru", subtitle.TTMLOptions{Layout: "divs"})
	if langs, _ := subtitle.ParseTTMLByLang(mono); len(langs) != 1 || langs[0].Lang != "ru" {
		t.Fatalf("monolingual divs output parsed as %+v:\n%s", langs, mono)
	}
}

// TestLanguageCode pins the ISO 639-2 mapping and the pass-through cases.
func TestLanguageCode(t *testing.T) {
	cases := []struct{ tag, scheme, want string }{
		{"ru", "iso639_2", "rus"},
		{"en-US", "ISO639_2", "eng"},
		{"uk", "iso639_2", "ukr"},
		{"fr", "iso639_2", "fra"}, // ISO 639-2/T, not the bibliographic "fre"
		{"de", "iso639_2", "deu"},
		{"ru", "bcp47", "ru"},
		{"ru", "", "ru"},
		{"zz-not-a-language", "iso639_2", "zz-not-a-language"},
		{"", "iso639_2", ""},
	}
	for _, c := range cases {
		if got := subtitle.LanguageCode(c.tag, c.scheme); got != c.want {
			t.Fatalf("LanguageCode(%q, %q) = %q, want %q", c.tag, c.scheme, got, c.want)
		}
	}
}

// TestRenderTTMLWith_DivsKeepColliding639Codes pins the documented collision
// rule: two languages that share a primary subtag get the same ISO 639-2 code
// under iso639_2, and the TTML still keeps one <div> per language, primary
// first, rather than merging them.
func TestRenderTTMLWith_DivsKeepColliding639Codes(t *testing.T) {
	cues := subtitle.SeparateLanguageCues(
		[]subtitle.Cue{{StartMS: 0, EndMS: 1000, Text: "Olá, Brasil"}},
		[]subtitle.Cue{{StartMS: 0, EndMS: 1000, Text: "Olá, Portugal"}},
		"pt-BR", "pt-PT")
	got := subtitle.RenderTTMLWith(cues, "pt-BR", subtitle.TTMLOptions{Layout: "divs", LanguageCodes: "iso639_2"})
	br := strings.Index(got, "Olá, Brasil")
	pt := strings.Index(got, "Olá, Portugal")
	if strings.Count(got, `<div xml:lang="por">`) != 2 || br < 0 || pt < 0 || br > pt {
		t.Fatalf("want two por divs, pt-BR first:\n%s", got)
	}
}

// TestTTMLLanguages pins the language list a companion SMIL is built from:
// every language-tagged block or run, unique, in document order, never the
// document root's own tag.
func TestTTMLLanguages(t *testing.T) {
	spans := subtitle.RenderTTML(layoutCues, "ru")
	divs := subtitle.RenderTTMLWith(layoutCues, "ru", subtitle.TTMLOptions{Layout: "divs", LanguageCodes: "iso639_2"})
	untagged := `<tt xmlns="http://www.w3.org/ns/ttml" xml:lang="ru"><body><div><p begin="00:00:00.000" end="00:00:01.000">x</p></div></body></tt>`
	for name, c := range map[string]struct {
		doc  string
		want string
	}{
		"spans":    {spans, "ru,en"},
		"divs":     {divs, "rus,eng"},
		"untagged": {untagged, ""},
	} {
		if got := strings.Join(subtitle.TTMLLanguages(c.doc), ","); got != c.want {
			t.Fatalf("%s: TTMLLanguages = %q, want %q", name, got, c.want)
		}
	}
}
