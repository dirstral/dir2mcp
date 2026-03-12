package mcp

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dir2mcp/internal/x402"
)

type paymentExecutionOutcome struct {
	StatusCode      int
	Result          *toolCallResult
	RPCError        *rpcError
	RequiresSettle  bool
	Settled         bool
	PaymentResponse string
	UpdatedAt       time.Time
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
	}
	s.x402Client = x402.NewHTTPClient(s.cfg.X402.FacilitatorURL, s.cfg.X402.FacilitatorToken, nil)
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
	executionKey := paymentExecutionKey(paymentSignature, rawParams)

	// hold a per-key lock to serialize check/execute/set actions and avoid
	// races when the same signature+params are processed concurrently.
	unlock := s.lockForExecutionKey(executionKey)
	defer unlock()

	if s.replayCachedPaymentOutcomeIfAny(ctx, w, id, paymentSignature, executionKey) {
		return
	}

	verifyResponse, err := s.x402Client.Verify(ctx, paymentSignature, s.x402Requirement)
	if err != nil {
		s.handlePaymentFailure(w, id, "verify", err, executionKey)
		return
	}
	s.emitPaymentEvent("info", "payment_verified", map[string]interface{}{
		"response": json.RawMessage(verifyResponse),
	})
	s.appendPaymentLog("payment_verified", map[string]interface{}{
		"response": json.RawMessage(verifyResponse),
	})

	result, statusCode, rpcErr := s.processToolsCall(ctx, rawParams)
	outcome := paymentExecutionOutcome{
		StatusCode: statusCode,
		UpdatedAt:  time.Now().UTC(),
	}
	if rpcErr != nil {
		outcome.RPCError = cloneRPCError(rpcErr)
		outcome.RequiresSettle = false
		outcome.Settled = true
		s.setPaymentExecutionOutcome(executionKey, outcome)
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
	s.setPaymentExecutionOutcome(executionKey, outcome)
	if result.IsError {
		writeResult(w, statusCode, id, result)
		return
	}

	settleResponse, err := s.x402Client.Settle(ctx, paymentSignature, s.x402Requirement)
	if err != nil {
		s.handlePaymentFailure(w, id, "settle", err, executionKey)
		return
	}

	// update the cached outcome; if the entry was pruned we need to
	// reconstruct and persist the successful state before replaying it.
	updated, found := s.markPaymentExecutionSettled(executionKey, string(settleResponse))
	if !found {
		// use the local copy of outcome that still holds the original
		// execution result, then mark it settled and persist it.
		outcome.Settled = true
		outcome.PaymentResponse = strings.TrimSpace(string(settleResponse))
		outcome.UpdatedAt = time.Now().UTC()
		s.setPaymentExecutionOutcome(executionKey, outcome)
		updated = outcome
	}
	s.replayPaymentExecutionOutcome(w, id, updated)

	s.emitPaymentEvent("info", "payment_settled", map[string]interface{}{
		"response": json.RawMessage(settleResponse),
	})
	s.appendPaymentLog("payment_settled", map[string]interface{}{
		"response": json.RawMessage(settleResponse),
	})
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

func paymentExecutionKey(paymentSignature string, rawParams json.RawMessage) string {
	sum := sha256.Sum256(rawParams)
	return paymentSignature + ":" + hex.EncodeToString(sum[:])
}

func (s *Server) replayCachedPaymentOutcomeIfAny(ctx context.Context, w http.ResponseWriter, id interface{}, paymentSignature, executionKey string) bool {
	outcome, ok := s.getPaymentExecutionOutcome(executionKey)
	if !ok {
		return false
	}
	if !outcome.RequiresSettle || outcome.Settled {
		s.replayPaymentExecutionOutcome(w, id, outcome)
		return true
	}

	settleResponse, settleErr := s.x402Client.Settle(ctx, paymentSignature, s.x402Requirement)
	if settleErr != nil {
		s.handlePaymentFailure(w, id, "settle", settleErr, executionKey)
		return true
	}
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

	s.emitPaymentEvent("info", "payment_settled", map[string]interface{}{
		"response": json.RawMessage(settleResponse),
		"replay":   true,
	})
	s.appendPaymentLog("payment_settled", map[string]interface{}{
		"response": json.RawMessage(settleResponse),
		"replay":   true,
	})
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
	defer s.paymentMu.Unlock()
	s.prunePaymentOutcomesLocked(time.Now().UTC())
	outcome, ok := s.paymentOutcomes[key]
	return outcome, ok
}

func (s *Server) setPaymentExecutionOutcome(key string, outcome paymentExecutionOutcome) {
	if strings.TrimSpace(key) == "" {
		return
	}
	s.paymentMu.Lock()
	defer s.paymentMu.Unlock()
	now := time.Now().UTC()
	s.prunePaymentOutcomesLocked(now)

	// compare-and-swap: only write if there is no existing outcome.  Any
	// stored outcome has a non-zero UpdatedAt, so we only need to check for
	// existence rather than inspect the timestamp.
	if _, ok := s.paymentOutcomes[key]; ok {
		// already completed by another goroutine; skip overwrite.
		return
	}

	s.paymentOutcomes[key] = outcome
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
		if err := os.MkdirAll(filepath.Dir(s.paymentLogPath), 0o755); err != nil {
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
