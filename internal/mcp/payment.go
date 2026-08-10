package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dirstral/dir2mcp/internal/statefs"
	storepkg "github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/internal/x402"
)

type paymentExecutionOutcome struct {
	StatusCode      int
	Result          *toolCallResult
	RPCError        *rpcError
	RequiresSettle  bool
	Settled         bool
	PaymentResponse string
	UpdatedAt       time.Time
	// ExpiresAt aligns the cached outcome's lifetime with its nonce ledger entry
	// so an idempotent retry can re-surface the recorded outcome for as long as
	// the nonce stays consumed. Without this the outcome could be pruned (fixed
	// 10-min TTL) while the nonce remains consumed (validity-window TTL), causing
	// a legitimate retry to be rejected as "nonce already used".
	ExpiresAt time.Time
}

type keyMutex struct {
	mu  sync.Mutex
	ref int
}

func (s *Server) initPaymentConfig() {
	mode := x402.NormalizeMode(s.cfg.X402.Mode)
	if !x402.IsModeEnabled(mode) || !s.cfg.X402.ToolsCallEnabled {
		return
	}

	// In "on" mode we fail open: if strict payment config is incomplete,
	// keep tools/call ungated instead of enabling a runtime-bricking gate.
	if mode == x402.ModeOn {
		if err := s.cfg.ValidateX402(true); err != nil {
			// validation failed; log a warning so operators understand why x402
			// isn't being enabled.  We still return early to avoid enabling the
			// payment gate.
			s.emitPaymentEvent("warning", "x402_validation_failed", map[string]interface{}{
				"err": err.Error(),
			})
			return
		}
	}

	// Transport security is a hard, non-degradable condition (bs-010 / adapter
	// spec): even in fail-open "on" mode we refuse to enable gating over a
	// credentialed or non-loopback plaintext-http facilitator URL, because that
	// would leak the bearer token / payment payload. Emit a loud error and leave
	// the gate off rather than silently degrade. (The CLI `up` path enforces the
	// same via ValidateX402 and hard-fails startup before we get here.)
	if err := s.cfg.X402FacilitatorTransportError(); err != nil {
		s.emitPaymentEvent("error", "x402_transport_insecure", map[string]interface{}{
			"err": err.Error(),
		})
		return
	}

	s.x402Requirement = x402.Requirement{
		Scheme:  strings.TrimSpace(s.cfg.X402.Scheme),
		Network: strings.TrimSpace(s.cfg.X402.Network),
		Amount:  strings.TrimSpace(s.cfg.X402.PriceAtomic),
		// MaxAmountRequired intentionally uses a separate field; by default we
		// mirror the configured price but callers (or future config) may set
		// a larger upper bound for "upto" schemes.
		MaxAmountRequired: strings.TrimSpace(s.cfg.X402.PriceAtomic),
		Asset:             strings.TrimSpace(s.cfg.X402.Asset),
		PayTo:             strings.TrimSpace(s.cfg.X402.PayTo),
		Resource:          strings.TrimSpace(buildPaymentResourceURL(s.cfg.X402.ResourceBaseURL, s.cfg.MCPPath)),
		MaxTimeoutSeconds: x402.DefaultMaxTimeoutSeconds,
	}
	// Allow a test/embedding seam to inject the facilitator client
	// (WithX402Client); otherwise build one from config.
	if s.x402Client == nil {
		s.x402Client = x402.NewFacilitatorClient(s.cfg.X402.FacilitatorURL, s.cfg.X402.FacilitatorToken, nil)
	}
	s.x402Enabled = true
	s.paymentLogPath = filepath.Join(s.cfg.StateDir, "payments", "settlement.log")
}

func buildPaymentResourceURL(baseURL, mcpPath string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return ""
	}
	if !strings.HasPrefix(mcpPath, "/") {
		mcpPath = "/" + mcpPath
	}
	return baseURL + mcpPath
}

func (s *Server) handleToolsCallRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, rawParams json.RawMessage, id interface{}) {
	if !s.x402Enabled {
		s.handleToolsCall(ctx, w, rawParams, id)
		return
	}

	paymentSignature := strings.TrimSpace(r.Header.Get(x402.HeaderPaymentSignature))
	if paymentSignature == "" {
		s.emitPaymentEvent("info", "payment_required", map[string]interface{}{
			"reason": "missing_payment_signature",
		})
		s.writePaymentChallenge(w, id, x402.CodePaymentRequired, "payment required", false)
		return
	}

	now := time.Now().UTC()
	parsed := parsePaymentPayload(paymentSignature)

	// The v2 primitives the adapter spec makes normative: a client-signed
	// single-use nonce and a validity window whose age is bounded by the
	// matched maxTimeoutSeconds. Checked BEFORE the facilitator call, because
	// "MUST NOT rely on the facilitator alone for the time check" and because a
	// facilitator that approves a proof the adapter cannot inspect would
	// otherwise bypass every local control (#699).
	//
	// Strict only in `required` mode. `on` is documented as fail-open on
	// incomplete input and keeps its previous tolerance.
	strictProof := strings.EqualFold(strings.TrimSpace(s.cfg.X402.Mode), x402.ModeRequired)
	if reason := v2ProofError(parsed, strictProof, s.x402Requirement.MaxTimeoutSeconds, now); reason != "" {
		s.rejectPaymentWindow(w, id, reason)
		return
	}

	// The idempotency/replay key binds to the client's single-use authorization
	// nonce (not the raw request bytes) and to the canonicalized request +
	// entire PaymentRequirements (not scheme+network alone).
	nonce := replayNonce(paymentSignature, parsed)
	requestKey := canonicalPaymentRequestKey(rawParams, s.x402Requirement)
	executionKey := nonce + ":" + requestKey
	expiresAt := now.Add(nonceLedgerTTL(parsed, s.x402Requirement.MaxTimeoutSeconds, now))

	pc := paymentContext{
		signature:    paymentSignature,
		nonce:        nonce,
		requestKey:   requestKey,
		executionKey: executionKey,
		expiresAt:    expiresAt,
	}

	// hold a per-key lock to serialize check/execute/set actions and avoid
	// races when the same (nonce, request) is processed concurrently.
	unlock := s.lockForExecutionKey(executionKey)
	defer unlock()

	// Exact idempotent retry of the same (nonce, request): re-surface the
	// recorded outcome (driving a pending settle to completion). Never a second
	// execution or re-charge.
	if s.replayCachedPaymentOutcomeIfAny(ctx, w, id, pc) {
		return
	}

	// Cross-request replay / already-consumed classification (read-only,
	// pre-verify so an invalid or transient verify never burns a nonce).
	if s.handleNonceDecision(w, id, s.classifyNonce(nonce, requestKey)) {
		return
	}

	verifyResponse, err := s.x402Client.Verify(ctx, paymentSignature, s.x402Requirement)
	if err != nil {
		s.handlePaymentFailure(w, id, "verify", err, executionKey)
		return
	}
	// The facilitator returns HTTP 200 with {"isValid":false} for a rejected
	// payment, which surfaces here as a nil Go error. Inspect the verdict and
	// fail closed so an invalid payment never reaches the gated tool.
	if !paymentVerdictTrue(verifyResponse, "isValid") {
		s.rejectInvalidPayment(w, id, verifyResponse, executionKey)
		return
	}
	s.emitPaymentEvent("info", "payment_verified", safePaymentResponseFields(verifyResponse))
	s.appendPaymentLog("payment_verified", safePaymentResponseFields(verifyResponse))

	// Reserve the nonce now (atomic). A concurrent request presenting the same
	// nonce with a different logical request loses this race and is rejected as a
	// replay, so it cannot reach tool execution.
	if s.handleNonceDecision(w, id, s.reserveNonce(nonce, requestKey, executionKey, expiresAt)) {
		return
	}

	s.executeAndSettlePaidToolCall(ctx, w, id, rawParams, pc)
}

// handleNonceDecision writes the appropriate response for a non-proceed nonce
// ledger decision and reports whether the request was fully handled. A
// nonceProceed decision writes nothing and returns false so the caller
// continues the flow.
func (s *Server) handleNonceDecision(w http.ResponseWriter, id interface{}, dec nonceDecision) bool {
	switch dec.kind {
	case nonceReplay:
		s.rejectReplayedNonce(w, id)
		return true
	case nonceConsumed:
		if s.resurfaceConsumedOutcome(w, id, dec.executionKey) {
			return true
		}
		s.rejectReplayedNonce(w, id)
		return true
	case nonceError:
		// The durable single-use ledger could not be consulted or written, so we
		// cannot prove this nonce is unused. Fail closed with a retryable error
		// rather than risk admitting a replay.
		s.emitPaymentEvent("warning", "payment_replay_ledger_unavailable", map[string]interface{}{
			"reason": "nonce_ledger_unavailable",
		})
		s.appendPaymentLog("payment_replay_ledger_unavailable", map[string]interface{}{
			"reason": "nonce_ledger_unavailable",
		})
		s.writePaymentChallenge(w, id, x402.CodePaymentInvalid, "payment temporarily unavailable: single-use ledger unreachable, retry", true)
		return true
	default:
		return false
	}
}

// executeAndSettlePaidToolCall runs the gated tool for a verified+reserved
// payment, then settles. It owns the reserve->commit/rollback transitions: a
// tool error rolls the reservation back (no charge), a transient settle failure
// keeps it held for retry, and settlement success durably consumes the nonce.
func (s *Server) executeAndSettlePaidToolCall(ctx context.Context, w http.ResponseWriter, id interface{}, rawParams json.RawMessage, pc paymentContext) {
	result, statusCode, rpcErr := s.processToolsCall(ctx, rawParams)
	outcome := paymentExecutionOutcome{
		StatusCode: statusCode,
		UpdatedAt:  time.Now().UTC(),
		ExpiresAt:  pc.expiresAt,
	}
	if rpcErr != nil {
		outcome.RPCError = cloneRPCError(rpcErr)
		outcome.RequiresSettle = false
		outcome.Settled = true
		s.setPaymentExecutionOutcome(pc.executionKey, outcome)
		// The gated tool failed transport-side; no payment is captured, so the
		// nonce is NOT consumed and the SAME (nonce, request) may be retried
		// (re-surfacing this cached outcome, or re-executing after it expires).
		// The reservation binding is intentionally retained until expiry so the
		// single-use nonce cannot be reused for a DIFFERENT request — that would
		// be a cross-request replay.
		s.releaseNonceReservation(pc.nonce)
		writeResponse(w, statusCode, rpcResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error:   rpcErr,
		})
		return
	}
	outcome.Result = &result
	outcome.RequiresSettle = !result.IsError
	outcome.Settled = result.IsError
	s.setPaymentExecutionOutcome(pc.executionKey, outcome)
	if result.IsError {
		// Tool-level error result: we do not settle, so no charge is captured and
		// the nonce is not consumed. As above, the (nonce, request) binding is
		// retained until expiry so the same request may retry but a different
		// request cannot reuse the nonce.
		s.releaseNonceReservation(pc.nonce)
		writeResult(w, statusCode, id, result)
		return
	}

	settleResponse, err := s.x402Client.Settle(ctx, pc.signature, s.x402Requirement)
	if err != nil {
		// Transient/other settle failure: the executed outcome stays cached and
		// the reservation stays held so a retry of the same (nonce, request)
		// re-drives settlement (and a cross-request replay stays blocked). The
		// nonce is NOT consumed until settlement succeeds.
		s.handlePaymentFailure(w, id, "settle", err, pc.executionKey)
		return
	}
	// As with verify, the facilitator returns HTTP 200 with {"success":false}
	// for a failed settlement (nil Go error). Do not mark the outcome settled
	// or emit a PAYMENT-RESPONSE for an unsettled payment.
	if !paymentVerdictTrue(settleResponse, "success") {
		s.rejectFailedSettlement(w, id, settleResponse, pc.executionKey)
		return
	}

	// Settlement succeeded: durably consume the nonce before finalizing.
	s.commitNonce(pc.nonce, pc.requestKey, pc.executionKey, pc.expiresAt)

	// update the cached outcome; if the entry was pruned we need to
	// reconstruct and persist the successful state before replaying it.
	updated, found := s.markPaymentExecutionSettled(pc.executionKey, string(settleResponse))
	if !found {
		// use the local copy of outcome that still holds the original
		// execution result, then mark it settled and persist it.
		outcome.Settled = true
		outcome.PaymentResponse = strings.TrimSpace(string(settleResponse))
		outcome.UpdatedAt = time.Now().UTC()
		s.setPaymentExecutionOutcome(pc.executionKey, outcome)
		updated = outcome
	}
	s.replayPaymentExecutionOutcome(w, id, updated)

	s.emitPaymentEvent("info", "payment_settled", safePaymentResponseFields(settleResponse))
	s.appendPaymentLog("payment_settled", safePaymentResponseFields(settleResponse))
}

// paymentContext bundles the derived per-request payment identifiers so they can
// be threaded through the verify/execute/settle flow without repeated
// recomputation.
type paymentContext struct {
	signature    string
	nonce        string
	requestKey   string
	executionKey string
	expiresAt    time.Time
}

// rejectReplayedNonce responds to a replay/misuse attempt (a nonce already
// recorded for a different logical request, or an already-consumed nonce whose
// outcome is no longer available). It maps to the spec's `rejected` failure
// branch and never drives a second tool execution or settlement.
func (s *Server) rejectReplayedNonce(w http.ResponseWriter, id interface{}) {
	s.emitPaymentEvent("warning", "payment_replay_rejected", map[string]interface{}{
		"reason": "nonce_already_used",
	})
	s.appendPaymentLog("payment_replay_rejected", map[string]interface{}{
		"reason": "nonce_already_used",
	})
	s.writePaymentChallenge(w, id, x402.CodePaymentInvalid, "payment rejected: authorization nonce already used", false)
}

// rejectPaymentWindow responds to a proof whose validity window does not cover
// the current time (or exceeds maxTimeoutSeconds). It re-emits the challenge so
// the client can obtain a fresh, in-window authorization.
func (s *Server) rejectPaymentWindow(w http.ResponseWriter, id interface{}, reason string) {
	s.emitPaymentEvent("info", "payment_window_rejected", map[string]interface{}{
		"reason": reason,
	})
	s.appendPaymentLog("payment_window_rejected", map[string]interface{}{
		"reason": reason,
	})
	s.writePaymentChallenge(w, id, x402.CodePaymentInvalid, "payment rejected: "+reason, false)
}

// resurfaceConsumedOutcome replays a previously recorded settled outcome for an
// idempotent retry when the exact-key cache missed but the nonce ledger recorded
// consumption. It returns false when no outcome is available to re-surface.
func (s *Server) resurfaceConsumedOutcome(w http.ResponseWriter, id interface{}, executionKey string) bool {
	if strings.TrimSpace(executionKey) == "" {
		return false
	}
	outcome, ok := s.getPaymentExecutionOutcome(executionKey)
	if !ok {
		return false
	}
	s.emitPaymentEvent("info", "payment_idempotent_replay", map[string]interface{}{
		"reason": "nonce_already_consumed",
	})
	s.replayPaymentExecutionOutcome(w, id, outcome)
	return true
}

// paymentVerdictTrue reports whether the facilitator response body carries the
// named boolean verdict field set to true ("isValid" for verify, "success" for
// settle). A missing, malformed, or non-true field is treated as false so the
// payment gate fails closed: the facilitator returns HTTP 200 with
// isValid=false / success=false for a rejected payment, which surfaces as a nil
// Go error and must not be mistaken for a successful payment.
func paymentVerdictTrue(raw json.RawMessage, field string) bool {
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return false
	}
	v, ok := parsed[field]
	if !ok {
		return false
	}
	var b bool
	if err := json.Unmarshal(v, &b); err != nil {
		return false
	}
	return b
}

// paymentStringField extracts a string field from a facilitator response body,
// returning "" when the field is absent or not a string.
func paymentStringField(raw json.RawMessage, field string) string {
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ""
	}
	v, ok := parsed[field]
	if !ok {
		return ""
	}
	var str string
	if err := json.Unmarshal(v, &str); err != nil {
		return ""
	}
	return strings.TrimSpace(str)
}

// rejectInvalidPayment handles a facilitator verify that returned a non-true
// verdict (isValid=false) without a transport error. It records the verdict and
// responds with a PAYMENT_INVALID challenge instead of executing the tool.
func (s *Server) rejectInvalidPayment(w http.ResponseWriter, id interface{}, verifyResponse json.RawMessage, executionKey string) {
	fields := safePaymentResponseFields(verifyResponse)
	s.emitPaymentEvent("info", "payment_invalid", fields)
	s.appendPaymentLog("payment_invalid", fields)
	message := "payment verification rejected"
	if reason := paymentStringField(verifyResponse, "invalidReason"); reason != "" {
		message += ": " + reason
	}
	s.handlePaymentFailure(w, id, "verify", &x402.FacilitatorError{
		Operation:  "verify",
		StatusCode: http.StatusPaymentRequired,
		Code:       x402.CodePaymentInvalid,
		Message:    message,
		Retryable:  false,
	}, executionKey)
}

// rejectFailedSettlement handles a facilitator settle that returned a non-true
// verdict (success=false) without a transport error. The gated tool has already
// executed, but the payment did not settle, so the outcome is not marked
// settled and no successful PAYMENT-RESPONSE is emitted.
func (s *Server) rejectFailedSettlement(w http.ResponseWriter, id interface{}, settleResponse json.RawMessage, executionKey string) {
	fields := safePaymentResponseFields(settleResponse)
	s.emitPaymentEvent("error", "payment_settlement_failed", fields)
	s.appendPaymentLog("payment_settlement_failed", fields)
	message := "payment settlement failed"
	if reason := paymentStringField(settleResponse, "errorReason"); reason != "" {
		message += ": " + reason
	}
	s.handlePaymentFailure(w, id, "settle", &x402.FacilitatorError{
		Operation:  "settle",
		StatusCode: http.StatusPaymentRequired,
		Code:       x402.CodePaymentSettlementFailed,
		Message:    message,
		Retryable:  false,
	}, executionKey)
}

func (s *Server) handlePaymentFailure(w http.ResponseWriter, id interface{}, operation string, err error, executionKey string) {
	facErr, ok := err.(*x402.FacilitatorError)
	if !ok {
		code := x402.CodePaymentFacilitatorUnavailable
		if operation == "settle" {
			code = x402.CodePaymentSettlementUnavailable
		}
		facErr = &x402.FacilitatorError{
			Operation:  operation,
			StatusCode: http.StatusServiceUnavailable,
			Code:       code,
			Message:    "payment processing failed",
			Retryable:  true,
			Cause:      err,
		}
	}
	if operation == "settle" {
		if outcome, ok := s.getPaymentExecutionOutcome(executionKey); ok {
			if !outcome.RequiresSettle || outcome.Settled {
				s.replayPaymentExecutionOutcome(w, id, outcome)
				return
			}
		}
	}

	statusCode := http.StatusServiceUnavailable
	includeChallenge := false
	switch facErr.Code {
	case x402.CodePaymentRequired:
		statusCode = http.StatusPaymentRequired
		includeChallenge = true
	case x402.CodePaymentInvalid, x402.CodePaymentSettlementFailed:
		statusCode = http.StatusPaymentRequired
		includeChallenge = true
	case x402.CodePaymentConfigInvalid:
		statusCode = http.StatusServiceUnavailable
	default:
		if facErr.StatusCode >= 400 && facErr.StatusCode < 500 && !facErr.Retryable {
			statusCode = http.StatusPaymentRequired
			includeChallenge = true
		}
	}

	s.emitPaymentEvent("error", "payment_failed", map[string]interface{}{
		"operation": operation,
		"code":      facErr.Code,
		"message":   facErr.Message,
		"retryable": facErr.Retryable,
		"status":    facErr.StatusCode,
	})
	s.appendPaymentLog("payment_failed", map[string]interface{}{
		"operation": operation,
		"code":      facErr.Code,
		"message":   facErr.Message,
		"retryable": facErr.Retryable,
		"status":    facErr.StatusCode,
	})

	if includeChallenge {
		s.writePaymentChallenge(w, id, facErr.Code, facErr.Message, facErr.Retryable)
		return
	}
	writeError(w, statusCode, id, -32000, facErr.Message, facErr.Code, facErr.Retryable)
}

func (s *Server) replayCachedPaymentOutcomeIfAny(ctx context.Context, w http.ResponseWriter, id interface{}, pc paymentContext) bool {
	executionKey := pc.executionKey
	outcome, ok := s.getPaymentExecutionOutcome(executionKey)
	if !ok {
		return false
	}
	if !outcome.RequiresSettle || outcome.Settled {
		s.replayPaymentExecutionOutcome(w, id, outcome)
		return true
	}

	settleResponse, settleErr := s.x402Client.Settle(ctx, pc.signature, s.x402Requirement)
	if settleErr != nil {
		s.handlePaymentFailure(w, id, "settle", settleErr, executionKey)
		return true
	}
	if !paymentVerdictTrue(settleResponse, "success") {
		s.rejectFailedSettlement(w, id, settleResponse, executionKey)
		return true
	}
	// Settlement of the previously-executed request succeeded on retry: durably
	// consume the nonce (the first attempt held a reservation that never
	// committed, or was rolled back and is re-consumed here).
	s.commitNonce(pc.nonce, pc.requestKey, executionKey, pc.expiresAt)
	// original outcome loaded above; keep a copy in case the cache entry
	// is gone by the time we call markPaymentExecutionSettled.
	orig := outcome
	updated, found := s.markPaymentExecutionSettled(executionKey, string(settleResponse))
	if !found {
		orig.Settled = true
		orig.PaymentResponse = strings.TrimSpace(string(settleResponse))
		orig.UpdatedAt = time.Now().UTC()
		s.setPaymentExecutionOutcome(executionKey, orig)
		updated = orig
	}
	s.replayPaymentExecutionOutcome(w, id, updated)

	fields := safePaymentResponseFields(settleResponse)
	fields["replay"] = true
	s.emitPaymentEvent("info", "payment_settled", fields)
	s.appendPaymentLog("payment_settled", fields)
	return true
}

// lockForExecutionKey returns an unlock function for the mutex associated with the
// given executionKey.  The caller must call the returned function when the
// critical section is complete.  If the key is empty, a no-op unlock is
// returned.
func (s *Server) lockForExecutionKey(key string) func() {
	if strings.TrimSpace(key) == "" {
		return func() {}
	}

	s.execMu.Lock()
	km, ok := s.execKeyMu[key]
	if !ok {
		km = &keyMutex{}
		s.execKeyMu[key] = km
	}
	km.ref++
	// wake any waiters observing ref counts
	if s.execCond != nil {
		s.execCond.Broadcast()
	}
	s.execMu.Unlock()

	km.mu.Lock()
	return func() {
		km.mu.Unlock()
		s.execMu.Lock()
		km.ref--
		if km.ref == 0 {
			delete(s.execKeyMu, key)
		}
		if s.execCond != nil {
			s.execCond.Broadcast()
		}
		s.execMu.Unlock()
	}
}

func (s *Server) getPaymentExecutionOutcome(key string) (paymentExecutionOutcome, bool) {
	if strings.TrimSpace(key) == "" {
		return paymentExecutionOutcome{}, false
	}
	s.paymentMu.Lock()
	keysToDelete := s.prunePaymentOutcomesLocked(time.Now().UTC())
	outcome, ok := s.paymentOutcomes[key]
	s.paymentMu.Unlock()

	// Perform store deletions outside the mutex
	for _, k := range keysToDelete {
		s.deletePersistedPaymentOutcome(k)
	}
	return outcome, ok
}

func (s *Server) setPaymentExecutionOutcome(key string, outcome paymentExecutionOutcome) {
	if strings.TrimSpace(key) == "" {
		return
	}
	s.paymentMu.Lock()
	now := time.Now().UTC()
	keysToDelete := s.prunePaymentOutcomesLocked(now)

	// compare-and-swap: only write if there is no existing outcome.  Any
	// stored outcome has a non-zero UpdatedAt, so we only need to check for
	// existence rather than inspect the timestamp.
	if _, ok := s.paymentOutcomes[key]; ok {
		// already completed by another goroutine; skip overwrite.
		s.paymentMu.Unlock()
		// Perform store deletions outside the mutex
		for _, k := range keysToDelete {
			s.deletePersistedPaymentOutcome(k)
		}
		return
	}

	s.paymentOutcomes[key] = outcome
	s.paymentMu.Unlock()

	// Perform store operations outside the mutex
	s.persistPaymentOutcome(key, outcome)
	for _, k := range keysToDelete {
		s.deletePersistedPaymentOutcome(k)
	}
}

func (s *Server) markPaymentExecutionSettled(key, paymentResponse string) (paymentExecutionOutcome, bool) {
	// read and update shared state under lock; emit any warning afterwards.
	var outcome paymentExecutionOutcome
	var ok bool

	s.paymentMu.Lock()
	outcome, ok = s.paymentOutcomes[key]
	if ok {
		outcome.Settled = true
		outcome.PaymentResponse = strings.TrimSpace(paymentResponse)
		outcome.UpdatedAt = time.Now().UTC()
		s.paymentOutcomes[key] = outcome
		s.persistPaymentOutcome(key, outcome)
	}
	s.paymentMu.Unlock()

	if !ok {
		// nothing to settle; avoid creating a partial entry. emit warning after
		// releasing the lock to avoid blocking other goroutines holding
		// paymentMu.
		s.emitPaymentEvent("warning", "payment_outcome_missing", map[string]interface{}{"key": key})
		return paymentExecutionOutcome{}, false
	}
	return outcome, true
}

func paymentOutcomeToRecord(key string, outcome paymentExecutionOutcome) (storepkg.MCPPaymentOutcomeRecord, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return storepkg.MCPPaymentOutcomeRecord{}, false
	}
	var (
		resultJSON string
		rpcErrJSON string
	)
	if outcome.Result != nil {
		b, err := json.Marshal(outcome.Result)
		if err != nil {
			return storepkg.MCPPaymentOutcomeRecord{}, false
		}
		resultJSON = string(b)
	}
	if outcome.RPCError != nil {
		b, err := json.Marshal(outcome.RPCError)
		if err != nil {
			return storepkg.MCPPaymentOutcomeRecord{}, false
		}
		rpcErrJSON = string(b)
	}
	updatedAt := outcome.UpdatedAt.UTC()
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	return storepkg.MCPPaymentOutcomeRecord{
		ExecutionKey:    key,
		StatusCode:      outcome.StatusCode,
		ResultJSON:      resultJSON,
		RPCErrorJSON:    rpcErrJSON,
		RequiresSettle:  outcome.RequiresSettle,
		Settled:         outcome.Settled,
		PaymentResponse: strings.TrimSpace(outcome.PaymentResponse),
		UpdatedAt:       updatedAt,
		// Persist the nonce-aligned expiry too. Without it a restart restored the
		// outcome with a zero ExpiresAt, so pruning fell back to the fixed
		// paymentOutcomeTTL and dropped an outcome whose nonce was still consumed
		// in the ledger. A valid idempotent retry then got "nonce already used"
		// instead of the result it had already paid for (#697). A zero value stays
		// zero and keeps the fallback.
		ExpiresAt: outcome.ExpiresAt.UTC(),
	}, true
}

func paymentOutcomeFromRecord(rec storepkg.MCPPaymentOutcomeRecord) (paymentExecutionOutcome, bool) {
	if strings.TrimSpace(rec.ExecutionKey) == "" || rec.UpdatedAt.IsZero() {
		return paymentExecutionOutcome{}, false
	}
	var outcome paymentExecutionOutcome
	outcome.StatusCode = rec.StatusCode
	outcome.RequiresSettle = rec.RequiresSettle
	outcome.Settled = rec.Settled
	outcome.PaymentResponse = strings.TrimSpace(rec.PaymentResponse)
	outcome.UpdatedAt = rec.UpdatedAt.UTC()
	// A row written before the expires_unix column existed reads back as a zero
	// time. Pruning then uses the UpdatedAt plus TTL fallback, which is the
	// behavior those rows already had, so an old database still loads (#697).
	if !rec.ExpiresAt.IsZero() {
		outcome.ExpiresAt = rec.ExpiresAt.UTC()
	}
	if strings.TrimSpace(rec.ResultJSON) != "" {
		var result toolCallResult
		if err := json.Unmarshal([]byte(rec.ResultJSON), &result); err != nil {
			return paymentExecutionOutcome{}, false
		}
		outcome.Result = &result
	}
	if strings.TrimSpace(rec.RPCErrorJSON) != "" {
		var rpcErr rpcError
		if err := json.Unmarshal([]byte(rec.RPCErrorJSON), &rpcErr); err != nil {
			return paymentExecutionOutcome{}, false
		}
		outcome.RPCError = &rpcErr
	}
	return outcome, true
}

func (s *Server) persistPaymentOutcome(key string, outcome paymentExecutionOutcome) {
	store, ok := s.store.(paymentOutcomePersistenceStore)
	if !ok || store == nil {
		return
	}
	rec, ok := paymentOutcomeToRecord(key, outcome)
	if !ok {
		return
	}
	if err := store.UpsertMCPPaymentOutcome(context.Background(), rec); err != nil {
		s.emitPaymentEvent("warning", "payment_outcome_persist_failed", map[string]interface{}{
			"key": key,
			"err": err.Error(),
		})
	}
}

func (s *Server) deletePersistedPaymentOutcome(key string) {
	store, ok := s.store.(paymentOutcomePersistenceStore)
	if !ok || store == nil {
		return
	}
	if err := store.DeleteMCPPaymentOutcome(context.Background(), key); err != nil {
		s.emitPaymentEvent("warning", "payment_outcome_delete_failed", map[string]interface{}{
			"key": key,
			"err": err.Error(),
		})
	}
}

// cloneRPCError returns a copy of the supplied rpcError.  callers may
// hold on to the returned value and modify it without contaminating the
// original error – the copy must not share any mutable state with `err`.
//
// Historically the implementation performed a shallow struct copy and then
// duplicated the top‑level `Data` pointer value (see previous version below).
// That was sufficient because rpcErrorData was a simple struct containing
// only primitive fields.  If rpcErrorData is later extended with slices,
// maps, or pointers a naive copy would allow the original and clone to share
// substructures, leading to data races when both are modified concurrently.
//
// To guard against that future possibility we perform a deterministic
// encoding round‑trip using encoding/json.  JSON serialization works with the
// existing exported fields and will recursively copy any nested collections
// or pointers.  The cost is negligible in this hot path (error cloning only
// occurs during payment caching) and keeps the implementation simple.
func cloneRPCError(err *rpcError) *rpcError {
	if err == nil {
		return nil
	}

	// fast path: marshal/unmarshal to create a deep copy.  The error return
	// from these calls is ignored because the types involved are known to be
	// JSON‑encodable; in the unlikely event of a failure we fall back to a
	// manual copy to avoid returning nil.
	var cloned rpcError
	if b, marshalErr := json.Marshal(err); marshalErr == nil {
		if json.Unmarshal(b, &cloned) != nil {
			// fallback on unmarshal failure
			cloned = *err
			if err.Data != nil {
				data := *err.Data
				cloned.Data = &data
			}
		}
	} else {
		// fallback to previous behaviour; copy top-level and data by value.
		cloned = *err
		if err.Data != nil {
			data := *err.Data
			cloned.Data = &data
		}
	}
	return &cloned
}

func (s *Server) replayPaymentExecutionOutcome(w http.ResponseWriter, id interface{}, outcome paymentExecutionOutcome) {
	if strings.TrimSpace(outcome.PaymentResponse) != "" {
		w.Header().Set(x402.HeaderPaymentResponse, outcome.PaymentResponse)
	}
	if outcome.RPCError != nil {
		writeResponse(w, outcome.StatusCode, rpcResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error:   cloneRPCError(outcome.RPCError),
		})
		return
	}
	if outcome.Result != nil {
		writeResult(w, outcome.StatusCode, id, *outcome.Result)
		return
	}
	writeError(w, http.StatusServiceUnavailable, id, -32603, "cached payment outcome unavailable", "INTERNAL_ERROR", true)
}

func (s *Server) writePaymentChallenge(w http.ResponseWriter, id interface{}, code, message string, retryable bool) {
	headerValue, err := x402.BuildPaymentRequiredHeaderValue(s.x402Requirement)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, id, -32000, err.Error(), x402.CodePaymentConfigInvalid, false)
		return
	}
	w.Header().Set(x402.HeaderPaymentRequired, headerValue)
	writeError(w, http.StatusPaymentRequired, id, -32000, message, code, retryable)
}

func (s *Server) emitPaymentEvent(level, event string, data interface{}) {
	if s.eventEmitter == nil {
		return
	}
	s.eventEmitter(level, event, data)
}

// writeLogEntry centralizes writing a raw entry plus newline and
// flushing if the writer supports it. callers should close w if
// appropriate.
func writeLogEntry(w io.Writer, raw []byte) error {
	if _, err := w.Write(raw); err != nil {
		return err
	}
	// newline
	if _, err := w.Write([]byte("\n")); err != nil {
		return err
	}
	if flusher, ok := w.(interface{ Flush() error }); ok {
		return flusher.Flush()
	}
	return nil
}

func (s *Server) appendPaymentLog(event string, data map[string]interface{}) {
	if strings.TrimSpace(s.paymentLogPath) == "" {
		return
	}

	entry := map[string]interface{}{
		"ts":    time.Now().UTC().Format(time.RFC3339Nano),
		"event": event,
		"data":  data,
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		s.emitPaymentLogWarning(err)
		return
	}

	// acquire lock and ensure writer is initialized before doing any write.  this
	// prevents a second goroutine from racing in between the nil-check and the
	// actual write and dropping an entry.
	s.paymentLogMu.Lock()
	defer s.paymentLogMu.Unlock()

	// helper that (re)initializes the cached file/writer; caller must hold mutex.
	initWriter := func() error {
		if err := statefs.MkdirAll(filepath.Dir(s.paymentLogPath)); err != nil {
			return err
		}
		f, err := os.OpenFile(s.paymentLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) // owner read/write only
		if err != nil {
			return err
		}
		s.paymentLogFile = f
		s.paymentLogWriter = bufio.NewWriter(f)
		return nil
	}

	if s.paymentLogWriter == nil || s.paymentLogFile == nil {
		if err := initWriter(); err != nil {
			s.emitPaymentLogWarning(err)
			return
		}
	}

	// attempt write; on error try to recover once
	if err := writeLogEntry(s.paymentLogWriter, raw); err != nil {
		s.emitPaymentLogWarning(err)
		// persistent writer failure; try to re-create writer & retry once
		if s.paymentLogWriter != nil {
			// flush any buffered data before dropping the writer. we deliberately
			// ignore flush errors beyond emitting a warning since the primary
			// write has already failed and we're about to reinitialize the writer.
			if err := s.paymentLogWriter.Flush(); err != nil {
				s.emitPaymentLogWarning(err)
			}
			// drop the buffered writer; there is nothing to close
			s.paymentLogWriter = nil
		}
		if s.paymentLogFile != nil {
			_ = s.paymentLogFile.Close()
			s.paymentLogFile = nil
		}
		if err2 := initWriter(); err2 != nil {
			s.emitPaymentLogWarning(err2)
			return
		}
		if err2 := writeLogEntry(s.paymentLogWriter, raw); err2 != nil {
			s.emitPaymentLogWarning(err2)
		}
	}

	// done successfully
}

// safePaymentResponseFields returns a log-safe subset of a facilitator
// response, including only explicitly allowed fields to avoid recording
// unexpected or sensitive fields that the facilitator may add in future.
func safePaymentResponseFields(raw json.RawMessage) map[string]interface{} {
	allowed := []string{"ok", "txHash", "status", "network", "amount", "isValid", "success", "invalidReason"}
	var parsed map[string]interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return map[string]interface{}{}
	}
	out := make(map[string]interface{}, len(allowed))
	for _, key := range allowed {
		v, ok := parsed[key]
		if !ok {
			continue
		}
		switch v.(type) {
		case string, float64, bool, nil:
			out[key] = v
			// skip nested objects and arrays
		}
	}
	return out
}

func (s *Server) emitPaymentLogWarning(err error) {
	if err == nil {
		return
	}
	s.emitPaymentEvent("warning", "payment_log_write_failed", map[string]interface{}{
		"msg":  "payment log write failed",
		"path": s.paymentLogPath,
		"err":  err.Error(),
	})
}
