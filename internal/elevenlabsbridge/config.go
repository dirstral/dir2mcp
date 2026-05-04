package elevenlabsbridge

import (
	"encoding/json"
	"errors"
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
	connectionFileName  = "connection.json"
)

type connectionPayload struct {
	URL       string `json:"url"`
	TokenFile string `json:"token_file,omitempty"`
}

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

// LoadConfigFromEnv resolves bridge config from environment variables and
// state metadata. MCP_URL from env takes precedence; when unset, the bridge
// falls back to <state-dir>/connection.json URL discovery and then defaults.
// Token resolution precedence is handled by ResolveToken.
func LoadConfigFromEnv(env map[string]string) (Config, error) {
	cfg := DefaultConfig()

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

	if v := strings.TrimSpace(env["MCP_URL"]); v != "" {
		cfg.MCPURL = v
		return cfg, nil
	}

	// Prefer the current dir2mcp connection metadata when available, then
	// fall back to the static default URL.
	conn, err := readConnectionPayload(cfg.StateDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, err
	}
	if strings.TrimSpace(conn.URL) != "" {
		cfg.MCPURL = strings.TrimSpace(conn.URL)
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

	// If dir2mcp uses --auth file:<path>, connection.json includes token_file.
	conn, connErr := readConnectionPayload(stateDir)
	if connErr != nil && !errors.Is(connErr, os.ErrNotExist) {
		return "", "", "", connErr
	}
	if tokenFromConn := strings.TrimSpace(conn.TokenFile); tokenFromConn != "" {
		return readTokenFromFile(tokenFromConn)
	}

	tokenPath = filepath.Join(stateDir, secretTokenName)
	token, source, tokenPath, err = readTokenFromFile(tokenPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "none", "", nil
		}
		return "", "", "", err
	}
	return token, source, tokenPath, nil
}

func readTokenFromFile(tokenPath string) (token, source, absPath string, err error) {
	content, readErr := os.ReadFile(tokenPath)
	if readErr != nil {
		return "", "", "", readErr
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

func readConnectionPayload(stateDir string) (connectionPayload, error) {
	path := filepath.Join(stateDir, connectionFileName)
	content, err := os.ReadFile(path)
	if err != nil {
		return connectionPayload{}, err
	}
	var payload connectionPayload
	if err := json.Unmarshal(content, &payload); err != nil {
		return connectionPayload{}, fmt.Errorf("parse connection file %q: %w", path, err)
	}
	return payload, nil
}
