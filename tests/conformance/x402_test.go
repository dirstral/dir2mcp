package conformance

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/x402"
)

// TestX402_ModeOff_NoPaymentHeaders verifies that when mode=off, tools/call
// succeeds without any payment header, and no PAYMENT-REQUIRED header is set
// on the response.
// validV2Signature builds a syntactically complete x402 v2 PAYMENT-SIGNATURE.
//
// The adapter spec makes the v2 primitives normative, and `required` mode now
// enforces them adapter-side before calling the facilitator (#699). An opaque
// fixture is therefore refused there by design. That matters for a CONFORMANCE
// suite in particular: asserting that an opaque proof is accepted in required
// mode was asserting the non-conformance this fixes.
func validV2Signature(t *testing.T) string {
	t.Helper()
	now := time.Now().UTC().Unix()
	raw, err := json.Marshal(map[string]interface{}{
		"x402Version": 2,
		"scheme":      "exact",
		"network":     "eip155:8453",
		"payload": map[string]interface{}{
			"authorization": map[string]interface{}{
				"nonce":       "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				"validAfter":  now - 5,
				"validBefore": now + 60,
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal v2 payload: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestX402_ModeOff_NoPaymentHeaders(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.X402.Mode = x402.ModeOff

	srv := newServer(cfg)
	defer srv.Close()

	sid := initSession(t, srv.URL+cfg.MCPPath)
	resp := sendRPC(t, srv.URL+cfg.MCPPath, sid, statsCallBody(1), nil)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mode=off: status=%d want=200 body=%s", resp.StatusCode, body)
	}
	if headerPresent(resp, paymentRequiredHeader) {
		t.Fatalf("mode=off: unexpected %s header", paymentRequiredHeader)
	}
}

// TestX402_ModeOff_PaymentSignatureIgnored verifies that when mode=off, a
// request that includes a PAYMENT-SIGNATURE header is processed normally and
// the facilitator is never contacted.
func TestX402_ModeOff_PaymentSignatureIgnored(t *testing.T) {
	t.Parallel()
	fac := newMockFacilitator()
	facSrv := httptest.NewServer(fac)
	defer facSrv.Close()

	cfg := defaultConfig()
	cfg.X402.Mode = x402.ModeOff
	cfg.X402.FacilitatorURL = facSrv.URL

	srv := newServer(cfg)
	defer srv.Close()

	sid := initSession(t, srv.URL+cfg.MCPPath)
	resp := sendRPC(t, srv.URL+cfg.MCPPath, sid, statsCallBody(2), map[string]string{
		paymentSignatureHeader: "some-sig",
	})
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mode=off sig ignored: status=%d want=200 body=%s", resp.StatusCode, body)
	}
	if fac.verifyCalls.Load() != 0 {
		t.Fatalf("mode=off: facilitator verify called %d times, want 0", fac.verifyCalls.Load())
	}
}

// TestX402_ModeOn_IncompleteConfigFailsOpen verifies that when mode=on and the
// x402 config is incomplete, the server starts and handles requests without
// gating them (fail-open semantics).
func TestX402_ModeOn_IncompleteConfigFailsOpen(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.X402.Mode = x402.ModeOn
	cfg.X402.ToolsCallEnabled = true

	var warningEmitted atomic.Bool
	srv := newServer(cfg, mcp.WithEventEmitter(func(level, event string, _ interface{}) {
		if event == "x402_validation_failed" {
			warningEmitted.Store(true)
		}
	}))
	defer srv.Close()

	sid := initSession(t, srv.URL+cfg.MCPPath)
	resp := sendRPC(t, srv.URL+cfg.MCPPath, sid, statsCallBody(3), nil)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mode=on incomplete: status=%d want=200 (fail-open) body=%s", resp.StatusCode, body)
	}
	if headerPresent(resp, paymentRequiredHeader) {
		t.Fatalf("mode=on incomplete: unexpected PAYMENT-REQUIRED header")
	}
	if !warningEmitted.Load() {
		t.Fatal("mode=on incomplete: expected x402_validation_failed warning event")
	}
}

// TestX402_ModeOn_NoPaymentReturns402 verifies that when mode=on with a
// complete x402 config, a request without a payment signature returns HTTP 402.
func TestX402_ModeOn_NoPaymentReturns402(t *testing.T) {
	t.Parallel()
	fac := newMockFacilitator()
	facSrv := httptest.NewServer(fac)
	defer facSrv.Close()

	cfg := x402Config(t, facSrv.URL)
	cfg.X402.Mode = x402.ModeOn

	srv := newServer(cfg)
	defer srv.Close()

	sid := initSession(t, srv.URL+cfg.MCPPath)
	resp := sendRPC(t, srv.URL+cfg.MCPPath, sid, statsCallBody(4), nil)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("mode=on no sig: status=%d want=402 body=%s", resp.StatusCode, body)
	}
	if !headerPresent(resp, paymentRequiredHeader) {
		t.Fatalf("mode=on no sig: expected %s header", paymentRequiredHeader)
	}
}

// TestX402_ModeRequired_MissingConfigReturns503 verifies that when mode=required
// and the payment config is incomplete (no facilitator URL, no resource URL),
// the server returns 503 because it cannot build a valid payment challenge.
func TestX402_ModeRequired_MissingConfigReturns503(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.X402.Mode = x402.ModeRequired
	cfg.X402.ToolsCallEnabled = true

	srv := newServer(cfg)
	defer srv.Close()

	sid := initSession(t, srv.URL+cfg.MCPPath)
	resp := sendRPC(t, srv.URL+cfg.MCPPath, sid, statsCallBody(5), nil)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("mode=required no config: status=%d want=503 body=%s", resp.StatusCode, body)
	}
}

// TestX402_ModeRequired_ValidPaymentAccepted verifies that when mode=required
// with a complete config and a valid payment proof, tools/call succeeds and
// returns HTTP 200 with a PAYMENT-RESPONSE header.
func TestX402_ModeRequired_ValidPaymentAccepted(t *testing.T) {
	t.Parallel()
	fac := newMockFacilitator()
	fac.verifyBody = `{"isValid":true}`
	fac.settleBody = `{"success":true,"transaction":"abc","network":"eip155:8453"}`
	facSrv := httptest.NewServer(fac)
	defer facSrv.Close()

	cfg := x402Config(t, facSrv.URL)
	cfg.X402.Mode = x402.ModeRequired

	srv := newServer(cfg)
	defer srv.Close()

	sid := initSession(t, srv.URL+cfg.MCPPath)
	resp := sendRPC(t, srv.URL+cfg.MCPPath, sid, statsCallBody(6), map[string]string{
		paymentSignatureHeader: validV2Signature(t),
	})
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mode=required valid payment: status=%d want=200 body=%s", resp.StatusCode, body)
	}
	if !headerPresent(resp, paymentResponseHeader) {
		t.Fatalf("mode=required valid payment: expected %s header", paymentResponseHeader)
	}
}

// TestX402_InitializeAndToolsListNotGated verifies that initialize and
// tools/list are never blocked by x402 gating regardless of mode.
func TestX402_InitializeAndToolsListNotGated(t *testing.T) {
	t.Parallel()
	fac := newMockFacilitator()
	facSrv := httptest.NewServer(fac)
	defer facSrv.Close()

	for _, mode := range []string{x402.ModeOff, x402.ModeOn, x402.ModeRequired} {
		t.Run("mode="+mode, func(t *testing.T) {
			t.Parallel()

			cfg := x402Config(t, facSrv.URL)
			cfg.X402.Mode = mode

			srv := newServer(cfg)
			defer srv.Close()

			// initialize must always succeed.
			sid := initSession(t, srv.URL+cfg.MCPPath)

			// tools/list must always succeed.
			resp := sendRPC(t, srv.URL+cfg.MCPPath, sid,
				`{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}`, nil)
			body := readBody(t, resp)

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("mode=%s tools/list: status=%d want=200 body=%s", mode, resp.StatusCode, body)
			}
			if !hasResult(body) {
				t.Fatalf("mode=%s tools/list: expected result field body=%s", mode, body)
			}
		})
	}
}

// TestX402_ModeOn_PaymentRequiredHeaderIsValidJSON verifies that when mode=on
// with complete config returns 402, the PAYMENT-REQUIRED header value is
// well-formed JSON containing x402Version and accepts fields.
func TestX402_ModeOn_PaymentRequiredHeaderIsValidJSON(t *testing.T) {
	t.Parallel()
	fac := newMockFacilitator()
	facSrv := httptest.NewServer(fac)
	defer facSrv.Close()

	cfg := x402Config(t, facSrv.URL)
	cfg.X402.Mode = x402.ModeOn

	srv := newServer(cfg)
	defer srv.Close()

	sid := initSession(t, srv.URL+cfg.MCPPath)
	resp := sendRPC(t, srv.URL+cfg.MCPPath, sid, statsCallBody(8), nil)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("payment header json: status=%d want=402 body=%s", resp.StatusCode, body)
	}

	hdr := strings.TrimSpace(resp.Header.Get(paymentRequiredHeader))
	if hdr == "" {
		t.Fatalf("payment header json: PAYMENT-REQUIRED header is empty")
	}

	// Verify it's at minimum valid JSON with the expected top-level fields.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(hdr), &raw); err != nil {
		t.Fatalf("payment header json: PAYMENT-REQUIRED is not valid JSON: %v value=%s", err, hdr)
	}
	if _, ok := raw["x402Version"]; !ok {
		t.Fatalf("payment header json: missing x402Version field value=%s", hdr)
	}
	if _, ok := raw["accepts"]; !ok {
		t.Fatalf("payment header json: missing accepts field value=%s", hdr)
	}
}
