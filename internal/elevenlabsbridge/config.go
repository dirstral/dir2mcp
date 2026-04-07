package elevenlabsbridge

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	DefaultMCPURL       = "http://127.0.0.1:8087/mcp"
	DefaultPort         = 8088
	DefaultStateDirName = ".dir2mcp"
	secretTokenName     = "secret.token"
)

// Config captures the bridge runtime configuration.
type Config struct {
	MCPURL   string
	MCPToken string
	StateDir string
	Port     int
}

// DefaultConfig returns the bridge defaults used when env vars and flags are
// unset.
func DefaultConfig() Config {
	return Config{
		MCPURL:   DefaultMCPURL,
		StateDir: DefaultStateDirName,
		Port:     DefaultPort,
	}
}

// LoadConfigFromEnv resolves the bridge config from environment variables.
// Explicit MCP_TOKEN takes precedence; STATE_DIR is used to discover
// <state-dir>/secret.token when MCP_TOKEN is not set.
func LoadConfigFromEnv(env map[string]string) (Config, error) {
	cfg := DefaultConfig()

	if v := strings.TrimSpace(env["MCP_URL"]); v != "" {
		cfg.MCPURL = v
	}
	if v := strings.TrimSpace(env["MCP_TOKEN"]); v != "" {
		cfg.MCPToken = v
	}
	if v := strings.TrimSpace(env["STATE_DIR"]); v != "" {
		cfg.StateDir = v
	}
	if v := strings.TrimSpace(env["PORT"]); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil || port < 1 || port > 65535 {
			return Config{}, fmt.Errorf("invalid PORT value %q", v)
		}
		cfg.Port = port
	}

	if cfg.MCPURL == "" {
		cfg.MCPURL = DefaultMCPURL
	}
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDirName
	}
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}

	return cfg, nil
}

// LoadConfigFromOSEnv resolves the bridge config from the current process env.
func LoadConfigFromOSEnv() (Config, error) {
	return LoadConfigFromEnv(map[string]string{
		"MCP_URL":   os.Getenv("MCP_URL"),
		"MCP_TOKEN": os.Getenv("MCP_TOKEN"),
		"STATE_DIR": os.Getenv("STATE_DIR"),
		"PORT":      os.Getenv("PORT"),
	})
}

// ResolveToken returns the MCP token and its source. When no explicit token or
// token file is available, the bridge runs without Authorization headers.
func ResolveToken(cfg Config) (token, source, tokenPath string, err error) {
	if v := strings.TrimSpace(cfg.MCPToken); v != "" {
		return v, "env", "", nil
	}

	stateDir := strings.TrimSpace(cfg.StateDir)
	if stateDir == "" {
		stateDir = DefaultStateDirName
	}
	tokenPath = filepath.Join(stateDir, secretTokenName)

	content, readErr := os.ReadFile(tokenPath)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return "", "none", "", nil
		}
		return "", "", "", fmt.Errorf("read MCP token file %q: %w", tokenPath, readErr)
	}

	token = strings.TrimSpace(string(content))
	if token == "" {
		return "", "", "", fmt.Errorf("MCP token file %q is empty", tokenPath)
	}

	if abs, absErr := filepath.Abs(tokenPath); absErr == nil {
		tokenPath = abs
	}
	return token, "file", tokenPath, nil
}

// EffectiveListenAddr computes the HTTP listen address from the configured
// port. The bridge intentionally binds to loopback by default.
func (cfg Config) EffectiveListenAddr() string {
	return fmt.Sprintf("127.0.0.1:%d", cfg.Port)
}
