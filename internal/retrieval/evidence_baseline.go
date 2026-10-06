package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
)

// Calibrated evidence threshold (SPEC §9.4.3, spec 0.76.0; issue #1081).
//
// The fixed cosine floor in evidenceThresholds fits every embedding family
// only because it rejects near-orthogonal text and nothing else. With a local
// embedder such as nomic-embed-text every question scores far above it: on the
// benchmark corpus the top cosine of an OFF-CORPUS question was 0.42 to 0.62,
// so the guard never fired. What separates "the corpus holds this subject"
// from "it does not" is not a constant; it is where unrelated questions land
// for THIS embedder on THIS corpus. That is the null baseline: embed the
// shipped probe questions (null_probes.go), take the top cosine of each against
// the index, and summarize the distribution.
//
// RULE. cosine threshold = max(fixed floor, baseline p90). The p90 is the
// 90th percentile of the probe readings, which a few on-topic probes cannot
// move (the maximum can, and the spec forbids it). Measured on the benchmark
// corpus with nomic-embed-text (344 questions that do not overlap the
// published 120, 32 probes): the rule refuses 1.5% of answerable questions,
// 0% of on-topic unanswerable ones and 26.6% of off-corpus ones; the fixed
// floor refused none of each.
//
// A provider whose baseline sits below the fixed floor keeps the floor, so
// the shipped behaviour is unchanged there. The rerank scale is untouched: a
// cross-encoder score is calibrated per pair, not per corpus.
//
// LIFECYCLE. The baseline is computed lazily on the first request that needs
// a verdict, and proactively by `up` once indexing stops, in one embed call
// of all probes plus one k=1 index search per probe. It is cached in memory
// and in <state_dir>/evidence_baseline.json, keyed by the index's recorded
// embed identity (SPEC §8.1.4: provider, model, dimensions, late chunking and
// the contextual prompt), the probe set version and the indexed chunk count,
// so a reindex, a model change or a contextual-retrieval change recomputes it
// and a restart does not. While it is unavailable
// (not computed yet, empty index, embedder unreachable) the fixed floor
// applies and no request is refused for that reason: this guard fails open,
// like every other part of it. A failed computation is retried after
// evidenceBaselineRetryAfter, not on every request, so a provider outage does
// not turn each ask into two failing calls.

// EvidenceCosineFloor and EvidenceRerankFloor are the fixed floors of
// evidenceThresholds, exported for `dir2mcp doctor`, which reports the
// threshold in effect without a running service.
var (
	EvidenceCosineFloor = evidenceThresholds[evidenceScaleCosine]
	EvidenceRerankFloor = evidenceThresholds[evidenceScaleRerank]
)

// evidenceBaselineFileName is the state-dir cache of the null baseline.
// `dir2mcp doctor` reads it without a daemon.
const evidenceBaselineFileName = "evidence_baseline.json"

// evidenceBaselineRetryAfter bounds how soon a failed baseline computation is
// attempted again.
const evidenceBaselineRetryAfter = time.Minute

// evidenceBaselineFile is the on-disk shape of the cache: the baseline plus
// the key it was computed under, so a reader can tell a stale file from a
// current one without recomputing anything.
type evidenceBaselineFile struct {
	Key      string                 `json:"key"`
	Baseline model.EvidenceBaseline `json:"baseline"`
}

// evidenceBaselineCache is the in-memory half of the cache plus the failure
// backoff. Its own mutex keeps a computation (an embed call) off metaMu.
type evidenceBaselineCache struct {
	mu       sync.Mutex
	value    *model.EvidenceBaseline
	key      string
	failedAt time.Time
	// loadedFile is set once the state-dir file has been consulted, so a
	// stale file is read at most once per process.
	loadedFile bool
}

// evidenceThresholdSettings is the operator's rag.evidence_threshold choice.
// Zero value: neither auto nor pinned, which keeps the fixed floors (the
// behaviour of a service nobody configured, and of every existing test).
type evidenceThresholdSettings struct {
	auto   bool
	pinned float64
}

// SetEvidenceThreshold wires rag.evidence_threshold (SPEC §9.4.3, spec 0.76.0).
// auto selects the calibrated rule; otherwise pinned, when positive, is the
// cosine threshold applied as-is; both false/zero keeps the fixed floors.
func (s *Service) SetEvidenceThreshold(auto bool, pinned float64) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.evidenceThreshold = evidenceThresholdSettings{auto: auto, pinned: pinned}
	if pinned > 0 {
		s.evidenceThreshold.auto = false
	}
}

// CalibratesEvidence reports whether SetEvidenceThreshold selected the
// calibrated rule or a pinned value. It is the model.EvidenceReporter gate:
// dir2mcp_stats declares and emits `evidence` only when this is true.
func (s *Service) CalibratesEvidence() bool {
	s.metaMu.RLock()
	defer s.metaMu.RUnlock()
	return s.evidenceThreshold.auto || s.evidenceThreshold.pinned > 0
}

// evidenceCosineThreshold applies the §9.4.3 rule and names its source.
// Exported through EvidenceCosineThreshold for the tests in tests/retrieval.
func evidenceCosineThreshold(floor float64, baseline *model.EvidenceBaseline, settings evidenceThresholdSettings) (float64, string) {
	if settings.pinned > 0 {
		return settings.pinned, model.EvidenceThresholdSourceConfig
	}
	if !settings.auto || baseline == nil || baseline.Probes == 0 {
		return floor, model.EvidenceThresholdSourceFloor
	}
	if math.IsNaN(baseline.P90) || baseline.P90 <= floor {
		return floor, model.EvidenceThresholdSourceFloor
	}
	return baseline.P90, model.EvidenceThresholdSourceAuto
}

// EvidenceCosineThreshold is the pure threshold rule of SPEC §9.4.3 for the
// cosine scale: max(floor, baseline p90) under auto, the pinned value when one
// is set, and the floor when no baseline is available. It returns the value
// and its source name (model.EvidenceThresholdSource*).
func EvidenceCosineThreshold(floor float64, baseline *model.EvidenceBaseline, auto bool, pinned float64) (float64, string) {
	settings := evidenceThresholdSettings{auto: auto, pinned: pinned}
	if pinned > 0 {
		settings.auto = false
	}
	return evidenceCosineThreshold(floor, baseline, settings)
}

// SummarizeNullBaseline builds the baseline statistics from the top cosine of
// each probe. Quantiles interpolate linearly on the sorted readings (the
// definition the benchmark measurement used), so a published number can be
// reproduced from the raw readings. An empty slice yields a zero baseline.
func SummarizeNullBaseline(topScores []float64) model.EvidenceBaseline {
	if len(topScores) == 0 {
		return model.EvidenceBaseline{}
	}
	sorted := append([]float64(nil), topScores...)
	sort.Float64s(sorted)
	return model.EvidenceBaseline{
		Probes: len(sorted),
		P50:    quantileSorted(sorted, 0.5),
		P90:    quantileSorted(sorted, 0.9),
		Max:    sorted[len(sorted)-1],
	}
}

func quantileSorted(sorted []float64, q float64) float64 {
	pos := float64(len(sorted)-1) * q
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	return sorted[lo] + (sorted[hi]-sorted[lo])*(pos-float64(lo))
}

// effectiveEvidenceThresholds returns the per-scale thresholds the verdict
// functions apply right now. Under auto it computes or loads the baseline on
// first use; every other setting is a map lookup.
func (s *Service) effectiveEvidenceThresholds(ctx context.Context) map[string]float64 {
	s.metaMu.RLock()
	settings := s.evidenceThreshold
	s.metaMu.RUnlock()
	if !settings.auto && settings.pinned <= 0 {
		return evidenceThresholds
	}
	var baseline *model.EvidenceBaseline
	if settings.auto {
		baseline = s.nullBaseline(ctx)
	}
	cosine, _ := evidenceCosineThreshold(evidenceThresholds[evidenceScaleCosine], baseline, settings)
	return map[string]float64{
		evidenceScaleCosine: cosine,
		evidenceScaleRerank: evidenceThresholds[evidenceScaleRerank],
	}
}

// EvidenceReport is the dir2mcp_stats `evidence` object (SPEC §15.6): the
// thresholds in effect and the baseline behind them. Under auto it computes
// the baseline when none is cached yet, so stats after indexing reports the
// real number rather than "not computed".
func (s *Service) EvidenceReport(ctx context.Context) model.EvidenceReport {
	s.metaMu.RLock()
	settings := s.evidenceThreshold
	s.metaMu.RUnlock()
	var baseline *model.EvidenceBaseline
	if settings.auto || settings.pinned > 0 {
		baseline = s.nullBaseline(ctx)
	}
	cosine, source := evidenceCosineThreshold(evidenceThresholds[evidenceScaleCosine], baseline, settings)
	return model.EvidenceReport{
		CosineThreshold:       cosine,
		CosineThresholdSource: source,
		RerankThreshold:       evidenceThresholds[evidenceScaleRerank],
		Baseline:              baseline,
	}
}

// WarmEvidenceBaseline computes and caches the null baseline when the
// calibrated rule is on. `up` calls it once indexing stops, so the first ask
// and `doctor` find it ready. It returns the error of a failed computation
// for the caller's log; the service itself has already fallen back to the
// fixed floor.
func (s *Service) WarmEvidenceBaseline(ctx context.Context) error {
	s.metaMu.RLock()
	settings := s.evidenceThreshold
	s.metaMu.RUnlock()
	if !settings.auto && settings.pinned <= 0 {
		return nil
	}
	_, err := s.ensureNullBaseline(ctx)
	return err
}

// nullBaseline returns the cached baseline for the current (embed identity,
// model, probe set, chunk count) key, computing it when the key changed, or
// nil when it is unavailable. Never returns an error: unavailability is the
// fail-open case.
func (s *Service) nullBaseline(ctx context.Context) *model.EvidenceBaseline {
	baseline, _ := s.ensureNullBaseline(ctx)
	return baseline
}

func (s *Service) ensureNullBaseline(ctx context.Context) (*model.EvidenceBaseline, error) {
	s.metaMu.RLock()
	modelName := s.textModel
	idx := s.textIndex
	embedder := s.embedder
	chunks := len(s.chunkByLabel)
	stateDir := s.stateDir
	s.metaMu.RUnlock()
	if embedder == nil || idx == nil {
		return nil, nil
	}
	identity, err := idx.Identity(ctx)
	if err != nil {
		return nil, fmt.Errorf("read embed identity: %w", err)
	}
	key := evidenceBaselineKey(identity, modelName, chunks)

	c := &s.evidenceBaseline
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached := c.lookup(key, stateDir); cached != nil {
		return cached, nil
	}
	if !c.failedAt.IsZero() && time.Since(c.failedAt) < evidenceBaselineRetryAfter {
		return nil, nil
	}
	baseline, err := computeNullBaseline(ctx, embedder, modelName, idx)
	if err != nil {
		c.failedAt = time.Now()
		s.logf("evidence: null baseline unavailable, the fixed cosine floor %.2f applies: %v", evidenceThresholds[evidenceScaleCosine], err)
		return nil, err
	}
	if baseline == nil {
		// An empty index: nothing to calibrate against yet. Not a failure, and
		// not cached either, so the first ask after indexing computes it.
		return nil, nil
	}
	baseline.Chunks = chunks
	baseline.EmbedModel = modelName
	c.value, c.key, c.failedAt = baseline, key, time.Time{}
	if stateDir != "" {
		if err := writeEvidenceBaseline(stateDir, evidenceBaselineFile{Key: key, Baseline: *baseline}); err != nil {
			s.logf("evidence: cache null baseline: %v", err)
		}
	}
	s.logf("evidence: null baseline over %d probes p50=%.3f p90=%.3f max=%.3f (%s, %d chunks); cosine threshold %.3f",
		baseline.Probes, baseline.P50, baseline.P90, baseline.Max, modelName, chunks,
		math.Max(evidenceThresholds[evidenceScaleCosine], baseline.P90))
	return baseline, nil
}

// lookup returns the cached baseline for key from memory or, once per
// process, from the state-dir file when that file was written for the same
// model, probe set and chunk count. The caller holds c.mu.
func (c *evidenceBaselineCache) lookup(key, stateDir string) *model.EvidenceBaseline {
	if c.value != nil && c.key == key {
		return c.value
	}
	if c.loadedFile {
		return nil
	}
	c.loadedFile = true
	file, err := loadEvidenceBaselineFile(stateDir)
	if err != nil || file == nil || file.Key != key {
		return nil
	}
	cached := file.Baseline
	c.value, c.key = &cached, key
	return &cached
}

// evidenceBaselineKey names what a baseline was computed under. The identity
// is the index's recorded embed identity; the model name is kept beside it
// for an index that records none.
func evidenceBaselineKey(identity, modelName string, chunks int) string {
	return fmt.Sprintf("%s|%s|%s|%d", strings.TrimSpace(identity), strings.TrimSpace(modelName), nullProbeSetVersion, chunks)
}

// computeNullBaseline embeds the probe set in one call and reads the top
// cosine of each probe from the index. It returns nil, nil when the index
// answers no probe (an empty index).
func computeNullBaseline(ctx context.Context, embedder model.Embedder, modelName string, idx model.Index) (*model.EvidenceBaseline, error) {
	vectors, err := embedder.Embed(ctx, modelName, model.EmbedQuery, nullProbes)
	if err != nil {
		return nil, fmt.Errorf("embed %d probes: %w", len(nullProbes), err)
	}
	if len(vectors) != len(nullProbes) {
		return nil, fmt.Errorf("embed %d probes: got %d vectors", len(nullProbes), len(vectors))
	}
	tops := make([]float64, 0, len(vectors))
	for i, vec := range vectors {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hits, err := idx.Search(ctx, vec, 1, model.Filter{})
		if err != nil {
			return nil, fmt.Errorf("probe %d: %w", i, err)
		}
		if len(hits) == 0 {
			continue
		}
		tops = append(tops, float64(hits[0].Score))
	}
	if len(tops) == 0 {
		return nil, nil
	}
	baseline := SummarizeNullBaseline(tops)
	baseline.ProbeSet = nullProbeSetVersion
	baseline.ComputedAt = time.Now().UTC().Format(time.RFC3339)
	return &baseline, nil
}

// LoadEvidenceBaseline reads the cached null baseline from stateDir. It
// returns nil, nil when no cache exists. `dir2mcp doctor` uses it to report the
// baseline without a daemon; the service uses it to survive a restart.
func LoadEvidenceBaseline(stateDir string) (*model.EvidenceBaseline, error) {
	file, err := loadEvidenceBaselineFile(stateDir)
	if err != nil || file == nil {
		return nil, err
	}
	baseline := file.Baseline
	return &baseline, nil
}

func loadEvidenceBaselineFile(stateDir string) (*evidenceBaselineFile, error) {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, evidenceBaselineFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var file evidenceBaselineFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("decode %s: %w", evidenceBaselineFileName, err)
	}
	if file.Baseline.Probes <= 0 {
		return nil, nil
	}
	return &file, nil
}

// writeEvidenceBaseline persists the baseline via a temp file and rename, so a
// reader never sees a partial file.
func writeEvidenceBaseline(stateDir string, file evidenceBaselineFile) error {
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	final := filepath.Join(stateDir, evidenceBaselineFileName)
	tmp, err := os.CreateTemp(stateDir, evidenceBaselineFileName+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
