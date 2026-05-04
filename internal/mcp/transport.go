package mcp

import (
	"context"
	"net/http"
)

// Handler is the MCP request dispatcher.  It is intentionally identical to
// http.Handler so that the existing Server implementation can expose a
// compatible handler via Server.Handler() without any wrapping, and so that
// future SDK-based implementations can produce a compatible handler with
// minimal adaptation.
type Handler = http.Handler

// Transport abstracts the wire-framing layer for MCP-over-HTTP serving.
//
// Serve blocks until ctx is cancelled or a fatal error occurs.  The caller
// is responsible for creating and closing the listener; the Transport only
// serves/accepts on the listener it was given.  The supplied handler is
// responsible for all MCP request dispatch.
type Transport interface {
	Serve(ctx context.Context, handler Handler) error
}
