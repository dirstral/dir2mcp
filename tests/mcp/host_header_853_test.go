package tests

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
)

// The MCP Go SDK refuses a request that reaches a loopback-bound listener with a
// non-loopback Host header (DNS-rebinding protection). A tunnel or a reverse
// proxy forwards the public hostname by default, so it meets that refusal on
// every request. README "Reverse proxy and tunnel: the Host header" tells the
// operator to forward a loopback Host instead of disabling the guard.
//
// These cases pin the two facts that guidance rests on, so an SDK update that
// changes either one fails here instead of silently making the README wrong
// (issue #853).

// localhostGuardDisabled reports whether an MCPGODEBUG value turns the SDK's
// localhost guard off.
//
// It reads the value the way the SDK reads it: a comma-separated list of
// key=value pairs, each side trimmed, collected into a map, so a repeated key
// keeps its LAST value. Only the exact value "1" disables the guard, so
// "disablelocalhostprotection=0" leaves it on and these tests must still run.
func localhostGuardDisabled(mcpGoDebug string) bool {
	const key = "disablelocalhostprotection"

	value := ""
	for _, setting := range strings.Split(mcpGoDebug, ",") {
		settingKey, settingValue, ok := strings.Cut(setting, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(settingKey) == key {
			value = strings.TrimSpace(settingValue)
		}
	}
	return value == "1"
}

func startHostHeaderServer(t *testing.T) string {
	t.Helper()

	// The SDK reads this compatibility switch once, at package init. A developer
	// who exports it turns the guard off process-wide, so say that plainly rather
	// than report a failure the test cannot explain.
	if localhostGuardDisabled(os.Getenv("MCPGODEBUG")) {
		t.Skip("MCPGODEBUG disables the SDK localhost guard this test pins")
	}

	srv := mcp.NewServer(config.Config{MCPPath: "/mcp", AuthMode: "none"}, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	tr := mcp.NewSDKTransport(srv, ln, "", "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- tr.Serve(ctx, http.NotFoundHandler())
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("timeout waiting for Serve to exit")
		}
		_ = ln.Close()
	})

	return ln.Addr().String()
}

// postInitializeWithHost sends an initialize request whose Host header is the
// given value, the way a proxy would.
func postInitializeWithHost(t *testing.T, addr, host string) (int, string) {
	t.Helper()

	req, err := http.NewRequest(
		http.MethodPost,
		"http://"+addr+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`),
	)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Host = host

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST /mcp with Host %q: %v", host, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

// TestLoopbackListenerRefusesForwardedPublicHost documents the failure an
// operator hits when the proxy forwards its own public hostname.
func TestLoopbackListenerRefusesForwardedPublicHost(t *testing.T) {
	addr := startHostHeaderServer(t)

	status, body := postInitializeWithHost(t, addr, "sample.trycloudflare.com")
	if status != http.StatusForbidden {
		t.Fatalf("forwarded public Host: status=%d body=%q, want 403", status, body)
	}
	if !strings.Contains(body, "invalid Host header") {
		t.Fatalf("forwarded public Host: body=%q, want it to name the Host header", body)
	}
}

// TestLoopbackListenerAcceptsForwardedLoopbackHost documents the fix: a proxy
// that forwards a loopback Host is served, and the guard stays on. The second
// case shows the guard compares the host only, so a port mismatch is harmless.
func TestLoopbackListenerAcceptsForwardedLoopbackHost(t *testing.T) {
	addr := startHostHeaderServer(t)

	for _, host := range []string{addr, "localhost:9999", "[::1]:9999"} {
		status, body := postInitializeWithHost(t, addr, host)
		if status != http.StatusOK {
			t.Fatalf("forwarded Host %q: status=%d body=%q, want 200", host, status, body)
		}
	}
}
