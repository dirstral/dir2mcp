package tests

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"dir2mcp/internal/config"
	"dir2mcp/internal/mcp"
)

func TestNewTransport_IsSDK(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	srv := mcp.NewServer(config.Config{}, nil)
	var tr mcp.Transport = mcp.NewSDKTransport(srv, ln, "", "")
	if _, ok := tr.(*mcp.SDKTransport); !ok {
		t.Fatalf("expected *mcp.SDKTransport, got %T", tr)
	}
}

func TestSDKTransport_NilTransportGuard(t *testing.T) {
	var tr *mcp.SDKTransport
	err := tr.Serve(context.Background(), http.NotFoundHandler())
	if err == nil {
		t.Fatal("expected error for nil transport")
	}
}

func TestSDKTransport_NilListenerGuard(t *testing.T) {
	srv := mcp.NewServer(config.Config{}, nil)
	tr := mcp.NewSDKTransport(srv, nil, "", "")
	err := tr.Serve(context.Background(), http.NotFoundHandler())
	if err == nil {
		t.Fatal("expected error for nil listener")
	}
	if !strings.Contains(err.Error(), "nil listener") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestSDKTransport_NilHandlerGuard(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	srv := mcp.NewServer(config.Config{}, nil)
	tr := mcp.NewSDKTransport(srv, ln, "", "")
	err = tr.Serve(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for nil handler")
	}
	if !strings.Contains(err.Error(), "nil handler") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestSDKTransport_Serve(t *testing.T) {
	srv := mcp.NewServer(config.Config{MCPPath: "/mcp", AuthMode: "none"}, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	tr := mcp.NewSDKTransport(srv, ln, "", "")

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

	sessionID := strings.TrimSpace(resp.Header.Get("MCP-Session-Id"))
	if sessionID == "" {
		cancel()
		t.Fatal("expected MCP-Session-Id header from SDK transport initialize")
	}

	listReq, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`))
	if err != nil {
		cancel()
		t.Fatalf("create tools/list request: %v", err)
	}
	listReq.Header.Set("Content-Type", "application/json")
	listReq.Header.Set("MCP-Session-Id", sessionID)
	listResp, err := client.Do(listReq)
	if err != nil {
		cancel()
		t.Fatalf("POST tools/list: %v", err)
	}
	listBody, _ := io.ReadAll(listResp.Body)
	_ = listResp.Body.Close()
	if listResp.StatusCode < 200 || listResp.StatusCode >= 300 {
		cancel()
		t.Fatalf("tools/list unexpected status: %d body=%s", listResp.StatusCode, listBody)
	}
	if !strings.Contains(string(listBody), `"tools"`) {
		cancel()
		t.Fatalf("tools/list missing tools array: %s", listBody)
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

func TestSDKTransport_X402MissingPaymentSignature(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.X402.Mode = "required"
	cfg.X402.ToolsCallEnabled = true
	cfg.X402.FacilitatorURL = "http://127.0.0.1:1"
	cfg.X402.ResourceBaseURL = "https://resource.example.com"
	cfg.X402.Scheme = "exact"
	cfg.X402.PriceAtomic = "1000"
	cfg.X402.Network = "solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
	cfg.X402.Asset = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	cfg.X402.PayTo = "8N5A4rQU8vJrQmH3iiA7kE4m1df4WeyueXQqGb4G9tTj"

	srv := mcp.NewServer(cfg, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	tr := mcp.NewSDKTransport(srv, ln, "", "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- tr.Serve(ctx, http.NotFoundHandler())
	}()

	client := &http.Client{Timeout: 3 * time.Second}
	url := "http://" + ln.Addr().String() + "/mcp"
	initBody := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	initResp, err := client.Post(url, "application/json", initBody)
	if err != nil {
		cancel()
		t.Fatalf("initialize: %v", err)
	}
	sessionID := strings.TrimSpace(initResp.Header.Get("MCP-Session-Id"))
	_ = initResp.Body.Close()
	if sessionID == "" {
		cancel()
		t.Fatal("expected session id from initialize")
	}

	callReq, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"dir2mcp.stats","arguments":{}}}`))
	if err != nil {
		cancel()
		t.Fatalf("create tools/call request: %v", err)
	}
	callReq.Header.Set("Content-Type", "application/json")
	callReq.Header.Set("MCP-Session-Id", sessionID)
	callResp, err := client.Do(callReq)
	if err != nil {
		cancel()
		t.Fatalf("tools/call: %v", err)
	}
	callBody, _ := io.ReadAll(callResp.Body)
	_ = callResp.Body.Close()

	if callResp.StatusCode != http.StatusPaymentRequired {
		cancel()
		t.Fatalf("expected 402 on missing payment signature, got %d body=%s", callResp.StatusCode, callBody)
	}
	if strings.TrimSpace(callResp.Header.Get("PAYMENT-REQUIRED")) == "" {
		cancel()
		t.Fatalf("expected PAYMENT-REQUIRED header body=%s", callBody)
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

func TestSDKTransport_RejectsOversizedBody(t *testing.T) {
	srv := mcp.NewServer(config.Config{MCPPath: "/mcp", AuthMode: "none"}, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	tr := mcp.NewSDKTransport(srv, ln, "", "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- tr.Serve(ctx, srv.Handler())
	}()

	client := &http.Client{Timeout: 3 * time.Second}
	url := "http://" + ln.Addr().String() + "/mcp"
	oversized := strings.Repeat("x", (1<<20)+8)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(oversized))
	if err != nil {
		cancel()
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("POST oversized body: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		cancel()
		t.Fatalf("expected 400 for oversized body, got %d", resp.StatusCode)
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
