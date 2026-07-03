package index

import (
	"context"

	"github.com/dirstral/dir2mcp/internal/model"
)

// VectorPresence is an optional capability implemented by index backends whose
// durability is NOT per-write — specifically the in-memory HNSW, whose vectors
// only reach disk via the periodic snapshot (issue #402 A2). It reports which of
// the given chunk IDs currently have a vector in the index so a startup
// reconciliation can re-pend embedded chunks whose vectors were lost to an
// ungraceful crash (SIGKILL/OOM/power loss) before the snapshot ran.
//
// Durable backends (disk, qdrant, pgvector) deliberately do NOT implement it:
// their writes are individually durable and they rebuild from a durable store on
// load, so there is nothing to reconcile. ReconcileEmbeddedVectors treats a
// backend that does not implement VectorPresence as a no-op.
type VectorPresence interface {
	HasVectors(ctx context.Context, chunkIDs []uint64) (map[uint64]bool, error)
}

// EmbeddedVectorSource is the metadata-store surface ReconcileEmbeddedVectors
// needs: it enumerates the chunks sqlite records as embedded and re-pends the
// ones whose vector is missing. The shipped *store.SQLiteStore satisfies it.
type EmbeddedVectorSource interface {
	// ListEmbeddedChunkMetadata pages through chunks whose embedding_status is
	// "ok" (embedded) for the given index kind using keyset (seek) pagination:
	// afterChunkID is an exclusive lower bound on the ascending chunk_id key (pass
	// 0 to start); the caller carries the last-seen chunk_id forward for the next
	// page.
	ListEmbeddedChunkMetadata(ctx context.Context, indexKind string, limit int, afterChunkID int64) ([]model.ChunkTask, error)
	// RependEmbeddedChunks resets the given chunks' embedding_status to
	// "pending" so the embed worker re-embeds them.
	RependEmbeddedChunks(ctx context.Context, labels []uint64) error
}

// reconcilePageSize bounds each ListEmbeddedChunkMetadata page so the read scan
// never materializes the whole embedded set at once.
const reconcilePageSize = 500

// ReconcileEmbeddedVectors re-pends chunks that sqlite records as embedded but
// whose vector is absent from ix, and returns the number re-pended (issue #402
// A2). It is a no-op — returning (0, nil) — when ix does not implement
// VectorPresence (durable backends need no reconciliation).
//
// The read scan is kept strictly read-only (missing IDs are accumulated, not
// re-pended, while paging) so keyset pagination walks a stable "ok" set; the
// re-pend write happens once at the end. This avoids the pagination hazard of
// mutating the very rows the pages are drawn from.
//
// Pages are fetched by keyset (seek) rather than OFFSET: each page seeks past the
// last chunk_id seen, so every row is scanned once instead of OFFSET's quadratic
// rescan-and-discard of skipped rows on large embedded sets.
func ReconcileEmbeddedVectors(ctx context.Context, source EmbeddedVectorSource, ix model.Index, kind string) (int, error) {
	presence, ok := ix.(VectorPresence)
	if !ok {
		return 0, nil
	}

	var missing []uint64
	var afterChunkID int64
	for {
		chunks, err := source.ListEmbeddedChunkMetadata(ctx, kind, reconcilePageSize, afterChunkID)
		if err != nil {
			return 0, err
		}
		if len(chunks) == 0 {
			break
		}
		ids := make([]uint64, len(chunks))
		for i, c := range chunks {
			ids[i] = c.Label
		}
		present, err := presence.HasVectors(ctx, ids)
		if err != nil {
			return 0, err
		}
		for _, id := range ids {
			if id != 0 && !present[id] {
				missing = append(missing, id)
			}
		}
		if len(chunks) < reconcilePageSize {
			break
		}
		// Seek past the last chunk_id in this page (rows are ordered by chunk_id
		// ascending, so the final Label is the greatest key seen so far).
		afterChunkID = int64(chunks[len(chunks)-1].Label)
	}

	if len(missing) == 0 {
		return 0, nil
	}
	if err := source.RependEmbeddedChunks(ctx, missing); err != nil {
		return 0, err
	}
	return len(missing), nil
}
