package tests

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/qdrant/go-client/qdrant"

	"github.com/dirstral/dir2mcp/internal/index/qdrantindex"
	"github.com/dirstral/dir2mcp/internal/model"
)

// newStubIndexOver builds an Index over any stub client (newStubIndex only takes
// the plain stubQdrant).
func newStubIndexOver(t *testing.T, c qdrantindex.Client) *qdrantindex.Index {
	t.Helper()
	idx, err := qdrantindex.NewWithClient(c, qdrantindex.Config{Collection: "test"})
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	return idx
}

// The wire keys the Qdrant backend uses for the two pushed-down filter fields
// and for the reserved embed-identity sentinel. They are unexported in
// internal/index/qdrantindex, so the tests name them literally: a rename must
// break these tests, because the stored shape is what a live collection holds.
const (
	fieldIdxDocTypeLC = "doc_type_lc"
	fieldIdxRelPath   = "rel_path"
	sentinelKey       = "__embed_identity__"
	sentinelPointID   = uint64(0)
)

// partialInitQdrant wraps stubQdrant with fault injection on the steps that run
// AFTER CreateCollection: the two payload field indexes and the identity
// sentinel write (issue #675). Each fault fires a fixed number of times and then
// the step succeeds, which is exactly the transient-failure-then-retry shape the
// issue describes.
type partialInitQdrant struct {
	*stubQdrant

	// failFieldIndex maps a field name to the number of remaining failures.
	failFieldIndex map[string]int
	// failSentinel is the number of remaining failures for the sentinel write.
	failSentinel int

	fieldIndexOK   map[string]int
	sentinelWrites int
}

func newPartialInitQdrant() *partialInitQdrant {
	return &partialInitQdrant{
		stubQdrant:     newStubQdrant(),
		failFieldIndex: map[string]int{},
		fieldIndexOK:   map[string]int{},
	}
}

func (p *partialInitQdrant) CreateFieldIndex(ctx context.Context, req *qdrant.CreateFieldIndexCollection) (*qdrant.UpdateResult, error) {
	field := req.GetFieldName()
	if p.failFieldIndex[field] > 0 {
		p.failFieldIndex[field]--
		return nil, errors.New("injected: create field index failed")
	}
	p.fieldIndexOK[field]++
	return p.stubQdrant.CreateFieldIndex(ctx, req)
}

func (p *partialInitQdrant) Upsert(ctx context.Context, req *qdrant.UpsertPoints) (*qdrant.UpdateResult, error) {
	if isSentinelUpsert(req) {
		if p.failSentinel > 0 {
			p.failSentinel--
			return nil, errors.New("injected: sentinel write failed")
		}
		p.sentinelWrites++
	}
	return p.stubQdrant.Upsert(ctx, req)
}

// isSentinelUpsert reports whether req writes the reserved identity point.
func isSentinelUpsert(req *qdrant.UpsertPoints) bool {
	for _, pt := range req.GetPoints() {
		if pt.GetId().GetNum() == sentinelPointID {
			return true
		}
	}
	return false
}

// storedIdentity returns the identity the stub collection holds, or "".
func storedIdentity(s *stubQdrant) string {
	return s.points[sentinelPointID][sentinelKey].GetStringValue()
}

// seedIdentity records an identity sentinel directly in the stub collection, as
// a previous process would have left it.
func seedIdentity(s *stubQdrant, identity string) {
	s.collExists = true
	s.points[sentinelPointID] = map[string]*qdrant.Value{
		sentinelKey: qdrant.NewValueString(identity),
	}
}

// TestQdrant_RetryAfterFieldIndexFailureFinishesSetup drives the #675 defect:
// CreateCollection succeeds, the FIRST payload field index fails once, and the
// retry must finish every remaining setup step instead of adopting the
// half-built collection.
func TestQdrant_RetryAfterFieldIndexFailureFinishesSetup(t *testing.T) {
	ctx := context.Background()
	s := newPartialInitQdrant()
	s.failFieldIndex[fieldIdxDocTypeLC] = 1
	idx := newStubIndexOver(t, s)

	// EnsureIdentity on a fresh index does exactly this: it records the
	// configured identity, and the collection is created on the next Upsert.
	if err := idx.Reset(ctx, "ident-A"); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	vec := []float32{1, 0, 0}
	if err := idx.Upsert(ctx, vec, model.IndexPayload{ChunkID: 1, RelPath: "a.md"}); err == nil {
		t.Fatal("first Upsert must fail: the field index creation was made to fail")
	}
	if !s.created {
		t.Fatal("the collection should have been created before the failing step")
	}

	// The retry must complete the setup: both payload indexes and the sentinel.
	if err := idx.Upsert(ctx, vec, model.IndexPayload{ChunkID: 1, RelPath: "a.md"}); err != nil {
		t.Fatalf("retry Upsert: %v", err)
	}
	if s.fieldIndexOK[fieldIdxDocTypeLC] == 0 {
		t.Errorf("payload index %q was never created: the retry adopted a collection with missing indexes", fieldIdxDocTypeLC)
	}
	if s.fieldIndexOK[fieldIdxRelPath] == 0 {
		t.Errorf("payload index %q was never created: the retry adopted a collection with missing indexes", fieldIdxRelPath)
	}
	if got := storedIdentity(s.stubQdrant); got != "ident-A" {
		t.Errorf("stored embed identity = %q, want %q: the retry adopted a collection with no identity sentinel", got, "ident-A")
	}
	// The collection is adopted, never recreated destructively.
	if s.deletedCol {
		t.Error("the retry must not delete the collection")
	}
}

// TestQdrant_RetryAfterSentinelFailureRecordsIdentity drives the dangerous half
// of #675: the sentinel write is the last setup step, so a failure there leaves a
// collection that takes vectors while recording no embed identity. A later
// process reads no identity, treats the populated collection as fresh, and
// Reset wipes every vector while sqlite still records those chunks as embedded.
func TestQdrant_RetryAfterSentinelFailureRecordsIdentity(t *testing.T) {
	ctx := context.Background()
	s := newPartialInitQdrant()
	s.failSentinel = 1
	idx := newStubIndexOver(t, s)

	if err := idx.Reset(ctx, "ident-A"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	vec := []float32{1, 0, 0}
	if err := idx.Upsert(ctx, vec, model.IndexPayload{ChunkID: 1, RelPath: "a.md"}); err == nil {
		t.Fatal("first Upsert must fail: the sentinel write was made to fail")
	}
	if err := idx.Upsert(ctx, vec, model.IndexPayload{ChunkID: 1, RelPath: "a.md"}); err != nil {
		t.Fatalf("retry Upsert: %v", err)
	}
	if s.sentinelWrites != 1 {
		t.Errorf("sentinel writes = %d, want 1: the retry must write the identity the failed attempt lost", s.sentinelWrites)
	}

	// A NEW Index over the same collection is what the next process sees. It must
	// read the identity from the collection, not from this process's memory.
	next := newStubIndexOver(t, s)
	got, err := next.Identity(ctx)
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if got != "ident-A" {
		t.Fatalf("next process reads identity %q, want %q: a populated collection with no identity is reset and loses every vector", got, "ident-A")
	}
}

// TestQdrant_AdoptedCollectionKeepsStoredIdentity guards the other direction:
// reconciling an existing, fully initialized collection must not rewrite its
// recorded identity or drop its points.
func TestQdrant_AdoptedCollectionKeepsStoredIdentity(t *testing.T) {
	ctx := context.Background()
	s := newPartialInitQdrant()
	seedIdentity(s.stubQdrant, "ident-A")
	s.points[7] = map[string]*qdrant.Value{"chunk_id": qdrant.NewValueInt(7)}
	idx := newStubIndexOver(t, s)

	if err := idx.Upsert(ctx, []float32{1, 0, 0}, model.IndexPayload{ChunkID: 8, RelPath: "b.md"}); err != nil {
		t.Fatalf("Upsert over an adopted collection: %v", err)
	}
	if s.created || s.deletedCol {
		t.Error("an existing collection must be adopted without recreation")
	}
	if got := storedIdentity(s.stubQdrant); got != "ident-A" {
		t.Errorf("stored identity = %q, want it left at %q", got, "ident-A")
	}
	if s.sentinelWrites != 0 {
		t.Errorf("sentinel writes = %d, want 0: a recorded identity must never be overwritten", s.sentinelWrites)
	}
	if _, ok := s.points[7]; !ok {
		t.Error("the pre-existing point was lost")
	}
}

// TestQdrant_MismatchedStoredIdentityIsRefused proves the fence holds: a stored
// identity that differs from the one this process embeds under is neither
// adopted nor overwritten. The state is what two processes over one collection
// leave: this process reset the collection under ident-B, and another process
// recreated it under ident-A.
func TestQdrant_MismatchedStoredIdentityIsRefused(t *testing.T) {
	ctx := context.Background()
	s := newPartialInitQdrant()
	idx := newStubIndexOver(t, s)

	if err := idx.Reset(ctx, "ident-B"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	// Another process recreated the collection under a different identity.
	seedIdentity(s.stubQdrant, "ident-A")

	err := idx.Upsert(ctx, []float32{1, 0, 0}, model.IndexPayload{ChunkID: 9, RelPath: "c.md"})
	if err == nil {
		t.Fatal("Upsert must refuse a collection recorded under a different embed identity")
	}
	if !strings.Contains(err.Error(), "ident-A") || !strings.Contains(err.Error(), "ident-B") {
		t.Errorf("the error must name both identities, got: %v", err)
	}
	if got := storedIdentity(s.stubQdrant); got != "ident-A" {
		t.Errorf("stored identity = %q, want it left at %q", got, "ident-A")
	}
	if _, ok := s.points[9]; ok {
		t.Error("no vector may be written into a collection of another vector space")
	}
}

// TestQdrant_SearchAsksTheServerBeforeReportingAnEmptyIndex covers the same
// question as #666 on the qdrant side: readiness held in memory must not decide
// that a collection is empty. A process that has upserted nothing and has not
// read the identity yet must ask the server, because a collection an earlier run
// created holds vectors.
func TestQdrant_SearchAsksTheServerBeforeReportingAnEmptyIndex(t *testing.T) {
	ctx := context.Background()
	s := newPartialInitQdrant()
	seedIdentity(s.stubQdrant, "ident-A")
	s.points[7] = map[string]*qdrant.Value{"chunk_id": qdrant.NewValueInt(7)}
	idx := newStubIndexOver(t, s)

	hits, err := idx.Search(ctx, []float32{1, 0, 0}, 5, model.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ChunkID != 7 {
		t.Fatalf("hits = %+v, want the one stored point: an existing collection is not an empty index", hits)
	}
	if err := idx.Delete(ctx, []uint64{7}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.lastDelete == nil {
		t.Fatal("Delete must reach the server for an existing collection")
	}
}

// TestQdrant_SearchOverAdoptedCollectionBeforeSetup guards the split between
// "the collection exists" and "the setup is complete". Search and Delete need
// only the former, so a restart that has not upserted anything yet must still
// query the server rather than answer an empty result from memory.
func TestQdrant_SearchOverAdoptedCollectionBeforeSetup(t *testing.T) {
	ctx := context.Background()
	s := newPartialInitQdrant()
	seedIdentity(s.stubQdrant, "ident-A")
	s.points[7] = map[string]*qdrant.Value{"chunk_id": qdrant.NewValueInt(7)}
	idx := newStubIndexOver(t, s)

	if _, err := idx.Identity(ctx); err != nil {
		t.Fatalf("Identity: %v", err)
	}
	hits, err := idx.Search(ctx, []float32{1, 0, 0}, 5, model.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if s.lastQuery == nil {
		t.Fatal("Search must reach the server for an existing collection")
	}
	if len(hits) != 1 || hits[0].ChunkID != 7 {
		t.Fatalf("hits = %+v, want the one stored point", hits)
	}
	if err := idx.Delete(ctx, []uint64{7}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.lastDelete == nil {
		t.Fatal("Delete must reach the server for an existing collection")
	}
}
