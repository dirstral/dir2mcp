package qdrantindex

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/qdrant/go-client/qdrant"

	"github.com/dirstral/dir2mcp/internal/model"
)

// compile-time assertions that Index satisfies the core model.Index contract
// and the optional FilteringIndex capability (issue #247/#268). Qdrant owns its
// own durability, so Index deliberately does NOT implement model.Persistable.
var (
	_ model.Index          = (*Index)(nil)
	_ model.FilteringIndex = (*Index)(nil)
)

// Client is the subset of the official Qdrant Go client (*qdrant.Client) that
// this package depends on. Narrowing the surface to an interface keeps the
// vector logic unit-testable with an in-memory stub (no network) and documents
// exactly which wire calls the backend issues. NewWithClient accepts any
// implementation so tests can drive the Index against a stub.
type Client interface {
	CollectionExists(ctx context.Context, name string) (bool, error)
	CreateCollection(ctx context.Context, req *qdrant.CreateCollection) error
	DeleteCollection(ctx context.Context, name string) error
	CreateFieldIndex(ctx context.Context, req *qdrant.CreateFieldIndexCollection) (*qdrant.UpdateResult, error)
	Upsert(ctx context.Context, req *qdrant.UpsertPoints) (*qdrant.UpdateResult, error)
	Delete(ctx context.Context, req *qdrant.DeletePoints) (*qdrant.UpdateResult, error)
	Query(ctx context.Context, req *qdrant.QueryPoints) ([]*qdrant.ScoredPoint, error)
	Get(ctx context.Context, req *qdrant.GetPoints) ([]*qdrant.RetrievedPoint, error)
	Close() error
}

// Index is a Qdrant-backed model.Index. A single instance maps to one Qdrant
// collection (one per dir2mcp index kind, e.g. text/code). The collection is
// created lazily on first Upsert because Qdrant requires the vector dimension
// up front, which dir2mcp only learns from the first embedding; Search over a
// collection that does not exist yet returns no hits.
type Index struct {
	client     Client
	collection string

	mu sync.Mutex
	// dim is the vector dimension recorded when the collection was created;
	// 0 means the collection does not exist yet.
	dim int
	// exists is true once the collection has been observed on the server. It
	// gates Search/Delete only: an existing collection answers queries and
	// deletes by id whether or not THIS process has finished its setup.
	exists bool
	// ready is true once every required component of the collection is
	// confirmed: the collection itself, both payload field indexes, and the
	// embed-identity sentinel. Only then may ensureCollection skip its work
	// (issue #675). It is deliberately narrower than exists: a collection left
	// half-built by a failed setup exists but is not ready, so the next call
	// finishes it.
	ready bool
	// identity is the corpus-lifetime embed identity (SPEC 8.1.4) recorded for
	// this collection, mirrored in memory for cheap Identity() reads.
	identity string
}

// Config configures a Qdrant Index.
type Config struct {
	// Collection is the Qdrant collection name backing this index kind.
	Collection string
}

// New constructs a Qdrant-backed Index over an existing *qdrant.Client. The
// caller owns the client's lifetime only insofar as Close on the last Index
// closes it; typically one client is shared and Close is wired per-kind.
func New(client *qdrant.Client, cfg Config) (*Index, error) {
	if client == nil {
		return nil, errors.New("qdrant client is required")
	}
	return NewWithClient(clientAdapter{client}, cfg)
}

// NewWithClient constructs an Index over any Client implementation. The real
// path uses New (which wraps *qdrant.Client); this constructor is the seam tests
// use to drive the Index against an in-memory stub with no network access.
func NewWithClient(client Client, cfg Config) (*Index, error) {
	if client == nil {
		return nil, errors.New("qdrant client is required")
	}
	if cfg.Collection == "" {
		return nil, errors.New("qdrant collection name is required")
	}
	return &Index{client: client, collection: cfg.Collection}, nil
}

// clientAdapter adapts *qdrant.Client to the Client interface. The real
// client already exposes the exact method set, so the adapter is a thin
// pass-through that exists only to keep *qdrant.Client out of the interface.
type clientAdapter struct{ c *qdrant.Client }

func (a clientAdapter) CollectionExists(ctx context.Context, name string) (bool, error) {
	return a.c.CollectionExists(ctx, name)
}

func (a clientAdapter) CreateCollection(ctx context.Context, req *qdrant.CreateCollection) error {
	return a.c.CreateCollection(ctx, req)
}

func (a clientAdapter) DeleteCollection(ctx context.Context, name string) error {
	return a.c.DeleteCollection(ctx, name)
}

func (a clientAdapter) CreateFieldIndex(ctx context.Context, req *qdrant.CreateFieldIndexCollection) (*qdrant.UpdateResult, error) {
	return a.c.CreateFieldIndex(ctx, req)
}

func (a clientAdapter) Upsert(ctx context.Context, req *qdrant.UpsertPoints) (*qdrant.UpdateResult, error) {
	return a.c.Upsert(ctx, req)
}

func (a clientAdapter) Delete(ctx context.Context, req *qdrant.DeletePoints) (*qdrant.UpdateResult, error) {
	return a.c.Delete(ctx, req)
}

func (a clientAdapter) Query(ctx context.Context, req *qdrant.QueryPoints) ([]*qdrant.ScoredPoint, error) {
	return a.c.Query(ctx, req)
}

func (a clientAdapter) Get(ctx context.Context, req *qdrant.GetPoints) ([]*qdrant.RetrievedPoint, error) {
	return a.c.Get(ctx, req)
}

func (a clientAdapter) Close() error { return a.c.Close() }

// Upsert stores (or replaces) the vector and its payload, keyed by
// payload.ChunkID. The collection is created on the first call using the
// vector's dimension. Cosine distance over the (assumed unit-normalized)
// embeddings is used so Search scores match the HNSW backend's semantics.
func (i *Index) Upsert(ctx context.Context, vector []float32, payload model.IndexPayload) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(vector) == 0 {
		return errors.New("vector cannot be empty")
	}
	if payload.ChunkID == 0 {
		return errors.New("payload chunk_id cannot be zero")
	}

	if err := i.ensureCollection(ctx, len(vector)); err != nil {
		return err
	}

	point := &qdrant.PointStruct{
		Id:      qdrant.NewIDNum(payload.ChunkID),
		Vectors: qdrant.NewVectorsDense(vector),
		Payload: payloadToQdrant(payload),
	}
	_, err := i.client.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: i.collection,
		Points:         []*qdrant.PointStruct{point},
		Wait:           qdrant.PtrOf(true),
	})
	if err != nil {
		return fmt.Errorf("qdrant upsert: %w", err)
	}
	return nil
}

// Delete removes the vectors (and payloads) for the given chunk IDs. Unknown
// IDs are ignored by Qdrant. A no-op when the collection does not yet exist.
func (i *Index) Delete(ctx context.Context, chunkIDs []uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(chunkIDs) == 0 {
		return nil
	}
	present, err := i.collectionPresent(ctx)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}

	ids := make([]*qdrant.PointId, 0, len(chunkIDs))
	for _, id := range chunkIDs {
		ids = append(ids, qdrant.NewIDNum(id))
	}
	if _, err := i.client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: i.collection,
		Points:         qdrant.NewPointsSelector(ids...),
		Wait:           qdrant.PtrOf(true),
	}); err != nil {
		return fmt.Errorf("qdrant delete: %w", err)
	}
	return nil
}

// Search returns the k best matches for vector, ordered best-first. Pushable
// predicates (DocTypes, ExcludeOrphans) are translated to a Qdrant filter; the
// path predicates are declined by CanFilter and re-applied by retrieval in Go.
func (i *Index) Search(ctx context.Context, vector []float32, k int, filter model.Filter) ([]model.IndexHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(vector) == 0 {
		return nil, errors.New("query vector cannot be empty")
	}
	if k <= 0 {
		return []model.IndexHit{}, nil
	}
	present, err := i.collectionPresent(ctx)
	if err != nil {
		return nil, err
	}
	if !present {
		return []model.IndexHit{}, nil
	}

	points, err := i.client.Query(ctx, &qdrant.QueryPoints{
		CollectionName: i.collection,
		Query:          qdrant.NewQueryDense(vector),
		Limit:          qdrant.PtrOf(uint64(k)),
		Filter:         toQdrantFilter(filter),
		WithPayload:    qdrant.NewWithPayload(true),
	})
	if err != nil {
		return nil, fmt.Errorf("qdrant query: %w", err)
	}

	hits := make([]model.IndexHit, 0, len(points))
	for _, p := range points {
		id := p.GetId().GetNum()
		if id == identityPointID {
			continue // never surface the reserved identity sentinel
		}
		hits = append(hits, model.IndexHit{
			ChunkID: id,
			Score:   p.GetScore(),
			Payload: qdrantToPayload(id, p.GetPayload()),
		})
	}
	return hits, nil
}

// CanFilter reports whether Qdrant can evaluate the filter itself (issue #247).
// See canFilter for the exact push-down rules.
func (i *Index) CanFilter(filter model.Filter) bool {
	return canFilter(filter)
}

// Identity returns the recorded corpus-lifetime embed identity, or "" when the
// index is fresh. It reads the reserved sentinel point on first call so an
// identity recorded by a previous process is honoured.
func (i *Index) Identity(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.identity != "" {
		return i.identity, nil
	}
	exists, err := i.client.CollectionExists(ctx, i.collection)
	if err != nil {
		return "", fmt.Errorf("qdrant collection exists: %w", err)
	}
	if !exists {
		return "", nil
	}
	// The collection answers queries from here on, but its setup is NOT known to
	// be complete: a previous run can have died between the create and the
	// payload indexes or the sentinel (issue #675). Only ensureCollection, which
	// reconciles every component, may set ready.
	i.exists = true
	stored, err := i.readIdentitySentinel(ctx)
	if err != nil {
		return "", err
	}
	i.identity = stored
	return stored, nil
}

// collectionPresent reports whether the collection exists, which is all Search
// and Delete need: both are well defined over a collection whose payload indexes
// or sentinel are still missing.
//
// The answer comes from the server whenever this process has not seen the
// collection yet, never from memory alone. A collection an earlier run created
// holds vectors, so answering "empty" from a process that has upserted nothing
// would return silently wrong results. A positive answer is cached; a negative
// one is not, because the collection appears as soon as something is embedded.
func (i *Index) collectionPresent(ctx context.Context) (bool, error) {
	i.mu.Lock()
	known := i.exists
	i.mu.Unlock()
	if known {
		return true, nil
	}
	exists, err := i.client.CollectionExists(ctx, i.collection)
	if err != nil {
		return false, fmt.Errorf("qdrant collection exists: %w", err)
	}
	if exists {
		i.mu.Lock()
		i.exists = true
		i.mu.Unlock()
	}
	return exists, nil
}

// readIdentitySentinel returns the embed identity recorded in the reserved
// sentinel point, or "" when the collection carries no sentinel. The caller must
// hold i.mu.
func (i *Index) readIdentitySentinel(ctx context.Context) (string, error) {
	got, err := i.client.Get(ctx, &qdrant.GetPoints{
		CollectionName: i.collection,
		Ids:            []*qdrant.PointId{qdrant.NewIDNum(identityPointID)},
		WithPayload:    qdrant.NewWithPayload(true),
	})
	if err != nil {
		return "", fmt.Errorf("qdrant get identity: %w", err)
	}
	if len(got) == 0 {
		return "", nil
	}
	return got[0].GetPayload()[identityKey].GetStringValue(), nil
}

// Reset clears all vectors/payloads and records identity as the new
// corpus-lifetime embed identity. Because Qdrant needs the vector dimension to
// (re)create the collection — which dir2mcp only learns from the first
// embedding — Reset drops any existing collection and defers (re)creation to
// the next Upsert, recording the identity to write into the sentinel then.
func (i *Index) Reset(ctx context.Context, identity string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	exists, err := i.client.CollectionExists(ctx, i.collection)
	if err != nil {
		return fmt.Errorf("qdrant collection exists: %w", err)
	}
	if exists {
		if err := i.client.DeleteCollection(ctx, i.collection); err != nil {
			return fmt.Errorf("qdrant delete collection: %w", err)
		}
	}
	i.exists = false
	i.ready = false
	i.dim = 0
	i.identity = identity
	return nil
}

// Close releases the underlying client connection.
func (i *Index) Close() error {
	return i.client.Close()
}

// ensureCollection brings the collection to its required shape on first use:
// the collection itself (cosine distance), the keyword payload indexes the
// pushed-down filter relies on, and the embed-identity sentinel.
//
// Every step is idempotent and every step runs on every attempt until they all
// succeed, so a setup that failed part-way is finished by the next call instead
// of being adopted half-built (issue #675). A pre-existing collection is still
// adopted without any destructive recreation: only the missing components are
// added. ready is set last, so it means "all components confirmed" and never
// "the collection is there".
func (i *Index) ensureCollection(ctx context.Context, dim int) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.ready {
		return nil
	}

	exists, err := i.client.CollectionExists(ctx, i.collection)
	if err != nil {
		return fmt.Errorf("qdrant collection exists: %w", err)
	}
	if !exists {
		if err := i.createCollection(ctx, dim); err != nil {
			return err
		}
	}
	i.exists = true
	if err := i.ensureFieldIndexes(ctx); err != nil {
		return err
	}
	if err := i.ensureIdentitySentinel(ctx, dim); err != nil {
		return err
	}
	i.dim = dim
	i.ready = true
	return nil
}

// createCollection creates the collection with cosine distance. The payload
// indexes and the identity sentinel are NOT written here: ensureCollection adds
// them for a newly created and for an adopted collection alike, so one code path
// covers both and neither can be skipped.
func (i *Index) createCollection(ctx context.Context, dim int) error {
	if err := i.client.CreateCollection(ctx, &qdrant.CreateCollection{
		CollectionName: i.collection,
		VectorsConfig: qdrant.NewVectorsConfig(&qdrant.VectorParams{
			Size:     uint64(dim),
			Distance: qdrant.Distance_Cosine,
		}),
	}); err != nil {
		return fmt.Errorf("qdrant create collection: %w", err)
	}
	return nil
}

// ensureFieldIndexes creates the keyword payload indexes for the pushable
// filter fields. Qdrant treats a create over an existing index of the same
// schema as a no-op, so this is safe to re-issue on every attempt and repairs a
// collection whose indexes were lost to a failed setup.
func (i *Index) ensureFieldIndexes(ctx context.Context) error {
	for _, field := range []string{fieldDocTypeLC, fieldRelPath} {
		if _, err := i.client.CreateFieldIndex(ctx, &qdrant.CreateFieldIndexCollection{
			CollectionName: i.collection,
			FieldName:      field,
			FieldType:      qdrant.FieldType_FieldTypeKeyword.Enum(),
		}); err != nil {
			return fmt.Errorf("qdrant create field index %q: %w", field, err)
		}
	}
	return nil
}

// ensureIdentitySentinel makes the stored embed identity (SPEC 8.1.4) agree with
// the one this Index holds, and it never overwrites a stored one. The embed
// identity is the fence that keeps two vector spaces apart, so the three cases
// are kept apart deliberately:
//
//   - nothing stored: write what this Index holds. A collection is only ever
//     written to after this returns, so a collection that holds vectors holds
//     the identity those vectors were built under.
//   - stored, and this Index holds nothing: adopt the stored value. The server
//     is the record; Identity() already reports it.
//   - stored, and it differs from what this Index holds: refuse. Overwriting the
//     sentinel would relabel another run's vectors, and adopting the stored
//     value would mislabel the vectors this process is about to write. Neither
//     is a silent option, so the upsert fails and the operator is told.
//
// The caller must hold i.mu.
func (i *Index) ensureIdentitySentinel(ctx context.Context, dim int) error {
	stored, err := i.readIdentitySentinel(ctx)
	if err != nil {
		return err
	}
	switch {
	case stored == "" && i.identity == "":
		return nil
	case stored == "":
		return i.writeIdentitySentinel(ctx, dim)
	case i.identity == "":
		i.identity = stored
		return nil
	case stored != i.identity:
		return fmt.Errorf("qdrant collection %q records embed identity %q but this process embeds under %q: refusing to mix vector spaces (SPEC 8.1.4); reindex the corpus or point it at a different collection",
			i.collection, stored, i.identity)
	default:
		return nil
	}
}

// writeIdentitySentinel upserts the reserved identity point. The sentinel uses
// a zero vector (it is never returned by Search, which skips identityPointID)
// purely to carry the identity payload alongside the corpus in Qdrant.
func (i *Index) writeIdentitySentinel(ctx context.Context, dim int) error {
	zero := make([]float32, dim)
	_, err := i.client.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: i.collection,
		Points: []*qdrant.PointStruct{{
			Id:      qdrant.NewIDNum(identityPointID),
			Vectors: qdrant.NewVectorsDense(zero),
			Payload: map[string]*qdrant.Value{identityKey: qdrant.NewValueString(i.identity)},
		}},
		Wait: qdrant.PtrOf(true),
	})
	if err != nil {
		return fmt.Errorf("qdrant write identity sentinel: %w", err)
	}
	return nil
}
