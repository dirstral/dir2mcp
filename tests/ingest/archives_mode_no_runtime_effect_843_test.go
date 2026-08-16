package tests

// `ingest.archives.mode` has no runtime effect, and that fact is pinned here on
// observable ingest output: #843.
//
// The shipped default is `deep`, whose name says an archive nested inside an
// archive is expanded. It is not. Nothing in the ingestion runtime reads the
// value, so all three accepted members (`off`, `shallow`, `deep`) index exactly
// the same documents: the top level of one container, and nothing below it.
//
// The assertion is deliberately on the store, not on a flag. A test that read
// cfg.IngestArchivesMode back would pass while the promise stayed broken, which
// is how the gap survived this long. tests/config asserts the operator is told.
//
// This test is the tripwire for the real fix as well. When a dirstral-spec PR
// defines what each member does and dir2mcp implements it, this test MUST fail:
// `off` will stop expanding and `deep` will recurse. Update it then, and only
// with a merged spec change behind it.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// ingestUnderArchivesMode indexes one corpus holding an archive nested inside an
// archive, under the given ingest.archives.mode, and returns the store.
func ingestUnderArchivesMode(t *testing.T, mode string) *store.SQLiteStore {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()

	innerZip := buildZip(t, map[string]string{"inner.txt": "nested payload"})
	outerZip := buildZip(t, map[string]string{
		"top.txt":   "top-level payload",
		"inner.zip": string(innerZip),
	})
	if err := os.WriteFile(filepath.Join(root, "outer.zip"), outerZip, 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = filepath.Join(root, ".dir2mcp")
	cfg.IngestArchivesMode = mode
	if err := cfg.Validate(); err != nil {
		t.Fatalf("ingest.archives.mode=%q must validate: %v", mode, err)
	}
	if err := mustNewIngestService(t, cfg, st).Run(ctx); err != nil {
		t.Fatalf("Run under ingest.archives.mode=%q: %v", mode, err)
	}
	return st
}

// corpusRecords renders every stored document as one comparable line holding
// the state a client can observe: the path, whether it was indexed or skipped,
// why it was skipped, how it was classified, and which representations it
// produced.
//
// A rel_path set on its own is too weak to hold the promise. A build that
// recorded `outer.zip/top.txt` as a skipped row, or that stored it with no
// representation, would keep the same set of paths while indexing nothing, and
// the test would still pass. The status and the representation types are what a
// search actually depends on, so they are what is compared.
func corpusRecords(t *testing.T, st *store.SQLiteStore) []string {
	t.Helper()
	ctx := context.Background()
	docs, _, err := st.ListFiles(ctx, "", "", 1000, 0)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	records := make([]string, 0, len(docs))
	for _, d := range docs {
		reps, err := st.ActiveRepresentations(ctx, d.RelPath)
		if err != nil {
			t.Fatalf("ActiveRepresentations(%s): %v", d.RelPath, err)
		}
		repTypes := make([]string, 0, len(reps))
		for _, r := range reps {
			repTypes = append(repTypes, r.RepType)
		}
		sort.Strings(repTypes)
		records = append(records, fmt.Sprintf("%s status=%s skip=%s doc_type=%s source_type=%s reps=[%s]",
			d.RelPath, d.Status, d.SkipReason, d.DocType, d.SourceType, strings.Join(repTypes, ",")))
	}
	sort.Strings(records)
	return records
}

// TestArchivesMode_EveryValueIndexesTheSameDocuments is the honesty proof. It
// runs one corpus under each accepted member and compares the resulting corpus
// records. They are identical, so no name in the enum describes anything the
// server does differently.
func TestArchivesMode_EveryValueIndexesTheSameDocuments(t *testing.T) {
	want := corpusRecords(t, ingestUnderArchivesMode(t, "shallow"))
	if len(want) == 0 {
		t.Fatal("the reference run indexed nothing; the corpus fixture is broken")
	}
	for _, mode := range []string{"off", "deep"} {
		got := corpusRecords(t, ingestUnderArchivesMode(t, mode))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ingest.archives.mode=%q produced a different corpus:\ngot  %v\nwant %v", mode, got, want)
		}
	}
}

// TestArchivesMode_OffStillExpandsTheTopLevel pins the direction the name gets
// wrong the other way round. An operator who sets `off` for a privacy or cost
// reason still gets every top-level member of every archive indexed, with real
// content behind it.
func TestArchivesMode_OffStillExpandsTheTopLevel(t *testing.T) {
	st := ingestUnderArchivesMode(t, "off")

	member := documentByPath(t, st, "outer.zip/top.txt")
	if member.Status != "ok" {
		t.Errorf("ingest.archives.mode=off does not disable expansion today; outer.zip/top.txt must be status=ok, got %q (skip_reason=%q)",
			member.Status, member.SkipReason)
	}
	if member.SourceType != "archive_member" {
		t.Errorf("outer.zip/top.txt must be recorded as an archive_member; got %q", member.SourceType)
	}

	// status=ok alone would still allow an empty document. The member is only
	// really indexed when it produced retrievable content.
	reps, err := st.ActiveRepresentations(context.Background(), "outer.zip/top.txt")
	if err != nil {
		t.Fatalf("ActiveRepresentations: %v", err)
	}
	if len(reps) == 0 {
		t.Error("ingest.archives.mode=off still extracts the member's content; outer.zip/top.txt must carry at least one representation")
	}
}

// TestArchivesMode_DeepDoesNotRecurse pins the promise in the issue title. Under
// the shipped default the nested archive is stored as a skipped document, and
// its own member never becomes one, so the container reports the gap instead of
// coverage it does not have (#683, #817).
func TestArchivesMode_DeepDoesNotRecurse(t *testing.T) {
	st := ingestUnderArchivesMode(t, "deep")
	paths := docPaths(t, st)

	if !paths["outer.zip/inner.zip"] {
		t.Fatalf("the nested archive must be recorded as a document; got %v", paths)
	}
	if paths["outer.zip/inner.zip/inner.txt"] {
		t.Errorf("ingest.archives.mode=deep does NOT recurse today; outer.zip/inner.zip/inner.txt must not be indexed")
	}
	nested := documentByPath(t, st, "outer.zip/inner.zip")
	if nested.SkipReason != model.SkipReasonArchive {
		t.Errorf("the unexpanded nested archive must carry skip_reason=%q so the gap is visible; got %q",
			model.SkipReasonArchive, nested.SkipReason)
	}
}
