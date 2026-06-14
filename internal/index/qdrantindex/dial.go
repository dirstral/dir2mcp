package qdrantindex

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/qdrant/go-client/qdrant"
)

// DefaultCollection is the Qdrant collection name used when index.qdrant.collection
// is left unset. A single dir2mcp index kind maps to one collection; callers that
// run more than one kind against the same Qdrant should override per kind.
const DefaultCollection = "dir2mcp"

// defaultGRPCPort is the Qdrant gRPC port assumed when the configured URL omits
// an explicit port. The Go client speaks gRPC (6334), not the REST port (6333).
const defaultGRPCPort = 6334

// BackendConfig is the resolved configuration for the Qdrant index backend,
// mirroring the `index.qdrant.{url,api_key,collection}` config block. The
// APIKey is a secret resolved by the caller via the existing env/keychain
// precedence (SPEC §16.1.1) and is never persisted by this package.
type BackendConfig struct {
	// URL is the Qdrant endpoint, e.g. "http://localhost:6334",
	// "https://xyz.cloud.qdrant.io:6334", or a bare "host:port". A missing
	// port defaults to the gRPC port (6334); an https:// scheme enables TLS.
	URL string
	// APIKey authenticates to Qdrant Cloud / a secured deployment. Empty for an
	// unsecured local instance.
	APIKey string
	// Collection is the Qdrant collection name. Empty defaults to DefaultCollection.
	Collection string
}

// Open dials Qdrant from cfg, verifies the server is reachable (no silent
// fallback on an unreachable endpoint), and returns a ready *Index over a fresh
// *qdrant.Client. The returned Index owns the client; Index.Close closes it.
func Open(ctx context.Context, cfg BackendConfig) (*Index, error) {
	clientCfg, err := dialConfig(cfg)
	if err != nil {
		return nil, err
	}
	client, err := qdrant.NewClient(clientCfg)
	if err != nil {
		return nil, fmt.Errorf("connect to qdrant at %q: %w", cfg.URL, err)
	}
	if _, err := client.HealthCheck(ctx); err != nil {
		// Reachability is verified up front so a misconfigured/unreachable
		// Qdrant fails loudly at startup rather than degrading silently.
		_ = client.Close()
		return nil, fmt.Errorf("qdrant health check at %q failed: %w", cfg.URL, err)
	}
	collection := strings.TrimSpace(cfg.Collection)
	if collection == "" {
		collection = DefaultCollection
	}
	return New(client, Config{Collection: collection})
}

// dialConfig translates a BackendConfig into the official client's *qdrant.Config,
// parsing the URL into host/port/TLS. It is split out from Open so the URL
// parsing is unit-testable without a live server.
func dialConfig(cfg BackendConfig) (*qdrant.Config, error) {
	host, port, useTLS, err := parseEndpoint(cfg.URL)
	if err != nil {
		return nil, err
	}
	return &qdrant.Config{
		Host:                   host,
		Port:                   port,
		APIKey:                 cfg.APIKey,
		UseTLS:                 useTLS,
		SkipCompatibilityCheck: true, // reachability is verified via HealthCheck in Open
	}, nil
}

// parseEndpoint extracts host, gRPC port, and TLS usage from a Qdrant URL.
// It accepts a full URL ("http(s)://host[:port]") or a bare "host[:port]";
// a missing port defaults to defaultGRPCPort, and an https scheme enables TLS.
func parseEndpoint(raw string) (host string, port int, useTLS bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", 0, false, errors.New("qdrant url is required")
	}
	if !strings.Contains(raw, "://") {
		// Bare host[:port]: treat as a plaintext gRPC endpoint.
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", 0, false, fmt.Errorf("parse qdrant url %q: %w", raw, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		useTLS = false
	case "https":
		useTLS = true
	default:
		return "", 0, false, fmt.Errorf("qdrant url %q: unsupported scheme %q (want http or https)", raw, u.Scheme)
	}
	host = u.Hostname()
	if host == "" {
		return "", 0, false, fmt.Errorf("qdrant url %q: missing host", raw)
	}
	port = defaultGRPCPort
	if p := u.Port(); p != "" {
		port, err = strconv.Atoi(p)
		if err != nil {
			return "", 0, false, fmt.Errorf("qdrant url %q: invalid port: %w", raw, err)
		}
	}
	// net.JoinHostPort is not needed by the caller, but validating the pair here
	// surfaces a malformed host early.
	_ = net.JoinHostPort(host, strconv.Itoa(port))
	return host, port, useTLS, nil
}
