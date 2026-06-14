package index

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/dirstral/dir2mcp/internal/index/diskindex"
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
//   - "memory" (default): the in-memory HNSW reference, persisted to the
//     versioned vectors_<kind>.v2.hnsw snapshot. This path is byte-identical to
//     legacy behavior.
//   - "disk": the Tier-B pure-Go on-disk backend (internal/index/diskindex),
//     persisted to vectors_<kind>.diskv1.idx with vector payloads kept
//     memory-mapped on disk so the corpus is not bounded by RAM.
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
	// Qdrant keeps no local index files: its durability lives server-side and a
	// reindex clears the collection via the index's Reset (driven by
	// EnsureIdentity), so there is nothing on disk to remove.
	if strings.EqualFold(strings.TrimSpace(backend), BackendQdrant) {
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
