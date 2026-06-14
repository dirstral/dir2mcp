package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/index/qdrantindex"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TestQdrant_PerKindCollections asserts the index.backend=qdrant dispatch
// derives distinct per-kind collections (issue #268) so the text and code
// vector spaces never collide in a shared Qdrant.
func TestQdrant_PerKindCollections(t *testing.T) {
	const base = "mycorpus"
	textColl := index.QdrantCollectionForKind(base, index.KindText)
	codeColl := index.QdrantCollectionForKind(base, index.KindCode)

	if textColl == codeColl {
		t.Fatalf("text and code collections must differ, both = %q", textColl)
	}
	if !strings.HasPrefix(textColl, base) || !strings.HasSuffix(textColl, index.KindText) {
		t.Errorf("text collection = %q, want %q-prefixed/%q-suffixed", textColl, base, index.KindText)
	}
	if !strings.HasPrefix(codeColl, base) || !strings.HasSuffix(codeColl, index.KindCode) {
		t.Errorf("code collection = %q, want %q-prefixed/%q-suffixed", codeColl, base, index.KindCode)
	}

	// An empty base falls back to the package default but still yields distinct
	// per-kind collections.
	defText := index.QdrantCollectionForKind("", index.KindText)
	defCode := index.QdrantCollectionForKind("", index.KindCode)
	if defText == defCode {
		t.Fatalf("default per-kind collections must differ, both = %q", defText)
	}
	if !strings.HasPrefix(defText, qdrantindex.DefaultCollection) {
		t.Errorf("default text collection = %q, want %q prefix", defText, qdrantindex.DefaultCollection)
	}
}

// TestStaleIndexFiles_QdrantHasNoLocalFiles asserts the qdrant backend names no
// local files for reindex cleanup (its durability is server-side; the collection
// is reset via EnsureIdentity/Reset, not by deleting local snapshots).
func TestStaleIndexFiles_QdrantHasNoLocalFiles(t *testing.T) {
	if names := index.StaleIndexFiles(index.BackendQdrant); len(names) != 0 {
		t.Fatalf("StaleIndexFiles(qdrant) = %v, want none (no local files)", names)
	}
}

// TestQdrant_NotPersistable asserts the qdrant-backed index is deliberately NOT
// model.Persistable, so the dispatch's Load step is skipped and the
// PersistenceManager never tries to write a local snapshot for it.
func TestQdrant_NotPersistable(t *testing.T) {
	var idx model.Index = newStubIndex(t, newStubQdrant())
	if _, ok := idx.(model.Persistable); ok {
		t.Fatal("qdrant index must not implement model.Persistable")
	}
}

// TestQdrant_DispatchEnsureIdentity proves the dispatch's downstream calls work
// against a qdrant index via the stub seam (no network): EnsureIdentity records
// the configured identity on a fresh per-kind collection and Identity reads it
// back, mirroring what loadVectorIndex does after constructing the backend.
func TestQdrant_DispatchEnsureIdentity(t *testing.T) {
	ctx := context.Background()
	s := newStubQdrant()
	coll := index.QdrantCollectionForKind("mycorpus", index.KindText)
	idx, err := qdrantindex.NewWithClient(s, qdrantindex.Config{Collection: coll})
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}

	const identity = "mistral:mistral-embed:1024"
	if err := index.EnsureIdentity(ctx, idx, identity); err != nil {
		t.Fatalf("EnsureIdentity: %v", err)
	}
	// Reset (driven by EnsureIdentity on a fresh index) defers collection
	// creation to the next Upsert; the recorded identity is surfaced after a
	// vector is written.
	if err := idx.Upsert(ctx, []float32{0.1, 0.2, 0.3}, model.IndexPayload{ChunkID: 1, RelPath: "a.md"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := idx.Identity(ctx)
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if got != identity {
		t.Fatalf("Identity = %q, want %q", got, identity)
	}
}
