package elevenlabsbridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
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
	// InboundSecret is the shared secret that inbound callers must present
	// (via "Authorization: Bearer <secret>" or "X-Bridge-Secret: <secret>")
	// on every protected route. When empty, inbound auth is disabled and the
	// bridge refuses to bind to a non-loopback address unless ForceInsecure is
	// set.
	InboundSecret string
	// ForceInsecure permits binding to a non-loopback address without an
	// inbound secret. It mirrors the main server's --force-insecure escape
	// hatch and is unsafe: anyone who can reach the socket can drive
	// authenticated MCP calls against the private corpus.
	ForceInsecure bool
}

// InboundSecretConfigured reports whether an inbound shared secret is set.
func (cfg Config) InboundSecretConfigured() bool {
	return strings.TrimSpace(cfg.InboundSecret) != ""
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
	if v := strings.TrimSpace(env["BRIDGE_INBOUND_SECRET"]); v != "" {
		cfg.InboundSecret = v
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
		"MCP_URL":               os.Getenv("MCP_URL"),
		"MCP_TOKEN":             os.Getenv("MCP_TOKEN"),
		"STATE_DIR":             os.Getenv("STATE_DIR"),
		"PORT":                  os.Getenv("PORT"),
		"BRIDGE_INBOUND_SECRET": os.Getenv("BRIDGE_INBOUND_SECRET"),
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

// isLoopbackListenAddr reports whether listenAddr binds only to a loopback
// interface. An empty host (e.g. ":8088") or a wildcard host (0.0.0.0, ::) is
// treated as non-loopback because it is reachable from other hosts. Unparseable
// addresses are treated as non-loopback (fail closed).
func isLoopbackListenAddr(listenAddr string) bool {
	addr := strings.TrimSpace(listenAddr)
	if addr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// No port present; treat the whole value as the host.
		host = addr
	}
	host = strings.TrimSpace(host)
	if host == "" {
		// Empty host binds all interfaces.
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	parsed := net.ParseIP(host)
	if parsed == nil {
		return false
	}
	if parsed.IsUnspecified() {
		return false
	}
	return parsed.IsLoopback()
}

// ValidateListenSecurity enforces the non-loopback guard: binding to a
// non-loopback address without an inbound secret is refused unless
// forceInsecure is set. Loopback binds (local development) are always allowed.
// It mirrors the main server's --public/--force-insecure contract.
func ValidateListenSecurity(listenAddr string, hasInboundSecret, forceInsecure bool) error {
	if hasInboundSecret || forceInsecure {
		return nil
	}
	if isLoopbackListenAddr(listenAddr) {
		return nil
	}
	return fmt.Errorf(
		"refusing to bind elevenlabs bridge to non-loopback address %q without inbound auth: "+
			"set BRIDGE_INBOUND_SECRET (or --inbound-secret) to require a shared secret, "+
			"or pass --force-insecure to override (unsafe: exposes the private corpus to anonymous callers)",
		strings.TrimSpace(listenAddr),
	)
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
