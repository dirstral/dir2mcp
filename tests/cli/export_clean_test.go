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

// seedCleanExportStore seeds a transcript exercising every cue-cleaning pass: a
// name to be rewritten by the glossary, a long identical run to collapse, a URL
// hallucination to drop, and ordinary speech that must survive.
func seedCleanExportStore(t *testing.T, stateDir, relPath string) {
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
	err = st.WithTx(ctx, func(tx model.RepresentationStore) error {
		repID, err := tx.UpsertRepresentation(ctx, model.Representation{
			DocID: doc.DocID, RepType: "transcript", RepHash: "h1",
		})
		if err != nil {
			return err
		}
		chunks := []struct {
			text  string
			start int
			end   int
		}{
			{"Letter signed by Ajubei", 0, 2000}, // glossary -> Adzhubei
			{"No.", 2000, 3000},                  // run of 4 identical "No."
			{"No.", 3000, 4000},
			{"No.", 4000, 5000},                           // 3rd -> dropped (threshold 3)
			{"No.", 5000, 6000},                           // 4th -> dropped
			{"Subtitles by www.spam.com", 6000, 8000},     // URL -> dropped
			{"Crimea, NATO.", 8000, 9000},                 // phrase-only -> dropped by drop_phrases
			{"Crimea, NATO. Genuine words.", 9000, 10000}, // leaked phrase -> scrubbed, sentence kept
			{"Real closing line", 10000, 11000},
		}
		for i, c := range chunks {
			if _, err := tx.InsertChunkWithSpans(ctx,
				model.Chunk{RepID: repID, Ordinal: i, Text: c.text, IndexKind: "text"},
				[]model.Span{{Kind: "time", StartMS: c.start, EndMS: c.end}},
			); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed transcript: %v", err)
	}
}

// TestExportAppliesCueCleaning pins the end-to-end cleaning path: glossary
// rewrite, repetition-collapse, and URL drop all applied via the export command
// when media.subtitles.{glossary,collapse_repeats,drop_urls} are configured.
func TestExportAppliesCueCleaning(t *testing.T) {
	tmp := t.TempDir()
	seedCleanExportStore(t, filepath.Join(tmp, ".dir2mcp"), "media/talk.mp3")
	cfgYAML := strings.Join([]string{
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
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(tmp, ".dir2mcp.yaml"), []byte(cfgYAML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		if code := app.RunWithContext(context.Background(),
			[]string{"export", "--format", "srt", "media/talk.mp3"}); code != 0 {
			t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
		}
	})
	out := stdout.String()

	if strings.Contains(out, "Ajubei") || !strings.Contains(out, "Adzhubei") {
		t.Errorf("glossary rewrite not applied:\n%s", out)
	}
	if strings.Contains(strings.ToLower(out), "spam.com") {
		t.Errorf("URL cue not dropped:\n%s", out)
	}
	if n := strings.Count(out, "No."); n != 2 {
		t.Errorf("repetition-collapse: got %d 'No.' cues, want 2:\n%s", n, out)
	}
	if strings.Contains(out, "Crimea, NATO") {
		t.Errorf("phrase not removed (drop_phrases + scrub_phrases):\n%s", out)
	}
	if !strings.Contains(out, "Genuine words.") {
		t.Errorf("scrub_phrases dropped the whole leaked cue instead of keeping the sentence:\n%s", out)
	}
	if !strings.Contains(out, "Real closing line") {
		t.Errorf("real closing cue missing:\n%s", out)
	}
}

// TestExportNoCleaningConfigUnchanged pins that without cleaning config the
// export is unaffected (all cues, including the would-be-cleaned ones, survive).
func TestExportNoCleaningConfigUnchanged(t *testing.T) {
	tmp := t.TempDir()
	seedCleanExportStore(t, filepath.Join(tmp, ".dir2mcp"), "media/talk.mp3")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		if code := app.RunWithContext(context.Background(),
			[]string{"export", "--format", "srt", "media/talk.mp3"}); code != 0 {
			t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
		}
	})
	out := stdout.String()

	if !strings.Contains(out, "Ajubei") {
		t.Errorf("without config the name should be untouched:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "spam.com") {
		t.Errorf("without config the URL cue should survive:\n%s", out)
	}
	if n := strings.Count(out, "No."); n != 4 {
		t.Errorf("without config all 4 'No.' cues should survive, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "Crimea, NATO") {
		t.Errorf("without config the phrase-only cue should survive:\n%s", out)
	}
}
