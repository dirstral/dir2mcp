package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Batch-ergonomics surface for large-archive media ingests (SPEC §8.6.11).
//
// This file implements three optional, additive, off-by-default features that
// change ingest ORDERING and REPORTING only — never the resulting
// representations, chunks, embeddings, or citations:
//
//   - a side-channel, monotonic progress reporter (media.batch.progress);
//   - a JSONL run manifest, one record per asset (media.batch.manifest);
//   - a two-phase pass split (media.batch.two_phase), implemented in service.go.
//
// All three are governed by the media.batch config block and are inert when
// unconfigured, so a default ingest is byte-identical to today.

// Canonical §14.4 error codes recorded in a manifest record's `error_code`
// field (§7.7). EXTRACT_FAILED is the generic representation/derivation failure.
// TRANSCRIBE_FAILED / OCR_FAILED / TRANSLATE_FAILED are distinguished via the
// package's provider-failure sentinels (ErrTranscriptProviderFailure /
// ErrOCRProviderFailure / ErrTranslateProviderFailure) in manifestErrorCode, and
// ALSO cover the degenerate-output quality-gate sub-case (§8.6.6): an OCR /
// transcript / translation output rejected by the gate records the matching code
// via qualityGateFailureCode (service.go).
const (
	manifestErrTranscribeFailed = "TRANSCRIBE_FAILED"
	manifestErrExtractFailed    = "EXTRACT_FAILED"
	manifestErrOCRFailed        = "OCR_FAILED"
	manifestErrTranslateFailed  = "TRANSLATE_FAILED"
	// manifestErrFileTooLarge (§14.4) classifies an asset over the ingest size cap.
	manifestErrFileTooLarge = "FILE_TOO_LARGE"
	// manifestErrBinarySkipped (§14.4) is recorded on the skipped manifest entry
	// for a non-textual binary asset dropped from ingestion.
	manifestErrBinarySkipped = "BINARY_SKIPPED"
)

// manifestErrorCode maps a per-asset processing error to a canonical §14.4 code
// for the run manifest. It distinguishes translation, OCR, transcript provider
// failures, and an over-cap file via their sentinels (TRANSLATE_FAILED /
// OCR_FAILED / TRANSCRIBE_FAILED / FILE_TOO_LARGE); any other
// representation/derivation failure is recorded as the generic EXTRACT_FAILED.
// Translate is matched first because a Whisper-engine translation failure is a
// translation failure, not a transcription one.
func manifestErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrTranslateProviderFailure):
		return manifestErrTranslateFailed
	case errors.Is(err, ErrOCRProviderFailure):
		return manifestErrOCRFailed
	case errors.Is(err, ErrTranscriptProviderFailure):
		return manifestErrTranscribeFailed
	case errors.Is(err, ErrFileTooLarge):
		return manifestErrFileTooLarge
	default:
		return manifestErrExtractFailed
	}
}

// batchManifestStatus is a terminal per-asset outcome (SPEC §8.6.11).
type batchManifestStatus string

const (
	batchStatusCompleted batchManifestStatus = "completed"
	batchStatusSkipped   batchManifestStatus = "skipped"
	batchStatusError     batchManifestStatus = "error"
)

// batchManifestRecord is one JSONL line in the run manifest (SPEC §8.6.11).
// The field set is self-describing and deterministic so the manifest is stable
// and machine-consumable. Optional fields use omitempty so an unknown value
// (e.g. an undecodable duration) is simply absent rather than a misleading zero.
type batchManifestRecord struct {
	// asset identity (§7.8/§7.6).
	RelPath     string `json:"rel_path"`
	ContentHash string `json:"content_hash,omitempty"`

	// terminal outcome (§7.7). On error, ErrorCode carries the canonical §14.4
	// code and ErrorMessage a redacted human message.
	Status       batchManifestStatus `json:"status"`
	ErrorCode    string              `json:"error_code,omitempty"`
	ErrorMessage string              `json:"error_message,omitempty"`

	// media duration (when known) and processing time for the asset.
	DurationMS   int64 `json:"duration_ms,omitempty"`
	ProcessingMS int64 `json:"processing_ms"`

	// outputs produced — the derived representation rep_types and any export
	// artifacts (e.g. transcript/translated language tags, subtitle formats).
	Outputs []string `json:"outputs,omitempty"`

	// the pass that produced this record under two-phase mode
	// ("transcription"|"derivation"); empty under single-pass.
	Pass string `json:"pass,omitempty"`
}

// assetOutcome accumulates the observable result of processing a single asset
// so a manifest record can be emitted and progress advanced. It is populated by
// processDocument and the representation generators when a batch run is active;
// when no batch run is active the accumulator is nil and the hot path is
// untouched.
type assetOutcome struct {
	relPath     string
	contentHash string
	status      batchManifestStatus
	errorCode   string
	errorMsg    string
	durationMS  int64
	startedAt   time.Time

	mu      sync.Mutex
	outputs map[string]struct{}
}

// newAssetOutcome starts timing an asset's processing.
func newAssetOutcome(relPath string) *assetOutcome {
	return &assetOutcome{
		relPath:   relPath,
		status:    batchStatusCompleted,
		startedAt: time.Now(),
		outputs:   make(map[string]struct{}),
	}
}

// addOutput records a produced output label (a rep_type or export artifact tag)
// on the outcome. Safe for concurrent use. No-op on a nil receiver so callers
// need not branch when a batch run is inactive.
func (o *assetOutcome) addOutput(label string) {
	if o == nil || label == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.outputs == nil {
		o.outputs = make(map[string]struct{})
	}
	o.outputs[label] = struct{}{}
}

// setContentHash records the asset's resolved content_hash (§7.6). No-op on a
// nil receiver or empty hash.
func (o *assetOutcome) setContentHash(hash string) {
	if o == nil || hash == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.contentHash = hash
}

// markErrorIfUnset records a terminal error outcome ONLY when one is not already
// recorded, so a specific inner error code (set by a representation generator) is
// never clobbered by a generic outer one. No-op on a nil receiver.
func (o *assetOutcome) markErrorIfUnset(code, message string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.status == batchStatusError {
		return
	}
	o.status = batchStatusError
	o.errorCode = code
	o.errorMsg = message
}

// markSkipped records a terminal skipped outcome (no work performed: cache hit,
// unchanged content, or a non-ingestable type). No-op on a nil receiver.
func (o *assetOutcome) markSkipped() {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	// An error already recorded wins over a late skip signal.
	if o.status != batchStatusError {
		o.status = batchStatusSkipped
	}
}

// markSkippedWithCode records a terminal skipped outcome and stamps a canonical
// §14.4 code on it (e.g. BINARY_SKIPPED), so a skip that carries a machine
// classification surfaces in the run manifest's error_code. No-op on a nil
// receiver; a recorded error still wins over a late skip.
func (o *assetOutcome) markSkippedWithCode(code string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.status == batchStatusError {
		return
	}
	o.status = batchStatusSkipped
	o.errorCode = code
}

// record materializes the deterministic manifest record from the accumulated
// outcome. Outputs are sorted so the record is reproducible across runs of an
// unchanged corpus (§8.6.11 determinism).
func (o *assetOutcome) record(pass string) batchManifestRecord {
	o.mu.Lock()
	defer o.mu.Unlock()
	outputs := make([]string, 0, len(o.outputs))
	for label := range o.outputs {
		outputs = append(outputs, label)
	}
	sort.Strings(outputs)
	if len(outputs) == 0 {
		outputs = nil
	}
	return batchManifestRecord{
		RelPath:      o.relPath,
		ContentHash:  o.contentHash,
		Status:       o.status,
		ErrorCode:    o.errorCode,
		ErrorMessage: o.errorMsg,
		DurationMS:   o.durationMS,
		ProcessingMS: time.Since(o.startedAt).Milliseconds(),
		Outputs:      outputs,
		Pass:         pass,
	}
}

// batchRun owns the per-scan batch state: the optional JSONL manifest writer and
// the optional progress reporter. It is created once per runScan when any
// media.batch feature is enabled and is nil otherwise, so the default ingest
// path allocates nothing and behaves exactly as before.
type batchRun struct {
	logger *log.Logger

	// progress
	progressEnabled bool
	total           int64
	completed       int64
	pass            string // current pass label for progress/manifest

	// manifest
	manifestEnabled bool
	manifestPath    string

	mu   sync.Mutex
	file *os.File
	enc  *json.Encoder
}

// newBatchRun constructs the per-scan batch state from config. It returns nil
// when no media.batch feature is enabled so the caller's hot path is untouched.
// When the manifest is enabled it opens (truncates) the manifest file up front;
// a manifest open failure is non-fatal — it disables the manifest and warns,
// because batch ergonomics must never fail an otherwise-healthy ingest.
func newBatchRun(progress, manifest bool, manifestPath string, logger *log.Logger) *batchRun {
	if !progress && !manifest {
		return nil
	}
	if logger == nil {
		logger = log.Default()
	}
	br := &batchRun{
		logger:          logger,
		progressEnabled: progress,
	}
	if manifest && manifestPath != "" {
		if err := br.openManifest(manifestPath); err != nil {
			logger.Printf("batch manifest disabled: %v", err)
		} else {
			br.manifestEnabled = true
			br.manifestPath = manifestPath
		}
	}
	return br
}

func (b *batchRun) openManifest(path string) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create manifest dir %s: %w", dir, err)
		}
	}
	f, err := os.Create(path) //nolint:gosec // operator-configured manifest path
	if err != nil {
		return fmt.Errorf("create manifest %s: %w", path, err)
	}
	b.file = f
	b.enc = json.NewEncoder(f)
	return nil
}

// startPass (re)establishes the progress total at the beginning of a pass and
// records the pass label stamped on progress lines and manifest records. The
// completed counter is reset so progress is monotonic WITHIN a pass (§8.6.11).
func (b *batchRun) startPass(label string, total int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.pass = label
	b.total = int64(total)
	b.completed = 0
	b.mu.Unlock()
	if b.progressEnabled {
		if label != "" {
			b.logger.Printf("ingest progress: starting %s pass (%d assets)", label, total)
		} else {
			b.logger.Printf("ingest progress: starting (%d assets)", total)
		}
	}
}

// advance records that one unit finished (completed, skipped, or errored — all
// count toward the monotonic completed total per §8.6.11, so a resumed run that
// resolves an asset from cache still reports faithful totals) and emits a
// progress line via the structured logger. Side-channel only: it never alters
// outputs, ordering, or error semantics.
func (b *batchRun) advance() {
	if b == nil || !b.progressEnabled {
		return
	}
	b.mu.Lock()
	b.completed++
	done := b.completed
	total := b.total
	pass := b.pass
	b.mu.Unlock()
	if pass != "" {
		b.logger.Printf("ingest progress [%s]: %d/%d", pass, done, total)
	} else {
		b.logger.Printf("ingest progress: %d/%d", done, total)
	}
}

// write appends one manifest record as a JSONL line. No-op when the manifest is
// disabled. A write error is logged and swallowed — the manifest is advisory and
// must never fail the ingest. Concurrency-safe.
func (b *batchRun) write(rec batchManifestRecord) {
	if b == nil || !b.manifestEnabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.enc == nil {
		return
	}
	if err := b.enc.Encode(rec); err != nil {
		b.logger.Printf("batch manifest write failed for %s: %v", rec.RelPath, err)
	}
}

// finalize records an outcome at the end of an asset's processing: it appends
// the manifest record and advances progress. It is the single completion point
// so the manifest and the progress total stay consistent. No-op on nil.
func (b *batchRun) finalize(o *assetOutcome) {
	if b == nil || o == nil {
		return
	}
	b.mu.Lock()
	pass := b.pass
	b.mu.Unlock()
	b.write(o.record(pass))
	b.advance()
}

// recordSkippedWithCode writes a single skipped manifest record carrying a
// canonical §14.4 code for an asset that never entered the per-asset processing
// loop (e.g. a file dropped at discovery for exceeding the size cap, #497). It
// is manifest-only and deliberately does NOT advance progress: the file was
// never counted in a pass total, so advancing here would push the counter past
// the total. No-op on nil or when the manifest is disabled.
func (b *batchRun) recordSkippedWithCode(relPath, code string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	pass := b.pass
	b.mu.Unlock()
	b.write(batchManifestRecord{
		RelPath:   relPath,
		Status:    batchStatusSkipped,
		ErrorCode: code,
		Pass:      pass,
	})
}

// close flushes and closes the manifest file. Safe to call on nil.
func (b *batchRun) close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.file != nil {
		if err := b.file.Close(); err != nil {
			b.logger.Printf("batch manifest close failed: %v", err)
		}
		b.file = nil
		b.enc = nil
	}
}
