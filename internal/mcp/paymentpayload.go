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
// validAfter/validBefore validity window. Both already exist in the v2
// PaymentPayload wire format; the adapter enforces them rather than adding new
// fields.
//
// Parsing stays best-effort and reports what it found. Deciding whether an
// absent primitive is fatal belongs to `v2ProofError`, because the answer
// depends on the configured mode: `required` fails closed, `on` is documented
// as fail-open on incomplete configuration and keeps the legacy tolerance.
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
// authorization nonce it is used verbatim, because the adapter spec requires
// replay detection to key off the client-signed nonce and NOT the raw request
// bytes.
//
// The signature-derived fallback below is for `x402.mode=on` only, and it is a
// materially weaker guarantee: an opaque proof is single-use per its own bytes
// rather than per a client-signed single-use value, and its ledger entry
// expires on the local TTL rather than with the proof. In `required` mode
// `v2ProofError` rejects a payload with no nonce before this is ever reached,
// so required mode never keys a payment off a signature hash (#699).
func replayNonce(paymentSignature string, parsed parsedPaymentPayload) string {
	if parsed.HasNonce {
		return "n:" + parsed.Nonce
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(paymentSignature)))
	return "s:" + hex.EncodeToString(sum[:])
}

// v2ProofError reports why a payment proof must be rejected before the adapter
// calls the facilitator, or "" when it may proceed.
//
// The adapter spec makes the v2 primitives normative: the authorization nonce
// "MUST be treated as single-use", "Replay detection MUST key off the
// authorization nonce", the adapter "MUST reject a proof whose
// validAfter/validBefore window does not cover the current time", and it "MUST
// enforce the matched PaymentRequirements.maxTimeoutSeconds as the maximum age
// between challenge and PAYMENT-SIGNATURE" and "MUST NOT rely on the
// facilitator alone for the time check".
//
// The implementation parsed all of that as optional. An opaque or partially
// malformed payload left HasNonce/HasWindow false, the nonce fell back to
// SHA-256 of the signature, the window check returned success, and the request
// proceeded to Verify and to tool execution even in `required` mode. A
// facilitator that approved a proof shape the adapter could not inspect
// therefore bypassed every local control, and the signature-derived ledger
// entry expired on the local TTL after which the identical timeless proof was
// classified fresh and could execute again (#699).
//
// `strict` is `x402.mode=required`. `on` keeps the previous tolerance, because
// fail-open on incomplete input is that mode's documented meaning; the
// difference between the two modes is now real rather than nominal.
//
// On maxTimeoutSeconds: the spec bounds the AGE of the proof, not its remaining
// life. The previous check measured `validBefore - now`, so a window that
// opened arbitrarily far in the past but closes soon passed unimpeded, which is
// exactly the shape a stale replayed proof has. Age is measured from
// `validAfter`, which is the only signed statement in the payload about when
// the authorization began, so strict mode requires it to be set: without it the
// age the spec mandates cannot be computed at all, and pretending otherwise is
// how the requirement stayed nominal.
func v2ProofError(parsed parsedPaymentPayload, strict bool, maxTimeoutSeconds int, now time.Time) string {
	if maxTimeoutSeconds <= 0 {
		maxTimeoutSeconds = x402.DefaultMaxTimeoutSeconds
	}
	if strict {
		if !parsed.HasNonce {
			return "payment authorization is missing a nonce"
		}
		if !parsed.HasWindow {
			return "payment authorization is missing a validity window"
		}
		if parsed.ValidAfter <= 0 {
			return "payment authorization is missing validAfter"
		}
	}
	if !parsed.HasWindow {
		return ""
	}
	if parsed.ValidBefore <= parsed.ValidAfter {
		return "payment authorization has an empty validity window"
	}
	nowUnix := now.UTC().Unix()
	if nowUnix < parsed.ValidAfter {
		return "payment authorization is not yet valid"
	}
	if nowUnix > parsed.ValidBefore {
		return "payment authorization has expired"
	}
	// The age bound, per spec. Only computable when validAfter is set; in `on`
	// mode a payload without it keeps the older remaining-time bound rather
	// than going unchecked entirely.
	if parsed.ValidAfter > 0 {
		// Age only. A separate bound on the window's total LIFETIME was written
		// first and removed: the spec mandates the age between challenge and
		// signature and nothing about how long a client's window may span, and
		// the extra rule rejected an ordinary `now-5 .. now+60` proof under a
		// 300s maximum. The age check already refuses the shape #699 names — a
		// window opened arbitrarily far in the past that closes soon — so the
		// lifetime bound bought nothing and cost working payments.
		if nowUnix-parsed.ValidAfter > int64(maxTimeoutSeconds) {
			return "payment authorization is older than the maximum validity window"
		}
	} else if parsed.ValidBefore-nowUnix > int64(maxTimeoutSeconds) {
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
