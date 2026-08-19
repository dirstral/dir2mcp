package config

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/netutil"
	"github.com/dirstral/dir2mcp/internal/provider"
	"github.com/dirstral/dir2mcp/internal/secrets"
	"github.com/dirstral/dir2mcp/internal/statefs"
	"github.com/dirstral/dir2mcp/internal/subtitle"
	"github.com/dirstral/dir2mcp/internal/usage"
)

const DefaultProtocolVersion = "2025-11-25"

// HyDE (Hypothetical Document Embeddings) query-transform modes
// (config `retrieval.hyde.mode`). HyDEModeFuse RRF-fuses the
// hypothetical-document hits with the raw-query hits; HyDEModeReplace uses the
// hypothetical-document embedding alone. HyDEModeFuse is the default.
const (
	HyDEModeFuse    = "fuse"
	HyDEModeReplace = "replace"
)

// Hierarchical (coarse-to-fine) retrieval knobs (SPEC §9.7/§16.2, config
// `retrieval.hierarchical.*`). HierarchicalSourceRepsAuto is the sentinel that
// selects each document's PRIMARY retrievable text representation — exactly one
// per document, so a summary's `coverage.source_rep_id` is unambiguous (§5.2).
// The levels name the two summary granularities of §5.2.
const (
	HierarchicalSourceRepsAuto = "auto"
	HierarchicalLevelDocument  = "document"
	HierarchicalLevelSection   = "section"
	// DefaultHierarchicalMaxTokens is the spec default per-summary generation
	// bound; DefaultHierarchicalPromptVersion names the built-in (general,
	// domain-free) summary prompt template, part of the summary derivation
	// identity (§8.6.7).
	DefaultHierarchicalMaxTokens     = 512
	DefaultHierarchicalPromptVersion = "v1"
)

const EffectiveConfigSnapshotFile = ".dir2mcp.yaml.snapshot"

// DefaultMediaClipMaxDurationMS / DefaultMediaClipMaxBytes are the built-in
// bounds for the dir2mcp_open_media_clip tool (SPEC §15.11): a 2-minute span
// cap and a 25 MiB inline byte cap. Both are overridable via
// media.clip.max_duration_ms / media.clip.max_bytes.
const (
	DefaultMediaClipMaxDurationMS = 120000
	DefaultMediaClipMaxBytes      = 26214400
)

// defaultRetrievalMinScore is the shipped default for `retrieval.min_score`,
// the RELATIVE pruning floor of SPEC §9.4.3, which requires the floor to ship
// enabled. The floor compares each score as a RATIO to the best score of the
// same result set, so 0.05 means "drop a hit that scores below 5% of the best
// hit". A set of near-equal candidates survives in full, because a hit reaches
// 0 only when its own score is 0 (issue #858).
//
// The value is deliberately permissive. §9.1.1 leaves raw scores on
// incommensurable scales, so this floor can only ever be a relative ranking
// control (#411); an aggressive relative default would cost recall without
// making a weak answer detectable, which is the job of the separate ABSOLUTE
// evidence threshold in internal/retrieval/evidence.go. `0` disables the floor.
const defaultRetrievalMinScore = 0.05

// RAGKMin, RAGKMax and RAGKFallback resolve the number of hits a retrieval
// request asks for (SPEC §9.1).
//
// RAGKMin/RAGKMax are the inclusive bound the `k` request field advertises on
// every tool that takes one, and `rag.k_default` MUST satisfy the SAME bound: a
// default that asks for a k the schema forbids can only fail later, at a request
// the operator did not write, so a configured value outside it is CONFIG_INVALID
// at load.
//
// RAGKFallback is the shipped fallback, used when the operator configured no
// `rag.k_default`. Default() ships it, so the config template, the served schema
// and the runtime all state the same number.
//
// They live here, not in internal/mcp, because config validation needs the bound
// and internal/mcp imports this package (mcp.MinSearchK/MaxSearchK/DefaultSearchK
// alias them, so there is still one definition).
const (
	RAGKMin      = 1
	RAGKMax      = 50
	RAGKFallback = 15
)

// DefaultMediaSubtitlesAlignToleranceMS is the bilingual cross-language cue
// alignment tolerance (SPEC §8.6.10, media.subtitles.ttml.align_tolerance_ms):
// a secondary segment whose start is within this many milliseconds of a primary
// cue is merged into that cue.
const DefaultMediaSubtitlesAlignToleranceMS = 2500

// DefaultMediaTranslateWindowLines / DefaultMediaTranslateContextLines are the
// built-in chat translate-engine window sizes (issue #573): translate cues in
// batches of DefaultMediaTranslateWindowLines, each carrying a read-only margin
// of DefaultMediaTranslateContextLines cues on either side. The window is large
// enough to give the model real cross-cue context (referents, split sentences,
// terminology) while staying a bounded single request; the margin is deliberately
// small so context stays cheap. Both are language-agnostic (general-purpose).
const (
	DefaultMediaTranslateWindowLines  = 16
	DefaultMediaTranslateContextLines = 3
)

type SecretSourceMetadata struct {
	ElevenLabsAPIKey     string
	CohereAPIKey         string
	X402FacilitatorToken string
	AuthToken            string
}

// SourceConfig selects the corpus backend (issue #244, epic #250). The default
// kind "local" reproduces the historical local-filesystem behavior exactly; an
// NFS mount is just a local path so it shares the local backend. The s3 kind
// reads the corpus from an object store. Credentials are never persisted: they
// are resolved at runtime through the existing secret precedence (env → keychain
// → file) from the standard AWS environment variables.
type SourceConfig struct {
	// Kind is one of "local", "nfs", or "s3". Empty normalizes to "local".
	Kind string
	// S3Bucket is the bucket name; required when Kind=s3.
	S3Bucket string
	// S3Prefix optionally scopes the corpus to a key prefix within the bucket.
	S3Prefix string
	// S3Region is the AWS region; empty falls back to the SDK's default chain.
	S3Region string
	// S3Endpoint optionally overrides the endpoint for S3-compatible stores
	// (MinIO, R2, etc.). Empty uses the default AWS endpoint.
	S3Endpoint string
	// S3AccessKeyID/S3SecretAccessKey/S3SessionToken are runtime-only resolved
	// credentials. They are populated from the environment (AWS_ACCESS_KEY_ID,
	// AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN) during load and MUST NOT be
	// persisted to the config file or the effective-config snapshot.
	S3AccessKeyID     string
	S3SecretAccessKey string
	S3SessionToken    string
}

// DistributedEmbedConfig configures the optional distributed-embedding job queue
// (issue #248, SPEC §8.7). It is capability-driven and off by default: the
// distributed mode activates only when Enabled is true. When off, the pipeline
// runs the in-process embedding loop unchanged (SPEC §1.2, §8.7.4).
//
// The broker transport is implementation-defined (SPEC §8.7.4): Broker selects a
// shipped default ("memory" in-process, or "sqlite" persistent) or an external
// adapter. Connection parameters that are secrets (e.g. a broker URL with
// embedded credentials) follow SPEC §16.1.1 and are NEVER persisted to the
// config snapshot — only the non-sensitive Broker selector and the SQLite path
// are persisted.
type DistributedEmbedConfig struct {
	// Enabled turns on the distributed coordinator+worker mode. When true the
	// coordinator enqueues pending chunks and workers drain the queue; the
	// in-process loop is not started. Requires a shared external (Tier C) vector
	// store (SPEC §8.7.4) — validation rejects an embedded Tier A/B backend.
	Enabled bool
	// Broker selects the queue implementation: "memory" (default, in-process,
	// single-node degenerate case) or "sqlite" (persistent, pure-Go). External
	// adapters MAY define additional values.
	Broker string
	// BrokerSQLitePath is the SQLite queue file path when Broker=="sqlite".
	// Empty defaults to <state_dir>/embed-queue.db. Non-sensitive; persisted.
	BrokerSQLitePath string
	// BrokerURL is the connection URL for an external broker (NATS/Redis/SQS).
	// It MAY embed credentials, so it is treated as a runtime-only secret
	// (SPEC §16.1.1): sourced from the environment / secret store and NEVER
	// persisted to the snapshot. Empty for the built-in brokers.
	BrokerURL string
	// MaxAttempts bounds redelivery before a job is dead-lettered (SPEC §8.7.3).
	// Non-positive falls back to the broker default (5).
	MaxAttempts int
}

// QdrantConfig configures the optional Qdrant vector index backend (issue #268),
// selected via index.backend=qdrant. URL and Collection are persisted invariants;
// APIKey is a runtime-only secret resolved through the existing env/keychain/
// .env.local precedence (SPEC §16.1.1) and MUST NOT be written to disk or the
// effective-config snapshot.
type QdrantConfig struct {
	// URL is the Qdrant endpoint (gRPC), e.g. "http://localhost:6334" or
	// "https://xyz.cloud.qdrant.io:6334". Required when index.backend=qdrant.
	URL string
	// Collection is the base collection name. Per-kind collections are derived
	// from it (<collection>_text / <collection>_code) so the text and code
	// vector spaces never collide. Empty defaults to qdrantindex.DefaultCollection.
	Collection string
	// APIKey authenticates to a secured/Cloud deployment. Runtime-only secret;
	// never persisted. Empty for an unsecured local instance.
	APIKey string
}

type X402Config struct {
	// Mode controls whether x402 payment gating is enabled.  Allowed values
	// are "off", "on" and "required".  Validation will normalize the
	// string by trimming whitespace, lower‑casing it, and defaulting the
	// empty value to "off"; this normalization is applied in
	// ValidateX402 and the resulting canonical value is written back into the
	// struct so callers can rely on a cleaned value after validation.  Any
	// invalid mode will cause validation to fail.
	Mode           string
	FacilitatorURL string
	// FacilitatorToken is sensitive and must not be written to disk.
	// Operators should provide it via DIR2MCP_X402_FACILITATOR_TOKEN env var
	// or CLI flags/file options; the config loader ignores file values.
	FacilitatorToken string
	ResourceBaseURL  string
	ToolsCallEnabled bool
	PriceAtomic      string
	Network          string
	Scheme           string
	Asset            string
	PayTo            string
}

type Config struct {
	RootDir         string
	StateDir        string
	ListenAddr      string
	MCPPath         string
	ProtocolVersion string
	Public          bool
	AuthMode        string
	// ServerName overrides the auto-derived MCP serverInfo.name and the
	// suggested `claude mcp add` alias. When empty, identity.AutoServerName
	// is used to produce a stable, unique-per-RootDir name so power users
	// running many instances can distinguish them in their MCP client list.
	ServerName string
	// RateLimitRPS and RateLimitBurst define per-IP token bucket limits
	// used by the MCP server when running in public mode.
	RateLimitRPS   int
	RateLimitBurst int
	// TrustedProxies controls when X-Forwarded-For may be used to derive
	// client identity. Values can be IPs or CIDRs.
	TrustedProxies []string
	PathExcludes   []string
	SecretPatterns []string
	// ResolvedAuthToken is a runtime-only token value injected by CLI wiring.
	// It is not loaded from disk and should not be persisted.
	ResolvedAuthToken string
	// DoclingCommand optionally configures a local docling CLI command
	// template used for rich document extraction.
	DoclingCommand string
	// IngestDoclingServeURL is the HTTP endpoint of a running docling-serve
	// container (e.g. http://127.0.0.1:5001). Required when
	// ingest.extractor=docling-serve; under extractor=auto an empty value
	// simply means the HTTP transport is not used (spec 0.10.0 §7.4.B).
	IngestDoclingServeURL string
	// IngestPandocCommand optionally configures a local pandoc CLI command
	// (its first field is the binary) used for the capability-activated pandoc
	// document extractor (#393). Empty resolves `pandoc` from PATH.
	IngestPandocCommand  string
	ElevenLabsAPIKey     string
	ElevenLabsBaseURL    string
	ElevenLabsTTSVoiceID string
	// AllowedOrigins is always initialized with local defaults and then extended
	// via env/CLI comma-separated origin lists.
	AllowedOrigins []string

	// Warnings captures non-fatal parsing messages that occurred while
	// loading configuration from environment variables, dotenv files, or
	// the config file.  Callers can inspect and log these as desired.  This
	// field is intentionally not persisted to disk.
	Warnings []error

	// RAGSystemPrompt is `rag.system_prompt` (SPEC §16). It supplies the domain
	// rules of the RAG system prompt and replaces the shipped ones. It does not
	// replace the prompt-injection guard: the retrieval service appends that to
	// every system prompt, and no setting switches it off (issue #885).
	RAGSystemPrompt string
	// RAGGenerateAnswer is `rag.generate_answer` (SPEC §16). false withholds
	// answer generation on every ask-family surface: the server returns the
	// retrieval hits with `answer: ""` and `citations: []`, the same shape
	// `mode=search_only` returns (SPEC §9.4). The setting and the request field
	// form a disjunction, so a request cannot turn generation back on, and
	// `mode=answer` against false is SERVED as search_only rather than refused.
	RAGGenerateAnswer bool
	// RAGKDefault is `rag.k_default` (SPEC §16): the number of hits a request
	// that OMITS `k` resolves to. It MUST satisfy the same RAGKMin..RAGKMax
	// bound the request field does; a configured value outside it is
	// CONFIG_INVALID at load. Read it through EffectiveKDefault, which applies
	// the shipped fallback for a Config that never went through Default().
	RAGKDefault         int
	RAGMaxContextChars  int
	RAGOversampleFactor int
	// RetrievalHybridEnabled controls whether the retrieval service combines
	// BM25 (lexical) and vector (semantic) candidates via reciprocal-rank
	// fusion. When false, search is vector-only (legacy behavior). Default
	// is true for new deployments; existing indexes auto-backfill the FTS
	// table on first start.
	RetrievalHybridEnabled bool
	// DedupRetrieval enables retrieval-time cross-file de-duplication (SPEC
	// 9.2): when true, search collapses candidate hits whose source documents
	// share an identical content_hash to a single best-ranked survivor before
	// rerank/truncation. Default false (every candidate is returned as before).
	DedupRetrieval bool
	// RetrievalMinScore is a server-side relevance floor (config
	// `retrieval.min_score`): candidate hits whose score — taken as a RATIO to the
	// best score of the result set — is strictly below this value are dropped after
	// scoring/fusion/rerank and after dedup/truncation, before results reach the
	// model. The floor is applied on that ratio (a RELATIVE floor in
	// [0,1]: 0 keeps everything, 1 keeps only the top hit) rather than the raw
	// authoritative score, because raw scores are incommensurable across retrieval
	// modes — cosine (~0..1) vs RRF (max ≈ 0.033) vs a provider rerank scale — so a
	// single raw threshold would silently wipe out all hybrid/RRF results (#411).
	// It is config-only (NOT an MCP tool parameter) so no tool input/output schema
	// changes. Values outside [0,1] are CONFIG_INVALID: a negative floor would
	// never drop anything, and a floor above 1 would silently drop every hit
	// (see validateMinScore).
	//
	// SPEC §9.4.3 splits this control in two and requires this one to ship
	// ENABLED, so it defaults to defaultRetrievalMinScore rather than to 0.
	// `0` remains the explicit disable representation. Note what this floor is
	// and is NOT: because it measures against the result set's best hit, that hit
	// always maps to 1.0, so some hit always clears any floor. It PRUNES weak hits
	// relative to the best one; it can never report that the best one is itself
	// too weak. The absolute threshold that triggers ask abstention is a
	// separate, non-configurable control (internal/retrieval/evidence.go).
	RetrievalMinScore float64
	// CostPriceOverrides maps a model name to per-1K-token USD prices,
	// overriding the built-in defaults used by the per-query query_metrics
	// event (issue #327). Parsed from the optional top-level `cost.prices:`
	// YAML block. nil/empty ⇒ built-in defaults only. Observability-only:
	// never affects retrieval or tool results.
	CostPriceOverrides map[string]usage.ModelPrice
	// Carbon configures the OPT-IN, approximate energy/CO2e estimate surfaced in
	// the query_metrics event (issue #328). Parsed from the optional top-level
	// `carbon:` YAML block. Disabled by default. Observability-only: never
	// affects retrieval or tool results, and records only counts/estimates.
	Carbon CarbonConfig
	// RetrievalRecencyHalfLife is an OPT-IN server-side time-decay half-life
	// (config `retrieval.recency_half_life`): when > 0, each candidate hit's
	// final (authoritative) score is multiplied by an exponential decay
	// `exp(-ln2 * age / half_life)`, where age is the hit's source document
	// mtime relative to a fixed "now" captured at query start. Newer content
	// therefore ranks higher in dated corpora; a hit with no resolvable date is
	// never boosted nor penalized. The decay is applied after scoring/fusion/
	// rerank and just before the relevance floor. It is config-only (NOT an MCP
	// tool parameter) so no tool input/output schema changes. Default 0 =
	// disabled (no decay): behavior is unchanged unless configured. Negative
	// values are CONFIG_INVALID.
	RetrievalRecencyHalfLife time.Duration
	// ContextCompressionEnabled toggles evidence-guided context compression
	// (config `retrieval.context_compression.enabled`): when true, the Ask path
	// compresses the per-hit text that is sent to the generator — keeping only
	// query-relevant sentences and dropping redundant ones — to cut prompt
	// tokens and fit more evidence inside `rag.max_context_chars`. It is
	// config-only (NOT an MCP tool parameter) so no tool input/output schema
	// changes. It compresses ONLY the model-facing context: citations and the
	// returned hits/snippets are never altered, so citation fidelity is
	// preserved. Default false ⇒ unchanged behavior (raw snippets are sent).
	ContextCompressionEnabled bool
	// ContextCompressionTargetRatio bounds how aggressively a hit's text is
	// compressed: the keeper keeps at most this fraction of the hit's original
	// rune length (rounded up), always retaining at least the single most
	// query-relevant sentence. Valid range (0,1]; 0 selects the built-in
	// default (0.5). Values >1 or non-finite are CONFIG_INVALID. Ignored when
	// ContextCompressionEnabled is false.
	ContextCompressionTargetRatio float64
	// RetrievalAdaptiveEnabled toggles the opt-in, training-free adaptive
	// retrieval gate (config `retrieval.adaptive.enabled`). When false
	// (default) Ask uses today's fixed-k behavior unchanged. When true a
	// cheap, deterministic per-query heuristic decides whether retrieval is
	// needed (skip trivial queries) and adjusts k within
	// [RetrievalAdaptiveKMin, RetrievalAdaptiveKMax] (narrow easy queries,
	// widen hard ones). It is config-only (NOT an MCP tool parameter): no tool
	// input/output schema change, so it is ungated. See SPEC 9 (retrieval).
	RetrievalAdaptiveEnabled bool
	// RetrievalAdaptiveKMin / RetrievalAdaptiveKMax bound the dynamic k chosen
	// by the adaptive gate when enabled. The gate never returns k outside
	// [k_min, k_max]; the base k (rag.k_default or a caller-supplied k) is
	// clamped into this window. 0 means "use the built-in default" (see
	// defaultConfig). Ignored entirely when RetrievalAdaptiveEnabled is false.
	RetrievalAdaptiveKMin int
	RetrievalAdaptiveKMax int
	// RetrievalMMREnabled toggles Maximal Marginal Relevance (MMR) diversity
	// re-ordering (config `retrieval.mmr.enabled`). When true, the final
	// candidate pool is re-ordered before truncation to trade some relevance for
	// coverage/diversity, iteratively picking the candidate that maximizes
	// `lambda*relevance - (1-lambda)*maxSimToAlreadyPicked`. It composes with
	// dedup (SPEC 9.2) and rerank (SPEC 9.1.1) and is config-only (NOT an MCP
	// tool parameter). Default false ⇒ pass-through (candidate order unchanged),
	// preserving the local-first, no-surprises default.
	RetrievalMMREnabled bool
	// RetrievalMMRLambda is the MMR relevance-vs-diversity trade-off
	// (config `retrieval.mmr.lambda`) in [0,1]: 1.0 = pure relevance (no
	// diversity penalty), 0.0 = pure diversity. Default 0.5 balances the two. It
	// is only consulted when RetrievalMMREnabled is true. Values outside [0,1]
	// (or NaN/Inf) are CONFIG_INVALID.
	RetrievalMMRLambda float64
	// RetrievalHyDEEnabled toggles the opt-in HyDE (Hypothetical Document
	// Embeddings) query transform (config `retrieval.hyde.enabled`). When true,
	// Search generates a short hypothetical answer to the query via the
	// configured generator, embeds that text, and retrieves with it. It is
	// config-only (NOT an MCP tool parameter) so no tool input/output schema
	// changes. Default false ⇒ unchanged behavior (raw query only). A generation
	// failure degrades gracefully back to the raw query (never fatal). See
	// Gao et al., "Precise Zero-Shot Dense Retrieval without Relevance Labels".
	RetrievalHyDEEnabled bool
	// RetrievalHyDEMode selects how the HyDE-variant results combine with the
	// raw-query results (config `retrieval.hyde.mode`): "fuse" (default) RRF-fuses
	// the hypothetical-document hits with the raw-query hits; "replace" uses the
	// hypothetical-document embedding alone. Ignored when RetrievalHyDEEnabled is
	// false. An empty value normalizes to "fuse".
	RetrievalHyDEMode string
	// RetrievalContextualEnabled opts IN to contextual retrieval (SPEC §8.1.8,
	// config `retrieval.contextual.enabled`, issue #330): before a chunk is
	// embedded, a short LLM-generated, document-aware context string is prepended
	// to the text sent to the EMBEDDER. The persisted, displayed, and CITED chunk
	// text stays the raw chunk, byte-for-byte (citation faithfulness, #403).
	// Off by default. It is capability-driven and fail-open like OCR/STT: with no
	// chat provider available the corpus embeds raw and records the effective
	// `…|off` embed identity plus a warning, never a hard error.
	RetrievalContextualEnabled bool
	// RetrievalContextualProvider optionally pins the chat provider profile that
	// generates the context (config `retrieval.contextual.provider`). Empty (the
	// default) uses the configured chat capability binding (SPEC 8.1.3). The
	// resolved provider — including its normalized endpoint — is part of the
	// embed identity's contextual component (§8.1.4).
	RetrievalContextualProvider string
	// RetrievalContextualModel optionally overrides the generation model
	// (config `retrieval.contextual.model`). Empty uses the resolved profile's
	// chat model. Part of the embed identity's contextual component.
	RetrievalContextualModel string
	// RetrievalContextualMaxTokens caps a single generated context (config
	// `retrieval.contextual.max_tokens`, default 128): the context is meant to be
	// one or two sentences. A different cap can yield a different context, so it
	// is part of the embed identity's contextual component (§8.1.4). Values <= 0
	// are CONFIG_INVALID when contextual retrieval is enabled.
	RetrievalContextualMaxTokens int
	// RetrievalContextualPromptVersion names the built-in, domain-general prompt
	// template (config `retrieval.contextual.prompt_version`, default "v1"). It
	// and the EFFECTIVE prompt text are both folded into the embed identity's
	// contextual component, so an edited operator override re-embeds even without
	// a version bump (§8.1.4). An unknown version is CONFIG_INVALID when
	// contextual retrieval is enabled and no explicit prompt override is set.
	RetrievalContextualPromptVersion string
	// RetrievalContextualPrompt optionally overrides the built-in prompt template
	// (config `retrieval.contextual.prompt`). It MUST contain the {{DOCUMENT}} and
	// {{CHUNK}} placeholders. Empty (the default) uses the built-in template named
	// by RetrievalContextualPromptVersion.
	RetrievalContextualPrompt string
	// CrossLingualEnabled enables server-side cross-lingual query expansion
	// (config `retrieval.cross_lingual.enabled`): when true and a translator is
	// available, the query is translated into the corpus's other languages, each
	// variant is retrieved independently, and the per-language result sets are
	// RRF-fused (reusing the hybrid fusion machinery) so an EN query surfaces RU
	// content (and vice versa). Config-only (NOT an MCP tool parameter) so no
	// tool input/output schema changes. Default false = disabled: search behavior
	// is unchanged unless configured. A translation failure for one language is
	// skipped, never fatal.
	CrossLingualEnabled bool
	// CrossLingualTargetLangs lists the language tag(s) the query is expanded
	// into (config `retrieval.cross_lingual.target_langs`). The sentinel "auto"
	// (the default when the list is empty) means "the corpus's detected languages"
	// (#267) resolved at startup; an explicit list pins the targets. The detected
	// query language is skipped (no self-translation). Has no built-in fixed
	// language pair (general-purpose).
	CrossLingualTargetLangs []string
	// RetrievalHierarchicalEnabled opts IN to hierarchical (coarse-to-fine)
	// retrieval (SPEC §9.7, config `retrieval.hierarchical.enabled`). When true,
	// ingest additionally derives a `summary` representation per document (§5.2)
	// and search expands a summary hit to the fine chunks it covers before
	// dedup/rerank. Off by default ⇒ flat retrieval (§9.1) is unchanged.
	//
	// It is deliberately NOT part of the corpus-lifetime embed identity (§8.1.4):
	// a summary is an ADDITIVE vector in the same space as its document's chunks,
	// so toggling this is an index add/remove, never a corpus-wide re-embed.
	RetrievalHierarchicalEnabled bool
	// RetrievalHierarchicalSourceReps selects WHICH fine representations of each
	// document are summarized (config `retrieval.hierarchical.source_reps`). Empty
	// or the sentinel "auto" (the default) means the document's PRIMARY retrievable
	// text representation — exactly one per document, so a summary's
	// `coverage.source_rep_id` is unambiguous (§5.2). An explicit list of rep_types
	// summarizes each named representation that exists on the document.
	RetrievalHierarchicalSourceReps []string
	// RetrievalHierarchicalLevels lists the summary levels produced (config
	// `retrieval.hierarchical.levels`): "document" (the default) and/or "section".
	// Section-level windowing is not implemented yet; a configured "section" is
	// accepted, warned about at startup, and produces no summaries so coverage
	// reporting stays honest.
	RetrievalHierarchicalLevels []string
	// RetrievalHierarchicalProvider optionally pins the generator profile used to
	// write summaries (config `retrieval.hierarchical.provider`). Empty ⇒ the
	// configured chat/annotator generator (§8.1.3).
	RetrievalHierarchicalProvider string
	// RetrievalHierarchicalMaxTokens bounds one summary generation call (config
	// `retrieval.hierarchical.max_tokens`, §9.7). Applied only when the generator
	// implements model.BoundedGenerator; 0 falls back to the generator default.
	RetrievalHierarchicalMaxTokens int
	// RetrievalHierarchicalPromptVersion names the built-in (general, domain-free)
	// summary prompt template (config `retrieval.hierarchical.prompt_version`). It
	// is part of the summary derivation identity (§8.6.7).
	RetrievalHierarchicalPromptVersion string
	// RetrievalHierarchicalPrompt optionally overrides the built-in template
	// (config `retrieval.hierarchical.prompt`). The EFFECTIVE prompt is hashed into
	// the summary derivation identity (`prompt_hash`, §5.2/§8.6.7), so an edited
	// override re-derives summaries even without a prompt_version bump.
	RetrievalHierarchicalPrompt string
	// Rerank* configure the optional post-fusion rerank stage (SPEC
	// 9.1.1). Reranking auto-activates when a provider credential is
	// present. CohereAPIKey is a secret: env-sourced, never persisted
	// to the config snapshot.
	//
	// RerankEnabled is a tri-state override (nil = auto: on iff a
	// credential is present; *false = force off even with a credential;
	// *true = require it, warn+fail-open if no credential). Resolve the
	// effective decision via rerankEnabledEffective / configureReranker.
	RerankEnabled  *bool
	RerankProvider string
	CohereAPIKey   string
	// CohereBaseURL overrides the Cohere API endpoint (self-hosted /
	// proxied deployments and tests). Empty = provider default. Not a
	// secret; persisted to the config snapshot.
	CohereBaseURL       string
	RerankModel         string
	RerankCandidatePool int
	// `chunking.strategy` used to be accepted here as ChunkingStrategy. It is
	// gone on purpose (issue #661). No runtime path ever read it, the canonical
	// spec defines no chunking-strategy selector, and the chunker picks its
	// strategy from the doc type (SPEC §7.5: characters for text, line windows
	// for code, time windows for a transcript). The key is therefore an
	// unrecognized key like any other typo: config load names it in a warning
	// (issue #628) and it is no longer written to a generated config or snapshot.
	// Do not re-add the field without a spec that says what a strategy value
	// means and a chunker that reads it.
	ChunkingMaxTokens     int
	ChunkingOverlapTokens int

	IngestGitignore      bool
	IngestFollowSymlinks bool
	IngestMaxFileMB      int
	// IngestExcludeDirs is the directory ignore list applied by discovery
	// (config `ingest.exclude_dirs`, SPEC §7.1, issue #773). Discovery does not
	// descend into a directory whose NAME matches an entry. An entry is a plain
	// directory name: it is not a path and it is not a glob, and it matches at
	// any depth below the root.
	//
	// A present key REPLACES the default list in full. It does not add to it.
	// That is the list behaviour of `security.path_excludes`, so there is one
	// rule to learn and not two. An absent key keeps the default list
	// (corpusfs.DefaultExcludedDirs), so an existing corpus does not change.
	//
	// `.dir2mcp` is always added back, even when the operator's list omits it.
	// The state directory lives under root_dir, so to index it is
	// self-referential: the walk would read the index the same run writes, and
	// watch mode would feed its own writes back into ingest. The loader records
	// a warning when it adds the name back.
	//
	// THE GATES ARE INDEPENDENT AND THEY COMPOSE BY AND. A file reaches the
	// index only if this list, `security.path_excludes` and the optional
	// `.gitignore` rules ALL admit it. Five names are in BOTH this default list
	// and the default `security.path_excludes`: `.git`, `.dir2mcp`,
	// `node_modules`, `vendor` and `__pycache__`. To re-index such a directory
	// an operator MUST clear the name from EVERY gate that lists it. To clear
	// this one alone changes nothing the operator can observe, because
	// `security.path_excludes` drops the same file a moment later.
	// `node_modules` is blocked in retrieval as well
	// (internal/retrieval/service.go defaultPathExcludes), so it needs a third
	// edit.
	IngestExcludeDirs []string
	// IngestPDFMode / IngestImagesMode / IngestAudioMode / IngestArchivesMode are
	// the per-format mode keys of the SPEC §16.2 config template
	// (`ingest.pdf.mode`, `ingest.images.mode`, `ingest.audio.mode`,
	// `ingest.archives.mode`). Each one is a CLOSED enum, listed with the
	// template: off|ocr|auto, off|ocr_auto|ocr_on, off|auto|on and
	// off|shallow|deep.
	//
	// Validate rejects a value outside the advertised set (issue #655), so a typo
	// no longer loads. The ingestion runtime does NOT read these values yet, and
	// that half of #655 is deliberately not implemented here: the canonical spec
	// names the enum members without defining what any member DOES, so writing
	// the behavior would author normative semantics this repository does not own
	// (spec-first governance; compare dirstral-spec#68). The README says the same
	// thing, so an operator is not told the modes work.
	//
	// IngestArchivesMode is the sharp case (issue #843). Its shipped default is
	// `deep`, which names recursive expansion of an archive nested inside an
	// archive, and no such recursion exists: extractArchiveMembers walks the top
	// level of one container and a nested archive is stored as a skipped
	// archive_member document. `off` is wrong in the other direction, because
	// archives are expanded under it too. Only `shallow` happens to match the
	// behavior, and it matches by coincidence rather than by being read.
	//
	// Neither the default nor the meaning of a member is changed here. The
	// default and the enum both come from the canonical SPEC §16.2 template, so
	// to move either one is a spec change and belongs in dirstral-spec first.
	// What this repository owns is the retraction: the generated config carries
	// inertIngestModeCommentLines directly above the keys, and a hand-written
	// config that sets one of them to `off` gets warnInertIngestModesOff at load.
	IngestPDFMode      string
	IngestImagesMode   string
	IngestAudioMode    string
	IngestArchivesMode string
	IngestExtractor    string
	// IngestOnUnsupported is the §7.4.B.2 degradation mode for a format no active
	// extraction engine can read: "lenient" (default) skips with a warning and
	// names the gap in the honest coverage report, "strict" records a non-fatal
	// per-document UNSUPPORTED_FORMAT error. Empty defaults to lenient.
	IngestOnUnsupported string

	// IndexBackend selects the vector index backend (issue #246): "memory"
	// (default, the in-memory HNSW) or "disk" (the Tier-B pure-Go on-disk
	// backend that keeps vector payloads memory-mapped on disk for corpora
	// that exceed RAM). The "memory" default is byte-identical to legacy
	// behavior. This is the backend-selection seam future networked backends
	// (#268 Qdrant, #269 pgvector) extend.
	IndexBackend string

	// IngestScanCache opts IN to a directory-discovery scan cache (issue #267
	// item 5, config `ingest.scan_cache`). For large local archives the corpus is
	// re-walked from scratch on every run; the cache persists a per-directory
	// signature (the directory's own mtime plus its direct children's
	// name/size/mtime/mode) so an unchanged directory skips re-reading and
	// re-sorting its entries. Correctness is never traded for speed: every cached
	// child file is still stat'd so an in-place modification is detected, and any
	// add/remove/rename (which bumps the parent directory mtime) or stat failure
	// falls the directory back to a full re-walk.
	//
	// Every timestamp is compared to the NANOSECOND, and a directory whose mtime
	// is not yet settled (older than the coarsest filesystem timestamp
	// granularity) is not cached at all (#667). Both rules exist because a
	// second-resolution comparison read a change inside the recorded second as no
	// change, and a file added in that window was never enumerated.
	//
	// Default OFF: discovery behaves exactly as before. Only consulted for the
	// local-filesystem backend.
	IngestScanCache bool

	// IngestLateChunking opts IN to "late chunking" (issue #332, Jina's
	// technique): instead of today's chunk-then-embed, the WHOLE document is
	// embedded once through a long-context model to obtain contextually-enriched
	// token embeddings, then the existing chunk boundaries are applied and the
	// token vectors within each chunk's span are mean-pooled — so every chunk
	// embedding carries document-level context (BEIR gains grow with document
	// length). It is provider/model-dependent: it requires the configured
	// embedder to expose token-level / long-context embeddings (the optional
	// model.TokenEmbedder capability). When the active embedder cannot supply
	// them, ingestion GRACEFULLY FALLS BACK to today's chunk-then-embed and
	// records that fallback; nothing changes silently. Default OFF: the pipeline
	// chunks-then-embeds exactly as before.
	IngestLateChunking bool

	// IngestWatch enables a filesystem watcher so a running server keeps
	// indexing added/changed/deleted files after the initial scan. Opt-in.
	IngestWatch bool
	// IngestWatchDebounce coalesces editor write bursts: a path is processed
	// once it has been quiet for this long.
	IngestWatchDebounce time.Duration

	// LanguageDetectionEnabled turns on best-effort representation language
	// auto-detection (SPEC §8.8, config `language_detection_enabled`). On by
	// default: for a plain-text (raw_text) representation with no operator pin
	// (configured) and no source-asserted language (declared), the language is
	// detected from its text and recorded with language_source="detected".
	// Detection is best-effort and never fails ingestion; below-floor/unknown
	// results simply leave the language unset (a first-class state). Set false to
	// record no detected language. (The confidence floor is a fixed default,
	// langdetect.DefaultMinConfidence; making it configurable is a follow-up.)
	LanguageDetectionEnabled bool

	STTProvider               string
	STTMistralModel           string
	STTElevenLabsModel        string
	STTElevenLabsLanguageCode string

	// RecognizeProvider selects the recognition capability backend (design
	// 0004): "off" (default) or "serve" (a locally served recognizer at
	// RecognizeServeURL, e.g. `dirstral-annotator serve`).
	RecognizeProvider string
	RecognizeServeURL string
	// RecognizeServeCommand, when set with provider "serve", makes dir2mcp
	// launch the backend itself (`sh -c <command>`), wait for /health, and
	// terminate it on daemon shutdown — instead of connecting to an
	// operator-started process.
	RecognizeServeCommand string

	// MediaSidecarsDisabled opts OUT of subtitle sidecar ingestion (spec
	// §8.6.4). Sidecar ingestion is enabled by default (spec default
	// media.sidecars.enabled: true): a .vtt/.srt/.ttml file next to a media file
	// is ingested as that media's transcript instead of running STT. Set this
	// true to disable that and always run STT (the kill-switch opt-out).
	MediaSidecarsDisabled bool

	// MediaVariantsGroup enables media multi-rendition grouping/dedup (spec
	// §8.6.5, config `media.variants.group`). When true, discovered media
	// renditions that share a normalized name (rendition markers such as
	// 1080p/720p, bitrate, codec/quality suffixes stripped) are grouped and a
	// single canonical rendition is ingested once, so chunks/embeddings are not
	// duplicated across renditions. Disabled by default: discovery behaves
	// exactly as before. Non-media and single-rendition assets are unaffected.
	MediaVariantsGroup bool

	// MediaVariantsSelect chooses the canonical rendition within a group (spec
	// §8.6.5, config `media.variants.select`): "best" (default) prefers the
	// highest detected resolution/quality, tiebreaking on largest size then
	// lexically-lowest path; "first" deterministically takes the
	// lexically-lowest path. Only consulted when MediaVariantsGroup is true.
	MediaVariantsSelect string

	// MediaTranslateEnabled opts IN to translating transcripts into one or more
	// target languages (spec §8.6.2, config `media.translate.enabled`). Off by
	// default. When enabled, each source transcript additionally yields one
	// translated transcript representation per target language. Enabling this
	// with an empty MediaTranslateTargetLangs is CONFIG_INVALID.
	MediaTranslateEnabled bool

	// MediaTranslateTargetLangs lists the target language tag(s) transcripts are
	// translated into (spec §8.6.2, config `media.translate.target_langs`). There
	// is NO built-in default (general-purpose: no language is assumed). Only
	// consulted when MediaTranslateEnabled is true; enabling translation with an
	// empty list is CONFIG_INVALID.
	MediaTranslateTargetLangs []string

	// MediaTranslateEngine selects HOW transcripts are translated (config
	// `media.translate.engine`): "chat" (default) translates the transcript text
	// line-by-line via the chat generator; "whisper" re-decodes the source audio
	// with Whisper's native translate task (audio->English single pass), which
	// keeps names/terms in the acoustic model's context and produces its own
	// segment timings. "whisper" requires the STT provider to be kind:whisper and
	// English-only target_langs (Whisper's only translation target). Only
	// consulted when MediaTranslateEnabled is true.
	MediaTranslateEngine string
	// MediaTranslateWhisperWindowSec bounds the audio window Whisper's translate
	// task decodes at a time (config `media.translate.whisper_window_sec`). A
	// single full-audio translate decode drifts its timestamps on long recordings;
	// when this is > 0 the audio is decoded in overlapping windows of this length
	// and each window's timestamps are offset by its start, so drift cannot
	// accumulate. 0 (default) preserves the historical single-pass behavior. Only
	// consulted for MediaTranslateEngine="whisper".
	MediaTranslateWhisperWindowSec int
	// MediaTranslateWindowLines sets how many consecutive transcript cues the
	// chat translate engine sends to the model in ONE request (config
	// `media.translate.window_lines`, issue #573). Translating cues in a window
	// lets the model see neighbouring cues, so it can resolve pronouns/agreement,
	// keep split sentences coherent, and hold terminology consistent — the blind
	// per-cue path degraded quality, worst on low-resource languages. A strict
	// numbered 1:1 request/response contract preserves the one-source-cue ↔
	// one-output-line invariant chunkTranscriptByTime depends on; a malformed
	// batch response falls back to per-line translation for that window (never a
	// desync). 0 (unset) means the built-in default; <=1 restores the historical
	// per-line behaviour (and, with window_lines<=1 and context_lines=0, keeps
	// pre-windowing translate caches valid). Only consulted for engine="chat".
	MediaTranslateWindowLines int
	// MediaTranslateContextLines sets how many surrounding cues on EACH side of a
	// window are supplied to the chat translate engine as READ-ONLY context —
	// shown to the model to disambiguate but never re-translated or returned
	// (config `media.translate.context_lines`, issue #573). 0 means no margin.
	// Only consulted for engine="chat".
	MediaTranslateContextLines int
	// MediaTranslateGlossary is OPTIONAL terminology guidance for the CHAT
	// translate engine (config `media.translate.glossary`, SPEC §8.6.2, issue
	// #574): a mapping keyed per BCP-47 TARGET language of source term/phrase →
	// preferred rendering, injected into BOTH the per-line and windowed translate
	// prompts as GUIDANCE (not a hard constraint) so proper nouns, domain terms,
	// and preferred spellings render consistently AND inflect correctly (the model
	// adapts morphology, unlike a find/replace). Only the entries for the CURRENT
	// target language are injected. Empty/unset = no guidance (historical
	// behaviour). It is DISTINCT from media.subtitles.glossary (an export-time
	// regex find/replace on rendered cues). Domain-general: no built-in terms; the
	// map is entirely operator-provided.
	MediaTranslateGlossary map[string]map[string]string
	// MediaFilterWords is an optional, general-purpose list of boilerplate /
	// credits / watermark phrases stripped from transcript and subtitle text
	// (config `media.filter_words`). Matching is case-insensitive substring
	// removal; the same filter is applied during transcript chunking (so the
	// phrases are never embedded) and on subtitle export in every format (so no
	// exported VTT/SRT/TTML contains them). Empty by default = off: behavior is unchanged everywhere
	// when no phrases are configured. There are NO built-in defaults — the list
	// carries no language- or domain-specific phrases.
	MediaFilterWords []string

	// MediaSubtitlesTTMLEnabled opts IN to the OPTIONAL bilingual broadcast
	// subtitle-packaging surface (SPEC §8.6.10, config
	// `media.subtitles.ttml.enabled`). OFF by default: VTT/SRT export (§8.6.3) is
	// unaffected and no TTML/SMIL is produced. When enabled the `export` command
	// can render TTML (monolingual, or bilingual when a secondary --lang transcript
	// exists) and, when also enabled, a companion SMIL packaging document.
	MediaSubtitlesTTMLEnabled bool

	// MediaSubtitlesTTMLAlignToleranceMS is the cross-language cue alignment
	// tolerance in milliseconds (SPEC §8.6.10, config
	// `media.subtitles.ttml.align_tolerance_ms`, default 2500). A secondary-language
	// segment whose start is within this tolerance of a primary cue is merged into
	// that cue; otherwise it is emitted as its own secondary-only cue. Only consulted
	// for bilingual TTML export. Zero/negative falls back to the default at
	// validation time.
	MediaSubtitlesTTMLAlignToleranceMS int

	// MediaSubtitlesSMILEnabled opts IN to emitting a SMIL packaging document
	// alongside TTML (SPEC §8.6.10, config `media.subtitles.smil.enabled`). OFF by
	// default. Only consulted when MediaSubtitlesTTMLEnabled is true. SMIL carries
	// the probed track metadata (container/codec, bitrate, video width/height) via
	// ffprobe and fails open: when metadata is unavailable the SMIL is omitted and
	// the text subtitle output is still produced.
	MediaSubtitlesSMILEnabled bool

	// MediaSubtitlesSegmentation selects HOW time-coded VTT/SRT cues are built on
	// export (config `media.subtitles.segmentation`): "chunk" (default) emits one
	// cue per stored transcript chunk (a whisper segment) — the historical
	// behavior; "broadcast" re-segments from per-word timings (spec §8.6.1) into
	// broadcast-legible cues (<= 6 s, <= 2x42 chars, reading-speed aware) via
	// subtitle.BuildBroadcastCues. "broadcast" needs word timings; a transcript/
	// span without them falls back to the chunk builder, so behavior is unchanged
	// when word timings are absent. Only affects VTT/SRT export, never ingest.
	//
	// This key is DELIBERATELY format-specific, unlike the editorial cleaning keys
	// below (issue #729): it rewrites cue BOUNDARIES for VTT/SRT reading speed,
	// whereas TTML cue regions are the alignment unit for bilingual packaging
	// (§8.6.10) and must stay the stored transcript segment spans so both language
	// runs remain traceable to the same span.
	MediaSubtitlesSegmentation string

	// The five media.subtitles.* keys below are the editorial cue-cleaning
	// pipeline. They apply to EVERY exported subtitle format — VTT, SRT and TTML
	// (plus its companion SMIL) — not to one format each: the spec describes
	// media.subtitles.glossary in format-neutral, export-time terms (§8.6.2), and
	// exporting the same transcript in two formats must not yield two different
	// editorial results. TTML used to skip all five (issue #729).
	//
	// This is the export surface. Four of the five (drop_urls, drop_phrases,
	// scrub_phrases, collapse_repeats) ALSO run at ingest, before chunks are
	// embedded, so the retrieval index agrees with the exported sidecar (issues
	// #545, #765): they are hallucination filters, and a cue an operator
	// configured away must not stay embedded and citable. glossary is export-only
	// because SPEC §8.6.2 defines it as an export-time find/replace on
	// already-rendered cue text. Each key notes which it is.
	//
	// The ingest half runs when a document is (re)indexed, so enabling or widening
	// any of the four on an EXISTING corpus cleans the exported sidecar
	// immediately but leaves already-embedded chunks as they were until the corpus
	// is re-indexed (`dir2mcp reindex`). The derivation identity (§8.6.7)
	// deliberately tracks provider/model provenance only, not operator cleaning
	// preferences: folding these keys into it would re-run transcription (the
	// transcript cache is keyed by that identity) for an editorial toggle.

	// MediaSubtitlesGlossary is an optional list of editorial term replacements
	// applied to exported cue text in every format (config
	// `media.subtitles.glossary`). Each entry is "pattern=>replacement" where
	// pattern is a case-insensitive, ASCII-word-bounded regular expression (e.g.
	// "Aju?bei=>Adzhubei" to normalize a transliterated name). Unlike
	// media.filter_words (which deletes phrases) this REWRITES text and never
	// drops a cue. Empty by default = off. Export-only: indexed/cited transcript
	// text is not rewritten, because SPEC §8.6.2 pins this key as a deterministic,
	// export-time find/replace on already-rendered cue text. The consequence is
	// worth knowing: search matches the pre-glossary spelling while every exported
	// artifact shows the post-glossary one (issue #765). Changing that is a spec
	// change first, not a code change.
	MediaSubtitlesGlossary []string

	// MediaSubtitlesCollapseRepeats drops the Nth-and-later cue in a run of
	// identical consecutive cues on export in every format (config
	// `media.subtitles.collapse_repeats`), the whisper repetition-collapse
	// artifact. A value < 2 disables the pass (the default 0 = off), so
	// legitimate short repeats survive; a typical setting is 3. Also applied at
	// ingest, so a repetition artifact cannot dominate a document's chunks (#765).
	MediaSubtitlesCollapseRepeats int

	// MediaSubtitlesDropURLs opts IN to dropping exported cues whose text is a
	// hallucinated URL / bare domain / credit line (config
	// `media.subtitles.drop_urls`), in every format. OFF by default. Also applied
	// at ingest, so a cue whisper hallucinated over silence is never embedded and
	// can never be cited for a span where nobody said anything (#765). It is a
	// whole-cue verdict on both surfaces: a stored chunk that merges several cues
	// is dropped entirely when any of its text trips the URL pattern, which is
	// exactly what the export pass does with the same chunk. That bluntness is why
	// the key is opt-in.
	MediaSubtitlesDropURLs bool

	// MediaSubtitlesDropPhrases is an optional list of regular expressions; an
	// exported cue whose text is composed ENTIRELY of matches (plus punctuation)
	// is dropped (config `media.subtitles.drop_phrases`), in every format. This
	// removes whisper keyword-spam hallucinated over silence/music/B-roll that
	// survives the URL-drop and consecutive-identical collapse passes, without
	// harming real speech that merely mentions one of the words. Empty by
	// default. Also applied at ingest, so the phrases are never embedded (#545).
	MediaSubtitlesDropPhrases []string

	// MediaSubtitlesScrubPhrases is an optional list of regular expressions
	// EXCISED from exported cue text in every format (config
	// `media.subtitles.scrub_phrases`). Unlike drop_phrases (a whole-cue verdict),
	// a scrub removes just the matched phrase from a cue that also carries real
	// speech — for a hallucinated phrase that leaked into the same cue as genuine
	// words. Configure with the full contiguous phrase so legitimate mentions of a
	// single word are untouched. A cue that scrubs to empty is dropped. Empty by
	// default. Also applied at ingest, so the phrases are never embedded (#545).
	MediaSubtitlesScrubPhrases []string

	// MediaTrimLeadingSilence opts IN to trimming leading silence from media
	// transcripts (dir2mcp#258, config `media.trim_leading_silence`). When true
	// and ffmpeg is available, the duration of dead air before the first speech
	// is detected (ffmpeg silencedetect) and subtracted from every transcript
	// segment/word timestamp (clamped at 0) so timings align to first speech.
	//
	// Default OFF. livevtt defaulted this on for its broadcast capture use case,
	// but dir2mcp is general-purpose: silence trimming is a heuristic that can
	// shift timestamps for media where the leading "silence" is intentional, so
	// it is opt-in here. ffmpeg absent / detection failure -> no trim, no error.
	MediaTrimLeadingSilence bool

	// MediaSilenceThresholdDB is the noise floor (dBFS) below which audio counts
	// as silence for leading-silence detection (config
	// `media.silence_threshold_db`). Zero/positive falls back to the avutil
	// default (-40 dB). Only consulted when MediaTrimLeadingSilence is true.
	MediaSilenceThresholdDB float64

	// MediaVAD enables a provider-side voice-activity-detection filter where the
	// STT provider supports it (config `media.vad`). For the self-hosted whisper
	// provider this sets the OpenAI-compatible `vad_filter` form field so the
	// server skips non-speech audio. Providers without VAD support ignore it.
	// Default OFF.
	MediaVAD bool

	// MediaDiarizeEnabled is the TRI-STATE speaker-diarization opt-out/in
	// (config `media.diarize.enabled`, SPEC §8.6.8). It is a *bool to distinguish
	// the three states the spec requires:
	//   - nil (omitted): AUTO — diarization auto-enables when the active STT
	//     backend advertises CapDiarize (capability-driven activation).
	//   - false: force OFF regardless of backend capability (the kill switch).
	//   - true: REQUIRE diarization — CONFIG_INVALID if no configured STT backend
	//     advertises the capability.
	// Default OFF (a backend without the capability never auto-enables). It is a
	// *bool here (not in the overrides block) because the tri-state must survive
	// all the way to runtime, not be flattened to a plain bool at merge time.
	MediaDiarizeEnabled *bool

	// MediaAudioWindowSec / MediaVideoWindowSec configure the direct-embedding
	// media chunk window length, in seconds, for audio and video respectively
	// (SPEC 8.1.7; config `media.audio_window_sec` / `media.video_window_sec`).
	// A positive value overrides the built-in default (audio 120 s, video 60 s);
	// a value exceeding the per-modality cap (audio 180 s, video 120 s) is
	// clamped to the cap (with a warning). Zero/unset falls back to the default,
	// so behavior is identical to the hardcoded constants when unconfigured.
	MediaAudioWindowSec int
	MediaVideoWindowSec int

	// MediaClipMaxDurationMS / MediaClipMaxBytes bound the
	// dir2mcp_open_media_clip tool (SPEC §15.11; config `media.clip.max_duration_ms`
	// / `media.clip.max_bytes`). A requested span whose duration exceeds
	// MediaClipMaxDurationMS, or an extracted clip whose size exceeds
	// MediaClipMaxBytes, is rejected with the non-retryable CLIP_TOO_LARGE error.
	// Defaults: 120000 ms (2 min) and 26214400 bytes (25 MiB).
	MediaClipMaxDurationMS int
	MediaClipMaxBytes      int

	// MediaSTTMaxPayloadMB / MediaSTTRequestTimeoutSec tune the self-hosted
	// whisper STT client's request limits (config `media.stt.max_payload_mb` /
	// `media.stt.request_timeout_sec`), applied onto whichever whisper STT profile
	// resolves. The whisper client's built-in caps (50 MB payload, 120 s request
	// timeout) are too small for long-form media — a 30-min mono file exceeds the
	// payload cap and takes longer than 120 s to transcribe — so these let an
	// operator raise them (dir2mcp#510, #511). 0 (default) means "use the client's
	// built-in default"; negative is CONFIG_INVALID.
	MediaSTTMaxPayloadMB      int
	MediaSTTRequestTimeoutSec int

	// MediaSTTLanguageStrict controls whether a corpus-pinned STT language
	// (config `media.stt.language` / a provider profile's language) is treated
	// by the output quality gate as ground truth about content, and therefore
	// enforced by its language/script-mismatch detector (config
	// `media.stt.language_strict`, dir2mcp#439 F3).
	//
	// A pinned STT language is a provider HINT that biases transcription; it is
	// NOT a per-file guarantee about content. Feeding it to the quality gate as
	// the expected language quarantines legitimate other-language clips (e.g. an
	// English interview in a corpus pinned to `ru` transcribes to ~100%
	// off-script and is discarded). That contradicts the general-purpose,
	// auto-detect mandate, so the default is false: a pinned language never
	// drives quarantine, and the language detector self-skips for transcripts
	// (degenerate output is still caught by the repetition/gibberish/density
	// detectors). Operators who have verified a genuinely single-language corpus
	// can set this true to restore strict script-mismatch enforcement.
	MediaSTTLanguageStrict bool

	// MediaSTTLanguageProviders routes transcription by the pinned source
	// language (config `media.stt.language_providers`, SPEC §8.2.1, #566): a map
	// from a BCP-47 primary language subtag to the NAME of an STT-capable provider
	// profile. When the resolved SOURCE-LANGUAGE pin matches a key, that profile
	// transcribes, OVERRIDING the default STT provider; empty/unset preserves the
	// single-provider behaviour. Routing is PIN-BASED and per-run: langdetect is
	// text-based and cannot know an audio file's language before transcription, so
	// the route keys off the configured language pin (resolved once), never
	// per-item auto-detection — with no pin there is nothing to route on. Keys are
	// normalized to the BCP-47 primary subtag (lower-cased) at load; a mapped name
	// that is absent or not STT-capable is rejected as CONFIG_INVALID at startup.
	MediaSTTLanguageProviders map[string]string

	// MediaSTTOnUncoveredLanguage is the honest-coverage floor action (config
	// `media.stt.on_uncovered_language`, SPEC §8.2.1, #566): what the daemon does
	// when the resolved source language is outside the finally-selected STT model's
	// declared stt_languages coverage. "warn" (default, fail-open) transcribes
	// anyway and records the (language, model, covered=false) fact on the transcript
	// meta_json so an operator sees it. "skip" (strict) records the item as
	// documents.status="skipped" with skip_reason="language_uncovered" instead of
	// transcribing to degraded output — no transcript representation is produced.
	// The floor only applies when coverage is DECLARED (a non-empty stt_languages);
	// unknown coverage never trips it under either mode.
	MediaSTTOnUncoveredLanguage string
	// MediaSTTTracks selects WHICH audio tracks of a multi-track media container
	// are transcribed (config `media.stt.tracks`, SPEC §8.6.12, issue #567). It is
	// the RAW, unvalidated form as written in config: an empty slice (the default)
	// or the single keyword `first` means "only the first audio stream" (today's
	// behavior and cost); `all` means "every audio track"; a list of 0-based
	// audio-relative indices (e.g. `0`, `2`) means exactly those tracks. Parse it
	// into the resolved selection with ParseSTTTracks, which also rejects invalid
	// forms (duplicate/negative indices, a keyword mixed with indices) as
	// CONFIG_INVALID. Track 0 always keeps the bare `transcript` rep_type; each
	// additional track N>=1 gets a `transcript@t<N>` rep_type, so a single-track
	// corpus is byte-for-byte unchanged.
	MediaSTTTracks []string

	// MediaBatchTwoPhase / MediaBatchProgress / MediaBatchManifest configure the
	// optional batch-ergonomics surface for large-archive media ingests (SPEC
	// §8.6.11; config block `media.batch`). All default OFF/empty so behavior is
	// byte-identical to today when unconfigured.
	//   - MediaBatchTwoPhase (media.batch.two_phase): run media ingest as two
	//     ordered passes (transcription, then derivation) instead of single-pass.
	//     Observably output-equivalent to single-pass — ordering/reporting only.
	//   - MediaBatchProgress (media.batch.progress): emit side-channel, monotonic
	//     progress against a per-pass total. Never alters outputs or ordering.
	//   - MediaBatchManifest (media.batch.manifest): path of a JSONL run manifest
	//     (one record per asset). Empty disables the manifest. The manifest is
	//     advisory for resume only — the live identity/cache/mtime gates always win.
	MediaBatchTwoPhase bool
	MediaBatchProgress bool
	MediaBatchManifest string

	// QualityGatesEnabled is the master switch for the output quality gate
	// (spec 0.16.0): when true (default), generated transcript/OCR text is
	// screened for degenerate output (repetition loops, empty output,
	// off-script, low density, gibberish) before it is chunked and embedded,
	// and failing chunks are quarantined instead of embedded. Set false to
	// skip the gate entirely.
	QualityGatesEnabled bool

	ServerTLSCertFile string
	ServerTLSKeyFile  string

	// SessionInactivityTimeout defines how long a session may be idle before it
	// is considered expired.  Zero means the default hardcoded value (24h).
	SessionInactivityTimeout time.Duration
	// SessionMaxLifetime sets an optional absolute upper bound on a session's
	// lifespan regardless of activity.  Zero disables this limit.
	SessionMaxLifetime time.Duration
	// HealthCheckInterval controls how frequently the runtime polls connector
	// health endpoints when checking for availability.  A zero value means the
	// default (5s).  The interval is used as the base fixed delay; failures
	// trigger bounded exponential backoff independent of this setting.
	HealthCheckInterval time.Duration

	X402 X402Config

	// Qdrant configures the optional Qdrant vector backend (issue #268), used
	// when IndexBackend=="qdrant". See QdrantConfig.
	Qdrant QdrantConfig

	// IndexPgvectorDSN is the libpq connection string for the pgvector backend
	// (issue #269), used when IndexBackend=="pgvector". It is sensitive (like an
	// API key / the Qdrant api_key): sourced from the environment / secret store,
	// never persisted to the config file or the effective-config snapshot.
	// Required when IndexBackend=pgvector.
	IndexPgvectorDSN string
	// IndexPgvectorSchema / IndexPgvectorTable name the vectors table. They are
	// persisted invariants; empty values fall back to the pgvectorindex package
	// defaults.
	IndexPgvectorSchema string
	IndexPgvectorTable  string

	// Source selects the corpus backend (local/nfs/s3). See SourceConfig.
	Source SourceConfig

	// DistributedEmbed configures the optional distributed-embedding job queue
	// (issue #248, SPEC §8.7). Off by default: when disabled the in-process
	// embedding loop runs unchanged (local-first single-binary default, §1.2).
	DistributedEmbed DistributedEmbedConfig

	// providersDoc holds the parsed `providers:`/`model:` subtree (SPEC
	// 0.7.0 §8.1/§16.2), decoded with yaml.v3 separately from the
	// bespoke flat parser. Unexported/runtime-derived: not persisted,
	// not part of Default()/persistedConfig. Access via cfg.Providers().
	providersDoc providersDoc
}

// STTTrackMode is the resolved kind of a media.stt.tracks selection (SPEC
// §8.6.12).
type STTTrackMode int

const (
	// STTTracksFirst transcribes only the container's first audio stream — the
	// default (and today's single-track behavior and cost). It is the zero value.
	STTTracksFirst STTTrackMode = iota
	// STTTracksAll transcribes every audio track.
	STTTracksAll
	// STTTracksList transcribes exactly the explicitly-listed 0-based tracks.
	STTTracksList
)

// STTTrackSelection is the resolved, validated media.stt.tracks selection (SPEC
// §8.6.12). Build it from the raw config with ParseSTTTracks. For STTTracksList
// the Indices are sorted ascending and de-duplicated so they are processed in
// container stream order regardless of how the list was written ([2,0] ≡ [0,2]).
type STTTrackSelection struct {
	Mode    STTTrackMode
	Indices []int
}

// ParseSTTTracks resolves the raw media.stt.tracks config (Config.MediaSTTTracks)
// into an STTTrackSelection (SPEC §8.6.12). The accepted forms are:
//
//   - empty / unset, or the single keyword "first"  -> STTTracksFirst (default)
//   - the single keyword "all"                      -> STTTracksAll
//   - a list of non-negative 0-based indices        -> STTTracksList
//
// It rejects, as CONFIG_INVALID: an unknown keyword; a keyword mixed with
// indices; a negative or non-integer index; a duplicate index; and an explicit
// empty list (which would select no tracks). Whether a listed index actually
// exists in a given media file is a per-file, runtime concern (a corpus mixes
// files with different track counts), so an out-of-range index is NOT rejected
// here — it is handled per file by the ingest pipeline as a track-scoped skip.
func ParseSTTTracks(raw []string) (STTTrackSelection, error) {
	items := make([]string, 0, len(raw))
	for _, r := range raw {
		if t := strings.TrimSpace(r); t != "" {
			items = append(items, t)
		}
	}
	if len(items) == 0 {
		return STTTrackSelection{Mode: STTTracksFirst}, nil
	}
	// A single keyword form.
	if len(items) == 1 {
		switch strings.ToLower(items[0]) {
		case "first":
			return STTTrackSelection{Mode: STTTracksFirst}, nil
		case "all":
			return STTTrackSelection{Mode: STTTracksAll}, nil
		}
	}
	// Otherwise every element MUST be a non-negative integer index.
	seen := make(map[int]struct{}, len(items))
	indices := make([]int, 0, len(items))
	for _, it := range items {
		switch strings.ToLower(it) {
		case "first", "all":
			return STTTrackSelection{}, fmt.Errorf(
				"CONFIG_INVALID: media.stt.tracks may not mix the keyword %q with track indices; use %q alone or a list of 0-based indices",
				strings.ToLower(it), strings.ToLower(it))
		}
		n, err := strconv.Atoi(it)
		if err != nil {
			return STTTrackSelection{}, fmt.Errorf(
				"CONFIG_INVALID: media.stt.tracks entry %q is not a valid track index; expected \"first\", \"all\", or 0-based integers", it)
		}
		if n < 0 {
			return STTTrackSelection{}, fmt.Errorf(
				"CONFIG_INVALID: media.stt.tracks index %d is negative; track indices are 0-based", n)
		}
		if _, dup := seen[n]; dup {
			return STTTrackSelection{}, fmt.Errorf(
				"CONFIG_INVALID: media.stt.tracks lists duplicate index %d", n)
		}
		seen[n] = struct{}{}
		indices = append(indices, n)
	}
	sort.Ints(indices)
	return STTTrackSelection{Mode: STTTracksList, Indices: indices}, nil
}

type fileConfig struct {
	// unknownKeys collects config-file keys that no setter claimed, so the
	// loader can warn instead of discarding operator intent in silence (#628).
	// Not a config value itself: it never participates in the overlay onto
	// Config, only in the warning surfaced after load.
	unknownKeys []string

	RootDir         *string
	StateDir        *string
	ListenAddr      *string
	MCPPath         *string
	ProtocolVersion *string
	Public          *bool
	AuthMode        *string
	ServerName      *string
	RateLimitRPS    *int
	RateLimitBurst  *int
	TrustedProxies  []string
	PathExcludes    []string
	SecretPatterns  []string
	DoclingCommand  *string

	IngestDoclingServeURL              *string
	IngestPandocCommand                *string
	ElevenLabsBaseURL                  *string
	ElevenLabsTTSVoiceID               *string
	AllowedOrigins                     []string
	RAGSystemPrompt                    *string
	RAGGenerateAnswer                  *bool
	RAGKDefault                        *int
	RAGMaxContextChars                 *int
	RAGOversampleFactor                *int
	RetrievalHybridEnabled             *bool
	DedupRetrieval                     *bool
	RetrievalMinScore                  *float64
	RetrievalRecencyHalfLife           *time.Duration
	ContextCompressionEnabled          *bool
	ContextCompressionTargetRatio      *float64
	RetrievalAdaptiveEnabled           *bool
	RetrievalAdaptiveKMin              *int
	RetrievalAdaptiveKMax              *int
	RetrievalMMREnabled                *bool
	RetrievalMMRLambda                 *float64
	RetrievalHyDEEnabled               *bool
	RetrievalHyDEMode                  *string
	RetrievalContextualEnabled         *bool
	RetrievalContextualProvider        *string
	RetrievalContextualModel           *string
	RetrievalContextualMaxTokens       *int
	RetrievalContextualPromptVersion   *string
	RetrievalContextualPrompt          *string
	CrossLingualEnabled                *bool
	CrossLingualTargetLangs            []string
	RetrievalHierarchicalEnabled       *bool
	RetrievalHierarchicalSourceReps    []string
	RetrievalHierarchicalLevels        []string
	RetrievalHierarchicalProvider      *string
	RetrievalHierarchicalMaxTokens     *int
	RetrievalHierarchicalPromptVersion *string
	RetrievalHierarchicalPrompt        *string
	RerankEnabled                      *bool
	RerankProvider                     *string
	CohereAPIKey                       *string
	CohereBaseURL                      *string
	RerankModel                        *string
	RerankCandidatePool                *int
	ChunkingMaxTokens                  *int
	ChunkingOverlapTokens              *int
	IngestGitignore                    *bool
	IngestFollowSymlinks               *bool
	IngestMaxFileMB                    *int
	IngestExcludeDirs                  []string
	IngestPDFMode                      *string
	IngestImagesMode                   *string
	IngestAudioMode                    *string
	IngestArchivesMode                 *string
	IngestExtractor                    *string
	IngestOnUnsupported                *string
	IndexBackend                       *string
	IngestScanCache                    *bool
	IngestLateChunking                 *bool
	IngestWatch                        *bool
	IngestWatchDebounce                *time.Duration
	STTProvider                        *string
	STTMistralModel                    *string
	STTElevenLabsModel                 *string
	STTElevenLabsLanguageCode          *string
	RecognizeProvider                  *string
	RecognizeServeURL                  *string
	RecognizeServeCommand              *string
	QualityGatesEnabled                *bool
	LanguageDetectionEnabled           *bool
	MediaSidecarsDisabled              *bool
	MediaVariantsGroup                 *bool
	MediaVariantsSelect                *string
	MediaTranslateEnabled              *bool
	MediaTranslateTargetLangs          []string
	MediaTranslateEngine               *string
	MediaTranslateWhisperWindowSec     *int
	MediaTranslateWindowLines          *int
	MediaTranslateContextLines         *int
	MediaFilterWords                   []string
	MediaSubtitlesTTMLEnabled          *bool
	MediaSubtitlesTTMLAlignToleranceMS *int
	MediaSubtitlesSMILEnabled          *bool
	MediaSubtitlesSegmentation         *string
	MediaSubtitlesGlossary             []string
	MediaSubtitlesDropPhrases          []string
	MediaSubtitlesScrubPhrases         []string
	MediaSubtitlesCollapseRepeats      *int
	MediaSubtitlesDropURLs             *bool
	MediaTrimLeadingSilence            *bool
	MediaSilenceThresholdDB            *float64
	MediaVAD                           *bool
	MediaDiarizeEnabled                *bool
	MediaAudioWindowSec                *int
	MediaVideoWindowSec                *int
	MediaClipMaxDurationMS             *int
	MediaClipMaxBytes                  *int
	MediaSTTMaxPayloadMB               *int
	MediaSTTRequestTimeoutSec          *int
	MediaSTTLanguageStrict             *bool
	MediaSTTOnUncoveredLanguage        *string
	MediaSTTTracks                     []string
	ElevenLabsAPIKey                   *string
	ServerTLSCertFile                  *string
	ServerTLSKeyFile                   *string
	MediaBatchTwoPhase                 *bool
	MediaBatchProgress                 *bool
	MediaBatchManifest                 *string
	// session timings expressed as YAML duration strings.  populated by
	// parseConfigYAML's custom parser via setFileScalarValue rather than the
	// standard yaml.Unmarshal machinery.  struct tags are therefore omitted
	// elsewhere and would be purely documentation if added here.
	SessionInactivityTimeout *time.Duration
	SessionMaxLifetime       *time.Duration
	HealthCheckInterval      *time.Duration
	X402Mode                 *string
	X402FacilitatorURL       *string
	X402FacilitatorToken     *string
	X402ResourceBaseURL      *string
	X402ToolsCallEnabled     *bool
	X402PriceAtomic          *string
	X402Network              *string
	X402Scheme               *string
	X402Asset                *string
	X402PayTo                *string
	SourceKind               *string
	SourceS3Bucket           *string
	SourceS3Prefix           *string
	SourceS3Region           *string
	SourceS3Endpoint         *string
	QdrantURL                *string
	QdrantCollection         *string
	IndexPgvectorDSN         *string
	IndexPgvectorSchema      *string
	IndexPgvectorTable       *string
	// Distributed embedding (issue #248, SPEC §8.7). BrokerURL is a runtime-only
	// secret and is intentionally absent so it can never be written to disk.
	DistributedEmbedEnabled     *bool
	DistributedEmbedBroker      *string
	DistributedEmbedSQLitePath  *string
	DistributedEmbedMaxAttempts *int
}

type persistedConfig struct {
	RootDir         string   `yaml:"root_dir"`
	StateDir        string   `yaml:"state_dir"`
	ListenAddr      string   `yaml:"listen_addr"`
	MCPPath         string   `yaml:"mcp_path"`
	ProtocolVersion string   `yaml:"protocol_version"`
	Public          bool     `yaml:"public"`
	AuthMode        string   `yaml:"auth_mode"`
	ServerName      string   `yaml:"server_name"`
	RateLimitRPS    int      `yaml:"rate_limit_rps"`
	RateLimitBurst  int      `yaml:"rate_limit_burst"`
	TrustedProxies  []string `yaml:"trusted_proxies"`
	PathExcludes    []string `yaml:"path_excludes"`
	SecretPatterns  []string `yaml:"secret_patterns"`
	DoclingCommand  string   `yaml:"docling_command"`
	DoclingServeURL string   `yaml:"docling_serve_url"`
	PandocCommand   string   `yaml:"pandoc_command"`
	// optional session timeouts expressed as YAML duration strings
	SessionInactivityTimeout time.Duration `yaml:"session_inactivity_timeout"`
	SessionMaxLifetime       time.Duration `yaml:"session_max_lifetime"`
	HealthCheckInterval      time.Duration `yaml:"health_check_interval"`

	ElevenLabsBaseURL                  string        `yaml:"elevenlabs_base_url"`
	ElevenLabsTTSVoiceID               string        `yaml:"elevenlabs_tts_voice_id"`
	AllowedOrigins                     []string      `yaml:"allowed_origins"`
	RAGSystemPrompt                    string        `yaml:"rag_system_prompt"`
	RAGGenerateAnswer                  bool          `yaml:"rag_generate_answer"`
	RAGKDefault                        int           `yaml:"rag_k_default"`
	RAGMaxContextChars                 int           `yaml:"rag_max_context_chars"`
	RAGOversampleFactor                int           `yaml:"rag_oversample_factor"`
	RetrievalHybridEnabled             bool          `yaml:"retrieval_hybrid_enabled"`
	DedupRetrieval                     bool          `yaml:"dedup_retrieval"`
	RetrievalMinScore                  float64       `yaml:"retrieval_min_score"`
	RetrievalRecencyHalfLife           time.Duration `yaml:"retrieval_recency_half_life"`
	ContextCompressionEnabled          bool          `yaml:"context_compression_enabled"`
	ContextCompressionTargetRatio      float64       `yaml:"context_compression_target_ratio"`
	RetrievalAdaptiveEnabled           bool          `yaml:"retrieval_adaptive_enabled"`
	RetrievalAdaptiveKMin              int           `yaml:"retrieval_adaptive_k_min"`
	RetrievalAdaptiveKMax              int           `yaml:"retrieval_adaptive_k_max"`
	RetrievalMMREnabled                bool          `yaml:"retrieval_mmr_enabled"`
	RetrievalMMRLambda                 float64       `yaml:"retrieval_mmr_lambda"`
	RetrievalHyDEEnabled               bool          `yaml:"retrieval_hyde_enabled"`
	RetrievalHyDEMode                  string        `yaml:"retrieval_hyde_mode"`
	RetrievalContextualEnabled         bool          `yaml:"retrieval_contextual_enabled"`
	RetrievalContextualProvider        string        `yaml:"retrieval_contextual_provider"`
	RetrievalContextualModel           string        `yaml:"retrieval_contextual_model"`
	RetrievalContextualMaxTokens       int           `yaml:"retrieval_contextual_max_tokens"`
	RetrievalContextualPromptVersion   string        `yaml:"retrieval_contextual_prompt_version"`
	RetrievalContextualPrompt          string        `yaml:"retrieval_contextual_prompt"`
	CrossLingualEnabled                bool          `yaml:"cross_lingual_enabled"`
	CrossLingualTargetLangs            []string      `yaml:"cross_lingual_target_langs"`
	RetrievalHierarchicalEnabled       bool          `yaml:"retrieval_hierarchical_enabled"`
	RetrievalHierarchicalSourceReps    []string      `yaml:"retrieval_hierarchical_source_reps"`
	RetrievalHierarchicalLevels        []string      `yaml:"retrieval_hierarchical_levels"`
	RetrievalHierarchicalProvider      string        `yaml:"retrieval_hierarchical_provider"`
	RetrievalHierarchicalMaxTokens     int           `yaml:"retrieval_hierarchical_max_tokens"`
	RetrievalHierarchicalPromptVersion string        `yaml:"retrieval_hierarchical_prompt_version"`
	RetrievalHierarchicalPrompt        string        `yaml:"retrieval_hierarchical_prompt"`
	RerankEnabled                      bool          `yaml:"rerank_enabled"`
	RerankProvider                     string        `yaml:"rerank_provider"`
	CohereBaseURL                      string        `yaml:"cohere_base_url"`
	RerankModel                        string        `yaml:"rerank_model"`
	RerankCandidatePool                int           `yaml:"rerank_candidate_pool"`
	ChunkingMaxTokens                  int           `yaml:"chunking_max_tokens"`
	ChunkingOverlapTokens              int           `yaml:"chunking_overlap_tokens"`
	IngestGitignore                    bool          `yaml:"ingest_gitignore"`
	IngestFollowSymlinks               bool          `yaml:"ingest_follow_symlinks"`
	IngestMaxFileMB                    int           `yaml:"ingest_max_file_mb"`
	IngestExcludeDirs                  []string      `yaml:"ingest_exclude_dirs"`
	IngestPDFMode                      string        `yaml:"ingest_pdf_mode"`
	IngestImagesMode                   string        `yaml:"ingest_images_mode"`
	IngestAudioMode                    string        `yaml:"ingest_audio_mode"`
	IngestArchivesMode                 string        `yaml:"ingest_archives_mode"`
	IngestExtractor                    string        `yaml:"ingest_extractor"`
	IngestOnUnsupported                string        `yaml:"ingest_on_unsupported"`
	IndexBackend                       string        `yaml:"index_backend"`
	IngestScanCache                    bool          `yaml:"ingest_scan_cache"`
	IngestLateChunking                 bool          `yaml:"ingest_late_chunking"`
	IngestWatch                        bool          `yaml:"ingest_watch"`
	IngestWatchDebounce                time.Duration `yaml:"ingest_watch_debounce"`
	STTProvider                        string        `yaml:"stt_provider"`
	STTMistralModel                    string        `yaml:"stt_mistral_model"`
	STTElevenLabsModel                 string        `yaml:"stt_elevenlabs_model"`
	STTElevenLabsLanguageCode          string        `yaml:"stt_elevenlabs_language_code"`
	RecognizeProvider                  string        `yaml:"recognize_provider"`
	RecognizeServeURL                  string        `yaml:"recognize_serve_url"`
	RecognizeServeCommand              string        `yaml:"recognize_serve_command"`
	QualityGatesEnabled                bool          `yaml:"quality_gates_enabled"`
	LanguageDetectionEnabled           bool          `yaml:"language_detection_enabled"`
	MediaSidecarsDisabled              bool          `yaml:"media_sidecars_disabled"`
	MediaVariantsGroup                 bool          `yaml:"media_variants_group"`
	MediaVariantsSelect                string        `yaml:"media_variants_select"`
	MediaTranslateEnabled              bool          `yaml:"media_translate_enabled"`
	MediaTranslateTargetLangs          []string      `yaml:"media_translate_target_langs"`
	MediaTranslateEngine               string        `yaml:"media_translate_engine"`
	MediaTranslateWhisperWindowSec     int           `yaml:"media_translate_whisper_window_sec"`
	MediaTranslateWindowLines          int           `yaml:"media_translate_window_lines"`
	MediaTranslateContextLines         int           `yaml:"media_translate_context_lines"`
	MediaFilterWords                   []string      `yaml:"media_filter_words"`
	MediaSubtitlesTTMLEnabled          bool          `yaml:"media_subtitles_ttml_enabled"`
	MediaSubtitlesTTMLAlignToleranceMS int           `yaml:"media_subtitles_ttml_align_tolerance_ms"`
	MediaSubtitlesSMILEnabled          bool          `yaml:"media_subtitles_smil_enabled"`
	MediaSubtitlesSegmentation         string        `yaml:"media_subtitles_segmentation"`
	MediaSubtitlesGlossary             []string      `yaml:"media_subtitles_glossary"`
	MediaSubtitlesDropPhrases          []string      `yaml:"media_subtitles_drop_phrases"`
	MediaSubtitlesScrubPhrases         []string      `yaml:"media_subtitles_scrub_phrases"`
	MediaSubtitlesCollapseRepeats      int           `yaml:"media_subtitles_collapse_repeats"`
	MediaSubtitlesDropURLs             bool          `yaml:"media_subtitles_drop_urls"`
	MediaTrimLeadingSilence            bool          `yaml:"media_trim_leading_silence"`
	MediaSilenceThresholdDB            float64       `yaml:"media_silence_threshold_db"`
	MediaVAD                           bool          `yaml:"media_vad"`
	MediaAudioWindowSec                int           `yaml:"media_audio_window_sec"`
	MediaVideoWindowSec                int           `yaml:"media_video_window_sec"`
	MediaClipMaxDurationMS             int           `yaml:"media_clip_max_duration_ms"`
	MediaClipMaxBytes                  int           `yaml:"media_clip_max_bytes"`
	MediaSTTMaxPayloadMB               int           `yaml:"media_stt_max_payload_mb"`
	MediaSTTRequestTimeoutSec          int           `yaml:"media_stt_request_timeout_sec"`
	MediaSTTLanguageStrict             bool          `yaml:"media_stt_language_strict"`
	MediaSTTOnUncoveredLanguage        string        `yaml:"media_stt_on_uncovered_language"`
	MediaSTTTracks                     []string      `yaml:"media_stt_tracks"`
	MediaBatchTwoPhase                 bool          `yaml:"media_batch_two_phase"`
	MediaBatchProgress                 bool          `yaml:"media_batch_progress"`
	MediaBatchManifest                 string        `yaml:"media_batch_manifest"`
	// MediaDiarizeEnabled is the tri-state diarization opt (SPEC §8.6.8): a
	// *bool so the snapshot can round-trip omitted (nil) vs. false vs. true
	// without collapsing the auto/off distinction.
	MediaDiarizeEnabled *bool  `yaml:"media_diarize_enabled,omitempty"`
	ServerTLSCertFile   string `yaml:"server_tls_cert_file"`
	ServerTLSKeyFile    string `yaml:"server_tls_key_file"`

	// The following fields configure optional x402 payment gating.  The
	// facilitator token itself is treated like any other sensitive API key:
	// it **must not** be written to disk and is therefore *not* part of the
	// persisted configuration struct.  Operators should provide the token via
	// DIR2MCP_X402_FACILITATOR_TOKEN (environment/CLI) when running the
	// application; the loader ignores file-supplied token values.
	//
	// Other x402 settings *are* persisted and must be declared in the
	// configuration file when required.
	X402Mode             string `yaml:"x402_mode"`
	X402FacilitatorURL   string `yaml:"x402_facilitator_url"`
	X402ResourceBaseURL  string `yaml:"x402_resource_base_url"`
	X402ToolsCallEnabled bool   `yaml:"x402_tools_call_enabled"`
	X402PriceAtomic      string `yaml:"x402_price_atomic"`
	X402Network          string `yaml:"x402_network"`
	X402Scheme           string `yaml:"x402_scheme"`
	X402Asset            string `yaml:"x402_asset"`
	X402PayTo            string `yaml:"x402_pay_to"`

	// Corpus source selection (issue #244). Credentials are never persisted.
	SourceKind       string `yaml:"source_kind"`
	SourceS3Bucket   string `yaml:"source_s3_bucket"`
	SourceS3Prefix   string `yaml:"source_s3_prefix"`
	SourceS3Region   string `yaml:"source_s3_region"`
	SourceS3Endpoint string `yaml:"source_s3_endpoint"`

	// Qdrant vector backend selection (issue #268). The api_key is a secret and
	// is intentionally NOT declared here so it can never be written to disk.
	QdrantURL        string `yaml:"qdrant_url"`
	QdrantCollection string `yaml:"qdrant_collection"`

	// pgvector backend selection (issue #269). The DSN is sensitive and, like the
	// Qdrant api_key, is intentionally NOT declared here so it can never be
	// written to disk: it is sourced from DIR2MCP_INDEX_PGVECTOR_DSN (or the
	// secret store) at runtime. Schema/table are non-sensitive invariants.
	IndexPgvectorSchema string `yaml:"index_pgvector_schema"`
	IndexPgvectorTable  string `yaml:"index_pgvector_table"`

	// Distributed embedding (issue #248, SPEC §8.7). The broker URL is sensitive
	// (may embed credentials) and is intentionally NOT declared here so it can
	// never be written to disk; it is sourced from DIR2MCP_DISTRIBUTED_EMBED_BROKER_URL
	// at runtime (SPEC §16.1.1). The enable flag, broker selector, SQLite path,
	// and max-attempts are non-sensitive invariants.
	DistributedEmbedEnabled     bool   `yaml:"distributed_embed_enabled"`
	DistributedEmbedBroker      string `yaml:"distributed_embed_broker"`
	DistributedEmbedSQLitePath  string `yaml:"distributed_embed_sqlite_path"`
	DistributedEmbedMaxAttempts int    `yaml:"distributed_embed_max_attempts"`
}

// Default returns the baseline Config (used as the starting point
// before dotenv/env/file overrides are layered on).
func Default() Config {
	return Config{
		RootDir:         ".",
		StateDir:        filepath.Join(".", ".dir2mcp"),
		ListenAddr:      "127.0.0.1:0",
		MCPPath:         "/mcp",
		ProtocolVersion: DefaultProtocolVersion,
		Public:          false,
		AuthMode:        "auto",
		RateLimitRPS:    60,
		RateLimitBurst:  20,
		// default session inactivity matches previous hardcoded sessionTTL (24h)
		SessionInactivityTimeout: 24 * time.Hour,
		SessionMaxLifetime:       0,
		HealthCheckInterval:      5 * time.Second,
		TrustedProxies: []string{
			"127.0.0.1/32",
			"::1/128",
		},
		PathExcludes: []string{
			"**/.git/**",
			"**/.dir2mcp/**",
			"**/node_modules/**",
			"**/vendor/**",
			"**/__pycache__/**",
			"**/.env",
			"**/*.pem",
			"**/*.key",
			"**/id_rsa",
			// dir2mcp's own support bundles: `support-bundle` writes
			// dir2mcp-support-<ts>.tar.gz into the working dir, and deep archive
			// ingestion would otherwise unpack and index its logs/config —
			// poisoning the knowledge corpus with dir2mcp's own diagnostics
			// (issue #366). Excluding the archive also skips its extraction.
			"**/dir2mcp-support-*.tar.gz",
		},
		SecretPatterns: []string{
			`AKIA[0-9A-Z]{16}`,
			`(?i)(?:aws(?:[_\s.]{0,20})?secret(?:[_\s.]*(?:access[_\s.]*)?key)?|secret[_\s.]*access[_\s.]*key)\s*[:=]\s*[0-9A-Za-z/+=]{20,}`,
			`(?i)(?:authorization\s*[:=]\s*bearer\s+|(?:access|id|refresh)_token\s*[:=]\s*)[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`,
			`(?i)token\s*[:=]\s*[A-Za-z0-9_.-]{20,}`,
			`sk_[a-z0-9]{32}|api_[A-Za-z0-9]{32}`,
			// OpenAI/Mistral-style hyphenated secret keys, e.g. "sk-proj-..."
			// and "sk-...". Case-sensitive lowercase "sk-" prefix; the {20,}
			// run of key chars keeps this from matching ordinary hyphenated
			// prose (issue #443).
			`sk-[A-Za-z0-9-]{20,}`,
			// Generic provider-key assignment, e.g. "MISTRAL_API_KEY=<32 alnum>",
			// "COHERE_API_KEY: ...", "GEMINI_API_KEY=...". Requires an
			// uppercase provider prefix plus a 16+ char non-whitespace value so
			// it gates real credentials without flagging plain text (issue #443).
			`[A-Z][A-Z0-9_]*API_KEY\s*[:=]\s*\S{16,}`,
		},
		ElevenLabsAPIKey:     "",
		ElevenLabsBaseURL:    "",
		ElevenLabsTTSVoiceID: "JBFqnCBsd6RMkjVDRZzb",
		AllowedOrigins: []string{
			"http://localhost",
			"http://127.0.0.1",
		},
		RAGSystemPrompt:        "",
		RAGGenerateAnswer:      true,
		RAGKDefault:            RAGKFallback,
		RAGMaxContextChars:     20000,
		RAGOversampleFactor:    5,
		RetrievalHybridEnabled: true,
		// DedupRetrieval defaults to false (SPEC 9.2): retrieval-time
		// cross-file de-duplication is off unless explicitly enabled.
		DedupRetrieval: false,
		// RetrievalMinScore ships ENABLED (SPEC §9.4.3): omitting the key keeps
		// the relevance floor on at defaultRetrievalMinScore. `0` disables it.
		RetrievalMinScore: defaultRetrievalMinScore,
		// RetrievalRecencyHalfLife defaults to 0 (disabled): no time-decay is
		// applied unless explicitly configured.
		RetrievalRecencyHalfLife: 0,
		// ContextCompressionEnabled defaults to false: the Ask path sends raw
		// snippets unchanged unless compression is explicitly enabled.
		ContextCompressionEnabled: false,
		// ContextCompressionTargetRatio defaults to 0 (use the built-in 0.5
		// keep-ratio); it only takes effect when compression is enabled.
		ContextCompressionTargetRatio: 0,
		// RetrievalAdaptiveEnabled defaults to false: the adaptive retrieval
		// gate is opt-in, so behavior is today's fixed-k path unless enabled.
		RetrievalAdaptiveEnabled: false,
		// RetrievalAdaptiveKMin / RetrievalAdaptiveKMax bound the dynamic k the
		// gate may choose. These defaults give the heuristic room to narrow easy
		// queries and widen hard ones around the shipped rag.k_default (15).
		RetrievalAdaptiveKMin: 4,
		RetrievalAdaptiveKMax: 30,
		// MMR diversity re-ordering defaults to OFF (issue #340): candidate order
		// is unchanged unless explicitly enabled. Lambda carries its balanced 0.5
		// default so an enabled-but-unspecified config behaves predictably.
		RetrievalMMREnabled: false,
		RetrievalMMRLambda:  0.5,
		// RetrievalHyDEEnabled defaults to false: the HyDE query transform is
		// off unless explicitly enabled, so default behavior is unchanged.
		RetrievalHyDEEnabled: false,
		// RetrievalHyDEMode defaults to "fuse": when HyDE is enabled the
		// hypothetical-document hits are RRF-fused with the raw-query hits.
		RetrievalHyDEMode: HyDEModeFuse,
		// Contextual retrieval (SPEC §8.1.8, #330) defaults to OFF: chunks embed
		// raw and the embed identity's terminal component stays "off", so an
		// existing corpus is byte-identical to before the feature existed. The
		// bound and prompt version carry their defaults so an enabled-but-
		// unspecified config behaves predictably.
		RetrievalContextualEnabled:       false,
		RetrievalContextualProvider:      "",
		RetrievalContextualModel:         "",
		RetrievalContextualMaxTokens:     DefaultContextualMaxTokens,
		RetrievalContextualPromptVersion: ContextualPromptVersionV1,
		RetrievalContextualPrompt:        "",
		// CrossLingualEnabled defaults to false (disabled): cross-lingual query
		// expansion is off unless explicitly enabled. The target-langs list is
		// left empty, which means "auto" (the corpus's detected languages).
		CrossLingualEnabled:     false,
		CrossLingualTargetLangs: nil,
		// Hierarchical (coarse-to-fine) retrieval defaults to OFF (SPEC §9.7): no
		// summary representations are derived and retrieval is exactly the flat
		// path. The remaining knobs carry their spec defaults so an enabled-but-
		// unspecified config behaves predictably: source_reps empty ⇒ "auto" (the
		// document's primary retrievable text representation), document-level
		// summaries only, a 512-token generation bound, and the built-in v1 prompt.
		RetrievalHierarchicalEnabled:       false,
		RetrievalHierarchicalSourceReps:    nil,
		RetrievalHierarchicalLevels:        []string{HierarchicalLevelDocument},
		RetrievalHierarchicalMaxTokens:     DefaultHierarchicalMaxTokens,
		RetrievalHierarchicalPromptVersion: DefaultHierarchicalPromptVersion,
		// RerankEnabled left nil: auto mode (activates iff a rerank
		// provider credential is present). See rerankEnabledEffective.
		RerankProvider:            "cohere",
		CohereAPIKey:              "",
		CohereBaseURL:             "",
		RerankModel:               "rerank-v3.5",
		RerankCandidatePool:       50,
		ChunkingMaxTokens:         0,
		ChunkingOverlapTokens:     0,
		IngestGitignore:           true,
		IngestFollowSymlinks:      false,
		IngestMaxFileMB:           20,
		IngestExcludeDirs:         corpusfs.DefaultExcludedDirs(),
		IngestPDFMode:             "ocr",
		IngestImagesMode:          "ocr_auto",
		IngestAudioMode:           "auto",
		IngestArchivesMode:        "deep",
		IngestExtractor:           "auto",
		IngestOnUnsupported:       "lenient",
		IndexBackend:              "memory",
		IngestScanCache:           false,
		IngestLateChunking:        false,
		IngestWatch:               false,
		IngestWatchDebounce:       500 * time.Millisecond,
		STTProvider:               "mistral",
		RecognizeProvider:         "off",
		STTMistralModel:           "voxtral-mini-latest",
		STTElevenLabsModel:        "scribe_v1",
		STTElevenLabsLanguageCode: "",
		QualityGatesEnabled:       true,
		LanguageDetectionEnabled:  true,
		MediaClipMaxDurationMS:    DefaultMediaClipMaxDurationMS,
		MediaClipMaxBytes:         DefaultMediaClipMaxBytes,
		// 0 = use the whisper client's built-in caps (#510, #511).
		MediaSTTMaxPayloadMB:      0,
		MediaSTTRequestTimeoutSec: 0,
		// A pinned STT language is a provider hint, not per-file ground truth, so
		// it does not drive quality-gate quarantine by default (dir2mcp#439 F3).
		MediaSTTLanguageStrict: false,
		// Honest-coverage floor action (SPEC §8.2.1, #566): fail-open by default —
		// transcribe an uncovered-language item anyway and record the fact — so
		// behavior is unchanged unless an operator opts into strict skipping.
		MediaSTTOnUncoveredLanguage: onUncoveredLanguageWarn,
		MediaVariantsGroup:          false,
		MediaVariantsSelect:         "best",
		MediaTranslateEnabled:       false,
		MediaTranslateTargetLangs:   nil,
		MediaTranslateEngine:        "chat",
		// Cross-line context for the chat translate engine (issue #573): translate
		// cues in windows of this many, each with a small read-only margin, so the
		// model sees neighbouring cues. On by default (quality fix); the 1:1
		// numbered contract + per-window fallback keep subtitle timing exact.
		MediaTranslateWindowLines:  DefaultMediaTranslateWindowLines,
		MediaTranslateContextLines: DefaultMediaTranslateContextLines,
		// Bilingual subtitle export (SPEC §8.6.10) is OFF by default; the align
		// tolerance carries its spec default so an enabled-but-unspecified config
		// behaves predictably.
		MediaSubtitlesTTMLEnabled:          false,
		MediaSubtitlesTTMLAlignToleranceMS: DefaultMediaSubtitlesAlignToleranceMS,
		MediaSubtitlesSMILEnabled:          false,
		MediaSubtitlesSegmentation:         "chunk",
		// Export-time cue cleaning (clean_srt.py port) is OFF by default: no
		// glossary rewrites, no phrase drops, no repetition-collapse (< 2
		// disables), no URL drop.
		MediaSubtitlesGlossary:        nil,
		MediaSubtitlesDropPhrases:     nil,
		MediaSubtitlesScrubPhrases:    nil,
		MediaSubtitlesCollapseRepeats: 0,
		MediaSubtitlesDropURLs:        false,
		ServerTLSCertFile:             "",
		ServerTLSKeyFile:              "",
		X402: X402Config{
			Mode:             "off",
			FacilitatorURL:   "",
			FacilitatorToken: "",
			ResourceBaseURL:  "",
			ToolsCallEnabled: true,
			PriceAtomic:      "1000",
			Network:          "",
			Scheme:           "exact",
			Asset:            "",
			PayTo:            "",
		},
		Source: SourceConfig{
			Kind: "local",
		},
		Qdrant: QdrantConfig{
			URL:        "",
			Collection: "",
			APIKey:     "",
		},
	}
}

// Load returns defaults with dotenv/env overrides and, if path is
// non-empty and exists, the YAML config file layered on top.
func Load(path string) (Config, error) {
	return load(path, nil, true)
}

// LoadFile loads defaults plus an optional YAML config file and does not
// apply dotenv/env overrides. This is useful for config init/update flows.
func LoadFile(path string) (Config, error) {
	return load(path, nil, false)
}

// buildPersistedConfig projects a Config onto persistedConfig, the
// subset that is safe to write to disk (secrets are intentionally
// excluded so they never land in the YAML/snapshot).
func buildPersistedConfig(cfg *Config) persistedConfig {
	if cfg == nil {
		return persistedConfig{}
	}

	return persistedConfig{
		RootDir:                            cfg.RootDir,
		StateDir:                           cfg.StateDir,
		ListenAddr:                         cfg.ListenAddr,
		MCPPath:                            cfg.MCPPath,
		ProtocolVersion:                    cfg.ProtocolVersion,
		Public:                             cfg.Public,
		AuthMode:                           cfg.AuthMode,
		ServerName:                         cfg.ServerName,
		RateLimitRPS:                       cfg.RateLimitRPS,
		RateLimitBurst:                     cfg.RateLimitBurst,
		TrustedProxies:                     append([]string(nil), cfg.TrustedProxies...),
		PathExcludes:                       append([]string(nil), cfg.PathExcludes...),
		SecretPatterns:                     append([]string(nil), cfg.SecretPatterns...),
		DoclingCommand:                     cfg.DoclingCommand,
		DoclingServeURL:                    cfg.IngestDoclingServeURL,
		PandocCommand:                      cfg.IngestPandocCommand,
		SessionInactivityTimeout:           cfg.SessionInactivityTimeout,
		SessionMaxLifetime:                 cfg.SessionMaxLifetime,
		HealthCheckInterval:                cfg.HealthCheckInterval,
		ElevenLabsBaseURL:                  cfg.ElevenLabsBaseURL,
		ElevenLabsTTSVoiceID:               cfg.ElevenLabsTTSVoiceID,
		AllowedOrigins:                     append([]string(nil), cfg.AllowedOrigins...),
		RAGSystemPrompt:                    cfg.RAGSystemPrompt,
		RAGGenerateAnswer:                  cfg.RAGGenerateAnswer,
		RAGKDefault:                        cfg.RAGKDefault,
		RAGMaxContextChars:                 cfg.RAGMaxContextChars,
		RAGOversampleFactor:                cfg.RAGOversampleFactor,
		RetrievalHybridEnabled:             cfg.RetrievalHybridEnabled,
		DedupRetrieval:                     cfg.DedupRetrieval,
		RetrievalMinScore:                  cfg.RetrievalMinScore,
		RetrievalRecencyHalfLife:           cfg.RetrievalRecencyHalfLife,
		ContextCompressionEnabled:          cfg.ContextCompressionEnabled,
		ContextCompressionTargetRatio:      cfg.ContextCompressionTargetRatio,
		RetrievalAdaptiveEnabled:           cfg.RetrievalAdaptiveEnabled,
		RetrievalAdaptiveKMin:              cfg.RetrievalAdaptiveKMin,
		RetrievalAdaptiveKMax:              cfg.RetrievalAdaptiveKMax,
		RetrievalMMREnabled:                cfg.RetrievalMMREnabled,
		RetrievalMMRLambda:                 cfg.RetrievalMMRLambda,
		RetrievalHyDEEnabled:               cfg.RetrievalHyDEEnabled,
		RetrievalHyDEMode:                  cfg.RetrievalHyDEMode,
		RetrievalContextualEnabled:         cfg.RetrievalContextualEnabled,
		RetrievalContextualProvider:        cfg.RetrievalContextualProvider,
		RetrievalContextualModel:           cfg.RetrievalContextualModel,
		RetrievalContextualMaxTokens:       cfg.RetrievalContextualMaxTokens,
		RetrievalContextualPromptVersion:   cfg.RetrievalContextualPromptVersion,
		RetrievalContextualPrompt:          cfg.RetrievalContextualPrompt,
		CrossLingualEnabled:                cfg.CrossLingualEnabled,
		CrossLingualTargetLangs:            append([]string(nil), cfg.CrossLingualTargetLangs...),
		RetrievalHierarchicalEnabled:       cfg.RetrievalHierarchicalEnabled,
		RetrievalHierarchicalSourceReps:    append([]string(nil), cfg.RetrievalHierarchicalSourceReps...),
		RetrievalHierarchicalLevels:        append([]string(nil), cfg.RetrievalHierarchicalLevels...),
		RetrievalHierarchicalProvider:      cfg.RetrievalHierarchicalProvider,
		RetrievalHierarchicalMaxTokens:     cfg.RetrievalHierarchicalMaxTokens,
		RetrievalHierarchicalPromptVersion: cfg.RetrievalHierarchicalPromptVersion,
		RetrievalHierarchicalPrompt:        cfg.RetrievalHierarchicalPrompt,
		RerankEnabled:                      rerankEnabledEffective(cfg),
		RerankProvider:                     cfg.RerankProvider,
		CohereBaseURL:                      cfg.CohereBaseURL,
		RerankModel:                        cfg.RerankModel,
		RerankCandidatePool:                cfg.RerankCandidatePool,
		ChunkingMaxTokens:                  cfg.ChunkingMaxTokens,
		ChunkingOverlapTokens:              cfg.ChunkingOverlapTokens,
		IngestGitignore:                    cfg.IngestGitignore,
		IngestFollowSymlinks:               cfg.IngestFollowSymlinks,
		IngestMaxFileMB:                    cfg.IngestMaxFileMB,
		IngestExcludeDirs:                  append([]string(nil), cfg.IngestExcludeDirs...),
		IngestPDFMode:                      cfg.IngestPDFMode,
		IngestImagesMode:                   cfg.IngestImagesMode,
		IngestAudioMode:                    cfg.IngestAudioMode,
		IngestArchivesMode:                 cfg.IngestArchivesMode,
		IngestExtractor:                    cfg.IngestExtractor,
		IngestOnUnsupported:                cfg.IngestOnUnsupported,
		IndexBackend:                       cfg.IndexBackend,
		IngestScanCache:                    cfg.IngestScanCache,
		IngestLateChunking:                 cfg.IngestLateChunking,
		IngestWatch:                        cfg.IngestWatch,
		IngestWatchDebounce:                cfg.IngestWatchDebounce,
		STTProvider:                        cfg.STTProvider,
		STTMistralModel:                    cfg.STTMistralModel,
		STTElevenLabsModel:                 cfg.STTElevenLabsModel,
		STTElevenLabsLanguageCode:          cfg.STTElevenLabsLanguageCode,
		RecognizeProvider:                  cfg.RecognizeProvider,
		RecognizeServeURL:                  cfg.RecognizeServeURL,
		RecognizeServeCommand:              cfg.RecognizeServeCommand,
		QualityGatesEnabled:                cfg.QualityGatesEnabled,
		LanguageDetectionEnabled:           cfg.LanguageDetectionEnabled,
		MediaSidecarsDisabled:              cfg.MediaSidecarsDisabled,
		MediaVariantsGroup:                 cfg.MediaVariantsGroup,
		MediaVariantsSelect:                cfg.MediaVariantsSelect,
		MediaTranslateEnabled:              cfg.MediaTranslateEnabled,
		MediaTranslateTargetLangs:          append([]string(nil), cfg.MediaTranslateTargetLangs...),
		MediaTranslateEngine:               cfg.MediaTranslateEngine,
		MediaTranslateWhisperWindowSec:     cfg.MediaTranslateWhisperWindowSec,
		MediaTranslateWindowLines:          cfg.MediaTranslateWindowLines,
		MediaTranslateContextLines:         cfg.MediaTranslateContextLines,
		MediaFilterWords:                   append([]string(nil), cfg.MediaFilterWords...),
		MediaSubtitlesTTMLEnabled:          cfg.MediaSubtitlesTTMLEnabled,
		MediaSubtitlesTTMLAlignToleranceMS: cfg.MediaSubtitlesTTMLAlignToleranceMS,
		MediaSubtitlesSMILEnabled:          cfg.MediaSubtitlesSMILEnabled,
		MediaSubtitlesSegmentation:         cfg.MediaSubtitlesSegmentation,
		MediaSubtitlesGlossary:             append([]string(nil), cfg.MediaSubtitlesGlossary...),
		MediaSubtitlesDropPhrases:          append([]string(nil), cfg.MediaSubtitlesDropPhrases...),
		MediaSubtitlesScrubPhrases:         append([]string(nil), cfg.MediaSubtitlesScrubPhrases...),
		MediaSubtitlesCollapseRepeats:      cfg.MediaSubtitlesCollapseRepeats,
		MediaSubtitlesDropURLs:             cfg.MediaSubtitlesDropURLs,
		MediaTrimLeadingSilence:            cfg.MediaTrimLeadingSilence,
		MediaSilenceThresholdDB:            cfg.MediaSilenceThresholdDB,
		MediaVAD:                           cfg.MediaVAD,
		MediaDiarizeEnabled:                copyBoolPtr(cfg.MediaDiarizeEnabled),
		MediaAudioWindowSec:                cfg.MediaAudioWindowSec,
		MediaVideoWindowSec:                cfg.MediaVideoWindowSec,
		MediaClipMaxDurationMS:             cfg.MediaClipMaxDurationMS,
		MediaClipMaxBytes:                  cfg.MediaClipMaxBytes,
		MediaSTTMaxPayloadMB:               cfg.MediaSTTMaxPayloadMB,
		MediaSTTRequestTimeoutSec:          cfg.MediaSTTRequestTimeoutSec,
		MediaSTTLanguageStrict:             cfg.MediaSTTLanguageStrict,
		MediaSTTOnUncoveredLanguage:        cfg.MediaSTTOnUncoveredLanguage,
		MediaSTTTracks:                     append([]string(nil), cfg.MediaSTTTracks...),
		ServerTLSCertFile:                  cfg.ServerTLSCertFile,
		ServerTLSKeyFile:                   cfg.ServerTLSKeyFile,
		X402Mode:                           cfg.X402.Mode,
		X402FacilitatorURL:                 cfg.X402.FacilitatorURL,
		MediaBatchTwoPhase:                 cfg.MediaBatchTwoPhase,
		MediaBatchProgress:                 cfg.MediaBatchProgress,
		MediaBatchManifest:                 cfg.MediaBatchManifest,
		// token intentionally omitted to avoid persisting secrets
		// X402FacilitatorToken: cfg.X402.FacilitatorToken,
		X402ResourceBaseURL:  cfg.X402.ResourceBaseURL,
		X402ToolsCallEnabled: cfg.X402.ToolsCallEnabled,
		X402PriceAtomic:      cfg.X402.PriceAtomic,
		X402Network:          cfg.X402.Network,
		X402Scheme:           cfg.X402.Scheme,
		X402Asset:            cfg.X402.Asset,
		X402PayTo:            cfg.X402.PayTo,
		// Source selection is safe to persist except the resolved credentials,
		// which are intentionally omitted (env/keychain-only).
		SourceKind:       cfg.Source.Kind,
		SourceS3Bucket:   cfg.Source.S3Bucket,
		SourceS3Prefix:   cfg.Source.S3Prefix,
		SourceS3Region:   cfg.Source.S3Region,
		SourceS3Endpoint: cfg.Source.S3Endpoint,
		// Qdrant url/collection are persistable invariants; the api_key is a
		// secret and is intentionally omitted so it never lands in the YAML.
		QdrantURL:        cfg.Qdrant.URL,
		QdrantCollection: cfg.Qdrant.Collection,
		// pgvector schema/table are persistable invariants; the DSN is a secret
		// and intentionally omitted (sourced from env/secret store at runtime).
		IndexPgvectorSchema: cfg.IndexPgvectorSchema,
		IndexPgvectorTable:  cfg.IndexPgvectorTable,
		// Distributed embedding (issue #248): enable flag, broker selector,
		// SQLite path, and max-attempts are persistable invariants. BrokerURL is a
		// secret (may embed credentials) and is intentionally omitted so it never
		// lands in the YAML/snapshot (SPEC §16.1.1).
		DistributedEmbedEnabled:     cfg.DistributedEmbed.Enabled,
		DistributedEmbedBroker:      cfg.DistributedEmbed.Broker,
		DistributedEmbedSQLitePath:  cfg.DistributedEmbed.BrokerSQLitePath,
		DistributedEmbedMaxAttempts: cfg.DistributedEmbed.MaxAttempts,
	}
}

// SaveFile validates cfg and writes its persistable subset to path as
// flat-key YAML (secrets excluded), creating parent directories.
func SaveFile(path string, cfg Config) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("config path is required")
	}

	// validate before persisting so callers don't accidentally write
	// nonsensical values (negative durations, mismatched session
	// lifetimes, etc.).  the error is wrapped to make the origin clear.
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	serializable := buildPersistedConfig(&cfg)

	raw, err := marshalConfigYAML(serializable)
	if err != nil {
		return fmt.Errorf("marshal config yaml: %w", err)
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		raw = append(raw, '\n')
	}

	// Deliberately not owner-only: this is the operator's own .dir2mcp.yaml
	// in their own directory, not the derived state tree (#726).
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("write config file %s: %w", path, err)
	}
	return nil
}

// EffectiveSnapshotPath returns the snapshot file path under stateDir,
// falling back to the default state dir when stateDir is empty.
func EffectiveSnapshotPath(stateDir string) string {
	trimmed := strings.TrimSpace(stateDir)
	if trimmed == "" {
		trimmed = Default().StateDir
	}
	return filepath.Join(trimmed, EffectiveConfigSnapshotFile)
}

// SaveEffectiveSnapshot validates cfg and writes the effective config
// snapshot (persistable subset plus secret-source metadata and the
// resolved embed identity) with 0600 perms, returning its path.
func SaveEffectiveSnapshot(cfg Config, sources SecretSourceMetadata) (string, error) {
	if err := cfg.Validate(); err != nil {
		return "", fmt.Errorf("validate config: %w", err)
	}

	path := EffectiveSnapshotPath(cfg.StateDir)
	serializable := buildPersistedConfig(&cfg)

	raw, err := marshalConfigYAML(serializable)
	if err != nil {
		return "", fmt.Errorf("marshal snapshot yaml: %w", err)
	}
	raw = appendSnapshotSecretSourceMetadata(raw, sources)
	providers := cfg.Providers()
	raw = appendSnapshotEmbedIdentity(raw, providers.EmbedIdentity())
	raw = appendSnapshotEmbedBaseURL(raw, providers.EmbedBaseURL())
	raw = appendSnapshotEmbedContextual(raw, providers.EmbedContextual())
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		raw = append(raw, '\n')
	}

	// The snapshot's directory IS the state directory, and this is often what
	// creates it: any command that loads config against a fresh state path
	// arrives here first. Creating it 0755 left the whole derived tree
	// world-readable even though the snapshot itself was 0600 (#726).
	if err := statefs.MkdirAllHardened(filepath.Dir(path)); err != nil {
		return "", fmt.Errorf("create snapshot directory: %w", err)
	}
	if err := statefs.WriteFile(path, raw); err != nil {
		return "", fmt.Errorf("write snapshot file %s: %w", path, err)
	}
	return path, nil
}

// LoadEffectiveSnapshot reads a snapshot written by
// SaveEffectiveSnapshot, returning the layered+validated Config and the
// recorded secret-source metadata.
func LoadEffectiveSnapshot(path string) (Config, SecretSourceMetadata, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, SecretSourceMetadata{}, fmt.Errorf("read snapshot file %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return Default(), SecretSourceMetadata{}, nil
	}

	fileCfg, err := parseConfigYAML(raw)
	if err != nil {
		return Config{}, SecretSourceMetadata{}, fmt.Errorf("parse snapshot file %s: %w", path, err)
	}
	sources, err := parseSecretSourceMetadata(raw)
	if err != nil {
		return Config{}, SecretSourceMetadata{}, fmt.Errorf("parse snapshot secret source metadata %s: %w", path, err)
	}

	cfg := Default()
	applyParsedFileOverrides(&cfg, fileCfg)
	if err := cfg.Validate(); err != nil {
		return Config{}, SecretSourceMetadata{}, err
	}
	return cfg, sources, nil
}

// appendSnapshotEmbedIdentity records the resolved embed identity
// (provider+models) in the snapshot so a later load can detect a
// changed embed provider/model and refuse to mix vector spaces
// (SPEC 8.1.4). A top-level scalar — ignored by the flat config parser
// and the providers:/model: yaml subtree decode on reload.
func appendSnapshotEmbedIdentity(raw []byte, id string) []byte {
	id = strings.TrimSpace(id)
	if id == "" {
		return raw
	}
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		raw = append(raw, '\n')
	}
	return append(raw, []byte("embed_identity: "+id+"\n")...)
}

// appendSnapshotEmbedBaseURL records the normalized embed base_url (SPEC 8.1.4
// / §6.4 `embed_base_url`) alongside the composite embed identity so the
// endpoint that pins the vector space is legible without parsing the identity.
// It is the SAME normalized value already folded into embed_identity's 2nd
// field; a canonical/default endpoint — including the HOSTED native
// gemini/cohere surface, though no longer a custom one (issue #702) —
// normalizes to "" and the line is omitted (nothing to disambiguate). A top-level scalar — ignored by
// the flat config parser and the providers:/model: yaml subtree decode on
// reload.
func appendSnapshotEmbedBaseURL(raw []byte, baseURL string) []byte {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return raw
	}
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		raw = append(raw, '\n')
	}
	return append(raw, []byte("embed_base_url: "+baseURL+"\n")...)
}

// appendSnapshotEmbedContextual records the effective contextual-retrieval
// component of the embed identity (SPEC §5.5 `embed_contextual`, §8.1.4/§8.1.8)
// alongside the composite identity, so the mode that pins the vector space is
// legible without parsing the identity. It is the SAME value already folded into
// embed_identity's terminal field. The line is OMITTED for the `off` default —
// per §5.5 an absent key is treated as `off`, so a non-contextual corpus's
// snapshot stays byte-identical to a pre-feature one. A top-level scalar —
// ignored by the flat config parser and the providers:/model: yaml subtree
// decode on reload.
func appendSnapshotEmbedContextual(raw []byte, contextual string) []byte {
	contextual = strings.TrimSpace(contextual)
	if contextual == "" || contextual == provider.EmbedContextualOff {
		return raw
	}
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		raw = append(raw, '\n')
	}
	return append(raw, []byte("embed_contextual: "+contextual+"\n")...)
}

// appendSnapshotSecretSourceMetadata appends a `secret_sources:` block
// to raw recording where each secret was sourced from (env/file/…)
// without writing the secret values themselves. Empty entries are
// skipped; the block is omitted entirely when no sources are set.
func appendSnapshotSecretSourceMetadata(raw []byte, sources SecretSourceMetadata) []byte {
	entries := []struct {
		key   string
		value string
	}{
		{key: "elevenlabs_api_key", value: strings.TrimSpace(sources.ElevenLabsAPIKey)},
		{key: "cohere_api_key", value: strings.TrimSpace(sources.CohereAPIKey)},
		{key: "x402_facilitator_token", value: strings.TrimSpace(sources.X402FacilitatorToken)},
		{key: "auth_token", value: strings.TrimSpace(sources.AuthToken)},
	}

	buf := strings.Builder{}
	for _, entry := range entries {
		if entry.value == "" {
			continue
		}
		if buf.Len() == 0 {
			buf.WriteString("secret_sources:\n")
		}
		buf.WriteString("  ")
		buf.WriteString(entry.key)
		buf.WriteString(": ")
		buf.WriteString(entry.value)
		buf.WriteByte('\n')
	}
	if buf.Len() == 0 {
		return raw
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		raw = append(raw, '\n')
	}
	return append(raw, []byte(buf.String())...)
}

// parseSecretSourceMetadata reads the `secret_sources:` block back out
// of a snapshot into SecretSourceMetadata (the inverse of
// appendSnapshotSecretSourceMetadata).
func parseSecretSourceMetadata(raw []byte) (SecretSourceMetadata, error) {
	meta := SecretSourceMetadata{}
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	sectionByIndent := map[int]string{}

	for scanner.Scan() {
		rawLine := scanner.Text()
		indent := len(rawLine) - len(strings.TrimLeft(rawLine, " "))
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "- ") {
			continue
		}
		pruneStaleIndents(sectionByIndent, indent)
		key, value, err := resolveYAMLKey(line, sectionByIndent, indent)
		if err != nil {
			continue
		}
		if value == "" && isMapSectionKey(key) {
			sectionByIndent[indent] = key
			continue
		}
		applySecretSourceField(&meta, key, unquoteYAMLScalar(value))
	}
	if err := scanner.Err(); err != nil {
		return SecretSourceMetadata{}, err
	}
	return meta, nil
}

// applySecretSourceField assigns a single parsed `secret_sources.<name>`
// key/value onto meta; unknown keys are ignored.
func applySecretSourceField(meta *SecretSourceMetadata, key, value string) {
	switch key {
	case "secret_sources.elevenlabs_api_key":
		meta.ElevenLabsAPIKey = value
	case "secret_sources.cohere_api_key":
		meta.CohereAPIKey = value
	case "secret_sources.x402_facilitator_token":
		meta.X402FacilitatorToken = value
	case "secret_sources.auth_token":
		meta.AuthToken = value
	}
}

// load builds a Config from defaults, optionally layering dotenv/env
// overrides and a YAML file, then validates the result. It is the shared
// implementation behind Load/LoadFile.
func load(path string, overrideEnv map[string]string, applyEnv bool) (Config, error) {
	// Start from defaults, then layer dotenv/env overrides.
	cfg := Default()
	if applyEnv {
		// SPEC §16.1.1 precedence: env (#1) > keychain (#2) > file/.env.local (#3).
		// Keychain is consulted before the dotenv files so a stored credential
		// wins over .env.local but never over an explicit environment variable.
		// The dotenv files themselves come from dotEnvSearchPaths: the config
		// file's own directory first, then the working directory (#677).
		loadKeychainCredentials(overrideEnv)
		loaded, err := loadDotEnvFiles(dotEnvSearchPaths(path), overrideEnv)
		if err != nil {
			return Config{}, fmt.Errorf("load dotenv files: %w", err)
		}
		if warn := dotEnvMultiDirWarning(loaded); warn != nil {
			cfg.Warnings = append(cfg.Warnings, warn)
		}
	}
	if path == "" {
		return finalizeLoadedConfig(cfg, overrideEnv, applyEnv)
	}

	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return finalizeLoadedConfig(cfg, overrideEnv, applyEnv)
		}
		return Config{}, fmt.Errorf("stat config: %w", err)
	}

	if err := applyFileOverrides(&cfg, path); err != nil {
		return Config{}, err
	}
	return finalizeLoadedConfig(cfg, overrideEnv, applyEnv)
}

// finalizeLoadedConfig applies env overrides (when applyEnv) and runs the
// post-load validation, returning the resolved config. Shared by every load
// branch (no-path, missing-file, and file-present) so the env-apply + validate
// sequence — and its error handling — lives in one place.
func finalizeLoadedConfig(cfg Config, overrideEnv map[string]string, applyEnv bool) (Config, error) {
	if applyEnv {
		if err := applyEnvOverrides(&cfg, overrideEnv); err != nil {
			return Config{}, err
		}
	}
	if err := cfg.finalizeLoaded(applyEnv); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// applyFileOverrides reads the YAML file at path and overlays its flat
// keys onto cfg, and decodes the providers:/model: subtree into
// cfg.providersDoc (SPEC 0.7.0 §16.2).
func applyFileOverrides(cfg *Config, path string) error {
	if cfg == nil {
		return nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config file %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}

	fileCfg, err := parseConfigYAML(raw)
	if err != nil {
		return fmt.Errorf("parse config file %s: %w", path, err)
	}
	applyParsedFileOverrides(cfg, fileCfg)
	// Keys nobody claimed are operator intent that would otherwise be discarded
	// without a word — a typo, a stale key from an older release, or a writer/
	// loader spelling mismatch like #624. Warn rather than fail: an unknown key
	// must not break an existing deployment (#628).
	if len(fileCfg.unknownKeys) > 0 {
		cfg.Warnings = append(cfg.Warnings, fmt.Errorf(
			"config file %s: %d unrecognized key(s) ignored: %s",
			path, len(fileCfg.unknownKeys), strings.Join(fileCfg.unknownKeys, ", ")))
	}
	// A recognized key that nothing reads is the same failure of honesty as a key
	// nobody claimed, so it gets the same instrument (#843).
	warnInertIngestModesOff(cfg, fileCfg, path)

	// Decode the dynamic providers:/model: subtree with yaml.v3
	// (SPEC 0.7.0 §16.2) — independent of the flat parser above.
	doc, err := parseProvidersDoc(raw)
	if err != nil {
		return fmt.Errorf("config file %s: %w", path, err)
	}
	cfg.providersDoc = doc

	// Optional cost.prices overrides for per-query metrics (issue #327).
	prices, err := parseCostPriceOverrides(raw)
	if err != nil {
		return fmt.Errorf("config file %s: %w", path, err)
	}
	if len(prices) > 0 {
		cfg.CostPriceOverrides = prices
	}

	// Optional carbon: block for the opt-in energy/CO2e estimate (issue #328).
	carbon, err := parseCarbonConfig(raw)
	if err != nil {
		return fmt.Errorf("config file %s: %w", path, err)
	}
	cfg.Carbon = carbon

	// Optional media.translate.glossary nested map (SPEC §8.6.2, issue #574).
	// It is a per-target-language map-of-maps, so it is decoded from the media:
	// subtree with yaml.v3 rather than the bespoke flat parser above (which is
	// scalar/list-only). Absent block ⇒ nil (no guidance), no error.
	glossary, err := parseMediaTranslateGlossary(raw)
	if err != nil {
		return fmt.Errorf("config file %s: %w", path, err)
	}
	if len(glossary) > 0 {
		cfg.MediaTranslateGlossary = glossary
	}

	// Optional media.stt.language_providers nested map (SPEC §8.2.1, #566). It is
	// a map of BCP-47 language -> STT provider profile name, so it is decoded from
	// the media: subtree with yaml.v3 rather than the bespoke flat parser above
	// (which is scalar/list-only). Absent block ⇒ nil (no routing), no error.
	langProviders, err := parseMediaSTTLanguageProviders(raw)
	if err != nil {
		return fmt.Errorf("config file %s: %w", path, err)
	}
	if len(langProviders) > 0 {
		cfg.MediaSTTLanguageProviders = langProviders
	}

	return nil
}

// applyParsedFileOverrides overlays a parsed fileConfig onto cfg by
// dispatching to the server/model/ingest/x402 sub-appliers.
func applyParsedFileOverrides(cfg *Config, fileCfg fileConfig) {
	applyServerFileParsed(cfg, fileCfg)
	applyModelFileParsed(cfg, fileCfg)
	applyIngestFileParsed(cfg, fileCfg)
	applyX402FileParsed(cfg, fileCfg)
	applySourceFileParsed(cfg, fileCfg)
}

// applySourceFileParsed overlays parsed corpus-source file fields onto
// cfg.Source. Credentials are never read from disk (env/keychain-only).
// It also overlays the networked vector-backend invariants (Qdrant url/
// collection, pgvector schema/table); their secrets (Qdrant api_key, pgvector
// DSN) are env/keychain-only and never read from the config file here.
func applySourceFileParsed(cfg *Config, fc fileConfig) {
	if fc.SourceKind != nil {
		cfg.Source.Kind = *fc.SourceKind
	}
	if fc.SourceS3Bucket != nil {
		cfg.Source.S3Bucket = *fc.SourceS3Bucket
	}
	if fc.SourceS3Prefix != nil {
		cfg.Source.S3Prefix = *fc.SourceS3Prefix
	}
	if fc.SourceS3Region != nil {
		cfg.Source.S3Region = *fc.SourceS3Region
	}
	if fc.SourceS3Endpoint != nil {
		cfg.Source.S3Endpoint = *fc.SourceS3Endpoint
	}
	if fc.QdrantURL != nil {
		cfg.Qdrant.URL = *fc.QdrantURL
	}
	if fc.QdrantCollection != nil {
		cfg.Qdrant.Collection = *fc.QdrantCollection
	}
	if fc.IndexPgvectorSchema != nil {
		cfg.IndexPgvectorSchema = *fc.IndexPgvectorSchema
	}
	if fc.IndexPgvectorTable != nil {
		cfg.IndexPgvectorTable = *fc.IndexPgvectorTable
	}
	if fc.DistributedEmbedEnabled != nil {
		cfg.DistributedEmbed.Enabled = *fc.DistributedEmbedEnabled
	}
	if fc.DistributedEmbedBroker != nil {
		cfg.DistributedEmbed.Broker = *fc.DistributedEmbedBroker
	}
	if fc.DistributedEmbedSQLitePath != nil {
		cfg.DistributedEmbed.BrokerSQLitePath = *fc.DistributedEmbedSQLitePath
	}
	if fc.DistributedEmbedMaxAttempts != nil {
		cfg.DistributedEmbed.MaxAttempts = *fc.DistributedEmbedMaxAttempts
	}
}

// applyServerFileParsed overlays parsed server core and network file
// settings onto cfg.
func applyServerFileParsed(cfg *Config, fc fileConfig) {
	applyServerCoreFileParsed(cfg, fc)
	applyServerNetworkFileParsed(cfg, fc)
}

// applyServerCoreFileParsed copies the set core server file fields
// (paths, listen addr, protocol, auth, rate limits) onto cfg.
func applyServerCoreFileParsed(cfg *Config, fc fileConfig) {
	if fc.RootDir != nil {
		cfg.RootDir = *fc.RootDir
	}
	if fc.StateDir != nil {
		cfg.StateDir = *fc.StateDir
	}
	if fc.ListenAddr != nil {
		cfg.ListenAddr = *fc.ListenAddr
	}
	if fc.MCPPath != nil {
		cfg.MCPPath = *fc.MCPPath
	}
	if fc.ProtocolVersion != nil {
		cfg.ProtocolVersion = *fc.ProtocolVersion
	}
	if fc.Public != nil {
		cfg.Public = *fc.Public
	}
	if fc.AuthMode != nil {
		cfg.AuthMode = *fc.AuthMode
	}
	if fc.ServerName != nil {
		cfg.ServerName = *fc.ServerName
	}
	if fc.RateLimitRPS != nil {
		cfg.RateLimitRPS = *fc.RateLimitRPS
	}
	if fc.RateLimitBurst != nil {
		cfg.RateLimitBurst = *fc.RateLimitBurst
	}
}

// applyServerNetworkFileParsed copies the set network-related file
// fields (proxy/origin/exclude lists and TLS paths) onto cfg,
// normalizing string slices.
func applyServerNetworkFileParsed(cfg *Config, fc fileConfig) {
	if fc.TrustedProxies != nil {
		cfg.TrustedProxies = normalizeStringSlice(fc.TrustedProxies)
	}
	if fc.PathExcludes != nil {
		cfg.PathExcludes = normalizeStringSlice(fc.PathExcludes)
	}
	if fc.SecretPatterns != nil {
		cfg.SecretPatterns = normalizeStringSlice(fc.SecretPatterns)
	}
	if fc.AllowedOrigins != nil {
		cfg.AllowedOrigins = normalizeStringSlice(fc.AllowedOrigins)
	}
	if fc.ServerTLSCertFile != nil {
		cfg.ServerTLSCertFile = *fc.ServerTLSCertFile
	}
	if fc.ServerTLSKeyFile != nil {
		cfg.ServerTLSKeyFile = *fc.ServerTLSKeyFile
	}
}

// applyModelFileParsed overlays parsed model client, rerank, and RAG
// file settings onto cfg.
func applyModelFileParsed(cfg *Config, fc fileConfig) {
	applyModelClientsFileParsed(cfg, fc)
	applyRerankFileParsed(cfg, fc)
	applyModelRAGFileParsed(cfg, fc)
}

// rerankEnabledEffective resolves the tri-state RerankEnabled override
// into the effective on/off decision used for the persisted snapshot:
// an explicit value wins; when unset (nil), reranking is on iff a
// rerank provider credential is present. Runtime activation in
// configureReranker applies the same rule plus provider validation and
// the explicit-true-but-no-credential warning.
func rerankEnabledEffective(cfg *Config) bool {
	if cfg.RerankEnabled != nil {
		return *cfg.RerankEnabled
	}
	return strings.TrimSpace(cfg.CohereAPIKey) != ""
}

// applyRerankFileParsed copies parsed rerank.* file values onto cfg.
// Split out of applyModelFileParsed (its only caller) to keep that
// function under the gocyclo budget.
func applyRerankFileParsed(cfg *Config, fc fileConfig) {
	if fc.RerankEnabled != nil {
		cfg.RerankEnabled = boolPtr(*fc.RerankEnabled)
	}
	if fc.RerankProvider != nil {
		cfg.RerankProvider = *fc.RerankProvider
	}
	if fc.CohereAPIKey != nil {
		cfg.CohereAPIKey = *fc.CohereAPIKey
	}
	if fc.CohereBaseURL != nil {
		cfg.CohereBaseURL = *fc.CohereBaseURL
	}
	if fc.RerankModel != nil {
		cfg.RerankModel = *fc.RerankModel
	}
	if fc.RerankCandidatePool != nil {
		cfg.RerankCandidatePool = *fc.RerankCandidatePool
	}
}

// applyModelClientsFileParsed overlays the parsed ingest/ElevenLabs
// client settings from the config file onto cfg (only keys present in
// the file are applied; the spec-removed Mistral client keys are no
// longer handled here).
func applyModelClientsFileParsed(cfg *Config, fc fileConfig) {
	if fc.DoclingCommand != nil {
		cfg.DoclingCommand = *fc.DoclingCommand
	}
	if fc.IngestDoclingServeURL != nil {
		cfg.IngestDoclingServeURL = *fc.IngestDoclingServeURL
	}
	if fc.IngestPandocCommand != nil {
		cfg.IngestPandocCommand = *fc.IngestPandocCommand
	}
	if fc.ElevenLabsBaseURL != nil {
		cfg.ElevenLabsBaseURL = *fc.ElevenLabsBaseURL
	}
	if fc.ElevenLabsAPIKey != nil {
		cfg.ElevenLabsAPIKey = *fc.ElevenLabsAPIKey
	}
	if fc.ElevenLabsTTSVoiceID != nil {
		cfg.ElevenLabsTTSVoiceID = *fc.ElevenLabsTTSVoiceID
	}
}

// applyModelRAGFileParsed copies the set RAG/retrieval and session/health
// timing file fields onto cfg.
func applyModelRAGFileParsed(cfg *Config, fc fileConfig) {
	if fc.RAGSystemPrompt != nil {
		cfg.RAGSystemPrompt = *fc.RAGSystemPrompt
	}
	if fc.RAGGenerateAnswer != nil {
		cfg.RAGGenerateAnswer = *fc.RAGGenerateAnswer
	}
	if fc.RAGKDefault != nil {
		cfg.RAGKDefault = *fc.RAGKDefault
	}
	if fc.RAGMaxContextChars != nil {
		cfg.RAGMaxContextChars = *fc.RAGMaxContextChars
	}
	if fc.RAGOversampleFactor != nil {
		cfg.RAGOversampleFactor = *fc.RAGOversampleFactor
	}
	if fc.RetrievalHybridEnabled != nil {
		cfg.RetrievalHybridEnabled = *fc.RetrievalHybridEnabled
	}
	if fc.DedupRetrieval != nil {
		cfg.DedupRetrieval = *fc.DedupRetrieval
	}
	if fc.RetrievalMinScore != nil {
		cfg.RetrievalMinScore = *fc.RetrievalMinScore
	}
	applyRetrievalTuningFileParsed(cfg, fc)
	if fc.SessionInactivityTimeout != nil {
		cfg.SessionInactivityTimeout = *fc.SessionInactivityTimeout
	}
	if fc.SessionMaxLifetime != nil {
		cfg.SessionMaxLifetime = *fc.SessionMaxLifetime
	}
	if fc.HealthCheckInterval != nil {
		cfg.HealthCheckInterval = *fc.HealthCheckInterval
	}
}

// applyRetrievalTuningFileParsed overlays the opt-in retrieval-tuning file
// fields (recency time-decay #323, context compression #335, the adaptive
// gate #338, MMR diversity #340, the HyDE query transform #333, and
// cross-lingual query expansion #325) onto cfg. Split out of
// applyModelRAGFileParsed to keep that function within the gocyclo budget as
// tuning knobs are added.
func applyRetrievalTuningFileParsed(cfg *Config, fc fileConfig) {
	if fc.RetrievalRecencyHalfLife != nil {
		cfg.RetrievalRecencyHalfLife = *fc.RetrievalRecencyHalfLife
	}
	if fc.ContextCompressionEnabled != nil {
		cfg.ContextCompressionEnabled = *fc.ContextCompressionEnabled
	}
	if fc.ContextCompressionTargetRatio != nil {
		cfg.ContextCompressionTargetRatio = *fc.ContextCompressionTargetRatio
	}
	if fc.RetrievalAdaptiveEnabled != nil {
		cfg.RetrievalAdaptiveEnabled = *fc.RetrievalAdaptiveEnabled
	}
	if fc.RetrievalAdaptiveKMin != nil {
		cfg.RetrievalAdaptiveKMin = *fc.RetrievalAdaptiveKMin
	}
	if fc.RetrievalAdaptiveKMax != nil {
		cfg.RetrievalAdaptiveKMax = *fc.RetrievalAdaptiveKMax
	}
	if fc.RetrievalMMREnabled != nil {
		cfg.RetrievalMMREnabled = *fc.RetrievalMMREnabled
	}
	if fc.RetrievalMMRLambda != nil {
		cfg.RetrievalMMRLambda = *fc.RetrievalMMRLambda
	}
	if fc.RetrievalHyDEEnabled != nil {
		cfg.RetrievalHyDEEnabled = *fc.RetrievalHyDEEnabled
	}
	if fc.RetrievalHyDEMode != nil {
		cfg.RetrievalHyDEMode = *fc.RetrievalHyDEMode
	}
	if fc.CrossLingualEnabled != nil {
		cfg.CrossLingualEnabled = *fc.CrossLingualEnabled
	}
	if fc.CrossLingualTargetLangs != nil {
		cfg.CrossLingualTargetLangs = normalizeStringSlice(fc.CrossLingualTargetLangs)
	}
	applyHierarchicalFileParsed(cfg, fc)
	applyRetrievalContextualFileParsed(cfg, fc)
}

// applyHierarchicalFileParsed overlays the opt-in hierarchical (coarse-to-fine)
// retrieval file fields (SPEC §9.7/§16.2, #329) onto cfg. Split out of
// applyRetrievalTuningFileParsed to keep that function within the gocyclo budget.
func applyHierarchicalFileParsed(cfg *Config, fc fileConfig) {
	if fc.RetrievalHierarchicalEnabled != nil {
		cfg.RetrievalHierarchicalEnabled = *fc.RetrievalHierarchicalEnabled
	}
	if fc.RetrievalHierarchicalSourceReps != nil {
		cfg.RetrievalHierarchicalSourceReps = normalizeStringSlice(fc.RetrievalHierarchicalSourceReps)
	}
	if fc.RetrievalHierarchicalLevels != nil {
		cfg.RetrievalHierarchicalLevels = normalizeStringSlice(fc.RetrievalHierarchicalLevels)
	}
	if fc.RetrievalHierarchicalProvider != nil {
		cfg.RetrievalHierarchicalProvider = *fc.RetrievalHierarchicalProvider
	}
	if fc.RetrievalHierarchicalMaxTokens != nil {
		cfg.RetrievalHierarchicalMaxTokens = *fc.RetrievalHierarchicalMaxTokens
	}
	if fc.RetrievalHierarchicalPromptVersion != nil {
		cfg.RetrievalHierarchicalPromptVersion = *fc.RetrievalHierarchicalPromptVersion
	}
	if fc.RetrievalHierarchicalPrompt != nil {
		cfg.RetrievalHierarchicalPrompt = *fc.RetrievalHierarchicalPrompt
	}
}

// applyRetrievalContextualFileParsed overlays the opt-in contextual-retrieval
// block (`retrieval.contextual.*`, SPEC §8.1.8/§16.2, issue #330) onto cfg.
// Split out of applyRetrievalTuningFileParsed so both stay within the gocyclo
// budget.
func applyRetrievalContextualFileParsed(cfg *Config, fc fileConfig) {
	if fc.RetrievalContextualEnabled != nil {
		cfg.RetrievalContextualEnabled = *fc.RetrievalContextualEnabled
	}
	if fc.RetrievalContextualProvider != nil {
		cfg.RetrievalContextualProvider = *fc.RetrievalContextualProvider
	}
	if fc.RetrievalContextualModel != nil {
		cfg.RetrievalContextualModel = *fc.RetrievalContextualModel
	}
	if fc.RetrievalContextualMaxTokens != nil {
		cfg.RetrievalContextualMaxTokens = *fc.RetrievalContextualMaxTokens
	}
	if fc.RetrievalContextualPromptVersion != nil {
		cfg.RetrievalContextualPromptVersion = *fc.RetrievalContextualPromptVersion
	}
	if fc.RetrievalContextualPrompt != nil {
		cfg.RetrievalContextualPrompt = *fc.RetrievalContextualPrompt
	}
}

// applyIngestFileParsed overlays parsed chunking, ingest-mode, and STT
// file settings onto cfg.
func applyIngestFileParsed(cfg *Config, fc fileConfig) {
	applyChunkingFileParsed(cfg, fc)
	applyIngestModesFileParsed(cfg, fc)
	applySTTFileParsed(cfg, fc)
}

// applyChunkingFileParsed copies the set chunking file fields onto cfg.
func applyChunkingFileParsed(cfg *Config, fc fileConfig) {
	if fc.ChunkingMaxTokens != nil {
		cfg.ChunkingMaxTokens = *fc.ChunkingMaxTokens
	}
	if fc.ChunkingOverlapTokens != nil {
		cfg.ChunkingOverlapTokens = *fc.ChunkingOverlapTokens
	}
}

// applyIngestModesFileParsed copies the set ingest discovery/mode file
// fields (gitignore, symlinks, size limit, per-type modes, extractor)
// onto cfg.
func applyIngestModesFileParsed(cfg *Config, fc fileConfig) {
	if fc.IngestGitignore != nil {
		cfg.IngestGitignore = *fc.IngestGitignore
	}
	if fc.IngestFollowSymlinks != nil {
		cfg.IngestFollowSymlinks = *fc.IngestFollowSymlinks
	}
	if fc.IngestMaxFileMB != nil {
		cfg.IngestMaxFileMB = *fc.IngestMaxFileMB
	}
	applyIngestExcludeDirsFileParsed(cfg, fc)
	if fc.IngestPDFMode != nil {
		cfg.IngestPDFMode = *fc.IngestPDFMode
	}
	if fc.IngestImagesMode != nil {
		cfg.IngestImagesMode = *fc.IngestImagesMode
	}
	if fc.IngestAudioMode != nil {
		cfg.IngestAudioMode = *fc.IngestAudioMode
	}
	if fc.IngestArchivesMode != nil {
		cfg.IngestArchivesMode = *fc.IngestArchivesMode
	}
	if fc.IngestExtractor != nil {
		cfg.IngestExtractor = *fc.IngestExtractor
	}
	if fc.IngestOnUnsupported != nil {
		cfg.IngestOnUnsupported = *fc.IngestOnUnsupported
	}
	if fc.IndexBackend != nil {
		cfg.IndexBackend = *fc.IndexBackend
	}
	if fc.IngestScanCache != nil {
		cfg.IngestScanCache = *fc.IngestScanCache
	}
	if fc.IngestLateChunking != nil {
		cfg.IngestLateChunking = *fc.IngestLateChunking
	}
	if fc.IngestWatch != nil {
		cfg.IngestWatch = *fc.IngestWatch
	}
	if fc.IngestWatchDebounce != nil {
		cfg.IngestWatchDebounce = *fc.IngestWatchDebounce
	}
}

// applyIngestExcludeDirsFileParsed copies a set `ingest.exclude_dirs` onto cfg.
// A present key replaces the default list in full (SPEC §7.1, #773), so the
// merge is an assignment, not an append. It is split out of
// applyIngestModesFileParsed to keep that function inside the cyclomatic budget.
func applyIngestExcludeDirsFileParsed(cfg *Config, fc fileConfig) {
	if fc.IngestExcludeDirs == nil {
		return
	}
	resolved, warn := resolveExcludeDirs(normalizeStringSlice(fc.IngestExcludeDirs))
	cfg.IngestExcludeDirs = resolved
	if warn != nil {
		cfg.Warnings = append(cfg.Warnings, warn)
	}
}

// applySTTFileParsed copies the set speech-to-text file fields
// (provider and Mistral/ElevenLabs model + language) onto cfg.
func applySTTFileParsed(cfg *Config, fc fileConfig) {
	if fc.STTProvider != nil {
		cfg.STTProvider = *fc.STTProvider
	}
	if fc.RecognizeProvider != nil {
		cfg.RecognizeProvider = *fc.RecognizeProvider
	}
	if fc.RecognizeServeURL != nil {
		cfg.RecognizeServeURL = *fc.RecognizeServeURL
	}
	if fc.RecognizeServeCommand != nil {
		cfg.RecognizeServeCommand = *fc.RecognizeServeCommand
	}
	if fc.STTMistralModel != nil {
		cfg.STTMistralModel = *fc.STTMistralModel
	}
	if fc.STTElevenLabsModel != nil {
		cfg.STTElevenLabsModel = *fc.STTElevenLabsModel
	}
	if fc.STTElevenLabsLanguageCode != nil {
		cfg.STTElevenLabsLanguageCode = *fc.STTElevenLabsLanguageCode
	}
	if fc.QualityGatesEnabled != nil {
		cfg.QualityGatesEnabled = *fc.QualityGatesEnabled
	}
	if fc.LanguageDetectionEnabled != nil {
		cfg.LanguageDetectionEnabled = *fc.LanguageDetectionEnabled
	}
	applyMediaFileParsed(cfg, fc)
}

// applyMediaFileParsed copies the set media.* file fields onto cfg. It is split
// out of applySTTFileParsed so each apply helper stays under the cyclomatic
// complexity budget as more media scalars are added.
func applyMediaFileParsed(cfg *Config, fc fileConfig) {
	if fc.MediaSidecarsDisabled != nil {
		cfg.MediaSidecarsDisabled = *fc.MediaSidecarsDisabled
	}
	if fc.MediaVariantsGroup != nil {
		cfg.MediaVariantsGroup = *fc.MediaVariantsGroup
	}
	if fc.MediaVariantsSelect != nil {
		cfg.MediaVariantsSelect = *fc.MediaVariantsSelect
	}
	if fc.MediaTranslateEnabled != nil {
		cfg.MediaTranslateEnabled = *fc.MediaTranslateEnabled
	}
	if fc.MediaTranslateTargetLangs != nil {
		cfg.MediaTranslateTargetLangs = normalizeStringSlice(fc.MediaTranslateTargetLangs)
	}
	if fc.MediaTranslateEngine != nil {
		cfg.MediaTranslateEngine = *fc.MediaTranslateEngine
	}
	if fc.MediaTranslateWhisperWindowSec != nil {
		cfg.MediaTranslateWhisperWindowSec = *fc.MediaTranslateWhisperWindowSec
	}
	if fc.MediaTranslateWindowLines != nil {
		cfg.MediaTranslateWindowLines = *fc.MediaTranslateWindowLines
	}
	if fc.MediaTranslateContextLines != nil {
		cfg.MediaTranslateContextLines = *fc.MediaTranslateContextLines
	}
	if fc.MediaFilterWords != nil {
		cfg.MediaFilterWords = normalizeStringSlice(fc.MediaFilterWords)
	}
	applyMediaSubtitlesFileParsed(cfg, fc)
	applyMediaProcessingFileParsed(cfg, fc)
	applyMediaBatchFileParsed(cfg, fc)
}

// applyMediaProcessingFileParsed copies the set media processing/clip file
// fields onto cfg. Split from applyMediaFileParsed to keep that function under
// the cyclomatic-complexity budget.
func applyMediaProcessingFileParsed(cfg *Config, fc fileConfig) {
	if fc.MediaTrimLeadingSilence != nil {
		cfg.MediaTrimLeadingSilence = *fc.MediaTrimLeadingSilence
	}
	if fc.MediaSilenceThresholdDB != nil {
		cfg.MediaSilenceThresholdDB = *fc.MediaSilenceThresholdDB
	}
	if fc.MediaVAD != nil {
		cfg.MediaVAD = *fc.MediaVAD
	}
	if fc.MediaDiarizeEnabled != nil {
		// Tri-state: copy the pointer (a fresh copy) so an explicit false/true
		// from the file is preserved as a distinct state from omitted (nil).
		cfg.MediaDiarizeEnabled = copyBoolPtr(fc.MediaDiarizeEnabled)
	}
	if fc.MediaAudioWindowSec != nil {
		cfg.MediaAudioWindowSec = *fc.MediaAudioWindowSec
	}
	if fc.MediaVideoWindowSec != nil {
		cfg.MediaVideoWindowSec = *fc.MediaVideoWindowSec
	}
	applyMediaSTTFileParsed(cfg, fc)
	if fc.MediaClipMaxDurationMS != nil {
		cfg.MediaClipMaxDurationMS = *fc.MediaClipMaxDurationMS
	}
	if fc.MediaClipMaxBytes != nil {
		cfg.MediaClipMaxBytes = *fc.MediaClipMaxBytes
	}
}

// applyMediaBatchFileParsed copies the set media.batch file fields (SPEC §8.6.11)
// onto cfg. Split from applyMediaFileParsed to keep that function under the
// cyclomatic-complexity budget.
func applyMediaBatchFileParsed(cfg *Config, fc fileConfig) {
	if fc.MediaBatchTwoPhase != nil {
		cfg.MediaBatchTwoPhase = *fc.MediaBatchTwoPhase
	}
	if fc.MediaBatchProgress != nil {
		cfg.MediaBatchProgress = *fc.MediaBatchProgress
	}
	if fc.MediaBatchManifest != nil {
		cfg.MediaBatchManifest = strings.TrimSpace(*fc.MediaBatchManifest)
	}
}

// applyMediaSTTFileParsed copies the set media.stt.* file fields (#510, #511)
// onto cfg. Split out of applyMediaFileParsed so that function stays under the
// cyclomatic-complexity budget.
func applyMediaSTTFileParsed(cfg *Config, fc fileConfig) {
	if fc.MediaSTTMaxPayloadMB != nil {
		cfg.MediaSTTMaxPayloadMB = *fc.MediaSTTMaxPayloadMB
	}
	if fc.MediaSTTRequestTimeoutSec != nil {
		cfg.MediaSTTRequestTimeoutSec = *fc.MediaSTTRequestTimeoutSec
	}
	if fc.MediaSTTLanguageStrict != nil {
		cfg.MediaSTTLanguageStrict = *fc.MediaSTTLanguageStrict
	}
	if fc.MediaSTTOnUncoveredLanguage != nil {
		cfg.MediaSTTOnUncoveredLanguage = *fc.MediaSTTOnUncoveredLanguage
	}
	if fc.MediaSTTTracks != nil {
		cfg.MediaSTTTracks = normalizeStringSlice(fc.MediaSTTTracks)
	}
}

// applyMediaSubtitlesFileParsed copies the set media.subtitles.* file fields
// (SPEC §8.6.10) onto cfg. Split out of applyMediaFileParsed so each apply
// helper stays under the cyclomatic-complexity budget.
func applyMediaSubtitlesFileParsed(cfg *Config, fc fileConfig) {
	if fc.MediaSubtitlesTTMLEnabled != nil {
		cfg.MediaSubtitlesTTMLEnabled = *fc.MediaSubtitlesTTMLEnabled
	}
	if fc.MediaSubtitlesTTMLAlignToleranceMS != nil {
		cfg.MediaSubtitlesTTMLAlignToleranceMS = *fc.MediaSubtitlesTTMLAlignToleranceMS
	}
	if fc.MediaSubtitlesSMILEnabled != nil {
		cfg.MediaSubtitlesSMILEnabled = *fc.MediaSubtitlesSMILEnabled
	}
	if fc.MediaSubtitlesSegmentation != nil {
		cfg.MediaSubtitlesSegmentation = *fc.MediaSubtitlesSegmentation
	}
	if fc.MediaSubtitlesGlossary != nil {
		cfg.MediaSubtitlesGlossary = normalizeStringSlice(fc.MediaSubtitlesGlossary)
	}
	if fc.MediaSubtitlesDropPhrases != nil {
		cfg.MediaSubtitlesDropPhrases = normalizeStringSlice(fc.MediaSubtitlesDropPhrases)
	}
	if fc.MediaSubtitlesScrubPhrases != nil {
		cfg.MediaSubtitlesScrubPhrases = normalizeStringSlice(fc.MediaSubtitlesScrubPhrases)
	}
	if fc.MediaSubtitlesCollapseRepeats != nil {
		cfg.MediaSubtitlesCollapseRepeats = *fc.MediaSubtitlesCollapseRepeats
	}
	if fc.MediaSubtitlesDropURLs != nil {
		cfg.MediaSubtitlesDropURLs = *fc.MediaSubtitlesDropURLs
	}
}

// applyX402FileParsed copies the set x402 file fields onto cfg.X402.
// Any file-supplied facilitator token is ignored (env-only).
func applyX402FileParsed(cfg *Config, fc fileConfig) {
	if fc.X402Mode != nil {
		cfg.X402.Mode = *fc.X402Mode
	}
	if fc.X402FacilitatorURL != nil {
		cfg.X402.FacilitatorURL = *fc.X402FacilitatorURL
	}
	// ignore any x402_facilitator_token value from disk; tokens must come from
	// the environment to avoid persistence.
	if fc.X402ResourceBaseURL != nil {
		cfg.X402.ResourceBaseURL = *fc.X402ResourceBaseURL
	}
	if fc.X402ToolsCallEnabled != nil {
		cfg.X402.ToolsCallEnabled = *fc.X402ToolsCallEnabled
	}
	if fc.X402PriceAtomic != nil {
		cfg.X402.PriceAtomic = *fc.X402PriceAtomic
	}
	if fc.X402Network != nil {
		cfg.X402.Network = *fc.X402Network
	}
	if fc.X402Scheme != nil {
		cfg.X402.Scheme = *fc.X402Scheme
	}
	if fc.X402Asset != nil {
		cfg.X402.Asset = *fc.X402Asset
	}
	if fc.X402PayTo != nil {
		cfg.X402.PayTo = *fc.X402PayTo
	}
}

// validateIngestExcludeDirs rejects an entry that is not a plain directory
// name. SPEC §7.1 states that an entry is a name, not a path and not a glob,
// and that it matches at any depth. A value such as `src/dist` or `dist/**`
// therefore matches nothing. Rejecting it tells the operator at once, instead
// of leaving them to wonder why the directory is still indexed.
func (c *Config) validateIngestExcludeDirs() error {
	for _, name := range c.IngestExcludeDirs {
		switch {
		case strings.ContainsAny(name, "/\\"):
			return fmt.Errorf("ingest.exclude_dirs entry %q must be a plain directory name, not a path", name)
		case strings.ContainsAny(name, "*?["):
			return fmt.Errorf("ingest.exclude_dirs entry %q must be a plain directory name, not a glob", name)
		case name == "." || name == "..":
			return fmt.Errorf("ingest.exclude_dirs entry %q is not a directory name", name)
		}
	}
	return nil
}

// resolveExcludeDirs applies the SPEC §7.1 resolution rules to an operator
// directory ignore list. The caller has already established that the key is
// present, so the returned list REPLACES the default list in full.
//
// `.dir2mcp` is added back when the list omits it, because the state directory
// lives under root_dir: the walk would read the index that the same run writes,
// and watch mode would feed its own writes back into ingest. The spec says an
// implementation SHOULD say that it did this, so the second return value is a
// warning the loader surfaces. Duplicates are dropped and the operator's order
// is kept, so `print-config` shows what the operator wrote.
func resolveExcludeDirs(names []string) ([]string, error) {
	seen := make(map[string]struct{}, len(names)+1)
	resolved := make([]string, 0, len(names)+1)
	for _, name := range names {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		resolved = append(resolved, name)
	}
	if _, ok := seen[corpusfs.StateDirName]; ok {
		return resolved, nil
	}
	resolved = append(resolved, corpusfs.StateDirName)
	return resolved, fmt.Errorf(
		"config: ingest.exclude_dirs does not list %q; the name was added back. "+
			"The state directory is under root_dir, so to index it is self-referential (SPEC 7.1)",
		corpusfs.StateDirName)
}

// normalizeStringSlice trims each value and drops empties, returning nil
// when the input is nil.
func normalizeStringSlice(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

// parseConfigYAML parses the supported flat/nested-key YAML subset into
// a fileConfig using a bespoke line scanner (lists, inline lists, and
// indentation-derived section prefixes).
func parseConfigYAML(raw []byte) (fileConfig, error) {
	cfg := fileConfig{}
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	lineNo := 0
	currentListKey := ""
	sectionByIndent := map[int]string{}
	// True while inside a top-level block decoded separately with yaml.v3, whose
	// children must not be reported as unrecognized keys (#628).
	inSideParsedBlock := false

	for scanner.Scan() {
		lineNo++
		rawLine := scanner.Text()
		indent := len(rawLine) - len(strings.TrimLeft(rawLine, " "))
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if strings.HasPrefix(line, "- ") {
			if currentListKey == "" {
				return fileConfig{}, fmt.Errorf("line %d: list item without a list key", lineNo)
			}
			setFileListValue(&cfg, currentListKey, unquoteYAMLScalar(strings.TrimPrefix(line, "- ")))
			continue
		}

		currentListKey = ""
		pruneStaleIndents(sectionByIndent, indent)

		key, value, err := resolveYAMLKey(line, sectionByIndent, indent)
		if err != nil {
			return fileConfig{}, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if key == "" {
			return fileConfig{}, fmt.Errorf("line %d: empty key", lineNo)
		}
		// A top-level key re-decides whether we are inside a side-parsed block;
		// nested lines inherit the enclosing block's answer.
		if indent == 0 {
			inSideParsedBlock = sideParsedTopLevelBlocks[key]
		}

		if value == "" {
			newListKey, err := handleYAMLEmptyValue(&cfg, key, sectionByIndent, indent)
			if err != nil {
				return fileConfig{}, fmt.Errorf("line %d: %w", lineNo, err)
			}
			currentListKey = newListKey
			continue
		}

		if strings.HasPrefix(value, "[") {
			if err := handleYAMLInlineList(&cfg, key, value, lineNo); err != nil {
				return fileConfig{}, err
			}
			continue
		}

		if err := handleYAMLScalarValue(&cfg, key, value, !inSideParsedBlock); err != nil {
			return fileConfig{}, fmt.Errorf("line %d: %w", lineNo, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fileConfig{}, err
	}
	return cfg, nil
}

// handleYAMLScalarValue assigns a single bare (non-list, non-section) value to the
// fileConfig field selected by key, after unescaping. A bare scalar under a
// list-valued key (e.g. `media.stt.tracks: all`) is a single-element list, not a
// scalar field, so it is routed to the list appender — otherwise the keyword forms
// (`first`/`all`) of media.stt.tracks would be silently dropped by the scalar path,
// which has no such field.
// `collectUnknown` is false while the parser is inside a side-parsed top-level
// block, whose children legitimately never reach a scalar setter (#628).
func handleYAMLScalarValue(cfg *fileConfig, key, value string, collectUnknown bool) error {
	value = unquoteYAMLScalar(value)
	if strings.Contains(value, "\\n") {
		value = strings.ReplaceAll(value, "\\n", "\n")
	}
	if isListConfigKey(key) {
		setFileListValue(cfg, key, value)
		return nil
	}
	if err := setFileScalarValue(cfg, key, value); err != nil {
		return err
	}
	if collectUnknown && !scalarKeyIsRecognized(key, value) {
		cfg.unknownKeys = append(cfg.unknownKeys, key)
	}
	return nil
}

// sideParsedTopLevelBlocks are the top-level config blocks decoded separately
// with yaml.v3 rather than by the flat parser: the provider/model bindings (SPEC
// §16.2), the cost.prices overrides (#327) and the carbon block (#328). The flat
// parser still walks their lines, and because they are not registered map
// sections their children arrive UNPREFIXED (`kind`, `base_url`, `provider`, …),
// so they must be exempted by enclosing block rather than by key prefix.
// Otherwise every valid config carrying a providers: block would warn — a false
// positive far worse than the silence this warning replaces (#628).
var sideParsedTopLevelBlocks = map[string]bool{
	"providers": true,
	"model":     true,
	"cost":      true,
	"carbon":    true,
}

// sideParsedKeyPrefixes are the nested maps decoded separately that DO arrive
// prefixed, because they sit under the registered `media` section (#574, #566).
var sideParsedKeyPrefixes = []string{
	"media.translate.glossary.",
	"media.stt.language_providers.",
}

// scalarKeyIsRecognized reports whether any setter claims key.
//
// Detection is by EFFECT rather than by enumerating every known key: the string
// keys live in switch chains that cannot be enumerated at runtime, and a
// hand-maintained list would drift out of step exactly like the alias table did
// in #624. Every fileConfig field is a pointer, so applying the key to a fresh
// fileConfig and finding it unchanged proves nothing assigned it.
func scalarKeyIsRecognized(key, value string) bool {
	for _, prefix := range sideParsedKeyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	var probe fileConfig
	if err := setFileScalarValue(&probe, key, value); err != nil {
		// A parse/validation error means a setter DID claim the key; the caller
		// surfaces the error itself.
		return true
	}
	probe.unknownKeys = nil
	return !reflect.DeepEqual(probe, fileConfig{})
}

// resolveYAMLKey splits a "key: value" line, prefixes the key with the
// nearest enclosing section, and returns its canonical form plus the
// trimmed value.
func resolveYAMLKey(line string, sectionByIndent map[int]string, indent int) (key, value string, err error) {
	k, v, ok := strings.Cut(line, ":")
	if !ok {
		return "", "", fmt.Errorf("expected key: value")
	}
	k = strings.TrimSpace(k)
	if prefix := nearestSectionPrefix(sectionByIndent, indent); prefix != "" && !strings.Contains(k, ".") {
		k = prefix + "." + k
	}
	return canonicalizeConfigKey(k), strings.TrimSpace(v), nil
}

// pruneStaleIndents drops section entries at or deeper than indent so
// section prefixes don't leak across sibling/dedented blocks.
func pruneStaleIndents(sectionByIndent map[int]string, indent int) {
	for level := range sectionByIndent {
		if level >= indent {
			delete(sectionByIndent, level)
		}
	}
}

// handleYAMLEmptyValue handles a key with no inline value: starting a
// list, opening a map section at indent, or setting an empty scalar.
// Returns the list key when a block list begins.
func handleYAMLEmptyValue(cfg *fileConfig, key string, sectionByIndent map[int]string, indent int) (newListKey string, err error) {
	if isListConfigKey(key) {
		setFileListValue(cfg, key, "")
		return key, nil
	}
	if isMapSectionKey(key) {
		sectionByIndent[indent] = key
		return "", nil
	}
	if err := setFileScalarValue(cfg, key, ""); err != nil {
		return "", err
	}
	return "", nil
}

// handleYAMLInlineList parses a bracketed inline list value (e.g.
// "[a, b]") and appends its items to the list field selected by key.
func handleYAMLInlineList(cfg *fileConfig, key, value string, lineNo int) error {
	if !strings.HasSuffix(value, "]") {
		return fmt.Errorf("line %d: malformed list value for %s", lineNo, key)
	}
	if value == "[]" || !isListConfigKey(key) {
		if isListConfigKey(key) {
			setFileListValue(cfg, key, "")
		}
		return nil
	}
	inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(value, "["), "]"))
	if inner == "" {
		setFileListValue(cfg, key, "")
		return nil
	}
	for _, token := range strings.Split(inner, ",") {
		setFileListValue(cfg, key, unquoteYAMLScalar(strings.TrimSpace(token)))
	}
	return nil
}

// nearestSectionPrefix returns the section name registered at the
// greatest indent strictly less than indent, or "" if none.
func nearestSectionPrefix(sectionByIndent map[int]string, indent int) string {
	bestIndent := -1
	best := ""
	for level, section := range sectionByIndent {
		if level < indent && level > bestIndent {
			bestIndent = level
			best = section
		}
	}
	return best
}

// configKeyAliases maps legacy/alternate key spellings to their canonical form.
// Keys not present in the map are returned unchanged by canonicalizeConfigKey.
var configKeyAliases = map[string]string{
	"server.listen":                           "listen_addr",
	"server.mcp_path":                         "mcp_path",
	"server.name":                             "server_name",
	"server.protocol_version":                 "protocol_version",
	"server.public":                           "public",
	"security.auth.mode":                      "auth_mode",
	"security.allowed_origins":                "allowed_origins",
	"security.path_excludes":                  "path_excludes",
	"ingest.exclude_dirs":                     "ingest_exclude_dirs",
	"security.secret_patterns":                "secret_patterns",
	"docling.command":                         "docling_command",
	"ingest.docling.command":                  "docling_command",
	"docling.serve_url":                       "docling_serve_url",
	"ingest.docling.serve_url":                "docling_serve_url",
	"pandoc.command":                          "pandoc_command",
	"ingest.pandoc.command":                   "pandoc_command",
	"stt.elevenlabs.api_key":                  "elevenlabs_api_key",
	"secrets.elevenlabs_api_key":              "elevenlabs_api_key",
	"secrets.x402_facilitator_url":            "x402_facilitator_url",
	"rag_generate_answer":                     "rag.generate_answer",
	"generate_answer":                         "rag.generate_answer",
	"rag_k_default":                           "rag.k_default",
	"k_default":                               "rag.k_default",
	"rag_system_prompt":                       "rag.system_prompt",
	"system_prompt":                           "rag.system_prompt",
	"rag_max_context_chars":                   "rag.max_context_chars",
	"max_context_chars":                       "rag.max_context_chars",
	"rag_oversample_factor":                   "rag.oversample_factor",
	"oversample_factor":                       "rag.oversample_factor",
	"retrieval_hybrid_enabled":                "retrieval.hybrid.enabled",
	"hybrid_enabled":                          "retrieval.hybrid.enabled",
	"dedup_retrieval":                         "dedup.retrieval",
	"retrieval_min_score":                     "retrieval.min_score",
	"min_score":                               "retrieval.min_score",
	"retrieval_recency_half_life":             "retrieval.recency_half_life",
	"recency_half_life":                       "retrieval.recency_half_life",
	"context_compression_enabled":             "retrieval.context_compression.enabled",
	"context_compression":                     "retrieval.context_compression.enabled",
	"context_compression_target_ratio":        "retrieval.context_compression.target_ratio",
	"retrieval_adaptive_enabled":              "retrieval.adaptive.enabled",
	"adaptive_enabled":                        "retrieval.adaptive.enabled",
	"retrieval_adaptive_k_min":                "retrieval.adaptive.k_min",
	"adaptive_k_min":                          "retrieval.adaptive.k_min",
	"retrieval_adaptive_k_max":                "retrieval.adaptive.k_max",
	"adaptive_k_max":                          "retrieval.adaptive.k_max",
	"retrieval_mmr_enabled":                   "retrieval.mmr.enabled",
	"mmr_enabled":                             "retrieval.mmr.enabled",
	"retrieval_mmr_lambda":                    "retrieval.mmr.lambda",
	"mmr_lambda":                              "retrieval.mmr.lambda",
	"retrieval_hyde_enabled":                  "retrieval.hyde.enabled",
	"hyde_enabled":                            "retrieval.hyde.enabled",
	"retrieval_hyde_mode":                     "retrieval.hyde.mode",
	"hyde_mode":                               "retrieval.hyde.mode",
	"retrieval_contextual_enabled":            "retrieval.contextual.enabled",
	"contextual_enabled":                      "retrieval.contextual.enabled",
	"retrieval_contextual_provider":           "retrieval.contextual.provider",
	"retrieval_contextual_model":              "retrieval.contextual.model",
	"retrieval_contextual_max_tokens":         "retrieval.contextual.max_tokens",
	"retrieval_contextual_prompt_version":     "retrieval.contextual.prompt_version",
	"retrieval_contextual_prompt":             "retrieval.contextual.prompt",
	"cross_lingual_enabled":                   "retrieval.cross_lingual.enabled",
	"cross_lingual":                           "retrieval.cross_lingual.enabled",
	"cross_lingual_target_langs":              "retrieval.cross_lingual.target_langs",
	"retrieval_hierarchical_enabled":          "retrieval.hierarchical.enabled",
	"hierarchical_enabled":                    "retrieval.hierarchical.enabled",
	"retrieval_hierarchical_source_reps":      "retrieval.hierarchical.source_reps",
	"retrieval_hierarchical_levels":           "retrieval.hierarchical.levels",
	"retrieval_hierarchical_provider":         "retrieval.hierarchical.provider",
	"retrieval_hierarchical_max_tokens":       "retrieval.hierarchical.max_tokens",
	"retrieval_hierarchical_prompt_version":   "retrieval.hierarchical.prompt_version",
	"retrieval_hierarchical_prompt":           "retrieval.hierarchical.prompt",
	"rerank_enabled":                          "rerank.enabled",
	"rerank.cohere.api_key":                   "cohere_api_key",
	"rerank.cohere.base_url":                  "cohere_base_url",
	"rerank.cohere.model":                     "rerank_model",
	"rerank.provider":                         "rerank_provider",
	"rerank.model":                            "rerank_model",
	"rerank_candidate_pool":                   "rerank.candidate_pool",
	"chunking_max_tokens":                     "chunking.max_tokens",
	"chunking_overlap_tokens":                 "chunking.overlap_tokens",
	"ingest_gitignore":                        "ingest.gitignore",
	"gitignore":                               "ingest.gitignore",
	"ingest_follow_symlinks":                  "ingest.follow_symlinks",
	"follow_symlinks":                         "ingest.follow_symlinks",
	"ingest_max_file_mb":                      "ingest.max_file_mb",
	"max_file_mb":                             "ingest.max_file_mb",
	"ingest_scan_cache":                       "ingest.scan_cache",
	"scan_cache":                              "ingest.scan_cache",
	"ingest_late_chunking":                    "ingest.late_chunking",
	"late_chunking":                           "ingest.late_chunking",
	"ingest_watch":                            "ingest.watch",
	"ingest_watch_debounce":                   "ingest.watch_debounce",
	"ingest_pdf_mode":                         "ingest.pdf.mode",
	"pdf_mode":                                "ingest.pdf.mode",
	"ingest_images_mode":                      "ingest.images.mode",
	"images_mode":                             "ingest.images.mode",
	"ingest_audio_mode":                       "ingest.audio.mode",
	"audio_mode":                              "ingest.audio.mode",
	"ingest_archives_mode":                    "ingest.archives.mode",
	"archives_mode":                           "ingest.archives.mode",
	"ingest_extractor":                        "ingest.extractor",
	"extractor":                               "ingest.extractor",
	"ingest_on_unsupported":                   "ingest.on_unsupported",
	"on_unsupported":                          "ingest.on_unsupported",
	"index_backend":                           "index.backend",
	"backend":                                 "index.backend",
	"media_variants_group":                    "media.variants.group",
	"media_variants_select":                   "media.variants.select",
	"media_translate_enabled":                 "media.translate.enabled",
	"media_translate_target_langs":            "media.translate.target_langs",
	"media_translate_engine":                  "media.translate.engine",
	"media_translate_whisper_window_sec":      "media.translate.whisper_window_sec",
	"media_translate_window_lines":            "media.translate.window_lines",
	"media_translate_context_lines":           "media.translate.context_lines",
	"media_filter_words":                      "media.filter_words",
	"filter_words":                            "media.filter_words",
	"media_subtitles_ttml_enabled":            "media.subtitles.ttml.enabled",
	"media_subtitles_ttml_align_tolerance_ms": "media.subtitles.ttml.align_tolerance_ms",
	"media_subtitles_smil_enabled":            "media.subtitles.smil.enabled",
	"media_subtitles_segmentation":            "media.subtitles.segmentation",
	"media_subtitles_glossary":                "media.subtitles.glossary",
	"media_subtitles_drop_phrases":            "media.subtitles.drop_phrases",
	"media_subtitles_scrub_phrases":           "media.subtitles.scrub_phrases",
	"media_subtitles_collapse_repeats":        "media.subtitles.collapse_repeats",
	"media_subtitles_drop_urls":               "media.subtitles.drop_urls",
	"media_trim_leading_silence":              "media.trim_leading_silence",
	"media_silence_threshold_db":              "media.silence_threshold_db",
	"media_vad":                               "media.vad",
	"media_diarize_enabled":                   "media.diarize.enabled",
	"media_audio_window_sec":                  "media.audio_window_sec",
	"media_video_window_sec":                  "media.video_window_sec",
	"media_clip_max_duration_ms":              "media.clip.max_duration_ms",
	"media_clip_max_bytes":                    "media.clip.max_bytes",
	"media_stt_max_payload_mb":                "media.stt.max_payload_mb",
	"media_stt_request_timeout_sec":           "media.stt.request_timeout_sec",
	"media_stt_language_strict":               "media.stt.language_strict",
	"media_stt_on_uncovered_language":         "media.stt.on_uncovered_language",
	"media_stt_tracks":                        "media.stt.tracks",
	"stt_provider":                            "stt.provider",
	"stt_mistral_model":                       "stt.mistral.model",
	"stt_elevenlabs_model":                    "stt.elevenlabs.model",
	"stt_elevenlabs_language_code":            "stt.elevenlabs.language_code",
	"elevenlabs_language_code":                "stt.elevenlabs.language_code",
	"server_tls_cert_file":                    "server.tls.cert_file",
	"tls_cert_file":                           "server.tls.cert_file",
	"cert_file":                               "server.tls.cert_file",
	"server.tls.cert":                         "server.tls.cert_file",
	"server_tls_key_file":                     "server.tls.key_file",
	"tls_key_file":                            "server.tls.key_file",
	"key_file":                                "server.tls.key_file",
	"server.tls.key":                          "server.tls.key_file",
	"x402.mode":                               "x402_mode",
	"x402.facilitator_url":                    "x402_facilitator_url",
	"x402.resource_base_url":                  "x402_resource_base_url",
	"x402.facilitator_token":                  "x402_facilitator_token",
	"x402.route_policy.tools_call.enabled":    "x402_tools_call_enabled",
	"x402.route_policy.tools_call.price":      "x402_price_atomic",
	"x402.route_policy.tools_call.network":    "x402_network",
	"x402.route_policy.tools_call.scheme":     "x402_scheme",
	"x402.route_policy.tools_call.asset":      "x402_asset",
	"x402.route_policy.tools_call.pay_to":     "x402_pay_to",
	"source.kind":                             "source_kind",
	"source.s3.bucket":                        "source_s3_bucket",
	"source.s3.prefix":                        "source_s3_prefix",
	"source.s3.region":                        "source_s3_region",
	"source.s3.endpoint":                      "source_s3_endpoint",
	"index.qdrant.url":                        "qdrant_url",
	"index.qdrant.collection":                 "qdrant_collection",
	"index.qdrant.api_key":                    "qdrant_api_key",
	"index.pgvector.dsn":                      "index_pgvector_dsn",
	"index.pgvector.schema":                   "index_pgvector_schema",
	"index.pgvector.table":                    "index_pgvector_table",
	"distributed_embed.enabled":               "distributed_embed_enabled",
	"distributed_embed.broker":                "distributed_embed_broker",
	"distributed_embed.sqlite_path":           "distributed_embed_sqlite_path",
	"distributed_embed.broker_url":            "distributed_embed_broker_url",
	"distributed_embed.max_attempts":          "distributed_embed_max_attempts",
	"media_batch_two_phase":                   "media.batch.two_phase",
	"media_batch_progress":                    "media.batch.progress",
	"media_batch_manifest":                    "media.batch.manifest",

	// Recognition (design 0004). These underscore spellings are what the snapshot
	// writer emits — and therefore what `dir2mcp config init` puts in the
	// generated .dir2mcp.yaml — so they MUST resolve here or that generated file
	// does not round-trip: the keys are dropped silently, recognition stays off
	// with no error or warning, and because the provider never reaches the runtime
	// config, validateRecognizeProvider never fires on an incomplete `serve`
	// binding either (#624). Note recognize_serve_url maps to recognize.base_url —
	// a rename, not a mechanical "_" -> "." substitution, which is why it was
	// missed.
	"recognize_provider":      "recognize.provider",
	"recognize_serve_url":     "recognize.base_url",
	"recognize_serve_command": "recognize.serve_command",
}

// canonicalizeConfigKey lower-cases and trims key and maps it through
// configKeyAliases, returning the canonical form (or key unchanged).
func canonicalizeConfigKey(key string) string {
	key = strings.TrimSpace(strings.ToLower(key))
	if canonical, ok := configKeyAliases[key]; ok {
		return canonical
	}
	return key
}

// isMapSectionKey reports whether key names a nested mapping section
// (so child keys should be prefixed) rather than a scalar/list key.
func isMapSectionKey(key string) bool {
	// media.translate.glossary is a nested per-target-language map-of-maps decoded
	// separately with yaml.v3 (parseMediaTranslateGlossary); treat the glossary key
	// AND every language sub-mapping under it as a section so the flat parser
	// prefixes their child keys into the glossary namespace (e.g.
	// media.translate.glossary.<lang>.<term>) instead of leaking a source term like
	// "engine" up to a real media.translate.<key> and corrupting config (#574).
	//
	// media.stt.language_providers is likewise a nested lang->profile map decoded
	// separately (parseMediaSTTLanguageProviders, #566); treat it and its language
	// sub-entries as a section so children prefix into that namespace
	// (media.stt.language_providers.<lang>) — a guaranteed no-op — instead of
	// leaking a language code up to a real media.stt.<key>.
	if key == "media.translate.glossary" || strings.HasPrefix(key, "media.translate.glossary.") ||
		key == "media.stt.language_providers" || strings.HasPrefix(key, "media.stt.language_providers.") {
		return true
	}
	switch key {
	case "rag", "ingest", "ingest.docling", "ingest.pandoc", "stt", "stt.mistral", "stt.elevenlabs", "server", "server.tls", "secret_sources", "mistral", "docling", "pandoc", "security", "security.auth", "x402", "x402.route_policy", "x402.route_policy.tools_call", "chunking", "retrieval", "retrieval.hybrid", "retrieval.context_compression", "retrieval.adaptive", "retrieval.mmr", "retrieval.hyde", "retrieval.contextual", "retrieval.cross_lingual", "retrieval.hierarchical", "rerank", "rerank.cohere", "index", "dedup":
		return true
	case "ingest.pdf", "ingest.images", "ingest.audio", "ingest.archives", "secrets", "index.qdrant":
		return true
	case "source", "source.s3":
		return true
	case "media", "media.variants", "media.translate", "media.clip", "media.stt", "media.diarize", "media.batch":
		return true
	case "media.subtitles", "media.subtitles.ttml", "media.subtitles.smil":
		return true
	case "index.pgvector":
		return true
	case "distributed_embed":
		return true
	case "ingest.extractor":
		return false
	default:
		return false
	}
}

// setFileScalarValue assigns a scalar value to the fileConfig field
// matching key, trying bool, int, and duration parsers before falling
// back to string assignment.
func setFileScalarValue(cfg *fileConfig, key, value string) error {
	if err := setBoolFileScalar(cfg, key, value); err != nil {
		return err
	}
	if err := setIntFileScalar(cfg, key, value); err != nil {
		return err
	}
	if err := setFloatFileScalar(cfg, key, value); err != nil {
		return err
	}
	if err := setDurationFileScalar(cfg, key, value); err != nil {
		return err
	}
	setStringFileScalar(cfg, key, value)
	return nil
}

// setBoolFileScalar parses value as a bool and assigns it to the
// fileConfig field selected by key; unknown keys are a no-op and an
// unparseable value is an error.
// boolFileScalarTargets maps a canonical config key to the accessor that
// selects the corresponding *bool field on a fileConfig. Driving setBoolFileScalar
// from a table (rather than a switch) keeps its cyclomatic complexity flat as
// more boolean keys are added.
var boolFileScalarTargets = map[string]func(*fileConfig) **bool{
	"public":                     func(c *fileConfig) **bool { return &c.Public },
	"rag.generate_answer":        func(c *fileConfig) **bool { return &c.RAGGenerateAnswer },
	"ingest.gitignore":           func(c *fileConfig) **bool { return &c.IngestGitignore },
	"ingest.follow_symlinks":     func(c *fileConfig) **bool { return &c.IngestFollowSymlinks },
	"ingest.scan_cache":          func(c *fileConfig) **bool { return &c.IngestScanCache },
	"ingest.late_chunking":       func(c *fileConfig) **bool { return &c.IngestLateChunking },
	"ingest.watch":               func(c *fileConfig) **bool { return &c.IngestWatch },
	"quality_gates_enabled":      func(c *fileConfig) **bool { return &c.QualityGatesEnabled },
	"language_detection_enabled": func(c *fileConfig) **bool { return &c.LanguageDetectionEnabled },
	"media_sidecars_disabled":    func(c *fileConfig) **bool { return &c.MediaSidecarsDisabled },
	"media.variants.group":       func(c *fileConfig) **bool { return &c.MediaVariantsGroup },
	"media.translate.enabled":    func(c *fileConfig) **bool { return &c.MediaTranslateEnabled },
	"media.stt.language_strict":  func(c *fileConfig) **bool { return &c.MediaSTTLanguageStrict },
	"media.subtitles.ttml.enabled": func(c *fileConfig) **bool {
		return &c.MediaSubtitlesTTMLEnabled
	},
	"media.subtitles.smil.enabled": func(c *fileConfig) **bool {
		return &c.MediaSubtitlesSMILEnabled
	},
	"media.subtitles.drop_urls": func(c *fileConfig) **bool {
		return &c.MediaSubtitlesDropURLs
	},
	"media.trim_leading_silence": func(c *fileConfig) **bool {
		return &c.MediaTrimLeadingSilence
	},
	"media.vad":                  func(c *fileConfig) **bool { return &c.MediaVAD },
	"media.diarize.enabled":      func(c *fileConfig) **bool { return &c.MediaDiarizeEnabled },
	"media.batch.two_phase":      func(c *fileConfig) **bool { return &c.MediaBatchTwoPhase },
	"media.batch.progress":       func(c *fileConfig) **bool { return &c.MediaBatchProgress },
	"x402_tools_call_enabled":    func(c *fileConfig) **bool { return &c.X402ToolsCallEnabled },
	"retrieval.hybrid.enabled":   func(c *fileConfig) **bool { return &c.RetrievalHybridEnabled },
	"retrieval.adaptive.enabled": func(c *fileConfig) **bool { return &c.RetrievalAdaptiveEnabled },
	"retrieval.mmr.enabled":      func(c *fileConfig) **bool { return &c.RetrievalMMREnabled },
	"retrieval.hyde.enabled":     func(c *fileConfig) **bool { return &c.RetrievalHyDEEnabled },
	"retrieval.contextual.enabled": func(c *fileConfig) **bool {
		return &c.RetrievalContextualEnabled
	},
	"retrieval.cross_lingual.enabled": func(c *fileConfig) **bool { return &c.CrossLingualEnabled },
	"retrieval.hierarchical.enabled":  func(c *fileConfig) **bool { return &c.RetrievalHierarchicalEnabled },
	"dedup.retrieval":                 func(c *fileConfig) **bool { return &c.DedupRetrieval },
	"retrieval.context_compression.enabled": func(c *fileConfig) **bool {
		return &c.ContextCompressionEnabled
	},
	"rerank.enabled": func(c *fileConfig) **bool { return &c.RerankEnabled },
	"distributed_embed_enabled": func(c *fileConfig) **bool {
		return &c.DistributedEmbedEnabled
	},
}

func setBoolFileScalar(cfg *fileConfig, key, value string) error {
	accessor, ok := boolFileScalarTargets[key]
	if !ok {
		return nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fmt.Errorf("invalid boolean for %s", key)
	}
	*accessor(cfg) = boolPtr(parsed)
	return nil
}

// setIntFileScalar parses value as an int and assigns it to the
// fileConfig field selected by key; returns an error for an unknown key
// or a non-integer value.
// intFileScalarTargets maps a canonical integer config key to an accessor that
// returns the address of the matching *int fileConfig field. Using a table
// keeps setIntFileScalar a flat dispatch (one new entry per key) rather than an
// ever-growing switch that trips the cyclomatic-complexity gate.
var intFileScalarTargets = map[string]func(*fileConfig) **int{
	"rate_limit_rps":           func(c *fileConfig) **int { return &c.RateLimitRPS },
	"rate_limit_burst":         func(c *fileConfig) **int { return &c.RateLimitBurst },
	"rag.k_default":            func(c *fileConfig) **int { return &c.RAGKDefault },
	"retrieval.adaptive.k_min": func(c *fileConfig) **int { return &c.RetrievalAdaptiveKMin },
	"retrieval.adaptive.k_max": func(c *fileConfig) **int { return &c.RetrievalAdaptiveKMax },
	"retrieval.contextual.max_tokens": func(c *fileConfig) **int {
		return &c.RetrievalContextualMaxTokens
	},
	"rag.max_context_chars":              func(c *fileConfig) **int { return &c.RAGMaxContextChars },
	"rag.oversample_factor":              func(c *fileConfig) **int { return &c.RAGOversampleFactor },
	"chunking.max_tokens":                func(c *fileConfig) **int { return &c.ChunkingMaxTokens },
	"chunking.overlap_tokens":            func(c *fileConfig) **int { return &c.ChunkingOverlapTokens },
	"ingest.max_file_mb":                 func(c *fileConfig) **int { return &c.IngestMaxFileMB },
	"rerank.candidate_pool":              func(c *fileConfig) **int { return &c.RerankCandidatePool },
	"media.audio_window_sec":             func(c *fileConfig) **int { return &c.MediaAudioWindowSec },
	"media.translate.whisper_window_sec": func(c *fileConfig) **int { return &c.MediaTranslateWhisperWindowSec },
	"media.translate.window_lines":       func(c *fileConfig) **int { return &c.MediaTranslateWindowLines },
	"media.translate.context_lines":      func(c *fileConfig) **int { return &c.MediaTranslateContextLines },
	"media.video_window_sec":             func(c *fileConfig) **int { return &c.MediaVideoWindowSec },
	"media.clip.max_duration_ms":         func(c *fileConfig) **int { return &c.MediaClipMaxDurationMS },
	"media.clip.max_bytes":               func(c *fileConfig) **int { return &c.MediaClipMaxBytes },
	"media.stt.max_payload_mb":           func(c *fileConfig) **int { return &c.MediaSTTMaxPayloadMB },
	"media.stt.request_timeout_sec": func(c *fileConfig) **int {
		return &c.MediaSTTRequestTimeoutSec
	},
	"media.subtitles.ttml.align_tolerance_ms": func(c *fileConfig) **int {
		return &c.MediaSubtitlesTTMLAlignToleranceMS
	},
	"media.subtitles.collapse_repeats": func(c *fileConfig) **int {
		return &c.MediaSubtitlesCollapseRepeats
	},
	"distributed_embed_max_attempts": func(c *fileConfig) **int {
		return &c.DistributedEmbedMaxAttempts
	},
	"retrieval.hierarchical.max_tokens": func(c *fileConfig) **int {
		return &c.RetrievalHierarchicalMaxTokens
	},
}

func setIntFileScalar(cfg *fileConfig, key, value string) error {
	accessor, ok := intFileScalarTargets[key]
	if !ok {
		return nil
	}
	target := accessor(cfg)
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("invalid integer for %s", key)
	}
	if nonNegativeIntKeys[key] && parsed < 0 {
		return fmt.Errorf("invalid integer for %s: must not be negative", key)
	}
	*target = intPtr(parsed)
	return nil
}

// nonNegativeIntKeys lists canonical integer config keys whose value must not
// be negative. A negative value is rejected at config-parse time (explicit,
// deterministic) rather than being silently clamped later.
var nonNegativeIntKeys = map[string]bool{
	"retrieval.adaptive.k_min":                true,
	"retrieval.contextual.max_tokens":         true,
	"retrieval.adaptive.k_max":                true,
	"media.audio_window_sec":                  true,
	"media.translate.whisper_window_sec":      true,
	"media.translate.window_lines":            true,
	"media.translate.context_lines":           true,
	"media.video_window_sec":                  true,
	"media.clip.max_duration_ms":              true,
	"media.clip.max_bytes":                    true,
	"media.stt.max_payload_mb":                true,
	"media.stt.request_timeout_sec":           true,
	"media.subtitles.ttml.align_tolerance_ms": true,
	"media.subtitles.collapse_repeats":        true,
	"retrieval.hierarchical.max_tokens":       true,
}

// setFloatFileScalar parses value as a float64 and assigns it to the
// fileConfig field selected by key; unknown keys are a no-op and an
// unparseable value is an error.
func setFloatFileScalar(cfg *fileConfig, key, value string) error {
	var target **float64
	switch key {
	case "media.silence_threshold_db":
		target = &cfg.MediaSilenceThresholdDB
	case "retrieval.min_score":
		target = &cfg.RetrievalMinScore
	case "retrieval.context_compression.target_ratio":
		target = &cfg.ContextCompressionTargetRatio
	case "retrieval.mmr.lambda":
		target = &cfg.RetrievalMMRLambda
	default:
		return nil
	}
	// strconv.ParseFloat accepts "NaN"/"Inf"/"Infinity"; reject non-finite
	// values here so a malformed threshold fails at config-parse time (explicit,
	// deterministic) rather than surfacing later during ffmpeg execution.
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return fmt.Errorf("invalid number for %s", key)
	}
	*target = floatPtr(parsed)
	return nil
}

// setDurationFileScalar parses value as a time.Duration and assigns it
// to the fileConfig field selected by key; unknown keys are a no-op and
// an unparseable value is an error.
func setDurationFileScalar(cfg *fileConfig, key, value string) error {
	var target **time.Duration
	switch key {
	case "session_inactivity_timeout":
		target = &cfg.SessionInactivityTimeout
	case "session_max_lifetime":
		target = &cfg.SessionMaxLifetime
	case "health_check_interval":
		target = &cfg.HealthCheckInterval
	case "ingest.watch_debounce":
		target = &cfg.IngestWatchDebounce
	case "retrieval.recency_half_life":
		target = &cfg.RetrievalRecencyHalfLife
	default:
		return nil
	}
	if value == "" {
		return nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("invalid duration for %s", key)
	}
	*target = &d
	return nil
}

// setStringFileScalar dispatches a string key/value to the
// server/model/ingest/x402 string setters.
func setStringFileScalar(cfg *fileConfig, key, value string) {
	setServerStringFileScalar(cfg, key, value)
	setModelStringFileScalar(cfg, key, value)
	setIngestStringFileScalar(cfg, key, value)
	setX402StringFileScalar(cfg, key, value)
	setSourceStringFileScalar(cfg, key, value)
}

// setSourceStringFileScalar assigns corpus-source string keys onto the
// fileConfig. Credential keys are deliberately not accepted from disk.
// It also handles the networked vector-backend invariants (Qdrant url/
// collection, pgvector schema/table); their secrets (Qdrant api_key, pgvector
// DSN) are env/keychain-only and intentionally not read from the config file.
func setSourceStringFileScalar(cfg *fileConfig, key, value string) {
	switch key {
	case "source_kind":
		cfg.SourceKind = strPtr(value)
	case "source_s3_bucket":
		cfg.SourceS3Bucket = strPtr(value)
	case "source_s3_prefix":
		cfg.SourceS3Prefix = strPtr(value)
	case "source_s3_region":
		cfg.SourceS3Region = strPtr(value)
	case "source_s3_endpoint":
		cfg.SourceS3Endpoint = strPtr(value)
	case "qdrant_url":
		cfg.QdrantURL = strPtr(value)
	case "qdrant_collection":
		cfg.QdrantCollection = strPtr(value)
	case "index_pgvector_schema":
		cfg.IndexPgvectorSchema = strPtr(value)
	case "index_pgvector_table":
		cfg.IndexPgvectorTable = strPtr(value)
	case "distributed_embed_broker":
		cfg.DistributedEmbedBroker = strPtr(value)
	case "distributed_embed_sqlite_path":
		cfg.DistributedEmbedSQLitePath = strPtr(value)
	}
}

// setServerStringFileScalar assigns server-related string keys (paths,
// listen addr, protocol, auth mode, server name, TLS files) onto the
// fileConfig.
func setServerStringFileScalar(cfg *fileConfig, key, value string) {
	switch key {
	case "root_dir":
		cfg.RootDir = strPtr(value)
	case "state_dir":
		cfg.StateDir = strPtr(value)
	case "listen_addr":
		cfg.ListenAddr = strPtr(value)
	case "mcp_path":
		cfg.MCPPath = strPtr(value)
	case "protocol_version":
		cfg.ProtocolVersion = strPtr(value)
	case "auth_mode":
		cfg.AuthMode = strPtr(value)
	case "server_name":
		cfg.ServerName = strPtr(value)
	case "server.tls.cert_file":
		cfg.ServerTLSCertFile = strPtr(value)
	case "server.tls.key_file":
		cfg.ServerTLSKeyFile = strPtr(value)
	}
}

// setRerankStringFileScalar handles rerank.* string scalars. Split out
// of setModelStringFileScalar to keep that function under the gocyclo
// budget. Reports whether key was a rerank scalar.
func setRerankStringFileScalar(cfg *fileConfig, key, value string) bool {
	switch key {
	case "rerank_provider":
		cfg.RerankProvider = strPtr(value)
	case "cohere_api_key":
		cfg.CohereAPIKey = strPtr(value)
	case "cohere_base_url":
		cfg.CohereBaseURL = strPtr(value)
	case "rerank_model":
		cfg.RerankModel = strPtr(value)
	default:
		return false
	}
	return true
}

// setModelStringFileScalar assigns a model/provider-related flat string
// key onto the fileConfig (rerank keys first, then ElevenLabs/STT). The
// spec-removed Mistral/embed/chat keys are no longer accepted here.
func setModelStringFileScalar(cfg *fileConfig, key, value string) {
	if setRerankStringFileScalar(cfg, key, value) {
		return
	}
	if setRetrievalContextualStringFileScalar(cfg, key, value) {
		return
	}
	if setRetrievalHierarchicalStringFileScalar(cfg, key, value) {
		return
	}
	switch key {
	case "elevenlabs_base_url":
		cfg.ElevenLabsBaseURL = strPtr(value)
	case "elevenlabs_api_key":
		cfg.ElevenLabsAPIKey = strPtr(value)
	case "elevenlabs_tts_voice_id":
		cfg.ElevenLabsTTSVoiceID = strPtr(value)
	case "docling_command":
		cfg.DoclingCommand = strPtr(value)
	case "docling_serve_url":
		cfg.IngestDoclingServeURL = strPtr(value)
	case "pandoc_command":
		cfg.IngestPandocCommand = strPtr(value)
	case "rag.system_prompt":
		cfg.RAGSystemPrompt = strPtr(value)
	case "retrieval.hyde.mode":
		cfg.RetrievalHyDEMode = strPtr(value)
	}
}

// setRetrievalHierarchicalStringFileScalar assigns the retrieval.hierarchical.*
// string keys (SPEC §9.7/§16.2, #329), returning true when key matched. Split
// out of setModelStringFileScalar to keep that function within the gocyclo
// budget alongside the contextual (#330) delegation.
func setRetrievalHierarchicalStringFileScalar(cfg *fileConfig, key, value string) bool {
	switch key {
	case "retrieval.hierarchical.provider":
		cfg.RetrievalHierarchicalProvider = strPtr(value)
	case "retrieval.hierarchical.prompt_version":
		cfg.RetrievalHierarchicalPromptVersion = strPtr(value)
	case "retrieval.hierarchical.prompt":
		cfg.RetrievalHierarchicalPrompt = strPtr(value)
	case "retrieval.hierarchical.source_reps", "retrieval.hierarchical.levels":
		// Scalar form of a list-valued key: the spec's `source_reps: auto`
		// sentinel and a single `levels: document` both parse as a scalar, so
		// fold them into the same one-element list the block form produces.
		setFileListValue(cfg, key, value)
	default:
		return false
	}
	return true
}

// setRetrievalContextualStringFileScalar assigns the retrieval.contextual.*
// string keys (SPEC §8.1.8/§16.2, #330), returning true when key matched. Split
// out of setModelStringFileScalar so that function stays within the gocyclo
// budget once #330's cases land on top of #329's hierarchical cases.
func setRetrievalContextualStringFileScalar(cfg *fileConfig, key, value string) bool {
	switch key {
	case "retrieval.contextual.provider":
		cfg.RetrievalContextualProvider = strPtr(value)
	case "retrieval.contextual.model":
		cfg.RetrievalContextualModel = strPtr(value)
	case "retrieval.contextual.prompt_version":
		cfg.RetrievalContextualPromptVersion = strPtr(value)
	case "retrieval.contextual.prompt":
		cfg.RetrievalContextualPrompt = strPtr(value)
	default:
		return false
	}
	return true
}

// setIngestStringFileScalar assigns ingest mode and STT string keys
// onto the fileConfig.
func setIngestStringFileScalar(cfg *fileConfig, key, value string) {
	switch key {
	case "ingest.pdf.mode":
		cfg.IngestPDFMode = strPtr(value)
	case "ingest.images.mode":
		cfg.IngestImagesMode = strPtr(value)
	case "ingest.audio.mode":
		cfg.IngestAudioMode = strPtr(value)
	case "ingest.archives.mode":
		cfg.IngestArchivesMode = strPtr(value)
	case "ingest.extractor":
		cfg.IngestExtractor = strPtr(value)
	case "ingest.on_unsupported":
		cfg.IngestOnUnsupported = strPtr(value)
	case "index.backend":
		cfg.IndexBackend = strPtr(value)
	case "stt.provider":
		cfg.STTProvider = strPtr(value)
	case "recognize.provider":
		cfg.RecognizeProvider = strPtr(value)
	case "recognize.base_url":
		cfg.RecognizeServeURL = strPtr(value)
	case "recognize.serve_command":
		cfg.RecognizeServeCommand = strPtr(value)
	case "stt.mistral.model":
		cfg.STTMistralModel = strPtr(value)
	case "stt.elevenlabs.model":
		cfg.STTElevenLabsModel = strPtr(value)
	case "stt.elevenlabs.language_code":
		cfg.STTElevenLabsLanguageCode = strPtr(value)
	default:
		setMediaStringFileScalar(cfg, key, value)
	}
}

// setMediaStringFileScalar assigns media.* string keys onto the fileConfig. Split
// out of setIngestStringFileScalar so each stays within the cyclomatic budget.
func setMediaStringFileScalar(cfg *fileConfig, key, value string) {
	switch key {
	case "media.variants.select":
		cfg.MediaVariantsSelect = strPtr(value)
	case "media.subtitles.segmentation":
		cfg.MediaSubtitlesSegmentation = strPtr(value)
	case "media.translate.engine":
		cfg.MediaTranslateEngine = strPtr(value)
	case "media.stt.on_uncovered_language":
		cfg.MediaSTTOnUncoveredLanguage = strPtr(value)
	case "media.batch.manifest":
		cfg.MediaBatchManifest = strPtr(value)
	}
}

// setX402StringFileScalar assigns x402 string keys onto the fileConfig;
// the facilitator token key is deliberately ignored (env-only).
func setX402StringFileScalar(cfg *fileConfig, key, value string) {
	switch key {
	case "x402_mode":
		cfg.X402Mode = strPtr(value)
	case "x402_facilitator_url":
		cfg.X402FacilitatorURL = strPtr(value)
	case "x402_facilitator_token":
		// field deliberately ignored; tokens are env-only for security
	case "x402_resource_base_url":
		cfg.X402ResourceBaseURL = strPtr(value)
	case "x402_price_atomic":
		cfg.X402PriceAtomic = strPtr(value)
	case "x402_network":
		cfg.X402Network = strPtr(value)
	case "x402_scheme":
		cfg.X402Scheme = strPtr(value)
	case "x402_asset":
		cfg.X402Asset = strPtr(value)
	case "x402_pay_to":
		cfg.X402PayTo = strPtr(value)
	}
}

// setFileListValue appends value to the list field selected by the
// canonicalized key, initializing the slice on first use and skipping
// blank items.
// appendConfigListValue appends item to *target (allocating an empty slice
// first so a set-but-empty list is distinguishable from an unset one), skipping
// blank items. Shared by setFileListValue and its retrieval delegation.
func appendConfigListValue(target *[]string, item string) {
	if *target == nil {
		*target = []string{}
	}
	if strings.TrimSpace(item) == "" {
		return
	}
	*target = append(*target, item)
}

func setFileListValue(cfg *fileConfig, key, value string) {
	key = canonicalizeConfigKey(key)
	if setRetrievalFileListValue(cfg, key, value) {
		return
	}
	switch key {
	case "trusted_proxies":
		appendConfigListValue(&cfg.TrustedProxies, value)
	case "path_excludes":
		appendConfigListValue(&cfg.PathExcludes, value)
	case "ingest_exclude_dirs":
		appendConfigListValue(&cfg.IngestExcludeDirs, value)
	case "secret_patterns":
		appendConfigListValue(&cfg.SecretPatterns, value)
	case "allowed_origins":
		appendConfigListValue(&cfg.AllowedOrigins, value)
	case "media.translate.target_langs":
		appendConfigListValue(&cfg.MediaTranslateTargetLangs, value)
	case "media.stt.tracks":
		appendConfigListValue(&cfg.MediaSTTTracks, value)
	case "media.filter_words":
		appendConfigListValue(&cfg.MediaFilterWords, value)
	case "media.subtitles.glossary":
		appendConfigListValue(&cfg.MediaSubtitlesGlossary, value)
	case "media.subtitles.drop_phrases":
		appendConfigListValue(&cfg.MediaSubtitlesDropPhrases, value)
	case "media.subtitles.scrub_phrases":
		appendConfigListValue(&cfg.MediaSubtitlesScrubPhrases, value)
	}
}

// setRetrievalFileListValue appends the retrieval.* list-valued keys
// (cross-lingual target langs #574, hierarchical source_reps/levels #329),
// returning true when key matched. Split out of setFileListValue so it stays
// within the gocyclo budget once media.stt.tracks (#567) lands alongside them.
func setRetrievalFileListValue(cfg *fileConfig, key, value string) bool {
	switch key {
	case "retrieval.cross_lingual.target_langs":
		appendConfigListValue(&cfg.CrossLingualTargetLangs, value)
	case "retrieval.hierarchical.source_reps":
		appendConfigListValue(&cfg.RetrievalHierarchicalSourceReps, value)
	case "retrieval.hierarchical.levels":
		appendConfigListValue(&cfg.RetrievalHierarchicalLevels, value)
	default:
		return false
	}
	return true
}

// isListConfigKey reports whether key (after canonicalization) maps to
// a list-valued config field.
func isListConfigKey(key string) bool {
	key = canonicalizeConfigKey(key)
	switch key {
	case "trusted_proxies", "path_excludes", "ingest_exclude_dirs", "secret_patterns", "allowed_origins", "media.translate.target_langs", "media.stt.tracks", "media.filter_words", "media.subtitles.glossary", "media.subtitles.drop_phrases", "media.subtitles.scrub_phrases", "retrieval.cross_lingual.target_langs", "retrieval.hierarchical.source_reps", "retrieval.hierarchical.levels":
		return true
	default:
		return false
	}
}

// marshalConfigYAML renders a persistedConfig as the flat-key YAML
// written to .dir2mcp.yaml / the effective snapshot.
func marshalConfigYAML(cfg persistedConfig) ([]byte, error) {
	var b strings.Builder
	writeScalar := func(key, value string) {
		value = strings.ReplaceAll(value, "\n", "\\n")
		b.WriteString(key)
		b.WriteString(": ")
		b.WriteString(value)
		b.WriteByte('\n')
	}
	writeInt := func(key string, value int) {
		writeScalar(key, strconv.Itoa(value))
	}
	writeBool := func(key string, value bool) {
		writeScalar(key, strconv.FormatBool(value))
	}
	// writeComment emits YAML comment lines. The loader skips them (a line whose
	// first non-space character is `#`), so they document the generated file for
	// the operator without changing what is read back.
	writeComment := func(lines ...string) {
		for _, line := range lines {
			b.WriteString("# ")
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	writeList := func(key string, values []string) {
		b.WriteString(key)
		b.WriteString(":")
		if len(values) == 0 {
			b.WriteString(" []\n")
			return
		}
		b.WriteByte('\n')
		for _, value := range values {
			b.WriteString("  - ")
			b.WriteString(value)
			b.WriteByte('\n')
		}
	}

	writeScalar("root_dir", cfg.RootDir)
	writeScalar("state_dir", cfg.StateDir)
	writeScalar("listen_addr", cfg.ListenAddr)
	writeScalar("mcp_path", cfg.MCPPath)
	writeScalar("protocol_version", cfg.ProtocolVersion)
	writeBool("public", cfg.Public)
	writeScalar("auth_mode", cfg.AuthMode)
	writeScalar("server_name", cfg.ServerName)
	writeInt("rate_limit_rps", cfg.RateLimitRPS)
	writeInt("rate_limit_burst", cfg.RateLimitBurst)
	writeList("trusted_proxies", cfg.TrustedProxies)
	writeList("path_excludes", cfg.PathExcludes)
	writeList("secret_patterns", cfg.SecretPatterns)
	writeScalar("docling_command", cfg.DoclingCommand)
	writeScalar("docling_serve_url", cfg.DoclingServeURL)
	writeScalar("pandoc_command", cfg.PandocCommand)
	writeScalar("session_inactivity_timeout", cfg.SessionInactivityTimeout.String())
	writeScalar("session_max_lifetime", cfg.SessionMaxLifetime.String())
	writeScalar("health_check_interval", cfg.HealthCheckInterval.String())
	writeScalar("elevenlabs_base_url", cfg.ElevenLabsBaseURL)
	writeScalar("elevenlabs_tts_voice_id", cfg.ElevenLabsTTSVoiceID)
	writeList("allowed_origins", cfg.AllowedOrigins)
	writeBool("rag_generate_answer", cfg.RAGGenerateAnswer)
	writeInt("rag_k_default", cfg.RAGKDefault)
	writeScalar("rag_system_prompt", cfg.RAGSystemPrompt)
	writeInt("rag_max_context_chars", cfg.RAGMaxContextChars)
	writeInt("rag_oversample_factor", cfg.RAGOversampleFactor)
	writeBool("retrieval_hybrid_enabled", cfg.RetrievalHybridEnabled)
	writeBool("dedup_retrieval", cfg.DedupRetrieval)
	writeScalar("retrieval_min_score", strconv.FormatFloat(cfg.RetrievalMinScore, 'f', -1, 64))
	writeScalar("retrieval_recency_half_life", cfg.RetrievalRecencyHalfLife.String())
	writeBool("context_compression_enabled", cfg.ContextCompressionEnabled)
	writeScalar("context_compression_target_ratio", strconv.FormatFloat(cfg.ContextCompressionTargetRatio, 'f', -1, 64))
	writeBool("retrieval_adaptive_enabled", cfg.RetrievalAdaptiveEnabled)
	writeInt("retrieval_adaptive_k_min", cfg.RetrievalAdaptiveKMin)
	writeInt("retrieval_adaptive_k_max", cfg.RetrievalAdaptiveKMax)
	writeBool("retrieval_mmr_enabled", cfg.RetrievalMMREnabled)
	writeScalar("retrieval_mmr_lambda", strconv.FormatFloat(cfg.RetrievalMMRLambda, 'f', -1, 64))
	writeBool("retrieval_hyde_enabled", cfg.RetrievalHyDEEnabled)
	writeScalar("retrieval_hyde_mode", cfg.RetrievalHyDEMode)
	writeBool("retrieval_contextual_enabled", cfg.RetrievalContextualEnabled)
	writeScalar("retrieval_contextual_provider", cfg.RetrievalContextualProvider)
	writeScalar("retrieval_contextual_model", cfg.RetrievalContextualModel)
	writeInt("retrieval_contextual_max_tokens", cfg.RetrievalContextualMaxTokens)
	writeScalar("retrieval_contextual_prompt_version", cfg.RetrievalContextualPromptVersion)
	writeScalar("retrieval_contextual_prompt", cfg.RetrievalContextualPrompt)
	writeBool("cross_lingual_enabled", cfg.CrossLingualEnabled)
	writeList("cross_lingual_target_langs", cfg.CrossLingualTargetLangs)
	writeBool("retrieval_hierarchical_enabled", cfg.RetrievalHierarchicalEnabled)
	writeList("retrieval_hierarchical_source_reps", cfg.RetrievalHierarchicalSourceReps)
	writeList("retrieval_hierarchical_levels", cfg.RetrievalHierarchicalLevels)
	writeScalar("retrieval_hierarchical_provider", cfg.RetrievalHierarchicalProvider)
	writeInt("retrieval_hierarchical_max_tokens", cfg.RetrievalHierarchicalMaxTokens)
	writeScalar("retrieval_hierarchical_prompt_version", cfg.RetrievalHierarchicalPromptVersion)
	writeScalar("retrieval_hierarchical_prompt", cfg.RetrievalHierarchicalPrompt)
	writeBool("rerank_enabled", cfg.RerankEnabled)
	writeScalar("rerank_provider", cfg.RerankProvider)
	writeScalar("cohere_base_url", cfg.CohereBaseURL)
	writeScalar("rerank_model", cfg.RerankModel)
	writeInt("rerank_candidate_pool", cfg.RerankCandidatePool)
	writeInt("chunking_max_tokens", cfg.ChunkingMaxTokens)
	writeInt("chunking_overlap_tokens", cfg.ChunkingOverlapTokens)
	writeComment(
		"ingest_exclude_dirs: directory names discovery does not descend into (SPEC 7.1).",
		"A present key REPLACES this list in full. It does not add to it.",
		"An entry is a plain directory name. It is not a path and it is not a glob.",
		"`.dir2mcp` is always kept: the state directory is under root_dir, so to",
		"index it is self-referential.",
		"The gates compose by AND. A file is indexed only if this list,",
		"path_excludes and the optional .gitignore rules all admit it. These five",
		"names are in BOTH lists: .git, .dir2mcp, node_modules, vendor,",
		"__pycache__. To re-index one of them, clear the name from BOTH keys.",
		"Clearing one key alone changes nothing you can see.",
	)
	writeList("ingest_exclude_dirs", cfg.IngestExcludeDirs)
	writeBool("ingest_gitignore", cfg.IngestGitignore)
	writeBool("ingest_follow_symlinks", cfg.IngestFollowSymlinks)
	writeInt("ingest_max_file_mb", cfg.IngestMaxFileMB)
	writeComment(inertIngestModeCommentLines...)
	writeScalar("ingest_pdf_mode", cfg.IngestPDFMode)
	writeScalar("ingest_images_mode", cfg.IngestImagesMode)
	writeScalar("ingest_audio_mode", cfg.IngestAudioMode)
	writeScalar("ingest_archives_mode", cfg.IngestArchivesMode)
	writeScalar("ingest_extractor", cfg.IngestExtractor)
	writeScalar("ingest_on_unsupported", cfg.IngestOnUnsupported)
	writeScalar("index_backend", cfg.IndexBackend)
	writeBool("ingest_scan_cache", cfg.IngestScanCache)
	writeBool("ingest_late_chunking", cfg.IngestLateChunking)
	writeBool("ingest_watch", cfg.IngestWatch)
	writeScalar("ingest_watch_debounce", cfg.IngestWatchDebounce.String())
	writeScalar("stt_provider", cfg.STTProvider)
	writeScalar("recognize_provider", cfg.RecognizeProvider)
	writeScalar("recognize_serve_url", cfg.RecognizeServeURL)
	writeScalar("recognize_serve_command", cfg.RecognizeServeCommand)
	writeScalar("stt_mistral_model", cfg.STTMistralModel)
	writeScalar("stt_elevenlabs_model", cfg.STTElevenLabsModel)
	writeScalar("stt_elevenlabs_language_code", cfg.STTElevenLabsLanguageCode)
	writeBool("quality_gates_enabled", cfg.QualityGatesEnabled)
	writeBool("language_detection_enabled", cfg.LanguageDetectionEnabled)
	writeBool("media_sidecars_disabled", cfg.MediaSidecarsDisabled)
	writeBool("media_variants_group", cfg.MediaVariantsGroup)
	writeScalar("media_variants_select", cfg.MediaVariantsSelect)
	writeBool("media_translate_enabled", cfg.MediaTranslateEnabled)
	writeList("media_translate_target_langs", cfg.MediaTranslateTargetLangs)
	writeScalar("media_translate_engine", cfg.MediaTranslateEngine)
	writeList("media_filter_words", cfg.MediaFilterWords)
	writeBool("media_subtitles_ttml_enabled", cfg.MediaSubtitlesTTMLEnabled)
	writeInt("media_subtitles_ttml_align_tolerance_ms", cfg.MediaSubtitlesTTMLAlignToleranceMS)
	writeBool("media_subtitles_smil_enabled", cfg.MediaSubtitlesSMILEnabled)
	writeScalar("media_subtitles_segmentation", cfg.MediaSubtitlesSegmentation)
	writeList("media_subtitles_glossary", cfg.MediaSubtitlesGlossary)
	writeList("media_subtitles_drop_phrases", cfg.MediaSubtitlesDropPhrases)
	writeList("media_subtitles_scrub_phrases", cfg.MediaSubtitlesScrubPhrases)
	writeInt("media_subtitles_collapse_repeats", cfg.MediaSubtitlesCollapseRepeats)
	writeBool("media_subtitles_drop_urls", cfg.MediaSubtitlesDropURLs)
	writeBool("media_trim_leading_silence", cfg.MediaTrimLeadingSilence)
	writeScalar("media_silence_threshold_db", strconv.FormatFloat(cfg.MediaSilenceThresholdDB, 'f', -1, 64))
	writeBool("media_vad", cfg.MediaVAD)
	// Tri-state (SPEC §8.6.8): only emit when explicitly set so an omitted
	// (auto) diarization config round-trips as omitted rather than collapsing to
	// false. nil means auto-enable when the backend advertises the capability.
	if cfg.MediaDiarizeEnabled != nil {
		writeBool("media_diarize_enabled", *cfg.MediaDiarizeEnabled)
	}
	writeInt("media_audio_window_sec", cfg.MediaAudioWindowSec)
	writeInt("media_translate_whisper_window_sec", cfg.MediaTranslateWhisperWindowSec)
	writeInt("media_translate_window_lines", cfg.MediaTranslateWindowLines)
	writeInt("media_translate_context_lines", cfg.MediaTranslateContextLines)
	writeInt("media_video_window_sec", cfg.MediaVideoWindowSec)
	writeInt("media_clip_max_duration_ms", cfg.MediaClipMaxDurationMS)
	writeInt("media_clip_max_bytes", cfg.MediaClipMaxBytes)
	writeInt("media_stt_max_payload_mb", cfg.MediaSTTMaxPayloadMB)
	writeInt("media_stt_request_timeout_sec", cfg.MediaSTTRequestTimeoutSec)
	writeBool("media_stt_language_strict", cfg.MediaSTTLanguageStrict)
	writeScalar("media_stt_on_uncovered_language", cfg.MediaSTTOnUncoveredLanguage)
	writeList("media_stt_tracks", cfg.MediaSTTTracks)
	writeBool("media_batch_two_phase", cfg.MediaBatchTwoPhase)
	writeBool("media_batch_progress", cfg.MediaBatchProgress)
	writeScalar("media_batch_manifest", cfg.MediaBatchManifest)
	writeScalar("server_tls_cert_file", cfg.ServerTLSCertFile)
	writeScalar("server_tls_key_file", cfg.ServerTLSKeyFile)
	writeScalar("x402_mode", cfg.X402Mode)
	writeScalar("x402_facilitator_url", cfg.X402FacilitatorURL)
	// token is never written to disk
	// writeScalar("x402_facilitator_token", cfg.X402FacilitatorToken)
	writeScalar("x402_resource_base_url", cfg.X402ResourceBaseURL)
	writeBool("x402_tools_call_enabled", cfg.X402ToolsCallEnabled)
	writeScalar("x402_price_atomic", cfg.X402PriceAtomic)
	writeScalar("x402_network", cfg.X402Network)
	writeScalar("x402_scheme", cfg.X402Scheme)
	writeScalar("x402_asset", cfg.X402Asset)
	writeScalar("x402_pay_to", cfg.X402PayTo)
	writeScalar("source_kind", cfg.SourceKind)
	writeScalar("source_s3_bucket", cfg.SourceS3Bucket)
	writeScalar("source_s3_prefix", cfg.SourceS3Prefix)
	writeScalar("source_s3_region", cfg.SourceS3Region)
	writeScalar("source_s3_endpoint", cfg.SourceS3Endpoint)
	// S3 credentials are never written to disk (env/keychain-only).
	writeScalar("qdrant_url", cfg.QdrantURL)
	writeScalar("qdrant_collection", cfg.QdrantCollection)
	// qdrant api_key is never written to disk (env/keychain/.env.local-only).
	writeScalar("index_pgvector_schema", cfg.IndexPgvectorSchema)
	writeScalar("index_pgvector_table", cfg.IndexPgvectorTable)
	// index_pgvector_dsn is a secret and never written to disk (env/keychain/.env.local-only).
	writeBool("distributed_embed_enabled", cfg.DistributedEmbedEnabled)
	writeScalar("distributed_embed_broker", cfg.DistributedEmbedBroker)
	writeScalar("distributed_embed_sqlite_path", cfg.DistributedEmbedSQLitePath)
	writeInt("distributed_embed_max_attempts", cfg.DistributedEmbedMaxAttempts)
	// distributed_embed_broker_url is a secret (may embed credentials) and is
	// never written to disk (env/keychain/.env.local-only, SPEC §16.1.1).

	return []byte(b.String()), nil
}

// unquoteYAMLScalar trims value and strips matching single/double
// quotes (interpreting escapes in double-quoted form).
func unquoteYAMLScalar(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"") {
			if unquoted, err := strconv.Unquote(value); err == nil {
				return unquoted
			}
		}
		if strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") {
			return value[1 : len(value)-1]
		}
	}
	return value
}

// strPtr returns a pointer to value.
func strPtr(value string) *string { return &value }

// boolPtr returns a pointer to value.
func boolPtr(value bool) *bool { return &value }

// copyBoolPtr returns a new pointer to the same bool value, or nil when the
// input is nil. It preserves a tri-state *bool (nil / false / true) across a
// snapshot copy without aliasing the original pointer.
func copyBoolPtr(value *bool) *bool {
	if value == nil {
		return nil
	}
	v := *value
	return &v
}

// intPtr returns a pointer to value.
func intPtr(value int) *int { return &value }

func floatPtr(value float64) *float64 { return &value }

// applyEnvOverrides layers all supported environment-variable overrides
// onto cfg. Env always wins when present (an empty DIR2MCP_SERVER_NAME
// clears a YAML-set name).
func applyEnvOverrides(cfg *Config, overrideEnv map[string]string) error {
	if cfg == nil {
		return nil
	}
	// Env always wins when present so DIR2MCP_SERVER_NAME="" can clear a
	// YAML-set server.name and fall through to the auto-derived name at
	// resolution time. The trim happens later in identity.Resolve.
	if raw, ok := envLookup("DIR2MCP_SERVER_NAME", overrideEnv); ok {
		cfg.ServerName = strings.TrimSpace(raw)
	}
	applyIngestEnvOverrides(cfg, overrideEnv)
	applyElevenLabsEnvOverrides(cfg, overrideEnv)
	applyRerankEnvOverrides(cfg, overrideEnv)
	applyNetworkEnvOverrides(cfg, overrideEnv)
	applySessionEnvOverrides(cfg, overrideEnv)
	applyX402EnvOverrides(cfg, overrideEnv)
	applySourceEnvOverrides(cfg, overrideEnv)
	applyQdrantEnvOverrides(cfg, overrideEnv)
	applyIndexEnvOverrides(cfg, overrideEnv)
	return applyDistributedEmbedEnvOverrides(cfg, overrideEnv)
}

// applyDistributedEmbedEnvOverrides applies the distributed-embedding env
// overrides (issue #248, SPEC §8.7). Enabled/broker/sqlite_path/max_attempts are
// non-secret settings. The broker URL is a runtime-only secret (it may embed
// credentials): resolved through the existing precedence (env → keychain →
// file/.env.local) and NEVER persisted to the snapshot (SPEC §16.1.1). A
// malformed boolean/integer override is reported rather than silently ignored, so
// automation cannot believe an override applied when it did not.
func applyDistributedEmbedEnvOverrides(cfg *Config, env map[string]string) error {
	if raw, ok := envLookup("DIR2MCP_DISTRIBUTED_EMBED_ENABLED", env); ok {
		parsed, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("invalid DIR2MCP_DISTRIBUTED_EMBED_ENABLED %q: want a boolean", raw)
		}
		cfg.DistributedEmbed.Enabled = parsed
	}
	setTrimmedEnv(env, "DIR2MCP_DISTRIBUTED_EMBED_BROKER", &cfg.DistributedEmbed.Broker)
	setTrimmedEnv(env, "DIR2MCP_DISTRIBUTED_EMBED_SQLITE_PATH", &cfg.DistributedEmbed.BrokerSQLitePath)
	if raw, ok := envLookup("DIR2MCP_DISTRIBUTED_EMBED_MAX_ATTEMPTS", env); ok {
		parsed, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("invalid DIR2MCP_DISTRIBUTED_EMBED_MAX_ATTEMPTS %q: want an integer", raw)
		}
		cfg.DistributedEmbed.MaxAttempts = parsed
	}
	setSecretEnv(env, "DIR2MCP_DISTRIBUTED_EMBED_BROKER_URL", &cfg.DistributedEmbed.BrokerURL)
	return nil
}

// applyIndexEnvOverrides sources vector-index backend settings from the
// environment. The pgvector DSN is a runtime-only secret resolved through the
// existing precedence (env → keychain → file/.env.local): the loader has already
// layered keychain/.env.local onto DIR2MCP_INDEX_PGVECTOR_DSN by the time this
// runs, so reading it here honors all three sources. It is never persisted.
// Backend/schema/table are non-secret and may also be set via env for parity
// with the file keys.
func applyIndexEnvOverrides(cfg *Config, env map[string]string) {
	setTrimmedEnv(env, "DIR2MCP_INDEX_BACKEND", &cfg.IndexBackend)
	setTrimmedEnv(env, "DIR2MCP_INDEX_PGVECTOR_SCHEMA", &cfg.IndexPgvectorSchema)
	setTrimmedEnv(env, "DIR2MCP_INDEX_PGVECTOR_TABLE", &cfg.IndexPgvectorTable)
	setSecretEnv(env, "DIR2MCP_INDEX_PGVECTOR_DSN", &cfg.IndexPgvectorDSN)
}

// applyQdrantEnvOverrides applies the Qdrant vector-backend env overrides
// (issue #268). URL/collection are non-secret settings sourced from
// DIR2MCP_QDRANT_*. The api_key is a runtime-only secret resolved through the
// existing precedence (env → keychain → file/.env.local): the loader has already
// layered keychain/.env.local onto QDRANT_API_KEY by the time this runs, so
// reading it here honors all three sources. The api_key is never persisted.
func applyQdrantEnvOverrides(cfg *Config, env map[string]string) {
	setTrimmedEnv(env, "DIR2MCP_QDRANT_URL", &cfg.Qdrant.URL)
	setTrimmedEnv(env, "DIR2MCP_QDRANT_COLLECTION", &cfg.Qdrant.Collection)
	setSecretEnv(env, "QDRANT_API_KEY", &cfg.Qdrant.APIKey)
}

// applySourceEnvOverrides applies the corpus-source selection env overrides and
// resolves S3 credentials. Non-secret settings come from DIR2MCP_SOURCE_* and
// override file values when non-empty. Credentials are resolved at runtime only,
// through the existing secret precedence (env → keychain → file/.env.local): the
// loader has already layered keychain/.env.local values onto the standard AWS
// env vars by the time this runs, so reading them here honors all three sources.
// Credentials are never persisted to the snapshot.
func applySourceEnvOverrides(cfg *Config, env map[string]string) {
	setTrimmedEnv(env, "DIR2MCP_SOURCE_KIND", &cfg.Source.Kind)
	setTrimmedEnv(env, "DIR2MCP_SOURCE_S3_BUCKET", &cfg.Source.S3Bucket)
	setTrimmedEnv(env, "DIR2MCP_SOURCE_S3_PREFIX", &cfg.Source.S3Prefix)
	setTrimmedEnv(env, "DIR2MCP_SOURCE_S3_ENDPOINT", &cfg.Source.S3Endpoint)
	// Region prefers the dir2mcp-specific override, then the standard AWS var.
	if !setTrimmedEnv(env, "DIR2MCP_SOURCE_S3_REGION", &cfg.Source.S3Region) {
		setTrimmedEnv(env, "AWS_REGION", &cfg.Source.S3Region)
	}
	// Credentials are runtime-only (env/keychain/.env.local), never persisted.
	// Secret values are stored raw (untrimmed) but only when non-blank.
	setSecretEnv(env, "AWS_ACCESS_KEY_ID", &cfg.Source.S3AccessKeyID)
	setSecretEnv(env, "AWS_SECRET_ACCESS_KEY", &cfg.Source.S3SecretAccessKey)
	setSecretEnv(env, "AWS_SESSION_TOKEN", &cfg.Source.S3SessionToken)
}

// setTrimmedEnv assigns the trimmed value of env var key onto dst when present
// and non-blank, returning whether an assignment occurred.
func setTrimmedEnv(env map[string]string, key string, dst *string) bool {
	if raw, ok := envLookup(key, env); ok && strings.TrimSpace(raw) != "" {
		*dst = strings.TrimSpace(raw)
		return true
	}
	return false
}

// setSecretEnv assigns the raw (untrimmed) value of env var key onto dst when
// present and non-blank. Used for credentials, which must not be trimmed.
func setSecretEnv(env map[string]string, key string, dst *string) {
	if raw, ok := envLookup(key, env); ok && strings.TrimSpace(raw) != "" {
		*dst = raw
	}
}

// applyRerankEnvOverrides sources the Cohere key (secret) and
// optional rerank toggles from the environment. COHERE_API_KEY
// follows the same env-wins-if-nonempty rule as MISTRAL_API_KEY and
// is never persisted.
func applyRerankEnvOverrides(cfg *Config, env map[string]string) {
	if v, ok := envLookup("COHERE_API_KEY", env); ok && strings.TrimSpace(v) != "" {
		cfg.CohereAPIKey = v
	}
	if v, ok := envLookup("COHERE_BASE_URL", env); ok && strings.TrimSpace(v) != "" {
		cfg.CohereBaseURL = strings.TrimSpace(v)
	}
	if v, ok := envLookup("DIR2MCP_RERANK_ENABLED", env); ok && strings.TrimSpace(v) != "" {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			cfg.RerankEnabled = boolPtr(b)
		}
	}
	if v, ok := envLookup("DIR2MCP_RERANK_MODEL", env); ok && strings.TrimSpace(v) != "" {
		cfg.RerankModel = strings.TrimSpace(v)
	}
}

// applyIngestEnvOverrides applies the ingest-related env overrides.
// Mistral provider credentials/models are no longer flat config: the
// MISTRAL_API_KEY env var is consumed by the built-in `mistral`/
// `mistral-ocr` profiles via their ${MISTRAL_API_KEY} placeholder
// (expanded at resolution time); base URL / model overrides go through
// providers:/model: in .dir2mcp.yaml.
func applyIngestEnvOverrides(cfg *Config, env map[string]string) {
	// Trimmed string overrides applied only when the variable is set and
	// non-empty; collected in a table so adding a field stays linear in
	// cyclomatic complexity.
	for _, o := range []struct {
		key   string
		field *string
	}{
		{"DIR2MCP_DOCLING_COMMAND", &cfg.DoclingCommand},
		{"DIR2MCP_DOCLING_SERVE_URL", &cfg.IngestDoclingServeURL},
		{"DIR2MCP_PANDOC_COMMAND", &cfg.IngestPandocCommand},
		{"DIR2MCP_INGEST_EXTRACTOR", &cfg.IngestExtractor},
		{"DIR2MCP_INGEST_ON_UNSUPPORTED", &cfg.IngestOnUnsupported},
		{"DIR2MCP_INDEX_BACKEND", &cfg.IndexBackend},
	} {
		if raw, ok := envLookup(o.key, env); ok && strings.TrimSpace(raw) != "" {
			*o.field = strings.TrimSpace(raw)
		}
	}
	// Boolean overrides applied only when the variable is set and parses; a table
	// keeps this linear in cyclomatic complexity as boolean keys are added.
	for _, o := range []struct {
		key   string
		field *bool
	}{
		{"DIR2MCP_INGEST_SCAN_CACHE", &cfg.IngestScanCache},
		{"DIR2MCP_INGEST_LATE_CHUNKING", &cfg.IngestLateChunking},
		{"DIR2MCP_INGEST_WATCH", &cfg.IngestWatch},
		{"DIR2MCP_RETRIEVAL_HYBRID_ENABLED", &cfg.RetrievalHybridEnabled},
		{"DIR2MCP_RETRIEVAL_ADAPTIVE_ENABLED", &cfg.RetrievalAdaptiveEnabled},
		{"DIR2MCP_RETRIEVAL_CONTEXTUAL_ENABLED", &cfg.RetrievalContextualEnabled},
	} {
		applyBoolEnvField(o.field, o.key, env)
	}
	if raw, ok := envLookup("DIR2MCP_INGEST_WATCH_DEBOUNCE", env); ok && strings.TrimSpace(raw) != "" {
		applyDurationEnvField(cfg, raw, "DIR2MCP_INGEST_WATCH_DEBOUNCE", &cfg.IngestWatchDebounce)
	}
	applyRetrievalAdaptiveKEnv(cfg, env)
}

// applyBoolEnvField sets *field from env[key] when the variable is set, non-empty,
// and parses as a boolean; an unset/empty/unparseable value leaves field
// unchanged (preserving the prior best-effort, silently-ignore-garbage behavior).
func applyBoolEnvField(field *bool, key string, env map[string]string) {
	if raw, ok := envLookup(key, env); ok && strings.TrimSpace(raw) != "" {
		if parsed, err := strconv.ParseBool(strings.TrimSpace(raw)); err == nil {
			*field = parsed
		}
	}
}

// applyRetrievalAdaptiveKEnv applies the opt-in adaptive-gate k-bound int env
// overrides (DIR2MCP_RETRIEVAL_ADAPTIVE_K_MIN / _K_MAX), each only when set and
// non-empty. A table keeps the cyclomatic complexity flat as bounds are added.
func applyRetrievalAdaptiveKEnv(cfg *Config, env map[string]string) {
	for _, o := range []struct {
		key   string
		field *int
	}{
		{"DIR2MCP_RETRIEVAL_ADAPTIVE_K_MIN", &cfg.RetrievalAdaptiveKMin},
		{"DIR2MCP_RETRIEVAL_ADAPTIVE_K_MAX", &cfg.RetrievalAdaptiveKMax},
	} {
		if raw, ok := envLookup(o.key, env); ok && strings.TrimSpace(raw) != "" {
			if parsed, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
				*o.field = parsed
			}
		}
	}
}

// applyElevenLabsEnvOverrides applies the ElevenLabs API key, base URL,
// and voice ID env overrides (only when non-empty).
func applyElevenLabsEnvOverrides(cfg *Config, env map[string]string) {
	if apiKey, ok := envLookup("ELEVENLABS_API_KEY", env); ok && strings.TrimSpace(apiKey) != "" {
		cfg.ElevenLabsAPIKey = apiKey
	}
	if baseURL, ok := envLookup("ELEVENLABS_BASE_URL", env); ok && strings.TrimSpace(baseURL) != "" {
		cfg.ElevenLabsBaseURL = baseURL
	}
	if voiceID, ok := envLookup("ELEVENLABS_VOICE_ID", env); ok && strings.TrimSpace(voiceID) != "" {
		cfg.ElevenLabsTTSVoiceID = strings.TrimSpace(voiceID)
	}
}

// applyNetworkEnvOverrides merges allowed-origins and trusted-proxy env
// lists and applies rate-limit env overrides (ignoring invalid/negative
// numbers).
func applyNetworkEnvOverrides(cfg *Config, env map[string]string) {
	if allowedOrigins, ok := envLookup("DIR2MCP_ALLOWED_ORIGINS", env); ok {
		cfg.AllowedOrigins = MergeAllowedOrigins(cfg.AllowedOrigins, allowedOrigins)
	}
	if rawRPS, ok := envLookup("DIR2MCP_RATE_LIMIT_RPS", env); ok {
		if rps, err := strconv.Atoi(strings.TrimSpace(rawRPS)); err == nil && rps >= 0 {
			cfg.RateLimitRPS = rps
		}
	}
	if rawBurst, ok := envLookup("DIR2MCP_RATE_LIMIT_BURST", env); ok {
		if burst, err := strconv.Atoi(strings.TrimSpace(rawBurst)); err == nil && burst >= 0 {
			cfg.RateLimitBurst = burst
		}
	}
	if trustedProxies, ok := envLookup("DIR2MCP_TRUSTED_PROXIES", env); ok {
		cfg.TrustedProxies = MergeTrustedProxies(cfg.TrustedProxies, trustedProxies)
	}
}

// applySessionEnvOverrides applies the session inactivity/max-lifetime
// and health-check-interval duration env overrides, preferring the new
// inactivity var name and warning on the deprecated alias.
func applySessionEnvOverrides(cfg *Config, env map[string]string) {
	// session-related environment variables are durations parsed by time.ParseDuration.
	// Syntactically invalid values (parse errors) are warned about but not fatal; values
	// that parse successfully (including negative durations) are stored and may still
	// cause Validate() to fail later.
	// Historically the variable was named DIR2MCP_SESSION_TIMEOUT; we
	// elect to prefer the more explicit DIR2MCP_SESSION_INACTIVITY_TIMEOUT
	// while still accepting the old name for compatibility.
	if raw, ok := envLookup("DIR2MCP_SESSION_INACTIVITY_TIMEOUT", env); ok {
		applyDurationEnvField(cfg, raw, "DIR2MCP_SESSION_INACTIVITY_TIMEOUT", &cfg.SessionInactivityTimeout)
	} else if raw, ok := envLookup("DIR2MCP_SESSION_TIMEOUT", env); ok {
		// fallback to old name
		trimmed := strings.TrimSpace(raw)
		if trimmed != "" {
			cfg.Warnings = append(cfg.Warnings, fmt.Errorf("DIR2MCP_SESSION_TIMEOUT is deprecated; use DIR2MCP_SESSION_INACTIVITY_TIMEOUT instead (current value: %q)", trimmed))
			applyDurationEnvField(cfg, raw, "DIR2MCP_SESSION_TIMEOUT", &cfg.SessionInactivityTimeout)
		}
	}
	if raw, ok := envLookup("DIR2MCP_SESSION_MAX_LIFETIME", env); ok {
		applyDurationEnvField(cfg, raw, "DIR2MCP_SESSION_MAX_LIFETIME", &cfg.SessionMaxLifetime)
	}
	// health check interval env; zero duration interpreted as default later
	if raw, ok := envLookup("DIR2MCP_HEALTH_CHECK_INTERVAL", env); ok {
		applyDurationEnvField(cfg, raw, "DIR2MCP_HEALTH_CHECK_INTERVAL", &cfg.HealthCheckInterval)
	}
}

// applyDurationEnvField parses raw as a duration into target; an empty
// value is ignored and a parse error is recorded as a non-fatal warning.
func applyDurationEnvField(cfg *Config, raw, varName string, target *time.Duration) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return
	}
	d, err := time.ParseDuration(trimmed)
	if err != nil {
		cfg.Warnings = append(cfg.Warnings, fmt.Errorf("invalid duration for %s: %q (%v)", varName, trimmed, err))
		return
	}
	*target = d
}

// applyX402EnvOverrides applies the basic and route-policy x402 env
// overrides onto cfg.X402.
func applyX402EnvOverrides(cfg *Config, env map[string]string) {
	applyX402BasicEnvOverrides(cfg, env)
	applyX402RouteEnvOverrides(cfg, env)
}

// applyX402BasicEnvOverrides applies the x402 endpoint and pricing env
// overrides onto cfg.X402.
func applyX402BasicEnvOverrides(cfg *Config, env map[string]string) {
	applyX402EndpointEnvOverrides(cfg, env)
	applyX402PricingEnvOverrides(cfg, env)
}

// applyX402EndpointEnvOverrides applies the x402 mode, facilitator
// URL/token, and resource base URL env overrides (only when non-empty).
func applyX402EndpointEnvOverrides(cfg *Config, env map[string]string) {
	if raw, ok := envLookup("DIR2MCP_X402_MODE", env); ok && strings.TrimSpace(raw) != "" {
		cfg.X402.Mode = strings.TrimSpace(raw)
	}
	if raw, ok := envLookup("DIR2MCP_X402_FACILITATOR_URL", env); ok && strings.TrimSpace(raw) != "" {
		cfg.X402.FacilitatorURL = strings.TrimSpace(raw)
	}
	if raw, ok := envLookup("DIR2MCP_X402_FACILITATOR_TOKEN", env); ok && strings.TrimSpace(raw) != "" {
		cfg.X402.FacilitatorToken = strings.TrimSpace(raw)
	}
	if raw, ok := envLookup("DIR2MCP_X402_RESOURCE_BASE_URL", env); ok && strings.TrimSpace(raw) != "" {
		cfg.X402.ResourceBaseURL = strings.TrimSpace(raw)
	}
}

// applyX402PricingEnvOverrides applies the x402 tools-call-enabled and
// price env overrides (warning on an invalid boolean and accepting a
// compatibility alias for the price var).
func applyX402PricingEnvOverrides(cfg *Config, env map[string]string) {
	if raw, ok := envLookup("DIR2MCP_X402_TOOLS_CALL_ENABLED", env); ok && strings.TrimSpace(raw) != "" {
		trimmed := strings.TrimSpace(raw)
		if enabled, err := strconv.ParseBool(trimmed); err == nil {
			cfg.X402.ToolsCallEnabled = enabled
		} else {
			// record a non-fatal warning instead of printing directly to stderr so
			// callers of the loader can decide how to surface it.  Do not override
			// the existing value when parsing fails, keeping the prior setting
			// (which may be the default).
			cfg.Warnings = append(cfg.Warnings, fmt.Errorf("invalid boolean for DIR2MCP_X402_TOOLS_CALL_ENABLED: %q (%v)", trimmed, err))
		}
	}
	// prefer the atomic env var name matching the YAML key; fall back for compatibility
	if raw, ok := envLookup("DIR2MCP_X402_PRICE_ATOMIC", env); ok && strings.TrimSpace(raw) != "" {
		cfg.X402.PriceAtomic = strings.TrimSpace(raw)
	} else if raw, ok := envLookup("DIR2MCP_X402_PRICE", env); ok && strings.TrimSpace(raw) != "" {
		cfg.X402.PriceAtomic = strings.TrimSpace(raw)
	}
}

// applyX402RouteEnvOverrides applies the x402 route-policy network,
// scheme, asset, and pay-to env overrides (only when non-empty).
func applyX402RouteEnvOverrides(cfg *Config, env map[string]string) {
	if raw, ok := envLookup("DIR2MCP_X402_NETWORK", env); ok && strings.TrimSpace(raw) != "" {
		cfg.X402.Network = strings.TrimSpace(raw)
	}
	if raw, ok := envLookup("DIR2MCP_X402_SCHEME", env); ok && strings.TrimSpace(raw) != "" {
		cfg.X402.Scheme = strings.TrimSpace(raw)
	}
	if raw, ok := envLookup("DIR2MCP_X402_ASSET", env); ok && strings.TrimSpace(raw) != "" {
		cfg.X402.Asset = strings.TrimSpace(raw)
	}
	if raw, ok := envLookup("DIR2MCP_X402_PAY_TO", env); ok && strings.TrimSpace(raw) != "" {
		cfg.X402.PayTo = strings.TrimSpace(raw)
	}
}

// EffectiveKDefault returns the number of hits a request that OMITS `k`
// resolves to (SPEC §9.1, step 2 then step 3): `rag.k_default` when it is
// configured and in bound, and the shipped RAGKFallback otherwise.
//
// Every surface that takes a `k` reads it through this one method: the tool
// handlers, the `default` the served tool schemas advertise, the retrieval
// service and the CLI. One corpus therefore has ONE default, and the value a
// client reads from the schema is the value an omitted field produces.
//
// Validate rejects an out-of-bound configured value, so the fallback branch is
// reached only by a Config that never went through Default()/Load (a test or an
// embedder constructing a Config literal). It resolves such a Config to the
// shipped default rather than to 0, which would retrieve nothing.
func (c Config) EffectiveKDefault() int {
	if c.RAGKDefault >= RAGKMin && c.RAGKDefault <= RAGKMax {
		return c.RAGKDefault
	}
	return RAGKFallback
}

// Validate checks configuration consistency and applies normalization
// or defaults.  It currently enforces rules around session durations:
//
//   - both SessionInactivityTimeout and SessionMaxLifetime must be >= 0
//   - a zero SessionInactivityTimeout is interpreted as the default value
//     (24h) and is rewritten accordingly.  callers should invoke this
//     method after the config is loaded so they needn't special-case a
//     zero value elsewhere.
//
// Future validations unrelated to x402 should also live here.  Like
// ValidateX402, this method operates on a pointer receiver so that it can
// modify the receiver in-place.
func (c *Config) Validate() error {
	// Ordered validation gates. Run in sequence; the first failure short-circuits
	// (a loop rather than a straight-line if-chain so this stays under the
	// cyclomatic-complexity budget as gates are added).
	for _, validate := range []func() error{
		c.validateIngestOnUnsupported,
		c.validateIngestExtractor,
		c.validateIngestFormatModes,
		c.validateIndexBackend,
		// Before the provider-resolving gates (STT/media): an unrecognized
		// provider `kind:` must fail here with its own root-cause CONFIG_INVALID
		// (naming the profile + bad kind) rather than surfacing downstream as a
		// generic "no eligible provider" error (issue #440 F7).
		c.validateProviderKinds,
		c.validateMediaVariants,
		// Before the media gates: validateMediaTranslate/validateMediaDiarize
		// resolve the STT profile, so an unrecognized stt.provider must fail here
		// with its own root-cause CONFIG_INVALID rather than surfacing as a
		// misleading "requires STT provider to be kind:whisper" (optibot #592).
		c.validateSTTProvider,
		// After validateSTTProvider (so an unrecognized default selector fails with
		// its own root cause first): reject a language_providers route that names an
		// unknown or non-STT-capable profile (SPEC §8.2.1, #566) as CONFIG_INVALID.
		c.validateSTTLanguageProviders,
		c.validateMediaSTTOnUncoveredLanguage,
		c.validateRecognizeProvider,
		c.validateMediaTranslate,
		c.validateMediaSubtitles,
		c.validateMediaDiarize,
		c.validateNumericBounds,
		c.validateIngestExcludeDirs,
		c.validateSource,
		c.validateDistributedEmbed,
		c.validateMediaBatch,
		c.validateCrossLingual,
		c.validateMCPPath,
	} {
		if err := validate(); err != nil {
			return err
		}
	}
	c.applyValidationDefaults()
	if c.SessionMaxLifetime > 0 && c.SessionMaxLifetime < c.SessionInactivityTimeout {
		return fmt.Errorf("session_max_lifetime (%v) must be >= session_inactivity_timeout (%v)",
			c.SessionMaxLifetime, c.SessionInactivityTimeout)
	}
	return nil
}

// validateMCPPath fails fast (CONFIG_INVALID) when mcp_path is not a literal URL
// path this server can register (issue #653).
//
// The only check used to be a leading slash, applied in the `up` command. Go
// 1.22's http.ServeMux reads `{name}` in a pattern as a wildcard segment and
// PANICS on a malformed one, so `--mcp-path '/{'` took the whole daemon down
// with `panic: parsing "/{": at offset 1: bad wildcard segment` AFTER startup
// work had already run. A typo in a persisted config crashed the process
// instead of producing this repository's machine-parseable CONFIG_INVALID.
//
// The grammar is deliberately narrower than what ServeMux accepts: mcp_path
// names ONE endpoint, so pattern syntax is never useful here and accepting it
// would only create ways to write a path that does not mean what it looks like.
// Each rejected character is rejected for its own reason, named in the error, so
// an operator can fix the value rather than guess at a grammar.
func (c *Config) validateMCPPath() error {
	// The value is checked UNTRIMMED, because the server registers it untrimmed.
	// An earlier version of this gate validated a trimmed copy, so "/mcp " passed
	// and then panicked at registration: ServeMux reads a space as the separator
	// before the path, so "/mcp " parses as the METHOD "/mcp" with no path at all
	// (`invalid method "/mcp"`). Validating anything other than the value that is
	// used is the same class of defect this gate exists to close.
	path := c.MCPPath
	if path == "" {
		// An empty value keeps the default, which the loader supplies. Only an
		// actually empty string qualifies: a whitespace-only value is a typo, and
		// it panics registration rather than selecting the default.
		return nil
	}
	// Every Unicode space, not just ' ' and '\t'. A tab panics for the same reason
	// a space does; a newline and a non-breaking space register a route that is
	// not the one the operator wrote, which is worse than a refusal because the
	// daemon then runs and no client can reach it.
	for _, r := range path {
		if unicode.IsSpace(r) {
			return fmt.Errorf("mcp_path %q must not contain whitespace (found %q): "+
				"a space or tab panics route registration, and any other whitespace registers a path no client can request", path, r)
		}
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("mcp_path %q must start with '/'", path)
	}
	// `{` and `}` are the panic. ServeMux reads them as a wildcard segment.
	if strings.ContainsAny(path, "{}") {
		return fmt.Errorf("mcp_path %q must not contain '{' or '}': "+
			"Go's request multiplexer reads those as a wildcard pattern, and a malformed one panics the server", path)
	}
	// `?` and `#` never reach a handler as part of the path: the client sends the
	// query and the fragment separately, so a path containing them can never
	// match and the server would silently answer nothing on it.
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		return fmt.Errorf("mcp_path %q must not contain %q: a query or fragment is not part of the request path, so no request could ever match it",
			path, path[i:i+1])
	}
	// ServeMux redirects a request with a doubled slash to the cleaned path, so a
	// pattern holding one is registered at an address no client is served on.
	if strings.Contains(path, "//") {
		return fmt.Errorf("mcp_path %q must not contain '//': requests are redirected to the cleaned path, so nothing would be served on this one", path)
	}
	// A trailing slash makes ServeMux treat the pattern as a SUBTREE, so
	// `/mcp/` would also claim `/mcp/anything`. mcp_path names one endpoint.
	if path != "/" && strings.HasSuffix(path, "/") {
		return fmt.Errorf("mcp_path %q must not end with '/': a trailing slash registers a subtree rather than the single endpoint mcp_path names", path)
	}
	return nil
}

// validateSTTProvider fails fast (CONFIG_INVALID) when stt.provider names a
// selector no STT resolver can map (issue #440 F6). Empty/`auto` and the off
// aliases are valid; every other value must map to a built-in profile via
// STTSelectorProfile. Without this gate an unknown or unmappable selector (a
// typo, or a backend the selector table forgot) silently disabled STT on the
// expected-language, derivation-identity, and diarization-gating paths while
// erroring only at first transcription — a divergence surfaced far from its
// cause. Validating here makes the misconfiguration fail at startup, uniformly.
func (c *Config) validateSTTProvider() error {
	sel := strings.ToLower(strings.TrimSpace(c.STTProvider))
	switch sel {
	case "", "auto", "off", "none", "disabled":
		return nil
	}
	if _, ok := STTSelectorProfile(sel); ok {
		return nil
	}
	return fmt.Errorf("CONFIG_INVALID: stt.provider %q is not a recognized speech-to-text backend; "+
		"use one of auto, mistral, elevenlabs, whisper, openai, gemini, or off", c.STTProvider)
}

// validateRecognizeProvider fails fast (CONFIG_INVALID) on a misconfigured
// recognition binding (design 0004 §3): the only providers are off (default)
// and serve, and serve requires a usable http(s) base URL — a recognizer
// silently disabled by a bad URL would only surface at first video ingest,
// far from the misconfiguration.
func (c *Config) validateRecognizeProvider() error {
	sel := strings.ToLower(strings.TrimSpace(c.RecognizeProvider))
	switch sel {
	case "", "off", "none", "disabled":
		if strings.TrimSpace(c.RecognizeServeCommand) != "" {
			return fmt.Errorf("CONFIG_INVALID: recognize.serve_command is set but recognize.provider is %q; "+
				"set recognize.provider to serve (or remove the command)", c.RecognizeProvider)
		}
		return nil
	case "serve":
		u, err := url.Parse(strings.TrimSpace(c.RecognizeServeURL))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("CONFIG_INVALID: recognize.provider \"serve\" requires recognize.base_url "+
				"to be an http(s) URL, got %q", c.RecognizeServeURL)
		}
		if u.User != nil {
			// Reject embedded credentials outright: recognize.base_url is persisted
			// verbatim into config/snapshot YAML and echoed in health-probe logs, so
			// user:password@host would leak the secret. The error deliberately does
			// NOT echo the URL. The local recognition backend is unauthenticated;
			// any real auth belongs out of band, not in the persisted base URL.
			return fmt.Errorf("CONFIG_INVALID: recognize.base_url must not embed credentials (user:password@host)")
		}
		return nil
	}
	return fmt.Errorf("CONFIG_INVALID: recognize.provider %q is not a recognized recognition backend; "+
		"use serve or off", c.RecognizeProvider)
}

// validateMediaBatch enforces the media.batch (SPEC §8.6.11) invariants. All
// three features — the two-phase pass split (media.batch.two_phase), the JSONL
// run manifest, and side-channel progress — are implemented. The two-phase split
// is observably equivalent to single-pass for the resulting
// representations/chunks/embeddings/citations (it reorders work into a
// transcription pass then a derivation pass, never changing output), so it has no
// config interdependencies to reject here; it is a self-contained ordering toggle.
func (c *Config) validateMediaBatch() error {
	return nil
}

// validateMediaVariants normalizes MediaVariantsSelect (defaulting empty to
// "best") and rejects any value outside best/first (spec §8.6.5,
// media.variants.select). The select policy is only consulted when grouping is
// enabled, but it is validated unconditionally so a bad value is caught at save
// time.
func (c *Config) validateMediaVariants() error {
	sel := strings.ToLower(strings.TrimSpace(c.MediaVariantsSelect))
	if sel == "" {
		sel = Default().MediaVariantsSelect
	}
	switch sel {
	case "best", "first":
	default:
		return fmt.Errorf("media.variants.select must be one of best, first: %q", c.MediaVariantsSelect)
	}
	c.MediaVariantsSelect = sel
	return nil
}

// validateMediaTranslate enforces the transcript-translation config invariants
// (spec §8.6.2): translation is opt-in and off by default, and the target
// language list has NO built-in default. Enabling translation with an empty (or
// all-blank) target_langs is CONFIG_INVALID. When enabled the list is
// normalized (trimmed, lower-cased, de-duplicated) so downstream keying via
// TranscriptLangSuffix is stable. When translation is disabled the list is left
// untouched, so toggling it back on restores any previously-saved targets.
func (c *Config) validateMediaTranslate() error {
	if !c.MediaTranslateEnabled {
		return nil
	}
	out, err := normalizeMediaTranslateTargets(c.MediaTranslateTargetLangs)
	if err != nil {
		return err
	}
	if len(out) == 0 {
		return errors.New("media.translate.enabled=true requires a non-empty " +
			"media.translate.target_langs (no default target language)")
	}
	c.MediaTranslateTargetLangs = out

	engine, err := c.validateMediaTranslateEngine(out)
	if err != nil {
		return err
	}
	c.MediaTranslateEngine = engine
	return nil
}

// normalizeMediaTranslateTargets trims, lower-cases and de-duplicates the
// media.translate.target_langs list, rejecting any tag outside the cache-safe
// BCP-47 alphabet. Split out of validateMediaTranslate to keep that function
// under the cyclomatic-complexity budget; behaviour is unchanged.
func normalizeMediaTranslateTargets(langs []string) ([]string, error) {
	seen := make(map[string]struct{}, len(langs))
	out := make([]string, 0, len(langs))
	for _, lang := range langs {
		l := strings.ToLower(strings.TrimSpace(lang))
		if l == "" {
			continue
		}
		// Reject tags outside the cache-safe alphabet (BCP-47 letters/digits/
		// hyphen). TranscriptLangSuffix strips any other rune, so e.g. "en_us"
		// and "enus" would collapse to the same translation-cache suffix and
		// reuse the wrong translated text. Require an already-safe form.
		for _, r := range l {
			safe := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
			if !safe {
				return nil, fmt.Errorf("media.translate.target_langs contains an invalid language tag %q "+
					"(use BCP-47 letters/digits/hyphen, e.g. \"en\" or \"pt-br\")", lang)
			}
		}
		if _, dup := seen[l]; dup {
			continue
		}
		seen[l] = struct{}{}
		out = append(out, l)
	}
	return out, nil
}

// validateMediaTranslateEngine resolves and validates the translation engine
// against the normalized target list. Split out of validateMediaTranslate to
// keep that function under the cyclomatic-complexity budget; behaviour is
// unchanged.
//
// Translation engine: "chat" (default, line-by-line via the chat generator)
// or "whisper" (native audio->English translate task). Whisper only produces
// English and needs a whisper STT provider, so validate those preconditions
// here where the normalized target list and resolved providers are available.
func (c *Config) validateMediaTranslateEngine(out []string) (string, error) {
	engine := strings.ToLower(strings.TrimSpace(c.MediaTranslateEngine))
	if engine == "" {
		engine = Default().MediaTranslateEngine
	}
	switch engine {
	case "chat":
	case "whisper":
		prof, ok := resolveSTTProfileForCapability(*c)
		if !ok || prof.Kind != provider.KindWhisper {
			return "", errors.New("CONFIG_INVALID: media.translate.engine=whisper requires the STT provider " +
				"to be kind:whisper (set stt.provider: whisper), or use media.translate.engine=chat")
		}
		for _, lang := range out {
			if strings.SplitN(lang, "-", 2)[0] != "en" {
				return "", fmt.Errorf("CONFIG_INVALID: media.translate.engine=whisper only produces English; set "+
					"media.translate.target_langs: [en] (got %q) or use media.translate.engine=chat", lang)
			}
		}
	default:
		return "", fmt.Errorf("media.translate.engine must be one of chat, whisper: %q", c.MediaTranslateEngine)
	}
	return engine, nil
}

// crossLingualAutoSentinel is the target-langs value meaning "expand into the
// corpus's detected languages (#267)" rather than an explicit pinned list. It is
// resolved at startup against the live corpus, so it is accepted by validation
// even though it is not a BCP-47 tag.
const crossLingualAutoSentinel = "auto"

// validateCrossLingual enforces the cross-lingual query-expansion config
// invariants (#325). The feature is opt-in and off by default. The target-langs
// list is normalized (trimmed, lower-cased, de-duplicated). An empty list is
// valid and means "auto" (the corpus's detected languages, resolved at startup).
// An explicit list may contain the "auto" sentinel and/or BCP-47 tags; any other
// tag must use the cache-safe BCP-47 alphabet (letters/digits/hyphen), matching
// validateMediaTranslate. Validation runs regardless of the enable flag so a
// saved-but-disabled list round-trips and re-enabling restores it.
func (c *Config) validateCrossLingual() error {
	seen := make(map[string]struct{}, len(c.CrossLingualTargetLangs))
	out := make([]string, 0, len(c.CrossLingualTargetLangs))
	for _, lang := range c.CrossLingualTargetLangs {
		l := strings.ToLower(strings.TrimSpace(lang))
		if l == "" {
			continue
		}
		if l != crossLingualAutoSentinel {
			for _, r := range l {
				safe := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
				if !safe {
					return fmt.Errorf("retrieval.cross_lingual.target_langs contains an invalid language tag %q "+
						"(use \"auto\" or BCP-47 letters/digits/hyphen, e.g. \"en\" or \"pt-br\")", lang)
				}
			}
			// Reject structurally malformed tags (e.g. "-", "en-", "en--us"): a
			// leading/trailing/empty subtag is character-class-safe but has no
			// primary subtag, so it would be silently dropped at target resolution.
			// Failing here keeps misconfiguration explicit rather than silent.
			for _, sub := range strings.Split(l, "-") {
				if sub == "" {
					return fmt.Errorf("retrieval.cross_lingual.target_langs contains a malformed language tag %q "+
						"(empty subtag; use \"auto\" or BCP-47 tags like \"en\" or \"pt-br\")", lang)
				}
			}
		}
		if _, dup := seen[l]; dup {
			continue
		}
		seen[l] = struct{}{}
		out = append(out, l)
	}
	if len(out) == 0 {
		c.CrossLingualTargetLangs = nil
	} else {
		c.CrossLingualTargetLangs = out
	}
	return nil
}

// validateMediaSubtitles enforces the optional bilingual subtitle-export config
// invariants (SPEC §8.6.10): the surface is off by default and additive, so
// nothing is validated against translation/STT state here. The align tolerance
// has a spec default (2500 ms); a zero/omitted value is normalized to the
// default and a negative value is rejected (a tolerance cannot be negative).
func (c *Config) validateMediaSubtitles() error {
	if c.MediaSubtitlesTTMLAlignToleranceMS < 0 {
		return fmt.Errorf("media.subtitles.ttml.align_tolerance_ms must not be negative: %d",
			c.MediaSubtitlesTTMLAlignToleranceMS)
	}
	if c.MediaSubtitlesTTMLAlignToleranceMS == 0 {
		c.MediaSubtitlesTTMLAlignToleranceMS = DefaultMediaSubtitlesAlignToleranceMS
	}
	segmentation := strings.ToLower(strings.TrimSpace(c.MediaSubtitlesSegmentation))
	if segmentation == "" {
		segmentation = Default().MediaSubtitlesSegmentation
	}
	switch segmentation {
	case "chunk", "broadcast":
	default:
		return fmt.Errorf("media.subtitles.segmentation must be one of chunk, broadcast: %q",
			c.MediaSubtitlesSegmentation)
	}
	c.MediaSubtitlesSegmentation = segmentation
	if c.MediaSubtitlesCollapseRepeats < 0 {
		return fmt.Errorf("media.subtitles.collapse_repeats must not be negative: %d",
			c.MediaSubtitlesCollapseRepeats)
	}
	// Fail fast on a malformed glossary (bad "pattern=>replacement" entry or an
	// invalid regexp) at config time rather than at export time. subtitle.NewGlossary
	// is the single source of truth for the entry grammar.
	if _, err := subtitle.NewGlossary(c.MediaSubtitlesGlossary); err != nil {
		return fmt.Errorf("media.subtitles.glossary: %w", err)
	}
	// Fail fast on a malformed drop phrase (invalid regexp) at config time rather
	// than at export time. subtitle.NewDropSet owns the pattern grammar.
	if _, err := subtitle.NewDropSet(c.MediaSubtitlesDropPhrases); err != nil {
		return fmt.Errorf("media.subtitles.drop_phrases: %w", err)
	}
	if _, err := subtitle.NewDropSet(c.MediaSubtitlesScrubPhrases); err != nil {
		return fmt.Errorf("media.subtitles.scrub_phrases: %w", err)
	}
	return nil
}

// validateIngestExtractor normalizes IngestExtractor (defaulting empty) and
// rejects any value outside auto/docling/docling-serve/pandoc/mistral/off. The
// accepted set is the one the error message prints, so the two cannot drift.
func (c *Config) validateIngestExtractor() error {
	normalized, err := normalizeClosedEnum("ingest.extractor", c.IngestExtractor, Default().IngestExtractor,
		[]string{"auto", "docling", "docling-serve", "pandoc", "mistral", "off"})
	if err != nil {
		return err
	}
	c.IngestExtractor = normalized
	return nil
}

// normalizeClosedEnum lowercases and trims value, substitutes fallback for an
// empty value, and returns an error naming key and the accepted set when the
// result is outside allowed. It is the shared body of the small closed-enum
// gates (ingest.extractor, ingest.on_unsupported, the per-format ingest modes),
// so a new enum costs one line here instead of another switch chain, and the
// gate that holds four of them stays inside the cyclomatic-complexity budget.
func normalizeClosedEnum(key, value, fallback string, allowed []string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		normalized = fallback
	}
	for _, candidate := range allowed {
		if normalized == candidate {
			return normalized, nil
		}
	}
	return "", fmt.Errorf("%s must be one of %s: %q", key, strings.Join(allowed, ", "), value)
}

// inertIngestModeCommentLines is the retraction the generated config carries
// directly above the four per-format mode keys (issue #843).
//
// The keys are validated (#655) but no accepted value reaches the ingestion
// runtime, so every value behaves the same. `ingest.archives.mode` is the sharp
// case: the shipped default is `deep`, the name promises that an archive nested
// inside an archive is expanded, and the implementation expands the top level
// only. An operator who reads the key in their own config file must read the
// retraction in the same place, or the name is the only thing they have to go on.
//
// The text is a package-level value rather than a literal inside
// marshalConfigYAML so a test can assert the generated file carries it without
// restating the wording.
var inertIngestModeCommentLines = []string{
	"The four per-format mode keys below are VALIDATED but INERT (#655, #843).",
	"A value outside its closed set is refused at startup, and the accepted",
	"value is stored and reported back. No accepted value changes ingestion.",
	"ingest_archives_mode is the one to watch: the default `deep` names",
	"recursive expansion of an archive nested inside an archive, and no such",
	"recursion exists. Archives are expanded at the TOP LEVEL only, under every",
	"value including `off`. A nested archive is stored as a skipped document",
	"with skip_reason=archive, so the gap shows in the coverage report.",
	"The canonical spec names the members of each set without defining what any",
	"member does, so dir2mcp refuses to invent the meaning. Knobs that DO work:",
	"ingest_extractor (PDF/image text), stt_provider (audio/video),",
	"ingest_on_unsupported (report a format nothing can read).",
}

// warnInertIngestModesOff warns when a config file sets a per-format mode key to
// `off` (issue #843).
//
// The four keys are inert, so no value is honored, but the values do not all
// cost the same to get wrong. `off` is the one member every reader understands
// identically ("do not do this"), and an operator who writes it has decided that
// content must NOT be sent to a provider or indexed. The key withholds nothing,
// so the belief is unsafe in the direction of cost and privacy, and there IS an
// action to take instead: ingest.extractor, stt.provider, security.path_excludes.
//
// The message states what still happens without overstating it. Archive
// expansion is unconditional, so it is asserted flatly. Extraction and
// transcription depend on a resolved extractor and STT provider, which the
// operator may already have turned off through the keys that do work, so they
// are qualified rather than claimed outright.
//
// The other members are left silent on purpose. None of them is a decision to
// withhold anything, `deep` is the shipped default, and #628 pins that a config
// dir2mcp itself generated loads with no warnings at all. A warning that fires
// on every start over a default the operator cannot improve on is a warning
// operators learn to skip, which would cost more than it buys. Their retraction
// is the comment the generated config carries (inertIngestModeCommentLines) and
// the README section, both read at exactly the moment the value is read.
//
// It is driven by the FILE fields, not by the merged Config, so `off` warns only
// when the operator actually wrote it. It is called from applyFileOverrides, so
// an effective-config snapshot (machine-written, carrying no operator intent)
// stays silent, matching the #661 split.
//
// cfg is never nil: the one caller returns early on a nil config.
func warnInertIngestModesOff(cfg *Config, fc fileConfig, path string) {
	var offKeys []string
	for _, mode := range []struct {
		key   string
		value *string
	}{
		{"ingest.pdf.mode", fc.IngestPDFMode},
		{"ingest.images.mode", fc.IngestImagesMode},
		{"ingest.audio.mode", fc.IngestAudioMode},
		{"ingest.archives.mode", fc.IngestArchivesMode},
	} {
		// Trim and fold the same way normalizeClosedEnum does, so the warning and
		// the gate agree on what counts as `off` (`OFF`, ` off ` included).
		if mode.value != nil && strings.EqualFold(strings.TrimSpace(*mode.value), "off") {
			offKeys = append(offKeys, mode.key)
		}
	}
	if len(offKeys) == 0 {
		return
	}
	cfg.Warnings = append(cfg.Warnings, fmt.Errorf(
		"config file %s: %s set to \"off\" but NOT honored: the per-format mode keys are validated only, "+
			"and no accepted value changes ingestion yet. These keys withhold nothing on their own. "+
			"PDFs and images are still extracted, and audio and video are still transcribed, whenever the "+
			"resolved extractor or STT provider is active; archives are still expanded at the top level "+
			"unconditionally. To actually turn these off use ingest.extractor=off, stt.provider=off, "+
			"or keep the files out with security.path_excludes",
		path, strings.Join(offKeys, ", ")))
}

// validateIngestFormatModes rejects a per-format ingest mode outside its
// advertised enum (issue #655).
//
// The four keys were parsed, persisted and reported with meaningful defaults, and
// nothing validated them: `ingest.archives.mode: shalow` loaded exactly as
// happily as `deep`. That is the dangerous half for media and archives, because
// an operator who mistypes a value believes a provider-cost or privacy setting is
// in force when no value at all was understood.
//
// The gate lives in Validate(), not in the `up` command, for the reason
// validateMCPPath gives: Validate() is the one gate every entry point runs (a
// persisted config, the CLI flag overlay, and the save paths), so a value that
// cannot be honored is refused wherever it is written, and no single command is
// the only place that notices. It also runs after the CLI overrides are merged,
// which is what the issue asks for.
//
// Each value is normalized in place, so the accepted set is closed and the stored
// value is canonical (`OFF` becomes `off`).
func (c *Config) validateIngestFormatModes() error {
	defaults := Default()
	for _, mode := range []struct {
		key      string
		value    *string
		fallback string
		allowed  []string
	}{
		{"ingest.pdf.mode", &c.IngestPDFMode, defaults.IngestPDFMode, []string{"off", "ocr", "auto"}},
		{"ingest.images.mode", &c.IngestImagesMode, defaults.IngestImagesMode, []string{"off", "ocr_auto", "ocr_on"}},
		{"ingest.audio.mode", &c.IngestAudioMode, defaults.IngestAudioMode, []string{"off", "auto", "on"}},
		{"ingest.archives.mode", &c.IngestArchivesMode, defaults.IngestArchivesMode, []string{"off", "shallow", "deep"}},
	} {
		normalized, err := normalizeClosedEnum(mode.key, *mode.value, mode.fallback, mode.allowed)
		if err != nil {
			return err
		}
		*mode.value = normalized
	}
	return nil
}

// validateIngestOnUnsupported normalizes IngestOnUnsupported (defaulting empty to
// the lenient default) and rejects any value outside lenient/strict. It is the
// §7.4.B.2 degradation-mode knob mirroring the tri-state opt-outs used elsewhere.
func (c *Config) validateIngestOnUnsupported() error {
	normalized, err := normalizeClosedEnum("ingest.on_unsupported", c.IngestOnUnsupported, Default().IngestOnUnsupported,
		[]string{"lenient", "strict"})
	if err != nil {
		return err
	}
	c.IngestOnUnsupported = normalized
	return nil
}

// onUncoveredLanguageWarn / onUncoveredLanguageSkip are the two honest-coverage
// floor actions for media.stt.on_uncovered_language (SPEC §8.2.1, #566). "warn"
// (default, fail-open) transcribes an uncovered-language item anyway and records
// the fact; "skip" (strict) records it as documents.status="skipped" with
// skip_reason="language_uncovered" instead of transcribing to degraded output.
const (
	onUncoveredLanguageWarn = "warn"
	onUncoveredLanguageSkip = "skip"
)

// validateMediaSTTOnUncoveredLanguage normalizes MediaSTTOnUncoveredLanguage
// (defaulting empty to the fail-open "warn" default) and rejects any value
// outside warn/skip (SPEC §8.2.1, #566). It mirrors validateIngestOnUnsupported:
// a small closed string enum with a lenient default so a bare config validates
// unchanged.
func (c *Config) validateMediaSTTOnUncoveredLanguage() error {
	action := strings.ToLower(strings.TrimSpace(c.MediaSTTOnUncoveredLanguage))
	if action == "" {
		action = Default().MediaSTTOnUncoveredLanguage
	}
	switch action {
	case onUncoveredLanguageWarn, onUncoveredLanguageSkip:
	default:
		return fmt.Errorf("media.stt.on_uncovered_language must be one of warn, skip: %q", c.MediaSTTOnUncoveredLanguage)
	}
	c.MediaSTTOnUncoveredLanguage = action
	return nil
}

// validateIndexBackend normalizes IndexBackend (defaulting empty to the
// "memory" baseline) and rejects any value outside memory/disk/qdrant/pgvector
// (issues #246, #268, #269). When qdrant is selected it enforces the qdrant
// persisted invariants (url required); when pgvector is selected it validates
// the schema/table names are safe SQL identifiers when set. It deliberately does
// NOT require the pgvector DSN: the DSN is a runtime-only secret (never
// persisted), so the missing-DSN check lives in the `up` startup path, not in
// Validate (which also runs at save time).
func (c *Config) validateIndexBackend() error {
	backend := strings.ToLower(strings.TrimSpace(c.IndexBackend))
	if backend == "" {
		backend = Default().IndexBackend
	}
	switch backend {
	case "memory", "disk":
	case "qdrant":
		c.IndexBackend = backend
		return c.validateQdrant()
	case "pgvector":
		if s := strings.TrimSpace(c.IndexPgvectorSchema); s != "" {
			if err := validateSQLIdentifier("index.pgvector.schema", s); err != nil {
				return err
			}
		}
		if t := strings.TrimSpace(c.IndexPgvectorTable); t != "" {
			if err := validateSQLIdentifier("index.pgvector.table", t); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("index.backend must be one of memory, disk, qdrant, pgvector: %q", c.IndexBackend)
	}
	c.IndexBackend = backend
	return nil
}

// validateQdrant enforces the Qdrant backend persisted invariants (issue #268):
// a URL is required and the URL/collection are trimmed. The api_key is NOT
// validated here: it is an optional runtime-only secret (an unsecured local
// instance needs none), resolved on env-aware load paths and never persisted,
// so requiring it here would break the env-skipping LoadFile/snapshot paths.
// Errors are CONFIG_INVALID-style (clear, actionable) so selecting qdrant
// without an endpoint fails loudly rather than at first request time.
func (c *Config) validateQdrant() error {
	c.Qdrant.URL = strings.TrimSpace(c.Qdrant.URL)
	c.Qdrant.Collection = strings.TrimSpace(c.Qdrant.Collection)
	if c.Qdrant.URL == "" {
		return errors.New("index.backend=qdrant requires index.qdrant.url " +
			"(e.g. http://localhost:6334 or https://<cluster>.cloud.qdrant.io:6334)")
	}
	return nil
}

// validateSQLIdentifier rejects a schema/table name that is not a safe,
// unqualified SQL identifier (letters, digits, underscore; not starting with a
// digit). Mirrors pgvectorindex.ValidateIdentifier but is kept local so the
// config package does not depend on the pgx-backed index package.
func validateSQLIdentifier(field, ident string) error {
	for i, r := range ident {
		isLetter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		isDigit := r >= '0' && r <= '9'
		if isLetter || r == '_' {
			continue
		}
		if isDigit && i > 0 {
			continue
		}
		return fmt.Errorf("%s %q contains an invalid character %q (allowed: letters, digits, underscore; must not start with a digit)", field, ident, string(r))
	}
	return nil
}

// validateNumericBounds rejects negative session/health durations, RAG,
// chunking, and ingest size values.
func (c *Config) validateNumericBounds() error {
	if c.SessionInactivityTimeout < 0 {
		return fmt.Errorf("session_inactivity_timeout must be non-negative: %v", c.SessionInactivityTimeout)
	}
	if c.SessionMaxLifetime < 0 {
		return fmt.Errorf("session_max_lifetime must be non-negative: %v", c.SessionMaxLifetime)
	}
	if c.HealthCheckInterval < 0 {
		return fmt.Errorf("health_check_interval must be non-negative: %v", c.HealthCheckInterval)
	}
	if c.IngestWatchDebounce < 0 {
		return fmt.Errorf("ingest.watch_debounce must be non-negative: %v", c.IngestWatchDebounce)
	}
	if err := c.validateMediaSTTNumericBounds(); err != nil {
		return err
	}
	if err := c.validateRAGNumericBounds(); err != nil {
		return err
	}
	if c.ChunkingMaxTokens < 0 {
		return fmt.Errorf("chunking.max_tokens must be non-negative: %d", c.ChunkingMaxTokens)
	}
	if c.ChunkingOverlapTokens < 0 {
		return fmt.Errorf("chunking.overlap_tokens must be non-negative: %d", c.ChunkingOverlapTokens)
	}
	// Relational guard (#405): an overlap >= the window size produces chunks
	// that never advance (or run backwards), so reject it. Only enforced when
	// a window is explicitly set (max_tokens > 0); 0 means "unset, use the
	// chunker default", where a nonzero overlap is not yet meaningful.
	if c.ChunkingMaxTokens > 0 && c.ChunkingOverlapTokens >= c.ChunkingMaxTokens {
		return fmt.Errorf("chunking.overlap_tokens (%d) must be less than chunking.max_tokens (%d)", c.ChunkingOverlapTokens, c.ChunkingMaxTokens)
	}
	if c.IngestMaxFileMB < 0 {
		return fmt.Errorf("ingest.max_file_mb must be non-negative: %d", c.IngestMaxFileMB)
	}
	if err := c.validateRetrievalNumericBounds(); err != nil {
		return err
	}
	return nil
}

// validateRAGNumericBounds validates the rag.* numeric knobs.
func (c *Config) validateRAGNumericBounds() error {
	if c.RAGMaxContextChars < 0 {
		return fmt.Errorf("rag.max_context_chars must be non-negative: %d", c.RAGMaxContextChars)
	}
	// rag.k_default carries the same bound as the `k` request field (SPEC 9.1),
	// and it fails HERE rather than at a later request: an operator who asks for
	// a k the tool schemas forbid must learn it at load, not from a request they
	// did not write.
	if c.RAGKDefault < RAGKMin || c.RAGKDefault > RAGKMax {
		return fmt.Errorf("rag.k_default must be between %d and %d: %d", RAGKMin, RAGKMax, c.RAGKDefault)
	}
	if c.RAGOversampleFactor < 0 {
		return fmt.Errorf("rag.oversample_factor must be non-negative: %d", c.RAGOversampleFactor)
	}
	return nil
}

// validateRetrievalNumericBounds validates the retrieval-tuning numeric knobs —
// the min_score relevance floor (#305), the recency time-decay half-life (#323),
// the context-compression keep-ratio (#335), and the MMR lambda (#340) —
// extracted from validateNumericBounds to keep it within the gocyclo budget.
// NaN/Inf are also rejected at parse time, but are re-guarded here so a
// programmatically-injected value still fails validation rather than silently
// corrupting behavior.
// validateMediaSTTNumericBounds checks the media.stt.* numeric config fields.
// Split from validateNumericBounds to keep it under the cyclomatic-complexity
// budget; behaviour is unchanged.
func (c *Config) validateMediaSTTNumericBounds() error {
	if c.MediaSTTMaxPayloadMB < 0 {
		return fmt.Errorf("media.stt.max_payload_mb must be non-negative (0 = client default): %d", c.MediaSTTMaxPayloadMB)
	}
	if c.MediaSTTRequestTimeoutSec < 0 {
		return fmt.Errorf("media.stt.request_timeout_sec must be non-negative (0 = client default): %d", c.MediaSTTRequestTimeoutSec)
	}
	// media.stt.tracks (SPEC §8.6.12): reject an invalid selection (unknown keyword,
	// keyword-mixed-with-indices, negative/non-integer/duplicate index) at startup so
	// a typo fails fast rather than per file. ParseSTTTracks is the single source of
	// truth for the grammar (also used by the ingest pipeline).
	if _, err := ParseSTTTracks(c.MediaSTTTracks); err != nil {
		return err
	}
	return nil
}

// validateMinScore validates retrieval.min_score. The floor is applied to a
// score RATIO to the result set's best hit, in [0,1] (#411, #858); 0 disables
// it. A negative floor
// would never drop anything, and a floor > 1 would silently drop EVERY hit, so
// both are rejected explicitly. Split out to keep validateRetrievalNumericBounds
// under the cyclomatic-complexity budget.
func (c *Config) validateMinScore() error {
	if c.RetrievalMinScore < 0 || c.RetrievalMinScore > 1 || math.IsNaN(c.RetrievalMinScore) || math.IsInf(c.RetrievalMinScore, 0) {
		return fmt.Errorf("retrieval.min_score must be in [0,1] (relative relevance, a ratio to the best hit; 0 disables the floor): %v", c.RetrievalMinScore)
	}
	return nil
}

func (c *Config) validateRetrievalNumericBounds() error {
	if err := c.validateMinScore(); err != nil {
		return err
	}
	// retrieval.recency_half_life is a time-decay half-life; 0 disables it. A
	// negative half-life would amplify rather than decay older content.
	if c.RetrievalRecencyHalfLife < 0 {
		return fmt.Errorf("retrieval.recency_half_life must be non-negative: %v", c.RetrievalRecencyHalfLife)
	}
	// retrieval.context_compression.target_ratio is a keep-fraction in (0,1];
	// 0 selects the built-in default at the retrieval layer.
	if math.IsNaN(c.ContextCompressionTargetRatio) || math.IsInf(c.ContextCompressionTargetRatio, 0) ||
		c.ContextCompressionTargetRatio < 0 || c.ContextCompressionTargetRatio > 1 {
		return fmt.Errorf("retrieval.context_compression.target_ratio must be in [0,1] (0 = default): %v", c.ContextCompressionTargetRatio)
	}
	// retrieval.mmr.lambda is the MMR relevance-vs-diversity trade-off and MUST
	// lie in [0,1]. Validate unconditionally (even when MMR is disabled) so a
	// malformed value is rejected deterministically at config time rather than
	// lying dormant until the knob is flipped on. NaN/Inf are already rejected at
	// parse time in setFloatFileScalar, but guard here too for values injected
	// programmatically (not via the file parser).
	if c.RetrievalMMRLambda < 0 || c.RetrievalMMRLambda > 1 ||
		math.IsNaN(c.RetrievalMMRLambda) || math.IsInf(c.RetrievalMMRLambda, 0) {
		return fmt.Errorf("retrieval.mmr.lambda must be within [0,1]: %v", c.RetrievalMMRLambda)
	}
	if err := c.normalizeHyDEMode(); err != nil {
		return err
	}
	if err := c.validateAdaptiveBounds(); err != nil {
		return err
	}
	if err := c.normalizeHierarchical(); err != nil {
		return err
	}
	return c.validateRetrievalContextual()
}

// normalizeHierarchical normalizes and validates the retrieval.hierarchical
// block (SPEC §9.7/§16.2) in place. It runs regardless of `enabled` so a
// misspelled level or a negative token bound is CONFIG_INVALID at config time
// rather than lying dormant until the knob is flipped on.
//
// `source_reps` is normalized to nil for the `auto` sentinel (and for an empty
// list) so downstream code has ONE representation of "auto". `levels` defaults
// to [document]; the only accepted values are `document` and `section`.
func (c *Config) normalizeHierarchical() error {
	if strings.TrimSpace(c.RetrievalHierarchicalPromptVersion) == "" {
		c.RetrievalHierarchicalPromptVersion = DefaultHierarchicalPromptVersion
	}
	if c.RetrievalHierarchicalMaxTokens < 0 {
		return fmt.Errorf("retrieval.hierarchical.max_tokens must be non-negative (0 = generator default): %d", c.RetrievalHierarchicalMaxTokens)
	}
	c.RetrievalHierarchicalSourceReps = normalizeHierarchicalSourceReps(c.RetrievalHierarchicalSourceReps)
	levels, err := normalizeHierarchicalLevels(c.RetrievalHierarchicalLevels)
	if err != nil {
		return err
	}
	c.RetrievalHierarchicalLevels = levels
	return nil
}

// normalizeHierarchicalSourceReps lowercases/trims the configured source rep
// types and collapses the `auto` sentinel (or an empty list) to nil, which every
// consumer reads as "the document's primary retrievable text representation"
// (SPEC §16.2). An explicit list keeps its declared order.
func normalizeHierarchicalSourceReps(reps []string) []string {
	out := make([]string, 0, len(reps))
	for _, rep := range reps {
		rep = strings.ToLower(strings.TrimSpace(rep))
		if rep == "" || rep == HierarchicalSourceRepsAuto {
			// Any `auto` entry means auto for the whole document.
			return nil
		}
		out = append(out, rep)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeHierarchicalLevels lowercases/trims the configured summary levels,
// defaults an empty list to [document], and rejects any value outside the §5.2
// enum so a typo fails deterministically at config time.
func normalizeHierarchicalLevels(levels []string) ([]string, error) {
	out := make([]string, 0, len(levels))
	seen := make(map[string]struct{}, len(levels))
	for _, level := range levels {
		level = strings.ToLower(strings.TrimSpace(level))
		if level == "" {
			continue
		}
		if level != HierarchicalLevelDocument && level != HierarchicalLevelSection {
			return nil, fmt.Errorf("retrieval.hierarchical.levels must contain only %q or %q: %q",
				HierarchicalLevelDocument, HierarchicalLevelSection, level)
		}
		if _, dup := seen[level]; dup {
			continue
		}
		seen[level] = struct{}{}
		out = append(out, level)
	}
	if len(out) == 0 {
		return []string{HierarchicalLevelDocument}, nil
	}
	return out, nil
}

// HierarchicalDocumentLevelEnabled reports whether document-level summaries
// should be derived: hierarchical retrieval is on and `document` is among the
// configured levels (SPEC §16.2).
func (c Config) HierarchicalDocumentLevelEnabled() bool {
	if !c.RetrievalHierarchicalEnabled {
		return false
	}
	for _, level := range c.RetrievalHierarchicalLevels {
		if level == HierarchicalLevelDocument {
			return true
		}
	}
	return false
}

// HierarchicalSectionLevelRequested reports whether the operator asked for
// section-level summaries. Section windowing is not implemented yet, so callers
// warn once and derive document-level summaries only — honest coverage rather
// than a silent no-op (SPEC §9.7).
func (c Config) HierarchicalSectionLevelRequested() bool {
	if !c.RetrievalHierarchicalEnabled {
		return false
	}
	for _, level := range c.RetrievalHierarchicalLevels {
		if level == HierarchicalLevelSection {
			return true
		}
	}
	return false
}

// normalizeHyDEMode normalizes retrieval.hyde.mode in place: empty becomes
// "fuse" (the default) and a recognized value is lowercased; any other value is
// CONFIG_INVALID so a typo fails at config time (explicit, deterministic) rather
// than silently behaving like "fuse" at query time. It runs regardless of
// RetrievalHyDEEnabled so a stale/misspelled mode is caught even when HyDE is
// currently off. Extracted to keep validateRetrievalNumericBounds under the
// cyclomatic budget.
func (c *Config) normalizeHyDEMode() error {
	switch mode := strings.ToLower(strings.TrimSpace(c.RetrievalHyDEMode)); mode {
	case "":
		c.RetrievalHyDEMode = HyDEModeFuse
	case HyDEModeFuse, HyDEModeReplace:
		c.RetrievalHyDEMode = mode
	default:
		return fmt.Errorf("retrieval.hyde.mode must be one of %q, %q: %q", HyDEModeFuse, HyDEModeReplace, c.RetrievalHyDEMode)
	}
	return nil
}

// validateAdaptiveBounds enforces the adaptive-gate k window when the gate is
// enabled: negative bounds are meaningless and an inverted window (k_min >
// k_max) would make clamping ill-defined, so both are rejected explicitly
// rather than silently clamped. A bound of 0 is allowed and means "use the
// built-in default" at apply time. When the gate is disabled the bounds are
// ignored, so a stale window never blocks startup.
func (c *Config) validateAdaptiveBounds() error {
	if !c.RetrievalAdaptiveEnabled {
		return nil
	}
	if c.RetrievalAdaptiveKMin < 0 {
		return fmt.Errorf("retrieval.adaptive.k_min must be non-negative: %d", c.RetrievalAdaptiveKMin)
	}
	if c.RetrievalAdaptiveKMax < 0 {
		return fmt.Errorf("retrieval.adaptive.k_max must be non-negative: %d", c.RetrievalAdaptiveKMax)
	}
	if c.RetrievalAdaptiveKMin > 0 && c.RetrievalAdaptiveKMax > 0 && c.RetrievalAdaptiveKMin > c.RetrievalAdaptiveKMax {
		return fmt.Errorf("retrieval.adaptive.k_min (%d) must not exceed retrieval.adaptive.k_max (%d)", c.RetrievalAdaptiveKMin, c.RetrievalAdaptiveKMax)
	}
	return nil
}

// validateSource normalizes Source.Kind (defaulting empty to "local") and
// enforces the corpus-source invariants (issue #244): kind must be one of
// local/nfs/s3; s3 requires a bucket and a resolved access key + secret. The
// errors are CONFIG_INVALID-style (clear, actionable) so enabling s3 without
// credentials fails loudly rather than at first request time.
func (c *Config) validateSource() error {
	kind := strings.ToLower(strings.TrimSpace(c.Source.Kind))
	if kind == "" {
		kind = "local"
	}
	switch kind {
	case "local", "nfs":
		c.Source.Kind = kind
		return nil
	case "s3":
		c.Source.Kind = kind
	default:
		return fmt.Errorf("source.kind must be one of local, nfs, s3: %q", c.Source.Kind)
	}

	// s3-specific requirements.
	c.Source.S3Bucket = strings.TrimSpace(c.Source.S3Bucket)
	c.Source.S3Prefix = strings.TrimSpace(c.Source.S3Prefix)
	c.Source.S3Region = strings.TrimSpace(c.Source.S3Region)
	c.Source.S3Endpoint = strings.TrimSpace(c.Source.S3Endpoint)
	if c.Source.S3Bucket == "" {
		return errors.New("source.kind=s3 requires source.s3.bucket")
	}
	// Credential presence is NOT validated here: AWS credentials are never
	// persisted and are only populated on env-aware load paths. Enforcing them
	// in validateSource would break LoadFile/LoadEffectiveSnapshot for an
	// otherwise-valid persisted s3 config. Credential presence is checked in
	// validateSourceRuntimeSecrets, invoked only when env is applied.
	return nil
}

// validateSourceRuntimeSecrets enforces that an s3 corpus source has resolved
// AWS credentials. It runs only on env-aware load paths (where credentials from
// environment/keychain/.env.local have been layered in), keeping validateSource
// limited to persisted invariants so config-file-only loads do not spuriously
// fail.
func (c *Config) validateSourceRuntimeSecrets() error {
	if strings.ToLower(strings.TrimSpace(c.Source.Kind)) != "s3" {
		return nil
	}
	if strings.TrimSpace(c.Source.S3AccessKeyID) == "" || strings.TrimSpace(c.Source.S3SecretAccessKey) == "" {
		return errors.New("source.kind=s3 requires AWS credentials " +
			"(set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY via environment, keychain, or .env.local)")
	}
	return nil
}

// validateDistributedEmbed enforces the distributed-embedding invariants
// (issue #248, SPEC §8.7). It is a no-op when distributed mode is off (the
// in-process loop runs, §1.2). When enabled it: (1) normalizes/validates the
// broker selector (only the built-in memory|sqlite brokers are constructible in
// this build), and (2) REQUIRES a
// shared external Tier-C vector store (qdrant/pgvector, §8.7.4) — the embedded
// Tier A/B backends are single-node and cannot be shared across workers, so a
// distributed pool over an embedded backend is rejected as CONFIG_INVALID. The
// broker URL credential is never validated here (it is a runtime-only secret
// resolved per §16.1.1, absent on file/snapshot loads).
func (c *Config) validateDistributedEmbed() error {
	if !c.DistributedEmbed.Enabled {
		return nil
	}
	broker := strings.ToLower(strings.TrimSpace(c.DistributedEmbed.Broker))
	if broker == "" {
		broker = "memory"
	}
	switch broker {
	case "memory", "sqlite":
		// built-in, dependency-free brokers (SPEC §8.7.4 default impl)
	default:
		// Stay in sync with buildEmbedBroker (internal/cli), which can only
		// construct the built-in brokers: an external adapter value would validate
		// here but then fail at startup. Reject it up front. External brokers
		// (NATS/Redis/SQS) plug in behind the Broker interface but ship in a
		// follow-up, not this build.
		return fmt.Errorf("distributed_embed.broker=%q is not supported in this build "+
			"(built-in brokers: memory, sqlite); external broker adapters are not yet implemented",
			c.DistributedEmbed.Broker)
	}
	c.DistributedEmbed.Broker = broker
	if c.DistributedEmbed.MaxAttempts < 0 {
		return fmt.Errorf("distributed_embed.max_attempts must be non-negative: %d", c.DistributedEmbed.MaxAttempts)
	}

	// Tier C is a PREREQUISITE of the distributed mode (SPEC §8.7.4): the embedded
	// Tier A/B backends (memory/disk) are single-node and not a shared store.
	backend := strings.ToLower(strings.TrimSpace(c.IndexBackend))
	if backend == "" {
		backend = Default().IndexBackend
	}
	switch backend {
	case "qdrant", "pgvector":
		return nil
	default:
		return fmt.Errorf("distributed_embed.enabled requires an external Tier C vector store "+
			"(index.backend=qdrant or pgvector); the embedded backend %q is single-node and cannot be "+
			"shared across distributed workers (SPEC §8.7.4)", backend)
	}
}

// finalizeLoaded runs the standard post-load validation: Validate (persisted
// invariants, always) plus, when env was applied, the runtime-secret checks
// that depend on resolved credentials.
func (c *Config) finalizeLoaded(applyEnv bool) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if applyEnv {
		if err := c.validateSourceRuntimeSecrets(); err != nil {
			return err
		}
	}
	return nil
}

// applyValidationDefaults rewrites zero SessionInactivityTimeout and
// HealthCheckInterval to their Default() values.
func (c *Config) applyValidationDefaults() {
	if c.SessionInactivityTimeout == 0 {
		// zero is shorthand for the default
		c.SessionInactivityTimeout = Default().SessionInactivityTimeout
	}
	if c.HealthCheckInterval == 0 {
		c.HealthCheckInterval = Default().HealthCheckInterval
	}
}

// ValidateX402 performs consistency checks on the embedded X402Config
// state.  When called it normalizes certain fields (most notably
// `Mode`) and writes the canonical form back into the config struct,
// therefore it must be invoked on a pointer receiver (the method is
// defined on *Config).  The `strict` parameter enables additional
// semantic checks that aren't required in non-strict modes.
//
// The validation is intentionally side‑effecting so that callers may rely
// on `cfg.X402.Mode` being a lowercase, trimmed, non-empty string after a
// successful call.  Errors are returned for invalid values regardless of
// whether mutation has already occurred.
func (c *Config) ValidateX402(strict bool) error {
	// normalize and store back so callers looking at the struct afterwards
	// see a canonical value.  this mirrors the behaviour used elsewhere
	// (eg. x402.NormalizeMode) but keeps the logic self-contained.  we
	// perform the assignment immediately because many of the subsequent
	// branches rely on comparing `mode` and there are early return paths.
	mode := strings.ToLower(strings.TrimSpace(c.X402.Mode))
	if mode == "" {
		mode = "off"
	}
	c.X402.Mode = mode

	switch mode {
	case "off", "on", "required":
	default:
		return fmt.Errorf("invalid x402 mode: %q (accepted: off, on, required)", mode)
	}

	// if tools calls are disabled we only accept mode "off"; any other
	// value implies an inconsistent configuration and should fail. this
	// prevents a situation where Mode="required" but the gating flag is
	// turned off, which would otherwise quietly bypass validation.
	if !c.X402.ToolsCallEnabled {
		if mode != "off" {
			return fmt.Errorf("x402 mode %q requires ToolsCallEnabled=true", mode)
		}
		return nil
	}
	// at this point tools-call is enabled; if the mode is "off" we can
	// short-circuit and skip the remaining checks.
	if mode == "off" {
		return nil
	}

	if normalized, err := normalizeX402URL(c.X402.FacilitatorURL, "facilitator"); err != nil {
		return err
	} else if normalized != "" {
		c.X402.FacilitatorURL = normalized
	}
	if normalized, err := normalizeX402URL(c.X402.ResourceBaseURL, "resource base"); err != nil {
		return err
	} else if normalized != "" {
		c.X402.ResourceBaseURL = normalized
	}

	// Transport security (bs-010 / x402 adapter spec): the adapter->facilitator
	// transport MUST be https whenever it is credentialed OR the facilitator
	// host is non-loopback. Plaintext http is permitted ONLY for a loopback host
	// with no credential attached. This is a hard configuration error in ALL
	// enabled modes (including "on" fail-open) — an insecure credentialed or
	// non-loopback facilitator URL is not a degradable condition, so it is
	// enforced here regardless of the strict flag.
	if err := c.validateX402FacilitatorTransport(); err != nil {
		return err
	}

	// network is validated later when strict mode is enabled; no need to duplicate

	if !strict {
		return nil
	}
	return c.validateX402Strict()
}

// validateX402Strict enforces the strict-mode x402 requirements:
// facilitator/resource URLs, a positive integer price, a valid
// scheme, a CAIP-2 network, and non-empty asset/pay_to. It normalizes
// scheme and network in place.
func (c *Config) validateX402Strict() error {
	if strings.TrimSpace(c.X402.FacilitatorURL) == "" {
		return fmt.Errorf("x402 facilitator URL is required")
	}
	if strings.TrimSpace(c.X402.ResourceBaseURL) == "" {
		return fmt.Errorf("x402 resource base URL is required")
	}
	priceStr := strings.TrimSpace(c.X402.PriceAtomic)
	if priceStr == "" {
		return fmt.Errorf("x402 price is required")
	}
	// ensure price is a positive integer
	price := new(big.Int)
	if _, ok := price.SetString(priceStr, 10); !ok || price.Sign() <= 0 {
		return fmt.Errorf("x402 price must be a positive integer")
	}
	// normalize scheme input by trimming spaces and converting to lower-case
	// write the normalized value back to the struct so later code sees it too
	scheme := strings.ToLower(strings.TrimSpace(c.X402.Scheme))
	c.X402.Scheme = scheme
	if scheme == "" {
		return fmt.Errorf("x402 scheme is required")
	}
	switch scheme {
	case "exact", "upto":
	default:
		return fmt.Errorf("x402 scheme must be one of: exact, upto")
	}
	// ensure network string is trimmed before we validate and store it
	net := strings.TrimSpace(c.X402.Network)
	c.X402.Network = net
	if net == "" {
		return fmt.Errorf("x402 network is required")
	}
	if !isCAIP2Network(net) {
		return fmt.Errorf("x402 network must be CAIP-2")
	}
	if strings.TrimSpace(c.X402.Asset) == "" {
		return fmt.Errorf("x402 asset is required")
	}
	if strings.TrimSpace(c.X402.PayTo) == "" {
		return fmt.Errorf("x402 pay_to is required")
	}
	return nil
}

// normalizeX402URL validates rawURL has a scheme and host and returns
// it with any trailing slash trimmed; an empty input yields "" with no
// error.
func normalizeX402URL(rawURL, label string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("invalid x402 %s URL: %w", label, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid x402 %s URL: %s", label, redactURLForError(parsed, rawURL))
	}
	if parsed.Path == "/" {
		parsed.Path = ""
	} else {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
	}
	return parsed.String(), nil
}

// validateX402FacilitatorTransport enforces the adapter->facilitator transport
// security requirement (bs-010 / x402 adapter spec): https is mandatory whenever
// a credential (facilitator token) is attached OR the facilitator host is
// non-loopback. Plaintext http is permitted only for a loopback host with no
// credential (local development). An empty facilitator URL is left to the
// strict-mode required-field checks and is not treated as a transport error.
func (c *Config) validateX402FacilitatorTransport() error {
	raw := strings.TrimSpace(c.X402.FacilitatorURL)
	if raw == "" {
		return nil
	}
	hasCredential := strings.TrimSpace(c.X402.FacilitatorToken) != ""
	return validateX402TransportSecurity(raw, hasCredential)
}

// X402FacilitatorTransportError returns a non-nil error when the configured
// facilitator URL would carry a credential or reach a non-loopback host over
// plaintext http. It is exposed so the MCP server can fail closed (refuse to
// enable gating) independently of the full x402 validation flow — transport
// insecurity is a hard, non-degradable condition in every mode.
func (c *Config) X402FacilitatorTransportError() error {
	return c.validateX402FacilitatorTransport()
}

// validateX402TransportSecurity is the shared transport-security predicate used
// by config validation. It returns a descriptive configuration error when the
// URL would carry a credential or reach a non-loopback host over plaintext http.
func validateX402TransportSecurity(rawURL string, hasCredential bool) error {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("invalid x402 facilitator URL: %w", err)
	}
	safe := redactURLForError(parsed, rawURL)
	// URL userinfo (http://user:pass@host) is itself a credential that must never
	// traverse plaintext http, so it counts toward hasCredential regardless of a
	// separately-configured facilitator token.
	if parsed.User != nil {
		hasCredential = true
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "https":
		return nil
	case "http":
		if hasCredential {
			return fmt.Errorf("x402 facilitator URL must use https when a credential is attached (plaintext http would leak it): %s", safe)
		}
		if !netutil.IsLoopbackHost(parsed.Hostname()) {
			return fmt.Errorf("x402 facilitator URL must use https for a non-loopback host: %s", safe)
		}
		return nil
	default:
		return fmt.Errorf("x402 facilitator URL must use https (or http for a credential-less loopback host): %s", safe)
	}
}

// redactURLForError returns a log-safe rendering of a facilitator URL: scheme +
// host only, dropping userinfo, path, and query so embedded credentials or
// token-like query params never reach an error string or log line. When the URL
// could not be parsed into a host, it returns a fixed placeholder rather than
// echoing the raw input.
func redactURLForError(parsed *url.URL, rawURL string) string {
	if parsed == nil || parsed.Host == "" {
		return "<redacted>"
	}
	scheme := parsed.Scheme
	if scheme == "" {
		scheme = "?"
	}
	return scheme + "://" + parsed.Host
}

// isCAIP2Network reports whether value is a CAIP-2 "namespace:reference"
// identifier with valid lengths and character classes.
func isCAIP2Network(value string) bool {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 {
		return false
	}
	ns := parts[0]
	ref := parts[1]
	if len(ns) == 0 || len(ns) > 32 || len(ref) == 0 || len(ref) > 128 {
		return false
	}
	return isCAIP2Namespace(ns) && isCAIP2Reference(ref)
}

// isCAIP2Namespace reports whether ns contains only lowercase letters
// and digits.
func isCAIP2Namespace(ns string) bool {
	for _, r := range ns {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// isCAIP2Reference reports whether ref contains only alphanumerics,
// hyphen, or underscore.
func isCAIP2Reference(ref string) bool {
	for _, r := range ref {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

// MergeAllowedOrigins appends comma-separated origins to an existing allowlist,
// preserving first-seen entries and deduplicating with case-insensitive host
// matching.
func MergeAllowedOrigins(existing []string, csv string) []string {
	merged := make([]string, 0, len(existing))
	seen := make(map[string]struct{}, len(existing))

	add := func(origin string) {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			return
		}
		key := normalizeOriginKey(origin)
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		merged = append(merged, origin)
	}

	for _, origin := range existing {
		add(origin)
	}
	for _, origin := range strings.Split(csv, ",") {
		add(origin)
	}
	return merged
}

// normalizeOriginKey returns a canonical dedup key for origin
// (scheme://host[:port] with default ports dropped, or host:port / bare
// host), or "" if origin is invalid.
func normalizeOriginKey(origin string) string {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return ""
	}

	if strings.Contains(origin, "://") {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return ""
		}
		scheme := strings.ToLower(parsed.Scheme)
		host := strings.ToLower(parsed.Hostname())
		port := parsed.Port()
		if port == "" || (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
			return scheme + "://" + host
		}
		return scheme + "://" + host + ":" + port
	}

	if host, port, err := net.SplitHostPort(origin); err == nil {
		return strings.ToLower(host) + ":" + port
	}
	if strings.Contains(origin, "/") || strings.Contains(origin, "\\") || strings.ContainsAny(origin, " \t\r\n") {
		return ""
	}

	return strings.ToLower(origin)
}

// MergeTrustedProxies appends comma-separated trusted proxies to an existing
// list while preserving first-seen, normalized CIDR entries.
func MergeTrustedProxies(existing []string, csv string) []string {
	merged := make([]string, 0, len(existing))
	seen := make(map[string]struct{}, len(existing))

	add := func(value string) {
		key := normalizeTrustedProxyKey(value)
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		merged = append(merged, key)
	}

	for _, value := range existing {
		add(value)
	}
	for _, value := range strings.Split(csv, ",") {
		add(value)
	}
	return merged
}

// normalizeTrustedProxyKey returns the canonical CIDR string for value
// (a CIDR or a single IP widened to /32 or /128), or "" if invalid.
func normalizeTrustedProxyKey(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	if strings.Contains(value, "/") {
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			return ""
		}
		return network.String()
	}

	ip := net.ParseIP(value)
	if ip == nil {
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		return (&net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)}).String()
	}
	return (&net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}).String()
}

// loadKeychainCredentials layers OS keychain credentials into resolution at
// SPEC §16.1.1 precedence #2 (env → keychain → file): for each managed provider
// env var not already set by the real environment, it reads the value from the
// keychain and sets it, so a subsequent .env.local only fills what remains. It
// is fail-open (a missing entry or any backend error is skipped) and is disabled
// entirely when DIR2MCP_DISABLE_KEYCHAIN is set.
func loadKeychainCredentials(overrideEnv map[string]string) {
	if v, ok := envLookup(secrets.DisableEnvVar, overrideEnv); ok && strings.TrimSpace(v) != "" {
		return
	}
	for _, key := range secrets.ManagedEnvVars() {
		if v, ok := envLookup(key, overrideEnv); ok && strings.TrimSpace(v) != "" {
			continue // explicit environment variable wins (precedence #1)
		}
		val, err := secrets.Get(secrets.DefaultService, key)
		if err != nil || strings.TrimSpace(val) == "" {
			continue // absent or backend unavailable → fall through to file/env
		}
		_ = envSet(key, val, overrideEnv)
	}
}

// dotEnvFileNames are the dotenv basenames read in each searched directory,
// in precedence order: .env.local wins over .env.
var dotEnvFileNames = []string{".env.local", ".env"}

// dotEnvSearchPaths returns the dotenv files the loader reads, highest
// precedence first.
//
// The directory that holds the resolved config file comes first, then the
// process working directory. `config init` writes credentials beside the
// selected config file, so a loader that read the working directory only lost
// every credential an operator saved through a custom --config path (#677).
// Both directories are read, so an existing working-directory setup keeps
// working; the config directory wins because --config is the explicit choice.
// Within one directory .env.local still wins over .env.
//
// Paths that resolve to the same file collapse, so the common case (config
// file in the working directory) reads each file once.
func dotEnvSearchPaths(configPath string) []string {
	candidates := make([]string, 0, 2)
	if trimmed := strings.TrimSpace(configPath); trimmed != "" {
		candidates = append(candidates, filepath.Dir(trimmed))
	}
	candidates = append(candidates, ".")

	dirs := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, dir := range candidates {
		key := canonicalDirKey(dir)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		dirs = append(dirs, dir)
	}

	paths := make([]string, 0, len(dirs)*len(dotEnvFileNames))
	for _, dir := range dirs {
		for _, name := range dotEnvFileNames {
			paths = append(paths, filepath.Join(dir, name))
		}
	}
	return paths
}

// canonicalDirKey returns a comparison key for a directory, so "." and an
// absolute path to the same directory collapse to one entry. The path is made
// absolute first, because EvalSymlinks keeps a relative path relative and "."
// would then never match its own absolute form. Symlinks are resolved when
// possible (a macOS /var vs /private/var pair is the same directory), and the
// key falls back to the absolute, then the raw, path when the directory cannot
// be resolved.
func canonicalDirKey(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return filepath.Clean(dir)
	}
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		return resolved
	}
	return filepath.Clean(abs)
}

// dotEnvMultiDirWarning reports when dotenv files were read from more than one
// directory. Two directories in play is the situation an operator cannot see
// otherwise: one file supplies a variable and the other is quietly outranked
// for it (#677). The warning names the files in precedence order and nothing
// else: never a variable name, never a value.
func dotEnvMultiDirWarning(loaded []string) error {
	if len(loaded) < 2 {
		return nil
	}
	dirs := make(map[string]struct{}, len(loaded))
	for _, path := range loaded {
		dirs[filepath.Dir(path)] = struct{}{}
	}
	if len(dirs) < 2 {
		return nil
	}
	return fmt.Errorf(
		"dotenv files loaded from %d directories: %s (in that order, the first file that defines a variable wins; the environment and the keychain still win over all of them)",
		len(dirs), strings.Join(loaded, ", "))
}

// loadDotEnvFiles loads each dotenv path in order, stopping at the
// first error. It returns the paths that existed and were read, in load
// order, so the caller can report which files took part. Paths only: a
// returned value never carries a variable name or a secret value.
func loadDotEnvFiles(paths []string, overrideEnv map[string]string) ([]string, error) {
	loaded := make([]string, 0, len(paths))
	for _, p := range paths {
		read, err := loadDotEnvFile(p, overrideEnv)
		if err != nil {
			return loaded, err
		}
		if read {
			loaded = append(loaded, p)
		}
	}
	return loaded, nil
}

// loadDotEnvFile parses a dotenv file (supporting "export" and
// quoting), setting each key only when not already present and
// non-empty. It reports whether the file existed and was read. A missing
// file is not an error; a file that exists but cannot be read is.
func loadDotEnvFile(path string, overrideEnv map[string]string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer func() {
		_ = file.Close()
	}()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			continue
		}
		existingValue, exists := envLookup(key, overrideEnv)
		if exists && strings.TrimSpace(existingValue) != "" {
			continue
		}
		if setErr := envSet(key, unquoteEnvValue(value), overrideEnv); setErr != nil {
			return true, setErr
		}
	}

	return true, scanner.Err()
}

// envLookup reads key from overrideEnv when non-nil, otherwise from the
// process environment.
func envLookup(key string, overrideEnv map[string]string) (string, bool) {
	if overrideEnv != nil {
		val, ok := overrideEnv[key]
		return val, ok
	}
	return os.LookupEnv(key)
}

// envSet writes key=value into overrideEnv when non-nil, otherwise into
// the process environment.
func envSet(key, value string, overrideEnv map[string]string) error {
	if overrideEnv != nil {
		overrideEnv[key] = value
		return nil
	}
	return os.Setenv(key, value)
}

// unquoteEnvValue strips matching surrounding quotes from a dotenv
// value (interpreting escapes only for double quotes).
func unquoteEnvValue(v string) string {
	if len(v) >= 2 {
		if strings.HasPrefix(v, "\"") && strings.HasSuffix(v, "\"") {
			unquoted, err := strconv.Unquote(v)
			if err != nil {
				return v
			}
			return unquoted
		}
		if strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'") {
			// Single-quoted values are stripped but escape sequences are not processed.
			return v[1 : len(v)-1]
		}
	}
	return v
}
