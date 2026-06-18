package embedqueue

import (
	"context"
	"fmt"
	"strings"

	"github.com/dirstral/dir2mcp/internal/model"
)

// PendingSource is the read side of the chunk store the coordinator drains: it
// returns chunks whose embedding_status is "pending" (SPEC §5.3). The metadata
// store (internal/store.SQLiteStore) already satisfies it via NextPending, so the
// coordinator reuses the exact pending-selection the in-process loop uses.
type PendingSource interface {
	NextPending(ctx context.Context, limit int, indexKind string) ([]model.ChunkTask, error)
}

// Coordinator enqueues embedding jobs for pending chunks (SPEC §8.7.1). It owns
// no embedding compute — it only translates the store's pending chunks into
// broker jobs bound to the current corpus reference and embed identity (§8.7.2).
// It is opt-in: the in-process loop remains the default and this type is only
// constructed when distributed mode is enabled.
type Coordinator struct {
	Source        PendingSource
	Broker        Broker
	CorpusID      string
	SourceKind    string
	EmbedIdentity string
	// BatchSize bounds how many pending chunks are read per drain pass. A
	// non-positive value defaults to 256.
	BatchSize int
}

// EnqueuePending enqueues the currently-pending chunks of indexKind ("text"/
// "code", or "" for both) into the broker and returns the number of jobs
// submitted. NextPending keeps returning the same pending head until those chunks
// leave the pending state (a worker marks them ok/error out-of-band), and the
// interface has no offset cursor, so one call enqueues the head it observes — NOT
// necessarily the entire backlog. The coordinator loop (runCoordinatorLoop) calls
// this on a ticker, so as the head drains the next pending chunks are picked up;
// the broker dedups by chunk_id+index_kind, so repeated ticks never pile up
// duplicate live jobs (SPEC §8.7.3). Re-running is always safe: an already-
// embedded chunk is no longer pending, and a duplicate job is idempotent at the
// embed layer (vector writes keyed by chunk_id).
func (c *Coordinator) EnqueuePending(ctx context.Context, indexKind string) (int, error) {
	if c.Source == nil || c.Broker == nil {
		return 0, fmt.Errorf("embedqueue: coordinator requires a source and broker")
	}
	if strings.TrimSpace(c.EmbedIdentity) == "" {
		return 0, fmt.Errorf("embedqueue: coordinator requires an embed identity")
	}
	batch := c.BatchSize
	if batch <= 0 {
		batch = 256
	}

	total := 0
	// NextPending returns the same chunks until they leave the pending state, so
	// we track which chunk_ids we have already enqueued this call to avoid a
	// re-read of the same head re-enqueuing them in a tight loop. Embedding marks
	// them ok/error out-of-band; within one call we simply stop once a batch
	// yields nothing new.
	seen := make(map[uint64]struct{})
	for {
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
		tasks, err := c.Source.NextPending(ctx, batch, indexKind)
		if err != nil {
			return total, fmt.Errorf("embedqueue: read pending: %w", err)
		}
		enqueuedThisPass := 0
		for _, t := range tasks {
			id := t.Metadata.ChunkID
			if id == 0 {
				id = t.Label
			}
			if _, dup := seen[id]; dup {
				continue
			}
			job := c.jobFromTask(t)
			if err := c.Broker.Enqueue(ctx, job); err != nil {
				return total, fmt.Errorf("embedqueue: enqueue chunk %d: %w", id, err)
			}
			seen[id] = struct{}{}
			total++
			enqueuedThisPass++
		}
		// Stop when a pass enqueued nothing new: either the store is empty or it
		// keeps returning the same already-enqueued head (status not yet updated).
		if enqueuedThisPass == 0 {
			return total, nil
		}
	}
}

// jobFromTask projects a pending chunk task into a broker Job (SPEC §8.7.2):
// corpus ref + chunk identity + payload identity + the enqueue-time embed
// identity. No bytes are carried — the worker reads them via CorpusFS (§7.10).
func (c *Coordinator) jobFromTask(t model.ChunkTask) Job {
	id := t.Metadata.ChunkID
	if id == 0 {
		id = t.Label
	}
	idxKind := strings.TrimSpace(t.IndexKind)
	if idxKind == "" {
		idxKind = "text"
	}
	span := t.Metadata.Span
	return Job{
		CorpusID:  c.CorpusID,
		Source:    c.SourceKind,
		ChunkID:   id,
		IndexKind: idxKind,
		// TextHash (payload identity, §8.7.2) is left empty here: NextPending does
		// not surface the chunk's text_hash, and it is not load-bearing for
		// correctness — the worker re-reads the AUTHORITATIVE task (text, span,
		// tombstone status) from the shared store by chunk_id (ChunkTaskByID), so
		// a since-changed or tombstoned chunk is handled there.
		Modality: t.Modality,
		RelPath:  t.MediaRef,
		Span: Span{
			Kind:    span.Kind,
			Page:    span.Page,
			StartMS: span.StartMS,
			EndMS:   span.EndMS,
		},
		EmbedIdentity: c.EmbedIdentity,
	}
}
