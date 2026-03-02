package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"dir2mcp/internal/dirstral/host"
	"dir2mcp/internal/protocol"
)

func TestSmokeServerLifecycleWithFakeDir2MCP(t *testing.T) {
	setTestConfigDir(t)
	installFakeDir2MCPOnPath(t)

	errCh := startServerWithRetry(t, 5)

	waitFor(t, 3*time.Second, func() bool {
		_, err := host.LoadState()
		return err == nil
	})

	waitFor(t, 3*time.Second, func() bool {
		return host.Status() == nil
	})

	if err := host.Down(); err != nil {
		t.Fatalf("down failed: %v", err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("up returned error after down: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for server up to exit")
	}

	if _, err := host.LoadState(); err == nil {
		t.Fatalf("expected host state to be cleared")
	}
}

func startServerWithRetry(t *testing.T, attempts int) chan error {
	t.Helper()

	for attempt := 1; attempt <= attempts; attempt++ {
		reserved := reserveListener(t)
		listen := reserved.Addr().String()

		errCh := make(chan error, 1)
		go func(ln net.Listener, addr string) {
			_ = ln.Close()
			errCh <- host.Up(context.Background(), host.UpOptions{Listen: addr, MCPPath: protocol.DefaultMCPPath})
		}(reserved, listen)

		select {
		case err := <-errCh:
			if attempt < attempts && isAddressInUseError(err) {
				continue
			}
			if err != nil {
				t.Fatalf("server up failed (attempt %d/%d): %v", attempt, attempts, err)
			}
			t.Fatalf("server up exited before test shutdown (attempt %d/%d)", attempt, attempts)
		case <-time.After(350 * time.Millisecond):
			return errCh
		}
	}

	t.Fatalf("failed to start server after %d attempts", attempts)
	return nil
}

func installFakeDir2MCPOnPath(t *testing.T) {
	t.Helper()

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin dir: %v", err)
	}
	shimPath := filepath.Join(binDir, "dir2mcp")
	shim := fmt.Sprintf("#!/bin/sh\nGO_WANT_HELPER_PROCESS=dir2mcp exec %q -test.run=TestHelperProcessDir2MCPBinary -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(shimPath, []byte(shim), 0o755); err != nil {
		t.Fatalf("write dir2mcp shim: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func reserveListener(t *testing.T) net.Listener {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	return ln
}

func isAddressInUseError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "address already in use") || strings.Contains(msg, "bind:")
}

func waitFor(t *testing.T, timeout time.Duration, ok func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func TestHelperProcessDir2MCPBinary(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "dir2mcp" {
		return
	}

	if err := runFakeDir2MCPBinary(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	os.Exit(0)
}

func runFakeDir2MCPBinary() error {
	args := os.Args
	for i, a := range os.Args {
		if a == "--" {
			args = os.Args[i+1:]
			break
		}
	}

	listen := "127.0.0.1:8087"
	mcpPath := protocol.DefaultMCPPath
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--listen":
			if i+1 < len(args) {
				listen = args[i+1]
				i++
			}
		case "--mcp-path":
			if i+1 < len(args) {
				mcpPath = args[i+1]
				i++
			}
		}
	}
	if !strings.HasPrefix(mcpPath, "/") {
		mcpPath = "/" + mcpPath
	}

	mux := http.NewServeMux()
	const sessionID = "sess-smoke-host"
	mux.HandleFunc(mcpPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		method, _ := req["method"].(string)
		id := req["id"]

		switch method {
		case protocol.RPCMethodInitialize:
			w.Header().Set(protocol.MCPSessionHeader, sessionID)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"capabilities": map[string]any{"tools": map[string]any{}},
				},
			})
		case protocol.RPCMethodNotificationsInitialized:
			w.WriteHeader(http.StatusAccepted)
		case protocol.RPCMethodToolsList:
			if r.Header.Get(protocol.MCPSessionHeader) != sessionID {
				http.Error(w, "unknown session", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"tools": []map[string]any{{"name": protocol.ToolNameSearch, "description": "search"}},
				},
			})
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})

	server := &http.Server{Addr: listen, Handler: mux}
	sigCh := make(chan os.Signal, 1)
	signalNotify(sigCh)
	defer signalStop(sigCh)

	errCh := make(chan error, 1)
	go func() {
		err := server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	for {
		select {
		case <-sigCh:
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			_ = server.Shutdown(ctx)
			cancel()
			return nil
		case err := <-errCh:
			return err
		}
	}
}

func signalNotify(c chan<- os.Signal) {
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
}

func signalStop(c chan<- os.Signal) {
	signal.Stop(c)
}
