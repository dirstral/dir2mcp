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

// reconcileRependBatch bounds how many confirmed-missing chunk IDs are buffered
// before they are flushed to RependEmbeddedChunks. Without it a corpus whose
// snapshot lost most/all vectors would buffer O(total embedded chunks) IDs before
// the single end-of-scan re-pend (issue #503). Peak buffer is bounded to
// reconcileRependBatch + reconcilePageSize (one whole page is appended before the
// threshold is re-checked), i.e. a few thousand uint64s regardless of corpus size.
const reconcileRependBatch = 1000

// ReconcileEmbeddedVectors re-pends chunks that sqlite records as embedded but
// whose vector is absent from ix, and returns the number re-pended (issue #402
// A2). It is a no-op — returning (0, nil) — when ix does not implement
// VectorPresence (durable backends need no reconciliation).
//
// Missing IDs are re-pended in bounded batches as the scan proceeds (issue #503)
// rather than buffered whole and flushed once at the end, which caps peak memory
// at reconcileRependBatch + reconcilePageSize IDs on very large corpora.
//
// Stable-set correctness (unchanged): reconciliation runs at startup before the
// embed worker starts, so no concurrent embed can be finishing a vector mid-scan;
// the only invariant to preserve is that a re-pend must not disturb the keyset
// walk of the "ok" set. Batching is safe because a flush only ever re-pends IDs
// confirmed missing by HasVectors during the read of an *already-completed* page,
// so every flushed ID has chunk_id <= afterChunkID (the seek cursor for the next
// fetch). Flipping those rows ok->pending therefore cannot alter any future page:
// subsequent fetches select only chunk_id > afterChunkID, which excludes every
// re-pended row. The scan still walks a stable "ok" set exactly as the prior
// single end-of-scan re-pend did.
//
// Pages are fetched by keyset (seek) rather than OFFSET: each page seeks past the
// last chunk_id seen, so every row is scanned once instead of OFFSET's quadratic
// rescan-and-discard of skipped rows on large embedded sets.
func ReconcileEmbeddedVectors(ctx context.Context, source EmbeddedVectorSource, ix model.Index, kind string) (int, error) {
	presence, ok := ix.(VectorPresence)
	if !ok {
		return 0, nil
	}

	var (
		missing      []uint64
		repended     int
		afterChunkID int64
	)
	// flush re-pends the buffered missing IDs and resets the buffer. afterChunkID
	// is advanced to a page's last chunk_id before that page's IDs can be flushed,
	// so every buffered ID has chunk_id <= afterChunkID and the write cannot perturb
	// the keyset walk of the remaining "ok" set.
	flush := func() error {
		if len(missing) == 0 {
			return nil
		}
		if err := source.RependEmbeddedChunks(ctx, missing); err != nil {
			return err
		}
		repended += len(missing)
		missing = missing[:0]
		return nil
	}

	for {
		chunks, err := source.ListEmbeddedChunkMetadata(ctx, kind, reconcilePageSize, afterChunkID)
		if err != nil {
			return repended, err
		}
		if len(chunks) == 0 {
			break
		}
		pageMiss, err := pageMissing(ctx, presence, chunks)
		if err != nil {
			return repended, err
		}
		missing = append(missing, pageMiss...)
		// Advance the cursor to the largest chunk_id read on THIS page BEFORE any
		// flush (rows are ordered by chunk_id ascending, so the final Label is the
		// greatest key seen so far). Advancing on every page — including the final
		// short one — keeps every buffered ID at chunk_id <= afterChunkID, so the
		// end-of-scan flush's invariant holds literally for the last page too.
		afterChunkID = int64(chunks[len(chunks)-1].Label)
		if len(chunks) < reconcilePageSize {
			break
		}
		if len(missing) >= reconcileRependBatch {
			if err := flush(); err != nil {
				return repended, err
			}
		}
	}

	if err := flush(); err != nil {
		return repended, err
	}
	return repended, nil
}

// pageMissing returns the chunk IDs in a single embedded page whose vectors are
// absent from the index. Zero labels are skipped defensively. The caller passes a
// non-empty page (the scan loop breaks on an empty page before calling this).
func pageMissing(ctx context.Context, presence VectorPresence, chunks []model.ChunkTask) ([]uint64, error) {
	ids := make([]uint64, len(chunks))
	for i, c := range chunks {
		ids[i] = c.Label
	}
	present, err := presence.HasVectors(ctx, ids)
	if err != nil {
		return nil, err
	}
	missing := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if id != 0 && !present[id] {
			missing = append(missing, id)
		}
	}
	return missing, nil
}
