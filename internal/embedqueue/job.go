// Package embedqueue implements the optional distributed-embedding job queue
// (SPEC §8.7): a coordinator enqueues embedding jobs for pending chunks and one
// or more stateless embed-workers lease those jobs, read the referenced corpus
// bytes directly via CorpusFS (§7.10), embed via the configured provider, and
// write the resulting vectors + chunk status back to a shared store.
//
// The package is additive and off by default. The single-binary default keeps
// running the in-process embedding loop (internal/index.EmbeddingWorker) with no
// broker; the distributed mode is the degenerate-case generalization where the
// transport is an external broker and embedding compute lives on separate hosts.
//
// The broker transport is implementation-defined (SPEC §8.7.4): this package
// ships the Broker interface plus a default, dependency-free implementation
// (in-process and SQLite-backed, both pure-Go) sufficient for the single-node
// degenerate case and for tests. External-broker adapters (NATS/Redis/SQS) plug
// in behind the Broker interface without touching the coordinator or worker.
package embedqueue

import (
	"errors"
	"strings"
)

// Job is one unit of embedding work (SPEC §8.7.2). It identifies its work
// precisely enough that any worker can execute it WITHOUT coordinator-relayed
// payload bytes: the worker reads the referenced corpus bytes directly via
// CorpusFS (§7.10).
//
// A Job carries no secret material (no broker credentials, no presigned URLs, no
// media bytes) so it is safe to serialize onto a broker and to log by ID.
type Job struct {
	// --- corpus reference (which corpus + how to read its bytes) ---

	// CorpusID is the stable corpus identity (SPEC §5.5) the chunk belongs to.
	// It scopes the shared Tier-C collection/namespace and lets a worker reject
	// a job for a corpus it is not configured to serve.
	CorpusID string
	// Source is the corpus source binding (SPEC §7.8) — "local", "nfs", or "s3"
	// — that tells the worker which CorpusFS backend to read bytes through.
	Source string

	// --- chunk identity (which vector axis to write) ---

	// ChunkID is the chunk's stable id and ANN label (SPEC §5.3). Vector writes
	// are keyed by it, which is what makes redelivery idempotent (§8.7.3).
	ChunkID uint64
	// IndexKind routes the write to the correct axis: "text" or "code" (§6.1).
	IndexKind string

	// --- payload identity (which exact bytes to embed) ---

	// TextHash is the chunk's content hash (SPEC §5.3) for a text chunk. It lets
	// a worker detect a stale job whose chunk text changed since enqueue.
	TextHash string
	// Modality is "" / "text" for a text chunk, or "image"/"audio"/"video"/"pdf"
	// for a media chunk (SPEC §8.1.7).
	Modality string
	// RelPath is the corpus rel_path of the source media for a media chunk; the
	// worker reads those bytes via CorpusFS. Empty for text chunks.
	RelPath string
	// Span is the media chunk's window (SPEC §5.4): the page for a PDF chunk or
	// the time window for an audio/video chunk, so the worker fetches and windows
	// the exact bytes via CorpusFS range reads. Zero for text chunks.
	Span Span

	// --- embed identity the job was enqueued under (SPEC §8.1.4) ---

	// EmbedIdentity is the corpus-lifetime embed identity
	// (provider|text_model|code_model|text_dim|code_dim|multimodal) the
	// coordinator enqueued the job under. A worker whose configured embed
	// provider does not match this MUST reject the job rather than write a vector
	// from the wrong vector space (SPEC §8.7.3, §6.4).
	EmbedIdentity string
}

// Span is the media-chunk window carried on a Job (SPEC §5.4). It mirrors the
// scalar fields of model.Span needed to re-fetch and window media bytes, kept
// local so the broker payload has no dependency on the model package and stays a
// flat, serialization-friendly shape.
type Span struct {
	// Kind is "page" for a PDF page chunk or "time" for an audio/video window.
	Kind string
	// Page is the 1-based page number for a "page" span.
	Page int
	// StartMS/EndMS bound a "time" span window in milliseconds.
	StartMS int
	EndMS   int
}

// Validate checks that a job carries the minimum identity needed to execute it.
func (j Job) Validate() error {
	if j.ChunkID == 0 {
		return errors.New("embedqueue: job has zero chunk_id")
	}
	if strings.TrimSpace(j.EmbedIdentity) == "" {
		return errors.New("embedqueue: job has empty embed identity")
	}
	return nil
}

// IsMedia reports whether the job embeds non-text media (SPEC §8.1.7).
func (j Job) IsMedia() bool {
	switch strings.ToLower(strings.TrimSpace(j.Modality)) {
	case "image", "audio", "video", "pdf":
		return true
	default:
		return false
	}
}
