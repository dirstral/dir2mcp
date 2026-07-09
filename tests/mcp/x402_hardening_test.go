package tests

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/store"
)

// noncePaymentSignature builds a base64-encoded x402 v2 PaymentPayload carrying a
// client authorization nonce and an in-window validAfter/validBefore, matching
// what a compliant client attaches in PAYMENT-SIGNATURE.
func noncePaymentSignature(t *testing.T, nonce string) string {
	t.Helper()
	now := time.Now().UTC().Unix()
	payload := map[string]any{
		"x402Version": 2,
		"scheme":      "exact",
		"network":     "solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d",
		"payload": map[string]any{
			"signature": "0xsignature",
			"authorization": map[string]any{
				"nonce":       nonce,
				"validAfter":  fmt.Sprintf("%d", now-30),
				"validBefore": fmt.Sprintf("%d", now+120),
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payment payload: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func searchCallBody(id int, query string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"dir2mcp_search","arguments":{"query":%q}}}`, id, query)
}

// TestX402Nonce_ReplayWithDifferentRequestRejected verifies gap 2/4: a nonce that
// was consumed for one logical request MUST NOT authorize a different request,
// even against a facilitator that would happily re-approve it.
func TestX402Nonce_ReplayWithDifferentRequestRejected(t *testing.T) {
	t.Parallel()
	fac := newFacilitatorStub(t)
	facServer := httptest.NewServer(fac)
	defer facServer.Close()

	cfg := x402EnabledTestConfig("https://resource.example.com")
	cfg.AuthMode = "none"
	cfg.X402.FacilitatorURL = facServer.URL

	retriever := &countingSearchRetriever{}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	sig := noncePaymentSignature(t, "0x1111111111111111111111111111111111111111111111111111111111111111")

	first := postRPCWithHeaders(t, server.URL+cfg.MCPPath, sessionID, searchCallBody(701, "foo"), map[string]string{
		"PAYMENT-SIGNATURE": sig,
	})
	if first.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(first.Body)
		_ = first.Body.Close()
		t.Fatalf("first call status=%d want=200 body=%s", first.StatusCode, string(payload))
	}
	_ = first.Body.Close()

	// Same nonce, different request (different query) -> replay -> reject.
	second := postRPCWithHeaders(t, server.URL+cfg.MCPPath, sessionID, searchCallBody(702, "bar"), map[string]string{
		"PAYMENT-SIGNATURE": sig,
	})
	defer func() { _ = second.Body.Close() }()
	assertRPCErrorCodeAndRetryable(t, second, http.StatusPaymentRequired, "PAYMENT_INVALID", false)

	if retriever.searchCalls.Load() != 1 {
		t.Fatalf("search calls=%d want=1 (replay must not execute the tool)", retriever.searchCalls.Load())
	}
	if fac.settleCalls.Load() != 1 {
		t.Fatalf("settle calls=%d want=1 (replay must not settle again)", fac.settleCalls.Load())
	}
}

// TestX402Nonce_IdempotentRetrySameRequestReplaysOutcome verifies that an exact
// idempotent retry of the same (nonce, request) re-surfaces the recorded outcome
// without a second execution or settlement (and without a re-charge).
func TestX402Nonce_IdempotentRetrySameRequestReplaysOutcome(t *testing.T) {
	t.Parallel()
	fac := newFacilitatorStub(t)
	facServer := httptest.NewServer(fac)
	defer facServer.Close()

	cfg := x402EnabledTestConfig("https://resource.example.com")
	cfg.AuthMode = "none"
	cfg.X402.FacilitatorURL = facServer.URL

	retriever := &countingSearchRetriever{}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	sig := noncePaymentSignature(t, "0x2222222222222222222222222222222222222222222222222222222222222222")
	body := searchCallBody(711, "foo")

	for i := 0; i < 2; i++ {
		resp := postRPCWithHeaders(t, server.URL+cfg.MCPPath, sessionID, body, map[string]string{
			"PAYMENT-SIGNATURE": sig,
		})
		if resp.StatusCode != http.StatusOK {
			payload, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			t.Fatalf("call %d status=%d want=200 body=%s", i, resp.StatusCode, string(payload))
		}
		if strings.TrimSpace(resp.Header.Get("PAYMENT-RESPONSE")) == "" {
			_ = resp.Body.Close()
			t.Fatalf("call %d: expected PAYMENT-RESPONSE header", i)
		}
		_ = resp.Body.Close()
	}

	if retriever.searchCalls.Load() != 1 {
		t.Fatalf("search calls=%d want=1 (idempotent retry must not re-execute)", retriever.searchCalls.Load())
	}
	if fac.settleCalls.Load() != 1 {
		t.Fatalf("settle calls=%d want=1 (idempotent retry must not re-settle)", fac.settleCalls.Load())
	}
}

// TestX402Nonce_ToolErrorRollsBackReservation verifies the reserve->rollback rule:
// a gated call whose tool errors captures no payment, so the nonce is released
// and may be reused for a subsequent (different) request rather than being burned.
func TestX402Nonce_ToolErrorRetainsBindingRejectsDifferentRequest(t *testing.T) {
	t.Parallel()
	fac := newFacilitatorStub(t)
	facServer := httptest.NewServer(fac)
	defer facServer.Close()

	cfg := x402EnabledTestConfig("https://resource.example.com")
	cfg.AuthMode = "none"
	cfg.X402.FacilitatorURL = facServer.URL

	retriever := &countingSearchRetriever{}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	sig := noncePaymentSignature(t, "0x3333333333333333333333333333333333333333333333333333333333333333")

	// First: an unknown tool -> tool error -> no settle. The nonce is not
	// consumed (no charge), but its (nonce, requestKey) reservation binding is
	// retained until expiry.
	first := postRPCWithHeaders(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":721,"method":"tools/call","params":{"name":"dir2mcp_unknown","arguments":{}}}`, map[string]string{
		"PAYMENT-SIGNATURE": sig,
	})
	assertToolCallErrorCode(t, first, "METHOD_NOT_FOUND")
	_ = first.Body.Close()
	if fac.settleCalls.Load() != 0 {
		t.Fatalf("settle calls after tool error=%d want=0", fac.settleCalls.Load())
	}

	// Second: SAME nonce, a DIFFERENT request. The single-use nonce was already
	// presented for a different request key, so reusing it here is a cross-request
	// replay and MUST be rejected — it must not reach tool execution or settle,
	// even though the first call captured no payment.
	second := postRPCWithHeaders(t, server.URL+cfg.MCPPath, sessionID, searchCallBody(722, "foo"), map[string]string{
		"PAYMENT-SIGNATURE": sig,
	})
	defer func() { _ = second.Body.Close() }()
	assertRPCErrorCodeAndRetryable(t, second, http.StatusPaymentRequired, "PAYMENT_INVALID", false)
	if retriever.searchCalls.Load() != 0 {
		t.Fatalf("search calls=%d want=0 (different-request reuse of a presented nonce must be rejected)", retriever.searchCalls.Load())
	}
	if fac.settleCalls.Load() != 0 {
		t.Fatalf("settle calls=%d want=0 (cross-request replay must not settle)", fac.settleCalls.Load())
	}
}

// TestX402Nonce_ConsumedNoncePersistsAcrossRestart verifies the ledger is
// durable: a nonce consumed before a restart is still recognized afterwards, so
// a cross-request replay of it remains rejected across process lifetime.
func TestX402Nonce_ConsumedNoncePersistsAcrossRestart(t *testing.T) {
	t.Parallel()
	fac := newFacilitatorStub(t)
	facServer := httptest.NewServer(fac)
	defer facServer.Close()

	cfg := x402EnabledTestConfig("https://resource.example.com")
	cfg.AuthMode = "none"
	cfg.X402.FacilitatorURL = facServer.URL

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("Init store failed: %v", err)
	}
	defer func() { _ = st.Close() }()

	retriever := &countingSearchRetriever{}
	server1 := httptest.NewServer(mcp.NewServer(cfg, retriever, mcp.WithStore(st)).Handler())
	sessionID := initializeSession(t, server1.URL+cfg.MCPPath)
	sig := noncePaymentSignature(t, "0x6666666666666666666666666666666666666666666666666666666666666666")

	first := postRPCWithHeaders(t, server1.URL+cfg.MCPPath, sessionID, searchCallBody(751, "foo"), map[string]string{
		"PAYMENT-SIGNATURE": sig,
	})
	if first.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(first.Body)
		_ = first.Body.Close()
		server1.Close()
		t.Fatalf("first call status=%d want=200 body=%s", first.StatusCode, string(payload))
	}
	_ = first.Body.Close()
	server1.Close()

	// Restart onto the same store; the consumed nonce must be re-hydrated.
	server2 := httptest.NewServer(mcp.NewServer(cfg, retriever, mcp.WithStore(st)).Handler())
	defer server2.Close()

	second := postRPCWithHeaders(t, server2.URL+cfg.MCPPath, sessionID, searchCallBody(752, "bar"), map[string]string{
		"PAYMENT-SIGNATURE": sig,
	})
	defer func() { _ = second.Body.Close() }()
	assertRPCErrorCodeAndRetryable(t, second, http.StatusPaymentRequired, "PAYMENT_INVALID", false)

	if retriever.searchCalls.Load() != 1 {
		t.Fatalf("search calls=%d want=1 (replay after restart must not execute)", retriever.searchCalls.Load())
	}
}

// TestX402Nonce_CanonicalKeyDedupsReorderedParams verifies gap 3: two calls whose
// params differ only in JSON key order / whitespace dedupe to one execution and
// one settlement (no double-charge, no dedup bypass by re-serialization).
func TestX402Nonce_CanonicalKeyDedupsReorderedParams(t *testing.T) {
	t.Parallel()
	fac := newFacilitatorStub(t)
	facServer := httptest.NewServer(fac)
	defer facServer.Close()

	cfg := x402EnabledTestConfig("https://resource.example.com")
	cfg.AuthMode = "none"
	cfg.X402.FacilitatorURL = facServer.URL

	retriever := &countingSearchRetriever{}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	sig := noncePaymentSignature(t, "0x4444444444444444444444444444444444444444444444444444444444444444")

	bodyA := `{"jsonrpc":"2.0","id":731,"method":"tools/call","params":{"name":"dir2mcp_search","arguments":{"query":"foo","k":5}}}`
	// Same semantics, keys reordered and whitespace added.
	bodyB := `{"jsonrpc":"2.0","id":732,"method":"tools/call","params":{ "name" : "dir2mcp_search" , "arguments" : { "k" : 5 , "query" : "foo" } }}`

	respA := postRPCWithHeaders(t, server.URL+cfg.MCPPath, sessionID, bodyA, map[string]string{"PAYMENT-SIGNATURE": sig})
	if respA.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(respA.Body)
		_ = respA.Body.Close()
		t.Fatalf("call A status=%d want=200 body=%s", respA.StatusCode, string(payload))
	}
	_ = respA.Body.Close()

	respB := postRPCWithHeaders(t, server.URL+cfg.MCPPath, sessionID, bodyB, map[string]string{"PAYMENT-SIGNATURE": sig})
	defer func() { _ = respB.Body.Close() }()
	if respB.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(respB.Body)
		t.Fatalf("call B status=%d want=200 body=%s", respB.StatusCode, string(payload))
	}

	if retriever.searchCalls.Load() != 1 {
		t.Fatalf("search calls=%d want=1 (canonicalized params must dedupe)", retriever.searchCalls.Load())
	}
	if fac.settleCalls.Load() != 1 {
		t.Fatalf("settle calls=%d want=1 (no double settlement on re-serialized retry)", fac.settleCalls.Load())
	}
}

// TestX402Nonce_ExpiredValidityWindowRejected verifies the validity-window check:
// a proof whose validBefore is in the past is rejected adapter-side without a
// facilitator round-trip and without executing the tool.
func TestX402Nonce_ExpiredValidityWindowRejected(t *testing.T) {
	t.Parallel()
	fac := newFacilitatorStub(t)
	facServer := httptest.NewServer(fac)
	defer facServer.Close()

	cfg := x402EnabledTestConfig("https://resource.example.com")
	cfg.AuthMode = "none"
	cfg.X402.FacilitatorURL = facServer.URL

	retriever := &countingSearchRetriever{}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)

	now := time.Now().UTC().Unix()
	payload := map[string]any{
		"x402Version": 2,
		"scheme":      "exact",
		"network":     "solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d",
		"payload": map[string]any{
			"signature": "0xsignature",
			"authorization": map[string]any{
				"nonce":       "0x5555555555555555555555555555555555555555555555555555555555555555",
				"validAfter":  fmt.Sprintf("%d", now-600),
				"validBefore": fmt.Sprintf("%d", now-300), // already expired
			},
		},
	}
	raw, _ := json.Marshal(payload)
	sig := base64.StdEncoding.EncodeToString(raw)

	resp := postRPCWithHeaders(t, server.URL+cfg.MCPPath, sessionID, searchCallBody(741, "foo"), map[string]string{
		"PAYMENT-SIGNATURE": sig,
	})
	defer func() { _ = resp.Body.Close() }()
	assertRPCErrorCodeAndRetryable(t, resp, http.StatusPaymentRequired, "PAYMENT_INVALID", false)

	if retriever.searchCalls.Load() != 0 {
		t.Fatalf("search calls=%d want=0 (expired proof must not execute)", retriever.searchCalls.Load())
	}
	if fac.verifyCalls.Load() != 0 {
		t.Fatalf("verify calls=%d want=0 (expired proof rejected before facilitator)", fac.verifyCalls.Load())
	}
}
