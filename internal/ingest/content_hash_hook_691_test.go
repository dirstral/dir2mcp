package ingest

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// contentHashStubStore is the minimal model.Store needed to drive the document
// content-hash notification (#691). current is the row GetDocumentByPath
// returns; getErr overrides it.
type contentHashStubStore struct {
	mu        sync.Mutex
	current   model.Document
	getErr    error
	upsertErr error
	upserted  []model.Document
}

func (s *contentHashStubStore) Init(context.Context) error { return nil }

func (s *contentHashStubStore) UpsertDocument(_ context.Context, doc model.Document) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upsertErr != nil {
		return s.upsertErr
	}
	s.upserted = append(s.upserted, doc)
	s.current = doc
	return nil
}

func (s *contentHashStubStore) GetDocumentByPath(context.Context, string) (model.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return model.Document{}, s.getErr
	}
	return s.current, nil
}

func (s *contentHashStubStore) ListFiles(context.Context, string, string, int, int) ([]model.Document, int64, error) {
	return nil, 0, nil
}

func (s *contentHashStubStore) Close() error { return nil }

// contentHashRecorder collects every (rel_path, content_hash) pair the hook
// reports, in order.
type contentHashRecorder struct {
	paths  []string
	hashes []string
}

func (r *contentHashRecorder) record(relPath, contentHash string) {
	r.paths = append(r.paths, relPath)
	r.hashes = append(r.hashes, contentHash)
}

func newContentHashService(store model.Store) *Service {
	return &Service{store: store, logger: log.New(io.Discard, "", 0)}
}

// TestFinalizeContentHash_NotifiesAfterDurableWrite pins the #691 publication
// point: the group key becomes visible to retrieval only after the store write
// that stamps the #402 done marker returns without error.
func TestFinalizeContentHash_NotifiesAfterDurableWrite(t *testing.T) {
	store := &contentHashStubStore{current: model.Document{RelPath: "a.md", Status: "ok"}}
	svc := newContentHashService(store)
	rec := &contentHashRecorder{}
	svc.SetOnDocumentContentHash(rec.record)

	doc := model.Document{RelPath: "a.md", Status: "ok"}
	if err := svc.finalizeContentHash(context.Background(), &doc, "H1"); err != nil {
		t.Fatalf("finalizeContentHash: %v", err)
	}

	if len(rec.paths) != 1 || rec.paths[0] != "a.md" || rec.hashes[0] != "H1" {
		t.Fatalf("want one report of (a.md, H1), got paths=%v hashes=%v", rec.paths, rec.hashes)
	}
}

// TestFinalizeContentHash_NotifiesWhenRowRecreated covers the not-found branch:
// the row is recreated with the withheld hash stamped on, so the group key is
// durable and must be reported too.
func TestFinalizeContentHash_NotifiesWhenRowRecreated(t *testing.T) {
	store := &contentHashStubStore{getErr: model.ErrNotFound}
	svc := newContentHashService(store)
	rec := &contentHashRecorder{}
	svc.SetOnDocumentContentHash(rec.record)

	doc := model.Document{RelPath: "a.md", Status: "ok"}
	if err := svc.finalizeContentHash(context.Background(), &doc, "H1"); err != nil {
		t.Fatalf("finalizeContentHash: %v", err)
	}

	if len(rec.paths) != 1 || rec.hashes[0] != "H1" {
		t.Fatalf("want one report of H1, got paths=%v hashes=%v", rec.paths, rec.hashes)
	}
}

// TestFinalizeContentHash_NoNotifyWhenStatusGuardBlocks pins the #413 guard: a
// document that failed out of band keeps an empty content_hash in the store, so
// no group key may be published for it.
func TestFinalizeContentHash_NoNotifyWhenStatusGuardBlocks(t *testing.T) {
	store := &contentHashStubStore{current: model.Document{RelPath: "a.md", Status: "error"}}
	svc := newContentHashService(store)
	rec := &contentHashRecorder{}
	svc.SetOnDocumentContentHash(rec.record)

	doc := model.Document{RelPath: "a.md", Status: "ok"}
	if err := svc.finalizeContentHash(context.Background(), &doc, "H1"); err != nil {
		t.Fatalf("finalizeContentHash: %v", err)
	}

	if len(rec.paths) != 0 {
		t.Fatalf("no group key may be published, got paths=%v hashes=%v", rec.paths, rec.hashes)
	}
}

// TestFinalizeContentHash_NoNotifyWhenUpsertFails pins the same rule for a
// failed write: an unwritten hash must never reach retrieval.
func TestFinalizeContentHash_NoNotifyWhenUpsertFails(t *testing.T) {
	store := &contentHashStubStore{
		current:   model.Document{RelPath: "a.md", Status: "ok"},
		upsertErr: errors.New("disk full"),
	}
	svc := newContentHashService(store)
	rec := &contentHashRecorder{}
	svc.SetOnDocumentContentHash(rec.record)

	doc := model.Document{RelPath: "a.md", Status: "ok"}
	if err := svc.finalizeContentHash(context.Background(), &doc, "H1"); err == nil {
		t.Fatal("a failed upsert must return an error")
	}

	if len(rec.paths) != 0 {
		t.Fatalf("no group key may be published, got paths=%v hashes=%v", rec.paths, rec.hashes)
	}
}

// TestPersistNonFatalDocError_NotifiesEmptyContentHash pins the fail-safe
// direction: a document recorded as an error carries no usable content_hash, so
// the report is empty and retrieval forgets the path instead of grouping it on
// stale content.
func TestPersistNonFatalDocError_NotifiesEmptyContentHash(t *testing.T) {
	store := &contentHashStubStore{}
	svc := newContentHashService(store)
	rec := &contentHashRecorder{}
	svc.SetOnDocumentContentHash(rec.record)

	doc := model.Document{RelPath: "a.md", DocType: "pdf"}
	svc.persistNonFatalDocError(context.Background(), doc, errors.New("provider outage"), nil)

	if len(rec.paths) != 1 || rec.paths[0] != "a.md" || rec.hashes[0] != "" {
		t.Fatalf("want one report of (a.md, empty), got paths=%v hashes=%v", rec.paths, rec.hashes)
	}
}

// TestNotifyDocumentContentHash_ContainsPanic keeps a misbehaving consumer from
// killing ingest, matching the other document hooks.
func TestNotifyDocumentContentHash_ContainsPanic(t *testing.T) {
	svc := newContentHashService(&contentHashStubStore{})
	svc.SetOnDocumentContentHash(func(string, string) { panic("consumer blew up") })

	svc.notifyDocumentContentHash("a.md", "H1")
}

// TestSetOnDocumentContentHash_NilClears pins that clearing the callback stops
// the reports.
func TestSetOnDocumentContentHash_NilClears(t *testing.T) {
	svc := newContentHashService(&contentHashStubStore{})
	svc.SetOnDocumentContentHash(func(string, string) { t.Fatal("cleared callback was invoked") })
	svc.SetOnDocumentContentHash(nil)

	svc.notifyDocumentContentHash("a.md", "H1")
}
