package x402

// sdk_adapter.go — adapter layer for the Coinbase x402 Go SDK.
//
// # SDK research findings (issue #110)
//
// The Coinbase x402 Go SDK lives at github.com/coinbase/x402/go (module path
// github.com/coinbase/x402/go).  Its HTTP facilitator client is in the
// sub-package github.com/coinbase/x402/go/http (HTTPFacilitatorClient).
//
// The SDK is real and well-maintained, but is NOT a drop-in replacement for
// dir2mcp's legacy client for several reasons:
//
//  1. Wire-format mismatch.  The legacy client posts to
//     /v2/x402/verify and /v2/x402/settle with body shape
//     {"paymentPayload":…,"paymentRequirements":[…]}.
//     The SDK's HTTPFacilitatorClient posts to /verify and /settle with body
//     {"x402Version":…,"paymentPayload":{…},"paymentRequirements":{…}}.
//     These are incompatible: a facilitator expecting the SDK format will reject
//     legacy requests and vice-versa.
//
//  2. Heavy transitive dependencies.  The SDK module requires
//     github.com/ethereum/go-ethereum, github.com/gagliardetto/solana-go, and
//     gin — totaling many megabytes of chain-library code.  dir2mcp is a lean
//     binary and importing the full SDK solely for its HTTP plumbing is
//     disproportionate.
//
//  3. API shape mismatch.  The SDK's Verify/Settle methods accept raw
//     []byte payloads that represent serialized SDK-specific structs
//     (types.PaymentPayload, types.PaymentRequirements), whereas dir2mcp's
//     FacilitatorClient accepts a plain string signature and a Requirement
//     struct.  A non-trivial translation layer would be required.
//
//  4. Response schema mismatch.  The SDK returns typed *VerifyResponse /
//     *SettleResponse structs; dir2mcp's FacilitatorClient returns
//     json.RawMessage (the raw facilitator body) so that payment.go can log
//     and forward it unchanged.
//
// # Design decision
//
// Rather than adding the SDK as a direct dependency now, this file establishes
// the adapter skeleton that makes the eventual migration a contained,
// low-risk change:
//
//   - The FacilitatorClient interface (see facilitator.go) is the sole
//     contract consumed by payment.go.
//   - NewFacilitatorClient is the single construction point, selected by the
//     X402_CLIENT environment variable (values: "legacy" or "sdk").
//   - sdkAdapterClient is a typed stub that makes the SDK code-path visible
//     at compile time and returns a clear error if activated before the SDK
//     adapter is fully implemented.
//
// When the SDK integration is ready, the body of sdkAdapterClient.Verify and
// sdkAdapterClient.Settle should be replaced with calls to the SDK's
// HTTPFacilitatorClient.  The feature flag wiring and interface boundary are
// already in place — the swap should require no changes outside this file.
//
// # Feature flag
//
// Set X402_CLIENT=sdk to activate the SDK path (currently returns
// CodePaymentConfigInvalid to prevent accidental use in production).
// The default (X402_CLIENT unset or "legacy") uses the battle-tested
// HTTPClient.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
)

const (
	// X402ClientEnvVar is the environment variable that selects the
	// facilitator client implementation.  Accepted values are "legacy"
	// (default) and "sdk".
	X402ClientEnvVar = "X402_CLIENT"

	// X402ClientLegacy selects the hand-rolled HTTPClient (default).
	X402ClientLegacy = "legacy"

	// X402ClientSDK selects the Coinbase x402 SDK adapter.  This value is
	// reserved for future use; activating it currently returns an explicit
	// error explaining that the adapter is pending SDK integration.
	X402ClientSDK = "sdk"
)

// NewFacilitatorClient constructs a FacilitatorClient according to the
// X402_CLIENT environment variable.
//
//   - "legacy" or unset → &HTTPClient (existing hand-rolled client)
//   - "sdk"             → &sdkAdapterClient (SDK adapter stub)
//   - any other value   → &HTTPClient (falls back to legacy with no error)
//
// The httpClient argument is forwarded to the legacy implementation; pass nil
// to use the package default timeout.
func NewFacilitatorClient(baseURL, bearerToken string, httpClient *http.Client) FacilitatorClient {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(X402ClientEnvVar)))
	if raw == X402ClientSDK {
		return &sdkAdapterClient{
			baseURL:     baseURL,
			bearerToken: bearerToken,
		}
	}
	// "legacy", "", or any unrecognised value → use the existing HTTP client.
	return NewHTTPClient(baseURL, bearerToken, httpClient)
}

// sdkAdapterClient is a placeholder that satisfies FacilitatorClient.
// It is instantiated when X402_CLIENT=sdk and communicates to the operator
// (via a structured FacilitatorError) that the SDK integration is pending.
//
// To complete the SDK migration, replace the bodies of Verify and Settle with
// calls to github.com/coinbase/x402/go/http.HTTPFacilitatorClient after
// resolving the wire-format and dependency concerns documented at the top of
// this file.
type sdkAdapterClient struct {
	baseURL     string
	bearerToken string
}

func (c *sdkAdapterClient) Verify(ctx context.Context, paymentSignature string, req Requirement) (json.RawMessage, error) {
	return nil, &FacilitatorError{
		Operation: "verify",
		Code:      CodePaymentConfigInvalid,
		Message:   "X402_CLIENT=sdk is not yet fully implemented; set X402_CLIENT=legacy or unset to use the default client",
		Retryable: false,
	}
}

func (c *sdkAdapterClient) Settle(ctx context.Context, paymentSignature string, req Requirement) (json.RawMessage, error) {
	return nil, &FacilitatorError{
		Operation: "settle",
		Code:      CodePaymentConfigInvalid,
		Message:   "X402_CLIENT=sdk is not yet fully implemented; set X402_CLIENT=legacy or unset to use the default client",
		Retryable: false,
	}
}
