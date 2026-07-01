package retrieval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/usage"
)

var (
	// compiled regexes used by looksLikeCodeQuery; moved out of the
	// function to avoid rebuilding on every invocation.
	codeKeywordRe   = regexp.MustCompile(`\b(func|class|package|import|return|if|for|while|switch|case)\b`)
	codePunctRe     = regexp.MustCompile(`[(){}\[\];]`)
	fileExtensionRe = regexp.MustCompile(`\.(js|ts|py|go|java|rb|cpp|c|cs|html|css|json|yaml|yml)\b`)
	timePrefixRe    = regexp.MustCompile(`^\s*\[?(\d{1,2}):(\d{2})(?::(\d{2}))?\]?\s*(.*)$`)
)

var defaultPathExcludes = []string{
	"**/.git/**",
	"**/node_modules/**",
	"**/.dir2mcp/**",
	"**/.env",
	"**/*.pem",
	"**/*.key",
	"**/id_rsa",
}

var defaultSecretPatternLiterals = []string{
	`AKIA[0-9A-Z]{16}`,
	`(?i)(?:aws(?:[_\s.]{0,20})?secret(?:[_\s.]*(?:access[_\s.]*)?key)?|secret[_\s.]*access[_\s.]*key)\s*[:=]\s*[0-9A-Za-z/+=]{20,}`,

	`(?i)(?:authorization\s*[:=]\s*bearer\s+|(?:access|id|refresh)_token\s*[:=]\s*)[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`,
	`(?i)token\s*[:=]\s*[A-Za-z0-9_.-]{20,}`,
	`sk_[a-z0-9]{32}|api_[A-Za-z0-9]{32}`,
}

const (
	defaultOverfetchMultiplier = 5
	maxOverfetchMultiplier     = 100

	defaultRAGSystemPrompt = "Answer the question using only the provided context.\nInclude concise source attributions in the form [rel_path]."
	defaultRAGMaxContext   = 20000
	maxRAGMaxContext       = 200000
)

// Service implements retrieval operations over embedded data.
// It holds necessary components like store, index, embedder and
// supports configurable overfetching during searches. OverfetchMultiplier
// determines how many candidates the underlying index returns per
// requested hit. A multiplier of 1 means no overfetch; the default is 5
// which generally provides enough buffer for downstream filtering.
// Callers may tune the value via SetOverfetchMultiplier, and it is validated
// to be at least 1 (higher values are capped at 100 to avoid runaway work).
//
// NOTE: adjusting the multiplier can help when heavy filtering is applied
// or when `k` is large; a smaller value reduces work at the cost of
// potentially missing some matches.
//
// WARNING: changing this value after the service has been used may
// affect the semantics of subsequent searches.
//
// The field is unexported to encourage use of the setter where
// validation takes place.
//
// See NewService for default initialization details.

type Service struct {
	store               model.Store
	textIndex           model.Index
	codeIndex           model.Index
	embedder            model.Embedder
	gen                 model.Generator
	logger              *log.Logger
	indexingStateFn     func() bool
	textModel           string
	codeModel           string
	overfetchMultiplier int
	ragSystemPrompt     string
	ragMaxContextChars  int
	metaMu              sync.RWMutex
	chunkByLabel        map[uint64]model.SearchHit
	chunkByIndex        map[string]map[uint64]model.SearchHit
	rootDir             string
	stateDir            string
	protocolVersion     string
	pathExcludes        []string
	// cached compiled regexps for exclude patterns; keys are normalized patterns
	excludeRegexps map[string]*regexp.Regexp
	secretPatterns []*regexp.Regexp
	// hybridEnabled toggles BM25+vector RRF fusion in Search. Defaults to
	// true; the engine can disable it via SetHybridEnabled when the operator
	// sets retrieval.hybrid.enabled=false.
	hybridEnabled bool
	// reranker, when set and rerankEnabled, re-scores the fused candidate
	// pool before truncation to k (see SPEC 9.1.1). Fail-open: any error
	// falls back to the pre-rerank order.
	reranker            model.Reranker
	rerankEnabled       bool
	rerankModel         string
	rerankCandidatePool int
	// crossFileDedupEnabled toggles retrieval-time cross-file de-duplication
	// (SPEC 9.2): when true, candidate hits whose source documents share an
	// identical content_hash collapse to one best-ranked survivor. Default
	// false (pass-through). Wired from config.DedupRetrieval at construction.
	crossFileDedupEnabled bool
	// groupKeyByRelPath maps a document rel_path to its content_hash (SPEC
	// 7.6), populated at startup from a model.DocumentHashLister. It is the
	// grouping key for cross-file dedup; an empty/absent value disables
	// grouping for that path (entries are never collapsed together).
	groupKeyByRelPath map[string]string
	// minScore is a server-side relevance floor (config retrieval.min_score):
	// hits whose final (authoritative) Score is strictly below it are dropped
	// from Search results, after scoring/fusion/rerank/dedup/truncation. It is
	// config-only (never an MCP tool parameter). Default 0 ⇒ disabled
	// (pass-through). Wired from config.RetrievalMinScore at construction.
	minScore float64
	// genModel is the resolved chat/generation model name, used only to label
	// and price the generate stage in per-query metrics (issue #327). Empty
	// when no generator is configured. Never affects retrieval behavior.
	genModel string
	// priceTable maps model names to approximate USD prices for the
	// query_metrics event (issue #327). nil ⇒ cost is always omitted.
	priceTable *usage.PriceTable
	// carbon supplies the OPT-IN, approximate energy/CO2e estimate for the
	// query_metrics event (issue #328). nil/disabled ⇒ no energy/CO2e fields.
	carbon *usage.CarbonModel
	// metricsEmit, when set, receives one structured query_metrics event per
	// Ask/Search (issue #327). nil ⇒ metrics collection is skipped entirely
	// (zero overhead). It never alters tool results.
	metricsEmit func(level, event string, data interface{})
	// recencyHalfLife is an opt-in server-side time-decay half-life (config
	// retrieval.recency_half_life): when > 0, each hit's final authoritative
	// Score is multiplied by exp(-ln2 * age / half_life) where age is the hit's
	// source document mtime relative to a fixed "now" captured at query start.
	// Newer content therefore ranks higher; a hit with no resolvable date is
	// neither boosted nor penalized. Applied after scoring/fusion/rerank and just
	// before the min_score floor. Config-only (never an MCP tool parameter).
	// Default 0 ⇒ disabled (pass-through). Wired from
	// config.RetrievalRecencyHalfLife at construction.
	recencyHalfLife time.Duration
	// nowFn returns the reference instant for recency decay; overridable in
	// tests for determinism. Defaults to time.Now.
	nowFn func() time.Time
	// compressor applies evidence-guided context compression (issue #335) to the
	// per-hit text assembled into the RAG prompt — NEVER to the returned hits or
	// citations. Default disabled ⇒ raw snippets are sent unchanged. Wired from
	// config.ContextCompression* via SetContextCompression at construction.
	compressor contextCompressor
	// adaptiveEnabled toggles the opt-in, training-free retrieval gate
	// (config retrieval.adaptive.enabled). Default false ⇒ Ask uses today's
	// fixed-k path. When true, Ask consults adaptiveGate to decide whether to
	// retrieve and what k to use, bounded by [adaptiveKMin, adaptiveKMax].
	// Config-only — never an MCP tool parameter. Wired via SetAdaptiveRetrieval.
	adaptiveEnabled bool
	// adaptiveKMin / adaptiveKMax bound the dynamic k chosen by the gate. They
	// are sanitized at wiring time (SetAdaptiveRetrieval) to 1 <= min <= max so
	// the gate never emits an out-of-range k. Ignored when adaptiveEnabled is
	// false.
	adaptiveKMin int
	adaptiveKMax int
	// mmrEnabled toggles Maximal Marginal Relevance diversity re-ordering
	// (config retrieval.mmr.enabled, issue #340). When true, the final candidate
	// pool is re-ordered before truncation to k to trade some relevance for
	// coverage/diversity. Default false ⇒ pass-through (order unchanged). Wired
	// from config.RetrievalMMREnabled at construction.
	mmrEnabled bool
	// mmrLambda is the MMR relevance-vs-diversity trade-off in [0,1] (config
	// retrieval.mmr.lambda): 1 = pure relevance, 0 = pure diversity. Only
	// consulted when mmrEnabled. Wired from config.RetrievalMMRLambda.
	mmrLambda float64
	// hydeEnabled toggles the opt-in HyDE (Hypothetical Document Embeddings)
	// query transform (config retrieval.hyde.enabled). When true and a
	// generator is configured, Search generates a short hypothetical answer to
	// the query, embeds it, and retrieves with that text. Config-only (never an
	// MCP tool parameter). Default false ⇒ unchanged behavior. Wired from
	// config.RetrievalHyDEEnabled at construction.
	hydeEnabled bool
	// hydeMode selects how the HyDE-variant hits combine with the raw-query hits
	// ("fuse" = RRF-fuse the two; "replace" = use the hypothetical-document hits
	// alone). Default "fuse". Wired from config.RetrievalHyDEMode.
	hydeMode string
	// crossLingualEnabled toggles server-side cross-lingual query expansion
	// (#325): when true and crossLingualTranslator is set, Search translates the
	// query into each resolved target language, retrieves per variant, and
	// RRF-fuses the per-language result sets. Default false ⇒ unchanged behavior.
	// Wired from config.CrossLingualEnabled at construction.
	crossLingualEnabled bool
	// crossLingualTargetLangs is the configured target-language list
	// (config retrieval.cross_lingual.target_langs). Empty, or the "auto"
	// sentinel, resolves to the corpus's detected languages via
	// crossLingualCorpusLangsFn at query time. Tags are lower-cased.
	crossLingualTargetLangs []string
	// crossLingualTranslator is the reusable translate primitive (the chat
	// Generator, SPEC §8.6.2). When nil, cross-lingual expansion is inert even if
	// enabled (it degrades to the un-expanded search). Wired from the same
	// translatorFromConfig binding the ingest pipeline uses.
	crossLingualTranslator model.Generator
	// crossLingualCorpusLangsFn returns the corpus's detected languages (#267)
	// for the "auto" target resolution. nil ⇒ auto resolves to no targets (the
	// expansion is a no-op), so an explicit list is required to expand on a store
	// that cannot enumerate languages.
	crossLingualCorpusLangsFn func() []string
}

// compile-time assertion that Service implements model.Retriever.  This
// will fail to compile if the interface changes without updating this type.
var _ model.Retriever = (*Service)(nil)

func NewService(store model.Store, index model.Index, embedder model.Embedder, gen model.Generator) *Service {
	compiledPatterns := make([]*regexp.Regexp, 0, len(defaultSecretPatternLiterals))
	for _, pattern := range defaultSecretPatternLiterals {
		re, err := regexp.Compile(pattern)
		if err != nil {
			panic(fmt.Errorf("invalid default secret pattern %q: %w", pattern, err))
		}
		compiledPatterns = append(compiledPatterns, re)
	}
	// overfetchMultiplier defaults to 5; callers may override it with
	// SetOverfetchMultiplier to tune for their workload.  Values less than
	// 1 are silently bumped to 1, and values above 100 are capped.
	return &Service{
		store:               store,
		textIndex:           index,
		codeIndex:           index,
		embedder:            embedder,
		gen:                 gen,
		logger:              log.Default(),
		textModel:           "mistral-embed",
		codeModel:           "codestral-embed",
		overfetchMultiplier: defaultOverfetchMultiplier,
		ragSystemPrompt:     defaultRAGSystemPrompt,
		ragMaxContextChars:  defaultRAGMaxContext,
		chunkByLabel:        make(map[uint64]model.SearchHit),
		chunkByIndex: map[string]map[uint64]model.SearchHit{
			"text": make(map[uint64]model.SearchHit),
			"code": make(map[uint64]model.SearchHit),
		},
		rootDir:             ".",
		stateDir:            filepath.Join(".", ".dir2mcp"),
		protocolVersion:     "2025-11-25",
		excludeRegexps:      make(map[string]*regexp.Regexp),
		pathExcludes:        append([]string(nil), defaultPathExcludes...),
		secretPatterns:      compiledPatterns,
		hybridEnabled:       true,
		rerankCandidatePool: defaultRerankCandidatePool,
		hydeMode:            hydeModeFuse,
	}
}

// SetHybridEnabled toggles BM25+vector hybrid retrieval. The engine wires this
// from config.RetrievalHybridEnabled at construction time.
func (s *Service) SetHybridEnabled(enabled bool) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.hybridEnabled = enabled
}

// SetReranker wires an optional rerank provider. modelName is the
// provider model (empty = provider default); pool caps how many fused
// candidates are re-scored. pool <= 0 keeps the currently-configured
// value (default-initialized in NewService) so callers can swap the
// reranker without resetting an operator-tuned pool. Mirrors how the
// engine wires SetHybridEnabled from config.
func (s *Service) SetReranker(r model.Reranker, modelName string, pool int) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.reranker = r
	s.rerankModel = strings.TrimSpace(modelName)
	if pool > 0 {
		s.rerankCandidatePool = pool
	}
}

// SetRerankEnabled toggles the optional rerank stage. The engine wires
// this from config.RerankEnabled at construction time.
func (s *Service) SetRerankEnabled(enabled bool) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.rerankEnabled = enabled
}

// SetGenerationModel records the resolved chat/generation model name used to
// label and price the generate stage in per-query metrics (issue #327). It has
// no effect on retrieval or generation behavior.
func (s *Service) SetGenerationModel(modelName string) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.genModel = strings.TrimSpace(modelName)
}

// SetMetricsEmitter wires per-query cost/latency observability (issue #327).
// emit receives a single structured `query_metrics` event per Ask/Search;
// prices maps model names to USD costs (nil ⇒ cost omitted). Passing a nil
// emit disables metrics collection entirely. Metrics never change tool results.
func (s *Service) SetMetricsEmitter(emit func(level, event string, data interface{}), prices *usage.PriceTable) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.metricsEmit = emit
	s.priceTable = prices
}

// SetCarbonModel wires the OPT-IN energy/CO2e estimate (issue #328) onto the
// query_metrics event. A nil or disabled model omits all energy/CO2e fields,
// leaving cost/latency unchanged. Observability only; never affects results.
func (s *Service) SetCarbonModel(carbon *usage.CarbonModel) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.carbon = carbon
}

// metricsEnabled reports whether a metrics emitter is wired.
func (s *Service) metricsEnabled() bool {
	s.metaMu.RLock()
	defer s.metaMu.RUnlock()
	return s.metricsEmit != nil
}

// emitQueryMetrics builds a query_metrics event from the per-query sink and
// surfaces it via the structured emitter plus a concise log line. It records
// per-stage latency (always) and token usage + cost (where the provider
// reported usage and the model is priced). It records counts/costs/latency
// only — never prompts, documents, or keys. A nil sink or unset emitter is a
// no-op. This is observability only; it never affects tool results (#327).
func (s *Service) emitQueryMetrics(op string, sink *usage.Sink, total time.Duration) {
	if sink == nil {
		return
	}
	s.metaMu.RLock()
	emit := s.metricsEmit
	prices := s.priceTable
	carbon := s.carbon
	textModel := s.textModel
	codeModel := s.codeModel
	rerankModel := s.rerankModel
	genModel := s.genModel
	s.metaMu.RUnlock()
	if emit == nil {
		return
	}

	qm := usage.NewQueryMetricsWithCarbon(op, prices, carbon)

	// Embed: prefer the text model label; fall back to code model. Embedding is
	// symmetric across the two for pricing purposes here.
	embedModel := textModel
	if embedModel == "" {
		embedModel = codeModel
	}
	if lat := sink.Latency(usage.StageEmbed); lat > 0 || hasStageUsage(sink, usage.StageEmbed) {
		u, reported := sink.Stage(usage.StageEmbed)
		qm.RecordStage(usage.StageEmbed, embedModel, lat, u, reported)
	}
	if lat := sink.Latency(usage.StageRerank); lat > 0 {
		u, reported := sink.Stage(usage.StageRerank)
		qm.RecordStage(usage.StageRerank, rerankModel, lat, u, reported)
	}
	if lat := sink.Latency(usage.StageGenerate); lat > 0 || hasStageUsage(sink, usage.StageGenerate) {
		u, reported := sink.Stage(usage.StageGenerate)
		qm.RecordStage(usage.StageGenerate, genModel, lat, u, reported)
	}
	qm.SetTotalLatency(total)

	emit("info", "query_metrics", qm.Event())
	s.logf("%s", qm.LogLine())
}

// hasStageUsage reports whether the sink recorded provider usage for a stage.
func hasStageUsage(sink *usage.Sink, stage usage.Stage) bool {
	_, ok := sink.Stage(stage)
	return ok
}

// SetCrossFileDedupEnabled toggles retrieval-time cross-file de-duplication
// (SPEC 9.2). The engine wires this from config.DedupRetrieval at construction
// time. Default off ⇒ search returns the pre-dedup candidate set unchanged.
func (s *Service) SetCrossFileDedupEnabled(enabled bool) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.crossFileDedupEnabled = enabled
}

// SetMinScore wires the server-side relevance floor (config retrieval.min_score).
// Hits whose final authoritative Score is strictly below floor are dropped from
// Search results, after scoring/fusion/rerank and after dedup/truncation. A
// floor <= 0 disables the cutoff (pass-through). The engine wires this from
// config.RetrievalMinScore at construction time, mirroring SetCrossFileDedupEnabled.
func (s *Service) SetMinScore(floor float64) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.minScore = floor
}

// SetRecencyHalfLife wires the opt-in server-side time-decay (config
// retrieval.recency_half_life). When halfLife > 0, each hit's final
// authoritative Score is multiplied by exp(-ln2 * age / half_life) using the
// hit's source document mtime relative to a fixed "now" captured at query
// start, after scoring/fusion/rerank and just before the min_score floor. A
// halfLife <= 0 disables the decay (pass-through). The engine wires this from
// config.RetrievalRecencyHalfLife at construction time, mirroring SetMinScore.
func (s *Service) SetRecencyHalfLife(halfLife time.Duration) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.recencyHalfLife = halfLife
}

// SetNowFunc overrides the reference clock used by recency decay. Intended for
// deterministic tests; passing nil restores time.Now.
func (s *Service) SetNowFunc(fn func() time.Time) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.nowFn = fn
}

// now returns the reference instant for recency decay, defaulting to time.Now
// when no override is installed.
func (s *Service) now() time.Time {
	s.metaMu.RLock()
	fn := s.nowFn
	s.metaMu.RUnlock()
	if fn == nil {
		return time.Now()
	}
	return fn()
}

// SetContextCompression wires evidence-guided context compression (issue #335).
// When enabled, the Ask path compresses the per-hit text sent to the generator —
// keeping query-relevant, non-redundant sentences up to targetRatio of each
// hit's original length — to cut prompt tokens and fit more evidence. It affects
// ONLY the model-facing prompt: returned hits, snippets, and citations are never
// altered, preserving citation fidelity. A targetRatio outside (0,1] selects the
// built-in default. Disabled ⇒ raw snippets are sent unchanged (pass-through).
// The engine wires this from config.ContextCompression* at construction time,
// mirroring SetMinScore.
func (s *Service) SetContextCompression(enabled bool, targetRatio float64) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.compressor = newContextCompressor(enabled, targetRatio)
}

// adaptiveKFloor / adaptiveKCeil are the built-in fallbacks used when the
// operator leaves a bound at 0 ("use the default"). They give the gate a sane
// window around the typical rag.k_default without requiring explicit tuning.
const (
	adaptiveKFloor = 4
	adaptiveKCeil  = 30
)

// SetAdaptiveRetrieval wires the opt-in adaptive retrieval gate (config
// retrieval.adaptive.*). When enabled is false the service keeps today's
// fixed-k behavior and kMin/kMax are ignored. When enabled, kMin/kMax bound the
// dynamic k the gate may choose; a bound <= 0 falls back to the built-in
// default (adaptiveKFloor / adaptiveKCeil), and an inverted window is corrected
// to kMax = kMin so the gate always sees 1 <= min <= max. The engine wires this
// from config at construction time, mirroring SetMinScore.
func (s *Service) SetAdaptiveRetrieval(enabled bool, kMin, kMax int) {
	if kMin <= 0 {
		kMin = adaptiveKFloor
	}
	if kMax <= 0 {
		kMax = adaptiveKCeil
	}
	if kMin < 1 {
		kMin = 1
	}
	if kMax < kMin {
		kMax = kMin
	}
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.adaptiveEnabled = enabled
	s.adaptiveKMin = kMin
	s.adaptiveKMax = kMax
}

// SetMMR wires the optional Maximal Marginal Relevance diversity re-ordering
// (config retrieval.mmr.enabled / retrieval.mmr.lambda, issue #340). When
// enabled, the final candidate pool is re-ordered before truncation to k to
// trade some relevance for coverage/diversity. lambda is the relevance-vs-
// diversity trade-off in [0,1]; values outside the range are clamped (the
// config layer already rejects them, this is a defensive guard). Disabled ⇒
// candidate order is unchanged (pass-through). The engine wires this from
// config at construction time, mirroring SetMinScore.
func (s *Service) SetMMR(enabled bool, lambda float64) {
	if lambda < 0 {
		lambda = 0
	}
	if lambda > 1 {
		lambda = 1
	}
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.mmrEnabled = enabled
	s.mmrLambda = lambda
}

// SetHyDE wires the opt-in HyDE (Hypothetical Document Embeddings) query
// transform (config retrieval.hyde.enabled / retrieval.hyde.mode). When enabled
// and a generator is configured, Search generates a short hypothetical answer to
// the query, embeds it, and retrieves with that text — fused with the raw-query
// results (mode "fuse", the default) or used alone (mode "replace"). An empty or
// unrecognized mode normalizes to "fuse". A generation failure degrades
// gracefully to the raw query (never fatal). Config-only; the engine wires this
// from config at construction time, mirroring SetMinScore.
func (s *Service) SetHyDE(enabled bool, mode string) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode != hydeModeReplace {
		mode = hydeModeFuse
	}
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.hydeEnabled = enabled
	s.hydeMode = mode
}

// SetCrossLingual wires server-side cross-lingual query expansion (#325). When
// enabled and translator is non-nil, Search translates the query into each
// resolved target language, retrieves per variant, and RRF-fuses the result
// sets so a query in one language surfaces content in the others. targetLangs is
// the configured list (config retrieval.cross_lingual.target_langs); an empty
// list or the "auto" sentinel resolves to the corpus's detected languages via
// the provider registered with SetCorpusLanguagesProvider. Tags are lower-cased
// and de-duplicated. Passing enabled=false or a nil translator leaves search
// behavior unchanged. The engine wires this from config.CrossLingualEnabled /
// config.CrossLingualTargetLangs at construction, mirroring SetMinScore.
func (s *Service) SetCrossLingual(enabled bool, targetLangs []string, translator model.Generator) {
	norm := make([]string, 0, len(targetLangs))
	seen := make(map[string]struct{}, len(targetLangs))
	for _, l := range targetLangs {
		t := strings.ToLower(strings.TrimSpace(l))
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		norm = append(norm, t)
	}
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.crossLingualEnabled = enabled
	s.crossLingualTargetLangs = norm
	s.crossLingualTranslator = translator
}

// SetCorpusLanguagesProvider registers a callback returning the corpus's
// detected languages (#267), used to resolve the "auto" cross-lingual target
// set. Passing nil clears it (auto then resolves to no targets). The callback is
// invoked at query time so it always reflects the live corpus.
func (s *Service) SetCorpusLanguagesProvider(fn func() []string) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.crossLingualCorpusLangsFn = fn
}

// SetDocumentHashes installs the rel_path → content_hash map used to group
// candidate hits for cross-file dedup (SPEC 9.2). Entries with an empty
// content_hash are ignored (never grouped together). The map is copied so the
// caller may reuse its slice/map. Passing nil clears the map (pass-through).
func (s *Service) SetDocumentHashes(hashes []model.DocumentHash) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	if len(hashes) == 0 {
		s.groupKeyByRelPath = nil
		return
	}
	m := make(map[string]string, len(hashes))
	for _, h := range hashes {
		if strings.TrimSpace(h.ContentHash) == "" {
			continue
		}
		m[h.RelPath] = h.ContentHash
	}
	s.groupKeyByRelPath = m
}

func (s *Service) SetLogger(l *log.Logger) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	if l == nil {
		s.logger = log.Default()
		return
	}
	s.logger = l
}

// SetIndexingCompleteProvider sets a callback used to populate AskResult.IndexingComplete.
// The callback should return true when indexing is complete.
func (s *Service) SetIndexingCompleteProvider(fn func() bool) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.indexingStateFn = fn
}

func (s *Service) logf(format string, args ...interface{}) {
	s.metaMu.RLock()
	logger := s.logger
	s.metaMu.RUnlock()
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf(format, args...)
}

// truncateQuestion returns a shortened representation of the question
// suitable for logging. If the original string is longer than 64
// characters it is trimmed and an ellipsis appended.  Empty input yields
// a placeholder so callers don't accidentally log an empty quoted string.
func truncateQuestion(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return "<empty>"
	}
	const max = 64
	r := []rune(q)
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}

// SetQueryEmbeddingModel records the text-axis model used to embed the QUERY.
// It MUST resolve to the same model the corpus was embedded with — sending a
// query through a different embedder/model than the index yields a vector in a
// foreign space and garbage hits.
//
// An empty modelName is recorded as empty (clearing the historical
// "mistral-embed" default), so the active provider adapter applies its OWN
// default — the very same default the embed worker used for the corpus when its
// profile left the model blank (modelForKind, issue #396). Previously this was
// a no-op on empty, so switching the embed provider to OpenAI/Cohere left the
// query pinned to "mistral-embed" and every search hit a wrong-provider model
// error while indexing had succeeded. The resolved embed profile always
// supplies a concrete model for providers that define one (e.g. Mistral), so
// this only clears the default for providers that intentionally defer to their
// adapter.
func (s *Service) SetQueryEmbeddingModel(modelName string) {
	s.metaMu.Lock()
	s.textModel = strings.TrimSpace(modelName)
	s.metaMu.Unlock()
}

// SetCodeEmbeddingModel records the code-axis query embed model. Empty clears
// the historical "codestral-embed" default so the active provider adapter
// applies its own default (issue #396); see SetQueryEmbeddingModel.
func (s *Service) SetCodeEmbeddingModel(modelName string) {
	s.metaMu.Lock()
	s.codeModel = strings.TrimSpace(modelName)
	s.metaMu.Unlock()
}

func (s *Service) SetCodeIndex(index model.Index) {
	if index == nil {
		return
	}
	s.metaMu.Lock()
	s.codeIndex = index
	s.metaMu.Unlock()
}

func (s *Service) SetRootDir(root string) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = "."
	}
	s.metaMu.Lock()
	s.rootDir = root
	s.metaMu.Unlock()
}

func (s *Service) SetStateDir(stateDir string) {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		stateDir = filepath.Join(".", ".dir2mcp")
	}
	s.metaMu.Lock()
	s.stateDir = stateDir
	s.metaMu.Unlock()
}

func (s *Service) SetProtocolVersion(protocolVersion string) {
	protocolVersion = strings.TrimSpace(protocolVersion)
	if protocolVersion == "" {
		protocolVersion = "2025-11-25"
	}
	s.metaMu.Lock()
	s.protocolVersion = protocolVersion
	s.metaMu.Unlock()
}

func (s *Service) SetPathExcludes(patterns []string) {
	// merge defaults with caller-provided patterns so that hardcoded
	// security exclusions (.git, node_modules, .env, key/pem files) are
	// never silently dropped when the caller supplies custom patterns.
	merged := make([]string, 0, len(defaultPathExcludes)+len(patterns))
	merged = append(merged, defaultPathExcludes...)
	merged = append(merged, patterns...)
	compiled := make(map[string]*regexp.Regexp, len(merged))
	for _, pat := range merged {
		norm := strings.TrimSpace(filepath.ToSlash(pat))
		if norm == "" {
			continue
		}
		re, err := regexp.Compile(globToRegexp(norm))
		if err != nil {
			// ignore invalid pattern, it'll simply never match
			continue
		}
		compiled[norm] = re
	}

	s.metaMu.Lock()
	// record the merged set of exclusions (defaults + caller patterns) in
	// s.pathExcludes. this no longer reflects just the caller-provided
	// values but the full list used for matching; compiled regexps are
	// still held in s.excludeRegexps and matchExcludePattern will normalize
	// and consult the merged patterns when performing lookups.
	s.pathExcludes = merged
	s.excludeRegexps = compiled
	s.metaMu.Unlock()
}

func (s *Service) SetSecretPatterns(patterns []string) error {
	// start with compiled defaults so that baseline secret-detection
	// patterns (AWS keys, JWT tokens, etc.) are never dropped when callers
	// add custom patterns.
	compiled := make([]*regexp.Regexp, 0, len(defaultSecretPatternLiterals)+len(patterns))
	for _, pattern := range defaultSecretPatternLiterals {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return fmt.Errorf("failed to compile default secret pattern %q: %w", pattern, err)
		}
		compiled = append(compiled, re)
	}
	for _, pattern := range patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return err
		}
		compiled = append(compiled, re)
	}
	s.metaMu.Lock()
	s.secretPatterns = compiled
	s.metaMu.Unlock()
	return nil
}

func (s *Service) SetChunkMetadata(label uint64, metadata model.SearchHit) {
	s.metaMu.Lock()
	s.chunkByLabel[label] = metadata
	s.chunkByIndex["text"][label] = metadata
	s.chunkByIndex["code"][label] = metadata
	s.metaMu.Unlock()
}

// EvictDocument removes all in-memory chunk metadata for one document.
func (s *Service) EvictDocument(relPath string) {
	s.EvictDocuments([]string{relPath})
}

// EvictDocuments removes all in-memory chunk metadata for the given
// documents. It is called when documents are tombstoned in the store so that
// their chunks no longer appear in search results for the remainder of the
// server session. The HNSW vector index has no delete support, so evicted
// labels will still be returned by the ANN search, but matchFilters will
// discard them because searchHitForLabel falls back to a stub with an empty
// RelPath.
func (s *Service) EvictDocuments(relPaths []string) {
	if len(relPaths) == 0 {
		return
	}
	normalized := make(map[string]struct{}, len(relPaths))
	for _, relPath := range relPaths {
		norm := strings.TrimSpace(filepath.ToSlash(relPath))
		if norm == "" {
			continue
		}
		normalized[norm] = struct{}{}
	}
	if len(normalized) == 0 {
		return
	}

	// First, scan under a read lock to find labels to delete without blocking
	// concurrent readers for the duration of the O(totalChunks) scan.
	s.metaMu.RLock()
	var labelsToDelete []uint64
	for label, hit := range s.chunkByLabel {
		if _, ok := normalized[strings.TrimSpace(filepath.ToSlash(hit.RelPath))]; ok {
			labelsToDelete = append(labelsToDelete, label)
		}
	}
	s.metaMu.RUnlock()

	if len(labelsToDelete) == 0 {
		return
	}

	// Now take the write lock only for the actual deletions.
	s.metaMu.Lock()
	for _, label := range labelsToDelete {
		delete(s.chunkByLabel, label)
		for _, byIndex := range s.chunkByIndex {
			delete(byIndex, label)
		}
	}
	s.metaMu.Unlock()
}

func (s *Service) SetChunkMetadataForIndex(indexName string, label uint64, metadata model.SearchHit) {
	kind := strings.ToLower(strings.TrimSpace(indexName))
	if kind != "text" && kind != "code" {
		s.SetChunkMetadata(label, metadata)
		return
	}

	s.metaMu.Lock()
	s.chunkByLabel[label] = metadata
	s.chunkByIndex[kind][label] = metadata
	s.metaMu.Unlock()
}

func (s *Service) SetRAGSystemPrompt(prompt string) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		prompt = defaultRAGSystemPrompt
	}
	s.metaMu.Lock()
	s.ragSystemPrompt = prompt
	s.metaMu.Unlock()
}

func (s *Service) SetMaxContextChars(maxChars int) {
	if maxChars <= 0 {
		maxChars = defaultRAGMaxContext
	}
	if maxChars > maxRAGMaxContext {
		maxChars = maxRAGMaxContext
	}
	s.metaMu.Lock()
	s.ragMaxContextChars = maxChars
	s.metaMu.Unlock()
}

// SetOversampleFactor changes retrieval fanout used for index search.
func (s *Service) SetOversampleFactor(factor int) {
	if factor < 1 {
		factor = 1
	}
	if factor > maxOverfetchMultiplier {
		factor = maxOverfetchMultiplier
	}
	s.metaMu.Lock()
	s.overfetchMultiplier = factor
	s.metaMu.Unlock()
}

// SetOverfetchMultiplier changes the multiplier used when querying the
// underlying vector index.  The service will ask for `k * multiplier`
// neighbors for a request that originally asked for `k` hits.  Values
// lower than 1 are bumped to 1 (no overfetch) and values greater than
// 100 are capped to prevent unreasonable work.  This method is safe to
// call concurrently.
func (s *Service) SetOverfetchMultiplier(m int) {
	s.SetOversampleFactor(m)
}

func (s *Service) Search(ctx context.Context, query model.SearchQuery) ([]model.SearchHit, error) {
	// When metrics are enabled, attach a per-query usage sink and emit one
	// query_metrics event for the search. When Ask calls Search internally it
	// installs its own sink first; we detect that and skip double-emitting so a
	// single `ask` produces exactly one event (issue #327).
	if s.metricsEnabled() && usage.SinkFrom(ctx) == nil {
		sink := usage.NewSink()
		ctx = usage.WithSink(ctx, sink)
		start := time.Now()
		hits, err := s.search(ctx, query)
		s.emitQueryMetrics("search", sink, time.Since(start))
		return hits, err
	}
	return s.search(ctx, query)
}

func (s *Service) search(ctx context.Context, query model.SearchQuery) ([]model.SearchHit, error) {
	k := query.K
	if k <= 0 {
		k = 15
	}

	// Cross-lingual query expansion (#325) wraps the HyDE/per-mode pipeline: when
	// active it runs that pipeline once per query-language variant and RRF-fuses
	// the result sets; when inactive it reduces to a single searchWithHyDE call,
	// so the un-expanded path is unchanged.
	hits, err := s.searchExpanded(ctx, query, k)
	if err != nil {
		return nil, err
	}
	// Apply the opt-in recency time-decay just BEFORE the relevance floor: it
	// re-scores each hit by its source-document age, so the floor compares the
	// decayed score and newer content survives a tie. Config-only; default 0 ⇒
	// pass-through (no allocation, no lookups).
	hits = s.applyRecencyDecay(ctx, hits)
	// Apply the server-side relevance floor LAST: after scoring/fusion/rerank,
	// the optional HyDE fusion, any cross-lingual fusion, their dedup/truncation,
	// and the recency decay, using each hit's final authoritative Score.
	// Config-only; default 0 ⇒ pass-through.
	return s.applyMinScoreFloor(hits), nil
}

// searchByMode runs the index-mode dispatch (text/code/both/auto) for one query
// text, returning up to k ranked hits. It is the shared retrieval primitive used
// by the raw query, the HyDE hypothetical-document variant (#333), and each
// cross-lingual translated variant (#325), so all go through identical
// fusion/rerank/dedup logic. allowRerank is forwarded to the single-index path
// (the "both" path reranks internally on its merged pool regardless).
func (s *Service) searchByMode(ctx context.Context, queryText string, k int, query model.SearchQuery, allowRerank bool) ([]model.SearchHit, error) {
	s.metaMu.RLock()
	textModel := s.textModel
	codeModel := s.codeModel
	textIndex := s.textIndex
	codeIndex := s.codeIndex
	s.metaMu.RUnlock()

	mode := strings.ToLower(strings.TrimSpace(query.Index))
	if mode == "" {
		mode = "auto"
	}
	switch mode {
	case "text":
		return s.searchSingleIndex(ctx, queryText, k, textModel, textIndex, "text", query, allowRerank)
	case "code":
		return s.searchSingleIndex(ctx, queryText, k, codeModel, codeIndex, "code", query, allowRerank)
	case "both":
		return s.searchBothIndices(ctx, queryText, k, textModel, codeModel, textIndex, codeIndex, query)
	case "auto":
		if looksLikeCodeQuery(queryText) {
			return s.searchSingleIndex(ctx, queryText, k, codeModel, codeIndex, "code", query, allowRerank)
		}
		return s.searchSingleIndex(ctx, queryText, k, textModel, textIndex, "text", query, allowRerank)
	default:
		return s.searchSingleIndex(ctx, queryText, k, textModel, textIndex, "text", query, allowRerank)
	}
}

// searchWithHyDE runs retrieval for the query, applying the opt-in HyDE
// (Hypothetical Document Embeddings) transform when enabled. With HyDE off it is
// exactly searchByMode for the raw query (unchanged behavior). With HyDE on it
// generates a short hypothetical answer, retrieves with that text, and either
// RRF-fuses those hits with the raw-query hits (mode "fuse") or returns them
// alone (mode "replace"). Generation failures, an empty hypothesis, or a missing
// generator degrade gracefully to the raw-query results — HyDE is an
// optimization, never a hard dependency.
func (s *Service) searchWithHyDE(ctx context.Context, query model.SearchQuery, k int) ([]model.SearchHit, error) {
	s.metaMu.RLock()
	enabled := s.hydeEnabled
	mode := s.hydeMode
	gen := s.gen
	s.metaMu.RUnlock()

	if !enabled || gen == nil {
		return s.searchByMode(ctx, query.Query, k, query, true)
	}

	hypothesis := s.generateHyDEDocument(ctx, gen, query.Query)
	if hypothesis == "" {
		// Graceful fallback: generation failed or produced nothing usable.
		return s.searchByMode(ctx, query.Query, k, query, true)
	}

	if mode == hydeModeReplace {
		// Use the hypothetical-document hits alone.
		return s.searchByMode(ctx, hypothesis, k, query, true)
	}

	// Mode "fuse": retrieve both variants with rerank deferred so RRF fuses the
	// raw candidate orderings, then rerank/truncate the fused pool once.
	rawHits, err := s.searchByMode(ctx, query.Query, hybridCandidatePoolSize, query, false)
	if err != nil {
		return nil, err
	}
	hydeHits, err := s.searchByMode(ctx, hypothesis, hybridCandidatePoolSize, query, false)
	if err != nil {
		// A HyDE-variant retrieval error must not fail the whole request: fall
		// back to the raw-query hits we already have.
		s.logf("hyde: variant retrieval failed, falling back to raw query: %v", err)
		return s.rerankPool(ctx, query.Query, rawHits, k), nil
	}
	fused := fuseRRF(rawHits, hydeHits, hybridCandidatePoolSize)
	return s.rerankPool(ctx, query.Query, fused, k), nil
}

// generateHyDEDocument asks the generator for a concise hypothetical answer to
// the query, used as the retrieval text for the HyDE transform. It returns the
// trimmed generated text, or "" when generation fails or yields nothing — the
// caller then falls back to the raw query. The question is not logged verbatim
// (it may carry sensitive data); only a truncated form is logged on error.
func (s *Service) generateHyDEDocument(ctx context.Context, gen model.Generator, queryText string) string {
	queryText = strings.TrimSpace(queryText)
	if queryText == "" {
		return ""
	}
	prompt := buildHyDEPrompt(queryText)
	generated, err := gen.Generate(ctx, prompt)
	if err != nil {
		s.logf("hyde: generation failed, falling back to raw query: %v", err)
		return ""
	}
	return truncateHyDEAnswer(generated)
}

// applyRecencyDecay multiplies each hit's final authoritative Score by an
// exponential time-decay exp(-ln2 * age / half_life), where age is the hit's
// source document mtime relative to a fixed "now" captured once at the start of
// this call (so a single Search is deterministic regardless of wall-clock drift
// across hits). A half-life <= 0 disables the decay and returns hits unchanged
// (pass-through), preserving slice identity so callers see no allocation when
// unconfigured. A hit whose source date cannot be resolved (unknown rel_path,
// store error, or mtime <= 0) is left untouched — never boosted, never
// penalized. A future-dated hit (age < 0) is clamped to age 0 (factor 1) so a
// skewed mtime can never amplify a score above its undecayed value. Because the
// re-scoring can reorder candidates, hits are re-sorted best-first with a
// stable, deterministic tiebreak (rel_path then chunk id) so output ordering is
// reproducible.
func (s *Service) applyRecencyDecay(ctx context.Context, hits []model.SearchHit) []model.SearchHit {
	s.metaMu.RLock()
	halfLife := s.recencyHalfLife
	store := s.store
	s.metaMu.RUnlock()
	if halfLife <= 0 || len(hits) == 0 || store == nil {
		return hits
	}
	now := s.now()
	halfLifeSec := halfLife.Seconds()
	// Cache document lookups by rel_path: many hits can share one source
	// document, and a single Search should stat each document at most once.
	mtimeByPath := make(map[string]int64, len(hits))
	resolveMtime := func(relPath string) int64 {
		if relPath == "" {
			return 0
		}
		if mt, ok := mtimeByPath[relPath]; ok {
			return mt
		}
		var mt int64
		if doc, err := store.GetDocumentByPath(ctx, relPath); err == nil {
			mt = doc.MTimeUnix
		}
		mtimeByPath[relPath] = mt
		return mt
	}
	out := make([]model.SearchHit, len(hits))
	copy(out, hits)
	for i := range out {
		mtime := resolveMtime(out[i].RelPath)
		if mtime <= 0 {
			continue
		}
		ageSec := now.Sub(time.Unix(mtime, 0)).Seconds()
		if ageSec <= 0 {
			continue
		}
		factor := math.Exp(-math.Ln2 * ageSec / halfLifeSec)
		out[i].Score *= factor
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].RelPath != out[j].RelPath {
			return out[i].RelPath < out[j].RelPath
		}
		return out[i].ChunkID < out[j].ChunkID
	})
	return out
}

// applyMinScoreFloor drops hits whose final authoritative Score is strictly
// below the configured relevance floor (config retrieval.min_score). A floor
// <= 0 disables the cutoff and returns hits unchanged (pass-through), preserving
// the slice identity so callers see no allocation when unconfigured. Order is
// preserved. This runs as the very last retrieval step so the comparison uses
// the post-rerank score (or, when rerank is off, the fused score).
func (s *Service) applyMinScoreFloor(hits []model.SearchHit) []model.SearchHit {
	s.metaMu.RLock()
	floor := s.minScore
	s.metaMu.RUnlock()
	if floor <= 0 || len(hits) == 0 {
		return hits
	}
	out := make([]model.SearchHit, 0, len(hits))
	for _, h := range hits {
		if h.Score < floor {
			continue
		}
		out = append(out, h)
	}
	return out
}

func (s *Service) Ask(ctx context.Context, question string, query model.SearchQuery) (model.AskResult, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return model.AskResult{}, errors.New("question is required")
	}

	if strings.TrimSpace(query.Query) == "" {
		query.Query = question
	}
	if query.K <= 0 {
		query.K = 15
	}

	// Install a per-query usage sink so embed/rerank/generate token usage and
	// latency for this ask are accumulated and emitted as a single
	// query_metrics event. Calling s.search (not s.Search) keeps the sink
	// owned here, so the nested search does not emit its own event (#327).
	var (
		sink     *usage.Sink
		askStart time.Time
	)
	if s.metricsEnabled() {
		sink = usage.NewSink()
		ctx = usage.WithSink(ctx, sink)
		askStart = time.Now()
		defer func() { s.emitQueryMetrics("ask", sink, time.Since(askStart)) }()
	}

	// Adaptive retrieval gate (config retrieval.adaptive.*, opt-in). When
	// enabled, a cheap deterministic heuristic decides whether to retrieve at
	// all and what k to use; when disabled this is a no-op so the fixed-k path
	// below is unchanged. The gate uses the question text (the effective query)
	// and never alters the MCP tool contract.
	skipRetrieval := false
	s.metaMu.RLock()
	adaptiveEnabled := s.adaptiveEnabled
	adaptiveKMin := s.adaptiveKMin
	adaptiveKMax := s.adaptiveKMax
	s.metaMu.RUnlock()
	if adaptiveEnabled {
		decision := adaptiveGate(query.Query, query.K, adaptiveKMin, adaptiveKMax)
		s.logf("adaptive gate: class=%s retrieve=%t k=%d", decision.Class, decision.Retrieve, decision.K)
		if decision.Retrieve {
			query.K = decision.K
		} else {
			skipRetrieval = true
		}
	}

	var (
		hits []model.SearchHit
		err  error
	)
	if !skipRetrieval {
		// s.search (not s.Search) keeps the #327 usage sink owned by this Ask,
		// so the nested search does not emit its own query_metrics event.
		hits, err = s.search(ctx, query)
		if err != nil {
			return model.AskResult{}, err
		}
	}

	citations := make([]model.Citation, 0, len(hits))
	for _, hit := range hits {
		citations = append(citations, model.Citation{
			ChunkID: hit.ChunkID,
			RelPath: hit.RelPath,
			Title:   hit.Title,
			Span:    hit.Span,
		})
	}

	answer := buildFallbackAnswer(question, hits)
	if s.gen != nil && len(hits) > 0 {
		s.metaMu.RLock()
		systemPrompt := s.ragSystemPrompt
		maxContextChars := s.ragMaxContextChars
		compressor := s.compressor
		s.metaMu.RUnlock()
		// buildRAGPrompt compresses only the model-facing snippet text; the
		// `hits` and `citations` built above are never mutated, so cited spans
		// remain byte-for-byte identical to what was retrieved.
		prompt := buildRAGPrompt(question, hits, systemPrompt, maxContextChars, compressor)
		var generated string
		genErr := usage.TimeStage(ctx, usage.StageGenerate, func() error {
			var gErr error
			generated, gErr = s.gen.Generate(ctx, prompt)
			return gErr
		})
		if genErr != nil {
			// log the error so callers have visibility; fall back to the
			// precomputed answer when generation fails.  avoid recording the
			// entire question in logs since it may contain sensitive data.
			safeQuestion := truncateQuestion(question)
			s.logf("generator error for question %q: %v", safeQuestion, genErr)
		} else {
			if trimmed := strings.TrimSpace(generated); trimmed != "" {
				answer = trimmed
			}
		}
	}
	answer = ensureAnswerAttributions(answer, citations)

	// use the shared accessor to determine whether indexing is complete;
	// this centralizes locking and nil-handling logic and avoids duplicating
	// the callback lookup that was previously done here.
	indexingComplete, _ := s.IndexingComplete(ctx)

	return model.AskResult{
		Question:         question,
		Answer:           answer,
		Citations:        citations,
		Hits:             hits,
		IndexingComplete: indexingComplete,
	}, nil
}

func (s *Service) OpenFile(ctx context.Context, relPath string, span model.Span, maxChars int) (string, error) {
	content, _, err := s.openFile(ctx, relPath, span, maxChars)
	return content, err
}

// IndexingComplete returns the current indexing state using the callback
// previously registered via SetIndexingCompleteProvider.  If no callback is
// available we conservatively report true (i.e. indexing complete) so that
// callers do not stall waiting for an event that cannot be delivered.
func (s *Service) IndexingComplete(ctx context.Context) (bool, error) {
	// grab the callback under lock, then release before doing any work.
	// this mirrors the pattern used elsewhere in the package and keeps the
	// critical section small.  We also respect the incoming context by
	// checking for cancellation before invoking the callback (which may
	// itself do expensive work or block).

	s.metaMu.RLock()
	indexingFn := s.indexingStateFn
	s.metaMu.RUnlock()

	if err := ctx.Err(); err != nil {
		// context already cancelled or expired; report that to caller rather
		// than potentially running the provider.
		return false, err
	}

	if indexingFn == nil {
		return true, nil
	}
	return indexingFn(), nil
}

func (s *Service) OpenFileWithMeta(ctx context.Context, relPath string, span model.Span, maxChars int) (string, bool, error) {
	return s.openFile(ctx, relPath, span, maxChars)
}

func normalizeOpenFileMaxChars(maxChars int) int {
	if maxChars <= 0 {
		return 20000
	}
	if maxChars > 50000 {
		return 50000
	}
	return maxChars
}

func isMetaSpanKind(kind string) bool {
	return kind == "page" || kind == "time"
}

func (s *Service) openFile(ctx context.Context, relPath string, span model.Span, maxChars int) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	relPath = strings.TrimSpace(relPath)
	if relPath == "" {
		return "", false, model.ErrForbidden
	}

	maxChars = normalizeOpenFileMaxChars(maxChars)

	s.metaMu.RLock()
	rootDir := s.rootDir
	stateDir := s.stateDir
	pathExcludes := append([]string(nil), s.pathExcludes...)
	secretPatterns := append([]*regexp.Regexp(nil), s.secretPatterns...)
	s.metaMu.RUnlock()
	if strings.TrimSpace(rootDir) == "" {
		rootDir = "."
	}

	normalizedRel, realRoot, targetAbs, err := s.resolveFilePath(relPath, rootDir, pathExcludes)
	if err != nil {
		return "", false, err
	}

	// A replace-mode media-only document (SPEC 8.1.7) has direct media chunks
	// but no text representation — a permanent condition. open_file returns the
	// non-retryable MEDIA_NO_TEXT (§15.4), never raw bytes and never the
	// retryable OCR_NOT_READY. Gated to media extensions so text files skip the
	// store lookup entirely.
	if isMediaDocExt(normalizedRel) && s.isMediaOnlyDoc(ctx, normalizedRel) {
		return "", false, model.ErrMediaNoText
	}

	kind := strings.ToLower(strings.TrimSpace(span.Kind))
	if content, truncated, handled, err := s.openFileFromMetadata(normalizedRel, span, maxChars, secretPatterns, kind); handled {
		return content, truncated, err
	}

	resolvedAbs, err := resolveSymlinkInRoot(targetAbs, realRoot, pathExcludes, s)
	if err != nil {
		return "", false, err
	}

	// For binary documents (PDF, audio) with no explicit span — OR with a
	// line-range span, which is meaningless for OCR/transcript text — return the
	// cached OCR/transcript markdown rather than the raw bytes. page=N / time
	// spans were already served from metadata above. Without this, a caller that
	// passes start_line/end_line on a PDF (a very natural thing for a model to
	// try) gets bounced with DOC_TYPE_UNSUPPORTED instead of the text it wanted
	// (issue #364; see also #177). Text-native types (md, txt, code, html) keep
	// the existing default of returning file bytes with line slicing.
	if isBinaryDocType(normalizedRel) && (kind == "" || kind == "lines") {
		content, truncated, err := s.openFileFromOCRCache(stateDir, resolvedAbs, normalizedRel, secretPatterns, maxChars)
		if err != nil {
			return "", false, err
		}
		return content, truncated, nil
	}

	return s.openFileFromResolvedPath(resolvedAbs, secretPatterns, kind, span, maxChars)
}

// chunkModalityChecker is the optional store capability used to classify a
// document as media-only (SPEC 8.1.7). Stores that don't implement it (e.g.
// test fakes) simply opt out, leaving open_file behavior unchanged.
type chunkModalityChecker interface {
	ChunkModalityPresence(ctx context.Context, relPath string) (hasMedia, hasText bool, err error)
}

// isMediaOnlyDoc reports whether relPath has direct media chunks but no
// text-bearing chunk — the permanent MEDIA_NO_TEXT condition. It is
// conservative: any lookup error or a store without the capability returns
// false, so open_file falls back to its existing behavior rather than
// misreporting MEDIA_NO_TEXT.
func (s *Service) isMediaOnlyDoc(ctx context.Context, relPath string) bool {
	s.metaMu.RLock()
	store := s.store
	s.metaMu.RUnlock()
	checker, ok := store.(chunkModalityChecker)
	if !ok {
		return false
	}
	hasMedia, hasText, err := checker.ChunkModalityPresence(ctx, relPath)
	if err != nil {
		s.logf("open_file: media-only check for %q failed: %v", relPath, err)
		return false
	}
	return hasMedia && !hasText
}

// Media extension predicates. These keep the media-only gate (isMediaDocExt),
// the text-serving binary classification (isBinaryDocType), and the cache
// candidate selector (openFileOCRCacheCandidates) over a single, consistent set
// so a format is never classified as media yet served as raw bytes.
func isImageExt(ext string) bool {
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".tif", ".tiff", ".svg":
		return true
	default:
		return false
	}
}

func isAudioExt(ext string) bool {
	switch ext {
	case ".mp3", ".wav", ".m4a", ".flac", ".aac", ".ogg", ".opus":
		return true
	default:
		return false
	}
}

func isVideoExt(ext string) bool {
	switch ext {
	case ".mp4", ".mov":
		return true
	default:
		return false
	}
}

// isMediaDocExt reports whether relPath's extension is an embeddable media type
// (SPEC 8.1.7). Used to gate the media-only store lookup so text/code files keep
// their fast path. Covers every media format the pipeline can ingest; video has
// no text fallback and is handled exclusively by the media-only guard
// (MEDIA_NO_TEXT), while images/PDF/audio also have a text-serving path below.
func isMediaDocExt(relPath string) bool {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(relPath)))
	return ext == ".pdf" || isImageExt(ext) || isAudioExt(ext) || isVideoExt(ext)
}

// isBinaryDocType reports whether relPath has an extension whose contents are
// not human-readable as raw bytes but DO have a document-text representation
// (PDF/image OCR, audio transcript). For these the default open_file response
// serves the OCR/transcript cache rather than raw bytes. Video is excluded: it
// has no text representation, so a media-only video resolves to MEDIA_NO_TEXT.
func isBinaryDocType(relPath string) bool {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(relPath)))
	return ext == ".pdf" || isImageExt(ext) || isAudioExt(ext)
}

// openFileFromOCRCache reads the precomputed OCR (or transcript) representation
// for a binary document. The cache layout mirrors what
// internal/ingest.Service.readOrComputeOCR / readOrComputeTranscript write:
// <stateDir>/cache/ocr/<sha256-of-source-bytes>.md for OCR, and
// <stateDir>/cache/transcribe/<sha256-of-source-bytes>*.txt for transcripts.
// When no cache file exists (e.g. ingest is still running), the function
// returns model.ErrOCRNotReady so callers can surface an actionable error
// rather than fall back to raw bytes.
func (s *Service) openFileFromOCRCache(stateDir, resolvedAbs, relPath string, secretPatterns []*regexp.Regexp, maxChars int) (string, bool, error) {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		stateDir = filepath.Join(".", ".dir2mcp")
	}

	// Reject directories explicitly, mirroring openFileFromResolvedPath. Without
	// this guard os.Open succeeds on a directory and io.Copy on a directory file
	// descriptor surfaces as an opaque OS error that the MCP layer would map to
	// INTERNAL_ERROR; DOC_TYPE_UNSUPPORTED is the correct, actionable mapping.
	info, err := os.Stat(resolvedAbs)
	if err != nil {
		return "", false, err
	}
	if info.IsDir() {
		return "", false, model.ErrDocTypeUnsupported
	}

	sourceFile, err := os.Open(resolvedAbs)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = sourceFile.Close() }()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, sourceFile); err != nil {
		return "", false, err
	}
	hashHex := hex.EncodeToString(hasher.Sum(nil))

	candidates := openFileOCRCacheCandidates(stateDir, hashHex, relPath)
	for _, candidate := range candidates {
		data, readErr := os.ReadFile(candidate)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			return "", false, readErr
		}
		text := string(data)
		if hasSecretMatch(secretPatterns, text) {
			return "", false, model.ErrForbidden
		}
		out, truncated := truncateRunesWithFlag(text, maxChars)
		return out, truncated, nil
	}
	return "", false, model.ErrOCRNotReady
}

// openFileOCRCacheCandidates returns the set of cache paths to consult, in
// preference order, for a given source document. Audio transcripts are stored
// with an optional language suffix (or none), so we glob for any matching
// file. PDFs use a single .md path.
func openFileOCRCacheCandidates(stateDir, hashHex, relPath string) []string {
	ext := strings.ToLower(filepath.Ext(relPath))
	switch {
	case ext == ".pdf" || isImageExt(ext):
		// PDF and image extraction both write extracted markdown to cache/ocr.
		return []string{filepath.Join(stateDir, "cache", "ocr", hashHex+".md")}
	case isAudioExt(ext):
		// transcripts are written as <hash>[<-lang>].txt; default-language
		// transcripts have no suffix, so the unsuffixed file is preferred.
		out := []string{filepath.Join(stateDir, "cache", "transcribe", hashHex+".txt")}
		matches, err := filepath.Glob(filepath.Join(stateDir, "cache", "transcribe", hashHex+"-*.txt"))
		if err == nil {
			sort.Strings(matches)
			out = append(out, matches...)
		}
		return out
	default:
		return nil
	}
}

func (s *Service) openFileFromMetadata(normalizedRel string, span model.Span, maxChars int, secretPatterns []*regexp.Regexp, kind string) (content string, truncated bool, handled bool, err error) {
	if !isMetaSpanKind(kind) || (kind == "time" && span.EndMS <= 0) {
		return "", false, false, nil
	}
	normalizedSpan := span
	normalizedSpan.Kind = kind
	fromMeta, ok := s.sliceFromMetadata(normalizedRel, normalizedSpan)
	if !ok {
		return "", false, false, nil
	}
	if hasSecretMatch(secretPatterns, fromMeta) {
		return "", false, true, model.ErrForbidden
	}
	out, truncated := truncateRunesWithFlag(fromMeta, maxChars)
	return out, truncated, true, nil
}

func (s *Service) openFileFromResolvedPath(resolvedAbs string, secretPatterns []*regexp.Regexp, kind string, span model.Span, maxChars int) (string, bool, error) {
	info, err := os.Stat(resolvedAbs)
	if err != nil {
		return "", false, err
	}
	if info.IsDir() {
		return "", false, model.ErrDocTypeUnsupported
	}

	raw, readTruncated, err := readFileBounded(resolvedAbs, 0)
	if err != nil {
		return "", false, err
	}
	content := string(raw)

	if hasSecretMatch(secretPatterns, content) {
		return "", false, model.ErrForbidden
	}

	selected, err := sliceContentBySpan(content, kind, span)
	if err != nil {
		return "", false, err
	}

	out, outTruncated := truncateRunesWithFlag(selected, maxChars)
	return out, readTruncated || outTruncated, nil
}

// hasSecretMatch reports whether any of the compiled secret patterns matches s.
func hasSecretMatch(patterns []*regexp.Regexp, s string) bool {
	for _, re := range patterns {
		if re != nil && re.MatchString(s) {
			return true
		}
	}
	return false
}

// resolveFilePath validates and resolves relPath relative to rootDir.
// It returns the normalized relative path, the real root (with symlinks
// resolved), the absolute target path, and any validation error.
func (s *Service) resolveFilePath(relPath, rootDir string, pathExcludes []string) (normalizedRel, realRoot, targetAbs string, err error) {
	normalizedRel = filepath.ToSlash(filepath.Clean(relPath))
	isTraversal := normalizedRel == "." || strings.HasPrefix(normalizedRel, "../") || normalizedRel == ".."
	if isTraversal || filepath.IsAbs(relPath) {
		return "", "", "", model.ErrPathOutsideRoot
	}
	for _, pattern := range pathExcludes {
		if s.matchExcludePattern(pattern, normalizedRel) {
			return "", "", "", model.ErrForbidden
		}
	}

	rootAbs, absErr := filepath.Abs(rootDir)
	if absErr != nil {
		return "", "", "", absErr
	}
	realRoot = rootAbs
	if resolvedRoot, rootErr := filepath.EvalSymlinks(rootAbs); rootErr != nil {
		if !errors.Is(rootErr, os.ErrNotExist) {
			return "", "", "", model.ErrForbidden
		}
	} else {
		realRoot = resolvedRoot
	}

	targetAbs = filepath.Join(realRoot, filepath.FromSlash(normalizedRel))
	relFromRoot, relErr := filepath.Rel(realRoot, targetAbs)
	if relErr != nil || relFromRoot == ".." || strings.HasPrefix(relFromRoot, ".."+string(os.PathSeparator)) {
		return "", "", "", model.ErrPathOutsideRoot
	}
	return normalizedRel, realRoot, targetAbs, nil
}

// resolveSymlinkInRoot follows symlinks on targetAbs and validates the result
// is still within realRoot and not excluded by pathExcludes.
func resolveSymlinkInRoot(targetAbs, realRoot string, pathExcludes []string, s *Service) (resolvedAbs string, err error) {
	resolvedAbs, err = filepath.EvalSymlinks(targetAbs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		// fail closed: any error other than not-exist means we cannot
		// confirm the path stays within root, so deny the request.
		return "", model.ErrForbidden
	}
	resolvedRel, relErr := filepath.Rel(realRoot, resolvedAbs)
	if relErr != nil || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(os.PathSeparator)) {
		return "", model.ErrPathOutsideRoot
	}
	resolvedRel = filepath.ToSlash(filepath.Clean(resolvedRel))
	for _, pattern := range pathExcludes {
		if s.matchExcludePattern(pattern, resolvedRel) {
			return "", model.ErrForbidden
		}
	}
	return resolvedAbs, nil
}

// sliceContentBySpan extracts the portion of content identified by the given
// span kind. An empty kind or "lines" applies line-range slicing; "page" and
// "time" select the relevant page/time segment.
func sliceContentBySpan(content, kind string, span model.Span) (string, error) {
	switch kind {
	case "", "lines":
		if kind == "lines" || span.StartLine > 0 || span.EndLine > 0 {
			return sliceLines(content, span.StartLine, span.EndLine), nil
		}
		return content, nil
	case "page":
		page := span.Page
		if page <= 0 {
			page = 1
		}
		// metadata-backed OCR handled above; fall back to slicing pages directly
		paged, ok := slicePage(content, page)
		if !ok {
			return "", model.ErrDocTypeUnsupported
		}
		return paged, nil
	case "time":
		return sliceTimeSpan(content, span)
	default:
		return "", model.ErrDocTypeUnsupported
	}
}

// sliceTimeSpan normalises the time boundaries and extracts the matching
// transcript segment from content.
func sliceTimeSpan(content string, span model.Span) (string, error) {
	startMS := span.StartMS
	endMS := span.EndMS
	if startMS < 0 {
		startMS = 0
	}
	if endMS < 0 {
		endMS = 0
	}
	if endMS > 0 && endMS < startMS {
		endMS = startMS
	}
	// metadata-backed slices for time spans are handled earlier; just extract
	timeSlice, ok := sliceTime(content, startMS, endMS)
	if !ok {
		return "", model.ErrDocTypeUnsupported
	}
	return timeSlice, nil
}

func (s *Service) Stats(ctx context.Context) (model.Stats, error) {
	if err := ctx.Err(); err != nil {
		return model.Stats{}, err
	}

	s.metaMu.RLock()
	rootDir := strings.TrimSpace(s.rootDir)
	stateDir := strings.TrimSpace(s.stateDir)
	protocolVersion := strings.TrimSpace(s.protocolVersion)
	st := s.store
	s.metaMu.RUnlock()

	if rootDir == "" {
		rootDir = "."
	}
	if stateDir == "" {
		stateDir = filepath.Join(rootDir, ".dir2mcp")
	}
	if protocolVersion == "" {
		protocolVersion = "2025-11-25"
	}

	out := model.Stats{
		Root:            rootDir,
		StateDir:        stateDir,
		ProtocolVersion: protocolVersion,
		CorpusStats:     model.CorpusStats{DocCounts: map[string]int64{}},
	}
	if st == nil {
		return out, nil
	}

	if agg, ok := st.(interface {
		CorpusStats(context.Context) (model.CorpusStats, error)
	}); ok {
		corpusStats, err := agg.CorpusStats(ctx)
		if err == nil {
			return applyCorpusStats(out, corpusStats), nil
		}
		if !errors.Is(err, model.ErrNotImplemented) {
			return model.Stats{}, err
		}
	}

	docStatus, docCounts, totalDocs, err := collectStoreStatsFallback(ctx, st)
	if err != nil {
		return model.Stats{}, err
	}
	out.DocCounts = docCounts
	out.TotalDocs = totalDocs
	out.Scanned = docStatus.scanned
	out.Indexed = docStatus.indexed
	out.Skipped = docStatus.skipped
	out.Deleted = docStatus.deleted
	out.Errors = docStatus.errors
	return out, nil
}

type docStatusCounts struct {
	scanned int64
	indexed int64
	skipped int64
	deleted int64
	errors  int64
}

func applyCorpusStats(base model.Stats, corpus model.CorpusStats) model.Stats {
	base.Scanned = corpus.Scanned
	base.Indexed = corpus.Indexed
	base.Skipped = corpus.Skipped
	base.Deleted = corpus.Deleted
	base.Representations = corpus.Representations
	base.ChunksTotal = corpus.ChunksTotal
	base.EmbeddedOK = corpus.EmbeddedOK
	base.EmbeddedPending = corpus.EmbeddedPending
	base.Errors = corpus.Errors
	base.TotalDocs = corpus.TotalDocs
	if len(corpus.DocCounts) == 0 {
		base.DocCounts = map[string]int64{}
	} else {
		base.DocCounts = make(map[string]int64, len(corpus.DocCounts))
		for docType, count := range corpus.DocCounts {
			base.DocCounts[docType] = count
		}
	}
	return base
}

func collectStoreStatsFallback(ctx context.Context, st model.Store) (docStatusCounts, map[string]int64, int64, error) {
	const pageSize = 500
	offset := 0
	counts := make(map[string]int64)
	status := docStatusCounts{}
	var totalDocs int64

	for {
		docs, total, err := st.ListFiles(ctx, "", "", pageSize, offset)
		if err != nil {
			return docStatusCounts{}, nil, 0, err
		}
		for _, doc := range docs {
			status.scanned++
			if doc.Deleted {
				status.deleted++
				continue
			}

			docType := strings.TrimSpace(doc.DocType)
			if docType == "" {
				docType = "unknown"
			}
			counts[docType]++
			totalDocs++

			switch strings.ToLower(strings.TrimSpace(doc.Status)) {
			case "skipped":
				status.skipped++
			case "error":
				status.errors++
			default:
				status.indexed++
			}
		}

		offset += len(docs)
		if len(docs) == 0 || int64(offset) >= total {
			break
		}
	}

	return status, counts, totalDocs, nil
}

func (s *Service) searchHitForLabel(indexName string, label uint64) model.SearchHit {
	s.metaMu.RLock()
	if byIndex, ok := s.chunkByIndex[indexName]; ok {
		if meta, exists := byIndex[label]; exists {
			s.metaMu.RUnlock()
			meta.ChunkID = label
			return meta
		}
	}
	meta, ok := s.chunkByLabel[label]
	s.metaMu.RUnlock()

	if ok {
		meta.ChunkID = label
		return meta
	}

	return model.SearchHit{
		ChunkID: label,
		RelPath: "",
		DocType: "unknown",
		RepType: "unknown",
		Snippet: "",
		Span:    model.Span{Kind: "lines"},
	}
}

// defaultRerankCandidatePool bounds how many fused candidates are sent
// to the rerank provider before truncation to k. 50 matches the
// hybrid candidate pool and keeps latency/cost predictable.
const defaultRerankCandidatePool = 50

// ErrMissingEmbedder is returned when the service was created without
// a configured embedder and a search attempt is made. This prevents a
// nil-pointer panic in searchSingleIndex while giving callers a clear
// failure reason.
var ErrMissingEmbedder = errors.New("embedder not configured")

func (s *Service) searchSingleIndex(ctx context.Context, query string, k int, modelName string, idx model.Index, indexName string, filters model.SearchQuery, allowRerank bool) ([]model.SearchHit, error) {
	if s.embedder == nil {
		// caller should have provided an embedder via NewService or
		// SetEmbedder (not currently available).  Return an explicit
		// error rather than letting the nil dereference panic later.
		return nil, ErrMissingEmbedder
	}
	var vectors [][]float32
	err := usage.TimeStage(ctx, usage.StageEmbed, func() error {
		var embErr error
		vectors, embErr = s.embedder.Embed(ctx, modelName, model.EmbedQuery, []string{query})
		return embErr
	})
	if err != nil {
		return nil, err
	}
	if len(vectors) == 0 {
		return []model.SearchHit{}, nil
	}
	if idx == nil {
		return []model.SearchHit{}, nil
	}

	vectorCandidateLimit := s.hybridVectorCandidateLimit(k, idx)
	filtered, err := s.collectVectorCandidates(ctx, vectors[0], idx, indexName, filters, vectorCandidateLimit)
	if err != nil {
		return nil, err
	}
	if fused, ok := s.runHybridSearch(ctx, query, k, indexName, filtered); ok {
		fused = dedupMediaCandidates(fused)
		fused = s.dedupCrossFileCandidates(fused)
		if allowRerank {
			return s.rerankPool(ctx, query, fused, k), nil
		}
		return truncateSearchHits(fused, k), nil
	}
	filtered = dedupMediaCandidates(filtered)
	filtered = s.dedupCrossFileCandidates(filtered)
	if allowRerank {
		return s.rerankPool(ctx, query, filtered, k), nil
	}
	return truncateSearchHits(filtered, k), nil
}

// isMediaHit reports whether a candidate is a direct media chunk (SPEC 8.1.7).
func isMediaHit(h model.SearchHit) bool {
	switch strings.ToLower(strings.TrimSpace(h.Modality)) {
	case "image", "audio", "video", "pdf":
		return true
	default:
		return false
	}
}

// hitPage returns the document page a hit localizes to, if any. Page spans carry
// it directly; region spans (structured extraction) carry it in the region.
func hitPage(h model.SearchHit) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(h.Span.Kind)) {
	case "page":
		if h.Span.Page > 0 {
			return h.Span.Page, true
		}
	case "region":
		if h.Span.Region != nil && h.Span.Region.StartPage > 0 {
			return h.Span.Region.StartPage, true
		}
	}
	return 0, false
}

func mediaPageKey(relPath string, page int) string {
	return relPath + "\x00" + strconv.Itoa(page)
}

// regionCoversPage reports whether a structured-extraction region span spans the
// given 1-based page. EndPage falls back to StartPage for single-page regions.
func regionCoversPage(region *model.RegionSpan, page int) bool {
	if region == nil || region.StartPage <= 0 || page <= 0 {
		return false
	}
	end := region.EndPage
	if end < region.StartPage {
		end = region.StartPage
	}
	return page >= region.StartPage && page <= end
}

// dedupMediaCandidates implements the SPEC 8.1.7 page-image dedup: drop a media
// page-image candidate for (rel_path, page) only when a text/region candidate
// for that same page survives, so a page is not double-counted. It runs BEFORE
// truncation/rerank. Audio/video time-window media chunks, and media on pages
// with no competing text/region candidate, are kept; distinct text/region
// chunks are never collapsed into each other. Order is preserved.
func dedupMediaCandidates(hits []model.SearchHit) []model.SearchHit {
	textPages := make(map[string]struct{})
	for _, h := range hits {
		if isMediaHit(h) {
			continue
		}
		if p, ok := hitPage(h); ok {
			textPages[mediaPageKey(h.RelPath, p)] = struct{}{}
		}
	}
	if len(textPages) == 0 {
		return hits
	}
	out := make([]model.SearchHit, 0, len(hits))
	for _, h := range hits {
		if isMediaHit(h) {
			if p, ok := hitPage(h); ok {
				if _, dup := textPages[mediaPageKey(h.RelPath, p)]; dup {
					continue
				}
			}
		}
		out = append(out, h)
	}
	return out
}

// dedupCrossFileCandidates implements SPEC 9.2 retrieval-time cross-file
// de-duplication: collapse candidate hits whose source documents share an
// identical content_hash (SPEC 7.6) to a single best-ranked survivor, keeping
// the (already canonical) rel_path of the first survivor in the group. It runs
// AFTER candidate generation/fusion and dedupMediaCandidates, and BEFORE rerank
// (SPEC 9.1.1) and truncation to k — so the candidate POOL shrinks, preserving
// the no-result-loss guarantee (a query MAY then return fewer than k hits).
//
// Grouping is by content_hash directly. Hits whose rel_path has no known (or an
// empty) content_hash are passed through untouched and NEVER grouped together.
// The first (best pre-rerank) survivor per group is kept and the relative order
// of survivors is preserved, so the result is deterministic. When disabled or
// when no hash map is loaded, this is a pass-through.
func (s *Service) dedupCrossFileCandidates(hits []model.SearchHit) []model.SearchHit {
	s.metaMu.RLock()
	enabled := s.crossFileDedupEnabled
	groupKeyByRelPath := s.groupKeyByRelPath
	s.metaMu.RUnlock()

	if !enabled || len(groupKeyByRelPath) == 0 || len(hits) == 0 {
		return hits
	}

	seen := make(map[string]struct{}, len(hits))
	out := make([]model.SearchHit, 0, len(hits))
	for _, h := range hits {
		contentHash := groupKeyByRelPath[h.RelPath]
		if contentHash == "" {
			// Unknown/empty content_hash: never grouped, always kept.
			out = append(out, h)
			continue
		}
		if _, dup := seen[contentHash]; dup {
			continue
		}
		seen[contentHash] = struct{}{}
		out = append(out, h)
	}
	return out
}

// hybridVectorCandidateLimit returns the number of vector candidates to
// request before fusion. When hybrid retrieval is active and the store
// supports BM25, we widen the vector pool to match hybridCandidatePoolSize so
// that vector candidates outside the top-k can still contribute via RRF.
// Otherwise we request only k candidates (the legacy vector-only path).
func (s *Service) hybridVectorCandidateLimit(k int, idx model.Index) int {
	if k <= 0 || idx == nil {
		return k
	}
	s.metaMu.RLock()
	enabled := s.hybridEnabled
	store := s.store
	s.metaMu.RUnlock()
	if !enabled {
		return k
	}
	if _, ok := store.(model.LexicalSearcher); !ok {
		return k
	}
	if hybridCandidatePoolSize > k {
		return hybridCandidatePoolSize
	}
	return k
}

// rerankPool applies the optional rerank stage to a fused candidate
// pool and returns the best k. When rerank is disabled or no reranker
// is configured it is exactly truncateSearchHits(hits, k). It is
// fail-open: any provider error keeps the pre-rerank order. Output is
// deterministically ordered (relevance desc, then chunk_id asc) so ties
// don't depend on provider response ordering (SPEC 9.1.1). Extracted
// from the search paths to keep their cyclomatic complexity in budget.
func (s *Service) rerankPool(ctx context.Context, query string, hits []model.SearchHit, k int) []model.SearchHit {
	s.metaMu.RLock()
	enabled := s.rerankEnabled
	rr := s.reranker
	rmodel := s.rerankModel
	pool := s.rerankCandidatePool
	s.metaMu.RUnlock()

	if !enabled || rr == nil || len(hits) <= 1 {
		return s.diversifyAndTruncate(hits, k)
	}
	if pool <= 0 {
		pool = defaultRerankCandidatePool
	}
	// fused keeps the full pre-rerank order for fail-open fallback;
	// cand is the (capped) slice actually sent to the provider. Never
	// shrink the fallback to the pool: a misconfigured pool < k must
	// still return up to k results on a rerank failure (and on success
	// the un-reranked tail is appended below).
	fused := hits
	cand := fused
	if len(cand) > pool {
		cand = cand[:pool]
	}
	docs := make([]string, len(cand))
	for i, h := range cand {
		docs[i] = h.Snippet
	}
	var results []model.Reranked
	err := usage.TimeStage(ctx, usage.StageRerank, func() error {
		var rerr error
		results, rerr = rr.Rerank(ctx, rmodel, query, docs, k)
		return rerr
	})
	if err != nil || len(results) == 0 {
		if err != nil {
			s.logf("rerank: provider error, falling back to fused order: %v", err)
		}
		return s.diversifyAndTruncate(fused, k)
	}
	out := make([]model.SearchHit, 0, len(fused))
	for _, r := range results {
		if r.Index < 0 || r.Index >= len(cand) {
			s.logf("rerank: out-of-range index %d, falling back to fused order", r.Index)
			return s.diversifyAndTruncate(fused, k)
		}
		h := cand[r.Index]
		h.Score = r.RelevanceScore
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ChunkID < out[j].ChunkID
	})
	// Preserve fused candidates beyond the reranked pool so rerank only
	// reorders and never returns fewer than k when more were available.
	if len(fused) > len(cand) {
		out = append(out, fused[len(cand):]...)
	}
	return s.diversifyAndTruncate(out, k)
}

// diversifyAndTruncate applies the optional MMR diversity re-ordering (issue
// #340) to the final candidate pool and then truncates to k. When MMR is
// disabled it is exactly truncateSearchHits(hits, k) — the candidate order is
// unchanged and no allocation beyond the truncation slice occurs. It runs as
// the last reordering step (after scoring/fusion/rerank and dedup), so the
// relevance signal it consumes is each hit's final authoritative Score.
func (s *Service) diversifyAndTruncate(hits []model.SearchHit, k int) []model.SearchHit {
	s.metaMu.RLock()
	enabled := s.mmrEnabled
	lambda := s.mmrLambda
	s.metaMu.RUnlock()
	if !enabled || len(hits) <= 1 {
		return truncateSearchHits(hits, k)
	}
	return truncateSearchHits(applyMMR(hits, lambda), k)
}

// applyMMR re-orders hits by Maximal Marginal Relevance: starting from the
// most-relevant candidate, it iteratively selects the candidate maximizing
//
//	lambda*relevance(c) - (1-lambda)*maxSim(c, alreadySelected)
//
// where relevance is the pool-normalized final Score in [0,1] and sim is a
// deterministic term-overlap (Jaccard) similarity over hit snippets — SearchHit
// carries no embedding vector, so snippet overlap is the available diversity
// signal. lambda=1 reduces to pure relevance ordering; lambda=0 to pure
// diversity. Ties (equal MMR objective) break on lower ChunkID so the result is
// deterministic. The input slice is not mutated; a re-ordered copy is returned.
func applyMMR(hits []model.SearchHit, lambda float64) []model.SearchHit {
	n := len(hits)
	rel := normalizedRelevance(hits)
	tokens := make([]map[string]struct{}, n)
	for i := range hits {
		tokens[i] = tokenizeSnippet(hits[i].Snippet)
	}

	selected := make([]model.SearchHit, 0, n)
	chosen := make([]bool, n)
	// maxSim[i] tracks the highest similarity of candidate i to any
	// already-selected hit, updated incrementally as selections are made so the
	// loop is O(n^2) overall rather than O(n^3).
	maxSim := make([]float64, n)

	for range hits {
		best := -1
		var bestScore float64
		// The first selection has no diversity penalty (maxSim is 0 for every
		// candidate), so seed it by pure relevance. Applying the full objective
		// here would make every candidate tie at 0 when lambda=0 and fall back to
		// the ChunkID tiebreak instead of picking the most relevant hit.
		firstPick := len(selected) == 0
		for i := 0; i < n; i++ {
			if chosen[i] {
				continue
			}
			score := rel[i]
			if !firstPick {
				score = lambda*rel[i] - (1-lambda)*maxSim[i]
			}
			if best == -1 || score > bestScore ||
				(score == bestScore && hits[i].ChunkID < hits[best].ChunkID) {
				best = i
				bestScore = score
			}
		}
		chosen[best] = true
		selected = append(selected, hits[best])
		// Update each remaining candidate's running max similarity against the
		// newly selected hit.
		for i := 0; i < n; i++ {
			if chosen[i] {
				continue
			}
			if sim := jaccardSimilarity(tokens[i], tokens[best]); sim > maxSim[i] {
				maxSim[i] = sim
			}
		}
	}
	return selected
}

// normalizedRelevance maps each hit's Score onto [0,1] via min-max scaling over
// the pool so the MMR relevance term is commensurate with the [0,1] similarity
// penalty regardless of the underlying score magnitude (cosine, RRF, or rerank
// score). A degenerate pool (all-equal scores) maps to all-1, so MMR then
// orders purely by the diversity penalty.
func normalizedRelevance(hits []model.SearchHit) []float64 {
	rel := make([]float64, len(hits))
	if len(hits) == 0 {
		return rel
	}
	minScore := math.Inf(1)
	maxScore := math.Inf(-1)
	for _, h := range hits {
		if h.Score < minScore {
			minScore = h.Score
		}
		if h.Score > maxScore {
			maxScore = h.Score
		}
	}
	denom := maxScore - minScore
	if denom <= 0 {
		for i := range rel {
			rel[i] = 1
		}
		return rel
	}
	for i, h := range hits {
		rel[i] = (h.Score - minScore) / denom
	}
	return rel
}

// mmrTokenRe splits a snippet into lowercase alphanumeric word tokens for the
// term-overlap diversity signal. Compiled once at package load.
var mmrTokenRe = regexp.MustCompile(`[\p{L}\p{N}]+`)

// tokenizeSnippet lowercases and splits a snippet into a set of word tokens used
// for the term-overlap diversity signal. Returns an empty (non-nil) set for an
// empty snippet.
func tokenizeSnippet(snippet string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, tok := range mmrTokenRe.FindAllString(strings.ToLower(snippet), -1) {
		set[tok] = struct{}{}
	}
	return set
}

// jaccardSimilarity returns |a∩b| / |a∪b| for two token sets, in [0,1]. Two
// empty sets are defined as maximally similar (1.0): snippet-less hits (e.g.
// media-only) are treated as near-duplicates of each other so MMR spreads them
// out rather than clustering them.
func jaccardSimilarity(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	// Intersect over the smaller set to keep the scan tight.
	small, large := a, b
	if len(large) < len(small) {
		small, large = large, small
	}
	inter := 0
	for tok := range small {
		if _, ok := large[tok]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func truncateSearchHits(hits []model.SearchHit, k int) []model.SearchHit {
	if k < 0 {
		k = 0
	}
	if len(hits) <= k {
		return hits
	}
	return hits[:k]
}

// filterFromQuery projects the retrieval-level SearchQuery predicates into the
// backend-agnostic model.Filter (issue #247). ExcludeOrphans is intentionally
// NOT pushed down: orphan/eviction state lives in the retrieval service's
// in-memory chunk metadata, not in the backend payload, so it is enforced by
// the post-materialization matchFilters re-check instead (a backend payload
// always carries a rel_path).
func filterFromQuery(q model.SearchQuery) model.Filter {
	return model.Filter{
		PathPrefix: q.PathPrefix,
		PathGlob:   q.FileGlob,
		DocTypes:   q.DocTypes,
		Speaker:    q.Speaker,
		Languages:  q.Languages,
	}
}

// collectVectorCandidates gathers up to k filtered dense-vector candidates. When
// the index can evaluate the filter itself (FilteringIndex + CanFilter) the
// filter is pushed down to narrow the backend's candidate pool; otherwise an
// empty filter is passed and the predicates are evaluated in Go. In both cases
// the same widening overfetch loop runs and matchFilters is re-applied so the
// in-memory orphan/eviction state is honoured. Extracted from searchSingleIndex
// so the outer function stays under the cyclomatic-complexity budget after
// hybrid fusion was layered on top.
func (s *Service) collectVectorCandidates(
	ctx context.Context,
	vector []float32,
	idx model.Index,
	indexName string,
	filters model.SearchQuery,
	k int,
) ([]model.SearchHit, error) {
	filter := filterFromQuery(filters)
	// Only push the filter down when the backend can evaluate it itself;
	// otherwise pass an empty filter so payload-blind backends behave as
	// before. Either way we run the same widening overfetch loop and re-apply
	// matchFilters in Go: pushing the filter down narrows the candidate pool
	// the backend returns, but the in-memory orphan/eviction state (which the
	// backend payload does not know about, see searchHitFromIndexHit) is still
	// enforced by the post-materialization re-check. Without the widening loop a
	// pushed-down search would under-return whenever the re-check drops an
	// evicted-but-still-indexed chunk.
	backendFilter := model.Filter{}
	if fi, ok := idx.(model.FilteringIndex); ok && fi.CanFilter(filter) {
		backendFilter = filter
	}
	return s.collectFilteredCandidates(ctx, vector, idx, indexName, filters, backendFilter, k)
}

// collectFilteredCandidates runs the widening overfetch loop: it requests a
// widening candidate pool from the backend (with backendFilter pushed down when
// the backend supports it, or an empty filter otherwise) and applies
// matchFilters in Go until k results are gathered or the backend is exhausted.
func (s *Service) collectFilteredCandidates(
	ctx context.Context,
	vector []float32,
	idx model.Index,
	indexName string,
	filters model.SearchQuery,
	backendFilter model.Filter,
	k int,
) ([]model.SearchHit, error) {
	// Read overfetch under lock to avoid races with SetOverfetchMultiplier.
	// The multiplier is clamped to [1,100] at construction time so no further
	// defensive checks are needed here.
	s.metaMu.RLock()
	overfetchMultiplier := s.overfetchMultiplier
	s.metaMu.RUnlock()
	capHint := k
	if capHint > 1024 {
		capHint = 1024
	}
	filtered := make([]model.SearchHit, 0, capHint)
	seen := make(map[uint64]struct{}, capHint)
	for n := initialSearchFanout(k, overfetchMultiplier); len(filtered) < k; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hits, err := idx.Search(ctx, vector, n, backendFilter)
		if err != nil {
			return nil, err
		}
		s.collectFilteredHits(hits, indexName, filters, k, seen, &filtered)
		if searchExhausted(len(filtered), k, len(hits), n) {
			break
		}
		n = nextSearchFanout(n)
	}
	return filtered, nil
}

// searchHitFromIndexHit materialises a SearchHit from a backend IndexHit. It
// prefers the in-memory chunk metadata (searchHitForLabel) when present so
// existing fakes and the HNSW path keep their established behavior, and falls
// back to the backend-supplied payload for fields the in-memory metadata lacks
// (a non-default backend may be the only source of the rel_path/doc_type).
func (s *Service) searchHitFromIndexHit(indexName string, h model.IndexHit) model.SearchHit {
	hit := s.searchHitForLabel(indexName, h.ChunkID)
	// The in-memory chunk metadata is authoritative for the default HNSW path:
	// a missing entry there is the eviction/orphan signal (EvictDocuments
	// deletes the entry), so we must NOT resurrect such a chunk from the backend
	// payload. Only fall back to the payload when the service holds no in-memory
	// metadata at all — i.e. a non-default backend is the sole source of truth.
	if strings.TrimSpace(hit.RelPath) == "" && !s.hasInMemoryMetadata() {
		if payloadHit := h.Payload.ToSearchHit(); strings.TrimSpace(payloadHit.RelPath) != "" {
			hit = payloadHit
			hit.ChunkID = h.ChunkID
		}
	}
	hit.Score = float64(h.Score)
	return hit
}

// hasInMemoryMetadata reports whether any chunk metadata has been registered in
// the service (via SetChunkMetadata*). When true the in-memory maps are the
// authoritative source for materialising hits; when false (e.g. an external
// backend that never populated them) hits are materialised from the backend
// payload instead.
func (s *Service) hasInMemoryMetadata() bool {
	s.metaMu.RLock()
	defer s.metaMu.RUnlock()
	if len(s.chunkByLabel) > 0 {
		return true
	}
	for _, byIndex := range s.chunkByIndex {
		if len(byIndex) > 0 {
			return true
		}
	}
	return false
}

// collectFilteredHits walks one batch of index hits and appends matching hits to
// dst until k results are gathered. Hits already represented in `seen` are
// skipped to avoid duplicates across fanout iterations.
func (s *Service) collectFilteredHits(
	hits []model.IndexHit,
	indexName string,
	filters model.SearchQuery,
	k int,
	seen map[uint64]struct{},
	dst *[]model.SearchHit,
) {
	for _, h := range hits {
		if _, ok := seen[h.ChunkID]; ok {
			continue
		}
		seen[h.ChunkID] = struct{}{}
		hit := s.searchHitFromIndexHit(indexName, h)
		if !matchFilters(hit, filters) {
			continue
		}
		*dst = append(*dst, hit)
		if len(*dst) >= k {
			return
		}
	}
}

func initialSearchFanout(k, overfetchMultiplier int) int {
	// Protect multiplication k * overfetchMultiplier against overflow.
	if k > math.MaxInt/overfetchMultiplier {
		return math.MaxInt
	}
	return k * overfetchMultiplier
}

func nextSearchFanout(current int) int {
	if current >= math.MaxInt/2 {
		return math.MaxInt
	}
	return current * 2
}

func searchExhausted(filteredLen, k, labelsLen, n int) bool {
	return filteredLen >= k || labelsLen < n || n == math.MaxInt
}

func (s *Service) searchBothIndices(ctx context.Context, query string, k int, textModel, codeModel string, textIndex, codeIndex model.Index, filters model.SearchQuery) ([]model.SearchHit, error) {
	// each single-index call will apply the overfetch multiplier internally
	textHits, err := s.searchSingleIndex(ctx, query, k, textModel, textIndex, "text", filters, false)
	if err != nil {
		return nil, err
	}
	codeHits, err := s.searchSingleIndex(ctx, query, k, codeModel, codeIndex, "code", filters, false)
	if err != nil {
		return nil, err
	}

	normalizeScores(textHits)
	normalizeScores(codeHits)

	merged := make(map[uint64]model.SearchHit)
	for _, hit := range textHits {
		merged[hit.ChunkID] = hit
	}
	for _, hit := range codeHits {
		if existing, ok := merged[hit.ChunkID]; ok {
			if hit.Score > existing.Score {
				merged[hit.ChunkID] = hit
			}
			continue
		}
		merged[hit.ChunkID] = hit
	}

	out := make([]model.SearchHit, 0, len(merged))
	for _, hit := range merged {
		out = append(out, hit)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].ChunkID < out[j].ChunkID
		}
		return out[i].Score > out[j].Score
	})
	// Cross-file dedup applies to the MERGED pool too (SPEC 9.2): each
	// searchSingleIndex call above deduped within its own axis, but a
	// byte-identical duplicate can still surface once in the text pool and once
	// in the code pool. Collapse across the merged, normalized, score-sorted
	// pool (keeping the best-ranked survivor) before rerank/truncation, so
	// index=both matches single-index behavior.
	out = s.dedupCrossFileCandidates(out)
	// index=both: rerank once on the merged, normalized pool (SPEC 9.1.1).
	return s.rerankPool(ctx, query, out, k), nil
}

func normalizeScores(hits []model.SearchHit) {
	if len(hits) == 0 {
		return
	}

	minScore := math.Inf(1)
	maxScore := math.Inf(-1)
	for _, hit := range hits {
		if hit.Score < minScore {
			minScore = hit.Score
		}
		if hit.Score > maxScore {
			maxScore = hit.Score
		}
	}
	if maxScore == minScore {
		for i := range hits {
			hits[i].Score = 1
		}
		return
	}

	denom := maxScore - minScore
	for i := range hits {
		hits[i].Score = (hits[i].Score - minScore) / denom
	}
}

func looksLikeCodeQuery(query string) bool {
	return LooksLikeCodeQuery(query)
}

// LooksLikeCodeQuery reports whether the query appears code-oriented.
func LooksLikeCodeQuery(query string) bool {
	q := strings.ToLower(query)

	// keyword pattern with word boundaries to avoid matching substrings.
	hasKw := codeKeywordRe.MatchString(q)
	// punctuation tokens commonly found in code
	hasPunct := codePunctRe.MatchString(q)
	// fenced code blocks or backticks
	hasFenced := strings.Contains(q, "```")
	hasBacktick := strings.Contains(q, "`")
	// file extension-like indicator – restrict to common code extensions and ensure a word boundary
	hasFileExt := fileExtensionRe.MatchString(q)

	// a strong signal: keyword + punctuation nearby
	if hasKw && hasPunct {
		return true
	}

	// otherwise count independent indicators
	indicators := 0
	if hasKw {
		indicators++
	}
	if hasPunct {
		indicators++
	}
	if hasFenced {
		indicators++
	}
	if hasBacktick {
		indicators++
	}
	if hasFileExt {
		indicators++
	}
	return indicators >= 2
}

func buildFallbackAnswer(question string, hits []model.SearchHit) string {
	if len(hits) == 0 {
		return "No relevant context found in the indexed corpus."
	}

	lines := make([]string, 0, len(hits)+1)
	lines = append(lines, fmt.Sprintf("Question: %s", question))
	lines = append(lines, "Top context:")
	limit := len(hits)
	if limit > 5 {
		limit = 5
	}
	for i := 0; i < limit; i++ {
		h := hits[i]
		snippet := truncateSnippet(strings.TrimSpace(h.Snippet), 300)
		if snippet == "" {
			snippet = "(no snippet)"
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", h.RelPath, snippet))
	}
	return strings.Join(lines, "\n")
}

func buildRAGPrompt(question string, hits []model.SearchHit, systemPrompt string, maxContextChars int, compressor contextCompressor) string {
	systemPrompt = strings.TrimSpace(systemPrompt)
	if systemPrompt == "" {
		systemPrompt = defaultRAGSystemPrompt
	}
	if maxContextChars <= 0 {
		maxContextChars = defaultRAGMaxContext
	}
	if maxContextChars > maxRAGMaxContext {
		maxContextChars = maxRAGMaxContext
	}

	var b strings.Builder
	b.WriteString(systemPrompt)
	b.WriteString("\n\n")
	b.WriteString("Question:\n")
	b.WriteString(question)
	b.WriteString("\n\nContext:\n")

	remaining := maxContextChars
	limit := len(hits)
	if limit > 8 {
		limit = 8
	}
	for i := 0; i < limit && remaining > 0; i++ {
		h := hits[i]
		// Keep the bracketed [rel_path] tag stable for the answering model
		// (ensureAnswerAttributions relies on it for canonical citation
		// matching). When a human-readable Title is available, surface it
		// alongside the path as a parenthetical hint so the model has the
		// document name in addition to its path.
		line := "- [" + h.RelPath + "]"
		if title := strings.TrimSpace(h.Title); title != "" {
			line += " (" + title + ")"
		}
		line += " "
		// Evidence-guided compression (issue #335) reshapes ONLY this local
		// copy of the snippet that flows into the prompt; h.Snippet and the
		// caller's citations are untouched. Disabled compressor ⇒ identity.
		modelText := compressor.compressSnippet(question, strings.TrimSpace(h.Snippet))
		snippet := truncateSnippet(modelText, 300)
		switch {
		case snippet != "":
			// Available text (incl. an augment media hit's OCR/transcript)
			// grounds the answer normally.
		case isMediaHit(h):
			// A replace-mode media-only hit has no text: cite it without quoted
			// context rather than as a missing snippet (SPEC 8.1.7).
			snippet = "(" + strings.ToLower(strings.TrimSpace(h.Modality)) + " media; cited without quoted text)"
		default:
			snippet = "(no snippet)"
		}
		line += snippet + "\n"

		lineLen := len([]rune(line))
		if lineLen <= remaining {
			b.WriteString(line)
			remaining -= lineLen
			continue
		}

		truncated := truncateRunes(line, remaining)
		if strings.TrimSpace(truncated) != "" {
			b.WriteString(truncated)
		}
		remaining = 0
	}
	return b.String()
}

func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	if maxRunes <= 3 {
		return string(r[:maxRunes])
	}
	return string(r[:maxRunes-3]) + "..."
}

func truncateSnippet(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	r := []rune(strings.TrimSpace(s))
	if len(r) <= maxRunes {
		return string(r)
	}
	return strings.TrimSpace(string(r[:maxRunes])) + "..."
}

func ensureAnswerAttributions(answer string, citations []model.Citation) string {
	answer = strings.TrimSpace(answer)
	if answer == "" || len(citations) == 0 {
		return answer
	}

	type sourceEntry struct {
		rel   string
		title string
	}
	ordered := make([]sourceEntry, 0, len(citations))
	seen := make(map[string]struct{}, len(citations))
	for _, c := range citations {
		rel := strings.TrimSpace(c.RelPath)
		if rel == "" {
			continue
		}
		if _, ok := seen[rel]; ok {
			continue
		}
		seen[rel] = struct{}{}
		ordered = append(ordered, sourceEntry{rel: rel, title: strings.TrimSpace(c.Title)})
	}
	if len(ordered) == 0 {
		return answer
	}

	missing := make([]string, 0, len(ordered))
	for _, src := range ordered {
		// The canonical [rel_path] tag is what the model emits inline; we
		// only need to add a Sources line for paths it failed to mention.
		// When a human-readable title is present we surface it next to the
		// path so the appended block is more readable than bare paths.
		tag := "[" + src.rel + "]"
		if strings.Contains(answer, tag) {
			continue
		}
		display := tag
		if src.title != "" {
			display = tag + " (" + src.title + ")"
		}
		missing = append(missing, display)
	}
	if len(missing) == 0 {
		return answer
	}

	limit := len(missing)
	if limit > 5 {
		limit = 5
	}
	return answer + "\n\nSources: " + strings.Join(missing[:limit], ", ")
}

// FormatCitation renders a human-readable citation string for a span (SPEC §9.3).
// The base forms are path-only ([rel_path]), page ([rel_path@p=N]), line range
// ([rel_path@L12-48]), and time ([rel_path@t=02:13-02:41]). On a diarized
// transcript a time span MAY append the speaker — preferring the human-readable
// label, falling back to the stable id — as " › Speaker" (§8.6.8), e.g.
// [interview.mp4@t=02:13-02:41 › S2]. The base form is used unchanged when no
// speaker is present, so a non-diarized transcript citation is byte-identical to
// before.
func FormatCitation(relPath string, span model.Span) string {
	relPath = strings.TrimSpace(relPath)
	suffix := ""
	speaker := ""
	switch strings.ToLower(strings.TrimSpace(span.Kind)) {
	case "page":
		if span.Page > 0 {
			suffix = fmt.Sprintf("@p=%d", span.Page)
		}
	case "lines":
		if span.StartLine > 0 && span.EndLine >= span.StartLine {
			suffix = fmt.Sprintf("@L%d-%d", span.StartLine, span.EndLine)
		}
	case "time":
		suffix = "@t=" + formatCitationTime(span.StartMS) + "-" + formatCitationTime(span.EndMS)
		// Prefer the label, fall back to the stable id (SPEC §9.3 example uses S2).
		if speaker = strings.TrimSpace(span.SpeakerLabel); speaker == "" {
			speaker = strings.TrimSpace(span.Speaker)
		}
	}
	out := "[" + relPath + suffix
	if speaker != "" {
		out += " › " + speaker
	}
	return out + "]"
}

// formatCitationTime renders a millisecond offset as mm:ss (or hh:mm:ss past one
// hour) for the §9.3 time-span citation form. Negative input clamps to zero.
func formatCitationTime(ms int) string {
	if ms < 0 {
		ms = 0
	}
	totalSeconds := ms / 1000
	seconds := totalSeconds % 60
	minutes := (totalSeconds / 60) % 60
	hours := totalSeconds / 3600
	if hours > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, seconds)
	}
	return fmt.Sprintf("%02d:%02d", minutes, seconds)
}

func matchFilters(hit model.SearchHit, query model.SearchQuery) bool {
	// Orphaned or evicted chunks have an empty RelPath and must never be
	// surfaced to callers regardless of any other filter criteria.
	if strings.TrimSpace(hit.RelPath) == "" {
		return false
	}

	// Normalize path_prefix the same way list_files does so a prefix that lists
	// a file (e.g. "./acts", "/acts", "acts/", "ACTS") also matches it here
	// (issue #286 Bug B). MatchesPathPrefix returns true for an empty prefix.
	if !model.MatchesPathPrefix(hit.RelPath, query.PathPrefix) {
		return false
	}

	if query.FileGlob != "" {
		matched, err := path.Match(query.FileGlob, hit.RelPath)
		if err != nil || !matched {
			return false
		}
	}

	if len(query.DocTypes) > 0 {
		docTypeMatch := false
		for _, docType := range query.DocTypes {
			if strings.EqualFold(strings.TrimSpace(docType), strings.TrimSpace(hit.DocType)) {
				docTypeMatch = true
				break
			}
		}
		if !docTypeMatch {
			return false
		}
	}

	// Optional speaker filter (SPEC §8.6.8/§15.2): restrict to time-spanned
	// transcript hits attributed to the requested stable speaker id
	// (case-insensitive). A hit whose span carries no speaker — every non-time
	// chunk and every non-diarized transcript — never matches a non-empty
	// speaker filter, so a corpus without diarized transcripts returns no
	// speaker-filtered hits. Empty filter is a no-op (behaviour unchanged).
	if speaker := strings.TrimSpace(query.Speaker); speaker != "" {
		if !strings.EqualFold(speaker, strings.TrimSpace(hit.Span.Speaker)) {
			return false
		}
	}

	// Optional per-language filter (SPEC §9.5/§15.2-3): restrict to candidates
	// whose source representation recorded any of the requested BCP-47 languages,
	// matched on the primary subtag case-insensitively (logical OR). Applied here
	// at candidate selection — before cross-file de-dup, reranking, and truncation
	// to k — so it only removes non-matching candidates and never reorders or
	// changes the result/citation structure. A hit with no recorded language
	// (unknown, §8.8) never matches a non-empty filter; an empty filter is a no-op.
	if len(query.Languages) > 0 {
		if !model.LanguageMatchesAny(hit.Language, query.Languages) {
			return false
		}
	}

	return true
}

func (s *Service) matchExcludePattern(pattern, relPath string) bool {
	pattern = strings.TrimSpace(filepath.ToSlash(pattern))
	relPath = strings.TrimSpace(filepath.ToSlash(relPath))
	if pattern == "" || relPath == "" {
		return false
	}

	// look up precompiled regexp
	s.metaMu.RLock()
	re := s.excludeRegexps[pattern]
	s.metaMu.RUnlock()
	if re == nil {
		// compile lazily in case cache was missed; store for future
		regex, err := regexp.Compile(globToRegexp(pattern))
		if err != nil {
			return false
		}
		// another goroutine may have stored the compiled regexp while we
		// were working; grab write lock and re-check before inserting.
		s.metaMu.Lock()
		if s.excludeRegexps == nil {
			s.excludeRegexps = make(map[string]*regexp.Regexp)
		}
		if existing := s.excludeRegexps[pattern]; existing != nil {
			// use the one already in cache instead of overwriting
			re = existing
		} else {
			s.excludeRegexps[pattern] = regex
			re = regex
		}
		s.metaMu.Unlock()
	}
	return re.MatchString(relPath)
}

// MatchExcludePattern reports whether relPath matches an exclude glob.
func (s *Service) MatchExcludePattern(pattern, relPath string) bool {
	return s.matchExcludePattern(pattern, relPath)
}

func globToRegexp(glob string) string {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); {
		c := glob[i]
		switch c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i += 2
				if i < len(glob) && glob[i] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
				continue
			}
			b.WriteString(`[^/]*`)
		case '?':
			b.WriteString(`[^/]`)
		default:
			if strings.ContainsRune(`.+()|[]{}^$\`, rune(c)) {
				b.WriteByte('\\')
			}
			b.WriteByte(c)
		}
		i++
	}
	b.WriteString("$")
	return b.String()
}

func sliceLines(content string, start, end int) string {
	lines := strings.Split(content, "\n")
	if start <= 0 {
		start = 1
	}
	if end <= 0 {
		end = start
	}
	if start > len(lines) {
		return ""
	}
	if end > len(lines) {
		end = len(lines)
	}
	if end < start {
		end = start
	}
	return strings.Join(lines[start-1:end], "\n")
}

func truncateRunesWithFlag(s string, max int) (string, bool) {
	if max <= 0 {
		return s, false
	}
	r := []rune(s)
	if len(r) <= max {
		return s, false
	}
	return string(r[:max]), true
}

func readFileBounded(path string, maxBytes int) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()

	if maxBytes <= 0 {
		data, readErr := io.ReadAll(f)
		return data, false, readErr
	}

	lim := io.LimitReader(f, int64(maxBytes))
	data, readErr := io.ReadAll(lim)
	if readErr != nil {
		return nil, false, readErr
	}
	return data, len(data) == maxBytes, nil
}

func (s *Service) sliceFromMetadata(relPath string, requested model.Span) (string, bool) {
	s.metaMu.RLock()
	defer s.metaMu.RUnlock()

	type candidate struct {
		start int
		page  int
		text  string
	}
	matches := make([]candidate, 0, 8)

	for _, hit := range s.chunkByLabel {
		if strings.TrimSpace(filepath.ToSlash(hit.RelPath)) != strings.TrimSpace(filepath.ToSlash(relPath)) {
			continue
		}
		if strings.TrimSpace(hit.Snippet) == "" {
			continue
		}
		span := hit.Span
		switch requested.Kind {
		case "page":
			if strings.EqualFold(span.Kind, "page") && span.Page == requested.Page {
				matches = append(matches, candidate{page: span.Page, text: hit.Snippet})
			} else if strings.EqualFold(span.Kind, "region") && regionCoversPage(span.Region, requested.Page) {
				// Structured (docling) chunks carry a region span with a page
				// range, not a plain page span. Without this, open_file page=N on
				// a docling-extracted PDF found no metadata match and fell through
				// to reading raw PDF bytes -> DOC_TYPE_UNSUPPORTED (issue #383).
				matches = append(matches, candidate{page: span.Region.StartPage, text: hit.Snippet})
			}
		case "time":
			if !strings.EqualFold(span.Kind, "time") {
				continue
			}
			if overlapsTime(span.StartMS, span.EndMS, requested.StartMS, requested.EndMS) {
				matches = append(matches, candidate{start: span.StartMS, text: hit.Snippet})
			}
		}
	}

	if len(matches) == 0 {
		return "", false
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].page != matches[j].page {
			return matches[i].page < matches[j].page
		}
		return matches[i].start < matches[j].start
	})

	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.text)
	}
	return strings.Join(out, "\n"), true
}

func overlapsTime(aStart, aEnd, bStart, bEnd int) bool {
	if aEnd <= 0 {
		aEnd = aStart
	}
	if bEnd <= 0 {
		bEnd = bStart
	}
	if aEnd < aStart {
		aEnd = aStart
	}
	if bEnd < bStart {
		bEnd = bStart
	}
	return aStart <= bEnd && bStart <= aEnd
}

func slicePage(content string, page int) (string, bool) {
	if page <= 0 {
		page = 1
	}
	parts := strings.Split(content, "\f")
	if len(parts) > 1 {
		if page > len(parts) {
			return "", false
		}
		return strings.Trim(parts[page-1], "\n"), true
	}
	if page == 1 {
		return content, true
	}
	return "", false
}

func sliceTime(content string, startMS, endMS int) (string, bool) {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	foundTimestamp := false

	for _, line := range lines {
		m := timePrefixRe.FindStringSubmatch(line)
		if len(m) == 0 {
			continue
		}
		foundTimestamp = true
		tsMS := parseTimestampMS(m[1], m[2], m[3])
		if tsMS < startMS {
			continue
		}
		if endMS > 0 && tsMS > endMS {
			continue
		}
		out = append(out, line)
	}

	if !foundTimestamp {
		return "", false
	}
	if len(out) == 0 {
		return "", true
	}
	return strings.Join(out, "\n"), true
}

func parseTimestampMS(a, b, c string) int {
	x, _ := strconv.Atoi(a)
	y, _ := strconv.Atoi(b)
	if c == "" {
		return (x*60 + y) * 1000
	}
	z, _ := strconv.Atoi(c)
	return (x*3600 + y*60 + z) * 1000
}
