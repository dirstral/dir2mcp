package tests

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §7.6: when the type that the current version assigns to a file differs
// from the stored doc_type, the next scan processes the document again, also
// with an unchanged content_hash, and that scan MUST retire the representations
// and chunks of the old type.

// legacyChunkWord is a word that only the seeded chunk of the old type holds.
const legacyChunkWord = "zyxlegacyword"

// seedOldTypeChunk stores an active raw_text representation with one
// searchable chunk for rel, as an older classifier left it.
func seedOldTypeChunk(t *testing.T, st *store.SQLiteStore, rel, indexKind string) {
	t.Helper()
	ctx := context.Background()
	doc := mustGetDoc(t, st, rel)
	repID, err := st.UpsertRepresentation(ctx, model.Representation{
		DocID:   doc.DocID,
		RepType: ingest.RepTypeRawText,
		RepHash: "old-type-rep-hash",
	})
	if err != nil {
		t.Fatalf("seed representation for %s: %v", rel, err)
	}
	text := "function " + legacyChunkWord + "() {}"
	if _, err := st.InsertChunkWithSpans(ctx, model.Chunk{
		RepID:           repID,
		Ordinal:         0,
		Text:            text,
		TextHash:        ingest.ComputeContentHash([]byte(text)),
		IndexKind:       indexKind,
		EmbeddingStatus: "ok",
	}, []model.Span{{Kind: "lines", StartLine: 1, EndLine: 1}}); err != nil {
		t.Fatalf("seed chunk for %s: %v", rel, err)
	}
}

// legacyHits returns the lexical hits for the seeded word in rel.
func legacyHits(t *testing.T, st *store.SQLiteStore, rel string) int {
	t.Helper()
	hits, err := st.SearchBM25(context.Background(), legacyChunkWord, 10, "")
	if err != nil {
		t.Fatalf("SearchBM25: %v", err)
	}
	n := 0
	for _, hit := range hits {
		if hit.RelPath == rel {
			n++
		}
	}
	return n
}

func typeChangeConfig(root string, t *testing.T) config.Config {
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = testutil.TempDir(t)
	cfg.STTProvider = "off"
	return cfg
}

// TestTypeChange_CodeToVideoRetiresTheCodeChunk is the example of the rule. A
// .ts transport stream that an older classifier stored as code holds a live
// raw_text representation with a searchable chunk. The bytes are unchanged.
// The next incremental scan decides video by content: the document is
// processed again, the raw_text representation and its chunk are retired (the
// lexical index returns no hit for them), and the transcript is live.
func TestTypeChange_CodeToVideoRetiresTheCodeChunk(t *testing.T) {
	ctx := context.Background()
	root := testutil.TempDir(t)
	body := mpegTSBytes(4)
	writeFile(t, filepath.Join(root, "news.ts"), string(body))
	st := newRealStore(t)
	seedOldClassifierRow(t, st, root, "news.ts", body, "code", "ok", "", "")
	seedOldTypeChunk(t, st, "news.ts", "code")
	if got := legacyHits(t, st, "news.ts"); got != 1 {
		t.Fatalf("seeded code chunk gives %d lexical hit(s), want 1; the fixture is wrong", got)
	}

	tr := &capturingTranscriber{text: "[00:00] archive footage\n[00:03] second line"}
	if err := newUpgradeScanService(t, typeChangeConfig(root, t), st, tr).Run(ctx); err != nil {
		t.Fatalf("incremental run: %v", err)
	}

	doc := mustGetDoc(t, st, "news.ts")
	if doc.DocType != "video" || doc.Status != "ok" {
		t.Fatalf("news.ts: type=%q status=%q error=%q, want video/ok", doc.DocType, doc.Status, doc.ErrorMessage)
	}
	if tr.calls != 1 {
		t.Errorf("transcriber calls = %d, want 1 (the document is processed again as video)", tr.calls)
	}
	types := liveRepTypes682(t, st, "news.ts")
	for _, repType := range types {
		if repType == ingest.RepTypeRawText {
			t.Errorf("live representation types = %v: the raw_text of the old code type is not retired", types)
		}
	}
	if len(types) == 0 {
		t.Error("no live representation: the video transcript is missing")
	}
	if got := legacyHits(t, st, "news.ts"); got != 0 {
		t.Errorf("the code chunk of the old type gives %d lexical hit(s), want 0", got)
	}
	hits, err := st.SearchBM25(ctx, "footage", 10, "")
	if err != nil {
		t.Fatalf("SearchBM25: %v", err)
	}
	if len(hits) == 0 {
		t.Error("the transcript of the new video type is not searchable")
	}
}

// TestTypeChange_CodeToBinaryIgnoredRetiresEverything covers a new type that
// writes no representation at all. A camcorder-style .mod path that holds no
// program stream is binary_ignored; a row that stored it as code keeps no
// searchable chunk.
func TestTypeChange_CodeToBinaryIgnoredRetiresEverything(t *testing.T) {
	root := testutil.TempDir(t)
	body := []byte("not a program stream\x00\x01\x02")
	writeFile(t, filepath.Join(root, "song.mod"), string(body))
	st := newRealStore(t)
	seedOldClassifierRow(t, st, root, "song.mod", body, "code", "ok", "", "")
	seedOldTypeChunk(t, st, "song.mod", "code")

	if err := mustNewIngestService(t, typeChangeConfig(root, t), st).Run(context.Background()); err != nil {
		t.Fatalf("incremental run: %v", err)
	}
	if doc := mustGetDoc(t, st, "song.mod"); doc.DocType != "binary_ignored" {
		t.Fatalf("song.mod doc_type = %q, want binary_ignored", doc.DocType)
	}
	if types := liveRepTypes682(t, st, "song.mod"); len(types) != 0 {
		t.Errorf("live representation types = %v, want none", types)
	}
	if got := legacyHits(t, st, "song.mod"); got != 0 {
		t.Errorf("the chunk of the old type gives %d lexical hit(s), want 0", got)
	}
}

// TestTypeChange_TextToCodeKeepsOneRawText pins the exception. Text and code
// both write raw_text, so the row is updated in place: after the scan there is
// one live raw_text with the current content, and the stale chunk text is gone.
func TestTypeChange_TextToCodeKeepsOneRawText(t *testing.T) {
	root := testutil.TempDir(t)
	body := []byte(typeScriptSource)
	writeFile(t, filepath.Join(root, "lib.mjs"), string(body))
	st := newRealStore(t)
	seedOldClassifierRow(t, st, root, "lib.mjs", body, "text", "ok", "", "")
	seedOldTypeChunk(t, st, "lib.mjs", "text")

	if err := mustNewIngestService(t, typeChangeConfig(root, t), st).Run(context.Background()); err != nil {
		t.Fatalf("incremental run: %v", err)
	}
	if doc := mustGetDoc(t, st, "lib.mjs"); doc.DocType != "code" || doc.Status != "ok" {
		t.Fatalf("lib.mjs: type=%q status=%q, want code/ok", doc.DocType, doc.Status)
	}
	types := liveRepTypes682(t, st, "lib.mjs")
	if len(types) != 1 || types[0] != ingest.RepTypeRawText {
		t.Errorf("live representation types = %v, want one raw_text", types)
	}
	if got := legacyHits(t, st, "lib.mjs"); got != 0 {
		t.Errorf("the stale chunk gives %d lexical hit(s), want 0 (the raw_text is rebuilt from the file)", got)
	}
	hits, err := st.SearchBM25(context.Background(), "number", 10, "")
	if err != nil {
		t.Fatalf("SearchBM25: %v", err)
	}
	if len(hits) == 0 {
		t.Error("the rebuilt raw_text is not searchable")
	}
}

// TestTypeChange_RefusedRetirementKeepsTheStoredType pins the failure order.
// When the store refuses the retirement, the scan does not store the new
// doc_type: the row keeps the old type, so the next scan sees the change again
// and retires then.
func TestTypeChange_RefusedRetirementKeepsTheStoredType(t *testing.T) {
	ctx := context.Background()
	root := testutil.TempDir(t)
	body := mpegTSBytes(4)
	writeFile(t, filepath.Join(root, "news.ts"), string(body))
	st := &retireFailingStore682{SQLiteStore: newRealStore(t)}
	seedOldClassifierRow(t, st.SQLiteStore, root, "news.ts", body, "code", "ok", "", "")
	seedOldTypeChunk(t, st.SQLiteStore, "news.ts", "code")
	cfg := typeChangeConfig(root, t)
	tr := &capturingTranscriber{text: "[00:00] archive footage"}

	st.fail.Store(true)
	// The run reports the document as an error; the run itself may succeed.
	_ = newUpgradeScanService(t, cfg, st, tr).Run(ctx)
	if doc := mustGetDoc(t, st.SQLiteStore, "news.ts"); doc.DocType != "code" {
		t.Fatalf("doc_type after a refused retirement = %q, want code (the change must stay visible to the next scan)", doc.DocType)
	}
	if tr.calls != 0 {
		t.Errorf("transcriber calls = %d, want 0 (no video work beside a live code chunk)", tr.calls)
	}

	st.fail.Store(false)
	if err := newUpgradeScanService(t, cfg, st, tr).Run(ctx); err != nil {
		t.Fatalf("retry run: %v", err)
	}
	if doc := mustGetDoc(t, st.SQLiteStore, "news.ts"); doc.DocType != "video" || doc.Status != "ok" {
		t.Fatalf("news.ts after the retry: type=%q status=%q, want video/ok", doc.DocType, doc.Status)
	}
	if got := legacyHits(t, st.SQLiteStore, "news.ts"); got != 0 {
		t.Errorf("the code chunk gives %d lexical hit(s) after the retry, want 0", got)
	}
}
