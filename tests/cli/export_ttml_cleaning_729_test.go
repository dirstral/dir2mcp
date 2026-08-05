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

// cleaningConfigLines is the media.subtitles.* editorial config shared by the
// VTT/SRT cleaning test (export_clean_test.go) and these TTML tests, so both
// formats are asserted against the SAME configuration. Issue #729: TTML applied
// only media.filter_words, so the same transcript exported as SRT and as TTML
// disagreed on every rule below.
func cleaningConfigLines() []string {
	return []string{
		"media_subtitles_ttml_enabled: true",
		"media:",
		"  subtitles:",
		"    glossary:",
		"      - Aju?bei=>Adzhubei",
		"    drop_phrases:",
		"      - Crimea|NATO",
		"    scrub_phrases:",
		"      - Crimea,?\\s*NATO\\.?",
		"    collapse_repeats: 3",
		"    drop_urls: true",
	}
}

// writeCleaningTTMLConfig writes the shared cleaning config with the TTML
// surface enabled, returning its path.
func writeCleaningTTMLConfig(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "dir2mcp.yaml")
	if err := os.WriteFile(path, []byte(strings.Join(cleaningConfigLines(), "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestExportTTMLAppliesCueCleaning729 pins that --format ttml applies the same
// media.subtitles.* editorial pipeline as VTT/SRT: glossary rewrite,
// repetition-collapse, URL drop, drop_phrases and scrub_phrases. It reuses the
// exact fixture the SRT test uses (seedCleanExportStore), so a divergence
// between the two formats fails here.
//
// Fails before the fix: TTML built cues with FilterCues only, so "Ajubei",
// "spam.com", four "No." cues and "Crimea, NATO" all survived into the TTML.
func TestExportTTMLAppliesCueCleaning729(t *testing.T) {
	tmp := t.TempDir()
	seedCleanExportStore(t, filepath.Join(tmp, ".dir2mcp"), "media/talk.mp3")
	cfgPath := writeCleaningTTMLConfig(t, tmp)

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		if code := app.RunWithContext(context.Background(),
			[]string{"--config", cfgPath, "export", "--format", "ttml", "media/talk.mp3"}); code != 0 {
			t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
		}
	})
	out := stdout.String()

	if strings.Contains(out, "Ajubei") || !strings.Contains(out, "Adzhubei") {
		t.Errorf("glossary rewrite not applied to TTML:\n%s", out)
	}
	if strings.Contains(strings.ToLower(out), "spam.com") {
		t.Errorf("URL cue not dropped from TTML:\n%s", out)
	}
	if n := strings.Count(out, "No."); n != 2 {
		t.Errorf("repetition-collapse: got %d 'No.' cues in TTML, want 2:\n%s", n, out)
	}
	if strings.Contains(out, "Crimea, NATO") {
		t.Errorf("phrase not removed from TTML (drop_phrases + scrub_phrases):\n%s", out)
	}
	if !strings.Contains(out, "Genuine words.") {
		t.Errorf("scrub_phrases dropped the whole leaked TTML cue instead of keeping the sentence:\n%s", out)
	}
	if !strings.Contains(out, "Real closing line") {
		t.Errorf("real closing cue missing from TTML:\n%s", out)
	}
}

// TestExportTTMLNoCleaningConfigUnchanged729 pins the empty-config contract: with
// the TTML surface enabled but no media.subtitles.* editorial rules, every cue
// survives verbatim. This guards the fix against becoming an unconditional
// rewrite of TTML output.
func TestExportTTMLNoCleaningConfigUnchanged729(t *testing.T) {
	tmp := t.TempDir()
	seedCleanExportStore(t, filepath.Join(tmp, ".dir2mcp"), "media/talk.mp3")
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

	if !strings.Contains(out, "Ajubei") {
		t.Errorf("without config the name should be untouched in TTML:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "spam.com") {
		t.Errorf("without config the URL cue should survive in TTML:\n%s", out)
	}
	if n := strings.Count(out, "No."); n != 4 {
		t.Errorf("without config all 4 'No.' cues should survive in TTML, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "Crimea, NATO") {
		t.Errorf("without config the phrase-only cue should survive in TTML:\n%s", out)
	}
}

// seedBilingualCleanStore seeds two language-tagged transcripts that BOTH carry
// cleanable content, so a fix that cleans only the primary language is caught.
// The two languages' cue timings overlap, so alignment pairs them.
func seedBilingualCleanStore(t *testing.T, stateDir, relPath string) {
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
	reps := []langRep{
		{"transcript-en", `{"language":"en"}`, "en1", [][3]any{
			{"Letter signed by Ajubei", 0, 2000},
			{"Subtitles by www.spam.com", 5000, 7000},
		}},
		{"transcript-fr", `{"language":"fr"}`, "fr1", [][3]any{
			{"Lettre signee par Ajubei", 200, 2100},
			{"Sous-titres par www.spam.com", 5100, 7100},
		}},
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

// TestExportTTMLBilingualCleansBothLanguages729 pins that BOTH languages are
// cleaned before bilingual alignment, not just the primary: alignment must run
// on the same cue set the exports render, otherwise pairing is computed over
// cues that will not be emitted.
func TestExportTTMLBilingualCleansBothLanguages729(t *testing.T) {
	tmp := t.TempDir()
	seedBilingualCleanStore(t, filepath.Join(tmp, ".dir2mcp"), "media/talk.mp3")
	cfgPath := writeCleaningTTMLConfig(t, tmp)

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		if code := app.RunWithContext(context.Background(),
			[]string{"--config", cfgPath, "export", "--format", "ttml",
				"--lang", "en", "--secondary-lang", "fr", "media/talk.mp3"}); code != 0 {
			t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
		}
	})
	out := stdout.String()

	if strings.Contains(out, "Ajubei") {
		t.Errorf("glossary must rewrite BOTH languages before alignment:\n%s", out)
	}
	if !strings.Contains(out, "Letter signed by Adzhubei") {
		t.Errorf("primary language cue missing its glossary rewrite:\n%s", out)
	}
	if !strings.Contains(out, "Lettre signee par Adzhubei") {
		t.Errorf("secondary language cue missing its glossary rewrite:\n%s", out)
	}
	if strings.Contains(strings.ToLower(out), "spam.com") {
		t.Errorf("URL cues must be dropped in BOTH languages:\n%s", out)
	}
}
