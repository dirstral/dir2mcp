package index

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/dirstral/dir2mcp/internal/index/diskindex"
	"github.com/dirstral/dir2mcp/internal/index/pgvectorindex"
	"github.com/dirstral/dir2mcp/internal/index/qdrantindex"
	"github.com/dirstral/dir2mcp/internal/model"
)

// Backend names for the index.backend selector (issue #246). The seam is
// deliberately small so future networked backends (#268 Qdrant, #269 pgvector)
// extend it with new cases rather than reworking the dispatch.
const (
	BackendMemory = "memory"
	BackendDisk   = "disk"
	// BackendQdrant is the networked Qdrant vector backend (issue #268). Unlike
	// memory/disk it has no local persistence path: it is constructed by the CLI
	// (internal/cli) via qdrantindex.Open with the resolved connection config,
	// not by NewBackend (which only knows the local stateDir). NewBackend never
	// returns a qdrant index; the const exists for the selector/StaleIndexFiles.
	BackendQdrant = "qdrant"
	// BackendPgvector is the networked PostgreSQL + pgvector backend (issue #269).
	// Like qdrant it has no local persistence path: it is constructed by the CLI
	// via NewPgvectorBackend with the resolved DSN/schema/table, not by
	// NewBackend. The const exists for the selector/StaleIndexFiles.
	BackendPgvector = "pgvector"
)

// IndexKind names the two per-corpus vector indices.
const (
	KindText = "text"
	KindCode = "code"
)

// NewBackend constructs the configured vector index for one kind ("text" or
// "code") behind the model.Index interface, returning the constructed index and
// the on-disk path it persists to (used by the caller to wire the
// PersistenceManager and reindex cleanup).
//
//   - "memory" (default): the in-memory reference (HNSWIndex; the name is
//     historical, and it performs an exhaustive brute-force cosine scan, not
//     approximate HNSW search), persisted to the versioned
//     vectors_<kind>.v2.hnsw snapshot. This path is byte-identical to legacy
//     behavior.
//   - "disk": the Tier-B pure-Go on-disk backend (internal/index/diskindex),
//     also an exhaustive brute-force cosine scan (not ANN), persisted to
//     vectors_<kind>.diskv1.idx with vector payloads kept memory-mapped on
//     disk so the corpus is not bounded by RAM.
//
// Both memory and disk are exact (linear-cost) search, not approximate
// nearest-neighbor; see docs/dual-machine-deployment.md §2.4 for the
// exact-vs-ANN trade-off and the corpus-size guidance for switching to
// "qdrant"/"pgvector" (issue #429 F3).
//
// An empty/unknown backend falls back to "memory"; config validation already
// rejects unknown values, so this is defensive only.
func NewBackend(backend, stateDir, kind string) (model.Index, string) {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case BackendDisk:
		path := filepath.Join(stateDir, diskindex.SegmentFileName(kind))
		return diskindex.New(path), path
	default:
		path := filepath.Join(stateDir, kindSnapshotFileName(kind))
		return NewHNSWIndex(path), path
	}
}

// QdrantParams carries the resolved connection config for the networked Qdrant
// backend (issue #268). It mirrors the index.qdrant.{url,api_key,collection}
// config block; the APIKey is a runtime-only secret resolved by the caller.
type QdrantParams struct {
	URL        string
	APIKey     string
	Collection string
}

// NewQdrantBackend opens a Qdrant-backed model.Index for one kind ("text" or
// "code"), deriving a distinct per-kind collection (<collection>_text /
// <collection>_code) so the two vector spaces never collide in a shared Qdrant.
// Unlike NewBackend it has no local persistence path (Qdrant owns durability)
// and the returned index is deliberately NOT model.Persistable, so the caller's
// Load step is correctly skipped. An unreachable/misconfigured endpoint returns
// a non-nil error (no silent fallback). It lives here, alongside the local
// dispatch, so internal/cli has a single backend-construction entrypoint.
func NewQdrantBackend(ctx context.Context, p QdrantParams, kind string) (model.Index, error) {
	return qdrantindex.Open(ctx, qdrantindex.BackendConfig{
		URL:        p.URL,
		APIKey:     p.APIKey,
		Collection: QdrantCollectionForKind(p.Collection, kind),
	})
}

// QdrantCollectionForKind derives the per-kind Qdrant collection name from the
// configured base. An empty base falls back to qdrantindex.DefaultCollection so
// the text/code suffixes remain stable across runs. Exported so callers/tests
// can assert the text/code collections never collide.
func QdrantCollectionForKind(base, kind string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = qdrantindex.DefaultCollection
	}
	if kind == KindCode {
		return base + "_" + KindCode
	}
	return base + "_" + KindText
}

// PgvectorParams carries the resolved connection config for the networked
// PostgreSQL + pgvector backend (issue #269). It mirrors the
// index.pgvector.{dsn,schema,table} config block; the DSN is a runtime-only
// secret resolved by the caller and never persisted.
type PgvectorParams struct {
	DSN    string
	Schema string
	Table  string
}

// NewPgvectorBackend opens a pgvector-backed model.Index for one kind ("text" or
// "code"), deriving a distinct per-kind table (<table> / <table>_code) so the
// two vector spaces never collide in a shared database. Like NewQdrantBackend it
// has no local persistence path (Postgres owns durability) and the returned
// index is deliberately NOT model.Persistable, so the caller's Load step is
// correctly skipped. An unreachable/misconfigured endpoint (bad DSN, missing
// pgvector extension, no CREATE privilege) returns a non-nil error — no silent
// fallback. It lives here, alongside the local dispatch, so internal/cli has a
// single backend-construction entrypoint.
func NewPgvectorBackend(ctx context.Context, p PgvectorParams, kind string) (model.Index, error) {
	return pgvectorindex.Open(ctx, pgvectorindex.Config{
		DSN:    p.DSN,
		Schema: p.Schema,
		Table:  PgvectorTableForKind(p.Table, kind),
	})
}

// PgvectorTableForKind derives the per-kind vectors table name from the
// configured base. An empty base falls back to pgvectorindex.DefaultTable so the
// text/code suffixes remain stable across runs. The text axis keeps the base
// name (so an existing single-axis table is reused) and the code axis appends
// "_code". Exported so callers/tests can assert the text/code tables never
// collide.
func PgvectorTableForKind(base, kind string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = pgvectorindex.DefaultTable
	}
	if kind == KindCode {
		return base + "_" + KindCode
	}
	return base
}

// kindSnapshotFileName maps an index kind to its HNSW v2 snapshot basename.
func kindSnapshotFileName(kind string) string {
	if kind == KindCode {
		return CodeIndexFileName
	}
	return TextIndexFileName
}

// StaleIndexFiles returns the basenames that a reindex must remove from the
// state directory for the given backend so a stale snapshot of any shape cannot
// survive a rebuild. The HNSW basenames (current v2 + legacy) are always
// included because a corpus may have been built under the memory backend before
// switching; the disk backend's segment + identity sidecar are added when
// selected.
func StaleIndexFiles(backend string) []string {
	// Networked backends keep no local index files: their durability lives
	// server-side, so there is nothing on disk to move aside or remove.
	//
	// This function returning nil is therefore NOT a statement that a reindex
	// rebuilds the remote store. It does not. `dir2mcp reindex` never opens an
	// index at all: it rewrites the sqlite rows, re-queues every chunk it
	// rewrote, and leaves the vectors to the daemon's embed worker. Reset runs
	// only through EnsureIdentity, which resets only a fresh or incompatible
	// index, and only `up`/`ask` call it. So a reindex under one of these
	// backends leaves the previous generation's vectors in place until the next
	// `dir2mcp up` re-embeds the queued chunks over them (issue #668).
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case BackendQdrant, BackendPgvector:
		return nil
	}
	names := append([]string{TextIndexFileName, CodeIndexFileName}, LegacyIndexFileNames...)
	if strings.EqualFold(strings.TrimSpace(backend), BackendDisk) {
		for _, kind := range []string{KindText, KindCode} {
			seg := diskindex.SegmentFileName(kind)
			names = append(names, seg, seg+diskindex.IdentitySidecarSuffix)
		}
	}
	return names
}
