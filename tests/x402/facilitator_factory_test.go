package x402_test

import (
	"context"
	"errors"
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

func TestNewFacilitatorClient_DefaultsToLegacy(t *testing.T) {
	t.Setenv(x402.X402ClientEnvVar, "")
	client := x402.NewFacilitatorClient("https://facilitator.test", "token", nil)
	if _, ok := client.(*x402.HTTPClient); !ok {
		t.Fatalf("expected *x402.HTTPClient, got %T", client)
	}
}

func TestNewFacilitatorClient_UnknownFallsBackToLegacy(t *testing.T) {
	t.Setenv(x402.X402ClientEnvVar, "mystery")
	client := x402.NewFacilitatorClient("https://facilitator.test", "token", nil)
	if _, ok := client.(*x402.HTTPClient); !ok {
		t.Fatalf("expected *x402.HTTPClient fallback, got %T", client)
	}
}

func TestNewFacilitatorClient_SDKReturnsConfigInvalidError(t *testing.T) {
	t.Setenv(x402.X402ClientEnvVar, x402.X402ClientSDK)
	client := x402.NewFacilitatorClient("https://facilitator.test", "token", nil)

	_, err := client.Verify(context.Background(), "sig", validRequirement())
	if err == nil {
		t.Fatal("expected sdk adapter verify error")
	}
	var facErr *x402.FacilitatorError
	if !errors.As(err, &facErr) {
		t.Fatalf("expected FacilitatorError, got %T", err)
	}
	if facErr.Code != x402.CodePaymentConfigInvalid {
		t.Fatalf("code=%q want=%q", facErr.Code, x402.CodePaymentConfigInvalid)
	}
	if facErr.Retryable {
		t.Fatalf("expected non-retryable error for sdk stub")
	}
}
