package tests

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
		// io.ReadAll, not one Read: a single Read can return part of the JSON,
		// and the assertion would then miss the refusal code intermittently.
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return resp.StatusCode, string(body)
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

// The SDK transport is the path the daemon serves, and the SDK compares
// MCP-Protocol-Version for exact equality against its supported list, exactly as
// it compares Content-Type. A joined field reached it as one unknown string, so
// every call AFTER the handshake failed with "Unsupported protocol version",
// which is what the pre-release smoke gate hit over the mcp-remote bridge.
//
// The handshake has to be real: this server's own session gate refuses an
// unknown session before the SDK is reached, so a bare call would pass whether
// or not the field was repaired and would prove nothing.
func TestSDKTransport_JoinedProtocolVersionReachesTheSDK(t *testing.T) {
	srv := mcp.NewServer(config.Config{MCPPath: "/mcp", AuthMode: "none"}, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	tr := mcp.NewSDKTransport(srv, ln, "", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = tr.Serve(ctx, http.NotFoundHandler()) }()

	url := "http://" + ln.Addr().String() + "/mcp"
	client := testClient(10 * time.Second)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", ln.Addr().String(), 200*time.Millisecond); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	const joined = "2025-11-25, 2025-11-25"
	post := func(t *testing.T, body, session string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		// The field as a bridge leaves it: one supported version, stated twice.
		req.Header.Set("MCP-Protocol-Version", joined)
		if session != "" {
			req.Header.Set("MCP-Session-Id", session)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return resp, string(raw)
	}

	initResp, initBody := post(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"bridge","version":"1"}}}`, "")
	session := initResp.Header.Get("Mcp-Session-Id")
	if session == "" {
		t.Fatalf("no session from initialize (status=%d): %s", initResp.StatusCode, strings.TrimSpace(initBody))
	}
	post(t, `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`, session)

	_, body := post(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, session)
	if strings.Contains(strings.ToLower(body), "unsupported protocol version") {
		t.Errorf("the SDK refused a field that names one supported version twice: %s", strings.TrimSpace(body))
	}
	if strings.Contains(body, "UNSUPPORTED_PROTOCOL_VERSION") {
		t.Errorf("this server refused a field that names one supported version twice: %s", strings.TrimSpace(body))
	}
	if !strings.Contains(body, `"tools"`) {
		t.Errorf("tools/list did not answer with a tool list: %s", strings.TrimSpace(body))
	}
}

// A refusal must quote what the CLIENT sent. The transport repairs a repeated
// field only when it names the version this server speaks, because only that
// one is about to be accepted; repairing a field the gate is about to refuse
// would make the error report a value this server invented rather than the one
// that arrived.
func TestSDKTransport_RefusalQuotesTheFieldTheClientSent(t *testing.T) {
	srv := mcp.NewServer(config.Config{MCPPath: "/mcp", AuthMode: "none"}, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	tr := mcp.NewSDKTransport(srv, ln, "", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = tr.Serve(ctx, http.NotFoundHandler()) }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", ln.Addr().String(), 200*time.Millisecond); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	for _, sent := range []string{"2024-11-05, 2024-11-05", "2024-11-05"} {
		req, err := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/mcp",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", sent)
		resp, err := testClient(10 * time.Second).Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		raw, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		body := string(raw)
		if !strings.Contains(body, "UNSUPPORTED_PROTOCOL_VERSION") {
			t.Fatalf("a version this server does not speak must be refused; got %s", strings.TrimSpace(body))
		}
		if !strings.Contains(body, sent) {
			t.Errorf("refusal must quote the field the client sent (%q): %s", sent, strings.TrimSpace(body))
		}
	}
}
