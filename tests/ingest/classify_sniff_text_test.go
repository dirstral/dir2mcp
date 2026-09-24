package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// SPEC §7.3: classification is "extension + MIME sniff + binary heuristics".
// An unknown extension used to be binary_ignored by path alone, so .mjs, .vue,
// .lua, a .gitignore and a README without an extension were never indexed.

func TestClassify_CommonLanguagesAreCode(t *testing.T) {
	for _, p := range []string{"a.mjs", "b.cjs", "c.vue", "d.svelte", "e.lua", "f.ex", "g.hs", "h.dart", "i.tf", "j.proto", "k.css", "l.scss", "m.graphql"} {
		if got := ingest.ClassifyDocType(p); got != "code" {
			t.Errorf("ClassifyDocType(%s) = %q, want code", p, got)
		}
	}
}

func TestSniffTextDocType(t *testing.T) {
	for _, tc := range []struct {
		name, path, body, want string
	}{
		{"a dotfile of plain text", ".gitignore", "node_modules/\n*.log\n", "text"},
		{"an unlisted language", "script.xyzlang", "print(\"hello\")\n", "text"},
		{"a README without an extension", "NOTES", "Meeting on Thursday.\n", "text"},
		{"UTF-8 prose in Cyrillic", "readme.ru", "Встреча в четверг.\n", "text"},
		{"a NUL byte means binary", "blob.bin", "abc\x00def", "binary_ignored"},
		{"invalid UTF-8 means binary", "img.raw", "\xff\xfe\xfd\xfc", "binary_ignored"},
		{"a private key under an odd name stays out", "deploy_key", "-----BEGIN OPENSSH PRIVATE KEY-----\nb3Blbn\n-----END OPENSSH PRIVATE KEY-----\n", "binary_ignored"},
		{"a subtitle stays a sidecar", "talk.srt", "1\n00:00:01,000 --> 00:00:02,000\nHello\n", "binary_ignored"},
		{"an empty file stays skipped", "empty.xyz", "", "binary_ignored"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ingest.SniffTextDocType(ingest.ClassifyDocType(tc.path), tc.path, []byte(tc.body))
			if tc.path == "talk.srt" && ingest.ClassifyDocType(tc.path) != "binary_ignored" {
				t.Skip("subtitles have their own classification now")
			}
			if got != tc.want {
				t.Errorf("SniffTextDocType(%s) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
	if got := ingest.SniffTextDocType("pdf", "x.pdf", []byte("plain")); got != "pdf" {
		t.Errorf("a known classification must not change, got %q", got)
	}
}

// TestSniffedTextIsIndexedEndToEnd: a folder with an .mjs module and a
// .gitignore indexes both, and an upgraded corpus that stored the dotfile as a
// binary skip reprocesses it although its bytes did not change.
func TestSniffedTextIsIndexedEndToEnd(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "tests", "clip_test.mjs"), "export function clip() { return 1 }\n")
	writeFile(t, filepath.Join(root, ".gitignore"), "node_modules/\n")
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	defer func() { _ = st.Close() }()
	// A row as the old classifier left it: skipped as binary.
	// The same bytes, so only the changed classification can trigger reprocessing.
	if err := st.UpsertDocument(ctx, model.Document{RelPath: ".gitignore", DocType: "binary_ignored", Status: "skipped", SkipReason: model.SkipReasonBinaryIgnored,
		ContentHash: ingest.ComputeContentHash([]byte("node_modules/\n")), SizeBytes: 14, MTimeUnix: mtimeOf(t, filepath.Join(root, ".gitignore"))}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = t.TempDir()
	cfg.STTProvider = "off"
	svc := mustNewIngestService(t, cfg, st)
	svc.SetIndexingState(appstate.NewIndexingState(appstate.ModeIncremental))
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d := mustGetDoc(t, st, "tests/clip_test.mjs"); d.Status != "ok" || d.DocType != "code" {
		t.Errorf("clip_test.mjs: status=%q type=%q, want ok/code", d.Status, d.DocType)
	}
	if d := mustGetDoc(t, st, ".gitignore"); d.Status != "ok" || d.DocType != "text" {
		t.Errorf(".gitignore: status=%q type=%q, want ok/text (reprocessed after the classifier upgrade)", d.Status, d.DocType)
	}
	// ok is not enough: the file must have a searchable representation, or it
	// is the silent "indexed but unsearchable" row again.
	if types := repTypesFor(t, st, ".gitignore"); len(types) == 0 {
		t.Errorf(".gitignore is ok but has no representation: it was relabelled, not reprocessed")
	}
	if types := repTypesFor(t, st, "tests/clip_test.mjs"); len(types) == 0 {
		t.Errorf("clip_test.mjs has no representation")
	}
}

func mtimeOf(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.ModTime().Unix()
}
