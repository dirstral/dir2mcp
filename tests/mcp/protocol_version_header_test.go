package tests

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
)

// The MCP-Protocol-Version field can arrive as repeated field lines or as one
// comma-joined list; RFC 9110 §5.3 makes those forms equivalent. A client that
// sends the header while a bridge adds its own produces "2025-11-25,
// 2025-11-25": one version, stated twice.
//
// Comparing the raw field to the pinned version rejected exactly that, and it
// is the configuration dir2mcp itself writes: `install claude` registers the
// mcp-remote bridge with an explicit --header MCP-Protocol-Version, and current
// mcp-remote sends its own too, so every post-initialize call failed with
// UNSUPPORTED_PROTOCOL_VERSION. Found by the pre-release smoke gate's stdio leg
// against a live daemon.
func TestPostInitialize_ProtocolVersionHeaderIsAFieldList(t *testing.T) {
	srv := httptest.NewServer(mcp.NewServer(config.Config{MCPPath: "/mcp", AuthMode: "none"}, nil).Handler())
	defer srv.Close()

	// A session id the gate can reach: the version check runs before the
	// session lookup, so a refusal here is the version refusal, and anything
	// else means the version passed.
	post := func(t *testing.T, setHeaders func(h http.Header)) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp",
			strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}`))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		setHeaders(req.Header)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		buf := make([]byte, 2048)
		n, _ := resp.Body.Read(buf)
		return resp.StatusCode, string(buf[:n])
	}

	rejected := func(status int, body string) bool {
		return status == http.StatusBadRequest && strings.Contains(body, "UNSUPPORTED_PROTOCOL_VERSION")
	}

	for _, tc := range []struct {
		name       string
		set        func(h http.Header)
		wantReject bool
	}{
		{"single supported value", func(h http.Header) { h.Set("MCP-Protocol-Version", "2025-11-25") }, false},
		{"comma-joined duplicate (the bridge case)", func(h http.Header) {
			h.Set("MCP-Protocol-Version", "2025-11-25, 2025-11-25")
		}, false},
		{"repeated field lines, both supported", func(h http.Header) {
			h.Add("MCP-Protocol-Version", "2025-11-25")
			h.Add("MCP-Protocol-Version", "2025-11-25")
		}, false},
		{"untrimmed list", func(h http.Header) { h.Set("MCP-Protocol-Version", " 2025-11-25 ,2025-11-25 ") }, false},
		{"absent", func(h http.Header) {}, false},
		{"empty value", func(h http.Header) { h.Set("MCP-Protocol-Version", "  ") }, false},
		// Still refused: the client has not asserted one version the server speaks.
		{"single unsupported value", func(h http.Header) { h.Set("MCP-Protocol-Version", "2024-11-05") }, true},
		{"list naming two different versions", func(h http.Header) {
			h.Set("MCP-Protocol-Version", "2025-11-25, 2024-11-05")
		}, true},
		{"repeated lines naming different versions", func(h http.Header) {
			h.Add("MCP-Protocol-Version", "2025-11-25")
			h.Add("MCP-Protocol-Version", "2024-11-05")
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := post(t, tc.set)
			if got := rejected(status, body); got != tc.wantReject {
				t.Errorf("rejected=%v want=%v (status=%d body=%s)", got, tc.wantReject, status, strings.TrimSpace(body)[:min(len(strings.TrimSpace(body)), 300)])
			}
		})
	}
}
