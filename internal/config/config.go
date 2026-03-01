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

	"dir2mcp/internal/mistral"
)

const DefaultProtocolVersion = "2025-11-25"

const EffectiveConfigSnapshotFile = ".dir2mcp.yaml.snapshot"

type SecretSourceMetadata struct {
	MistralAPIKey        string
	ElevenLabsAPIKey     string
	X402FacilitatorToken string
	AuthToken            string
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
	ResolvedAuthToken    string
	MistralAPIKey        string
	MistralBaseURL       string
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

	// EmbedModelText and EmbedModelCode specify the names of the Mistral
	// embedding models used for text and code chunks respectively.  They are
	// exposed via configuration so operators can override the hardcoded
	// defaults if the upstream API changes or custom models are desired.
	EmbedModelText string
	EmbedModelCode string
	// ChatModel specifies the Mistral chat/completion model used for
	// RAG-style generation.  Operators can override the hardcoded default
	// when upstream introduces a new alias or model.  Environment variable
	// DIR2MCP_CHAT_MODEL also affects this value.
	ChatModel string

	RAGSystemPrompt       string
	RAGGenerateAnswer     bool
	RAGKDefault           int
	RAGMaxContextChars    int
	RAGOversampleFactor   int
	ChunkingStrategy      string
	ChunkingMaxTokens     int
	ChunkingOverlapTokens int

	IngestGitignore      bool
	IngestFollowSymlinks bool
	IngestMaxFileMB      int

	STTProvider               string
	STTMistralModel           string
	STTElevenLabsModel        string
	STTElevenLabsLanguageCode string

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
}

type fileConfig struct {
	RootDir         *string
	StateDir        *string
	ListenAddr      *string
	MCPPath         *string
	ProtocolVersion *string
	Public          *bool
	AuthMode        *string
	RateLimitRPS    *int
	RateLimitBurst  *int
	TrustedProxies  []string
	PathExcludes    []string
	SecretPatterns  []string
	MistralBaseURL  *string

	ElevenLabsBaseURL         *string
	ElevenLabsTTSVoiceID      *string
	AllowedOrigins            []string
	EmbedModelText            *string
	EmbedModelCode            *string
	ChatModel                 *string
	RAGSystemPrompt           *string
	RAGGenerateAnswer         *bool
	RAGKDefault               *int
	RAGMaxContextChars        *int
	RAGOversampleFactor       *int
	ChunkingStrategy          *string
	ChunkingMaxTokens         *int
	ChunkingOverlapTokens     *int
	IngestGitignore           *bool
	IngestFollowSymlinks      *bool
	IngestMaxFileMB           *int
	STTProvider               *string
	STTMistralModel           *string
	STTElevenLabsModel        *string
	STTElevenLabsLanguageCode *string
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
}

type persistedConfig struct {
	RootDir         string   `yaml:"root_dir"`
	StateDir        string   `yaml:"state_dir"`
	ListenAddr      string   `yaml:"listen_addr"`
	MCPPath         string   `yaml:"mcp_path"`
	ProtocolVersion string   `yaml:"protocol_version"`
	Public          bool     `yaml:"public"`
	AuthMode        string   `yaml:"auth_mode"`
	RateLimitRPS    int      `yaml:"rate_limit_rps"`
	RateLimitBurst  int      `yaml:"rate_limit_burst"`
	TrustedProxies  []string `yaml:"trusted_proxies"`
	PathExcludes    []string `yaml:"path_excludes"`
	SecretPatterns  []string `yaml:"secret_patterns"`
	MistralBaseURL  string   `yaml:"mistral_base_url"`
	// optional session timeouts expressed as YAML duration strings
	SessionInactivityTimeout time.Duration `yaml:"session_inactivity_timeout"`
	SessionMaxLifetime       time.Duration `yaml:"session_max_lifetime"`
	HealthCheckInterval      time.Duration `yaml:"health_check_interval"`

	ElevenLabsBaseURL         string   `yaml:"elevenlabs_base_url"`
	ElevenLabsTTSVoiceID      string   `yaml:"elevenlabs_tts_voice_id"`
	AllowedOrigins            []string `yaml:"allowed_origins"`
	EmbedModelText            string   `yaml:"embed_model_text"`
	EmbedModelCode            string   `yaml:"embed_model_code"`
	ChatModel                 string   `yaml:"chat_model"`
	RAGSystemPrompt           string   `yaml:"rag_system_prompt"`
	RAGGenerateAnswer         bool     `yaml:"rag_generate_answer"`
	RAGKDefault               int      `yaml:"rag_k_default"`
	RAGMaxContextChars        int      `yaml:"rag_max_context_chars"`
	RAGOversampleFactor       int      `yaml:"rag_oversample_factor"`
	ChunkingStrategy          string   `yaml:"chunking_strategy"`
	ChunkingMaxTokens         int      `yaml:"chunking_max_tokens"`
	ChunkingOverlapTokens     int      `yaml:"chunking_overlap_tokens"`
	IngestGitignore           bool     `yaml:"ingest_gitignore"`
	IngestFollowSymlinks      bool     `yaml:"ingest_follow_symlinks"`
	IngestMaxFileMB           int      `yaml:"ingest_max_file_mb"`
	STTProvider               string   `yaml:"stt_provider"`
	STTMistralModel           string   `yaml:"stt_mistral_model"`
	STTElevenLabsModel        string   `yaml:"stt_elevenlabs_model"`
	STTElevenLabsLanguageCode string   `yaml:"stt_elevenlabs_language_code"`
	ServerTLSCertFile         string   `yaml:"server_tls_cert_file"`
	ServerTLSKeyFile          string   `yaml:"server_tls_key_file"`

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
}

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
			`(?i)aws(.{0,20})?secret|([0-9a-zA-Z/+=]{40})`,
			`(?i)(?:authorization\s*[:=]\s*bearer\s+|(?:access|id|refresh)_token\s*[:=]\s*)[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`,
			`(?i)token\s*[:=]\s*[A-Za-z0-9_.-]{20,}`,
			`sk_[a-z0-9]{32}|api_[A-Za-z0-9]{32}`,
		},
		MistralAPIKey:        "",
		MistralBaseURL:       "",
		ElevenLabsAPIKey:     "",
		ElevenLabsBaseURL:    "",
		ElevenLabsTTSVoiceID: "JBFqnCBsd6RMkjVDRZzb",
		AllowedOrigins: []string{
			"http://localhost",
			"http://127.0.0.1",
		},
		// default embedding models
		EmbedModelText:            "mistral-embed",
		EmbedModelCode:            "codestral-embed",
		ChatModel:                 mistral.DefaultChatModel,
		RAGSystemPrompt:           "",
		RAGGenerateAnswer:         true,
		RAGKDefault:               10,
		RAGMaxContextChars:        20000,
		RAGOversampleFactor:       5,
		ChunkingStrategy:          "",
		ChunkingMaxTokens:         0,
		ChunkingOverlapTokens:     0,
		IngestGitignore:           true,
		IngestFollowSymlinks:      false,
		IngestMaxFileMB:           20,
		STTProvider:               "mistral",
		STTMistralModel:           "voxtral-mini-latest",
		STTElevenLabsModel:        "scribe",
		STTElevenLabsLanguageCode: "",
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
	}
}

func Load(path string) (Config, error) {
	return load(path, nil, true)
}

// LoadFile loads defaults plus an optional YAML config file and does not
// apply dotenv/env overrides. This is useful for config init/update flows.
func LoadFile(path string) (Config, error) {
	return load(path, nil, false)
}

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

	serializable := persistedConfig{
		RootDir:                   cfg.RootDir,
		StateDir:                  cfg.StateDir,
		ListenAddr:                cfg.ListenAddr,
		MCPPath:                   cfg.MCPPath,
		ProtocolVersion:           cfg.ProtocolVersion,
		Public:                    cfg.Public,
		AuthMode:                  cfg.AuthMode,
		RateLimitRPS:              cfg.RateLimitRPS,
		RateLimitBurst:            cfg.RateLimitBurst,
		TrustedProxies:            append([]string(nil), cfg.TrustedProxies...),
		PathExcludes:              append([]string(nil), cfg.PathExcludes...),
		SecretPatterns:            append([]string(nil), cfg.SecretPatterns...),
		MistralBaseURL:            cfg.MistralBaseURL,
		ElevenLabsBaseURL:         cfg.ElevenLabsBaseURL,
		ElevenLabsTTSVoiceID:      cfg.ElevenLabsTTSVoiceID,
		AllowedOrigins:            append([]string(nil), cfg.AllowedOrigins...),
		EmbedModelText:            cfg.EmbedModelText,
		EmbedModelCode:            cfg.EmbedModelCode,
		ChatModel:                 cfg.ChatModel,
		RAGSystemPrompt:           cfg.RAGSystemPrompt,
		RAGGenerateAnswer:         cfg.RAGGenerateAnswer,
		RAGKDefault:               cfg.RAGKDefault,
		RAGMaxContextChars:        cfg.RAGMaxContextChars,
		RAGOversampleFactor:       cfg.RAGOversampleFactor,
		ChunkingStrategy:          cfg.ChunkingStrategy,
		ChunkingMaxTokens:         cfg.ChunkingMaxTokens,
		ChunkingOverlapTokens:     cfg.ChunkingOverlapTokens,
		IngestGitignore:           cfg.IngestGitignore,
		IngestFollowSymlinks:      cfg.IngestFollowSymlinks,
		IngestMaxFileMB:           cfg.IngestMaxFileMB,
		STTProvider:               cfg.STTProvider,
		STTMistralModel:           cfg.STTMistralModel,
		STTElevenLabsModel:        cfg.STTElevenLabsModel,
		STTElevenLabsLanguageCode: cfg.STTElevenLabsLanguageCode,
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
		// session settings
		SessionInactivityTimeout: cfg.SessionInactivityTimeout,
		SessionMaxLifetime:       cfg.SessionMaxLifetime,
		HealthCheckInterval:      cfg.HealthCheckInterval,
	}

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

func EffectiveSnapshotPath(stateDir string) string {
	trimmed := strings.TrimSpace(stateDir)
	if trimmed == "" {
		trimmed = Default().StateDir
	}
	return filepath.Join(trimmed, EffectiveConfigSnapshotFile)
}

func SaveEffectiveSnapshot(cfg Config, sources SecretSourceMetadata) (string, error) {
	if err := cfg.Validate(); err != nil {
		return "", fmt.Errorf("validate config: %w", err)
	}

	path := EffectiveSnapshotPath(cfg.StateDir)
	serializable := persistedConfig{
		RootDir:                   cfg.RootDir,
		StateDir:                  cfg.StateDir,
		ListenAddr:                cfg.ListenAddr,
		MCPPath:                   cfg.MCPPath,
		ProtocolVersion:           cfg.ProtocolVersion,
		Public:                    cfg.Public,
		AuthMode:                  cfg.AuthMode,
		RateLimitRPS:              cfg.RateLimitRPS,
		RateLimitBurst:            cfg.RateLimitBurst,
		TrustedProxies:            append([]string(nil), cfg.TrustedProxies...),
		PathExcludes:              append([]string(nil), cfg.PathExcludes...),
		SecretPatterns:            append([]string(nil), cfg.SecretPatterns...),
		MistralBaseURL:            cfg.MistralBaseURL,
		SessionInactivityTimeout:  cfg.SessionInactivityTimeout,
		SessionMaxLifetime:        cfg.SessionMaxLifetime,
		HealthCheckInterval:       cfg.HealthCheckInterval,
		ElevenLabsBaseURL:         cfg.ElevenLabsBaseURL,
		ElevenLabsTTSVoiceID:      cfg.ElevenLabsTTSVoiceID,
		AllowedOrigins:            append([]string(nil), cfg.AllowedOrigins...),
		EmbedModelText:            cfg.EmbedModelText,
		EmbedModelCode:            cfg.EmbedModelCode,
		ChatModel:                 cfg.ChatModel,
		RAGSystemPrompt:           cfg.RAGSystemPrompt,
		RAGGenerateAnswer:         cfg.RAGGenerateAnswer,
		RAGKDefault:               cfg.RAGKDefault,
		RAGMaxContextChars:        cfg.RAGMaxContextChars,
		RAGOversampleFactor:       cfg.RAGOversampleFactor,
		ChunkingStrategy:          cfg.ChunkingStrategy,
		ChunkingMaxTokens:         cfg.ChunkingMaxTokens,
		ChunkingOverlapTokens:     cfg.ChunkingOverlapTokens,
		IngestGitignore:           cfg.IngestGitignore,
		IngestFollowSymlinks:      cfg.IngestFollowSymlinks,
		IngestMaxFileMB:           cfg.IngestMaxFileMB,
		STTProvider:               cfg.STTProvider,
		STTMistralModel:           cfg.STTMistralModel,
		STTElevenLabsModel:        cfg.STTElevenLabsModel,
		STTElevenLabsLanguageCode: cfg.STTElevenLabsLanguageCode,
		ServerTLSCertFile:         cfg.ServerTLSCertFile,
		ServerTLSKeyFile:          cfg.ServerTLSKeyFile,
		X402Mode:                  cfg.X402.Mode,
		X402FacilitatorURL:        cfg.X402.FacilitatorURL,
		X402ResourceBaseURL:       cfg.X402.ResourceBaseURL,
		X402ToolsCallEnabled:      cfg.X402.ToolsCallEnabled,
		X402PriceAtomic:           cfg.X402.PriceAtomic,
		X402Network:               cfg.X402.Network,
		X402Scheme:                cfg.X402.Scheme,
		X402Asset:                 cfg.X402.Asset,
		X402PayTo:                 cfg.X402.PayTo,
	}

	raw, err := marshalConfigYAML(serializable)
	if err != nil {
		return "", fmt.Errorf("marshal snapshot yaml: %w", err)
	}
	raw = appendSnapshotSecretSourceMetadata(raw, sources)
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
	sources := parseSecretSourceMetadata(raw)

	cfg := Default()
	applyParsedFileOverrides(&cfg, fileCfg)
	if err := cfg.Validate(); err != nil {
		return Config{}, SecretSourceMetadata{}, err
	}
	return cfg, sources, nil
}

func appendSnapshotSecretSourceMetadata(raw []byte, sources SecretSourceMetadata) []byte {
	entries := []struct {
		key   string
		value string
	}{
		{key: "mistral_api_key", value: strings.TrimSpace(sources.MistralAPIKey)},
		{key: "elevenlabs_api_key", value: strings.TrimSpace(sources.ElevenLabsAPIKey)},
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

func parseSecretSourceMetadata(raw []byte) SecretSourceMetadata {
	meta := SecretSourceMetadata{}
	reader := strings.NewReader(string(raw))
	scanner := bufio.NewScanner(reader)
	sectionByIndent := map[int]string{}

	for scanner.Scan() {
		rawLine := scanner.Text()
		indent := len(rawLine) - len(strings.TrimLeft(rawLine, " "))
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "- ") {
			continue
		}
		for level := range sectionByIndent {
			if level >= indent {
				delete(sectionByIndent, level)
			}
		}
		prefix := nearestSectionPrefix(sectionByIndent, indent)
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if prefix != "" && !strings.Contains(key, ".") {
			key = prefix + "." + key
		}
		if value == "" && isMapSectionKey(key) {
			sectionByIndent[indent] = key
			continue
		}

		value = unquoteYAMLScalar(value)
		switch key {
		case "secret_sources.mistral_api_key":
			meta.MistralAPIKey = value
		case "secret_sources.elevenlabs_api_key":
			meta.ElevenLabsAPIKey = value
		case "secret_sources.x402_facilitator_token":
			meta.X402FacilitatorToken = value
		case "secret_sources.auth_token":
			meta.AuthToken = value
		}
	}
	return meta
}

func load(path string, overrideEnv map[string]string, applyEnv bool) (Config, error) {
	// Start from defaults, then layer dotenv/env overrides.
	cfg := Default()
	if applyEnv {
		if err := loadDotEnvFiles([]string{".env.local", ".env"}, overrideEnv); err != nil {
			return Config{}, fmt.Errorf("load dotenv files: %w", err)
		}
	}
	if path == "" {
		if applyEnv {
			applyEnvOverrides(&cfg, overrideEnv)
		}
		if err := cfg.Validate(); err != nil {
			return Config{}, err
		}
		return cfg, nil
	}

	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if applyEnv {
				applyEnvOverrides(&cfg, overrideEnv)
			}
			if err := cfg.Validate(); err != nil {
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
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

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

	return nil
}

func applyParsedFileOverrides(cfg *Config, fileCfg fileConfig) {
	if fileCfg.RootDir != nil {
		cfg.RootDir = *fileCfg.RootDir
	}
	if fileCfg.StateDir != nil {
		cfg.StateDir = *fileCfg.StateDir
	}
	if fileCfg.ListenAddr != nil {
		cfg.ListenAddr = *fileCfg.ListenAddr
	}
	if fileCfg.MCPPath != nil {
		cfg.MCPPath = *fileCfg.MCPPath
	}
	if fileCfg.ProtocolVersion != nil {
		cfg.ProtocolVersion = *fileCfg.ProtocolVersion
	}
	if fileCfg.Public != nil {
		cfg.Public = *fileCfg.Public
	}
	if fileCfg.AuthMode != nil {
		cfg.AuthMode = *fileCfg.AuthMode
	}
	if fileCfg.RateLimitRPS != nil {
		cfg.RateLimitRPS = *fileCfg.RateLimitRPS
	}
	if fileCfg.RateLimitBurst != nil {
		cfg.RateLimitBurst = *fileCfg.RateLimitBurst
	}
	if fileCfg.TrustedProxies != nil {
		cfg.TrustedProxies = normalizeStringSlice(fileCfg.TrustedProxies)
	}
	if fileCfg.PathExcludes != nil {
		cfg.PathExcludes = normalizeStringSlice(fileCfg.PathExcludes)
	}
	if fileCfg.SecretPatterns != nil {
		cfg.SecretPatterns = normalizeStringSlice(fileCfg.SecretPatterns)
	}
	if fileCfg.MistralBaseURL != nil {
		cfg.MistralBaseURL = *fileCfg.MistralBaseURL
	}
	if fileCfg.SessionInactivityTimeout != nil {
		cfg.SessionInactivityTimeout = *fileCfg.SessionInactivityTimeout
	}
	if fileCfg.SessionMaxLifetime != nil {
		cfg.SessionMaxLifetime = *fileCfg.SessionMaxLifetime
	}
	if fileCfg.HealthCheckInterval != nil {
		cfg.HealthCheckInterval = *fileCfg.HealthCheckInterval
	}
	if fileCfg.ElevenLabsBaseURL != nil {
		cfg.ElevenLabsBaseURL = *fileCfg.ElevenLabsBaseURL
	}
	if fileCfg.ElevenLabsTTSVoiceID != nil {
		cfg.ElevenLabsTTSVoiceID = *fileCfg.ElevenLabsTTSVoiceID
	}
	if fileCfg.AllowedOrigins != nil {
		cfg.AllowedOrigins = normalizeStringSlice(fileCfg.AllowedOrigins)
	}
	if fileCfg.EmbedModelText != nil {
		cfg.EmbedModelText = *fileCfg.EmbedModelText
	}
	if fileCfg.EmbedModelCode != nil {
		cfg.EmbedModelCode = *fileCfg.EmbedModelCode
	}
	if fileCfg.ChatModel != nil {
		cfg.ChatModel = *fileCfg.ChatModel
	}
	if fileCfg.RAGSystemPrompt != nil {
		cfg.RAGSystemPrompt = *fileCfg.RAGSystemPrompt
	}
	if fileCfg.RAGGenerateAnswer != nil {
		cfg.RAGGenerateAnswer = *fileCfg.RAGGenerateAnswer
	}
	if fileCfg.RAGKDefault != nil {
		cfg.RAGKDefault = *fileCfg.RAGKDefault
	}
	if fileCfg.RAGMaxContextChars != nil {
		cfg.RAGMaxContextChars = *fileCfg.RAGMaxContextChars
	}
	if fileCfg.RAGOversampleFactor != nil {
		cfg.RAGOversampleFactor = *fileCfg.RAGOversampleFactor
	}
	if fileCfg.ChunkingStrategy != nil {
		cfg.ChunkingStrategy = *fileCfg.ChunkingStrategy
	}
	if fileCfg.ChunkingMaxTokens != nil {
		cfg.ChunkingMaxTokens = *fileCfg.ChunkingMaxTokens
	}
	if fileCfg.ChunkingOverlapTokens != nil {
		cfg.ChunkingOverlapTokens = *fileCfg.ChunkingOverlapTokens
	}
	if fileCfg.IngestGitignore != nil {
		cfg.IngestGitignore = *fileCfg.IngestGitignore
	}
	if fileCfg.IngestFollowSymlinks != nil {
		cfg.IngestFollowSymlinks = *fileCfg.IngestFollowSymlinks
	}
	if fileCfg.IngestMaxFileMB != nil {
		cfg.IngestMaxFileMB = *fileCfg.IngestMaxFileMB
	}
	if fileCfg.STTProvider != nil {
		cfg.STTProvider = *fileCfg.STTProvider
	}
	if fileCfg.STTMistralModel != nil {
		cfg.STTMistralModel = *fileCfg.STTMistralModel
	}
	if fileCfg.STTElevenLabsModel != nil {
		cfg.STTElevenLabsModel = *fileCfg.STTElevenLabsModel
	}
	if fileCfg.STTElevenLabsLanguageCode != nil {
		cfg.STTElevenLabsLanguageCode = *fileCfg.STTElevenLabsLanguageCode
	}
	if fileCfg.ServerTLSCertFile != nil {
		cfg.ServerTLSCertFile = *fileCfg.ServerTLSCertFile
	}
	if fileCfg.ServerTLSKeyFile != nil {
		cfg.ServerTLSKeyFile = *fileCfg.ServerTLSKeyFile
	}
	if fileCfg.X402Mode != nil {
		cfg.X402.Mode = *fileCfg.X402Mode
	}
	if fileCfg.X402FacilitatorURL != nil {
		cfg.X402.FacilitatorURL = *fileCfg.X402FacilitatorURL
	}
	// ignore any x402_facilitator_token value from disk; tokens must come from
	// the environment to avoid persistence.
	if fileCfg.X402ResourceBaseURL != nil {
		cfg.X402.ResourceBaseURL = *fileCfg.X402ResourceBaseURL
	}
	if fileCfg.X402ToolsCallEnabled != nil {
		cfg.X402.ToolsCallEnabled = *fileCfg.X402ToolsCallEnabled
	}
	if fileCfg.X402PriceAtomic != nil {
		cfg.X402.PriceAtomic = *fileCfg.X402PriceAtomic
	}
	if fileCfg.X402Network != nil {
		cfg.X402.Network = *fileCfg.X402Network
	}
	if fileCfg.X402Scheme != nil {
		cfg.X402.Scheme = *fileCfg.X402Scheme
	}
	if fileCfg.X402Asset != nil {
		cfg.X402.Asset = *fileCfg.X402Asset
	}
	if fileCfg.X402PayTo != nil {
		cfg.X402.PayTo = *fileCfg.X402PayTo
	}
}

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

func parseConfigYAML(raw []byte) (fileConfig, error) {
	cfg := fileConfig{}
	reader := strings.NewReader(string(raw))
	scanner := bufio.NewScanner(reader)
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
			value := strings.TrimSpace(strings.TrimPrefix(line, "- "))
			value = unquoteYAMLScalar(value)
			setFileListValue(&cfg, currentListKey, value)
			continue
		}

		currentListKey = ""
		for level := range sectionByIndent {
			if level >= indent {
				delete(sectionByIndent, level)
			}
		}
		sectionPrefix := nearestSectionPrefix(sectionByIndent, indent)

		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return fileConfig{}, fmt.Errorf("line %d: expected key: value", lineNo)
		}
		key = strings.TrimSpace(key)
		if sectionPrefix != "" && !strings.Contains(key, ".") {
			key = sectionPrefix + "." + key
		}
		key = canonicalizeConfigKey(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return fileConfig{}, fmt.Errorf("line %d: empty key", lineNo)
		}

		if value == "" {
			if isListConfigKey(key) {
				currentListKey = key
				setFileListValue(&cfg, key, "")
				continue
			}
			if isMapSectionKey(key) {
				sectionByIndent[indent] = key
				continue
			}
			if err := setFileScalarValue(&cfg, key, ""); err != nil {
				return fileConfig{}, fmt.Errorf("line %d: %w", lineNo, err)
			}
			continue
		}
		if value == "[]" {
			if isListConfigKey(key) {
				setFileListValue(&cfg, key, "")
			}
			continue
		}
		if strings.HasPrefix(value, "[") && !strings.HasSuffix(value, "]") {
			return fileConfig{}, fmt.Errorf("line %d: malformed list value for %s", lineNo, key)
		}
		if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
			inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(value, "["), "]"))
			if inner == "" {
				setFileListValue(&cfg, key, "")
				continue
			}
			for _, token := range strings.Split(inner, ",") {
				token = unquoteYAMLScalar(strings.TrimSpace(token))
				setFileListValue(&cfg, key, token)
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

func canonicalizeConfigKey(key string) string {
	key = strings.TrimSpace(strings.ToLower(key))
	switch key {
	case "server.listen":
		return "listen_addr"
	case "server.mcp_path":
		return "mcp_path"
	case "server.protocol_version":
		return "protocol_version"
	case "server.public":
		return "public"
	case "security.auth.mode":
		return "auth_mode"
	case "security.allowed_origins":
		return "allowed_origins"
	case "security.path_excludes":
		return "path_excludes"
	case "security.secret_patterns":
		return "secret_patterns"
	case "mistral.embed_text_model":
		return "embed_model_text"
	case "mistral.embed_code_model":
		return "embed_model_code"
	case "mistral.chat_model":
		return "chat_model"
	case "rag_generate_answer", "generate_answer":
		return "rag.generate_answer"
	case "rag_k_default", "k_default":
		return "rag.k_default"
	case "rag_system_prompt", "system_prompt":
		return "rag.system_prompt"
	case "rag_max_context_chars", "max_context_chars":
		return "rag.max_context_chars"
	case "rag_oversample_factor", "oversample_factor":
		return "rag.oversample_factor"
	case "chunking_strategy":
		return "chunking.strategy"
	case "chunking_max_tokens":
		return "chunking.max_tokens"
	case "chunking_overlap_tokens":
		return "chunking.overlap_tokens"
	case "ingest_gitignore", "gitignore":
		return "ingest.gitignore"
	case "ingest_follow_symlinks", "follow_symlinks":
		return "ingest.follow_symlinks"
	case "ingest_max_file_mb", "max_file_mb":
		return "ingest.max_file_mb"
	case "stt_provider":
		return "stt.provider"
	case "stt_mistral_model":
		return "stt.mistral.model"
	case "stt_elevenlabs_model":
		return "stt.elevenlabs.model"
	case "stt_elevenlabs_language_code", "elevenlabs_language_code":
		return "stt.elevenlabs.language_code"
	case "server_tls_cert_file", "tls_cert_file", "cert_file", "server.tls.cert":
		return "server.tls.cert_file"
	case "server_tls_key_file", "tls_key_file", "key_file", "server.tls.key":
		return "server.tls.key_file"
	case "x402.mode":
		return "x402_mode"
	case "x402.facilitator_url":
		return "x402_facilitator_url"
	case "x402.resource_base_url":
		return "x402_resource_base_url"
	case "x402.facilitator_token":
		return "x402_facilitator_token"
	case "x402.route_policy.tools_call.enabled":
		return "x402_tools_call_enabled"
	case "x402.route_policy.tools_call.price":
		return "x402_price_atomic"
	case "x402.route_policy.tools_call.network":
		return "x402_network"
	case "x402.route_policy.tools_call.scheme":
		return "x402_scheme"
	case "x402.route_policy.tools_call.asset":
		return "x402_asset"
	case "x402.route_policy.tools_call.pay_to":
		return "x402_pay_to"
	default:
		return key
	}
}

func isMapSectionKey(key string) bool {
	switch key {
	case "rag", "ingest", "stt", "stt.mistral", "stt.elevenlabs", "server", "server.tls", "secret_sources", "mistral", "security", "security.auth", "x402", "x402.route_policy", "x402.route_policy.tools_call", "chunking":
		return true
	default:
		return false
	}
}

func setFileScalarValue(cfg *fileConfig, key, value string) error {
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
	case "public":
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("invalid boolean for %s", key)
		}
		cfg.Public = boolPtr(parsed)
	case "auth_mode":
		cfg.AuthMode = strPtr(value)
	case "rate_limit_rps":
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s", key)
		}
		cfg.RateLimitRPS = intPtr(parsed)
	case "rate_limit_burst":
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s", key)
		}
		cfg.RateLimitBurst = intPtr(parsed)
	case "mistral_base_url":
		cfg.MistralBaseURL = strPtr(value)
	case "elevenlabs_base_url":
		cfg.ElevenLabsBaseURL = strPtr(value)
	case "elevenlabs_tts_voice_id":
		cfg.ElevenLabsTTSVoiceID = strPtr(value)
	case "embed_model_text":
		cfg.EmbedModelText = strPtr(value)
	case "embed_model_code":
		cfg.EmbedModelCode = strPtr(value)
	case "chat_model":
		cfg.ChatModel = strPtr(value)
	case "rag.generate_answer":
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("invalid boolean for %s", key)
		}
		cfg.RAGGenerateAnswer = boolPtr(parsed)
	case "rag.k_default":
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s", key)
		}
		cfg.RAGKDefault = intPtr(parsed)
	case "rag.system_prompt":
		cfg.RAGSystemPrompt = strPtr(value)
	case "rag.max_context_chars":
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s", key)
		}
		cfg.RAGMaxContextChars = intPtr(parsed)
	case "rag.oversample_factor":
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s", key)
		}
		cfg.RAGOversampleFactor = intPtr(parsed)
	case "chunking.strategy":
		cfg.ChunkingStrategy = strPtr(value)
	case "chunking.max_tokens":
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s", key)
		}
		cfg.ChunkingMaxTokens = intPtr(parsed)
	case "chunking.overlap_tokens":
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s", key)
		}
		cfg.ChunkingOverlapTokens = intPtr(parsed)
	case "ingest.gitignore":
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("invalid boolean for %s", key)
		}
		cfg.IngestGitignore = boolPtr(parsed)
	case "ingest.follow_symlinks":
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("invalid boolean for %s", key)
		}
		cfg.IngestFollowSymlinks = boolPtr(parsed)
	case "ingest.max_file_mb":
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s", key)
		}
		cfg.IngestMaxFileMB = intPtr(parsed)
	case "stt.provider":
		cfg.STTProvider = strPtr(value)
	case "stt.mistral.model":
		cfg.STTMistralModel = strPtr(value)
	case "stt.elevenlabs.model":
		cfg.STTElevenLabsModel = strPtr(value)
	case "stt.elevenlabs.language_code":
		cfg.STTElevenLabsLanguageCode = strPtr(value)
	case "server.tls.cert_file":
		cfg.ServerTLSCertFile = strPtr(value)
	case "server.tls.key_file":
		cfg.ServerTLSKeyFile = strPtr(value)
	case "session_inactivity_timeout":
		if value == "" {
			return nil
		}
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("invalid duration for %s", key)
		}
		cfg.SessionInactivityTimeout = &d
	case "session_max_lifetime":
		if value == "" {
			return nil
		}
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("invalid duration for %s", key)
		}
		cfg.SessionMaxLifetime = &d
	case "health_check_interval":
		if value == "" {
			return nil
		}
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("invalid duration for %s", key)
		}
		cfg.HealthCheckInterval = &d
	case "x402_mode":
		cfg.X402Mode = strPtr(value)
	case "x402_facilitator_url":
		cfg.X402FacilitatorURL = strPtr(value)
	case "x402_facilitator_token":
		// field deliberately ignored; tokens are env-only for security
	case "x402_resource_base_url":
		cfg.X402ResourceBaseURL = strPtr(value)
	case "x402_tools_call_enabled":
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("invalid boolean for %s", key)
		}
		cfg.X402ToolsCallEnabled = boolPtr(parsed)
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
	default:
		// unknown keys are intentionally ignored for forward compatibility
	}
	return nil
}

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

func isListConfigKey(key string) bool {
	key = canonicalizeConfigKey(key)
	switch key {
	case "trusted_proxies", "path_excludes", "secret_patterns", "allowed_origins":
		return true
	default:
		return false
	}
}

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
	writeInt("rate_limit_rps", cfg.RateLimitRPS)
	writeInt("rate_limit_burst", cfg.RateLimitBurst)
	writeList("trusted_proxies", cfg.TrustedProxies)
	writeList("path_excludes", cfg.PathExcludes)
	writeList("secret_patterns", cfg.SecretPatterns)
	writeScalar("mistral_base_url", cfg.MistralBaseURL)
	writeScalar("session_inactivity_timeout", cfg.SessionInactivityTimeout.String())
	writeScalar("session_max_lifetime", cfg.SessionMaxLifetime.String())
	writeScalar("health_check_interval", cfg.HealthCheckInterval.String())
	writeScalar("elevenlabs_base_url", cfg.ElevenLabsBaseURL)
	writeScalar("elevenlabs_tts_voice_id", cfg.ElevenLabsTTSVoiceID)
	writeList("allowed_origins", cfg.AllowedOrigins)
	writeScalar("embed_model_text", cfg.EmbedModelText)
	writeScalar("embed_model_code", cfg.EmbedModelCode)
	writeScalar("chat_model", cfg.ChatModel)
	writeBool("rag_generate_answer", cfg.RAGGenerateAnswer)
	writeInt("rag_k_default", cfg.RAGKDefault)
	writeScalar("rag_system_prompt", cfg.RAGSystemPrompt)
	writeInt("rag_max_context_chars", cfg.RAGMaxContextChars)
	writeInt("rag_oversample_factor", cfg.RAGOversampleFactor)
	writeScalar("chunking_strategy", cfg.ChunkingStrategy)
	writeInt("chunking_max_tokens", cfg.ChunkingMaxTokens)
	writeInt("chunking_overlap_tokens", cfg.ChunkingOverlapTokens)
	writeBool("ingest_gitignore", cfg.IngestGitignore)
	writeBool("ingest_follow_symlinks", cfg.IngestFollowSymlinks)
	writeInt("ingest_max_file_mb", cfg.IngestMaxFileMB)
	writeScalar("stt_provider", cfg.STTProvider)
	writeScalar("stt_mistral_model", cfg.STTMistralModel)
	writeScalar("stt_elevenlabs_model", cfg.STTElevenLabsModel)
	writeScalar("stt_elevenlabs_language_code", cfg.STTElevenLabsLanguageCode)
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

	return []byte(b.String()), nil
}

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

func strPtr(value string) *string { return &value }
func boolPtr(value bool) *bool    { return &value }
func intPtr(value int) *int       { return &value }

func applyEnvOverrides(cfg *Config, overrideEnv map[string]string) {
	if cfg == nil {
		return
	}
	if apiKey, ok := envLookup("MISTRAL_API_KEY", overrideEnv); ok && strings.TrimSpace(apiKey) != "" {
		cfg.MistralAPIKey = apiKey
	}
	if baseURL, ok := envLookup("MISTRAL_BASE_URL", overrideEnv); ok && strings.TrimSpace(baseURL) != "" {
		cfg.MistralBaseURL = baseURL
	}
	if m, ok := envLookup("DIR2MCP_EMBED_MODEL_TEXT", overrideEnv); ok && strings.TrimSpace(m) != "" {
		cfg.EmbedModelText = strings.TrimSpace(m)
	}
	if m, ok := envLookup("DIR2MCP_EMBED_MODEL_CODE", overrideEnv); ok && strings.TrimSpace(m) != "" {
		cfg.EmbedModelCode = strings.TrimSpace(m)
	}
	if m, ok := envLookup("DIR2MCP_CHAT_MODEL", overrideEnv); ok && strings.TrimSpace(m) != "" {
		cfg.ChatModel = strings.TrimSpace(m)
	}
	if apiKey, ok := envLookup("ELEVENLABS_API_KEY", overrideEnv); ok && strings.TrimSpace(apiKey) != "" {
		cfg.ElevenLabsAPIKey = apiKey
	}
	if baseURL, ok := envLookup("ELEVENLABS_BASE_URL", overrideEnv); ok && strings.TrimSpace(baseURL) != "" {
		cfg.ElevenLabsBaseURL = baseURL
	}
	if voiceID, ok := envLookup("ELEVENLABS_VOICE_ID", overrideEnv); ok && strings.TrimSpace(voiceID) != "" {
		cfg.ElevenLabsTTSVoiceID = strings.TrimSpace(voiceID)
	}
	if allowedOrigins, ok := envLookup("DIR2MCP_ALLOWED_ORIGINS", overrideEnv); ok {
		cfg.AllowedOrigins = MergeAllowedOrigins(cfg.AllowedOrigins, allowedOrigins)
	}
	if rawRPS, ok := envLookup("DIR2MCP_RATE_LIMIT_RPS", overrideEnv); ok {
		if rps, err := strconv.Atoi(strings.TrimSpace(rawRPS)); err == nil && rps >= 0 {
			cfg.RateLimitRPS = rps
		}
	}
	if rawBurst, ok := envLookup("DIR2MCP_RATE_LIMIT_BURST", overrideEnv); ok {
		if burst, err := strconv.Atoi(strings.TrimSpace(rawBurst)); err == nil && burst >= 0 {
			cfg.RateLimitBurst = burst
		}
	}
	if trustedProxies, ok := envLookup("DIR2MCP_TRUSTED_PROXIES", overrideEnv); ok {
		cfg.TrustedProxies = MergeTrustedProxies(cfg.TrustedProxies, trustedProxies)
	}
	// session-related environment variables are durations parsed by time.ParseDuration.
	// Syntactically invalid values (parse errors) are warned about but not fatal; values
	// that parse successfully (including negative durations) are stored and may still
	// cause Validate() to fail later.
	// Historically the variable was named DIR2MCP_SESSION_TIMEOUT; we
	// elect to prefer the more explicit DIR2MCP_SESSION_INACTIVITY_TIMEOUT
	// while still accepting the old name for compatibility.
	if raw, ok := envLookup("DIR2MCP_SESSION_INACTIVITY_TIMEOUT", overrideEnv); ok {
		trimmed := strings.TrimSpace(raw)
		if trimmed != "" {
			d, err := time.ParseDuration(trimmed)
			if err != nil {
				cfg.Warnings = append(cfg.Warnings, fmt.Errorf("invalid duration for DIR2MCP_SESSION_INACTIVITY_TIMEOUT: %q (%v)", trimmed, err))
			} else {
				cfg.SessionInactivityTimeout = d
			}
		}
	} else if raw, ok := envLookup("DIR2MCP_SESSION_TIMEOUT", overrideEnv); ok {
		// fallback to old name
		trimmed := strings.TrimSpace(raw)
		if trimmed != "" {
			cfg.Warnings = append(cfg.Warnings, fmt.Errorf("DIR2MCP_SESSION_TIMEOUT is deprecated; use DIR2MCP_SESSION_INACTIVITY_TIMEOUT instead (current value: %q)", trimmed))
			d, err := time.ParseDuration(trimmed)
			if err != nil {
				cfg.Warnings = append(cfg.Warnings, fmt.Errorf("invalid duration for DIR2MCP_SESSION_TIMEOUT: %q (%v)", trimmed, err))
			} else {
				cfg.SessionInactivityTimeout = d
			}
		}
	}
	if raw, ok := envLookup("DIR2MCP_SESSION_MAX_LIFETIME", overrideEnv); ok {
		if trimmed := strings.TrimSpace(raw); trimmed != "" {
			d, err := time.ParseDuration(trimmed)
			if err != nil {
				cfg.Warnings = append(cfg.Warnings, fmt.Errorf("invalid duration for DIR2MCP_SESSION_MAX_LIFETIME: %q (%v)", trimmed, err))
			} else {
				cfg.SessionMaxLifetime = d
			}
		}
	}
	// health check interval env; zero duration interpreted as default later
	if raw, ok := envLookup("DIR2MCP_HEALTH_CHECK_INTERVAL", overrideEnv); ok {
		trimmed := strings.TrimSpace(raw)
		if trimmed != "" {
			d, err := time.ParseDuration(trimmed)
			if err != nil {
				cfg.Warnings = append(cfg.Warnings, fmt.Errorf("invalid duration for DIR2MCP_HEALTH_CHECK_INTERVAL: %q (%v)", trimmed, err))
			} else {
				cfg.HealthCheckInterval = d
			}
		}
	}
	if raw, ok := envLookup("DIR2MCP_X402_MODE", overrideEnv); ok && strings.TrimSpace(raw) != "" {
		cfg.X402.Mode = strings.TrimSpace(raw)
	}
	if raw, ok := envLookup("DIR2MCP_X402_FACILITATOR_URL", overrideEnv); ok && strings.TrimSpace(raw) != "" {
		cfg.X402.FacilitatorURL = strings.TrimSpace(raw)
	}
	if raw, ok := envLookup("DIR2MCP_X402_FACILITATOR_TOKEN", overrideEnv); ok && strings.TrimSpace(raw) != "" {
		cfg.X402.FacilitatorToken = strings.TrimSpace(raw)
	}
	if raw, ok := envLookup("DIR2MCP_X402_RESOURCE_BASE_URL", overrideEnv); ok && strings.TrimSpace(raw) != "" {
		cfg.X402.ResourceBaseURL = strings.TrimSpace(raw)
	}
	if raw, ok := envLookup("DIR2MCP_X402_TOOLS_CALL_ENABLED", overrideEnv); ok && strings.TrimSpace(raw) != "" {
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
	if raw, ok := envLookup("DIR2MCP_X402_PRICE_ATOMIC", overrideEnv); ok && strings.TrimSpace(raw) != "" {
		cfg.X402.PriceAtomic = strings.TrimSpace(raw)
	} else if raw, ok := envLookup("DIR2MCP_X402_PRICE", overrideEnv); ok && strings.TrimSpace(raw) != "" {
		cfg.X402.PriceAtomic = strings.TrimSpace(raw)
	}
	if raw, ok := envLookup("DIR2MCP_X402_NETWORK", overrideEnv); ok && strings.TrimSpace(raw) != "" {
		cfg.X402.Network = strings.TrimSpace(raw)
	}
	if raw, ok := envLookup("DIR2MCP_X402_SCHEME", overrideEnv); ok && strings.TrimSpace(raw) != "" {
		cfg.X402.Scheme = strings.TrimSpace(raw)
	}
	if raw, ok := envLookup("DIR2MCP_X402_ASSET", overrideEnv); ok && strings.TrimSpace(raw) != "" {
		cfg.X402.Asset = strings.TrimSpace(raw)
	}
	if raw, ok := envLookup("DIR2MCP_X402_PAY_TO", overrideEnv); ok && strings.TrimSpace(raw) != "" {
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
	if c.SessionInactivityTimeout < 0 {
		return fmt.Errorf("session_inactivity_timeout must be non-negative: %v", c.SessionInactivityTimeout)
	}
	if c.SessionMaxLifetime < 0 {
		return fmt.Errorf("session_max_lifetime must be non-negative: %v", c.SessionMaxLifetime)
	}
	if c.HealthCheckInterval < 0 {
		return fmt.Errorf("health_check_interval must be non-negative: %v", c.HealthCheckInterval)
	}
	if c.RAGMaxContextChars < 0 {
		return fmt.Errorf("rag.max_context_chars must be non-negative: %d", c.RAGMaxContextChars)
	}
	if c.RAGOversampleFactor < 0 {
		return fmt.Errorf("rag.oversample_factor must be non-negative: %d", c.RAGOversampleFactor)
	}
	if c.IngestMaxFileMB < 0 {
		return fmt.Errorf("ingest.max_file_mb must be non-negative: %d", c.IngestMaxFileMB)
	}
	if c.SessionInactivityTimeout == 0 {
		// zero is shorthand for the default
		c.SessionInactivityTimeout = Default().SessionInactivityTimeout
	}
	if c.HealthCheckInterval == 0 {
		c.HealthCheckInterval = Default().HealthCheckInterval
	}
	// if both timeouts are set, the max lifetime must not be shorter than
	// the inactivity timeout; otherwise the session would expire before
	// inactivity checks could ever trigger.
	if c.SessionMaxLifetime > 0 && c.SessionMaxLifetime < c.SessionInactivityTimeout {
		return fmt.Errorf("session_max_lifetime (%v) must be >= session_inactivity_timeout (%v)",
			c.SessionMaxLifetime, c.SessionInactivityTimeout)
	}
	return nil
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

	if rawURL := strings.TrimSpace(c.X402.FacilitatorURL); rawURL != "" {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return fmt.Errorf("invalid x402 facilitator URL %q: %w", rawURL, err)
		}
		if parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("invalid x402 facilitator URL: %q", rawURL)
		}
		// normalize: strip trailing slash from path so callers can safely
		// append segments.  collapse a bare "/" path to empty.
		if parsed.Path == "/" {
			parsed.Path = ""
		} else {
			parsed.Path = strings.TrimRight(parsed.Path, "/")
		}
		c.X402.FacilitatorURL = parsed.String()
	}
	if rawURL := strings.TrimSpace(c.X402.ResourceBaseURL); rawURL != "" {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return fmt.Errorf("invalid x402 resource base URL %q: %w", rawURL, err)
		}
		if parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("invalid x402 resource base URL: %q", rawURL)
		}
		// normalize as above
		if parsed.Path == "/" {
			parsed.Path = ""
		} else {
			parsed.Path = strings.TrimRight(parsed.Path, "/")
		}
		c.X402.ResourceBaseURL = parsed.String()
	}

	// network is validated later when strict mode is enabled; no need to duplicate

	if !strict {
		return nil
	}
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
	for _, r := range ns {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
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

func loadDotEnvFiles(paths []string, overrideEnv map[string]string) error {
	for _, p := range paths {
		if err := loadDotEnvFile(p, overrideEnv); err != nil {
			return err
		}
	}
	return nil
}

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

func envLookup(key string, overrideEnv map[string]string) (string, bool) {
	if overrideEnv != nil {
		val, ok := overrideEnv[key]
		return val, ok
	}
	return os.LookupEnv(key)
}

func envSet(key, value string, overrideEnv map[string]string) error {
	if overrideEnv != nil {
		overrideEnv[key] = value
		return nil
	}
	return os.Setenv(key, value)
}

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
