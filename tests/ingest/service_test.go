package tests

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/elevenlabs"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

func TestServiceRun_ProcessesFilesAndMarksMissingDeleted(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "keep.txt"), []byte("plain text"))
	mustWriteFile(t, filepath.Join(root, "code", "main.go"), []byte("package main\n"))
	// A REAL (if empty) zip. This fixture stands for "an archive container is
	// skipped", so it must be readable: since #658 an archive whose bytes cannot
	// be opened is a durable status="error", not a skip.
	mustWriteFile(t, filepath.Join(root, "archive.zip"), buildZip(t, nil))
	mustWriteFile(t, filepath.Join(root, "secret.txt"), []byte("Authorization: Bearer abcdefgh.ijklmnop.qrstuvwx\n"))
	mustWriteFile(t, filepath.Join(root, "exclude", "private.txt"), []byte("should be excluded"))

	st := newMemoryStore()
	st.docs["gone.txt"] = model.Document{
		RelPath:   "gone.txt",
		DocType:   "text",
		SizeBytes: 4,
		MTimeUnix: 1,
		Status:    "ok",
	}
	st.docs["exclude/private.txt"] = model.Document{
		RelPath:   "exclude/private.txt",
		DocType:   "text",
		SizeBytes: 1,
		MTimeUnix: 1,
		Status:    "ok",
	}

	cfg := config.Default()
	cfg.RootDir = root
	cfg.PathExcludes = []string{"**/exclude/**"}

	indexState := appstate.NewIndexingState(appstate.ModeIncremental)
	svc := mustNewIngestService(t, cfg, st)
	svc.SetIndexingState(indexState)

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	assertServiceRunSnapshotCounts(t, indexState.Snapshot())
	assertServiceRunDocStatuses(t, st)
}

// TestServiceRun_CountersResetPerScan guards issue #426: the run-progress
// counters must reflect the current scan/corpus, not a monotonic sum of every
// scan the daemon has performed. Two successive scans over an unchanged corpus
// must therefore report the same steady-state totals instead of doubling.
func TestServiceRun_CountersResetPerScan(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "keep.txt"), []byte("plain text"))
	mustWriteFile(t, filepath.Join(root, "code", "main.go"), []byte("package main\n"))
	// A REAL (if empty) zip; see the fixture note in the test above.
	mustWriteFile(t, filepath.Join(root, "archive.zip"), buildZip(t, nil))
	mustWriteFile(t, filepath.Join(root, "secret.txt"), []byte("Authorization: Bearer abcdefgh.ijklmnop.qrstuvwx\n"))

	st := newMemoryStore()
	st.docs["gone.txt"] = model.Document{
		RelPath:   "gone.txt",
		DocType:   "text",
		SizeBytes: 4,
		MTimeUnix: 1,
		Status:    "ok",
	}

	cfg := config.Default()
	cfg.RootDir = root

	indexState := appstate.NewIndexingState(appstate.ModeIncremental)
	svc := mustNewIngestService(t, cfg, st)
	svc.SetIndexingState(indexState)

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("first Run failed: %v", err)
	}
	first := indexState.Snapshot()
	// keep.txt + code/main.go index; archive.zip + secret.txt skip; gone.txt (in
	// the store, absent on disk) is tombstoned.
	if first.Scanned != 4 || first.Indexed != 2 || first.Skipped != 2 || first.Deleted != 1 || first.Errors != 0 {
		t.Fatalf("first run counts unexpected: scanned=%d indexed=%d skipped=%d deleted=%d errors=%d",
			first.Scanned, first.Indexed, first.Skipped, first.Deleted, first.Errors)
	}

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("second Run failed: %v", err)
	}
	second := indexState.Snapshot()
	// Without the per-run reset these would be 8/4/4 (accumulated). gone.txt was
	// already tombstoned, so the second scan sees no new deletions.
	if second.Scanned != 4 {
		t.Fatalf("second run Scanned=%d want=4 (counters accumulated across scans)", second.Scanned)
	}
	if second.Indexed != 2 {
		t.Fatalf("second run Indexed=%d want=2 (counters accumulated across scans)", second.Indexed)
	}
	if second.Skipped != 2 {
		t.Fatalf("second run Skipped=%d want=2 (counters accumulated across scans)", second.Skipped)
	}
	if second.Deleted != 0 {
		t.Fatalf("second run Deleted=%d want=0", second.Deleted)
	}
	if second.Errors != 0 {
		t.Fatalf("second run Errors=%d want=0", second.Errors)
	}
	// The indexed+skipped+errors <= scanned invariant must hold every run.
	if second.Indexed+second.Skipped+second.Errors > second.Scanned {
		t.Fatalf("invariant violated: indexed(%d)+skipped(%d)+errors(%d) > scanned(%d)",
			second.Indexed, second.Skipped, second.Errors, second.Scanned)
	}
}

func assertServiceRunSnapshotCounts(t *testing.T, snapshot appstate.IndexingSnapshot) {
	t.Helper()
	if snapshot.Scanned != 5 {
		t.Fatalf("snapshot.Scanned=%d want=5", snapshot.Scanned)
	}
	if snapshot.Indexed != 2 {
		t.Fatalf("snapshot.Indexed=%d want=2", snapshot.Indexed)
	}
	if snapshot.Skipped != 3 {
		t.Fatalf("snapshot.Skipped=%d want=3", snapshot.Skipped)
	}
	if snapshot.Deleted != 2 {
		t.Fatalf("snapshot.Deleted=%d want=2", snapshot.Deleted)
	}
	if snapshot.Errors != 0 {
		t.Fatalf("snapshot.Errors=%d want=0", snapshot.Errors)
	}
}

func assertServiceRunDocStatuses(t *testing.T, st *memoryStore) {
	t.Helper()
	keep := st.docs["keep.txt"]
	if keep.Status != "ok" {
		t.Fatalf("keep.txt status=%q want=ok", keep.Status)
	}
	if keep.DocType != "text" {
		t.Fatalf("keep.txt doc_type=%q want=text", keep.DocType)
	}
	if keep.ContentHash == "" {
		t.Fatal("keep.txt content hash should not be empty")
	}

	code := st.docs["code/main.go"]
	if code.Status != "ok" || code.DocType != "code" {
		t.Fatalf("code/main.go unexpected doc: %#v", code)
	}

	archive := st.docs["archive.zip"]
	if archive.Status != "skipped" || archive.DocType != "archive" {
		t.Fatalf("archive.zip unexpected doc: %#v", archive)
	}

	secret := st.docs["secret.txt"]
	if secret.Status != "secret_excluded" {
		t.Fatalf("secret.txt status=%q want=secret_excluded", secret.Status)
	}

	excluded := st.docs["exclude/private.txt"]
	if !excluded.Deleted {
		t.Fatalf("exclude/private.txt should be marked deleted, got %#v", excluded)
	}

	gone := st.docs["gone.txt"]
	if !gone.Deleted {
		t.Fatalf("gone.txt should be marked deleted, got %#v", gone)
	}
}

func TestServiceRun_UnicodeDashesStillGenerateRepresentations(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "docs", "flow.md"), []byte("x402 – Payment Flow\navoid hard‑coding secrets.\n"))

	cfg := config.Default()
	cfg.RootDir = root

	st := newMemoryStore()
	svc := mustNewIngestService(t, cfg, st)

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	doc := st.docs["docs/flow.md"]
	if doc.Status != "ok" {
		t.Fatalf("flow.md status=%q want=ok", doc.Status)
	}
	if len(st.reps) == 0 {
		t.Fatal("expected at least one representation for unicode markdown")
	}
	if len(st.chunks) == 0 {
		t.Fatal("expected at least one chunk for unicode markdown")
	}
}

// TestServiceRun_OnDocumentsDeletedHookFired verifies that
// SetOnDocumentsDeleted receives a single batch of tombstoned documents, and
// does not report documents that still exist on disk.
func TestServiceRun_OnDocumentsDeletedHookFired(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "alive.txt"), []byte("still here"))

	st := newMemoryStore()
	// Pre-populate two docs that are no longer on disk so they will be deleted.
	st.docs["gone1.txt"] = model.Document{RelPath: "gone1.txt", DocType: "text", Status: "ok"}
	st.docs["gone2.txt"] = model.Document{RelPath: "gone2.txt", DocType: "text", Status: "ok"}

	cfg := config.Default()
	cfg.RootDir = root

	svc := mustNewIngestService(t, cfg, st)

	var mu sync.Mutex
	var batches [][]string
	svc.SetOnDocumentsDeleted(func(relPaths []string) {
		mu.Lock()
		batches = append(batches, append([]string(nil), relPaths...))
		mu.Unlock()
	})

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(batches) != 1 {
		t.Fatalf("expected exactly one deletion batch, got %d", len(batches))
	}
	deleted := append([]string(nil), batches[0]...)
	sort.Strings(deleted)
	wantDeleted := []string{"gone1.txt", "gone2.txt"}
	if !slices.Equal(deleted, wantDeleted) {
		t.Fatalf("deleted hook received %v, want %v", deleted, wantDeleted)
	}

	// The live document must NOT appear in the deleted list.
	for _, d := range deleted {
		if d == "alive.txt" {
			t.Fatal("alive.txt must not be in the deleted callback list")
		}
	}
}

func TestServiceRun_SetOnDocumentDeletedCompatibilityWrapper(t *testing.T) {
	root := t.TempDir()
	st := newMemoryStore()
	st.docs["gone1.txt"] = model.Document{RelPath: "gone1.txt", DocType: "text", Status: "ok"}
	st.docs["gone2.txt"] = model.Document{RelPath: "gone2.txt", DocType: "text", Status: "ok"}

	cfg := config.Default()
	cfg.RootDir = root

	svc := mustNewIngestService(t, cfg, st)
	var mu sync.Mutex
	var deleted []string
	svc.SetOnDocumentDeleted(func(relPath string) {
		mu.Lock()
		deleted = append(deleted, relPath)
		mu.Unlock()
	})

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	sort.Strings(deleted)
	if !slices.Equal(deleted, []string{"gone1.txt", "gone2.txt"}) {
		t.Fatalf("compatibility wrapper received %v", deleted)
	}
}

func TestServiceRun_SetOnDocumentDeletedCompatibilityWrapperBoundsConcurrency(t *testing.T) {
	root := t.TempDir()
	st := newMemoryStore()
	st.docs["gone1.txt"] = model.Document{RelPath: "gone1.txt", DocType: "text", Status: "ok"}
	st.docs["gone2.txt"] = model.Document{RelPath: "gone2.txt", DocType: "text", Status: "ok"}

	cfg := config.Default()
	cfg.RootDir = root

	svc := mustNewIngestService(t, cfg, st)
	svc.SetOnDocumentDeletedMaxConcurrency(1)

	started := make(chan string, 2)
	release := make(chan struct{})
	var mu sync.Mutex
	current := 0
	maxSeen := 0
	svc.SetOnDocumentDeleted(func(relPath string) {
		mu.Lock()
		current++
		if current > maxSeen {
			maxSeen = current
		}
		mu.Unlock()

		started <- relPath
		<-release

		mu.Lock()
		current--
		mu.Unlock()
	})

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- svc.Run(context.Background())
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first delete callback did not start")
	}

	select {
	case relPath := <-started:
		t.Fatalf("second callback started before release: %s", relPath)
	case <-time.After(100 * time.Millisecond):
	}

	release <- struct{}{}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("second delete callback did not start after release")
	}

	release <- struct{}{}

	select {
	case err := <-runErrCh:
		if err != nil {
			t.Fatalf("Run failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not finish")
	}

	mu.Lock()
	defer mu.Unlock()
	if maxSeen != 1 {
		t.Fatalf("max concurrent callbacks=%d want=1", maxSeen)
	}
}

func TestServiceRun_OnDocumentDeletedPanicRecovered(t *testing.T) {
	root := t.TempDir()

	st := newMemoryStore()
	st.docs["gone1.txt"] = model.Document{RelPath: "gone1.txt", DocType: "text", Status: "ok"}
	st.docs["gone2.txt"] = model.Document{RelPath: "gone2.txt", DocType: "text", Status: "ok"}

	cfg := config.Default()
	cfg.RootDir = root

	indexState := appstate.NewIndexingState(appstate.ModeIncremental)
	svc := mustNewIngestService(t, cfg, st)
	svc.SetIndexingState(indexState)
	var buf bytes.Buffer
	svc.SetLogger(log.New(&buf, "", 0))
	var notified []string
	var notifiedMu sync.Mutex
	svc.SetOnDocumentDeleted(func(relPath string) {
		notifiedMu.Lock()
		notified = append(notified, relPath)
		notifiedMu.Unlock()
		if relPath == "gone1.txt" {
			panic("boom secret-token")
		}
	})

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if !st.docs["gone1.txt"].Deleted {
		t.Fatal("gone1.txt should still be tombstoned when delete hook panics")
	}
	if !st.docs["gone2.txt"].Deleted {
		t.Fatal("gone2.txt should still be tombstoned after a prior hook panic")
	}

	snapshot := indexState.Snapshot()
	if snapshot.Deleted != 2 {
		t.Fatalf("snapshot.Deleted=%d want=2", snapshot.Deleted)
	}
	if snapshot.Errors != 1 {
		t.Fatalf("snapshot.Errors=%d want=1", snapshot.Errors)
	}
	notifiedMu.Lock()
	defer notifiedMu.Unlock()
	if !slices.Contains(notified, "gone2.txt") {
		t.Fatalf("expected gone2.txt notification after earlier panic, got %v", notified)
	}
	if strings.Contains(buf.String(), "secret-token") {
		t.Fatalf("panic payload leaked to logs: %q", buf.String())
	}
}

func TestServiceRun_DoesNotExcludeAWSSecretsManagerProse(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "security.md"), []byte("Credentials are stored in AWS Secrets Manager.\n"))

	cfg := config.Default()
	cfg.RootDir = root

	st := newMemoryStore()
	svc := mustNewIngestService(t, cfg, st)

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	doc := st.docs["security.md"]
	if doc.Status != "ok" {
		t.Fatalf("security.md status=%q want=ok", doc.Status)
	}
}

func TestServiceRun_StillExcludesAWSSecretAssignments(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "secrets.md"), []byte("AWS Secret Access Key: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n"))

	cfg := config.Default()
	cfg.RootDir = root

	st := newMemoryStore()
	svc := mustNewIngestService(t, cfg, st)

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	doc := st.docs["secrets.md"]
	if doc.Status != "secret_excluded" {
		t.Fatalf("secrets.md status=%q want=secret_excluded", doc.Status)
	}
}

// TestServiceRun_RepGenerationFailureMarksDocAsError verifies that when
// representation generation fails (e.g. a transient store error), the
// document is updated to status=error rather than being left as status=ok
// with zero representations.
func TestServiceRun_RepGenerationFailureMarksDocAsError(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "fail.txt"), []byte("some valid text content\n"))

	cfg := config.Default()
	cfg.RootDir = root

	inner := newMemoryStore()
	st := &failingChunkMemoryStore{memoryStore: inner}
	svc := mustNewIngestService(t, cfg, st)

	// Run should not return a top-level error; per-document errors are swallowed.
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	doc, ok := st.docs["fail.txt"]
	if !ok {
		t.Fatal("expected fail.txt to be persisted in store")
	}
	if doc.Status != "error" {
		t.Fatalf("expected fail.txt status=error after rep generation failure, got %q", doc.Status)
	}
	if len(st.reps) != 0 {
		t.Fatalf("expected no representations after rollback, got %d", len(st.reps))
	}
}

// TestServiceRun_RepGenerationFailureCountsErrorNotIndexed guards the
// double-count half of issue #426: a document whose representation generation
// fails must count solely as an error, never as both indexed and error, so the
// indexed+skipped+errors <= scanned invariant holds.
func TestServiceRun_RepGenerationFailureCountsErrorNotIndexed(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "fail.txt"), []byte("some valid text content\n"))

	cfg := config.Default()
	cfg.RootDir = root

	inner := newMemoryStore()
	st := &failingChunkMemoryStore{memoryStore: inner}

	indexState := appstate.NewIndexingState(appstate.ModeIncremental)
	svc := mustNewIngestService(t, cfg, st)
	svc.SetIndexingState(indexState)

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	snap := indexState.Snapshot()
	if snap.Errors != 1 {
		t.Fatalf("Errors=%d want=1", snap.Errors)
	}
	if snap.Indexed != 0 {
		t.Fatalf("Indexed=%d want=0 (failed rep-gen must not also count as indexed)", snap.Indexed)
	}
	if snap.Indexed+snap.Skipped+snap.Errors > snap.Scanned {
		t.Fatalf("invariant violated: indexed(%d)+skipped(%d)+errors(%d) > scanned(%d)",
			snap.Indexed, snap.Skipped, snap.Errors, snap.Scanned)
	}
}

// TestServiceRun_ErrorStatusDocIsRetriedOnNextRun verifies that a document
// previously stuck with status=error (zero representations) is re-processed on
// the next incremental scan even when its content hash has not changed.
func TestServiceRun_ErrorStatusDocIsRetriedOnNextRun(t *testing.T) {
	root := t.TempDir()
	content := []byte("some valid text content\n")
	mustWriteFile(t, filepath.Join(root, "retry.txt"), content)

	cfg := config.Default()
	cfg.RootDir = root

	st := newMemoryStore()
	// Pre-populate the store simulating a previous run that left the document
	// stuck in error status with the same content hash.
	st.docs["retry.txt"] = model.Document{
		DocID:       1,
		RelPath:     "retry.txt",
		DocType:     "text",
		SizeBytes:   int64(len(content)),
		ContentHash: ingest.ComputeContentHash(content),
		Status:      "error",
	}

	svc := mustNewIngestService(t, cfg, st)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	doc := st.docs["retry.txt"]
	if doc.Status != "ok" {
		t.Fatalf("expected retry.txt to be re-indexed to ok, got %q", doc.Status)
	}
	if len(st.reps) == 0 {
		t.Fatal("expected representations to be generated on retry")
	}
}

func TestServiceRun_ReturnsErrorOnInvalidSecretPattern(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "keep.txt"), []byte("plain text"))

	cfg := config.Default()
	cfg.RootDir = root
	cfg.SecretPatterns = []string{"["}

	svc := mustNewIngestService(t, cfg, newMemoryStore())
	if err := svc.Run(context.Background()); err == nil {
		t.Fatal("expected error for invalid secret pattern")
	}
}

func TestServiceRun_ContextCancelled(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "keep.txt"), []byte("plain text"))

	cfg := config.Default()
	cfg.RootDir = root

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	svc := mustNewIngestService(t, cfg, newMemoryStore())
	if err := svc.Run(ctx); err == nil {
		t.Fatal("expected context cancellation error")
	}
}

// verify that when a document is upserted and processed, any generated
// representations include the persisted DocID instead of zero.  This
// guards against the previous bug where the in-memory doc lacked an ID and
// orphaned rows were written.
func TestProcessDocument_DocIDSetBeforeRepGeneration(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "foo.txt")
	if err := os.WriteFile(path, []byte("hello world"), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	cfg := config.Default()
	cfg.RootDir = root

	st := newMemoryStore()
	svc := mustNewIngestService(t, cfg, st)

	// run a single scan by invoking Run; service will create raw text
	// representation since memoryStore implements model.RepresentationStore.
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if len(st.reps) == 0 {
		t.Fatal("expected at least one representation")
	}
	if st.reps[0].DocID == 0 {
		t.Fatalf("representation created with zero DocID")
	}
}

func TestServiceRun_AudioGeneratesTranscriptRepresentation(t *testing.T) {
	root := t.TempDir()
	audioPath := filepath.Join(root, "audio", "sample.mp3")
	mustWriteFile(t, audioPath, []byte("fake-audio-bytes"))

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = filepath.Join(root, ".dir2mcp")

	st := newMemoryStore()
	svc := mustNewIngestService(t, cfg, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] hello\n[00:02] world"})

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if len(st.reps) != 1 {
		t.Fatalf("expected one representation, got %d", len(st.reps))
	}
	if st.reps[0].RepType != ingest.RepTypeTranscript {
		t.Fatalf("expected transcript rep type, got %q", st.reps[0].RepType)
	}
	if len(st.chunks) != 2 {
		t.Fatalf("expected two transcript chunks, got %d", len(st.chunks))
	}
	if len(st.spans) != 2 {
		t.Fatalf("expected two transcript spans, got %d", len(st.spans))
	}
	if st.spans[0].Kind != "time" || st.spans[0].StartMS != 0 || st.spans[0].EndMS != 2000 {
		t.Fatalf("unexpected first transcript span: %+v", st.spans[0])
	}
}

type errTranscriber struct {
	err error
}

func (e errTranscriber) Transcribe(context.Context, string, []byte) (string, error) {
	return "", e.err
}

func TestServiceRun_AudioTranscriberFailure_DoesNotFailRun(t *testing.T) {
	root := t.TempDir()
	audioPath := filepath.Join(root, "audio", "broken.mp3")
	mustWriteFile(t, audioPath, []byte("fake-audio-bytes"))

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = filepath.Join(root, ".dir2mcp")

	st := newMemoryStore()
	state := appstate.NewIndexingState(appstate.ModeIncremental)
	svc := mustNewIngestService(t, cfg, st)
	svc.SetIndexingState(state)
	svc.SetTranscriber(errTranscriber{err: errors.New("provider down")})

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run should continue on transcription failure, got: %v", err)
	}

	snapshot := state.Snapshot()
	if snapshot.Errors == 0 {
		t.Fatalf("expected indexing error count increment on transcription failure, got %+v", snapshot)
	}
	if len(st.reps) != 0 {
		t.Fatalf("expected no transcript representations when transcription fails, got %d", len(st.reps))
	}
	doc, ok := st.docs["audio/broken.mp3"]
	if !ok {
		t.Fatal("expected audio document to be upserted")
	}
	// #413: a genuine provider failure that produced zero representations must be
	// persisted as status="error" with a descriptive message — not silently left
	// status="ok", which hid the unsearchable audio from CorpusStats.Errors /
	// RecentFailures and reported errors=0 after a restart. The run itself still
	// must not fail (asserted above).
	if doc.Status != "error" {
		t.Fatalf("expected audio document status to be persisted as error, got %q", doc.Status)
	}
	if doc.ErrorMessage == "" {
		t.Fatal("expected a descriptive error_message on the failed audio document")
	}
}

// TestServiceRun_MediaVariantGroupingDedupsThroughScan proves the production
// scan path (Service.Run -> runScan) actually applies §8.6.5 variant dedup when
// media.variants.group is enabled: only the canonical rendition is ingested as a
// document, the dropped renditions never produce documents/chunks, and unrelated
// media is untouched.
func TestServiceRun_MediaVariantGroupingDedupsThroughScan(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "clip.1080p.mp4"), []byte("hi"))
	mustWriteFile(t, filepath.Join(root, "clip.720p.mp4"), []byte("medium"))
	mustWriteFile(t, filepath.Join(root, "clip.480p.mp4"), []byte("the-largest-bytes"))
	mustWriteFile(t, filepath.Join(root, "other.mp4"), []byte("distinct"))

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = filepath.Join(root, ".dir2mcp")
	cfg.MediaVariantsGroup = true
	cfg.MediaVariantsSelect = "best"

	st := newMemoryStore()
	svc := mustNewIngestService(t, cfg, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] hello"})

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if _, ok := st.docs["clip.720p.mp4"]; ok {
		t.Fatalf("dropped rendition clip.720p.mp4 must not be ingested as a document")
	}
	if _, ok := st.docs["clip.480p.mp4"]; ok {
		t.Fatalf("dropped rendition clip.480p.mp4 must not be ingested as a document")
	}
	if _, ok := st.docs["clip.1080p.mp4"]; !ok {
		t.Fatalf("canonical rendition clip.1080p.mp4 must be ingested")
	}
	if _, ok := st.docs["other.mp4"]; !ok {
		t.Fatalf("unrelated media other.mp4 must be ingested")
	}
}

// TestServiceRun_MediaVariantGroupingDisabled_IngestsAllRenditions confirms the
// default (group=false) leaves the scan path unchanged: every rendition is
// ingested.
func TestServiceRun_MediaVariantGroupingDisabled_IngestsAllRenditions(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "clip.1080p.mp4"), []byte("hi"))
	mustWriteFile(t, filepath.Join(root, "clip.720p.mp4"), []byte("medium"))

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = filepath.Join(root, ".dir2mcp")
	// MediaVariantsGroup defaults to false.

	st := newMemoryStore()
	svc := mustNewIngestService(t, cfg, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] hello"})

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	for _, p := range []string{"clip.1080p.mp4", "clip.720p.mp4"} {
		if _, ok := st.docs[p]; !ok {
			t.Fatalf("with grouping disabled, rendition %q must be ingested", p)
		}
	}
}

func TestDiscoverOptionsFromConfig_DefaultsRemainSafe(t *testing.T) {
	cfg := config.Default()
	options := ingest.DiscoverOptionsFromConfig(cfg)
	if options.FollowSymlinks {
		t.Fatal("expected follow_symlinks default to false")
	}
	if !options.UseGitIgnore {
		t.Fatal("expected gitignore default to true")
	}
	if options.MaxSizeBytes <= 0 {
		t.Fatalf("expected positive max size default, got %d", options.MaxSizeBytes)
	}
}

func TestTranscriberFromConfig_AutoWiresElevenLabsWhenAPIKeyPresent(t *testing.T) {
	cfg := config.Default()
	cfg.ElevenLabsAPIKey = "test-key"
	cfg.ElevenLabsBaseURL = "https://example.test"
	cfg.STTProvider = "elevenlabs"

	transcriber, err := ingest.TranscriberFromConfig(cfg)
	if err != nil {
		t.Fatalf("TranscriberFromConfig failed: %v", err)
	}
	if transcriber == nil {
		t.Fatal("expected transcriber instance")
	}
	client, ok := transcriber.(*elevenlabs.Client)
	if !ok {
		t.Fatalf("expected elevenlabs client, got %T", transcriber)
	}
	if client.BaseURL != "https://example.test" {
		t.Fatalf("unexpected base URL: %q", client.BaseURL)
	}
}

func TestTranscriberFromConfig_ExplicitProviderRequiresCredentials(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		wantErr  string
	}{
		{
			// legacy stt.provider "mistral" maps to the mistral-ocr
			// profile; without a credential the resolver returns a
			// CONFIG_INVALID naming that profile.
			name:     "mistral missing key",
			provider: "mistral",
			wantErr:  "mistral-ocr",
		},
		{
			// legacy stt.provider "elevenlabs" now resolves the
			// elevenlabs profile; without a credential the resolver
			// returns a CONFIG_INVALID naming that profile.
			name:     "elevenlabs missing key",
			provider: "elevenlabs",
			wantErr:  "elevenlabs",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Credentials resolve from env via the built-in
			// ${MISTRAL_API_KEY}/${ELEVENLABS_API_KEY} placeholders
			// (clean break #38). Clear them so the resolver returns a
			// CONFIG_INVALID naming the explicitly-bound profile.
			t.Setenv("MISTRAL_API_KEY", "")
			t.Setenv("ELEVENLABS_API_KEY", "")
			cfg := config.Default()
			cfg.STTProvider = tc.provider
			cfg.ElevenLabsAPIKey = ""

			transcriber, err := ingest.TranscriberFromConfig(cfg)
			if err == nil {
				t.Fatalf("expected error for provider %q", tc.provider)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error mismatch: got=%v want substring=%q", err, tc.wantErr)
			}
			if transcriber != nil {
				t.Fatalf("expected nil transcriber on config error, got %T", transcriber)
			}
		})
	}
}

func TestTranscriberFromConfig_AutoProviderWithoutCredentialsReturnsNil(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "")
	t.Setenv("ELEVENLABS_API_KEY", "")
	cfg := config.Default()
	cfg.STTProvider = "auto"
	cfg.ElevenLabsAPIKey = ""

	transcriber, err := ingest.TranscriberFromConfig(cfg)
	if err != nil {
		t.Fatalf("TranscriberFromConfig should not fail in auto mode without credentials: %v", err)
	}
	if transcriber != nil {
		t.Fatalf("expected nil transcriber in auto mode without credentials, got %T", transcriber)
	}
}

type memoryStore struct {
	docs map[string]model.Document
	// hold persisted representations for verification
	reps   []model.Representation
	chunks []model.Chunk
	spans  []model.Span
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		docs:   make(map[string]model.Document),
		reps:   make([]model.Representation, 0),
		chunks: make([]model.Chunk, 0),
		spans:  make([]model.Span, 0),
	}
}

func (s *memoryStore) Init(_ context.Context) error { return nil }

func (s *memoryStore) UpsertDocument(_ context.Context, doc model.Document) error {
	current, ok := s.docs[doc.RelPath]
	if ok {
		doc.DocID = current.DocID
	} else {
		doc.DocID = int64(len(s.docs) + 1)
	}
	s.docs[doc.RelPath] = doc
	return nil
}

// representationStore (model.RepresentationStore) methods ------------------------------------------------
func (s *memoryStore) UpsertRepresentation(_ context.Context, rep model.Representation) (int64, error) {
	rep.RepID = int64(len(s.reps) + 1)
	s.reps = append(s.reps, rep)
	return rep.RepID, nil
}

func (s *memoryStore) InsertChunkWithSpans(_ context.Context, chunk model.Chunk, spans []model.Span) (int64, error) {
	// assign a deterministic ID before storing, mirroring the behavior of
	// UpsertRepresentation above. This ensures that any tests examining the
	// returned identifier or verifying relationships between chunks and
	// spans will see consistent values.
	chunk.ChunkID = uint64(len(s.chunks) + 1)

	// store the chunk with its ID and append spans in sequence; the span
	// records themselves don’t hold the chunk ID, but callers may rely on the
	// returned ID to correlate the two.
	s.chunks = append(s.chunks, chunk)
	s.spans = append(s.spans, spans...)
	return int64(chunk.ChunkID), nil
}

func (s *memoryStore) SoftDeleteChunksFromOrdinal(_ context.Context, _ int64, _ int) error {
	return nil
}

// WithTx is a noop for the in-memory implementation since there is no
// underlying database to transact against.
func (s *memoryStore) WithTx(ctx context.Context, fn func(tx model.RepresentationStore) error) error {
	return fn(s)
}

func (s *memoryStore) GetDocumentByPath(_ context.Context, relPath string) (model.Document, error) {
	doc, ok := s.docs[relPath]
	if !ok {
		return model.Document{}, os.ErrNotExist
	}
	return doc, nil
}

func (s *memoryStore) ListFiles(_ context.Context, prefix, glob string, limit, offset int) ([]model.Document, int64, error) {
	if limit <= 0 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}

	keys := make([]string, 0, len(s.docs))
	for relPath := range s.docs {
		if strings.TrimSpace(prefix) != "" && !strings.HasPrefix(relPath, prefix) {
			continue
		}
		if strings.TrimSpace(glob) != "" {
			match, err := path.Match(glob, relPath)
			if err != nil || !match {
				continue
			}
		}
		keys = append(keys, relPath)
	}
	sort.Strings(keys)

	total := int64(len(keys))
	if offset >= len(keys) {
		return []model.Document{}, total, nil
	}
	end := offset + limit
	if end > len(keys) {
		end = len(keys)
	}

	out := make([]model.Document, 0, end-offset)
	for _, key := range keys[offset:end] {
		out = append(out, s.docs[key])
	}
	return out, total, nil
}

func (s *memoryStore) Close() error { return nil }

func (s *memoryStore) MarkDocumentDeleted(_ context.Context, relPath string) error {
	doc, ok := s.docs[relPath]
	if !ok {
		doc = model.Document{RelPath: relPath}
	}
	doc.Deleted = true
	s.docs[relPath] = doc
	return nil
}

// failingChunkMemoryStore wraps memoryStore but always returns an error from
// InsertChunkWithSpans to simulate a representation generation failure.
type failingChunkMemoryStore struct {
	*memoryStore
}

func (s *failingChunkMemoryStore) InsertChunkWithSpans(_ context.Context, _ model.Chunk, _ []model.Span) (int64, error) {
	return 0, errors.New("injected chunk insert failure")
}

// WithTx must pass the wrapper (not the inner store) to fn so that the
// overridden InsertChunkWithSpans is called and the rollback logic covers
// the representation upserted inside the transaction.
func (s *failingChunkMemoryStore) WithTx(ctx context.Context, fn func(tx model.RepresentationStore) error) error {
	origReps := append([]model.Representation(nil), s.reps...)
	origChunks := append([]model.Chunk(nil), s.chunks...)
	origSpans := append([]model.Span(nil), s.spans...)
	err := fn(s)
	if err != nil {
		s.reps = origReps
		s.chunks = origChunks
		s.spans = origSpans
	}
	return err
}

func TestMemoryStoreListFilesPaging(t *testing.T) {
	st := newMemoryStore()
	st.docs["a.txt"] = model.Document{RelPath: "a.txt"}
	st.docs["b.txt"] = model.Document{RelPath: "b.txt"}
	st.docs["c.txt"] = model.Document{RelPath: "c.txt"}

	page1, total, err := st.ListFiles(context.Background(), "", "", 2, 0)
	if err != nil {
		t.Fatalf("ListFiles page1 failed: %v", err)
	}
	if total != 3 {
		t.Fatalf("total=%d want=3", total)
	}
	gotPage1 := []string{page1[0].RelPath, page1[1].RelPath}
	if !slices.Equal(gotPage1, []string{"a.txt", "b.txt"}) {
		t.Fatalf("page1 unexpected: %v", gotPage1)
	}

	page2, _, err := st.ListFiles(context.Background(), "", "", 2, 2)
	if err != nil {
		t.Fatalf("ListFiles page2 failed: %v", err)
	}
	if len(page2) != 1 || page2[0].RelPath != "c.txt" {
		t.Fatalf("page2 unexpected: %#v", page2)
	}
}
