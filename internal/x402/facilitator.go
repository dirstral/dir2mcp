package x402

import (
	"context"
	"encoding/json"
)

// FacilitatorClient is the interface satisfied by all x402 facilitator client
// implementations.  payment.go depends only on this interface, so the
// underlying transport (legacy HTTP, SDK-backed, mock) can be swapped without
// changing any call sites.
//
// Verify checks that a payment signature is valid for the given requirement.
// Settle instructs the facilitator to settle (finalize) the payment.
// Both operations return the raw facilitator response body as json.RawMessage
// so that callers can log and forward it without needing to understand the
// facilitator-specific schema.
type FacilitatorClient interface {
	Verify(ctx context.Context, paymentSignature string, req Requirement) (json.RawMessage, error)
	Settle(ctx context.Context, paymentSignature string, req Requirement) (json.RawMessage, error)
}
