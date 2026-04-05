package mcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
)

// Transport abstracts how the MCP server serves accepted connections.
// Implementations accept and serve on the provided listener until ctx is
// cancelled or a fatal error occurs. The caller creates and closes the
// listener.
type Transport interface {
	// Serve starts accepting connections on ln and blocks until ctx is done or
	// an unrecoverable error occurs. The caller is responsible for closing ln
	// after Serve returns.
	Serve(ctx context.Context, ln net.Listener) error
}

// LegacyTransport wraps [Server] and delegates to its existing
// RunOnListener / RunOnListenerTLS methods, preserving the current behaviour
// with zero changes to the serving logic.
type LegacyTransport struct {
	server   *Server
	certFile string
	keyFile  string
}

// NewLegacyTransport creates a LegacyTransport for the given server.
// Pass non-empty certFile and keyFile to enable TLS.
func NewLegacyTransport(server *Server, certFile, keyFile string) *LegacyTransport {
	return &LegacyTransport{
		server:   server,
		certFile: strings.TrimSpace(certFile),
		keyFile:  strings.TrimSpace(keyFile),
	}
}

// Serve implements [Transport] by delegating to the underlying [Server].
func (t *LegacyTransport) Serve(ctx context.Context, ln net.Listener) error {
	if t == nil || t.server == nil {
		return errors.New("legacy transport requires a non-nil server")
	}
	if (t.certFile == "") != (t.keyFile == "") {
		return fmt.Errorf("legacy transport TLS configuration requires both certFile and keyFile")
	}
	if t.certFile != "" && t.keyFile != "" {
		return t.server.RunOnListenerTLS(ctx, ln, t.certFile, t.keyFile)
	}
	return t.server.RunOnListener(ctx, ln)
}

// transportModeEnvVar is the environment variable that selects the transport
// implementation.  Recognised values: "legacy" (default), "sdk".
const transportModeEnvVar = "MCP_TRANSPORT"

// NewTransport is a factory that returns a [Transport] based on the mode
// string.  If mode is empty the value of the MCP_TRANSPORT environment
// variable is used; if that is also empty the default is "legacy".
//
// Supported modes:
//   - "legacy" – [LegacyTransport] backed by the existing HTTP server.
//   - "sdk"    – not yet implemented; returns an error.
func NewTransport(mode string, server *Server, certFile, keyFile string) (Transport, error) {
	if strings.TrimSpace(mode) == "" {
		mode = strings.TrimSpace(os.Getenv(transportModeEnvVar))
	}
	if strings.TrimSpace(mode) == "" {
		mode = "legacy"
	}

	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "legacy":
		return NewLegacyTransport(server, certFile, keyFile), nil
	case "sdk":
		return nil, errors.New("transport mode \"sdk\": not yet implemented")
	default:
		return nil, fmt.Errorf("transport mode %q: unknown transport mode", mode)
	}
}
