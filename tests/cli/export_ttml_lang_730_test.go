package tests

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
)

// Issue #730: `export --format ttml` documents --lang as optional and correctly
// selects the first/source transcript when it is omitted, but it handed the raw
// (empty) flag value to the renderer instead of the SELECTED representation's
// recorded language. A transcript whose meta_json records {"language":"en"}
// therefore exported as `<tt ... xml:lang="">` with unlabelled text runs and a
// SMIL with no systemLanguage.
//
// The resolution rule these tests pin: the effective language is the recorded
// language of the representation that was actually selected. There is NO
// invented fallback — for a genuinely untagged legacy transcript the tag stays
// empty (see TestExportTTMLUntaggedTranscriptEmitsNoLanguage730).

// TestExportTTMLOmittedLangUsesRecordedLanguage730 is the core regression: with
// --lang omitted, the document-level xml:lang and the cue text runs must carry
// the selected transcript's recorded language.
//
// Fails before the fix: emits xml:lang="" and bare <span>.
func TestExportTTMLOmittedLangUsesRecordedLanguage730(t *testing.T) {
	tmp := t.TempDir()
	seedExportStore(t, filepath.Join(tmp, ".dir2mcp"), "media/talk.mp3", `{"language":"en"}`)
	cfgPath := writeTTMLConfig(t, tmp, false)

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		if code := app.RunWithContext(context.Background(),
			[]string{"--config", cfgPath, "export", "--format", "ttml", "media/talk.mp3"}); code != 0 {
			t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
		}
	})
	out := stdout.String()

	if strings.Contains(out, `xml:lang=""`) {
		t.Errorf("empty xml:lang emitted for a transcript recording a language:\n%s", out)
	}
	if !strings.Contains(out, `<tt xmlns="http://www.w3.org/ns/ttml" xml:lang="en">`) {
		t.Errorf("document xml:lang must be the recorded language \"en\":\n%s", out)
	}
	if !strings.Contains(out, `<span xml:lang="en">First part</span>`) {
		t.Errorf("cue text run must carry the resolved language:\n%s", out)
	}
}

// TestExportTTMLOmittedLangMatchesExplicitLang730 pins that omitting --lang and
// passing the transcript's own --lang produce the SAME document: the flag
// selects a transcript, it is not an independent source of the emitted tag.
func TestExportTTMLOmittedLangMatchesExplicitLang730(t *testing.T) {
	render := func(t *testing.T, args ...string) string {
		t.Helper()
		tmp := t.TempDir()
		seedExportStore(t, filepath.Join(tmp, ".dir2mcp"), "media/talk.mp3", `{"language":"en"}`)
		cfgPath := writeTTMLConfig(t, tmp, false)
		var stdout, stderr bytes.Buffer
		app := cli.NewAppWithIO(&stdout, &stderr)
		withWorkingDir(t, tmp, func() {
			argv := append([]string{"--config", cfgPath, "export", "--format", "ttml"}, args...)
			if code := app.RunWithContext(context.Background(), append(argv, "media/talk.mp3")); code != 0 {
				t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
			}
		})
		return stdout.String()
	}

	omitted := render(t)
	explicit := render(t, "--lang", "en")
	if omitted != explicit {
		t.Errorf("omitted --lang must render identically to --lang en\nomitted:\n%s\nexplicit:\n%s", omitted, explicit)
	}
}

// TestExportTTMLUntaggedTranscriptEmitsNoLanguage730 pins the deliberate
// no-fallback decision for a genuinely untagged legacy transcript (empty
// meta_json): the document tag stays the empty string, which per XML 1.0 §2.12
// means "no language information is available" (TTML1's own examples use
// `<tt xml:lang="">`), and text runs omit xml:lang entirely.
//
// The alternative — substituting a configured or guessed default — would put a
// plausible-but-wrong BCP-47 tag in a broadcast subtitle file, which downstream
// players act on (track selection, font/shaping). An absent tag degrades; a
// wrong tag misroutes.
func TestExportTTMLUntaggedTranscriptEmitsNoLanguage730(t *testing.T) {
	tmp := t.TempDir()
	seedExportStore(t, filepath.Join(tmp, ".dir2mcp"), "media/talk.mp3", "")
	cfgPath := writeTTMLConfig(t, tmp, false)

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		if code := app.RunWithContext(context.Background(),
			[]string{"--config", cfgPath, "export", "--format", "ttml", "media/talk.mp3"}); code != 0 {
			t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
		}
	})
	out := stdout.String()

	if !strings.Contains(out, `<tt xmlns="http://www.w3.org/ns/ttml" xml:lang="">`) {
		t.Errorf("untagged transcript must emit the explicit empty tag, not a guess:\n%s", out)
	}
	if !strings.Contains(out, `<span>First part</span>`) {
		t.Errorf("untagged text runs must omit xml:lang entirely:\n%s", out)
	}
	// No invented tag of any kind.
	for _, bad := range []string{`xml:lang="en"`, `xml:lang="und"`, `xml:lang="ru"`} {
		if strings.Contains(out, bad) {
			t.Errorf("untagged transcript must not invent %s:\n%s", bad, out)
		}
	}
}

// TestExportTTMLBilingualOmittedLangResolvesBothTags730 pins that BOTH tags are
// resolved from their representations: with --lang omitted the primary tag comes
// from the selected (first) transcript, and the secondary run keeps its own.
func TestExportTTMLBilingualOmittedLangResolvesBothTags730(t *testing.T) {
	tmp := t.TempDir()
	seedBilingualStore(t, filepath.Join(tmp, ".dir2mcp"), "media/talk.mp3")
	cfgPath := writeTTMLConfig(t, tmp, false)

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		if code := app.RunWithContext(context.Background(),
			[]string{"--config", cfgPath, "export", "--format", "ttml",
				"--secondary-lang", "fr", "media/talk.mp3"}); code != 0 {
			t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
		}
	})
	out := stdout.String()

	if !strings.Contains(out, `<tt xmlns="http://www.w3.org/ns/ttml" xml:lang="en">`) {
		t.Errorf("primary tag must resolve from the selected transcript:\n%s", out)
	}
	for _, want := range []string{
		`<span xml:lang="en">Hello</span>`,
		`<span xml:lang="fr">Bonjour</span>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("bilingual TTML missing %q in:\n%s", want, out)
		}
	}
}

// TestExportTTMLSecondaryLangTagComesFromRepresentation730 pins that the emitted
// tag is the representation's recorded tag, not the raw flag spelling: --lang
// matching is case-insensitive, so "EN"/"FR" select the same transcripts and
// must still render the canonical recorded "en"/"fr".
func TestExportTTMLSecondaryLangTagComesFromRepresentation730(t *testing.T) {
	tmp := t.TempDir()
	seedBilingualStore(t, filepath.Join(tmp, ".dir2mcp"), "media/talk.mp3")
	cfgPath := writeTTMLConfig(t, tmp, false)

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		if code := app.RunWithContext(context.Background(),
			[]string{"--config", cfgPath, "export", "--format", "ttml",
				"--lang", "EN", "--secondary-lang", "FR", "media/talk.mp3"}); code != 0 {
			t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
		}
	})
	out := stdout.String()

	for _, bad := range []string{`xml:lang="EN"`, `xml:lang="FR"`} {
		if strings.Contains(out, bad) {
			t.Errorf("emitted tag must be the recorded one, not the flag spelling %s:\n%s", bad, out)
		}
	}
	for _, want := range []string{`xml:lang="en"`, `xml:lang="fr"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing recorded tag %s in:\n%s", want, out)
		}
	}
}

// TestExportTTMLSMILUsesResolvedLanguage730 pins that the companion SMIL's
// <textstream systemLanguage> is the same resolved tag the TTML carries, with
// --lang omitted. It probes through a stub ffprobe so no real codec tooling is
// needed.
//
// Fails before the fix: systemLanguage is omitted entirely because opts.lang was
// empty.
func TestExportTTMLSMILUsesResolvedLanguage730(t *testing.T) {
	tmp := t.TempDir()
	const rel = "media/talk.mp4"
	seedExportStore(t, filepath.Join(tmp, ".dir2mcp"), rel, `{"language":"en"}`)
	cfgPath := writeTTMLConfig(t, tmp, true)
	stubFFprobeOnPATH(t)

	// A real (non-empty) local media file so the stub probe succeeds.
	if err := os.MkdirAll(filepath.Join(tmp, "media"), 0o755); err != nil {
		t.Fatalf("mkdir media: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, rel), []byte("not-really-mp4"), 0o644); err != nil {
		t.Fatalf("write media: %v", err)
	}

	outPath := filepath.Join(tmp, "out", "talk.ttml")
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		if code := app.RunWithContext(context.Background(),
			[]string{"--config", cfgPath, "export", "--format", "ttml",
				"--out", outPath, rel}); code != 0 {
			t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
		}
	})

	smilPath := strings.TrimSuffix(outPath, ".ttml") + ".smil"
	smilRaw, err := os.ReadFile(smilPath)
	if err != nil {
		t.Fatalf("SMIL must be written (stderr=%s): %v", stderr.String(), err)
	}
	if !strings.Contains(string(smilRaw), `systemLanguage="en"`) {
		t.Errorf("SMIL must carry the resolved language:\n%s", smilRaw)
	}

	ttmlRaw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read TTML: %v", err)
	}
	if !strings.Contains(string(ttmlRaw), `xml:lang="en"`) {
		t.Errorf("TTML and SMIL must agree on the resolved language:\n%s", ttmlRaw)
	}
}
