package x402_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"dir2mcp/internal/x402"
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
	facServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	client := x402.NewFacilitatorClient(facServer.URL, "token", nil)
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
