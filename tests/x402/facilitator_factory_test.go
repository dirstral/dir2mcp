package x402_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dirstral/dir2mcp/internal/x402"
)

func validRequirement() x402.Requirement {
	return x402.Requirement{
		Scheme:            "exact",
		Network:           "eip155:8453",
		Amount:            "1",
		MaxAmountRequired: "1",
		Asset:             "usdc",
		PayTo:             "0x1111111111111111111111111111111111111111",
		Resource:          "https://example.com/mcp",
	}
}

func TestNewFacilitatorClient_UsesSDKFacilitatorClient(t *testing.T) {
	var verifyCalls, settleCalls int
	var verifyAuthorization, settleAuthorization string
	// A bearer token is attached, so the adapter requires https transport
	// (bs-010 / x402 adapter spec): use a TLS test server and its trusting
	// client. Plaintext http with a credential is a configuration error.
	facServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/x402/verify":
			verifyCalls++
			verifyAuthorization = r.Header.Get("Authorization")
			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"isValid": true,
				"payer":   "payer-1",
			})
		case "/v2/x402/settle":
			settleCalls++
			settleAuthorization = r.Header.Get("Authorization")
			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success":     true,
				"transaction": "tx-1",
				"network":     "eip155:8453",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer facServer.Close()

	client := x402.NewFacilitatorClient(facServer.URL, "token", facServer.Client())
	paymentPayload := `{"x402Version":2,"payload":{"paymentSignature":"sig"}}`

	verifyRaw, err := client.Verify(context.Background(), paymentPayload, validRequirement())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verifyCalls != 1 {
		t.Fatalf("verify calls=%d want=1", verifyCalls)
	}

	settleRaw, err := client.Settle(context.Background(), paymentPayload, validRequirement())
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if settleCalls != 1 {
		t.Fatalf("settle calls=%d want=1", settleCalls)
	}
	if verifyAuthorization != "Bearer token" {
		t.Fatalf("verify authorization header=%q want=%q", verifyAuthorization, "Bearer token")
	}
	if settleAuthorization != "Bearer token" {
		t.Fatalf("settle authorization header=%q want=%q", settleAuthorization, "Bearer token")
	}

	var verifyResp map[string]any
	if err := json.Unmarshal(verifyRaw, &verifyResp); err != nil {
		t.Fatalf("decode verify response: %v body=%s", err, string(verifyRaw))
	}
	if got := verifyResp["payer"]; got != "payer-1" {
		t.Fatalf("verify payer=%v want=%q", got, "payer-1")
	}

	var settleResp map[string]any
	if err := json.Unmarshal(settleRaw, &settleResp); err != nil {
		t.Fatalf("decode settle response: %v body=%s", err, string(settleRaw))
	}
	if got := settleResp["transaction"]; got != "tx-1" {
		t.Fatalf("settle transaction=%v want=%q", got, "tx-1")
	}
}

// TestNewFacilitatorClient_TransportSecurity verifies the adapter refuses to
// build a live facilitator client that would send a credential or reach a
// non-loopback host over plaintext http (bs-010). Such a client fails closed
// with PAYMENT_CONFIG_INVALID rather than leaking the token.
func TestNewFacilitatorClient_TransportSecurity(t *testing.T) {
	cases := []struct {
		name       string
		url        string
		token      string
		wantConfig bool // expect PAYMENT_CONFIG_INVALID (client built with no URL)
	}{
		{name: "credentialed http loopback rejected", url: "http://127.0.0.1:9000", token: "tok", wantConfig: true},
		{name: "non-loopback http rejected", url: "http://facilitator.example.com", token: "", wantConfig: true},
		{name: "loopback http no credential allowed", url: "http://127.0.0.1:9000", token: "", wantConfig: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := x402.NewFacilitatorClient(tc.url, tc.token, nil)
			_, err := client.Verify(context.Background(), `{"x402Version":2,"payload":{}}`, validRequirement())
			if err == nil {
				// A loopback no-credential client will attempt a real request and
				// fail on connection, not config — that is acceptable here.
				if tc.wantConfig {
					t.Fatalf("expected PAYMENT_CONFIG_INVALID for %q", tc.url)
				}
				return
			}
			facErr, ok := err.(*x402.FacilitatorError)
			if !ok {
				t.Fatalf("expected *x402.FacilitatorError, got %T: %v", err, err)
			}
			isConfig := facErr.Code == x402.CodePaymentConfigInvalid
			if tc.wantConfig && !isConfig {
				t.Fatalf("expected PAYMENT_CONFIG_INVALID for %q, got %q: %v", tc.url, facErr.Code, err)
			}
			if !tc.wantConfig && isConfig {
				t.Fatalf("did not expect PAYMENT_CONFIG_INVALID for %q, got: %v", tc.url, err)
			}
		})
	}
}
