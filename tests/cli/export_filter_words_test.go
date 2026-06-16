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

// seedFilterExportStore seeds a transcript whose middle chunk is pure
// boilerplate and whose other chunks contain an inline boilerplate phrase, so an
// export filter can be observed dropping/stripping them.
func seedFilterExportStore(t *testing.T, stateDir, relPath string) {
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
			{"Real opening line", 0, 2000},
			{"Subscribe to our channel", 2000, 4000},              // pure boilerplate -> dropped
			{"Real closing Subscribe to our channel", 4000, 6000}, // inline phrase stripped
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

// TestExportAppliesFilterWords pins that media.filter_words configured in
// .dir2mcp.yaml strips boilerplate from exported VTT: the pure-boilerplate cue
// is dropped and the inline phrase is removed, while real text and timing
// survive.
func TestExportAppliesFilterWords(t *testing.T) {
	tmp := t.TempDir()
	seedFilterExportStore(t, filepath.Join(tmp, ".dir2mcp"), "media/talk.mp3")
	// Config drives the filter; the phrase is general-purpose (no built-in list).
	cfgYAML := strings.Join([]string{
		"media:",
		"  filter_words:",
		"    - subscribe to our channel",
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(tmp, ".dir2mcp.yaml"), []byte(cfgYAML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(),
			[]string{"export", "--format", "vtt", "media/talk.mp3"})
		if code != 0 {
			t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
		}
	})

	out := stdout.String()
	if strings.Contains(strings.ToLower(out), "subscribe to our channel") {
		t.Fatalf("filtered phrase leaked into exported VTT:\n%s", out)
	}
	if !strings.Contains(out, "Real opening line") {
		t.Fatalf("real opening cue missing from export:\n%s", out)
	}
	if !strings.Contains(out, "Real closing") {
		t.Fatalf("real closing cue (phrase stripped) missing from export:\n%s", out)
	}
	// The pure-boilerplate cue's window [00:02,00:04] must not be present as a
	// standalone cue (its only text was the filtered phrase).
	if strings.Contains(out, "00:00:02.000 --> 00:00:04.000") {
		t.Fatalf("boilerplate-only cue was not dropped:\n%s", out)
	}
}

// TestExportNoFilterWordsUnchanged pins that with no media.filter_words config
// the export is byte-identical to today (no accidental stripping).
func TestExportNoFilterWordsUnchanged(t *testing.T) {
	tmp := t.TempDir()
	seedFilterExportStore(t, filepath.Join(tmp, ".dir2mcp"), "media/talk.mp3")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(),
			[]string{"export", "--format", "vtt", "media/talk.mp3"})
		if code != 0 {
			t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
		}
	})

	out := stdout.String()
	want := "WEBVTT\n\n" +
		"00:00:00.000 --> 00:00:02.000\nReal opening line\n\n" +
		"00:00:02.000 --> 00:00:04.000\nSubscribe to our channel\n\n" +
		"00:00:04.000 --> 00:00:06.000\nReal closing Subscribe to our channel\n\n"
	if out != want {
		t.Fatalf("export without filter changed:\n got %q\nwant %q", out, want)
	}
}
