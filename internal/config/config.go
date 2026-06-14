package config

import (
	"bufio"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/secrets"
)

const DefaultProtocolVersion = "2025-11-25"

const EffectiveConfigSnapshotFile = ".dir2mcp.yaml.snapshot"

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
	ElevenLabsAPIKey      string
	ElevenLabsBaseURL     string
	ElevenLabsTTSVoiceID  string
	// AllowedOrigins is always initialized with local defaults and then extended
	// via env/CLI comma-separated origin lists.
	AllowedOrigins []string

	// Warnings captures non-fatal parsing messages that occurred while
	// loading configuration from environment variables, dotenv files, or
	// the config file.  Callers can inspect and log these as desired.  This
	// field is intentionally not persisted to disk.
	Warnings []error

	RAGSystemPrompt     string
	RAGGenerateAnswer   bool
	RAGKDefault         int
	RAGMaxContextChars  int
	RAGOversampleFactor int
	// RetrievalHybridEnabled controls whether the retrieval service combines
	// BM25 (lexical) and vector (semantic) candidates via reciprocal-rank
	// fusion. When false, search is vector-only (legacy behavior). Default
	// is true for new deployments; existing indexes auto-backfill the FTS
	// table on first start.
	RetrievalHybridEnabled bool
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
	CohereBaseURL         string
	RerankModel           string
	RerankCandidatePool   int
	ChunkingStrategy      string
	ChunkingMaxTokens     int
	ChunkingOverlapTokens int

	IngestGitignore      bool
	IngestFollowSymlinks bool
	IngestMaxFileMB      int
	IngestPDFMode        string
	IngestImagesMode     string
	IngestAudioMode      string
	IngestArchivesMode   string
	IngestExtractor      string

	// IndexBackend selects the vector index backend (issue #246): "memory"
	// (default, the in-memory HNSW) or "disk" (the Tier-B pure-Go on-disk
	// backend that keeps vector payloads memory-mapped on disk for corpora
	// that exceed RAM). The "memory" default is byte-identical to legacy
	// behavior. This is the backend-selection seam future networked backends
	// (#268 Qdrant, #269 pgvector) extend.
	IndexBackend string

	// IngestWatch enables a filesystem watcher so a running server keeps
	// indexing added/changed/deleted files after the initial scan. Opt-in.
	IngestWatch bool
	// IngestWatchDebounce coalesces editor write bursts: a path is processed
	// once it has been quiet for this long.
	IngestWatchDebounce time.Duration

	STTProvider               string
	STTMistralModel           string
	STTElevenLabsModel        string
	STTElevenLabsLanguageCode string

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

	// Source selects the corpus backend (local/nfs/s3). See SourceConfig.
	Source SourceConfig

	// providersDoc holds the parsed `providers:`/`model:` subtree (SPEC
	// 0.7.0 §8.1/§16.2), decoded with yaml.v3 separately from the
	// bespoke flat parser. Unexported/runtime-derived: not persisted,
	// not part of Default()/persistedConfig. Access via cfg.Providers().
	providersDoc providersDoc
}

type fileConfig struct {
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

	IngestDoclingServeURL     *string
	ElevenLabsBaseURL         *string
	ElevenLabsTTSVoiceID      *string
	AllowedOrigins            []string
	RAGSystemPrompt           *string
	RAGGenerateAnswer         *bool
	RAGKDefault               *int
	RAGMaxContextChars        *int
	RAGOversampleFactor       *int
	RetrievalHybridEnabled    *bool
	RerankEnabled             *bool
	RerankProvider            *string
	CohereAPIKey              *string
	CohereBaseURL             *string
	RerankModel               *string
	RerankCandidatePool       *int
	ChunkingStrategy          *string
	ChunkingMaxTokens         *int
	ChunkingOverlapTokens     *int
	IngestGitignore           *bool
	IngestFollowSymlinks      *bool
	IngestMaxFileMB           *int
	IngestPDFMode             *string
	IngestImagesMode          *string
	IngestAudioMode           *string
	IngestArchivesMode        *string
	IngestExtractor           *string
	IndexBackend              *string
	IngestWatch               *bool
	IngestWatchDebounce       *time.Duration
	STTProvider               *string
	STTMistralModel           *string
	STTElevenLabsModel        *string
	STTElevenLabsLanguageCode *string
	QualityGatesEnabled       *bool
	ElevenLabsAPIKey          *string
	ServerTLSCertFile         *string
	ServerTLSKeyFile          *string
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
	// optional session timeouts expressed as YAML duration strings
	SessionInactivityTimeout time.Duration `yaml:"session_inactivity_timeout"`
	SessionMaxLifetime       time.Duration `yaml:"session_max_lifetime"`
	HealthCheckInterval      time.Duration `yaml:"health_check_interval"`

	ElevenLabsBaseURL         string        `yaml:"elevenlabs_base_url"`
	ElevenLabsTTSVoiceID      string        `yaml:"elevenlabs_tts_voice_id"`
	AllowedOrigins            []string      `yaml:"allowed_origins"`
	RAGSystemPrompt           string        `yaml:"rag_system_prompt"`
	RAGGenerateAnswer         bool          `yaml:"rag_generate_answer"`
	RAGKDefault               int           `yaml:"rag_k_default"`
	RAGMaxContextChars        int           `yaml:"rag_max_context_chars"`
	RAGOversampleFactor       int           `yaml:"rag_oversample_factor"`
	RetrievalHybridEnabled    bool          `yaml:"retrieval_hybrid_enabled"`
	RerankEnabled             bool          `yaml:"rerank_enabled"`
	RerankProvider            string        `yaml:"rerank_provider"`
	CohereBaseURL             string        `yaml:"cohere_base_url"`
	RerankModel               string        `yaml:"rerank_model"`
	RerankCandidatePool       int           `yaml:"rerank_candidate_pool"`
	ChunkingStrategy          string        `yaml:"chunking_strategy"`
	ChunkingMaxTokens         int           `yaml:"chunking_max_tokens"`
	ChunkingOverlapTokens     int           `yaml:"chunking_overlap_tokens"`
	IngestGitignore           bool          `yaml:"ingest_gitignore"`
	IngestFollowSymlinks      bool          `yaml:"ingest_follow_symlinks"`
	IngestMaxFileMB           int           `yaml:"ingest_max_file_mb"`
	IngestPDFMode             string        `yaml:"ingest_pdf_mode"`
	IngestImagesMode          string        `yaml:"ingest_images_mode"`
	IngestAudioMode           string        `yaml:"ingest_audio_mode"`
	IngestArchivesMode        string        `yaml:"ingest_archives_mode"`
	IngestExtractor           string        `yaml:"ingest_extractor"`
	IndexBackend              string        `yaml:"index_backend"`
	IngestWatch               bool          `yaml:"ingest_watch"`
	IngestWatchDebounce       time.Duration `yaml:"ingest_watch_debounce"`
	STTProvider               string        `yaml:"stt_provider"`
	STTMistralModel           string        `yaml:"stt_mistral_model"`
	STTElevenLabsModel        string        `yaml:"stt_elevenlabs_model"`
	STTElevenLabsLanguageCode string        `yaml:"stt_elevenlabs_language_code"`
	QualityGatesEnabled       bool          `yaml:"quality_gates_enabled"`
	ServerTLSCertFile         string        `yaml:"server_tls_cert_file"`
	ServerTLSKeyFile          string        `yaml:"server_tls_key_file"`

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
		},
		SecretPatterns: []string{
			`AKIA[0-9A-Z]{16}`,
			`(?i)(?:aws(?:[_\s.]{0,20})?secret(?:[_\s.]*(?:access[_\s.]*)?key)?|secret[_\s.]*access[_\s.]*key)\s*[:=]\s*[0-9A-Za-z/+=]{20,}`,
			`(?i)(?:authorization\s*[:=]\s*bearer\s+|(?:access|id|refresh)_token\s*[:=]\s*)[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`,
			`(?i)token\s*[:=]\s*[A-Za-z0-9_.-]{20,}`,
			`sk_[a-z0-9]{32}|api_[A-Za-z0-9]{32}`,
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
		RAGKDefault:            10,
		RAGMaxContextChars:     20000,
		RAGOversampleFactor:    5,
		RetrievalHybridEnabled: true,
		// RerankEnabled left nil: auto mode (activates iff a rerank
		// provider credential is present). See rerankEnabledEffective.
		RerankProvider:            "cohere",
		CohereAPIKey:              "",
		CohereBaseURL:             "",
		RerankModel:               "rerank-v3.5",
		RerankCandidatePool:       50,
		ChunkingStrategy:          "",
		ChunkingMaxTokens:         0,
		ChunkingOverlapTokens:     0,
		IngestGitignore:           true,
		IngestFollowSymlinks:      false,
		IngestMaxFileMB:           20,
		IngestPDFMode:             "ocr",
		IngestImagesMode:          "ocr_auto",
		IngestAudioMode:           "auto",
		IngestArchivesMode:        "deep",
		IngestExtractor:           "auto",
		IndexBackend:              "memory",
		IngestWatch:               false,
		IngestWatchDebounce:       500 * time.Millisecond,
		STTProvider:               "mistral",
		STTMistralModel:           "voxtral-mini-latest",
		STTElevenLabsModel:        "scribe_v1",
		STTElevenLabsLanguageCode: "",
		QualityGatesEnabled:       true,
		ServerTLSCertFile:         "",
		ServerTLSKeyFile:          "",
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
		RootDir:                   cfg.RootDir,
		StateDir:                  cfg.StateDir,
		ListenAddr:                cfg.ListenAddr,
		MCPPath:                   cfg.MCPPath,
		ProtocolVersion:           cfg.ProtocolVersion,
		Public:                    cfg.Public,
		AuthMode:                  cfg.AuthMode,
		ServerName:                cfg.ServerName,
		RateLimitRPS:              cfg.RateLimitRPS,
		RateLimitBurst:            cfg.RateLimitBurst,
		TrustedProxies:            append([]string(nil), cfg.TrustedProxies...),
		PathExcludes:              append([]string(nil), cfg.PathExcludes...),
		SecretPatterns:            append([]string(nil), cfg.SecretPatterns...),
		DoclingCommand:            cfg.DoclingCommand,
		DoclingServeURL:           cfg.IngestDoclingServeURL,
		SessionInactivityTimeout:  cfg.SessionInactivityTimeout,
		SessionMaxLifetime:        cfg.SessionMaxLifetime,
		HealthCheckInterval:       cfg.HealthCheckInterval,
		ElevenLabsBaseURL:         cfg.ElevenLabsBaseURL,
		ElevenLabsTTSVoiceID:      cfg.ElevenLabsTTSVoiceID,
		AllowedOrigins:            append([]string(nil), cfg.AllowedOrigins...),
		RAGSystemPrompt:           cfg.RAGSystemPrompt,
		RAGGenerateAnswer:         cfg.RAGGenerateAnswer,
		RAGKDefault:               cfg.RAGKDefault,
		RAGMaxContextChars:        cfg.RAGMaxContextChars,
		RAGOversampleFactor:       cfg.RAGOversampleFactor,
		RetrievalHybridEnabled:    cfg.RetrievalHybridEnabled,
		RerankEnabled:             rerankEnabledEffective(cfg),
		RerankProvider:            cfg.RerankProvider,
		CohereBaseURL:             cfg.CohereBaseURL,
		RerankModel:               cfg.RerankModel,
		RerankCandidatePool:       cfg.RerankCandidatePool,
		ChunkingStrategy:          cfg.ChunkingStrategy,
		ChunkingMaxTokens:         cfg.ChunkingMaxTokens,
		ChunkingOverlapTokens:     cfg.ChunkingOverlapTokens,
		IngestGitignore:           cfg.IngestGitignore,
		IngestFollowSymlinks:      cfg.IngestFollowSymlinks,
		IngestMaxFileMB:           cfg.IngestMaxFileMB,
		IngestPDFMode:             cfg.IngestPDFMode,
		IngestImagesMode:          cfg.IngestImagesMode,
		IngestAudioMode:           cfg.IngestAudioMode,
		IngestArchivesMode:        cfg.IngestArchivesMode,
		IngestExtractor:           cfg.IngestExtractor,
		IndexBackend:              cfg.IndexBackend,
		IngestWatch:               cfg.IngestWatch,
		IngestWatchDebounce:       cfg.IngestWatchDebounce,
		STTProvider:               cfg.STTProvider,
		STTMistralModel:           cfg.STTMistralModel,
		STTElevenLabsModel:        cfg.STTElevenLabsModel,
		STTElevenLabsLanguageCode: cfg.STTElevenLabsLanguageCode,
		QualityGatesEnabled:       cfg.QualityGatesEnabled,
		ServerTLSCertFile:         cfg.ServerTLSCertFile,
		ServerTLSKeyFile:          cfg.ServerTLSKeyFile,
		X402Mode:                  cfg.X402.Mode,
		X402FacilitatorURL:        cfg.X402.FacilitatorURL,
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
	raw = appendSnapshotEmbedIdentity(raw, cfg.Providers().EmbedIdentity())
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		raw = append(raw, '\n')
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create snapshot directory: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
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
		loadKeychainCredentials(overrideEnv)
		if err := loadDotEnvFiles([]string{".env.local", ".env"}, overrideEnv); err != nil {
			return Config{}, fmt.Errorf("load dotenv files: %w", err)
		}
	}
	if path == "" {
		if applyEnv {
			applyEnvOverrides(&cfg, overrideEnv)
		}
		if err := cfg.finalizeLoaded(applyEnv); err != nil {
			return Config{}, err
		}
		return cfg, nil
	}

	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if applyEnv {
				applyEnvOverrides(&cfg, overrideEnv)
			}
			if err := cfg.finalizeLoaded(applyEnv); err != nil {
				return Config{}, err
			}
			return cfg, nil
		}
		return Config{}, fmt.Errorf("stat config: %w", err)
	}

	if err := applyFileOverrides(&cfg, path); err != nil {
		return Config{}, err
	}
	if applyEnv {
		applyEnvOverrides(&cfg, overrideEnv)
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

	// Decode the dynamic providers:/model: subtree with yaml.v3
	// (SPEC 0.7.0 §16.2) — independent of the flat parser above.
	doc, err := parseProvidersDoc(raw)
	if err != nil {
		return fmt.Errorf("config file %s: %w", path, err)
	}
	cfg.providersDoc = doc

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

// applyIngestFileParsed overlays parsed chunking, ingest-mode, and STT
// file settings onto cfg.
func applyIngestFileParsed(cfg *Config, fc fileConfig) {
	applyChunkingFileParsed(cfg, fc)
	applyIngestModesFileParsed(cfg, fc)
	applySTTFileParsed(cfg, fc)
}

// applyChunkingFileParsed copies the set chunking file fields onto cfg.
func applyChunkingFileParsed(cfg *Config, fc fileConfig) {
	if fc.ChunkingStrategy != nil {
		cfg.ChunkingStrategy = *fc.ChunkingStrategy
	}
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
	if fc.IndexBackend != nil {
		cfg.IndexBackend = *fc.IndexBackend
	}
	if fc.IngestWatch != nil {
		cfg.IngestWatch = *fc.IngestWatch
	}
	if fc.IngestWatchDebounce != nil {
		cfg.IngestWatchDebounce = *fc.IngestWatchDebounce
	}
}

// applySTTFileParsed copies the set speech-to-text file fields
// (provider and Mistral/ElevenLabs model + language) onto cfg.
func applySTTFileParsed(cfg *Config, fc fileConfig) {
	if fc.STTProvider != nil {
		cfg.STTProvider = *fc.STTProvider
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

		value = unquoteYAMLScalar(value)
		if strings.Contains(value, "\\n") {
			value = strings.ReplaceAll(value, "\\n", "\n")
		}
		if err := setFileScalarValue(&cfg, key, value); err != nil {
			return fileConfig{}, fmt.Errorf("line %d: %w", lineNo, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fileConfig{}, err
	}
	return cfg, nil
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
	"server.listen":                        "listen_addr",
	"server.mcp_path":                      "mcp_path",
	"server.name":                          "server_name",
	"server.protocol_version":              "protocol_version",
	"server.public":                        "public",
	"security.auth.mode":                   "auth_mode",
	"security.allowed_origins":             "allowed_origins",
	"security.path_excludes":               "path_excludes",
	"security.secret_patterns":             "secret_patterns",
	"docling.command":                      "docling_command",
	"ingest.docling.command":               "docling_command",
	"docling.serve_url":                    "docling_serve_url",
	"ingest.docling.serve_url":             "docling_serve_url",
	"stt.elevenlabs.api_key":               "elevenlabs_api_key",
	"secrets.elevenlabs_api_key":           "elevenlabs_api_key",
	"secrets.x402_facilitator_url":         "x402_facilitator_url",
	"rag_generate_answer":                  "rag.generate_answer",
	"generate_answer":                      "rag.generate_answer",
	"rag_k_default":                        "rag.k_default",
	"k_default":                            "rag.k_default",
	"rag_system_prompt":                    "rag.system_prompt",
	"system_prompt":                        "rag.system_prompt",
	"rag_max_context_chars":                "rag.max_context_chars",
	"max_context_chars":                    "rag.max_context_chars",
	"rag_oversample_factor":                "rag.oversample_factor",
	"oversample_factor":                    "rag.oversample_factor",
	"retrieval_hybrid_enabled":             "retrieval.hybrid.enabled",
	"hybrid_enabled":                       "retrieval.hybrid.enabled",
	"rerank_enabled":                       "rerank.enabled",
	"rerank.cohere.api_key":                "cohere_api_key",
	"rerank.cohere.base_url":               "cohere_base_url",
	"rerank.cohere.model":                  "rerank_model",
	"rerank.provider":                      "rerank_provider",
	"rerank.model":                         "rerank_model",
	"rerank_candidate_pool":                "rerank.candidate_pool",
	"chunking_strategy":                    "chunking.strategy",
	"chunking_max_tokens":                  "chunking.max_tokens",
	"chunking_overlap_tokens":              "chunking.overlap_tokens",
	"ingest_gitignore":                     "ingest.gitignore",
	"gitignore":                            "ingest.gitignore",
	"ingest_follow_symlinks":               "ingest.follow_symlinks",
	"follow_symlinks":                      "ingest.follow_symlinks",
	"ingest_max_file_mb":                   "ingest.max_file_mb",
	"max_file_mb":                          "ingest.max_file_mb",
	"ingest_watch":                         "ingest.watch",
	"ingest_watch_debounce":                "ingest.watch_debounce",
	"ingest_pdf_mode":                      "ingest.pdf.mode",
	"pdf_mode":                             "ingest.pdf.mode",
	"ingest_images_mode":                   "ingest.images.mode",
	"images_mode":                          "ingest.images.mode",
	"ingest_audio_mode":                    "ingest.audio.mode",
	"audio_mode":                           "ingest.audio.mode",
	"ingest_archives_mode":                 "ingest.archives.mode",
	"archives_mode":                        "ingest.archives.mode",
	"ingest_extractor":                     "ingest.extractor",
	"extractor":                            "ingest.extractor",
	"index_backend":                        "index.backend",
	"backend":                              "index.backend",
	"stt_provider":                         "stt.provider",
	"stt_mistral_model":                    "stt.mistral.model",
	"stt_elevenlabs_model":                 "stt.elevenlabs.model",
	"stt_elevenlabs_language_code":         "stt.elevenlabs.language_code",
	"elevenlabs_language_code":             "stt.elevenlabs.language_code",
	"server_tls_cert_file":                 "server.tls.cert_file",
	"tls_cert_file":                        "server.tls.cert_file",
	"cert_file":                            "server.tls.cert_file",
	"server.tls.cert":                      "server.tls.cert_file",
	"server_tls_key_file":                  "server.tls.key_file",
	"tls_key_file":                         "server.tls.key_file",
	"key_file":                             "server.tls.key_file",
	"server.tls.key":                       "server.tls.key_file",
	"x402.mode":                            "x402_mode",
	"x402.facilitator_url":                 "x402_facilitator_url",
	"x402.resource_base_url":               "x402_resource_base_url",
	"x402.facilitator_token":               "x402_facilitator_token",
	"x402.route_policy.tools_call.enabled": "x402_tools_call_enabled",
	"x402.route_policy.tools_call.price":   "x402_price_atomic",
	"x402.route_policy.tools_call.network": "x402_network",
	"x402.route_policy.tools_call.scheme":  "x402_scheme",
	"x402.route_policy.tools_call.asset":   "x402_asset",
	"x402.route_policy.tools_call.pay_to":  "x402_pay_to",
	"source.kind":                          "source_kind",
	"source.s3.bucket":                     "source_s3_bucket",
	"source.s3.prefix":                     "source_s3_prefix",
	"source.s3.region":                     "source_s3_region",
	"source.s3.endpoint":                   "source_s3_endpoint",
	"index.qdrant.url":                     "qdrant_url",
	"index.qdrant.collection":              "qdrant_collection",
	"index.qdrant.api_key":                 "qdrant_api_key",
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
	switch key {
	case "rag", "ingest", "ingest.docling", "stt", "stt.mistral", "stt.elevenlabs", "server", "server.tls", "secret_sources", "mistral", "docling", "security", "security.auth", "x402", "x402.route_policy", "x402.route_policy.tools_call", "chunking", "retrieval", "retrieval.hybrid", "rerank", "rerank.cohere", "index":
		return true
	case "ingest.pdf", "ingest.images", "ingest.audio", "ingest.archives", "secrets", "index.qdrant":
		return true
	case "source", "source.s3":
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
	if err := setDurationFileScalar(cfg, key, value); err != nil {
		return err
	}
	setStringFileScalar(cfg, key, value)
	return nil
}

// setBoolFileScalar parses value as a bool and assigns it to the
// fileConfig field selected by key; unknown keys are a no-op and an
// unparseable value is an error.
func setBoolFileScalar(cfg *fileConfig, key, value string) error {
	var target **bool
	switch key {
	case "public":
		target = &cfg.Public
	case "rag.generate_answer":
		target = &cfg.RAGGenerateAnswer
	case "ingest.gitignore":
		target = &cfg.IngestGitignore
	case "ingest.follow_symlinks":
		target = &cfg.IngestFollowSymlinks
	case "ingest.watch":
		target = &cfg.IngestWatch
	case "quality_gates_enabled":
		target = &cfg.QualityGatesEnabled
	case "x402_tools_call_enabled":
		target = &cfg.X402ToolsCallEnabled
	case "retrieval.hybrid.enabled":
		target = &cfg.RetrievalHybridEnabled
	case "rerank.enabled":
		target = &cfg.RerankEnabled
	default:
		return nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fmt.Errorf("invalid boolean for %s", key)
	}
	*target = boolPtr(parsed)
	return nil
}

// setIntFileScalar parses value as an int and assigns it to the
// fileConfig field selected by key; returns an error for an unknown key
// or a non-integer value.
func setIntFileScalar(cfg *fileConfig, key, value string) error {
	var target **int
	switch key {
	case "rate_limit_rps":
		target = &cfg.RateLimitRPS
	case "rate_limit_burst":
		target = &cfg.RateLimitBurst
	case "rag.k_default":
		target = &cfg.RAGKDefault
	case "rag.max_context_chars":
		target = &cfg.RAGMaxContextChars
	case "rag.oversample_factor":
		target = &cfg.RAGOversampleFactor
	case "chunking.max_tokens":
		target = &cfg.ChunkingMaxTokens
	case "chunking.overlap_tokens":
		target = &cfg.ChunkingOverlapTokens
	case "ingest.max_file_mb":
		target = &cfg.IngestMaxFileMB
	case "rerank.candidate_pool":
		target = &cfg.RerankCandidatePool
	default:
		return nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("invalid integer for %s", key)
	}
	*target = intPtr(parsed)
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
	case "rag.system_prompt":
		cfg.RAGSystemPrompt = strPtr(value)
	case "chunking.strategy":
		cfg.ChunkingStrategy = strPtr(value)
	}
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
	case "index.backend":
		cfg.IndexBackend = strPtr(value)
	case "stt.provider":
		cfg.STTProvider = strPtr(value)
	case "stt.mistral.model":
		cfg.STTMistralModel = strPtr(value)
	case "stt.elevenlabs.model":
		cfg.STTElevenLabsModel = strPtr(value)
	case "stt.elevenlabs.language_code":
		cfg.STTElevenLabsLanguageCode = strPtr(value)
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
func setFileListValue(cfg *fileConfig, key, value string) {
	key = canonicalizeConfigKey(key)
	appendValue := func(target *[]string, item string) {
		if *target == nil {
			*target = []string{}
		}
		if strings.TrimSpace(item) == "" {
			return
		}
		*target = append(*target, item)
	}

	switch key {
	case "trusted_proxies":
		appendValue(&cfg.TrustedProxies, value)
	case "path_excludes":
		appendValue(&cfg.PathExcludes, value)
	case "secret_patterns":
		appendValue(&cfg.SecretPatterns, value)
	case "allowed_origins":
		appendValue(&cfg.AllowedOrigins, value)
	}
}

// isListConfigKey reports whether key (after canonicalization) maps to
// a list-valued config field.
func isListConfigKey(key string) bool {
	key = canonicalizeConfigKey(key)
	switch key {
	case "trusted_proxies", "path_excludes", "secret_patterns", "allowed_origins":
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
	writeBool("rerank_enabled", cfg.RerankEnabled)
	writeScalar("rerank_provider", cfg.RerankProvider)
	writeScalar("cohere_base_url", cfg.CohereBaseURL)
	writeScalar("rerank_model", cfg.RerankModel)
	writeInt("rerank_candidate_pool", cfg.RerankCandidatePool)
	writeScalar("chunking_strategy", cfg.ChunkingStrategy)
	writeInt("chunking_max_tokens", cfg.ChunkingMaxTokens)
	writeInt("chunking_overlap_tokens", cfg.ChunkingOverlapTokens)
	writeBool("ingest_gitignore", cfg.IngestGitignore)
	writeBool("ingest_follow_symlinks", cfg.IngestFollowSymlinks)
	writeInt("ingest_max_file_mb", cfg.IngestMaxFileMB)
	writeScalar("ingest_pdf_mode", cfg.IngestPDFMode)
	writeScalar("ingest_images_mode", cfg.IngestImagesMode)
	writeScalar("ingest_audio_mode", cfg.IngestAudioMode)
	writeScalar("ingest_archives_mode", cfg.IngestArchivesMode)
	writeScalar("ingest_extractor", cfg.IngestExtractor)
	writeScalar("index_backend", cfg.IndexBackend)
	writeBool("ingest_watch", cfg.IngestWatch)
	writeScalar("ingest_watch_debounce", cfg.IngestWatchDebounce.String())
	writeScalar("stt_provider", cfg.STTProvider)
	writeScalar("stt_mistral_model", cfg.STTMistralModel)
	writeScalar("stt_elevenlabs_model", cfg.STTElevenLabsModel)
	writeScalar("stt_elevenlabs_language_code", cfg.STTElevenLabsLanguageCode)
	writeBool("quality_gates_enabled", cfg.QualityGatesEnabled)
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

// intPtr returns a pointer to value.
func intPtr(value int) *int { return &value }

// applyEnvOverrides layers all supported environment-variable overrides
// onto cfg. Env always wins when present (an empty DIR2MCP_SERVER_NAME
// clears a YAML-set name).
func applyEnvOverrides(cfg *Config, overrideEnv map[string]string) {
	if cfg == nil {
		return
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
		{"DIR2MCP_INGEST_EXTRACTOR", &cfg.IngestExtractor},
		{"DIR2MCP_INDEX_BACKEND", &cfg.IndexBackend},
	} {
		if raw, ok := envLookup(o.key, env); ok && strings.TrimSpace(raw) != "" {
			*o.field = strings.TrimSpace(raw)
		}
	}
	if raw, ok := envLookup("DIR2MCP_INGEST_WATCH", env); ok && strings.TrimSpace(raw) != "" {
		if parsed, err := strconv.ParseBool(strings.TrimSpace(raw)); err == nil {
			cfg.IngestWatch = parsed
		}
	}
	if raw, ok := envLookup("DIR2MCP_INGEST_WATCH_DEBOUNCE", env); ok && strings.TrimSpace(raw) != "" {
		applyDurationEnvField(cfg, raw, "DIR2MCP_INGEST_WATCH_DEBOUNCE", &cfg.IngestWatchDebounce)
	}
	if raw, ok := envLookup("DIR2MCP_RETRIEVAL_HYBRID_ENABLED", env); ok && strings.TrimSpace(raw) != "" {
		if parsed, err := strconv.ParseBool(strings.TrimSpace(raw)); err == nil {
			cfg.RetrievalHybridEnabled = parsed
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
	if err := c.validateIngestExtractor(); err != nil {
		return err
	}

	if err := c.validateIndexBackend(); err != nil {
		return err
	}

	if err := c.validateNumericBounds(); err != nil {
		return err
	}
	if err := c.validateSource(); err != nil {
		return err
	}
	c.applyValidationDefaults()
	if c.SessionMaxLifetime > 0 && c.SessionMaxLifetime < c.SessionInactivityTimeout {
		return fmt.Errorf("session_max_lifetime (%v) must be >= session_inactivity_timeout (%v)",
			c.SessionMaxLifetime, c.SessionInactivityTimeout)
	}
	return nil
}

// validateIngestExtractor normalizes IngestExtractor (defaulting empty)
// and rejects any value outside auto/docling/docling-serve/mistral/off.
func (c *Config) validateIngestExtractor() error {
	extractorMode := strings.ToLower(strings.TrimSpace(c.IngestExtractor))
	if extractorMode == "" {
		extractorMode = Default().IngestExtractor
	}
	switch extractorMode {
	case "auto", "docling", "docling-serve", "mistral", "off":
	default:
		return fmt.Errorf("ingest.extractor must be one of auto, docling, docling-serve, mistral, off: %q", c.IngestExtractor)
	}
	c.IngestExtractor = extractorMode
	return nil
}

// validateIndexBackend normalizes IndexBackend (defaulting empty to the
// "memory" baseline) and rejects any value outside memory/disk/qdrant
// (issues #246, #268). When qdrant is selected it enforces the qdrant
// persisted invariants (url required). Future networked backends (#269
// pgvector) extend this set.
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
	default:
		return fmt.Errorf("index.backend must be one of memory, disk, qdrant: %q", c.IndexBackend)
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
	if c.RAGMaxContextChars < 0 {
		return fmt.Errorf("rag.max_context_chars must be non-negative: %d", c.RAGMaxContextChars)
	}
	if c.RAGKDefault < 0 {
		return fmt.Errorf("rag.k_default must be non-negative: %d", c.RAGKDefault)
	}
	if c.RAGOversampleFactor < 0 {
		return fmt.Errorf("rag.oversample_factor must be non-negative: %d", c.RAGOversampleFactor)
	}
	if c.ChunkingMaxTokens < 0 {
		return fmt.Errorf("chunking.max_tokens must be non-negative: %d", c.ChunkingMaxTokens)
	}
	if c.ChunkingOverlapTokens < 0 {
		return fmt.Errorf("chunking.overlap_tokens must be non-negative: %d", c.ChunkingOverlapTokens)
	}
	if c.IngestMaxFileMB < 0 {
		return fmt.Errorf("ingest.max_file_mb must be non-negative: %d", c.IngestMaxFileMB)
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
		return "", fmt.Errorf("invalid x402 %s URL %q: %w", label, rawURL, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid x402 %s URL: %q", label, rawURL)
	}
	if parsed.Path == "/" {
		parsed.Path = ""
	} else {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
	}
	return parsed.String(), nil
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

// loadDotEnvFiles loads each dotenv path in order, stopping at the
// first error.
func loadDotEnvFiles(paths []string, overrideEnv map[string]string) error {
	for _, p := range paths {
		if err := loadDotEnvFile(p, overrideEnv); err != nil {
			return err
		}
	}
	return nil
}

// loadDotEnvFile parses a dotenv file (supporting "export" and
// quoting), setting each key only when not already present and
// non-empty. A missing file is not an error.
func loadDotEnvFile(path string, overrideEnv map[string]string) error {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
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
			return setErr
		}
	}

	return scanner.Err()
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
