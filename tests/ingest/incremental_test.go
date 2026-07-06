package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

type fakeIncrementalStore struct {
	existingDoc      model.Document
	existingErr      error
	upsertDocCalls   int
	upsertRepCalls   int
	insertChunkCalls int

	// recorded inputs for verification
	lastUpsertedDoc model.Document
	upsertedDocs    []model.Document

	lastUpsertedRep model.Representation
	upsertedReps    []model.Representation

	lastInsertedChunk model.Chunk
	insertedChunks    []model.Chunk
	insertedSpans     [][]model.Span

	// insertChunkErr, when non-nil, is returned by InsertChunkWithSpans to
	// simulate an ungraceful failure/crash mid-representation-commit (#402).
	insertChunkErr error

	// reflectUpserts makes GetDocumentByPath return the most recently upserted
	// document (instead of existingDoc) so tests can exercise the read-back the
	// #402 content_hash finalize performs.
	reflectUpserts bool

	// reflectByPath makes GetDocumentByPath return the most recently upserted
	// document whose RelPath matches the requested path (falling back to
	// existingDoc when none matches). It is required by the #502 archive tests,
	// where an archive container and its members are distinct rows: the
	// container's deferred finalize must read back the container row (status
	// "skipped"), not whichever member happened to be upserted last. It takes
	// precedence over reflectUpserts when both are set.
	reflectByPath bool
}

func (f *fakeIncrementalStore) Init(context.Context) error { return nil }
func (f *fakeIncrementalStore) Close() error               { return nil }
func (f *fakeIncrementalStore) ListFiles(context.Context, string, string, int, int) ([]model.Document, int64, error) {
	return nil, 0, nil
}
func (f *fakeIncrementalStore) UpsertDocument(_ context.Context, doc model.Document) error {
	f.upsertDocCalls++
	// record for assertions
	f.lastUpsertedDoc = doc
	f.upsertedDocs = append(f.upsertedDocs, doc)
	return nil
}
func (f *fakeIncrementalStore) GetDocumentByPath(_ context.Context, relPath string) (model.Document, error) {
	if f.existingErr != nil {
		return model.Document{}, f.existingErr
	}
	if f.reflectByPath {
		for i := len(f.upsertedDocs) - 1; i >= 0; i-- {
			if f.upsertedDocs[i].RelPath == relPath {
				return f.upsertedDocs[i], nil
			}
		}
		return f.existingDoc, nil
	}
	if f.reflectUpserts && len(f.upsertedDocs) > 0 {
		return f.upsertedDocs[len(f.upsertedDocs)-1], nil
	}
	return f.existingDoc, nil
}
func (f *fakeIncrementalStore) UpsertRepresentation(_ context.Context, rep model.Representation) (int64, error) {
	f.upsertRepCalls++
	// record
	f.lastUpsertedRep = rep
	f.upsertedReps = append(f.upsertedReps, rep)
	return 1, nil
}
func (f *fakeIncrementalStore) InsertChunkWithSpans(_ context.Context, chunk model.Chunk, spans []model.Span) (int64, error) {
	f.insertChunkCalls++
	if f.insertChunkErr != nil {
		return 0, f.insertChunkErr
	}
	// record
	f.lastInsertedChunk = chunk
	f.insertedChunks = append(f.insertedChunks, chunk)
	f.insertedSpans = append(f.insertedSpans, spans)
	return int64(f.insertChunkCalls), nil
}
func (f *fakeIncrementalStore) SoftDeleteChunksFromOrdinal(context.Context, int64, int) error {
	return nil
}

// WithTx is a no-op transaction wrapper required by the
// model.RepresentationStore interface.  All operations happen directly on the
// fake store, so we simply invoke the callback immediately.
func (f *fakeIncrementalStore) WithTx(ctx context.Context, fn func(tx model.RepresentationStore) error) error {
	return fn(f)
}

func TestProcessDocument_IncrementalSkipsUnchangedRepresentation(t *testing.T) {
	root := t.TempDir()
	absPath := filepath.Join(root, "a.txt")
	if err := os.WriteFile(absPath, []byte("same-content"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	hash := ingest.ComputeContentHash([]byte("same-content"))

	st := &fakeIncrementalStore{
		existingDoc: model.Document{
			DocID:       10,
			RelPath:     "a.txt",
			ContentHash: hash,
		},
	}
	svc := mustNewIngestService(t, config.Config{RootDir: root}, st)
	df := ingest.DiscoveredFile{
		AbsPath:   absPath,
		RelPath:   "a.txt",
		SizeBytes: int64(len("same-content")),
	}
	if err := svc.ProcessDocument(context.Background(), df, nil, false); err != nil {
		t.Fatalf("processDocument failed: %v", err)
	}

	if st.upsertDocCalls != 1 {
		t.Fatalf("expected one document upsert, got %d", st.upsertDocCalls)
	}
	if st.upsertRepCalls != 0 {
		t.Fatalf("expected representation generation to be skipped, got %d upserts", st.upsertRepCalls)
	}
}

func TestProcessDocument_ForceReindexRegeneratesRepresentation(t *testing.T) {
	root := t.TempDir()
	absPath := filepath.Join(root, "main.go")
	content := "package main\n\nfunc main(){}\n"
	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	hash := ingest.ComputeContentHash([]byte(content))

	st := &fakeIncrementalStore{
		existingDoc: model.Document{
			DocID:       11,
			RelPath:     "main.go",
			ContentHash: hash,
		},
	}
	svc := mustNewIngestService(t, config.Config{RootDir: root}, st)
	df := ingest.DiscoveredFile{
		AbsPath:   absPath,
		RelPath:   "main.go",
		SizeBytes: int64(len(content)),
	}
	if err := svc.ProcessDocument(context.Background(), df, nil, true); err != nil {
		t.Fatalf("processDocument failed: %v", err)
	}

	if st.upsertRepCalls == 0 {
		t.Fatalf("expected representation to be regenerated in force mode")
	}
	if st.insertChunkCalls == 0 {
		t.Fatalf("expected chunk persistence during regeneration")
	}
}
