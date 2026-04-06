package mcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
)

// Handler is the MCP request dispatcher.  It is intentionally identical to
// http.Handler so that the existing Server implementation can expose a
// compatible handler via Server.Handler() without any wrapping, and so that
// future SDK-based implementations can produce a compatible handler with
// minimal adaptation.
type Handler = http.Handler

// Transport abstracts the wire-framing layer so that the hand-rolled HTTP
// transport can be swapped for the official MCP Go SDK transport behind a
// feature flag, without changing any externally-observable behavior.
//
// Serve blocks until ctx is cancelled or a fatal error occurs.  The caller
// is responsible for creating and closing the listener; the Transport only
// serves/accepts on the listener it was given.  The supplied handler is
// responsible for all MCP request dispatch.
type Transport interface {
	Serve(ctx context.Context, handler Handler) error
}

// LegacyTransport is the hand-rolled HTTP/TCP transport that was the only
// transport before the abstraction seam was introduced.  It delegates to
// Server.runOnListener, which owns session cleanup, rate-limit cleanup,
// payment-outcome cleanup, and graceful shutdown.
type LegacyTransport struct {
	server   *Server
	listener net.Listener
	certFile string
	keyFile  string
}

// NewLegacyTransport constructs a LegacyTransport.  certFile and keyFile are
// optional; both must be non-empty to enable TLS (matching the existing
// Server.RunOnListenerTLS contract).
func NewLegacyTransport(server *Server, listener net.Listener, certFile, keyFile string) *LegacyTransport {
	return &LegacyTransport{
		server:   server,
		listener: listener,
		certFile: certFile,
		keyFile:  keyFile,
	}
}

// Serve implements Transport.  The handler argument is accepted for interface
// compatibility but is intentionally unused: LegacyTransport delegates
// entirely to Server.runOnListener, which calls Server.Handler() internally to
// produce its own http.Handler.  Any future SDK-based transport would use the
// handler argument directly.
func (t *LegacyTransport) Serve(ctx context.Context, _ Handler) error {
	if t == nil || t.server == nil {
		return errors.New("legacy transport: nil server")
	}
	if t.listener == nil {
		return errors.New("nil listener passed to RunOnListener")
	}
	return t.server.runOnListener(ctx, t.listener, t.certFile, t.keyFile)
}

// NewTransport returns a Transport for the given mode string.  mode should be
// one of "legacy" or "sdk"; an empty string defaults to "legacy". Callers
// typically source mode from the MCP_TRANSPORT environment variable.
func NewTransport(mode string, server *Server, listener net.Listener, certFile, keyFile string) (Transport, error) {
	switch mode {
	case "", "legacy":
		return NewLegacyTransport(server, listener, certFile, keyFile), nil
	case "sdk":
		return NewSDKTransport(listener, certFile, keyFile), nil
	default:
		return nil, fmt.Errorf("MCP_TRANSPORT=%q: unknown transport mode (valid: legacy, sdk)", mode)
	}
}
