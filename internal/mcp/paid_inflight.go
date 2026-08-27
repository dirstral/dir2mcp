package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/dirstral/dir2mcp/internal/protocol"
)

// Cancellation of x402-gated tool calls (issue #657).
//
// notifications/cancelled reaches the MCP SDK, and the SDK cancels the context
// of the request it dispatched. That works for the default path, but an
// x402-gated tools/call never enters the SDK: handleSDKToolsCall routes it to
// handleToolsCallRequest directly. So on the one financially sensitive path, a
// cancellation was an acknowledged no-op: the client got HTTP 202 while the
// server kept spending provider quota and moved on toward settlement.
//
// This registry closes that gap. A gated call registers its cancel func under
// (session, requestId) for exactly as long as it is cancellable, and a
// cancellation notification looks it up and fires it.
//
// PAYMENT SEMANTICS, by state, and why each is safe:
//
//   - Before execution (verify, nonce reserve): the context is cancelled, the
//     flow aborts, and no charge exists to reverse. A reservation taken just
//     before the abort is released by the same error path a tool failure uses.
//   - During execution: processToolsCall observes the cancelled context and
//     returns an error. The existing path then releases the nonce reservation
//     and settles nothing, so the caller is not charged for work it abandoned.
//   - During settlement: NOT cancellable, deliberately. Aborting a settle
//     in flight leaves the facilitator's state unknown to us: the money may
//     have moved. A later retry could then double settle, which is precisely
//     what this issue forbids. The call therefore leaves the registry before
//     settlement begins, so a cancellation arriving in that window is a
//     truthful no-op on a payment that is already committed to.
//
// The registry never creates a second execution: it only cancels, and the
// execution-key lock plus the cached outcome continue to serialize retries.
type paidInFlightRegistry struct {
	mu    sync.Mutex
	calls map[string]context.CancelFunc
}

func newPaidInFlightRegistry() *paidInFlightRegistry {
	return &paidInFlightRegistry{calls: make(map[string]context.CancelFunc)}
}

// paidInFlightKey identifies one in-flight gated call. The session scopes the
// request id, because JSON-RPC ids are only unique within a session and one
// client must never be able to cancel another's work.
//
// The id is rendered rather than typed: JSON-RPC allows a string or a number,
// a JSON number decodes to float64, and a client may echo "42" where it sent
// 42. Comparing rendered forms makes those agree, which is what a client
// means; the session scope keeps the looser match from reaching anyone else's
// call.
func paidInFlightKey(sessionID string, id interface{}) string {
	return strings.TrimSpace(sessionID) + "\x00" + renderRPCID(id)
}

// renderRPCID renders a JSON-RPC id canonically, so 42, 42.0 and "42" all
// compare equal. A nil id yields "", which paidInFlightKey callers reject.
func renderRPCID(id interface{}) string {
	switch v := id.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case float64:
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprintf("%v", v)
	case json.Number:
		return strings.TrimSpace(v.String())
	default:
		// JSON-RPC 2.0 allows an id to be a string, a number, or null, and
		// nothing else. Rendering anything else with %v would coin a key from a
		// boolean, array or object: `true` would become "true" and could target
		// a call whose id is the STRING "true". Refuse instead of inventing.
		return ""
	}
}

// add registers cancel under the key and returns a release func. Release is
// idempotent and MUST be called before settlement begins, so a cancellation
// cannot interrupt a payment that is already being captured.
func (r *paidInFlightRegistry) add(key string, cancel context.CancelFunc) func() {
	if r == nil || key == "" || strings.HasSuffix(key, "\x00") {
		// No usable id (a notification, or an id that renders empty): nothing
		// can target this call, so registering it would only leak an entry.
		return func() {}
	}
	r.mu.Lock()
	r.calls[key] = cancel
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.calls, key)
			r.mu.Unlock()
		})
	}
}

// cancel fires the registered cancel func for key and reports whether one was
// found. Not finding a call is normal and not an error: the call may have
// finished, may never have existed, or may already be past the cancellable
// window (settling).
func (r *paidInFlightRegistry) cancel(key string) bool {
	if r == nil || key == "" {
		return false
	}
	r.mu.Lock()
	cancel, ok := r.calls[key]
	r.mu.Unlock()
	if !ok {
		return false
	}
	// Fired outside the lock: a cancel func runs arbitrary teardown, and
	// holding the registry lock across it would serialize every other gated
	// call behind it.
	cancel()
	return true
}

// cancelledRequestID extracts params.requestId from a notifications/cancelled
// message. It returns "" when the field is absent or unusable, which the
// caller treats as "nothing to cancel" rather than an error: the notification
// still gets its 202, because a notification has no response to carry a
// failure in.
func cancelledRequestID(rawParams json.RawMessage) string {
	if len(rawParams) == 0 {
		return ""
	}
	var params struct {
		RequestID interface{} `json:"requestId"`
	}
	if err := json.Unmarshal(rawParams, &params); err != nil {
		return ""
	}
	return renderRPCID(params.RequestID)
}

// beginCancellableToolCall derives a cancellable context for an x402-gated tool
// call and registers it so notifications/cancelled can reach a path the SDK
// never dispatched. Both transports call this one helper, so the SDK chain and
// the direct handler chain cannot drift apart on cancellation the way they
// once did on the gated call itself.
//
// The caller MUST defer cancel (to release the context) and MUST call release
// before settlement begins. release is also safe to defer: it is idempotent.
func (s *Server) beginCancellableToolCall(r *http.Request, id interface{}) (context.Context, func(), context.CancelFunc) {
	ctx, cancel := context.WithCancel(r.Context())
	key := paidInFlightKey(r.Header.Get(protocol.MCPSessionHeader), id)
	return ctx, s.paidInFlight.add(key, cancel), cancel
}

// cancelPaidToolCall routes a notifications/cancelled message to the gated call
// it targets, if any. Missing or unknown request ids are no-ops: a notification
// carries no response, so there is nowhere truthful to report a failure, and
// "the call already finished" is the common case rather than an error.
func (s *Server) cancelPaidToolCall(r *http.Request, rawParams json.RawMessage) {
	target := cancelledRequestID(rawParams)
	if target == "" {
		return
	}
	// Session-scoped: a JSON-RPC id is unique only within a session, so an
	// unscoped lookup would let one client cancel another client's paid work.
	s.paidInFlight.cancel(paidInFlightKey(r.Header.Get(protocol.MCPSessionHeader), target))
}
