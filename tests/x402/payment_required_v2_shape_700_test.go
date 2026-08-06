package x402

import (
	"encoding/json"
	"testing"

	sdktypes "github.com/x402-foundation/x402/go/types"

	"github.com/dirstral/dir2mcp/internal/x402"
)

// #700: the PAYMENT-REQUIRED header advertised `x402Version: 2` while emitting
// a shape that is not a v2 `PaymentRequired`. `resource` sat inside each accept
// entry as a bare string and there was no top-level resource object, though the
// v2 SDK already vendored in this module defines `PaymentRequired` as
// x402Version + a first-class `resource *ResourceInfo` + `accepts`, whose
// entries carry no `resource` field at all. SPEC §18 requires the standard
// shape in as many words.
//
// It matters beyond tidiness: the adapter binds a proof to the challenge
// `resource` and refuses one issued for another route, so the field a client
// must read in order to comply was not where the format says it is.
//
// Nothing asserted the header's shape before this file, which is why it
// shipped: every existing x402 test checks status codes, headers being present,
// and settlement behaviour, never the object itself.

func requirement() x402.Requirement {
	return x402.Requirement{
		Scheme:            "exact",
		Network:           "eip155:8453",
		Amount:            "1000",
		MaxAmountRequired: "1000",
		Asset:             "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48",
		PayTo:             "0x1111111111111111111111111111111111111111",
		Resource:          "https://resource.example.com/mcp",
		MaxTimeoutSeconds: 300,
	}
}

// TestPaymentRequiredParsesAsTheSDKsOwnV2Type is the assertion that matters: a
// client using the official types must be able to read what we emit. Anything
// weaker just re-describes our own struct back to itself.
func TestPaymentRequiredParsesAsTheSDKsOwnV2Type(t *testing.T) {
	raw, err := x402.BuildPaymentRequiredHeaderValue(requirement())
	if err != nil {
		t.Fatalf("BuildPaymentRequiredHeaderValue: %v", err)
	}

	var got sdktypes.PaymentRequired
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("the header does not parse as the SDK's PaymentRequired: %v\nraw=%s", err, raw)
	}

	if got.X402Version != 2 {
		t.Fatalf("x402Version = %d, want 2", got.X402Version)
	}
	if got.Resource == nil {
		t.Fatalf("no top-level resource; a v2 client reads the resource from there, and the adapter binds proofs to it\nraw=%s", raw)
	}
	if got.Resource.URL != "https://resource.example.com/mcp" {
		t.Fatalf("resource.url = %q, want the configured resource", got.Resource.URL)
	}
	if len(got.Accepts) != 1 {
		t.Fatalf("accepts has %d entries, want 1", len(got.Accepts))
	}
	entry := got.Accepts[0]
	if entry.Scheme != "exact" || entry.Network != "eip155:8453" {
		t.Fatalf("accepts[0] scheme/network = %q/%q", entry.Scheme, entry.Network)
	}
	if entry.Amount != "1000" || entry.PayTo == "" || entry.Asset == "" {
		t.Fatalf("accepts[0] lost a required field: %+v", entry)
	}
	if entry.MaxTimeoutSeconds != 300 {
		t.Fatalf("accepts[0] maxTimeoutSeconds = %d, want 300", entry.MaxTimeoutSeconds)
	}
}

// TestAcceptEntriesCarryNoResource pins the half that was wrong. The SDK's
// PaymentRequirements has no such field, so a `resource` inside an accept entry
// is not merely redundant: it is not part of the format being advertised.
func TestAcceptEntriesCarryNoResource(t *testing.T) {
	raw, err := x402.BuildPaymentRequiredHeaderValue(requirement())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var envelope struct {
		Accepts []map[string]interface{} `json:"accepts"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(envelope.Accepts) != 1 {
		t.Fatalf("accepts has %d entries", len(envelope.Accepts))
	}
	if _, present := envelope.Accepts[0]["resource"]; present {
		t.Fatalf("accepts[0] still carries a resource field: %v", envelope.Accepts[0])
	}
}

// TestOurNonStandardFieldTravelsInExtra: `maxAmountRequired` is this adapter's
// field, not the standard's — the canonical spec never mentions it — but the
// request fingerprint the replay ledger binds to includes it, so it has to stay
// on the wire. `extra` is the SDK's own escape hatch for exactly this.
func TestOurNonStandardFieldTravelsInExtra(t *testing.T) {
	raw, err := x402.BuildPaymentRequiredHeaderValue(requirement())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var envelope struct {
		Accepts []struct {
			MaxAmountRequired interface{}            `json:"maxAmountRequired"`
			Extra             map[string]interface{} `json:"extra"`
		} `json:"accepts"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	entry := envelope.Accepts[0]
	if entry.MaxAmountRequired != nil {
		t.Fatalf("maxAmountRequired is still a top-level accept field: %v", entry.MaxAmountRequired)
	}
	if got := entry.Extra["maxAmountRequired"]; got != "1000" {
		t.Fatalf("extra.maxAmountRequired = %v, want \"1000\"", got)
	}
}

// TestAMissingResourceIsRefusedRatherThanAdvertisedEmpty: the resource is
// REQUIRED, not optional — `Validate` rejects a requirement without one, so a
// challenge can never advertise an empty resource. That is the stronger
// guarantee: the adapter binds proofs to this value, and a client binding to ""
// would be worse than no challenge at all.
//
// The nil guard in the builder is therefore defensive rather than reachable,
// and this test records which of the two actually holds the line.
func TestAMissingResourceIsRefusedRatherThanAdvertisedEmpty(t *testing.T) {
	req := requirement()
	req.Resource = ""
	if _, err := x402.BuildPaymentRequiredHeaderValue(req); err == nil {
		t.Fatal("a requirement with no resource produced a challenge; it must be refused")
	}
}
