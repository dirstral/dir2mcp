package mcp

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/x402"
)

// parsedPaymentPayload carries the x402 v2 primitives the adapter enforces
// server-side: the client's single-use authorization nonce and its
// validAfter/validBefore validity window. These fields already exist in the
// x402 v2 PaymentPayload wire format; this revision enforces them rather than
// adding new fields. Fields are best-effort: an opaque or legacy PAYMENT-SIGNATURE
// that carries no authorization leaves HasNonce/HasWindow false and the caller
// falls back to a signature-derived nonce with no window check.
type parsedPaymentPayload struct {
	Nonce       string
	HasNonce    bool
	ValidAfter  int64
	ValidBefore int64
	HasWindow   bool
}

// parsePaymentPayload extracts the authorization nonce and validity window from
// a PAYMENT-SIGNATURE header value. The value may be raw JSON, base64-encoded
// JSON, or an opaque token; the first two are inspected for
// payload.authorization.{nonce,validAfter,validBefore}.
func parsePaymentPayload(paymentSignature string) parsedPaymentPayload {
	trimmed := strings.TrimSpace(paymentSignature)
	out := parsedPaymentPayload{}
	if trimmed == "" {
		return out
	}

	raw := decodePaymentPayloadJSON(trimmed)
	if raw == nil {
		return out
	}

	var envelope struct {
		Payload struct {
			Authorization struct {
				Nonce       string          `json:"nonce"`
				ValidAfter  json.RawMessage `json:"validAfter"`
				ValidBefore json.RawMessage `json:"validBefore"`
			} `json:"authorization"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return out
	}

	auth := envelope.Payload.Authorization
	if nonce := strings.TrimSpace(auth.Nonce); nonce != "" {
		out.Nonce = nonce
		out.HasNonce = true
	}
	validAfter, okA := parseUnixSeconds(auth.ValidAfter)
	validBefore, okB := parseUnixSeconds(auth.ValidBefore)
	if okA && okB {
		out.ValidAfter = validAfter
		out.ValidBefore = validBefore
		out.HasWindow = true
	}
	return out
}

// decodePaymentPayloadJSON returns the JSON bytes of a PAYMENT-SIGNATURE value,
// accepting either raw JSON or base64-encoded JSON. It returns nil for an opaque
// token that is neither.
func decodePaymentPayloadJSON(trimmed string) []byte {
	if json.Valid([]byte(trimmed)) {
		return []byte(trimmed)
	}
	if decoded, err := base64.StdEncoding.DecodeString(trimmed); err == nil && json.Valid(decoded) {
		return decoded
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(trimmed); err == nil && json.Valid(decoded) {
		return decoded
	}
	return nil
}

// parseUnixSeconds interprets an x402 validity-window field, which may be encoded
// as a JSON number or a decimal string of whole seconds.
func parseUnixSeconds(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var asNumber json.Number
	if err := json.Unmarshal(raw, &asNumber); err == nil {
		if v, err := asNumber.Int64(); err == nil {
			return v, true
		}
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		asString = strings.TrimSpace(asString)
		if asString == "" {
			return 0, false
		}
		if v, err := strconv.ParseInt(asString, 10, 64); err == nil {
			return v, true
		}
	}
	return 0, false
}

// replayNonce returns the ledger key for a payment. When the payload carries an
// authorization nonce it is used verbatim (single-use replay keys off the
// client-signed nonce, per spec). Otherwise a deterministic signature-derived
// fallback is used so an opaque/legacy signature is still single-use per its own
// bytes (never keyed off the raw request bytes, so retry-vs-replay classification
// still holds).
func replayNonce(paymentSignature string, parsed parsedPaymentPayload) string {
	if parsed.HasNonce {
		return "n:" + parsed.Nonce
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(paymentSignature)))
	return "s:" + hex.EncodeToString(sum[:])
}

// validityWindowError reports whether the payment's validity window fails to
// cover now, or exceeds the matched maxTimeoutSeconds. It returns a non-empty
// reason string when the proof must be rejected. When the payload carries no
// window (legacy/opaque signature) it returns "" (no enforcement possible).
func validityWindowError(parsed parsedPaymentPayload, maxTimeoutSeconds int, now time.Time) string {
	if !parsed.HasWindow {
		return ""
	}
	nowUnix := now.UTC().Unix()
	if nowUnix < parsed.ValidAfter {
		return "payment authorization is not yet valid"
	}
	if nowUnix > parsed.ValidBefore {
		return "payment authorization has expired"
	}
	if maxTimeoutSeconds <= 0 {
		maxTimeoutSeconds = x402.DefaultMaxTimeoutSeconds
	}
	// Bound the remaining validity of the proof by the matched maxTimeoutSeconds:
	// a client following the challenge sets validBefore ≈ now + maxTimeoutSeconds,
	// so a window that stays valid far longer than the advertised maximum is
	// rejected. Measured against validBefore (not validAfter) so the common
	// validAfter=0 encoding is not false-rejected.
	if parsed.ValidBefore-nowUnix > int64(maxTimeoutSeconds) {
		return "payment authorization exceeds the maximum validity window"
	}
	return ""
}

// canonicalPaymentRequestKey binds the idempotency/replay key to the logical
// request: the canonicalized tool-call params plus a fingerprint of the entire
// selected PaymentRequirements (not scheme+network alone). Canonicalizing the
// params first means two semantically identical calls that differ only in JSON
// whitespace or key order dedupe to the same key (no double-charge, no dedup
// bypass).
func canonicalPaymentRequestKey(rawParams json.RawMessage, req x402.Requirement) string {
	canonicalParams := canonicalizeJSON(rawParams)
	fingerprint := requirementFingerprint(req)
	sum := sha256.Sum256(append(append([]byte(canonicalParams), '|'), fingerprint...))
	return hex.EncodeToString(sum[:])
}

// canonicalizeJSON returns a stable serialization of a JSON document: decoding
// then re-encoding sorts object keys (encoding/json marshals map keys in sorted
// order) and normalizes whitespace. On any decode error it falls back to the
// trimmed raw bytes so a non-JSON payload still hashes deterministically.
func canonicalizeJSON(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return ""
	}
	var v interface{}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return trimmed
	}
	out, err := json.Marshal(v)
	if err != nil {
		return trimmed
	}
	return string(out)
}

// requirementFingerprint is a stable string identity of a PaymentRequirements
// object used to bind proofs to a specific resource/price/route.
func requirementFingerprint(req x402.Requirement) []byte {
	req = req.Normalize()
	parts := []string{
		req.Scheme,
		req.Network,
		req.Amount,
		req.MaxAmountRequired,
		req.Asset,
		req.PayTo,
		req.Resource,
		strconv.Itoa(req.MaxTimeoutSeconds),
	}
	return []byte(strings.Join(parts, "\x1f"))
}
