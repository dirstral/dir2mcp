package tests

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

func TestSQLiteStore_PendingChunkLifecycle(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	defer func() { _ = st.Close() }()

	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// the schema should have indexes to avoid full table scans on pending
	// lookups and rel_path queries.  verify they were created during Init.
	if err := verifyChunkIndexes(ctx, st); err != nil {
		t.Fatalf("index verification failed: %v", err)
	}

	if err := st.UpsertChunkTask(ctx, model.NewChunkTask(101, "chunk text", "text", model.ChunkMetadata{
		ChunkID: 101,
		RelPath: "docs/a.md",
		DocType: "md",
		RepType: "raw_text",
	})); err != nil {
		t.Fatalf("UpsertChunkTask failed: %v", err)
	}

	tasks, err := st.NextPending(ctx, 10, "text")
	if err != nil {
		t.Fatalf("NextPending failed: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected exactly one task, got %#v", tasks)
	}
	// verify all fields persisted correctly
	expectedText := "chunk text"
	expectedIndexKind := "text"
	expectedMetadata := model.ChunkMetadata{
		ChunkID:  101,
		RelPath:  "docs/a.md",
		DocType:  "md",
		RepType:  "raw_text",
		Snippet:  "chunk text",
		Span:     model.Span{Kind: "lines"},
		Modality: "text", // chunks.modality defaults to 'text' (SPEC 8.1.7)
	}
	if tasks[0].Label != 101 || tasks[0].Metadata.ChunkID != 101 ||
		tasks[0].Text != expectedText ||
		tasks[0].IndexKind != expectedIndexKind ||
		!reflect.DeepEqual(tasks[0].Metadata, expectedMetadata) {
		t.Fatalf("unexpected task fields: %#v", tasks)
	}

	if err := st.MarkEmbedded(ctx, []uint64{101}); err != nil {
		t.Fatalf("MarkEmbedded failed: %v", err)
	}
	tasks, err = st.NextPending(ctx, 10, "text")
	if err != nil {
		t.Fatalf("NextPending failed: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("expected no pending tasks after embedding, got %#v", tasks)
	}
}

func TestSQLiteStore_MarkEmbeddingStatus_LabelOverflow(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	defer func() { _ = st.Close() }()

	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// a label that cannot be represented as a signed 64-bit integer
	tooBig := uint64(math.MaxInt64) + 1

	err := st.MarkEmbedded(ctx, []uint64{tooBig})
	if err == nil {
		t.Fatalf("expected error for label > MaxInt64, got nil")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("unexpected error message: %v", err)
	}

	err = st.MarkFailed(ctx, []uint64{tooBig}, "reason")
	if err == nil {
		t.Fatalf("expected error for label > MaxInt64 in MarkFailed, got nil")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("unexpected error message for MarkFailed: %v", err)
	}
}

func TestSQLiteStore_UpsertChunkTask_RequiresRelPath(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	defer func() { _ = st.Close() }()

	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	err := st.UpsertChunkTask(ctx, model.NewChunkTask(1, "some text", "text", model.ChunkMetadata{
		ChunkID: 1,
		RelPath: "",
	}))
	if err == nil {
		t.Fatal("expected error for empty RelPath, got nil")
	}

	err = st.UpsertChunkTask(ctx, model.NewChunkTask(2, "some text", "text", model.ChunkMetadata{
		ChunkID: 2,
		RelPath: "   ",
	}))
	if err == nil {
		t.Fatal("expected error for whitespace RelPath, got nil")
	}
}

func TestSQLiteStore_UpsertChunkTask_RequiresNonZeroLabel(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	defer func() { _ = st.Close() }()

	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// zero label should be rejected (requires non-zero)
	err := st.UpsertChunkTask(ctx, model.NewChunkTask(0, "text", "text", model.ChunkMetadata{
		ChunkID: 0,
		RelPath: "foo",
	}))
	if err == nil {
		t.Fatal("expected error for label 0, got nil")
	}
	if !strings.Contains(err.Error(), "non-zero") {
		t.Fatalf("unexpected error message for zero label: %v", err)
	}

	// non-zero (positive) label should succeed
	if err := st.UpsertChunkTask(ctx, model.NewChunkTask(1, "text", "text", model.ChunkMetadata{
		ChunkID: 1,
		RelPath: "foo",
	})); err != nil {
		t.Fatalf("expected success for positive label, got %v", err)
	}
}

func TestSQLiteStore_UpsertChunkTask_LabelMetadataMismatch(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	defer func() { _ = st.Close() }()

	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// construct a task where label and metadata.ChunkID disagree; validation
	// should catch this and return an error instead of panicking or accepting
	// inconsistent data.
	task := model.ChunkTask{
		Label:     1,
		Text:      "mismatch",
		IndexKind: "text",
		Metadata: model.ChunkMetadata{
			ChunkID: 2,
			RelPath: "foo",
		},
	}
	if err := st.UpsertChunkTask(ctx, task); err == nil {
		t.Fatalf("expected error for mismatched label/metadata, got nil")
	}
}

func TestSQLiteStore_UpsertChunkTask_TrimsRelPath(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	defer func() { _ = st.Close() }()

	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	if err := st.UpsertChunkTask(ctx, model.NewChunkTask(1, "trim me", "text", model.ChunkMetadata{
		ChunkID: 1,
		RelPath: "  docs/a.md  ",
		DocType: "md",
		RepType: "raw_text",
	})); err != nil {
		t.Fatalf("UpsertChunkTask failed: %v", err)
	}

	tasks, err := st.NextPending(ctx, 10, "text")
	if err != nil {
		t.Fatalf("NextPending failed: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].Metadata.RelPath != "docs/a.md" {
		t.Fatalf("expected trimmed rel_path, got %q", tasks[0].Metadata.RelPath)
	}
}

func TestSQLiteStore_ClearDocumentContentHashes(t *testing.T) {
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	doc := model.Document{
		RelPath:     "docs/a.md",
		DocType:     "md",
		SizeBytes:   12,
		MTimeUnix:   123,
		ContentHash: "abc123",
		Status:      "ready",
	}
	if err := st.UpsertDocument(ctx, doc); err != nil {
		t.Fatalf("UpsertDocument failed: %v", err)
	}
	got, err := st.GetDocumentByPath(ctx, "docs/a.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath failed: %v", err)
	}
	if got.ContentHash == "" {
		t.Fatalf("expected non-empty content_hash before clear")
	}

	if err := st.ClearDocumentContentHashes(ctx); err != nil {
		t.Fatalf("ClearDocumentContentHashes failed: %v", err)
	}
	got, err = st.GetDocumentByPath(ctx, "docs/a.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath failed after clear: %v", err)
	}
	if got.ContentHash != "" {
		t.Fatalf("expected content_hash cleared, got %q", got.ContentHash)
	}
}

// TestSQLiteStore_ContentHashBackupRestore covers the reindex-rollback snapshot
// (issue #418): a snapshot taken before ClearDocumentContentHashes must restore
// the prior content_hash on rollback, and a discard must make a later restore a
// clean no-op so a committed rebuild's hashes are not overwritten.
func TestSQLiteStore_ContentHashBackupRestore(t *testing.T) {
	const relPath = "docs/a.md"
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	mustNoErr(t, "Init", st.Init(ctx))

	setHash := func(h string) {
		t.Helper()
		mustNoErr(t, "UpsertDocument", st.UpsertDocument(ctx, model.Document{
			RelPath: relPath, DocType: "md", ContentHash: h, Status: "ready",
		}))
	}
	wantHash := func(stage, want string) {
		t.Helper()
		got, err := st.GetDocumentByPath(ctx, relPath)
		mustNoErr(t, "GetDocumentByPath", err)
		if got.ContentHash != want {
			t.Fatalf("%s: content_hash = %q, want %q", stage, got.ContentHash, want)
		}
	}

	const original = "hash-original"
	setHash(original)

	// Restore with no snapshot present leaves the hash alone. Since #807 it
	// also SAYS that there was nothing to restore, instead of returning a
	// silent nil that a caller could not tell from a real restore.
	if err := st.RestoreContentHashes(ctx); !errors.Is(err, store.ErrNoContentHashSnapshot) {
		t.Fatalf("restore with no backup should report absence, got %v", err)
	}
	wantHash("no-op restore", original)

	// Snapshot -> clear -> restore must bring the original hash back.
	mustNoErr(t, "BackupContentHashes", st.BackupContentHashes(ctx))
	mustNoErr(t, "ClearDocumentContentHashes", st.ClearDocumentContentHashes(ctx))
	wantHash("after clear", "")
	mustNoErr(t, "RestoreContentHashes", st.RestoreContentHashes(ctx))
	wantHash("after restore", original)

	// After restore the snapshot is consumed, so a second restore finds nothing
	// and reports it (#807). The hash must be untouched either way.
	if err := st.RestoreContentHashes(ctx); !errors.Is(err, store.ErrNoContentHashSnapshot) {
		t.Fatalf("second restore should report absence, got %v", err)
	}
	wantHash("second restore", original)

	// Snapshot -> discard: a later restore must NOT resurrect the old snapshot,
	// so a committed rebuild's fresh hash survives.
	mustNoErr(t, "BackupContentHashes (2)", st.BackupContentHashes(ctx))
	const rebuilt = "hash-rebuilt"
	setHash(rebuilt)
	mustNoErr(t, "DiscardContentHashBackup", st.DiscardContentHashBackup(ctx))
	// A discarded snapshot is gone, so this reports absence rather than
	// resurrecting the pre-clear hash (#807).
	if err := st.RestoreContentHashes(ctx); !errors.Is(err, store.ErrNoContentHashSnapshot) {
		t.Fatalf("restore after discard should report absence, got %v", err)
	}
	wantHash("after discard+restore", rebuilt)

	// Discard with nothing to drop is idempotent.
	mustNoErr(t, "idempotent discard", st.DiscardContentHashBackup(ctx))
}

// mustNoErr fails the test with a labelled message when err is non-nil.
func mustNoErr(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s failed: %v", what, err)
	}
}

func TestSQLiteStore_EnsureDB_ConcurrentInitClose(t *testing.T) {
	// this test exercises the window addressed by the recent race fix: calling
	// ensureDB and Close simultaneously should not trigger a nil-pointer or
	// other panics.  with the mutex held during initialization there is no
	// race, but run under -race to be sure.
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	// don't defer Close here; we call it explicitly below

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// ignore result; we only care that it doesn't panic or cause race
		if db, err := st.EnsureDB(ctx); err == nil {
			st.ReleaseDB()
			_ = db
		}
	}()
	go func() {
		defer wg.Done()
		_ = st.Close()
	}()
	wg.Wait()

	// store should be safely closed now; further Close is no-op
	if err := st.Close(); err != nil {
		t.Fatalf("unexpected error on second Close: %v", err)
	}
}

// verifyChunkIndexes ensures the sqlite initialization created the indexes we
// added to avoid full table scans. A missing index would mean queries like
// NextPending or path lookups could be slow.
func verifyChunkIndexes(ctx context.Context, st *store.SQLiteStore) error {
	db, err := st.EnsureDB(ctx)
	if err != nil {
		return err
	}
	defer st.ReleaseDB()
	rows, err := db.QueryContext(ctx, "PRAGMA index_list('chunks')")
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	present := make(map[string]bool)
	for rows.Next() {
		var seq int
		var name string
		var unique int
		var origin string
		var partial int
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			return err
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, want := range []string{
		"idx_chunks_embedding_status",
		"idx_chunks_index_kind",
		"idx_chunks_rel_path_deleted",
	} {
		if !present[want] {
			return fmt.Errorf("expected index %s to exist", want)
		}
	}
	return nil
}
func TestSQLiteStore_WithTx_Rollback(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	defer func() { _ = st.Close() }()

	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// perform an operation that will be rolled back
	err := st.WithTx(ctx, func(tx model.RepresentationStore) error {
		_, err := tx.UpsertRepresentation(ctx, model.Representation{DocID: 1, RepType: "raw_text", RepHash: "abc"})
		if err != nil {
			return err
		}
		return fmt.Errorf("force rollback")
	})
	if err == nil {
		t.Fatal("expected error from transactional callback")
	}

	// verify the representation was not persisted
	db, err := st.EnsureDB(ctx)
	if err != nil {
		t.Fatalf("ensureDB: %v", err)
	}
	// release when we're done to ensure activeOps drops to zero
	defer st.ReleaseDB()
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM representations").Scan(&count); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 representations after rollback, got %d", count)
	}
}
func TestSQLiteStoreInitBootstrapsSchemaAndSettings(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})

	keys := []string{
		"schema_version",
		"protocol_version",
		"index_format_version",
		"embed_text_model",
		"embed_code_model",
		"ocr_model",
		"stt_provider",
		"stt_model",
		"chat_model",
	}

	for _, key := range keys {
		value, err := st.GetSetting(ctx, key)
		if err != nil {
			t.Fatalf("GetSetting(%q) failed: %v", key, err)
		}
		if value == "" {
			t.Fatalf("GetSetting(%q) returned empty value", key)
		}
	}
}

func TestSQLiteStoreDocumentCRUDAndListFilters(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})

	seedCRUDDocs(t, ctx, st)
	assertCRUDGetByPath(t, ctx, st)
	assertCRUDListFilters(t, ctx, st)
}

func seedCRUDDocs(t *testing.T, ctx context.Context, st *store.SQLiteStore) {
	t.Helper()
	docs := []model.Document{
		{RelPath: "src/main.go", DocType: "code", SizeBytes: 120, MTimeUnix: 1700000000, ContentHash: "hash-main", Status: "ok"},
		{RelPath: "src/lib/util.go", DocType: "code", SizeBytes: 80, MTimeUnix: 1700000001, ContentHash: "hash-util", Status: "ok"},
		{RelPath: "docs/readme.md", DocType: "md", SizeBytes: 64, MTimeUnix: 1700000002, ContentHash: "hash-readme", Status: "skipped", Deleted: true},
	}
	for _, doc := range docs {
		if err := st.UpsertDocument(ctx, doc); err != nil {
			t.Fatalf("UpsertDocument(%s) failed: %v", doc.RelPath, err)
		}
	}
}

func assertCRUDGetByPath(t *testing.T, ctx context.Context, st *store.SQLiteStore) {
	t.Helper()
	got, err := st.GetDocumentByPath(ctx, "./src/main.go")
	if err != nil {
		t.Fatalf("GetDocumentByPath failed: %v", err)
	}
	if got.RelPath != "src/main.go" {
		t.Fatalf("unexpected RelPath: got=%q want=%q", got.RelPath, "src/main.go")
	}
	if got.DocType != "code" {
		t.Fatalf("unexpected DocType: got=%q want=%q", got.DocType, "code")
	}
}

func assertCRUDListFilters(t *testing.T, ctx context.Context, st *store.SQLiteStore) {
	t.Helper()
	srcDocs, total, err := st.ListFiles(ctx, "src", "", 100, 0)
	if err != nil {
		t.Fatalf("ListFiles(prefix) failed: %v", err)
	}
	if total != 2 {
		t.Fatalf("unexpected prefix total: got=%d want=2", total)
	}
	if len(srcDocs) != 2 {
		t.Fatalf("unexpected prefix result count: got=%d want=2", len(srcDocs))
	}

	mdDocs, mdTotal, err := st.ListFiles(ctx, "", "docs/*.md", 100, 0)
	if err != nil {
		t.Fatalf("ListFiles(glob) failed: %v", err)
	}
	if mdTotal != 0 || len(mdDocs) != 0 {
		t.Fatalf("deleted doc must be excluded: total=%d len=%d", mdTotal, len(mdDocs))
	}

	pageOne, totalWithPaging, err := st.ListFiles(ctx, "", "", 1, 1)
	if err != nil {
		t.Fatalf("ListFiles(paging) failed: %v", err)
	}
	if totalWithPaging != 2 {
		t.Fatalf("unexpected total with paging: got=%d want=2", totalWithPaging)
	}
	if len(pageOne) != 1 {
		t.Fatalf("unexpected page size: got=%d want=1", len(pageOne))
	}
}

func TestSQLiteStoreRejectsAbsoluteAndTraversalRelPaths(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})

	absolutePath := filepath.Join(t.TempDir(), "escape.txt")
	invalidPaths := []string{
		absolutePath,
		"../escape.txt",
		"..",
		"a/../../b.txt",
	}

	for _, relPath := range invalidPaths {
		err := st.UpsertDocument(ctx, model.Document{
			RelPath:     relPath,
			DocType:     "text",
			SizeBytes:   12,
			MTimeUnix:   1700000999,
			ContentHash: "invalid",
			Status:      "ok",
		})
		if err == nil {
			t.Fatalf("expected invalid rel_path to fail: %q", relPath)
		}
	}

	if err := st.UpsertDocument(ctx, model.Document{
		RelPath:     "safe/path.txt",
		DocType:     "text",
		SizeBytes:   12,
		MTimeUnix:   1700001000,
		ContentHash: "valid",
		Status:      "ok",
	}); err != nil {
		t.Fatalf("expected valid rel_path to succeed: %v", err)
	}
}

func TestSQLiteStoreRepresentationChunkSpanAndDeleteCascade(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})

	doc := seedRepCascadeDoc(t, ctx, st)
	repID := seedRepCascadeRep(t, ctx, st, doc.DocID)
	seedRepCascadeChunk(t, ctx, st, repID)
	assertRepCascadeChunks(t, ctx, st, repID)
	assertRepCascadeDelete(t, ctx, st, doc.RelPath, repID)
}

func seedRepCascadeDoc(t *testing.T, ctx context.Context, st *store.SQLiteStore) model.Document {
	t.Helper()
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath:     "notes/todo.txt",
		DocType:     "text",
		SizeBytes:   42,
		MTimeUnix:   1700000100,
		ContentHash: "doc-hash",
		Status:      "ok",
	}); err != nil {
		t.Fatalf("UpsertDocument failed: %v", err)
	}
	doc, err := st.GetDocumentByPath(ctx, "notes/todo.txt")
	if err != nil {
		t.Fatalf("GetDocumentByPath failed: %v", err)
	}
	return doc
}

func seedRepCascadeRep(t *testing.T, ctx context.Context, st *store.SQLiteStore, docID int64) int64 {
	t.Helper()
	repID, err := st.UpsertRepresentation(ctx, model.Representation{
		DocID:       docID,
		RepType:     "raw_text",
		RepHash:     "rep-hash-v1",
		CreatedUnix: 1700000101,
	})
	if err != nil {
		t.Fatalf("UpsertRepresentation failed: %v", err)
	}
	if repID <= 0 {
		t.Fatalf("invalid repID: %d", repID)
	}
	return repID
}

func seedRepCascadeChunk(t *testing.T, ctx context.Context, st *store.SQLiteStore, repID int64) {
	t.Helper()
	chunkID, err := st.InsertChunkWithSpans(ctx, model.Chunk{
		RepID:           repID,
		Ordinal:         0,
		Text:            "hello world",
		TextHash:        "chunk-hash",
		IndexKind:       "text",
		EmbeddingStatus: "pending",
	}, []model.Span{
		{Kind: "lines", StartLine: 1, EndLine: 3},
	})
	if err != nil {
		t.Fatalf("InsertChunkWithSpans failed: %v", err)
	}
	if chunkID <= 0 {
		t.Fatalf("invalid chunkID: %d", chunkID)
	}
}

func assertRepCascadeChunks(t *testing.T, ctx context.Context, st *store.SQLiteStore, repID int64) {
	t.Helper()
	chunks, err := st.GetChunksByRepID(ctx, repID)
	if err != nil {
		t.Fatalf("GetChunksByRepID failed: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("unexpected chunk count: got=%d want=1", len(chunks))
	}
	if chunks[0].Text != "hello world" {
		t.Fatalf("unexpected chunk text: %q", chunks[0].Text)
	}
}

func assertRepCascadeDelete(t *testing.T, ctx context.Context, st *store.SQLiteStore, relPath string, repID int64) {
	t.Helper()
	if err := st.MarkDocumentDeleted(ctx, relPath); err != nil {
		t.Fatalf("MarkDocumentDeleted failed: %v", err)
	}

	deletedDoc, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath after delete failed: %v", err)
	}
	if !deletedDoc.Deleted {
		t.Fatal("expected document tombstone")
	}

	deletedChunks, err := st.GetChunksByRepID(ctx, repID)
	if err != nil {
		t.Fatalf("GetChunksByRepID after delete failed: %v", err)
	}
	if len(deletedChunks) != 1 || !deletedChunks[0].Deleted {
		t.Fatalf("expected chunk tombstone, got=%#v", deletedChunks)
	}
}

func TestSQLiteStore_CorpusStats_Populated(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seedCorpusStatsDocs(t, ctx, st)
	mainRepID, readmeRepID := seedCorpusStatsRepresentations(t, ctx, st)
	seedCorpusStatsChunks(t, ctx, st, mainRepID, readmeRepID)

	stats, err := st.CorpusStats(ctx)
	if err != nil {
		t.Fatalf("CorpusStats failed: %v", err)
	}
	assertCorpusStatsPopulated(t, stats)
}

func seedCorpusStatsDocs(t *testing.T, ctx context.Context, st *store.SQLiteStore) {
	t.Helper()
	docs := []model.Document{
		{RelPath: "src/main.go", DocType: "code", ContentHash: "h1", Status: "ok"},
		{RelPath: "docs/readme.md", DocType: "md", ContentHash: "h2", Status: "skipped"},
		{RelPath: "docs/errors.md", DocType: "md", ContentHash: "h3", Status: "error"},
		{RelPath: "archive/old.txt", DocType: "text", ContentHash: "h4", Status: "ok", Deleted: true},
	}
	for _, doc := range docs {
		d := doc
		if err := st.UpsertDocument(ctx, d); err != nil {
			t.Fatalf("UpsertDocument(%s) failed: %v", d.RelPath, err)
		}
	}
}

func seedCorpusStatsRepresentations(t *testing.T, ctx context.Context, st *store.SQLiteStore) (int64, int64) {
	t.Helper()
	mainDoc, err := st.GetDocumentByPath(ctx, "src/main.go")
	if err != nil {
		t.Fatalf("GetDocumentByPath(main) failed: %v", err)
	}
	readmeDoc, err := st.GetDocumentByPath(ctx, "docs/readme.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath(readme) failed: %v", err)
	}

	mainRepID, err := st.UpsertRepresentation(ctx, model.Representation{DocID: mainDoc.DocID, RepType: "raw_text", RepHash: "rep-main"})
	if err != nil {
		t.Fatalf("UpsertRepresentation(main) failed: %v", err)
	}
	readmeRepID, err := st.UpsertRepresentation(ctx, model.Representation{DocID: readmeDoc.DocID, RepType: "raw_text", RepHash: "rep-readme"})
	if err != nil {
		t.Fatalf("UpsertRepresentation(readme) failed: %v", err)
	}
	return mainRepID, readmeRepID
}

func seedCorpusStatsChunks(t *testing.T, ctx context.Context, st *store.SQLiteStore, mainRepID, readmeRepID int64) {
	t.Helper()
	if _, err := st.InsertChunkWithSpans(ctx, model.Chunk{
		RepID:           mainRepID,
		Ordinal:         0,
		Text:            "main chunk",
		TextHash:        "chunk-main",
		IndexKind:       "code",
		EmbeddingStatus: "ok",
	}, []model.Span{{Kind: "lines", StartLine: 1, EndLine: 2}}); err != nil {
		t.Fatalf("InsertChunkWithSpans(main) failed: %v", err)
	}
	if _, err := st.InsertChunkWithSpans(ctx, model.Chunk{
		RepID:           readmeRepID,
		Ordinal:         0,
		Text:            "readme chunk",
		TextHash:        "chunk-readme",
		IndexKind:       "text",
		EmbeddingStatus: "pending",
	}, []model.Span{{Kind: "lines", StartLine: 1, EndLine: 1}}); err != nil {
		t.Fatalf("InsertChunkWithSpans(readme) failed: %v", err)
	}
}

func assertCorpusStatsPopulated(t *testing.T, stats model.CorpusStats) {
	t.Helper()
	if stats.Scanned != 4 || stats.Indexed != 1 || stats.Skipped != 1 || stats.Deleted != 1 || stats.Errors != 1 {
		t.Fatalf("unexpected lifecycle counts: %+v", stats)
	}
	if stats.TotalDocs != 3 || stats.DocCounts["code"] != 1 || stats.DocCounts["md"] != 2 {
		t.Fatalf("unexpected doc counts: %+v", stats)
	}
	if stats.Representations != 2 || stats.ChunksTotal != 2 || stats.EmbeddedOK != 1 {
		t.Fatalf("unexpected rep/chunk counts: %+v", stats)
	}
}

func TestSQLiteStore_CorpusStats_Empty(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	stats, err := st.CorpusStats(ctx)
	if err != nil {
		t.Fatalf("CorpusStats failed: %v", err)
	}
	if stats.Scanned != 0 || stats.Indexed != 0 || stats.Skipped != 0 || stats.Deleted != 0 || stats.Errors != 0 {
		t.Fatalf("expected zero lifecycle counts, got %+v", stats)
	}
	if stats.TotalDocs != 0 || len(stats.DocCounts) != 0 {
		t.Fatalf("expected empty doc breakdown, got %+v", stats)
	}
	if stats.Representations != 0 || stats.ChunksTotal != 0 || stats.EmbeddedOK != 0 {
		t.Fatalf("expected zero representation/chunk counts, got %+v", stats)
	}
}

func TestSQLiteStoreConcurrentReadWriteWithWAL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), raceScaled(5*time.Second))
	defer cancel()

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})

	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 250; i++ {
			err := st.UpsertDocument(ctx, model.Document{
				RelPath:     fmt.Sprintf("src/file_%03d.go", i),
				DocType:     "code",
				SizeBytes:   int64(200 + i),
				MTimeUnix:   int64(1700000200 + i),
				ContentHash: fmt.Sprintf("hash-%03d", i),
				Status:      "ok",
			})
			if err != nil {
				errCh <- fmt.Errorf("writer failed at %d: %w", i, err)
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 250; i++ {
			_, _, err := st.ListFiles(ctx, "src", "", 50, 0)
			if err != nil {
				errCh <- fmt.Errorf("reader failed at %d: %w", i, err)
				return
			}
		}
	}()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	got, total, err := st.ListFiles(ctx, "src", "", 300, 0)
	if err != nil {
		t.Fatalf("final ListFiles failed: %v", err)
	}
	if total != 250 || len(got) != 250 {
		t.Fatalf("unexpected final listing size total=%d len=%d", total, len(got))
	}
}

func TestSQLiteStore_MCPSessionPersistenceRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	created := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	lastSeen := created.Add(30 * time.Second)
	if err := st.UpsertMCPSession(ctx, "sess_1", created, lastSeen, "test-scope"); err != nil {
		t.Fatalf("UpsertMCPSession failed: %v", err)
	}

	sessions, err := st.ListMCPSessions(ctx)
	if err != nil {
		t.Fatalf("ListMCPSessions failed: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].ID != "sess_1" || !sessions[0].Created.Equal(created) || !sessions[0].LastSeen.Equal(lastSeen) {
		t.Fatalf("unexpected session: %+v", sessions[0])
	}

	if err := st.DeleteMCPSession(ctx, "sess_1"); err != nil {
		t.Fatalf("DeleteMCPSession failed: %v", err)
	}
	sessions, err = st.ListMCPSessions(ctx)
	if err != nil {
		t.Fatalf("ListMCPSessions failed: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected 0 sessions, got %d", len(sessions))
	}
}

func TestSQLiteStore_MCPPaymentOutcomePersistenceRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC().Truncate(time.Second)
	in := store.MCPPaymentOutcomeRecord{
		ExecutionKey:    "sig:params",
		StatusCode:      200,
		ResultJSON:      `{"ok":true}`,
		RPCErrorJSON:    "",
		RequiresSettle:  true,
		Settled:         false,
		PaymentResponse: "",
		UpdatedAt:       now,
	}
	if err := st.UpsertMCPPaymentOutcome(ctx, in); err != nil {
		t.Fatalf("UpsertMCPPaymentOutcome failed: %v", err)
	}

	got, err := st.ListMCPPaymentOutcomes(ctx)
	if err != nil {
		t.Fatalf("ListMCPPaymentOutcomes failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 outcome, got %d", len(got))
	}
	if got[0] != in {
		t.Fatalf("unexpected outcome: got=%+v want=%+v", got[0], in)
	}

	if err := st.DeleteMCPPaymentOutcome(ctx, "sig:params"); err != nil {
		t.Fatalf("DeleteMCPPaymentOutcome failed: %v", err)
	}
	got, err = st.ListMCPPaymentOutcomes(ctx)
	if err != nil {
		t.Fatalf("ListMCPPaymentOutcomes failed: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 outcomes, got %d", len(got))
	}
}
