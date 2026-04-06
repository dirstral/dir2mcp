package tests

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"dir2mcp/internal/config"
	"dir2mcp/internal/mcp"
)

func TestNewTransport_Legacy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	srv := mcp.NewServer(config.Config{}, nil)
	tr, err := mcp.NewTransport("legacy", srv, ln, "", "")
	if err != nil {
		t.Fatalf("NewTransport legacy: %v", err)
	}
	if _, ok := tr.(*mcp.LegacyTransport); !ok {
		t.Fatalf("expected *mcp.LegacyTransport, got %T", tr)
	}
}

func TestNewTransport_DefaultsToLegacy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	srv := mcp.NewServer(config.Config{}, nil)
	tr, err := mcp.NewTransport("", srv, ln, "", "")
	if err != nil {
		t.Fatalf("NewTransport empty mode: %v", err)
	}
	if _, ok := tr.(*mcp.LegacyTransport); !ok {
		t.Fatalf("expected *mcp.LegacyTransport, got %T", tr)
	}
}

func TestNewTransport_SDK_NotImplemented(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	srv := mcp.NewServer(config.Config{}, nil)
	_, err = mcp.NewTransport("sdk", srv, ln, "", "")
	if err == nil {
		t.Fatal("expected error for sdk transport")
	}
	if !strings.Contains(err.Error(), "not yet implemented") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestNewTransport_Unknown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	srv := mcp.NewServer(config.Config{}, nil)
	_, err = mcp.NewTransport("bogus", srv, ln, "", "")
	if err == nil {
		t.Fatal("expected error for unknown transport")
	}
	if !strings.Contains(err.Error(), "unknown transport mode") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestLegacyTransport_NilServerGuard(t *testing.T) {
	var tr *mcp.LegacyTransport
	err := tr.Serve(context.Background(), http.NotFoundHandler())
	if err == nil {
		t.Fatal("expected error for nil transport")
	}
}

func TestLegacyTransport_NilListenerGuard(t *testing.T) {
	srv := mcp.NewServer(config.Config{}, nil)
	tr := mcp.NewLegacyTransport(srv, nil, "", "")
	err := tr.Serve(context.Background(), http.NotFoundHandler())
	if err == nil {
		t.Fatal("expected error for nil listener")
	}
	if !strings.Contains(err.Error(), "nil listener") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestLegacyTransport_Serve(t *testing.T) {
	srv := mcp.NewServer(config.Config{MCPPath: "/mcp", AuthMode: "none"}, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	tr := mcp.NewLegacyTransport(srv, ln, "", "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- tr.Serve(ctx, srv.Handler())
	}()

	client := &http.Client{Timeout: 3 * time.Second}
	url := "http://" + ln.Addr().String() + "/mcp"
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	resp, err := client.Post(url, "application/json", body)
	if err != nil {
		cancel()
		t.Fatalf("POST /mcp: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cancel()
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for Serve to exit after context cancellation")
	}
}
