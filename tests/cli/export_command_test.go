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

// seedExportStore writes a sqlite store at <stateDir>/meta.sqlite holding a
// document with a transcript representation (carrying metaJSON) and two
// time-spanned chunks, so the export command can resolve and render it.
func seedExportStore(t *testing.T, stateDir, relPath, metaJSON string) {
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
			DocID: doc.DocID, RepType: "transcript", RepHash: "h1", MetaJSON: metaJSON,
		})
		if err != nil {
			return err
		}
		chunks := []struct {
			text  string
			start int
			end   int
		}{
			{"Second part", 2000, 4000},
			{"First part", 0, 2000},
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

// TestExportVTTToStdout pins the end-to-end export path: a seeded transcript
// renders to ordered WebVTT on stdout.
func TestExportVTTToStdout(t *testing.T) {
	tmp := t.TempDir()
	seedExportStore(t, filepath.Join(tmp, ".dir2mcp"), "media/talk.mp3", "")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(),
			[]string{"export", "--format", "vtt", "media/talk.mp3"})
		if code != 0 {
			t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
		}
	})

	want := "WEBVTT\n\n" +
		"00:00:00.000 --> 00:00:02.000\nFirst part\n\n" +
		"00:00:02.000 --> 00:00:04.000\nSecond part\n\n"
	if stdout.String() != want {
		t.Fatalf("export VTT:\n got: %q\nwant: %q", stdout.String(), want)
	}
}

// TestExportSRTToFile pins atomic file output: --out writes a valid SRT file.
func TestExportSRTToFile(t *testing.T) {
	tmp := t.TempDir()
	seedExportStore(t, filepath.Join(tmp, ".dir2mcp"), "media/talk.mp3", "")
	outPath := filepath.Join(tmp, "out", "talk.srt")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(),
			[]string{"export", "--format", "srt", "--out", outPath, "media/talk.mp3"})
		if code != 0 {
			t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
		}
	})

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read out: %v", err)
	}
	want := "1\n00:00:00,000 --> 00:00:02,000\nFirst part\n\n" +
		"2\n00:00:02,000 --> 00:00:04,000\nSecond part\n\n"
	if string(data) != want {
		t.Fatalf("export SRT file:\n got: %q\nwant: %q", string(data), want)
	}
}

// TestExportLangSelectsTranscript pins --lang matching against the transcript's
// meta_json language, and a clear error when no transcript matches.
func TestExportLangSelectsTranscript(t *testing.T) {
	tmp := t.TempDir()
	seedExportStore(t, filepath.Join(tmp, ".dir2mcp"), "media/talk.mp3", `{"language":"en"}`)

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		if code := app.RunWithContext(context.Background(),
			[]string{"export", "--format", "vtt", "--lang", "en", "media/talk.mp3"}); code != 0 {
			t.Fatalf("--lang en exit = %d, stderr=%s", code, stderr.String())
		}
	})
	if !strings.Contains(stdout.String(), "First part") {
		t.Fatalf("expected matched transcript output, got %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	withWorkingDir(t, tmp, func() {
		if code := app.RunWithContext(context.Background(),
			[]string{"export", "--format", "vtt", "--lang", "fr", "media/talk.mp3"}); code == 0 {
			t.Fatalf("--lang fr should fail with no matching transcript; stdout=%s", stdout.String())
		}
	})
	if !strings.Contains(stderr.String(), "no transcript for language") {
		t.Fatalf("expected language error on stderr, got %q", stderr.String())
	}
}

// TestExportNoTranscript pins a clear error when the document has no transcript.
func TestExportNoTranscript(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: "notes.md", DocType: "md", SourceType: "local", Status: "ok",
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	_ = st.Close()

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		if code := app.RunWithContext(context.Background(),
			[]string{"export", "--format", "srt", "notes.md"}); code == 0 {
			t.Fatalf("expected non-zero exit for no transcript")
		}
	})
	if !strings.Contains(stderr.String(), "no transcript representation") {
		t.Fatalf("expected no-transcript error, got %q", stderr.String())
	}
}
