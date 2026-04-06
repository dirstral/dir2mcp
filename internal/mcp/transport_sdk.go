package mcp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// SDKTransport is an http.Handler-based transport that serves requests on the
// provided listener using the standard library's net/http.Server.  It is
// intended to be progressively enriched with official MCP Go SDK primitives as
// the SDK matures; today it provides a fully-functional HTTP path that is
// behaviourally identical to LegacyTransport but uses the handler argument
// instead of delegating back to Server.runOnListener.
//
// This is the transport selected when MCP_TRANSPORT=sdk.
type SDKTransport struct {
	listener net.Listener
	certFile string
	keyFile  string
}

// NewSDKTransport constructs an SDKTransport.  certFile and keyFile are
// optional; both must be non-empty to enable TLS, matching the contract of
// LegacyTransport / Server.RunOnListenerTLS.
func NewSDKTransport(listener net.Listener, certFile, keyFile string) *SDKTransport {
	return &SDKTransport{
		listener: listener,
		certFile: certFile,
		keyFile:  keyFile,
	}
}

// Serve implements Transport.  It serves handler on the listener until ctx is
// cancelled, then performs a graceful shutdown with a 5-second timeout
// (matching LegacyTransport's shutdown behaviour).  TLS is enabled when both
// certFile and keyFile are non-empty.
func (t *SDKTransport) Serve(ctx context.Context, handler Handler) error {
	if t == nil {
		return errors.New("sdk transport: nil transport")
	}
	if t.listener == nil {
		return errors.New("nil listener passed to SDKTransport.Serve")
	}
	if handler == nil {
		return errors.New("sdk transport: nil handler")
	}

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() {
		var err error
		if t.certFile != "" && t.keyFile != "" {
			err = server.ServeTLS(t.listener, t.certFile, t.keyFile)
		} else {
			err = server.Serve(t.listener)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}
