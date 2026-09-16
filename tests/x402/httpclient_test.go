package x402_test

import (
	"net/http"
	"time"
)

// testClient returns an HTTP client with its OWN transport and no keep-alive
// pool (issue #999). The package's tests run in parallel, each against its own
// httptest.Server. A client built as &http.Client{Timeout: d} or
// http.DefaultClient rides the process-wide http.DefaultTransport, and
// httptest.Server.Close() calls CloseIdleConnections on that transport when it
// is the one in use: a neighbour test finishing tore down a pooled connection
// this test was about to reuse ("transport connection broken:
// CloseIdleConnections called"), which the race detector's timing exposed.
// Disabling keep-alives means there is no pooled connection to break, and a
// private transport means no other test can reach it.
func testClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
}
