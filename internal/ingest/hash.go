package ingest

import (
	"crypto/sha256"
	"encoding/hex"
)

// computeContentHash computes a stable sha256 hash of file content.
// This is used for incremental indexing to detect if a document has changed.
func computeContentHash(content []byte) string {
	return ComputeContentHash(content)
}

// ComputeContentHash computes a stable sha256 hash of content.
func ComputeContentHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// computeRepHash computes a stable sha256 hash of representation content.
// Historically this duplicated the logic of computeContentHash, but the
// algorithms are identical.  Delegate to computeContentHash so there’s a
// single authoritative implementation of the sha256+hex logic.
func computeRepHash(content []byte) string {
	return ComputeRepHash(content)
}

// ComputeRepHash computes a stable sha256 hash of representation content.
func ComputeRepHash(content []byte) string {
	return ComputeContentHash(content)
}

// mediaContentHash returns the document content hash, folding in a sidecar
// fingerprint when present. With an empty fingerprint it is exactly
// computeContentHash(content), so non-media documents and media without sidecars
// keep their historical hash. A non-empty fingerprint (sidecar paths + mtimes)
// changes the hash whenever a sidecar is added, removed, or modified, which the
// incremental gate (§7.6) uses to re-process the media even though its bytes are
// unchanged.
func mediaContentHash(content []byte, sidecarFingerprint string) string {
	if sidecarFingerprint == "" {
		return computeContentHash(content)
	}
	combined := make([]byte, 0, len(content)+len(sidecarFingerprint)+1)
	combined = append(combined, content...)
	combined = append(combined, '\x00')
	combined = append(combined, sidecarFingerprint...)
	return computeContentHash(combined)
}

// needsReprocessing determines if a document needs to be reprocessed based on
// hash comparison. Returns true if the document should be reprocessed.
func needsReprocessing(oldHash, newHash string, forceReindex bool) bool {
	return NeedsReprocessing(oldHash, newHash, forceReindex)
}

// NeedsReprocessing determines if content should be reprocessed.
func NeedsReprocessing(oldHash, newHash string, forceReindex bool) bool {
	if forceReindex {
		return true
	}
	if oldHash == "" {
		// No existing hash means this is a new document
		return true
	}
	// Reprocess if hash has changed
	return oldHash != newHash
}
