package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	coreconfig "dir2mcp/internal/config"
	"dir2mcp/internal/protocol"
	"github.com/BurntSushi/toml"
	"github.com/joho/godotenv"
)

const (
	DefaultHostListen   = "127.0.0.1:8087"
	DefaultHostMCPPath  = protocol.DefaultMCPPath
	DefaultMCPURL       = "http://" + protocol.DefaultListenAddr + protocol.DefaultMCPPath
	DefaultMCPTransport = protocol.DefaultTransport
	DefaultModel        = protocol.DefaultModel
)

var DefaultProtocolVersion = coreconfig.DefaultProtocolVersion

// FieldSource indicates where a config value originates.
type FieldSource string

const (
	SourceDefault     FieldSource = "default"
	SourceConfigFile  FieldSource = "config.toml"
	SourceDotEnv      FieldSource = ".env"
	SourceDotEnvLocal FieldSource = ".env.local"
	SourceEnv         FieldSource = "env"
)

// FieldInfo describes a single configurable field and its provenance.
type FieldInfo struct {
	Key       string
	Value     string
	Source    FieldSource
	Sensitive bool
}

type Config struct {
	MCP        MCPConfig        `toml:"mcp"`
	Model      string           `toml:"model"`
	Verbose    bool             `toml:"verbose"`
	ElevenLabs ElevenLabsConfig `toml:"elevenlabs"`
	Host       HostConfig       `toml:"host"`
}

type MCPConfig struct {
	URL       string `toml:"url"`
	Transport string `toml:"transport"`
}

type ElevenLabsConfig struct {
	BaseURL string `toml:"base_url"`
	Voice   string `toml:"voice"`
}

type HostConfig struct {
	Listen  string `toml:"listen"`
	MCPPath string `toml:"mcp_path"`
}

func Default() Config {
	return Config{
		MCP: MCPConfig{
			URL:       DefaultMCPURL,
			Transport: DefaultMCPTransport,
		},
		Model:   DefaultModel,
		Verbose: false,
		ElevenLabs: ElevenLabsConfig{
			BaseURL: "https://api.elevenlabs.io",
			Voice:   "Rachel",
		},
		Host: HostConfig{
			Listen:  DefaultHostListen,
			MCPPath: DefaultHostMCPPath,
		},
	}
}

func Load() (Config, error) {
	if err := loadDotEnvPrecedence(); err != nil {
		return Config{}, err
	}

	cfg := Default()
	if err := mergeUserConfig(&cfg); err != nil {
		return Config{}, err
	}
	mergeEnv(&cfg)
	return cfg, nil
}

func StateDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "dirstral")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func secretEnvLocalPath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ".env.local"), nil
}

func dotEnvPaths(name string) []string {
	if strings.TrimSpace(name) != ".env.local" {
		return []string{name}
	}
	paths := make([]string, 0, 2)
	if secretPath, err := secretEnvLocalPath(); err == nil {
		paths = append(paths, secretPath)
	}
	paths = append(paths, name)
	return paths
}

func writeDotEnvFile(path string, env map[string]string) error {
	content, err := godotenv.Marshal(env)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

func loadDotEnvPrecedence() error {
	for _, name := range []string{".env.local", ".env"} {
		for _, path := range dotEnvPaths(name) {
			values, err := godotenv.Read(path)
			if err != nil {
				continue
			}
			for k, v := range values {
				if _, exists := os.LookupEnv(k); !exists {
					if setErr := os.Setenv(k, v); setErr != nil {
						return setErr
					}
				}
			}
		}
	}
	return nil
}

func mergeUserConfig(cfg *Config) error {
	base, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	path := filepath.Join(base, "dirstral", "config.toml")
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	_, err = toml.DecodeFile(path, cfg)
	return err
}

func mergeEnv(cfg *Config) {
	if v := strings.TrimSpace(os.Getenv("DIRSTRAL_MCP_URL")); v != "" {
		cfg.MCP.URL = v
	}
	if v := strings.TrimSpace(os.Getenv("DIRSTRAL_MCP_TRANSPORT")); v != "" {
		cfg.MCP.Transport = v
	}
	if v := strings.TrimSpace(os.Getenv("DIRSTRAL_MODEL")); v != "" {
		cfg.Model = v
	}
	if v := strings.TrimSpace(os.Getenv("DIRSTRAL_VERBOSE")); v != "" {
		cfg.Verbose = v == "1" || strings.EqualFold(v, "true")
	}
	if v := strings.TrimSpace(os.Getenv("ELEVENLABS_BASE_URL")); v != "" {
		cfg.ElevenLabs.BaseURL = v
	}
	if v := strings.TrimSpace(os.Getenv("DIRSTRAL_VOICE")); v != "" {
		cfg.ElevenLabs.Voice = v
	}
	if v := strings.TrimSpace(os.Getenv("DIRSTRAL_HOST_LISTEN")); v != "" {
		cfg.Host.Listen = v
	}
	if v := strings.TrimSpace(os.Getenv("DIRSTRAL_HOST_MCP_PATH")); v != "" {
		cfg.Host.MCPPath = v
	}
}

// ConfigPath returns the path to the user's config.toml file.
func ConfigPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "dirstral", "config.toml"), nil
}

// Save writes the config to ~/.config/dirstral/config.toml.
func Save(cfg Config) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	if err := enc.Encode(cfg); err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}

// SaveSecret writes a key=value pair into ~/.config/dirstral/.env.local.
// If the key already exists it is updated; otherwise it is appended.
// The environment variable is also set in the current process.
func SaveSecret(key, value string) error {
	path, err := secretEnvLocalPath()
	if err != nil {
		return err
	}
	env := map[string]string{}
	existing, err := godotenv.Read(path)
	if err == nil {
		env = existing
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	env[key] = value
	if err := writeDotEnvFile(path, env); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return os.Setenv(key, value)
}

// DeleteSecret removes a key from ~/.config/dirstral/.env.local and unsets it in the process env.
func DeleteSecret(key string) error {
	path, err := secretEnvLocalPath()
	if err != nil {
		return err
	}
	env := map[string]string{}
	existing, err := godotenv.Read(path)
	if err == nil {
		env = existing
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	delete(env, key)
	if err := writeDotEnvFile(path, env); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Unsetenv(key); err != nil {
		return fmt.Errorf("unsetting %s: %w", key, err)
	}
	return nil
}

// LoadSecret returns the stored secret value from ~/.config/dirstral/.env.local.
// It returns an empty string when the file or key does not exist.
func LoadSecret(key string) (string, error) {
	path, err := secretEnvLocalPath()
	if err != nil {
		return "", err
	}
	env, err := godotenv.Read(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return strings.TrimSpace(env[key]), nil
}

// fieldDef describes a configurable field for EffectiveFields.
type fieldDef struct {
	Key       string
	EnvVar    string
	Sensitive bool
}

var fieldDefs = []fieldDef{
	{Key: "mcp.url", EnvVar: "DIRSTRAL_MCP_URL"},
	{Key: "mcp.transport", EnvVar: "DIRSTRAL_MCP_TRANSPORT"},
	{Key: "model", EnvVar: "DIRSTRAL_MODEL"},
	{Key: "verbose", EnvVar: "DIRSTRAL_VERBOSE"},
	{Key: "host.listen", EnvVar: "DIRSTRAL_HOST_LISTEN"},
	{Key: "host.mcp_path", EnvVar: "DIRSTRAL_HOST_MCP_PATH"},
	{Key: "elevenlabs.base_url", EnvVar: "ELEVENLABS_BASE_URL"},
	{Key: "elevenlabs.voice", EnvVar: "DIRSTRAL_VOICE"},
	{Key: "DIR2MCP_AUTH_TOKEN", EnvVar: "DIR2MCP_AUTH_TOKEN", Sensitive: true},
	{Key: "ELEVENLABS_API_KEY", EnvVar: "ELEVENLABS_API_KEY", Sensitive: true},
}

// EnvVarForField returns the environment variable mapped to a field key.
func EnvVarForField(key string) string {
	for _, fd := range fieldDefs {
		if fd.Key == key {
			return fd.EnvVar
		}
	}
	return ""
}

// fieldValueFromConfig extracts a config field value by key name.
func fieldValueFromConfig(cfg Config, key string) string {
	switch key {
	case "mcp.url":
		return cfg.MCP.URL
	case "mcp.transport":
		return cfg.MCP.Transport
	case "model":
		return cfg.Model
	case "verbose":
		if cfg.Verbose {
			return "true"
		}
		return "false"
	case "host.listen":
		return cfg.Host.Listen
	case "host.mcp_path":
		return cfg.Host.MCPPath
	case "elevenlabs.base_url":
		return cfg.ElevenLabs.BaseURL
	case "elevenlabs.voice":
		return cfg.ElevenLabs.Voice
	default:
		return ""
	}
}

// readDotFile reads a dotenv file and returns its key-value pairs.
// Returns nil map if the file does not exist.
func readDotFile(name string) map[string]string {
	out := map[string]string{}
	found := false
	for _, path := range dotEnvPaths(name) {
		vals, err := godotenv.Read(path)
		if err != nil {
			continue
		}
		found = true
		for key, value := range vals {
			if _, exists := out[key]; !exists {
				out[key] = value
			}
		}
	}
	if !found {
		return nil
	}
	return out
}

// EffectiveFields returns info about each configurable field including
// which source provided its current value, checked in precedence order:
// env var → .env.local → .env → config.toml → default.
func EffectiveFields(cfg Config) []FieldInfo {
	dotEnvLocal := readDotFile(".env.local")
	dotEnv := readDotFile(".env")

	// Load config.toml into a separate struct to check overrides.
	def := Default()
	fileCfg := def
	if err := mergeUserConfig(&fileCfg); err != nil {
		// EffectiveFields is an observability helper. If config.toml is malformed,
		// continue with defaults/env provenance rather than failing the UI status view.
		fileCfg = def
	}
	result := make([]FieldInfo, 0, len(fieldDefs))

	for _, fd := range fieldDefs {
		fi := FieldInfo{
			Key:       fd.Key,
			Sensitive: fd.Sensitive,
		}

		// For secret-only fields, the value only comes from env / dotenv.
		if fd.Sensitive {
			fi.Value = os.Getenv(fd.EnvVar)
			fi.Source = resolveSecretSource(fd.EnvVar, dotEnvLocal, dotEnv)
			result = append(result, fi)
			continue
		}

		// Check precedence: env → .env.local → .env → config.toml → default.
		if v, ok := os.LookupEnv(fd.EnvVar); ok && strings.TrimSpace(v) != "" {
			fi.Value = strings.TrimSpace(v)
			// Disambiguate: was it set in shell env or loaded from dotenv?
			if _, inLocal := dotEnvLocal[fd.EnvVar]; inLocal {
				fi.Source = SourceDotEnvLocal
			} else if _, inDot := dotEnv[fd.EnvVar]; inDot {
				fi.Source = SourceDotEnv
			} else {
				fi.Source = SourceEnv
			}
			result = append(result, fi)
			continue
		}

		// Check config.toml (compare against default to detect overrides).
		fileVal := fieldValueFromConfig(fileCfg, fd.Key)
		defVal := fieldValueFromConfig(def, fd.Key)
		if fileVal != defVal {
			fi.Value = fileVal
			fi.Source = SourceConfigFile
			result = append(result, fi)
			continue
		}

		// Default value.
		fi.Value = fieldValueFromConfig(cfg, fd.Key)
		fi.Source = SourceDefault
		result = append(result, fi)
	}
	return result
}

// resolveSecretSource determines the source for a secret env var.
func resolveSecretSource(envVar string, dotEnvLocal, dotEnv map[string]string) FieldSource {
	if _, ok := dotEnvLocal[envVar]; ok {
		return SourceDotEnvLocal
	}
	if _, ok := dotEnv[envVar]; ok {
		return SourceDotEnv
	}
	if _, ok := os.LookupEnv(envVar); ok {
		return SourceEnv
	}
	return SourceDefault
}

// ValidateField checks whether value is valid for the given field key.
func ValidateField(key, value string) error {
	switch key {
	case "mcp.url":
		if strings.TrimSpace(value) == "" {
			return errors.New("mcp.url must not be empty")
		}
	case "mcp.transport":
		if value != "streamable-http" && value != "stdio" {
			return fmt.Errorf("mcp.transport must be \"streamable-http\" or \"stdio\", got %q", value)
		}
	case "model":
		if strings.TrimSpace(value) == "" {
			return errors.New("model must not be empty")
		}
	case "verbose":
		if value != "true" && value != "false" {
			return fmt.Errorf("verbose must be \"true\" or \"false\", got %q", value)
		}
	case "host.listen":
		_, port, err := net.SplitHostPort(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("host.listen must be host:port (e.g. %q): %w", DefaultHostListen, err)
		}
		if strings.TrimSpace(port) == "" {
			return fmt.Errorf("host.listen must include a non-empty port (e.g. %q)", DefaultHostListen)
		}
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 0 || portNumber > 65535 {
			if err != nil {
				return fmt.Errorf("host.listen must use a valid numeric port (e.g. %q): %w", DefaultHostListen, err)
			}
			return fmt.Errorf("host.listen port out of range in %q (e.g. %q)", value, DefaultHostListen)
		}
	case "host.mcp_path":
		if !strings.HasPrefix(value, "/") {
			return errors.New("host.mcp_path must start with \"/\"")
		}
	case "elevenlabs.base_url":
		if strings.TrimSpace(value) == "" {
			return errors.New("elevenlabs.base_url must not be empty")
		}
		if !strings.HasPrefix(value, "http") {
			return errors.New("elevenlabs.base_url must start with \"http\"")
		}
	case "elevenlabs.voice":
		if strings.TrimSpace(value) == "" {
			return errors.New("elevenlabs.voice must not be empty")
		}
	case "DIR2MCP_AUTH_TOKEN", "ELEVENLABS_API_KEY":
		if value != "" && strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s must not be whitespace-only", key)
		}
	}
	return nil
}

// ApplyField sets a field on the config struct by key name.
func ApplyField(cfg *Config, key, value string) {
	switch key {
	case "mcp.url":
		cfg.MCP.URL = value
	case "mcp.transport":
		cfg.MCP.Transport = value
	case "model":
		cfg.Model = value
	case "verbose":
		cfg.Verbose = strings.EqualFold(value, "true") || value == "1"
	case "host.listen":
		cfg.Host.Listen = value
	case "host.mcp_path":
		cfg.Host.MCPPath = value
	case "elevenlabs.base_url":
		cfg.ElevenLabs.BaseURL = value
	case "elevenlabs.voice":
		cfg.ElevenLabs.Voice = value
	}
}

// DefaultValueForField returns the default value for a field key.
func DefaultValueForField(key string) string {
	return fieldValueFromConfig(Default(), key)
}
