package x402

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

const (
	HeaderPaymentRequired  = "PAYMENT-REQUIRED"
	HeaderPaymentSignature = "PAYMENT-SIGNATURE"
	HeaderPaymentResponse  = "PAYMENT-RESPONSE"

	ModeOff      = "off"
	ModeOn       = "on"
	ModeRequired = "required"

	CodePaymentRequired               = "PAYMENT_REQUIRED"
	CodePaymentInvalid                = "PAYMENT_INVALID"
	CodePaymentFacilitatorUnavailable = "PAYMENT_FACILITATOR_UNAVAILABLE"
	CodePaymentSettlementFailed       = "PAYMENT_SETTLEMENT_FAILED"
	CodePaymentSettlementUnavailable  = "PAYMENT_SETTLEMENT_UNAVAILABLE"
	CodePaymentConfigInvalid          = "PAYMENT_CONFIG_INVALID"

	// X402Version is the current protocol version encoded in the
	// PAYMENT-REQUIRED header payload.  It's a small integer that
	// allows clients/servers to evolve the format in a backwards-
	// compatible way, and having it as a constant makes updates
	// straightforward.
	X402Version = 2
)

type Requirement struct {
	Scheme            string
	Network           string
	Amount            string
	MaxAmountRequired string
	Asset             string
	PayTo             string
	Resource          string
}

// X402Payload represents the JSON object returned in the PAYMENT-REQUIRED header.
// It mirrors the previous map structure but provides compile-time safety.
//
// Consumers depend on the field names to match the existing keys, so the tags
// are chosen accordingly.
type X402Payload struct {
	X402Version int           `json:"x402Version"`
	Accept      []AcceptEntry `json:"accepts"`
}

// AcceptEntry describes a single acceptable payment requirement.
// The json tags match the keys previously used in the map literal.
type AcceptEntry struct {
	Scheme            string `json:"scheme"`
	Network           string `json:"network"`
	Amount            string `json:"amount"`
	MaxAmountRequired string `json:"maxAmountRequired,omitempty"`
	Asset             string `json:"asset"`
	PayTo             string `json:"payTo"`
	Resource          string `json:"resource"`
}

const allowedSchemesText = "exact, upto"

func (r Requirement) Normalize() Requirement {
	r.Scheme = strings.ToLower(strings.TrimSpace(r.Scheme))
	r.Network = strings.TrimSpace(r.Network)
	r.Amount = strings.TrimSpace(r.Amount)
	r.MaxAmountRequired = strings.TrimSpace(r.MaxAmountRequired)
	r.Asset = strings.TrimSpace(r.Asset)
	r.PayTo = strings.TrimSpace(r.PayTo)
	r.Resource = strings.TrimSpace(r.Resource)
	return r
}

func (r Requirement) Validate() error {
	r = r.Normalize()
	if r.Scheme == "" {
		return fmt.Errorf("x402 scheme is required")
	}
	switch r.Scheme {
	case "exact", "upto":
	default:
		return fmt.Errorf("x402 scheme must be one of: %s", allowedSchemesText)
	}
	if r.Network == "" {
		return fmt.Errorf("x402 network is required")
	}
	if !IsCAIP2Network(r.Network) {
		return fmt.Errorf("x402 network must be CAIP-2")
	}
	// amount must be a non-empty positive integer.
	if r.Amount == "" {
		return fmt.Errorf("x402 amount is required")
	}
	value := new(big.Int)
	if _, ok := value.SetString(r.Amount, 10); !ok || value.Sign() <= 0 {
		return fmt.Errorf("x402 amount must be a positive integer")
	}

	// For "upto" scheme we also require a max value which must be a
	// positive integer and not smaller than amount.  The MaxAmountRequired
	// field may be empty for "exact" since it is ignored.
	if r.Scheme == "upto" {
		if r.MaxAmountRequired == "" {
			return fmt.Errorf("x402 maxAmountRequired is required for upto scheme")
		}
		maxVal := new(big.Int)
		if _, ok := maxVal.SetString(r.MaxAmountRequired, 10); !ok || maxVal.Sign() <= 0 {
			return fmt.Errorf("x402 maxAmountRequired must be a positive integer")
		}
		if maxVal.Cmp(value) < 0 {
			return fmt.Errorf("x402 maxAmountRequired must be >= amount")
		}
	}
	if r.Asset == "" {
		return fmt.Errorf("x402 asset is required")
	}
	if r.PayTo == "" {
		return fmt.Errorf("x402 payTo is required")
	}
	if r.Resource == "" {
		return fmt.Errorf("x402 resource is required")
	}
	return nil
}

// BuildPaymentRequiredHeaderValue returns a machine-readable challenge payload
// suitable for the PAYMENT-REQUIRED header.
func BuildPaymentRequiredHeaderValue(req Requirement) (string, error) {
	req = req.Normalize()
	if err := req.Validate(); err != nil {
		return "", err
	}

	// assemble a typed payload rather than a loose map; this aids
	// compile-time checking and prevents inadvertent typos.
	p := X402Payload{
		X402Version: X402Version,
		Accept: []AcceptEntry{
			{
				Scheme:            req.Scheme,
				Network:           req.Network,
				Amount:            req.Amount,
				MaxAmountRequired: req.MaxAmountRequired,
				Asset:             req.Asset,
				PayTo:             req.PayTo,
				Resource:          req.Resource,
			},
		},
	}

	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func NormalizeMode(mode string) string {
	// Normalize user-supplied mode strings to one of the canonical
	// constants.  This helper is used at runtime during configuration
	// to decide whether payment gating is enabled, so it's important
	// that the return value is always a valid mode.  Previously the
	// function would return unknown values lowercased which meant
	// callers such as IsModeValid could disagree with each other.
	//
	// After trimming and lowercasing we map anything that isn't
	// explicitly recognised back to ModeOff.  Callers that need to
	// validate the original input should use IsModeValid instead.
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case "", ModeOff:
		return ModeOff
	case ModeOn:
		return ModeOn
	case ModeRequired:
		return ModeRequired
	default:
		// unrecognised values fall back to off rather than leaking
		// arbitrary strings; this keeps the output in the set of
		// known modes and avoids surprising callers.
		return ModeOff
	}
}

func IsModeValid(mode string) bool {
	// Validate the raw input rather than normalising it.  With the
	// new behaviour of NormalizeMode the previous implementation would
	// always return true, which made the helper useless.  Keeping this
	// logic separate lets callers detect and reject bad configuration
	// while still using NormalizeMode for runtime decisions.
	m := strings.ToLower(strings.TrimSpace(mode))
	switch m {
	case ModeOff, ModeOn, ModeRequired:
		return true
	default:
		return false
	}
}

func IsModeEnabled(mode string) bool {
	switch NormalizeMode(mode) {
	case ModeOn, ModeRequired:
		return true
	default:
		return false
	}
}

// IsCAIP2Network validates a conservative CAIP-2 identifier shape:
// <namespace>:<reference>
func IsCAIP2Network(network string) bool {
	network = strings.TrimSpace(network)
	parts := strings.Split(network, ":")
	if len(parts) != 2 {
		return false
	}

	ns := parts[0]
	ref := parts[1]
	if len(ns) == 0 || len(ns) > 32 || len(ref) == 0 || len(ref) > 128 {
		return false
	}

	for _, r := range ns {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	for _, r := range ref {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

type FacilitatorError struct {
	Operation  string
	StatusCode int
	Retryable  bool
	Code       string
	Message    string
	Body       string
	Cause      error
}

func (e *FacilitatorError) Error() string {
	if e == nil {
		return "<nil FacilitatorError>"
	}
	if e.Code == "" && e.Message == "" {
		return "facilitator request failed"
	}
	if e.Code == "" {
		return e.Message
	}
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

func (e *FacilitatorError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
