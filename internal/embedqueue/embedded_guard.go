package embedqueue

import "sync"

// EmbeddedGuard makes the "count a chunk as embedded_ok" side effect fire at most
// once per (index_kind, chunk_id), even when a job is redelivered and re-embedded
// (SPEC §8.7.3; issue #435 C2).
//
// The distributed path enqueues at-least-once: a lease expiry or a transient Nack
// redelivers an already-succeeded chunk, and the worker re-runs the exact
// in-process embed step. Vector writes are idempotent (keyed by chunk_id), but the
// step's per-upsert OnIndexedChunk hook fires on EVERY successful upsert, so a
// naive AddEmbeddedOK(1) in that hook inflates embedded_ok past chunks_total on
// redelivery. The guard gates the COUNT (not the embed) to the first success.
//
// It is safe for concurrent use by the per-axis (text/code) embed hooks. The
// tracked set is bounded by the number of distinct embedded chunks, matching the
// corpus size — the same working-set the in-process loop already holds in memory.
type EmbeddedGuard struct {
	mu   sync.Mutex
	seen map[embeddedKey]struct{}
}

// embeddedKey identifies a single embedded vector axis: a chunk's stable id scoped
// by its index kind, so the same chunk_id on the "text" and "code" axes is counted
// independently (SPEC §6.1).
type embeddedKey struct {
	kind    string
	chunkID uint64
}

// NewEmbeddedGuard returns a ready EmbeddedGuard.
func NewEmbeddedGuard() *EmbeddedGuard {
	return &EmbeddedGuard{seen: make(map[embeddedKey]struct{})}
}

// First reports whether this is the first successful embed observed for
// (indexKind, chunkID). It returns true exactly once per pair; every later call
// for the same pair returns false. A caller wires it into the embed-success hook
// so the embedded_ok counter is incremented only on that first true, making the
// count idempotent across redelivery/retry (issue #435 C2).
//
// A nil guard treats every call as first (true), so callers that never construct
// one keep the pre-guard behavior instead of panicking.
func (g *EmbeddedGuard) First(indexKind string, chunkID uint64) bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.seen == nil {
		g.seen = make(map[embeddedKey]struct{})
	}
	key := embeddedKey{kind: indexKind, chunkID: chunkID}
	if _, dup := g.seen[key]; dup {
		return false
	}
	g.seen[key] = struct{}{}
	return true
}
