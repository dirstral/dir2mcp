package model

import (
	"strings"

	"github.com/dirstral/dir2mcp/internal/lifecycle"
)

// DocumentStatus is the value of documents.status. Document.Status stays a
// plain string so the struct, its JSON and every caller keep their shape; these
// constants name the values the store keeps.
type DocumentStatus string

const (
	// DocumentStatusOK: the document was read and its representations are (or
	// will be) searchable.
	DocumentStatusOK DocumentStatus = "ok"
	// DocumentStatusSkipped: the document was recorded but never indexed. It
	// carries a SkipReason* in skip_reason.
	DocumentStatusSkipped DocumentStatus = "skipped"
	// DocumentStatusError: ingest failed. error_message says why.
	DocumentStatusError DocumentStatus = "error"
	// DocumentStatusSecretExcluded: withheld because it matched a secret
	// pattern. Counted as skipped, never searchable.
	DocumentStatusSecretExcluded DocumentStatus = "secret_excluded"
	// DocumentStatusPending: kept by the store when written, but no code path
	// in this binary writes it today.
	DocumentStatusPending DocumentStatus = "pending"
)

// NormalizeDocumentStatus folds a raw status into the value the store
// persists. Unknown and empty values fold to DocumentStatusOK. This is the
// exact rule the store has always applied (normalizeStatus).
func NormalizeDocumentStatus(raw string) DocumentStatus {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "skipped":
		return DocumentStatusSkipped
	case "error":
		return DocumentStatusError
	case "secret_excluded":
		// A document withheld because it contains secrets is not "ok": it has
		// zero searchable chunks and must persist faithfully so it stays
		// visible as an audit signal (and is counted as skipped, not indexed).
		return DocumentStatusSecretExcluded
	case "pending":
		return DocumentStatusPending
	default:
		return DocumentStatusOK
	}
}

// ListFilesStatus is the list_files projection of a stored document status:
// secret_excluded reads as skipped, unknown values read as ok. The result is
// always one of ListFilesStatusEnum.
func ListFilesStatus(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "skipped", "secret_excluded":
		return "skipped"
	case "pending":
		return "pending"
	case "error":
		return "error"
	default:
		return "ok"
	}
}

// ListFilesStatusEnum is the list_files `status` enum, in schema order.
var ListFilesStatusEnum = []string{"ok", "skipped", "pending", "error"}

// documentWritten is every status a document write produces in code today.
// pending is left out: nothing writes it.
var documentWritten = []DocumentStatus{
	DocumentStatusOK, DocumentStatusSkipped, DocumentStatusError, DocumentStatusSecretExcluded,
}

// DocumentLifecycle lists the documents.status transitions the code can make
// today. UpsertDocument writes status unconditionally, and a re-scan rebuilds
// any row from its bytes, so every state can reach every written state. The
// table forbids nothing yet; it is the baseline later fixes tighten.
var DocumentLifecycle = lifecycle.Table[DocumentStatus]{
	Name: "document",
	States: []DocumentStatus{
		DocumentStatusOK, DocumentStatusSkipped, DocumentStatusError,
		DocumentStatusSecretExcluded, DocumentStatusPending,
	},
	Initial: documentWritten,
	Allowed: map[DocumentStatus][]DocumentStatus{
		DocumentStatusOK:             documentWritten,
		DocumentStatusSkipped:        documentWritten,
		DocumentStatusError:          documentWritten,
		DocumentStatusSecretExcluded: documentWritten,
		DocumentStatusPending:        documentWritten,
	},
	Via: map[lifecycle.Edge]string{
		{From: "", To: "ok"}:                             "scan build (buildDocumentWithContent), archive member, MCP transcribe upsert",
		{From: "", To: "skipped"}:                        "scan build (archive, binary_ignored, ignore), size_cap, symlink, watch skip",
		{From: "", To: "error"}:                          "persistBuildError (unreadable source)",
		{From: "", To: "secret_excluded"}:                "raw-byte secret match at build",
		{From: "ok", To: "ok"}:                           "re-scan, finalizeContentHash (done marker)",
		{From: "ok", To: "skipped"}:                      "persistNonFatalDocSkip, watch skip, size_cap, re-classified type",
		{From: "ok", To: "error"}:                        "representation failure, persistNonFatalDocError",
		{From: "ok", To: "secret_excluded"}:              "derived-text secret match (secret_derived.go)",
		{From: "skipped", To: "ok"}:                      "re-scan of an eligible file",
		{From: "skipped", To: "skipped"}:                 "re-scan, archive finalize",
		{From: "skipped", To: "error"}:                   "archive extraction failure",
		{From: "skipped", To: "secret_excluded"}:         "re-scan with a secret match",
		{From: "error", To: "ok"}:                        "retry on the next scan",
		{From: "error", To: "skipped"}:                   "re-scan builds a skip row",
		{From: "error", To: "error"}:                     "retry fails again",
		{From: "error", To: "secret_excluded"}:           "retry with a secret match",
		{From: "secret_excluded", To: "ok"}:              "content changed or --force, no secret now",
		{From: "secret_excluded", To: "skipped"}:         "re-scan builds a skip row",
		{From: "secret_excluded", To: "error"}:           "re-scan fails",
		{From: "secret_excluded", To: "secret_excluded"}: "carrySecretExclusion, finalizeSecretExcluded",
		{From: "pending", To: "ok"}:                      "any rebuild (no writer produces pending)",
		{From: "pending", To: "skipped"}:                 "any rebuild",
		{From: "pending", To: "error"}:                   "any rebuild",
		{From: "pending", To: "secret_excluded"}:         "any rebuild",
	},
}

// EmbeddingStatus is the value of chunks.embedding_status. Chunk.EmbeddingStatus
// stays a plain string; these constants name the values the store keeps.
type EmbeddingStatus string

const (
	// EmbeddingStatusPending: waiting for the embed worker (NextPending).
	EmbeddingStatusPending EmbeddingStatus = "pending"
	// EmbeddingStatusOK: the vector is written.
	EmbeddingStatusOK EmbeddingStatus = "ok"
	// EmbeddingStatusError: the chunk failed or was quarantined. error_category
	// says why.
	EmbeddingStatusError EmbeddingStatus = "error"
)

// NormalizeEmbeddingStatus folds a raw status into the value the store
// persists. Unknown and empty values fold to EmbeddingStatusPending. This is
// the exact rule the store has always applied (normalizeEmbeddingStatus).
func NormalizeEmbeddingStatus(raw string) EmbeddingStatus {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "ok":
		return EmbeddingStatusOK
	case "error":
		return EmbeddingStatusError
	default:
		return EmbeddingStatusPending
	}
}

var embeddingAll = []EmbeddingStatus{EmbeddingStatusPending, EmbeddingStatusOK, EmbeddingStatusError}

// EmbeddingLifecycle lists the chunks.embedding_status transitions the code can
// make today. markEmbeddingStatus and the chunk upserts write the column
// without a WHERE on the old value, so every edge is reachable. The table
// forbids nothing yet.
var EmbeddingLifecycle = lifecycle.Table[EmbeddingStatus]{
	Name:    "chunk",
	States:  embeddingAll,
	Initial: []EmbeddingStatus{EmbeddingStatusPending, EmbeddingStatusError},
	Allowed: map[EmbeddingStatus][]EmbeddingStatus{
		EmbeddingStatusPending: embeddingAll,
		EmbeddingStatusOK:      embeddingAll,
		EmbeddingStatusError:   embeddingAll,
	},
	Via: map[lifecycle.Edge]string{
		{From: "", To: "pending"}:        "chunk insert (represent.go), UpsertChunkTask",
		{From: "", To: "error"}:          "quality-gate quarantine at insert",
		{From: "pending", To: "pending"}: "re-ingest chunk upsert, UpsertChunkTask",
		{From: "pending", To: "ok"}:      "MarkEmbedded",
		{From: "pending", To: "error"}:   "MarkFailed, MarkFailedWithCategory, quarantine on re-ingest",
		{From: "ok", To: "pending"}:      "RependEmbeddedChunks (reconcile), re-ingest chunk upsert",
		{From: "ok", To: "ok"}:           "MarkEmbedded again (unconditional UPDATE)",
		{From: "ok", To: "error"}:        "quarantine on re-ingest, MarkFailed (unconditional UPDATE)",
		{From: "error", To: "pending"}:   "RequeueFailedChunks, re-ingest chunk upsert",
		{From: "error", To: "ok"}:        "MarkEmbedded (unconditional UPDATE)",
		{From: "error", To: "error"}:     "MarkFailed again, quarantine on re-ingest",
	},
}
