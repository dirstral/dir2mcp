package tests

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestX402PaymentOutcomePersistsAcrossServerRestart pins issue #124's payment
// guarantee: a settled payment outcome is persisted to the store, so after a
// server restart the same payment request is replayed from the persisted
// outcome instead of being re-verified/re-settled against the facilitator.
func TestX402PaymentOutcomePersistsAcrossServerRestart(t *testing.T) {
	fac := newFacilitatorStub(t)
	fac.verifyStatus = http.StatusOK
	fac.settleStatus = http.StatusOK
	fac.verifyBody = `{"ok":true,"isValid":true,"payer":"payer-1"}`
	fac.settleBody = `{"ok":true,"success":true,"transaction":"abc123","txHash":"abc123","network":"eip155:8453"}`
	facServer := httptest.NewServer(fac)
	defer facServer.Close()

	cfg := x402EnabledTestConfig("https://resource.example.com")
	cfg.AuthMode = "none"
	cfg.X402.FacilitatorURL = facServer.URL

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(t.Context()); err != nil {
		t.Fatalf("Init store: %v", err)
	}
	defer func() { _ = st.Close() }()

	const paidCall = `{"jsonrpc":"2.0","id":102,"method":"tools/call","params":{"name":"dir2mcp_stats","arguments":{}}}`
	const sig = "signed-payment-payload"

	// Server 1: a paid call verifies + settles exactly once.
	server1 := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	session1 := initializeSession(t, server1.URL+cfg.MCPPath)
	resp1 := postRPCWithHeaders(t, server1.URL+cfg.MCPPath, session1, paidCall, map[string]string{"PAYMENT-SIGNATURE": sig})
	if resp1.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp1.Body)
		_ = resp1.Body.Close()
		t.Fatalf("paid call status=%d want=200 body=%s", resp1.StatusCode, string(body))
	}
	_ = resp1.Body.Close()
	if fac.verifyCalls.Load() != 1 || fac.settleCalls.Load() != 1 {
		t.Fatalf("after first paid call: verify=%d settle=%d, want 1/1", fac.verifyCalls.Load(), fac.settleCalls.Load())
	}
	server1.Close()

	// Server 2: same store. Re-issuing the identical payment (same signature +
	// params => same execution key) must replay the persisted settled outcome
	// WITHOUT calling the facilitator again.
	server2 := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server2.Close()
	session2 := initializeSession(t, server2.URL+cfg.MCPPath)
	resp2 := postRPCWithHeaders(t, server2.URL+cfg.MCPPath, session2, paidCall, map[string]string{"PAYMENT-SIGNATURE": sig})
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("replayed call status=%d want=200 body=%s", resp2.StatusCode, string(body))
	}
	if strings.TrimSpace(resp2.Header.Get("PAYMENT-RESPONSE")) == "" {
		t.Error("expected PAYMENT-RESPONSE header on the replayed paid call")
	}
	if got := fac.verifyCalls.Load(); got != 1 {
		t.Errorf("verify calls after restart=%d, want 1 (no re-verification)", got)
	}
	if got := fac.settleCalls.Load(); got != 1 {
		t.Errorf("settle calls after restart=%d, want 1 (no re-settlement)", got)
	}
}
