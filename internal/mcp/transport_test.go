package mcp

import (
	"context"
	"net"
	"strings"
	"testing"

	"dir2mcp/internal/config"
)

func TestNewTransport_Legacy(t *testing.T) {
	srv := NewServer(config.Config{}, nil)
	tr, err := NewTransport("legacy", srv, "", "")
	if err != nil {
		t.Fatalf("NewTransport legacy: %v", err)
	}
	if _, ok := tr.(*LegacyTransport); !ok {
		t.Fatalf("expected *LegacyTransport, got %T", tr)
	}
}

func TestNewTransport_DefaultsToLegacy(t *testing.T) {
	t.Setenv(transportModeEnvVar, "")
	srv := NewServer(config.Config{}, nil)
	tr, err := NewTransport("", srv, "", "")
	if err != nil {
		t.Fatalf("NewTransport empty mode: %v", err)
	}
	if _, ok := tr.(*LegacyTransport); !ok {
		t.Fatalf("expected *LegacyTransport, got %T", tr)
	}
}

func TestNewTransport_EnvVar(t *testing.T) {
	t.Setenv(transportModeEnvVar, "legacy")
	srv := NewServer(config.Config{}, nil)
	tr, err := NewTransport("", srv, "", "")
	if err != nil {
		t.Fatalf("NewTransport env=legacy: %v", err)
	}
	if _, ok := tr.(*LegacyTransport); !ok {
		t.Fatalf("expected *LegacyTransport, got %T", tr)
	}
}

func TestNewTransport_SDK_NotImplemented(t *testing.T) {
	srv := NewServer(config.Config{}, nil)
	_, err := NewTransport("sdk", srv, "", "")
	if err == nil {
		t.Fatal("expected error for sdk transport")
	}
	if !strings.Contains(err.Error(), "not yet implemented") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestNewTransport_Unknown(t *testing.T) {
	srv := NewServer(config.Config{}, nil)
	_, err := NewTransport("bogus", srv, "", "")
	if err == nil {
		t.Fatal("expected error for unknown transport")
	}
	if !strings.Contains(err.Error(), "unknown transport mode") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestLegacyTransport_Serve(t *testing.T) {
	srv := NewServer(config.Config{MCPPath: "/mcp"}, nil)
	tr := NewLegacyTransport(srv, "", "")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- tr.Serve(ctx, ln)
	}()

	// cancel context to stop the server
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve returned unexpected error: %v", err)
	}
}
