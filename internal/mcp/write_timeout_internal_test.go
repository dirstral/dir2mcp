package mcp

import (
	"net/http"
	"testing"
	"time"
)

// TestNewMCPHTTPServer_NoWriteTimeout pins that the MCP HTTP server has no write
// deadline: net/http's WriteTimeout spans the entire handler execution, so any
// nonzero value would tear down legitimately long-running tool calls (annotate,
// ask, OCR) mid-flight and surface as "Failed to call tool". Regression for
// issue #362. ReadHeaderTimeout must remain set (slowloris protection).
func TestNewMCPHTTPServer_NoWriteTimeout(t *testing.T) {
	srv := newMCPHTTPServer(http.NewServeMux())

	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %v, want 0 (disabled) so long tool calls aren't killed mid-flight", srv.WriteTimeout)
	}
	if srv.ReadHeaderTimeout <= 0 {
		t.Fatalf("ReadHeaderTimeout = %v, want > 0 (slowloris protection must stay)", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout <= 0 {
		t.Fatalf("IdleTimeout = %v, want > 0 (idle keep-alives must still be reaped)", srv.IdleTimeout)
	}
	// Sanity: ReadHeaderTimeout should be a short, slowloris-grade bound, not a
	// value large enough to defeat its purpose.
	if srv.ReadHeaderTimeout > time.Minute {
		t.Fatalf("ReadHeaderTimeout = %v, unexpectedly large for slowloris protection", srv.ReadHeaderTimeout)
	}
}
