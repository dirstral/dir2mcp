package tests

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// seedBilingualStore writes a sqlite store holding one document with two
// transcript representations (primary + secondary language, keyed by meta_json),
// each with two time-spanned chunks, so the bilingual TTML export path can
// resolve both languages.
func seedBilingualStore(t *testing.T, stateDir, relPath string) {
	t.Helper()
	ctx := context.Background()
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: relPath, DocType: "audio", SourceType: "local", Status: "ok",
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	doc, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}

	type langRep struct {
		repType string
		meta    string
		hash    string
		chunks  [][3]any // text, startMS, endMS
	}
	// Per-language transcripts coexist under distinct rep_types
	// ("transcript-<lang>") per the store's UNIQUE(doc_id, rep_type) constraint
	// (SPEC §8.6.2/§8.6.4); TranscriptRepresentations reads both bare and suffixed
	// forms. This mirrors how the translation surface persists a translated
	// transcript alongside the source.
	reps := []langRep{
		{"transcript-en", `{"language":"en"}`, "en1", [][3]any{{"Hello", 0, 2000}, {"World", 5000, 7000}}},
		// French start offsets are within the default 2500 ms tolerance of the
		// English cues, so they align onto the same time regions.
		{"transcript-fr", `{"language":"fr"}`, "fr1", [][3]any{{"Bonjour", 200, 2100}, {"Monde", 5100, 7100}}},
	}
	err = st.WithTx(ctx, func(tx model.RepresentationStore) error {
		for _, r := range reps {
			repID, rerr := tx.UpsertRepresentation(ctx, model.Representation{
				DocID: doc.DocID, RepType: r.repType, RepHash: r.hash, MetaJSON: r.meta,
			})
			if rerr != nil {
				return rerr
			}
			for i, c := range r.chunks {
				if _, cerr := tx.InsertChunkWithSpans(ctx,
					model.Chunk{RepID: repID, Ordinal: i, Text: c[0].(string), IndexKind: "text"},
					[]model.Span{{Kind: "time", StartMS: c[1].(int), EndMS: c[2].(int)}},
				); cerr != nil {
					return cerr
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed transcripts: %v", err)
	}
}

// writeTTMLConfig writes a minimal config file enabling the TTML/SMIL surface,
// returning its path for use via the --config global flag.
func writeTTMLConfig(t *testing.T, dir string, smil bool) string {
	t.Helper()
	path := filepath.Join(dir, "dir2mcp.yaml")
	lines := []string{
		"media_subtitles_ttml_enabled: true",
	}
	if smil {
		lines = append(lines, "media_subtitles_smil_enabled: true")
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestExportTTMLDisabledByDefault pins that without enabling config, --format
// ttml is rejected: the optional surface is OFF by default (SPEC §8.6.10) and
// VTT/SRT behavior is unchanged.
func TestExportTTMLDisabledByDefault(t *testing.T) {
	tmp := t.TempDir()
	seedExportStore(t, filepath.Join(tmp, ".dir2mcp"), "media/talk.mp3", `{"language":"en"}`)

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(),
			[]string{"export", "--format", "ttml", "media/talk.mp3"})
		if code == 0 {
			t.Fatalf("ttml export must fail when disabled; stdout=%s", stdout.String())
		}
	})
	if !strings.Contains(stderr.String(), "disabled") {
		t.Fatalf("expected disabled error, got %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "<tt") {
		t.Fatalf("no TTML should be emitted when disabled")
	}
}

// TestExportTTMLMonolingual pins that with the surface enabled, a single-language
// TTML document is rendered to stdout.
func TestExportTTMLMonolingual(t *testing.T) {
	tmp := t.TempDir()
	seedExportStore(t, filepath.Join(tmp, ".dir2mcp"), "media/talk.mp3", `{"language":"en"}`)
	cfgPath := writeTTMLConfig(t, tmp, false)

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(),
			[]string{"--config", cfgPath, "export", "--format", "ttml", "--lang", "en", "media/talk.mp3"})
		if code != 0 {
			t.Fatalf("ttml export exit = %d, stderr=%s", code, stderr.String())
		}
	})
	out := stdout.String()
	for _, want := range []string{`<tt `, `xml:lang="en"`, `First part`, `Second part`} {
		if !strings.Contains(out, want) {
			t.Fatalf("monolingual TTML missing %q in:\n%s", want, out)
		}
	}
}

// TestExportTTMLBilingual pins bilingual TTML: both languages render over the
// same <p> time regions, aligned within the default tolerance.
func TestExportTTMLBilingual(t *testing.T) {
	tmp := t.TempDir()
	seedBilingualStore(t, filepath.Join(tmp, ".dir2mcp"), "media/talk.mp3")
	cfgPath := writeTTMLConfig(t, tmp, false)

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(),
			[]string{"--config", cfgPath, "export", "--format", "ttml",
				"--lang", "en", "--secondary-lang", "fr", "media/talk.mp3"})
		if code != 0 {
			t.Fatalf("bilingual ttml exit = %d, stderr=%s", code, stderr.String())
		}
	})
	out := stdout.String()
	for _, want := range []string{
		`<span xml:lang="en">Hello</span>`,
		`<span xml:lang="fr">Bonjour</span>`,
		`<span xml:lang="en">World</span>`,
		`<span xml:lang="fr">Monde</span>`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("bilingual TTML missing %q in:\n%s", want, out)
		}
	}
	// Both runs must sit under the same primary time region (single <p>).
	if strings.Count(out, "<p ") != 2 {
		t.Fatalf("expected 2 merged <p> cues, got %d:\n%s", strings.Count(out, "<p "), out)
	}
}

// TestExportTTMLMissingLanguageInvalidField pins that requesting a language with
// no transcript fails as INVALID_FIELD (SPEC §8.6.10), not a server error.
func TestExportTTMLMissingLanguageInvalidField(t *testing.T) {
	tmp := t.TempDir()
	seedExportStore(t, filepath.Join(tmp, ".dir2mcp"), "media/talk.mp3", `{"language":"en"}`)
	cfgPath := writeTTMLConfig(t, tmp, false)

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(),
			[]string{"--config", cfgPath, "export", "--format", "ttml",
				"--lang", "en", "--secondary-lang", "de", "media/talk.mp3"})
		if code == 0 {
			t.Fatalf("missing secondary language should fail; stdout=%s", stdout.String())
		}
	})
	if !strings.Contains(stderr.String(), "INVALID_FIELD") {
		t.Fatalf("expected INVALID_FIELD error, got %q", stderr.String())
	}
}

// TestExportTTMLSMILFailsOpen pins the SMIL fail-open contract: with SMIL
// enabled but the media file absent/unprobeable (or ffprobe missing), the TTML
// is still written and the export succeeds; SMIL is simply omitted.
func TestExportTTMLSMILFailsOpen(t *testing.T) {
	tmp := t.TempDir()
	seedExportStore(t, filepath.Join(tmp, ".dir2mcp"), "media/talk.mp3", `{"language":"en"}`)
	cfgPath := writeTTMLConfig(t, tmp, true)
	outPath := filepath.Join(tmp, "out", "talk.ttml")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		// No media file at media/talk.mp3 exists on disk -> probe fails -> SMIL
		// omitted, TTML still written, exit 0.
		code := app.RunWithContext(context.Background(),
			[]string{"--config", cfgPath, "export", "--format", "ttml",
				"--lang", "en", "--out", outPath, "media/talk.mp3"})
		if code != 0 {
			t.Fatalf("export should fail open on missing media; exit=%d stderr=%s", code, stderr.String())
		}
	})
	if _, err := os.ReadFile(outPath); err != nil {
		t.Fatalf("TTML must still be written on SMIL fail-open: %v", err)
	}
	smilPath := strings.TrimSuffix(outPath, ".ttml") + ".smil"
	if _, err := os.Stat(smilPath); err == nil {
		t.Fatalf("SMIL must be omitted when media metadata is unavailable")
	}
}
