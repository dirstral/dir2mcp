package tests

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// TestRuneSpans_RoundTripThroughNextPendingAndChunkTaskByID pins SPEC §5.3
// rune_start/rune_end and §8.1.9 (dir2mcp #565): the chunk's rune span and its
// rep_id ride on the ChunkTask both the in-process loop (NextPending) and a
// distributed worker (ChunkTaskByID) lease, identically, so both pool the same
// window from the same document text.
func TestRuneSpans_RoundTripThroughNextPendingAndChunkTaskByID(t *testing.T) {
	ctx := context.Background()
	st, relPath := newContextStore(t)
	insertChunk(t, st, relPath, 0, model.Chunk{Text: "alpha beta", TextHash: "h1", RuneStart: 0, RuneEnd: 10})
	insertChunk(t, st, relPath, 1, model.Chunk{Text: "gamma", TextHash: "h2", RuneStart: 11, RuneEnd: 16})

	tasks, err := st.NextPending(ctx, 10, "text")
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("pending = %d, want 2", len(tasks))
	}
	if tasks[0].RepID <= 0 || tasks[0].RepID != tasks[1].RepID {
		t.Fatalf("both chunks must carry their (shared) rep_id: %d / %d", tasks[0].RepID, tasks[1].RepID)
	}
	if !tasks[0].HasRuneSpan() || tasks[0].RuneStart != 0 || tasks[0].RuneEnd != 10 {
		t.Fatalf("task 0 span = [%d,%d), want [0,10)", tasks[0].RuneStart, tasks[0].RuneEnd)
	}
	if !tasks[1].HasRuneSpan() || tasks[1].RuneStart != 11 || tasks[1].RuneEnd != 16 {
		t.Fatalf("task 1 span = [%d,%d), want [11,16)", tasks[1].RuneStart, tasks[1].RuneEnd)
	}

	byID, _, err := st.ChunkTaskByID(ctx, tasks[1].Metadata.ChunkID)
	if err != nil {
		t.Fatalf("ChunkTaskByID: %v", err)
	}
	if byID.RepID != tasks[1].RepID || byID.RuneStart != 11 || byID.RuneEnd != 16 {
		t.Fatalf("ChunkTaskByID must load the same rep_id and span as NextPending: %+v", byID)
	}
}

// TestRuneSpans_UnknownPersistsAsSentinel pins the -1/-1 "unknown" sentinel: a
// chunk written without a span (the zero value, or a malformed one) reads back
// as unknown, so HasRuneSpan is false and the worker can never mistake it for a
// real window.
func TestRuneSpans_UnknownPersistsAsSentinel(t *testing.T) {
	ctx := context.Background()
	st, relPath := newContextStore(t)
	insertChunk(t, st, relPath, 0, model.Chunk{Text: "no span", TextHash: "h"})
	insertChunk(t, st, relPath, 1, model.Chunk{Text: "inverted", TextHash: "h", RuneStart: 9, RuneEnd: 3})

	tasks, err := st.NextPending(ctx, 10, "text")
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	for _, tk := range tasks {
		if tk.HasRuneSpan() || tk.RuneStart != -1 || tk.RuneEnd != -1 {
			t.Fatalf("chunk %d span = [%d,%d), want the -1/-1 unknown sentinel", tk.Metadata.ChunkID, tk.RuneStart, tk.RuneEnd)
		}
	}
}

// TestRepresentationText_RoundTripAndMissing pins SPEC §5.2
// representation_texts: the document text round-trips by rep_id, a rep without
// one reports ok=false (never an error), an upsert replaces, and the
// transaction handle WithTx supplies implements model.RepresentationTextStore so
// ingest can commit text and spans together.
func TestRepresentationText_RoundTripAndMissing(t *testing.T) {
	ctx := context.Background()
	st, relPath := newContextStore(t)
	insertChunk(t, st, relPath, 0, model.Chunk{Text: "alpha", TextHash: "h", RuneStart: 0, RuneEnd: 5})
	tasks, err := st.NextPending(ctx, 1, "text")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("NextPending: %v (%d tasks)", err, len(tasks))
	}
	repID := tasks[0].RepID

	if _, ok, err := st.RepresentationText(ctx, repID); err != nil || ok {
		t.Fatalf("before any write: ok=%v err=%v, want ok=false, nil", ok, err)
	}
	if _, ok, err := st.RepresentationText(ctx, 0); err != nil || ok {
		t.Fatalf("rep 0: ok=%v err=%v, want ok=false, nil", ok, err)
	}

	err = st.WithTx(ctx, func(tx model.RepresentationStore) error {
		ts, ok := tx.(model.RepresentationTextStore)
		if !ok {
			t.Fatalf("transaction handle %T must implement model.RepresentationTextStore", tx)
		}
		return ts.UpsertRepresentationText(ctx, repID, "alpha beta")
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	if text, ok, err := st.RepresentationText(ctx, repID); err != nil || !ok || text != "alpha beta" {
		t.Fatalf("after write: text=%q ok=%v err=%v", text, ok, err)
	}
	if err := st.UpsertRepresentationText(ctx, repID, "alpha beta gamma"); err != nil {
		t.Fatalf("UpsertRepresentationText: %v", err)
	}
	if text, _, _ := st.RepresentationText(ctx, repID); text != "alpha beta gamma" {
		t.Fatalf("upsert must replace: %q", text)
	}
	if err := st.UpsertRepresentationText(ctx, 0, "x"); err == nil {
		t.Fatal("rep_id 0 must be rejected")
	}
}

// TestRuneSpans_LegacyDatabaseMigratesInPlace pins the additive migration (SPEC
// §5.3, df-003 0.6.0): a database created before the columns existed opens, the
// columns are added with the -1 default, a pre-feature row reads back as
// unknown, and the companion table exists. Nothing is re-embedded.
func TestRuneSpans_LegacyDatabaseMigratesInPlace(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testutil.TempDir(t), "meta.sqlite")

	// A pre-feature schema: the chunks table without rune_start/rune_end and no
	// representation_texts table, with one pending chunk already in it.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	legacy := []string{
		`CREATE TABLE documents (doc_id INTEGER PRIMARY KEY AUTOINCREMENT, rel_path TEXT NOT NULL UNIQUE, doc_type TEXT NOT NULL, size_bytes INTEGER NOT NULL DEFAULT 0, mtime_unix INTEGER NOT NULL DEFAULT 0, content_hash TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'ok', deleted INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE representations (rep_id INTEGER PRIMARY KEY AUTOINCREMENT, doc_id INTEGER NOT NULL, rep_type TEXT NOT NULL, rep_hash TEXT NOT NULL, created_unix INTEGER NOT NULL, deleted INTEGER NOT NULL DEFAULT 0, UNIQUE(doc_id, rep_type))`,
		`CREATE TABLE chunks (chunk_id INTEGER PRIMARY KEY, rep_id INTEGER, ordinal INTEGER NOT NULL DEFAULT 0, rel_path TEXT NOT NULL, doc_type TEXT NOT NULL, rep_type TEXT NOT NULL DEFAULT 'raw_text', text TEXT NOT NULL, text_hash TEXT NOT NULL DEFAULT '', tokens_est INTEGER NOT NULL DEFAULT 0, index_kind TEXT NOT NULL DEFAULT 'text', embedding_status TEXT NOT NULL DEFAULT 'pending', embedding_error TEXT NOT NULL DEFAULT '', deleted INTEGER NOT NULL DEFAULT 0, UNIQUE(rep_id, ordinal))`,
		`INSERT INTO documents(doc_id, rel_path, doc_type) VALUES (1, 'old.md', 'md')`,
		`INSERT INTO representations(rep_id, doc_id, rep_type, rep_hash, created_unix) VALUES (1, 1, 'raw_text', 'h', 1)`,
		`INSERT INTO chunks(chunk_id, rep_id, ordinal, rel_path, doc_type, text) VALUES (42, 1, 0, 'old.md', 'md', 'legacy chunk')`,
	}
	for _, stmt := range legacy {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("legacy schema %q: %v", stmt[:30], err)
		}
	}
	_ = db.Close()

	st := store.NewSQLiteStore(path)
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init on a legacy database must succeed (additive migration): %v", err)
	}
	tasks, err := st.NextPending(ctx, 10, "text")
	if err != nil {
		t.Fatalf("NextPending after migration: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Metadata.ChunkID != 42 {
		t.Fatalf("the legacy chunk must still be pending: %+v", tasks)
	}
	if tasks[0].HasRuneSpan() || tasks[0].RuneStart != -1 || tasks[0].RuneEnd != -1 {
		t.Fatalf("legacy chunk span = [%d,%d), want -1/-1 unknown", tasks[0].RuneStart, tasks[0].RuneEnd)
	}
	if tasks[0].RepID != 1 {
		t.Fatalf("legacy chunk rep_id = %d, want 1", tasks[0].RepID)
	}
	if _, ok, err := st.RepresentationText(ctx, 1); err != nil || ok {
		t.Fatalf("legacy rep must have no text: ok=%v err=%v", ok, err)
	}
	if err := st.UpsertRepresentationText(ctx, 1, "legacy chunk"); err != nil {
		t.Fatalf("the companion table must exist after migration: %v", err)
	}
}

// TestDeleteRepresentationText_RemovesTheStaleText pins the store half of the
// #951 review finding: rep_id is stable per (document, rep_type) across
// rewrites, so a rewrite with late chunking OFF must be able to remove the text
// an earlier late-chunking run persisted, on both the store and the transaction
// handle, and deleting a text that does not exist is not an error.
func TestDeleteRepresentationText_RemovesTheStaleText(t *testing.T) {
	ctx := context.Background()
	st, relPath := newContextStore(t)
	insertChunk(t, st, relPath, 0, model.Chunk{Text: "alpha", TextHash: "h", RuneStart: 0, RuneEnd: 5})
	tasks, err := st.NextPending(ctx, 1, "text")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("NextPending: %v (%d tasks)", err, len(tasks))
	}
	repID := tasks[0].RepID
	if err := st.UpsertRepresentationText(ctx, repID, "alpha beta"); err != nil {
		t.Fatalf("UpsertRepresentationText: %v", err)
	}
	if err := st.DeleteRepresentationText(ctx, repID); err != nil {
		t.Fatalf("DeleteRepresentationText: %v", err)
	}
	if _, ok, err := st.RepresentationText(ctx, repID); err != nil || ok {
		t.Fatalf("after delete: ok=%v err=%v, want gone", ok, err)
	}
	if err := st.DeleteRepresentationText(ctx, repID); err != nil {
		t.Fatalf("deleting a missing text must be a no-op, got %v", err)
	}
	if err := st.DeleteRepresentationText(ctx, 0); err == nil {
		t.Fatal("rep_id 0 must be rejected")
	}
	// The transaction handle has the same capability, so ingest can delete the
	// stale text in the SAME transaction that rewrites the chunks.
	if err := st.UpsertRepresentationText(ctx, repID, "again"); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	err = st.WithTx(ctx, func(tx model.RepresentationStore) error {
		ts, ok := tx.(model.RepresentationTextStore)
		if !ok {
			t.Fatalf("transaction handle %T must implement model.RepresentationTextStore", tx)
		}
		return ts.DeleteRepresentationText(ctx, repID)
	})
	if err != nil {
		t.Fatalf("WithTx delete: %v", err)
	}
	if _, ok, _ := st.RepresentationText(ctx, repID); ok {
		t.Fatal("the transactional delete must remove the text")
	}
}
