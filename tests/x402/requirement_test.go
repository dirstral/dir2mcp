package x402_test

import (
	"testing"

	"dir2mcp/internal/x402"
)

func TestRequirementNormalize(t *testing.T) {
	original := x402.Requirement{
		Scheme:            " Exact ",
		Network:           " eip155:8453 ",
		Amount:            " 42 ",
		MaxAmountRequired: " 50 ",
		Asset:             " usdc ",
		PayTo:             " 0xabc ",
		Resource:          " https://example.com/mcp ",
	}

	normalized := original.Normalize()

	if normalized.Scheme != "exact" {
		t.Fatalf("scheme=%q want=%q", normalized.Scheme, "exact")
	}
	if normalized.Network != "eip155:8453" {
		t.Fatalf("network=%q want=%q", normalized.Network, "eip155:8453")
	}
	if normalized.Amount != "42" {
		t.Fatalf("amount=%q want=%q", normalized.Amount, "42")
	}
	if normalized.MaxAmountRequired != "50" {
		t.Fatalf("maxAmountRequired=%q want=%q", normalized.MaxAmountRequired, "50")
	}
	if normalized.Asset != "usdc" {
		t.Fatalf("asset=%q want=%q", normalized.Asset, "usdc")
	}
	if normalized.PayTo != "0xabc" {
		t.Fatalf("payTo=%q want=%q", normalized.PayTo, "0xabc")
	}
	if normalized.Resource != "https://example.com/mcp" {
		t.Fatalf("resource=%q want=%q", normalized.Resource, "https://example.com/mcp")
	}

	if original.Scheme != " Exact " {
		t.Fatalf("Normalize should not mutate receiver copy; original scheme=%q", original.Scheme)
	}
}
