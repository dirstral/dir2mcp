package tests

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// #980. Every count-reading check reports ok when its count is zero, so a corpus
// that indexed NOTHING produced the same all-green report as one that indexed
// everything, and no line anywhere said how many documents the record held. That
// state is reachable by a plain typo in source.path, or by a first scan that
// died after `up` created meta.sqlite. Every search then returns nothing while
// the health report says the install is perfect.

func TestDoctor_AnEmptyRecordOverANonEmptyCorpusIsAWarning(t *testing.T) {
	dir := t.TempDir()
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
	if !strings.Contains(check.Detail, "no documents") {
		t.Errorf("the detail does not state the record is empty: %q", check.Detail)
	}
	if !strings.Contains(check.Detail, "source.path") {
		t.Errorf("the detail names no remedy: %q", check.Detail)
	}
}

func TestDoctor_AnEmptyRecordOverAnEmptyCorpusIsNotAWarning(t *testing.T) {
	// Nothing indexed from nothing is correct, not a fault. Warning here would
	// train an operator to ignore the check on a corpus they have not filled yet.
	dir := t.TempDir()
	seedEmptyStore(t, dir)

	check, _ := doctorCheckNamed(t, dir, "corpus_record")
	if check.Status != "ok" {
		t.Errorf("status = %q, want ok for an empty corpus directory", check.Status)
	}
	if !strings.Contains(check.Detail, "empty too") {
		t.Errorf("the detail does not explain why this is fine: %q", check.Detail)
	}
}

func TestDoctor_APopulatedRecordAlwaysStatesItsSize(t *testing.T) {
	// The count is printed on every healthy run, so "nothing indexed" can never
	// look like "everything fine" through a line that is simply absent.
	dir := t.TempDir()
	seedTranscripts(t, dir, seedRep{relPath: "rfe/a.mp4",
		metaJSON: `{"source":"stt","provider":"whisper","model":"large-v3"}`})

	check, _ := doctorCheckNamed(t, dir, "corpus_record")
	if check.Status != "ok" {
		t.Errorf("status = %q, want ok", check.Status)
	}
	if !strings.Contains(check.Detail, "1 document(s) in the record") {
		t.Errorf("the size is not stated: %q", check.Detail)
	}
}

func TestDoctor_DurableSkipsAreNamedWithTheirReasons(t *testing.T) {
	// SkipSummary was read by `status` and `reindex` and by no doctor check, so
	// files dropped for size_cap or language_uncovered never reached the surface
	// an operator asks. extraction_coverage catches only the format-class gap.
	dir := t.TempDir()
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

func TestDoctor_NoSkipsIsReportedPositively(t *testing.T) {
	dir := t.TempDir()
	seedTranscripts(t, dir, seedRep{relPath: "rfe/a.mp4",
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

func TestDoctor_AnEmptyRecordOverAnUninspectableSourceIsNotCalledEmpty(t *testing.T) {
	// "empty", "not empty" and "I could not look" are three different answers.
	// Collapsing the third into the first is what lets an empty record over an
	// unreachable mount read as a clean bill — the exact bug class this check
	// exists to remove, committed by the check itself.
	dir := t.TempDir()
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

func TestDoctor_AnUnreadableCorpusRootIsReportedNotAssumedEmpty(t *testing.T) {
	// Same rule for a local root that cannot be opened: a missing or unreadable
	// corpus path is a fact about the probe, not a fact about the corpus.
	dir := t.TempDir()
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
