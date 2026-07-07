package tests

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// seedWordTimedExportStore seeds a transcript with a SINGLE time-spanned chunk
// covering 0..20 s, carrying per-word timings (40 words, 500 ms apart). In the
// default "chunk" segmentation this renders as one long cue; in "broadcast"
// segmentation the words re-segment into several norm-obeying cues.
func seedWordTimedExportStore(t *testing.T, stateDir, relPath string) {
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

	words := make([]model.WordSpan, 0, 40)
	var textParts []string
	for i := 0; i < 40; i++ {
		w := "word"
		words = append(words, model.WordSpan{T: i * 500, D: 400, W: w})
		textParts = append(textParts, w)
	}
	fullText := strings.Join(textParts, " ")

	err = st.WithTx(ctx, func(tx model.RepresentationStore) error {
		repID, err := tx.UpsertRepresentation(ctx, model.Representation{
			DocID: doc.DocID, RepType: "transcript", RepHash: "h1",
		})
		if err != nil {
			return err
		}
		_, err = tx.InsertChunkWithSpans(ctx,
			model.Chunk{RepID: repID, Ordinal: 0, Text: fullText, IndexKind: "text"},
			[]model.Span{{Kind: "time", StartMS: 0, EndMS: 20000, Words: words}},
		)
		return err
	})
	if err != nil {
		t.Fatalf("seed transcript: %v", err)
	}
}

var srtTimingRE = regexp.MustCompile(`(\d\d):(\d\d):(\d\d),(\d\d\d) --> (\d\d):(\d\d):(\d\d),(\d\d\d)`)

// TestExportBroadcastSegmentationSplitsLongCue pins the end-to-end broadcast
// path: with media.subtitles.segmentation: broadcast a single 20 s word-timed
// chunk is re-segmented into multiple cues, each within the 6 s duration cap and
// the 2x42 char limit — whereas the default renders it as one long cue.
func TestExportBroadcastSegmentationSplitsLongCue(t *testing.T) {
	tmp := t.TempDir()
	seedWordTimedExportStore(t, filepath.Join(tmp, ".dir2mcp"), "media/talk.mp3")
	cfgYAML := strings.Join([]string{
		"media:",
		"  subtitles:",
		"    segmentation: broadcast",
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

	cues := countSRTCues(t, stdout.String())
	if cues < 4 {
		t.Fatalf("broadcast segmentation should split the 20 s cue into several; got %d cues:\n%s",
			cues, stdout.String())
	}
	assertBroadcastCaps(t, stdout.String())
}

// TestExportDefaultSegmentationOneCue pins that WITHOUT the broadcast config the
// same word-timed chunk renders as a single cue (historical behavior), so the
// feature is strictly opt-in.
func TestExportDefaultSegmentationOneCue(t *testing.T) {
	tmp := t.TempDir()
	seedWordTimedExportStore(t, filepath.Join(tmp, ".dir2mcp"), "media/talk.mp3")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		if code := app.RunWithContext(context.Background(),
			[]string{"export", "--format", "srt", "media/talk.mp3"}); code != 0 {
			t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
		}
	})

	if cues := countSRTCues(t, stdout.String()); cues != 1 {
		t.Fatalf("default segmentation should render one cue, got %d:\n%s", cues, stdout.String())
	}
}

// countSRTCues counts timing lines (one per cue) in an SRT document.
func countSRTCues(t *testing.T, srt string) int {
	t.Helper()
	return len(srtTimingRE.FindAllString(srt, -1))
}

// assertBroadcastCaps checks every cue in an SRT document obeys the broadcast
// norms: <= 6 s duration and <= 42 characters per rendered line.
func assertBroadcastCaps(t *testing.T, srt string) {
	t.Helper()
	blocks := strings.Split(strings.TrimSpace(srt), "\n\n")
	for _, blk := range blocks {
		lines := strings.Split(blk, "\n")
		if len(lines) < 2 {
			continue
		}
		m := srtTimingRE.FindStringSubmatch(lines[1])
		if m == nil {
			t.Fatalf("cue block missing timing line: %q", blk)
		}
		start := srtToMS(m[1], m[2], m[3], m[4])
		end := srtToMS(m[5], m[6], m[7], m[8])
		if dur := end - start; dur > 6000 {
			t.Errorf("cue duration %d ms exceeds 6000 ms cap in block:\n%s", dur, blk)
		}
		for _, line := range lines[2:] {
			if n := len([]rune(line)); n > 42 {
				t.Errorf("cue line %q is %d chars, exceeds 42", line, n)
			}
		}
	}
}

// srtToMS converts SRT timestamp components to milliseconds.
func srtToMS(hh, mm, ss, mmm string) int {
	atoi := func(s string) int {
		n := 0
		for _, r := range s {
			n = n*10 + int(r-'0')
		}
		return n
	}
	return ((atoi(hh)*60+atoi(mm))*60+atoi(ss))*1000 + atoi(mmm)
}
