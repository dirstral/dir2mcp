package tests

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// #980. Every count-reading check reports ok when its count is zero, so a corpus
// that indexed NOTHING produced the same all-green report as one that indexed
// everything, and no line anywhere said how many documents the record held. That
// state is reachable by a plain typo in source.path, or by a first scan that
// died after `up` created meta.sqlite. Every search then returns nothing while
// the health report says the install is perfect.

// TestDoctor_AnEmptyRecordOverANonEmptyCorpusIsAWarning pins that the
// corpus_record check warns when the store holds no documents but the corpus
// directory has content, states the zero counts, and names source.path as the
// remedy.
func TestDoctor_AnEmptyRecordOverANonEmptyCorpusIsAWarning(t *testing.T) {
	dir := testutil.TempDir(t)
	// A corpus with content, and a store that recorded none of it.
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("# hello"), 0o644); err != nil {
		t.Fatalf("write corpus file: %v", err)
	}
	seedEmptyStore(t, dir)

	check, ok := doctorCheckNamed(t, dir, "corpus_record")
	if !ok {
		t.Fatalf("doctor has no corpus_record check")
	}
	if check.Status != "warn" {
		t.Errorf("status = %q, want warn: nothing was indexed from a non-empty directory", check.Status)
	}
	if !strings.Contains(check.Detail, "0 document(s) and 0 chunk(s)") {
		t.Errorf("the detail does not state the counts: %q", check.Detail)
	}
	if !strings.Contains(check.Detail, "source.path") {
		t.Errorf("the detail names no remedy: %q", check.Detail)
	}
}

// TestDoctor_AnEmptyRecordOverAnEmptyCorpusIsNotAWarning pins that an empty
// record over an empty corpus directory is ok, states the zero counts, and
// says the directory is "empty too". Nothing indexed from nothing is correct,
// not a fault. A warning here would train an operator to ignore the check on a
// corpus they have not filled yet.
func TestDoctor_AnEmptyRecordOverAnEmptyCorpusIsNotAWarning(t *testing.T) {
	dir := testutil.TempDir(t)
	seedEmptyStore(t, dir)

	check, _ := doctorCheckNamed(t, dir, "corpus_record")
	if check.Status != "ok" {
		t.Errorf("status = %q, want ok for an empty corpus directory", check.Status)
	}
	if !strings.Contains(check.Detail, "empty too") {
		t.Errorf("the detail does not explain why this is fine: %q", check.Detail)
	}
	if !strings.Contains(check.Detail, "0 document(s) and 0 chunk(s)") {
		t.Errorf("the detail does not state the counts: %q", check.Detail)
	}
}

// TestDoctor_APopulatedRecordAlwaysStatesItsSize pins that a healthy
// corpus_record check prints the document count on every run, so "nothing
// indexed" can never look like "everything fine" through a line that is simply
// absent.
func TestDoctor_APopulatedRecordAlwaysStatesItsSize(t *testing.T) {
	dir := testutil.TempDir(t)
	seedTranscripts(t, dir, seedRep{relPath: "archive/a.mp4",
		metaJSON: `{"source":"stt","provider":"whisper","model":"large-v3"}`})

	check, _ := doctorCheckNamed(t, dir, "corpus_record")
	if check.Status != "ok" {
		t.Errorf("status = %q, want ok", check.Status)
	}
	if !strings.Contains(check.Detail, "1 document(s) in the record") {
		t.Errorf("the size is not stated: %q", check.Detail)
	}
}

// TestDoctor_DurableSkipsAreNamedWithTheirReasons pins that the
// skipped_documents check warns, lists each skip reason with its count, and
// states that the files are NOT searchable. SkipSummary was read by `status`
// and `reindex` and by no doctor check, so files dropped for size_cap or
// language_uncovered never reached the surface an operator asks;
// extraction_coverage catches only the format-class gap.
func TestDoctor_DurableSkipsAreNamedWithTheirReasons(t *testing.T) {
	dir := testutil.TempDir(t)
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(dir, ".dir2mcp", "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	for _, d := range []model.Document{
		{RelPath: "big-1.mp4", DocType: "audio", Status: "skipped", SkipReason: "size_cap"},
		{RelPath: "big-2.mp4", DocType: "audio", Status: "skipped", SkipReason: "size_cap"},
		{RelPath: "ru.mp4", DocType: "audio", Status: "skipped", SkipReason: "language_uncovered"},
		{RelPath: "fine.mp4", DocType: "audio", Status: "ok"},
	} {
		if err := st.UpsertDocument(ctx, d); err != nil {
			t.Fatalf("seed %s: %v", d.RelPath, err)
		}
	}
	_ = st.Close()

	check, ok := doctorCheckNamed(t, dir, "skipped_documents")
	if !ok {
		t.Fatalf("doctor has no skipped_documents check")
	}
	if check.Status != "warn" {
		t.Errorf("status = %q, want warn", check.Status)
	}
	// Dominant reason leads, so the line is stable and the biggest gap is first.
	if !strings.Contains(check.Detail, "size_cap (2), language_uncovered (1)") {
		t.Errorf("the breakdown is missing or unordered: %q", check.Detail)
	}
	if !strings.Contains(check.Detail, "NOT searchable") {
		t.Errorf("the consequence is not stated: %q", check.Detail)
	}
}

// TestDoctor_NoSkipsIsReportedPositively pins that a corpus with no skipped
// document gets an ok skipped_documents check whose detail says so in words,
// not through an absent line.
func TestDoctor_NoSkipsIsReportedPositively(t *testing.T) {
	dir := testutil.TempDir(t)
	seedTranscripts(t, dir, seedRep{relPath: "archive/a.mp4",
		metaJSON: `{"source":"stt","provider":"whisper","model":"large-v3"}`})

	check, _ := doctorCheckNamed(t, dir, "skipped_documents")
	if check.Status != "ok" {
		t.Errorf("status = %q, want ok", check.Status)
	}
	if !strings.Contains(check.Detail, "no document was recorded as skipped") {
		t.Errorf("a clean corpus is not stated positively: %q", check.Detail)
	}
}

// seedEmptyStore creates an initialized but document-free meta.sqlite, which is
// what a first scan that died after `up` leaves behind.
func seedEmptyStore(t *testing.T, dir string) {
	t.Helper()
	st := store.NewSQLiteStore(filepath.Join(dir, ".dir2mcp", "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Logf("close: %v", err)
	}
}

// TestDoctor_AnEmptyRecordOverAnUninspectableSourceIsNotCalledEmpty pins that
// an empty record over a remote source the probe cannot read is a warning
// whose detail admits the source "could not be inspected" and never says
// "empty too". "Empty", "not empty" and "I could not look" are three different
// answers. To collapse the third into the first lets an empty record over an
// unreachable mount read as a clean bill: the exact bug class this check
// exists to remove, committed by the check itself.
func TestDoctor_AnEmptyRecordOverAnUninspectableSourceIsNotCalledEmpty(t *testing.T) {
	dir := testutil.TempDir(t)
	seedEmptyStore(t, dir)
	// A remote source: the probe cannot read it cheaply and must not guess.
	// nfs rather than s3 because s3 additionally demands AWS credentials, which
	// would fail config validation before this check ever runs.
	cfgPath := filepath.Join(dir, ".dir2mcp.yaml")
	if err := os.WriteFile(cfgPath, []byte("source:\n  kind: nfs\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	check, ok := doctorCheckNamed(t, dir, "corpus_record")
	if !ok {
		t.Fatalf("doctor has no corpus_record check")
	}
	if strings.Contains(check.Detail, "empty too") {
		t.Fatalf("an uninspected source was asserted to be empty: %q", check.Detail)
	}
	if check.Status != "warn" {
		t.Errorf("status = %q, want warn: the record is empty and the source was never read", check.Status)
	}
	if !strings.Contains(check.Detail, "could not be inspected") {
		t.Errorf("the detail does not admit what it could not do: %q", check.Detail)
	}
}

// TestDoctor_AnUnreadableCorpusRootIsReportedNotAssumedEmpty pins the same
// rule for a local root that cannot be opened: the detail never says "empty
// too". A missing or unreadable corpus path is a fact about the probe, not a
// fact about the corpus.
func TestDoctor_AnUnreadableCorpusRootIsReportedNotAssumedEmpty(t *testing.T) {
	dir := testutil.TempDir(t)
	seedEmptyStore(t, dir)
	cfgPath := filepath.Join(dir, ".dir2mcp.yaml")
	missing := filepath.Join(dir, "not-here")
	if err := os.WriteFile(cfgPath, []byte("root_dir: \""+missing+"\"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	check, ok := doctorCheckNamed(t, dir, "corpus_record")
	if !ok {
		t.Fatalf("doctor has no corpus_record check")
	}
	if strings.Contains(check.Detail, "empty too") {
		t.Errorf("an unreadable root was asserted to be empty: %q", check.Detail)
	}
}

// TestDoctor_TheCountsAreStatedOnEveryPath pins that every corpus_record branch
// states the document and chunk counts, also where the answer is zero. The
// check exists so that "nothing indexed" can never look like "everything fine"
// through a line that is simply absent; a branch that describes the situation
// without the numbers reintroduces that gap.
//
// It also makes one inconsistency visible that nothing else names: zero
// documents with a non-zero chunk count is a store whose chunks outlived their
// documents, which "the record holds no documents" alone would describe as
// merely empty.
func TestDoctor_TheCountsAreStatedOnEveryPath(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{"empty record, non-empty corpus", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("x"), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			seedEmptyStore(t, dir)
		}},
		{"empty record, empty corpus", func(t *testing.T, dir string) {
			seedEmptyStore(t, dir)
		}},
		{"empty record, uninspectable source", func(t *testing.T, dir string) {
			seedEmptyStore(t, dir)
			if err := os.WriteFile(filepath.Join(dir, ".dir2mcp.yaml"),
				[]byte("source:\n  kind: nfs\n"), 0o644); err != nil {
				t.Fatalf("write config: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := testutil.TempDir(t)
			tc.setup(t, dir)
			check, ok := doctorCheckNamed(t, dir, "corpus_record")
			if !ok {
				t.Fatalf("doctor has no corpus_record check")
			}
			if !strings.Contains(check.Detail, "document(s)") || !strings.Contains(check.Detail, "chunk(s)") {
				t.Errorf("the counts are missing on this path: %q", check.Detail)
			}
		})
	}
}
