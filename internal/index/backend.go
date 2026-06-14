package index

import (
	"path/filepath"
	"strings"

	"github.com/dirstral/dir2mcp/internal/index/diskindex"
	"github.com/dirstral/dir2mcp/internal/model"
)

// Backend names for the index.backend selector (issue #246). The seam is
// deliberately small so future networked backends (#268 Qdrant, #269 pgvector)
// extend it with new cases rather than reworking the dispatch.
const (
	BackendMemory = "memory"
	BackendDisk   = "disk"
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
	names := append([]string{TextIndexFileName, CodeIndexFileName}, LegacyIndexFileNames...)
	if strings.EqualFold(strings.TrimSpace(backend), BackendDisk) {
		for _, kind := range []string{KindText, KindCode} {
			seg := diskindex.SegmentFileName(kind)
			names = append(names, seg, seg+diskindex.IdentitySidecarSuffix)
		}
	}
	return names
}
