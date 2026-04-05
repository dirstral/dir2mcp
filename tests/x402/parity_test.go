package x402_test

// parity_test.go contains black-box integration tests that verify the
// observable x402 payment-gating behaviour for all three modes (off, on,
// required) against a real MCP server backed by a mock facilitator.
//
// These tests are the regression gate for the Coinbase x402 SDK migration
// (issue #110); any refactor that breaks these assertions has changed
// user-visible behaviour.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dir2mcp/internal/config"
	"dir2mcp/internal/mcp"
	"dir2mcp/internal/protocol"
	"dir2mcp/internal/x402"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// parityMockFacilitator is a minimal httptest.Handler that records verify/settle
// calls and returns configurable status codes and bodies.
type parityMockFacilitator struct {
	verifyStatus int
	settleStatus int
	verifyBody   string
	settleBody   string
	verifyCalls  atomic.Int64
	settleCalls  atomic.Int64
}

func newParityFacilitator() *parityMockFacilitator {
	return &parityMockFacilitator{
		verifyStatus: http.StatusOK,
		settleStatus: http.StatusOK,
		verifyBody:   `{"ok":true}`,
		settleBody:   `{"ok":true}`,
	}
}

func (f *parityMockFacilitator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v2/x402/verify":
		f.verifyCalls.Add(1)
		w.WriteHeader(f.verifyStatus)
		_, _ = w.Write([]byte(f.verifyBody))
	case "/v2/x402/settle":
		f.settleCalls.Add(1)
		w.WriteHeader(f.settleStatus)
		_, _ = w.Write([]byte(f.settleBody))
	default:
		http.NotFound(w, r)
	}
}

// baseX402Config returns a fully-populated x402 config for use in tests that
// need a working facilitator.
func baseX402Config(facilitatorURL string) config.Config {
	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.X402.ToolsCallEnabled = true
	cfg.X402.FacilitatorURL = facilitatorURL
	cfg.X402.ResourceBaseURL = "https://resource.example.com"
	cfg.X402.Scheme = "exact"
	cfg.X402.PriceAtomic = "1000"
	cfg.X402.Network = "solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
	cfg.X402.Asset = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	cfg.X402.PayTo = "8N5A4rQU8vJrQmH3iiA7kE4m1df4WeyueXQqGb4G9tTj"
	return cfg
}

// parityInitSession calls MCP initialize and returns the session ID.
func parityInitSession(t *testing.T, mcpURL string) string {
	t.Helper()
	resp := paritySendRPC(t, mcpURL, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("initialize: status=%d body=%s", resp.StatusCode, body)
	}
	sid := resp.Header.Get(protocol.MCPSessionHeader)
	if sid == "" {
		t.Fatalf("initialize did not return a session ID")
	}
	return sid
}

// paritySendRPC posts a JSON-RPC request and returns the raw *http.Response.
// Callers are responsible for closing the body.
func paritySendRPC(t *testing.T, mcpURL, sessionID, body string, extraHeaders map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, mcpURL, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set(protocol.MCPSessionHeader, sessionID)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// toolsCallBody returns a JSON-RPC tools/call body for dir2mcp.stats.
func toolsCallBody(id int) string {
	return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"tools/call","params":{"name":"dir2mcp.stats","arguments":{}}}`
}

// decodeErrorCode extracts the canonical error code from a JSON-RPC error
// response body.
func decodeErrorCode(t *testing.T, body io.Reader) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Data struct {
				Code string `json:"code"`
			} `json:"data"`
		} `json:"error"`
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode error response: %v body=%s", err, raw)
	}
	return envelope.Error.Data.Code
}

// decodeRetryable extracts the retryable flag from a JSON-RPC error response
// body.
func decodeRetryable(t *testing.T, raw []byte) bool {
	t.Helper()
	var envelope struct {
		Error struct {
			Data struct {
				Retryable bool `json:"retryable"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode retryable: %v body=%s", err, raw)
	}
	return envelope.Error.Data.Retryable
}

// ---------------------------------------------------------------------------
// Mode: off
// ---------------------------------------------------------------------------

// TestParityModeOff_NoPaymentHeaderPassesThrough verifies that when mode=off,
// tools/call succeeds without any payment header.
func TestParityModeOff_NoPaymentHeaderPassesThrough(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.X402.Mode = x402.ModeOff

	srv := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer srv.Close()

	sid := parityInitSession(t, srv.URL+cfg.MCPPath)
	resp := paritySendRPC(t, srv.URL+cfg.MCPPath, sid, toolsCallBody(1), nil)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("mode=off: expected 200, got %d body=%s", resp.StatusCode, body)
	}
	if h := strings.TrimSpace(resp.Header.Get(x402.HeaderPaymentRequired)); h != "" {
		t.Fatalf("mode=off: unexpected PAYMENT-REQUIRED header: %s", h)
	}
}

// TestParityModeOff_PaymentSignatureIsIgnored verifies that when mode=off,
// a payment signature header is silently ignored (no facilitator call made).
func TestParityModeOff_PaymentSignatureIsIgnored(t *testing.T) {
	t.Parallel()
	fac := newParityFacilitator()
	facSrv := httptest.NewServer(fac)
	defer facSrv.Close()

	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.X402.Mode = x402.ModeOff
	// Deliberately set a facilitator URL; mode=off must not contact it.
	cfg.X402.FacilitatorURL = facSrv.URL

	srv := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer srv.Close()

	sid := parityInitSession(t, srv.URL+cfg.MCPPath)
	resp := paritySendRPC(t, srv.URL+cfg.MCPPath, sid, toolsCallBody(2), map[string]string{
		x402.HeaderPaymentSignature: "some-signature",
	})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("mode=off: expected 200, got %d body=%s", resp.StatusCode, body)
	}
	if fac.verifyCalls.Load() != 0 {
		t.Fatalf("mode=off: facilitator verify called %d times, want 0", fac.verifyCalls.Load())
	}
	if fac.settleCalls.Load() != 0 {
		t.Fatalf("mode=off: facilitator settle called %d times, want 0", fac.settleCalls.Load())
	}
}

// TestParityModeOff_InitializeAndToolsListAlwaysPass verifies that
// initialize and tools/list are not gated even when mode=off (baseline sanity).
func TestParityModeOff_InitializeAndToolsListAlwaysPass(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.X402.Mode = x402.ModeOff

	srv := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer srv.Close()

	sid := parityInitSession(t, srv.URL+cfg.MCPPath)

	resp := paritySendRPC(t, srv.URL+cfg.MCPPath, sid, `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("mode=off tools/list: expected 200, got %d body=%s", resp.StatusCode, body)
	}
}

// ---------------------------------------------------------------------------
// Mode: on
// ---------------------------------------------------------------------------

// TestParityModeOn_MissingConfigFailsOpen verifies that when mode=on and the
// x402 config is incomplete (no facilitator URL, no payment fields), the server
// starts without gating tools/call — fail-open semantics.
func TestParityModeOn_MissingConfigFailsOpen(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.X402.Mode = x402.ModeOn
	cfg.X402.ToolsCallEnabled = true
	// Intentionally leave all payment fields empty.

	var warningEmitted atomic.Bool
	srv := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithEventEmitter(func(level, event string, _ interface{}) {
		if event == "x402_validation_failed" {
			warningEmitted.Store(true)
		}
	})).Handler())
	defer srv.Close()

	sid := parityInitSession(t, srv.URL+cfg.MCPPath)
	resp := paritySendRPC(t, srv.URL+cfg.MCPPath, sid, toolsCallBody(4), nil)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("mode=on incomplete config: expected 200 (fail-open), got %d body=%s", resp.StatusCode, body)
	}
	if h := strings.TrimSpace(resp.Header.Get(x402.HeaderPaymentRequired)); h != "" {
		t.Fatalf("mode=on incomplete config: unexpected PAYMENT-REQUIRED header")
	}
	if !warningEmitted.Load() {
		t.Fatal("mode=on incomplete config: expected x402_validation_failed warning event")
	}
}

// TestParityModeOn_NoPaymentHeaderReturns402 verifies that when mode=on with a
// complete x402 config and no payment signature, tools/call returns 402 with a
// PAYMENT-REQUIRED challenge header.
func TestParityModeOn_NoPaymentHeaderReturns402(t *testing.T) {
	t.Parallel()
	fac := newParityFacilitator()
	facSrv := httptest.NewServer(fac)
	defer facSrv.Close()

	cfg := baseX402Config(facSrv.URL)
	cfg.X402.Mode = x402.ModeOn

	srv := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer srv.Close()

	sid := parityInitSession(t, srv.URL+cfg.MCPPath)
	resp := paritySendRPC(t, srv.URL+cfg.MCPPath, sid, toolsCallBody(5), nil)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusPaymentRequired {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("mode=on no signature: expected 402, got %d body=%s", resp.StatusCode, body)
	}
	if h := strings.TrimSpace(resp.Header.Get(x402.HeaderPaymentRequired)); h == "" {
		t.Fatal("mode=on no signature: expected PAYMENT-REQUIRED header")
	}
	if fac.verifyCalls.Load() != 0 {
		t.Fatalf("mode=on no signature: verify called %d times, want 0", fac.verifyCalls.Load())
	}
}

// TestParityModeOn_ValidPaymentAccepted verifies that when mode=on with a
// complete x402 config and a valid payment signature, tools/call succeeds and
// the PAYMENT-RESPONSE header is set.
func TestParityModeOn_ValidPaymentAccepted(t *testing.T) {
	t.Parallel()
	fac := newParityFacilitator()
	fac.verifyBody = `{"ok":true,"kind":"verify"}`
	fac.settleBody = `{"ok":true,"kind":"settle","txHash":"abc"}`
	facSrv := httptest.NewServer(fac)
	defer facSrv.Close()

	cfg := baseX402Config(facSrv.URL)
	cfg.X402.Mode = x402.ModeOn

	srv := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer srv.Close()

	sid := parityInitSession(t, srv.URL+cfg.MCPPath)
	resp := paritySendRPC(t, srv.URL+cfg.MCPPath, sid, toolsCallBody(6), map[string]string{
		x402.HeaderPaymentSignature: "valid-sig",
	})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("mode=on valid payment: expected 200, got %d body=%s", resp.StatusCode, body)
	}
	if h := strings.TrimSpace(resp.Header.Get(x402.HeaderPaymentResponse)); h == "" {
		t.Fatal("mode=on valid payment: expected PAYMENT-RESPONSE header")
	}
	if fac.verifyCalls.Load() != 1 {
		t.Fatalf("mode=on valid payment: verify calls=%d want=1", fac.verifyCalls.Load())
	}
	if fac.settleCalls.Load() != 1 {
		t.Fatalf("mode=on valid payment: settle calls=%d want=1", fac.settleCalls.Load())
	}
}

// TestParityModeOn_FacilitatorUnavailableIsRetryable verifies that when
// mode=on and the facilitator is unreachable (connection refused), the server
// returns a retryable 503.  This is fail-open at the transport level — the
// caller can retry rather than treating the request as definitively rejected.
func TestParityModeOn_FacilitatorUnavailableIsRetryable(t *testing.T) {
	t.Parallel()
	fac := newParityFacilitator()
	fac.verifyStatus = http.StatusServiceUnavailable
	fac.verifyBody = `{"message":"outage"}`
	facSrv := httptest.NewServer(fac)
	defer facSrv.Close()

	cfg := baseX402Config(facSrv.URL)
	cfg.X402.Mode = x402.ModeOn

	srv := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer srv.Close()

	sid := parityInitSession(t, srv.URL+cfg.MCPPath)
	resp := paritySendRPC(t, srv.URL+cfg.MCPPath, sid, toolsCallBody(7), map[string]string{
		x402.HeaderPaymentSignature: "sig",
	})
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("mode=on facilitator 503: expected 503, got %d body=%s", resp.StatusCode, body)
	}
	if code := decodeErrorCode(t, bytes.NewReader(body)); code != x402.CodePaymentFacilitatorUnavailable {
		t.Fatalf("mode=on facilitator 503: code=%q want=%q", code, x402.CodePaymentFacilitatorUnavailable)
	}
	if !decodeRetryable(t, body) {
		t.Fatal("mode=on facilitator 503: expected retryable=true")
	}
}

// ---------------------------------------------------------------------------
// Mode: required
// ---------------------------------------------------------------------------

// TestParityModeRequired_MissingConfigReturns503WhenChallengeCannotBeBuilt
// verifies the observable behaviour when mode=required is enabled with
// incomplete x402 config: the server still starts, but tools/call returns 503
// because it cannot build a valid PAYMENT-REQUIRED challenge from the empty
// requirement fields.
//
// Note: unlike mode=on, mode=required with incomplete config still needs
// explicit handling. The current implementation in initPaymentConfig only
// calls ValidateX402(true) for mode=on; for mode=required, it always enables
// gating if ToolsCallEnabled is true. However, with an empty FacilitatorURL,
// the x402 client itself will return CodePaymentConfigInvalid on any call,
// leading to a 503 response. This test validates that runtime behaviour.
func TestParityModeRequired_MissingConfigReturns503WhenChallengeCannotBeBuilt(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.X402.Mode = x402.ModeRequired
	cfg.X402.ToolsCallEnabled = true
	// Intentionally leave payment config empty: no facilitator URL, no
	// network, no asset, no payTo.  x402.Requirement will have empty fields.
	// Since x402.Requirement.Resource will also be empty (no ResourceBaseURL),
	// x402.BuildPaymentRequiredHeaderValue will fail, so the server falls back
	// to a 503.

	srv := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer srv.Close()

	sid := parityInitSession(t, srv.URL+cfg.MCPPath)

	// Without a payment signature: x402Enabled=true means the server checks
	// the signature first — an empty sig returns 402. But with empty
	// Requirement fields, BuildPaymentRequiredHeaderValue fails, so the server
	// returns 503 instead of 402.
	resp := paritySendRPC(t, srv.URL+cfg.MCPPath, sid, toolsCallBody(8), nil)
	defer func() { _ = resp.Body.Close() }()

	// The server cannot build a valid PAYMENT-REQUIRED challenge because the
	// requirement is invalid; it should return 503.
	if resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("mode=required no config, no sig: expected 503, got %d body=%s", resp.StatusCode, body)
	}
}

// TestParityModeRequired_NoPaymentHeaderReturns402 verifies that when
// mode=required with a complete config and no payment signature, tools/call
// returns 402 with a PAYMENT-REQUIRED challenge header.
func TestParityModeRequired_NoPaymentHeaderReturns402(t *testing.T) {
	t.Parallel()
	fac := newParityFacilitator()
	facSrv := httptest.NewServer(fac)
	defer facSrv.Close()

	cfg := baseX402Config(facSrv.URL)
	cfg.X402.Mode = x402.ModeRequired

	srv := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer srv.Close()

	sid := parityInitSession(t, srv.URL+cfg.MCPPath)
	resp := paritySendRPC(t, srv.URL+cfg.MCPPath, sid, toolsCallBody(9), nil)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusPaymentRequired {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("mode=required no sig: expected 402, got %d body=%s", resp.StatusCode, body)
	}
	if h := strings.TrimSpace(resp.Header.Get(x402.HeaderPaymentRequired)); h == "" {
		t.Fatal("mode=required no sig: expected PAYMENT-REQUIRED header")
	}
	code := decodeErrorCode(t, resp.Body)
	if code != x402.CodePaymentRequired {
		t.Fatalf("mode=required no sig: code=%q want=%q", code, x402.CodePaymentRequired)
	}
}

// TestParityModeRequired_ValidPaymentAccepted verifies that when mode=required
// with a complete config and a valid payment proof, tools/call succeeds.
func TestParityModeRequired_ValidPaymentAccepted(t *testing.T) {
	t.Parallel()
	fac := newParityFacilitator()
	fac.verifyBody = `{"ok":true}`
	fac.settleBody = `{"ok":true,"txHash":"xyz"}`
	facSrv := httptest.NewServer(fac)
	defer facSrv.Close()

	cfg := baseX402Config(facSrv.URL)
	cfg.X402.Mode = x402.ModeRequired

	srv := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer srv.Close()

	sid := parityInitSession(t, srv.URL+cfg.MCPPath)
	resp := paritySendRPC(t, srv.URL+cfg.MCPPath, sid, toolsCallBody(10), map[string]string{
		x402.HeaderPaymentSignature: "valid-sig",
	})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("mode=required valid payment: expected 200, got %d body=%s", resp.StatusCode, body)
	}
	if h := strings.TrimSpace(resp.Header.Get(x402.HeaderPaymentResponse)); h == "" {
		t.Fatal("mode=required valid payment: expected PAYMENT-RESPONSE header")
	}
}

// TestParityModeRequired_FacilitatorUnavailableIsFailClosed verifies that
// when mode=required and the facilitator returns a server error, the server
// returns 503 (retryable) — fail-closed means the request is not passed
// through even though payment can't be confirmed.
func TestParityModeRequired_FacilitatorUnavailableIsFailClosed(t *testing.T) {
	t.Parallel()
	fac := newParityFacilitator()
	fac.verifyStatus = http.StatusServiceUnavailable
	fac.verifyBody = `{"message":"down"}`
	facSrv := httptest.NewServer(fac)
	defer facSrv.Close()

	cfg := baseX402Config(facSrv.URL)
	cfg.X402.Mode = x402.ModeRequired

	srv := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer srv.Close()

	sid := parityInitSession(t, srv.URL+cfg.MCPPath)
	resp := paritySendRPC(t, srv.URL+cfg.MCPPath, sid, toolsCallBody(11), map[string]string{
		x402.HeaderPaymentSignature: "sig",
	})
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	// The request must NOT pass through — it must be rejected.
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("mode=required facilitator down: expected non-200, got 200 body=%s", body)
	}
	// Must signal retryable since the facilitator is temporarily unavailable.
	if !decodeRetryable(t, body) {
		t.Fatalf("mode=required facilitator down: expected retryable=true body=%s", body)
	}
	if code := decodeErrorCode(t, bytes.NewReader(body)); code != x402.CodePaymentFacilitatorUnavailable {
		t.Fatalf("mode=required facilitator down: code=%q want=%q", code, x402.CodePaymentFacilitatorUnavailable)
	}
}

// ---------------------------------------------------------------------------
// Cross-mode: header parity
// ---------------------------------------------------------------------------

// TestParityPaymentRequiredHeaderContainsValidJSON verifies that the
// PAYMENT-REQUIRED header value, when present, is a well-formed JSON payload
// with the expected x402Version and accepts fields.
func TestParityPaymentRequiredHeaderContainsValidJSON(t *testing.T) {
	t.Parallel()
	fac := newParityFacilitator()
	facSrv := httptest.NewServer(fac)
	defer facSrv.Close()

	for _, mode := range []string{x402.ModeOn, x402.ModeRequired} {
		mode := mode
		t.Run("mode="+mode, func(t *testing.T) {
			t.Parallel()
			cfg := baseX402Config(facSrv.URL)
			cfg.X402.Mode = mode

			srv := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
			defer srv.Close()

			sid := parityInitSession(t, srv.URL+cfg.MCPPath)
			resp := paritySendRPC(t, srv.URL+cfg.MCPPath, sid, toolsCallBody(20), nil)
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusPaymentRequired {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("mode=%s: expected 402, got %d body=%s", mode, resp.StatusCode, body)
			}

			hdr := strings.TrimSpace(resp.Header.Get(x402.HeaderPaymentRequired))
			if hdr == "" {
				t.Fatalf("mode=%s: PAYMENT-REQUIRED header is empty", mode)
			}

			var payload x402.X402Payload
			if err := json.Unmarshal([]byte(hdr), &payload); err != nil {
				t.Fatalf("mode=%s: PAYMENT-REQUIRED is not valid JSON: %v value=%s", mode, err, hdr)
			}
			if payload.X402Version != x402.X402Version {
				t.Fatalf("mode=%s: x402Version=%d want=%d", mode, payload.X402Version, x402.X402Version)
			}
			if len(payload.Accept) == 0 {
				t.Fatalf("mode=%s: expected at least one entry in accepts", mode)
			}
		})
	}
}

// TestParityModeOff_NoPaymentRequiredHeader verifies that mode=off never
// emits a PAYMENT-REQUIRED header, even for edge-case request shapes.
func TestParityModeOff_NoPaymentRequiredHeader(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.X402.Mode = x402.ModeOff

	srv := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer srv.Close()

	sid := parityInitSession(t, srv.URL+cfg.MCPPath)

	// tools/call without signature
	resp := paritySendRPC(t, srv.URL+cfg.MCPPath, sid, toolsCallBody(21), nil)
	defer func() { _ = resp.Body.Close() }()

	if h := strings.TrimSpace(resp.Header.Get(x402.HeaderPaymentRequired)); h != "" {
		t.Fatalf("mode=off: PAYMENT-REQUIRED header must be absent, got: %s", h)
	}
}

// TestParityModeOn_InvalidPaymentProofReturns402WithChallenge verifies that
// when the facilitator rejects a payment proof (4xx), the server re-issues the
// payment challenge (402 + PAYMENT-REQUIRED header).
func TestParityModeOn_InvalidPaymentProofReturns402WithChallenge(t *testing.T) {
	t.Parallel()
	fac := newParityFacilitator()
	fac.verifyStatus = http.StatusBadRequest
	fac.verifyBody = `{"message":"invalid proof"}`
	facSrv := httptest.NewServer(fac)
	defer facSrv.Close()

	cfg := baseX402Config(facSrv.URL)
	cfg.X402.Mode = x402.ModeOn

	srv := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer srv.Close()

	sid := parityInitSession(t, srv.URL+cfg.MCPPath)
	resp := paritySendRPC(t, srv.URL+cfg.MCPPath, sid, toolsCallBody(22), map[string]string{
		x402.HeaderPaymentSignature: "bad-sig",
	})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusPaymentRequired {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("mode=on bad sig: expected 402, got %d body=%s", resp.StatusCode, body)
	}
	if h := strings.TrimSpace(resp.Header.Get(x402.HeaderPaymentRequired)); h == "" {
		t.Fatal("mode=on bad sig: expected PAYMENT-REQUIRED header on re-challenge")
	}
	// Settle must not be called when verify rejects.
	if fac.settleCalls.Load() != 0 {
		t.Fatalf("mode=on bad sig: settle called %d times, want 0", fac.settleCalls.Load())
	}
}

// TestParityModeRequired_InvalidPaymentProofReturns402WithChallenge mirrors
// the above test for mode=required.
func TestParityModeRequired_InvalidPaymentProofReturns402WithChallenge(t *testing.T) {
	t.Parallel()
	fac := newParityFacilitator()
	fac.verifyStatus = http.StatusBadRequest
	fac.verifyBody = `{"message":"invalid proof"}`
	facSrv := httptest.NewServer(fac)
	defer facSrv.Close()

	cfg := baseX402Config(facSrv.URL)
	cfg.X402.Mode = x402.ModeRequired

	srv := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer srv.Close()

	sid := parityInitSession(t, srv.URL+cfg.MCPPath)
	resp := paritySendRPC(t, srv.URL+cfg.MCPPath, sid, toolsCallBody(23), map[string]string{
		x402.HeaderPaymentSignature: "bad-sig",
	})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusPaymentRequired {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("mode=required bad sig: expected 402, got %d body=%s", resp.StatusCode, body)
	}
	if h := strings.TrimSpace(resp.Header.Get(x402.HeaderPaymentRequired)); h == "" {
		t.Fatal("mode=required bad sig: expected PAYMENT-REQUIRED header")
	}
	if fac.settleCalls.Load() != 0 {
		t.Fatalf("mode=required bad sig: settle called %d times, want 0", fac.settleCalls.Load())
	}
}
