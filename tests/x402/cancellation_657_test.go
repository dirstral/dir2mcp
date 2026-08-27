package x402_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/x402"
)

// Issue #657: notifications/cancelled reached the MCP SDK, which cancels the
// request it dispatched. An x402-gated tools/call never enters the SDK, so on
// the one financially sensitive path a cancellation was an acknowledged no-op:
// the client got HTTP 202 while the server kept spending provider quota and
// moved on toward settlement.
//
// These tests walk the payment state machine boundary by boundary. The
// facilitator below can block inside verify or settle, which is what makes a
// specific window observable rather than a race.

// blockingRetriever makes a gated tool call observably long: Search blocks
// until the request context is cancelled (or a generous safety timeout). This
// is the window the issue is about, because tool execution is where provider
// quota is spent, not the facilitator handshake around it.
type blockingRetriever struct {
	entered   chan struct{}
	cancelled atomic.Bool
	once      sync.Once
}

func newBlockingRetriever() *blockingRetriever {
	return &blockingRetriever{entered: make(chan struct{})}
}

func (r *blockingRetriever) Search(ctx context.Context, _ model.SearchQuery) ([]model.SearchHit, error) {
	r.once.Do(func() { close(r.entered) })
	select {
	case <-ctx.Done():
		r.cancelled.Store(true)
		return nil, ctx.Err()
	case <-time.After(10 * time.Second):
		return nil, nil
	}
}

func (r *blockingRetriever) Ask(context.Context, string, model.SearchQuery) (model.AskResult, error) {
	return model.AskResult{}, nil
}
func (r *blockingRetriever) OpenFile(context.Context, string, model.Span, int) (string, error) {
	return "", nil
}
func (r *blockingRetriever) Stats(context.Context) (model.Stats, error) { return model.Stats{}, nil }
func (r *blockingRetriever) IndexingComplete(context.Context) (bool, error) {
	return true, nil
}

// cancelBody is a well-formed notifications/cancelled targeting id.
func cancelBody(id int) string {
	b, _ := json.Marshal(id)
	return `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":` +
		string(b) + `,"reason":"user aborted"}}`
}

// searchCallBody is a gated tools/call that routes into the blocking retriever.
func searchCallBody(id int) string {
	b, _ := json.Marshal(id)
	return `{"jsonrpc":"2.0","id":` + string(b) +
		`,"method":"tools/call","params":{"name":"dir2mcp_search","arguments":{"query":"anything"}}}`
}

// startGatedServer builds an x402-required server over a fresh facilitator and
// the given retriever.
func startGatedServer(t *testing.T, ret model.Retriever) (string, config.Config, *parityMockFacilitator) {
	t.Helper()
	f := newParityFacilitator()
	fac := httptest.NewServer(f)
	t.Cleanup(fac.Close)
	cfg := baseX402Config(t, fac.URL)
	cfg.X402.Mode = x402.ModeRequired
	srv := httptest.NewServer(mcp.NewServer(cfg, ret).Handler())
	t.Cleanup(srv.Close)
	return srv.URL + cfg.MCPPath, cfg, f
}

// TestX402Cancel657_DuringExecutionStopsTheToolAndChargesNothing is the
// reported defect. A gated call is cancelled while the tool is running: the
// tool must observe the cancellation, and nothing may settle, because the
// caller abandoned the work it would have paid for.
func TestX402Cancel657_DuringExecutionStopsTheToolAndChargesNothing(t *testing.T) {
	ret := newBlockingRetriever()
	mcpURL, cfg, fac := startGatedServer(t, ret)
	sid := parityInitSession(t, mcpURL)

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp := paritySendRPC(t, mcpURL, sid, searchCallBody(77), map[string]string{
			x402.HeaderPaymentSignature: validV2Signature(t, cfg),
		})
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	// Cancel only once the tool is provably running, so the window is exact.
	select {
	case <-ret.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the gated tool never started")
	}

	cancel := paritySendRPC(t, mcpURL, sid, cancelBody(77), nil)
	_ = cancel.Body.Close()
	if cancel.StatusCode != http.StatusAccepted {
		t.Fatalf("cancellation status=%d want=202", cancel.StatusCode)
	}

	start := time.Now()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not stop the gated tool: still running 5s later")
	}
	// It must stop BECAUSE it was cancelled, not because a timer elsewhere
	// expired. Without this the test would pass on the 10s safety fallback.
	if !ret.cancelled.Load() {
		t.Fatal("the tool did not observe the cancellation; provider work would have continued")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("gated call took %v to unwind; want prompt", elapsed)
	}
	// The money boundary: abandoned work is never charged for.
	if n := fac.settleCalls.Load(); n != 0 {
		t.Fatalf("settle called %d times after cancelling during execution; want 0", n)
	}
}

// okRetriever answers instantly, for the tests that need a gated call to run
// to completion rather than block.
type okRetriever struct{}

func (okRetriever) Search(context.Context, model.SearchQuery) ([]model.SearchHit, error) {
	return nil, nil
}
func (okRetriever) Ask(context.Context, string, model.SearchQuery) (model.AskResult, error) {
	return model.AskResult{}, nil
}
func (okRetriever) OpenFile(context.Context, string, model.Span, int) (string, error) {
	return "", nil
}
func (okRetriever) Stats(context.Context) (model.Stats, error)     { return model.Stats{}, nil }
func (okRetriever) IndexingComplete(context.Context) (bool, error) { return true, nil }

// TestX402Cancel657_SettlementIsNotInterruptible is the safety property. Once
// the tool has produced a result the payment is committed to, so the call
// leaves the cancellable window BEFORE settling: an aborted settle would leave
// the facilitator's state unknown and a retry could double settle.
func TestX402Cancel657_SettlementIsNotInterruptible(t *testing.T) {
	mcpURL, cfg, fac := startGatedServer(t, okRetriever{})
	sid := parityInitSession(t, mcpURL)

	resp := paritySendRPC(t, mcpURL, sid, toolsCallBody(78), map[string]string{
		x402.HeaderPaymentSignature: validV2Signature(t, cfg),
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gated call status=%d want=200 body=%s", resp.StatusCode, body)
	}
	if n := fac.settleCalls.Load(); n != 1 {
		t.Fatalf("settle called %d times, want exactly 1", n)
	}

	// A cancellation arriving after the call completed must be a truthful
	// no-op: accepted, and settling exactly once, never twice.
	cancel := paritySendRPC(t, mcpURL, sid, cancelBody(78), nil)
	_ = cancel.Body.Close()
	if cancel.StatusCode != http.StatusAccepted {
		t.Fatalf("late cancellation status=%d want=202", cancel.StatusCode)
	}
	if n := fac.settleCalls.Load(); n != 1 {
		t.Fatalf("settle called %d times after a late cancellation; want still 1 (no double settlement)", n)
	}
}

// TestX402Cancel657_CancellationIsSessionScoped pins that one client cannot
// cancel another's paid work: JSON-RPC ids are unique only within a session.
func TestX402Cancel657_CancellationIsSessionScoped(t *testing.T) {
	ret := newBlockingRetriever()
	mcpURL, cfg, _ := startGatedServer(t, ret)
	victim := parityInitSession(t, mcpURL)
	attacker := parityInitSession(t, mcpURL)
	if victim == attacker {
		t.Fatal("the two sessions are identical; the scoping cannot be tested")
	}

	done := make(chan int, 1)
	go func() {
		resp := paritySendRPC(t, mcpURL, victim, searchCallBody(79), map[string]string{
			x402.HeaderPaymentSignature: validV2Signature(t, cfg),
		})
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		done <- resp.StatusCode
	}()

	select {
	case <-ret.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the victim's gated tool never started")
	}

	// Same request id, different session: must NOT cancel the victim's call.
	other := paritySendRPC(t, mcpURL, attacker, cancelBody(79), nil)
	_ = other.Body.Close()

	select {
	case <-done:
		t.Fatal("a cancellation from a different session stopped the call")
	case <-time.After(600 * time.Millisecond):
		// Still running, which is correct.
	}
	if ret.cancelled.Load() {
		t.Fatal("the victim's tool observed a cancellation it never received")
	}

	// The victim's own cancellation still works, which proves the call was
	// cancellable all along and the cross-session attempt failed on scope.
	own := paritySendRPC(t, mcpURL, victim, cancelBody(79), nil)
	_ = own.Body.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the victim could not cancel its own call")
	}
}

// TestX402Cancel657_UnknownRequestIDIsAccepted pins the notification contract:
// a cancellation for a call that already finished, or never existed, is still
// a well-formed notification and gets its 202. A notification carries no
// response, so there is nowhere truthful to report "not found".
func TestX402Cancel657_UnknownRequestIDIsAccepted(t *testing.T) {
	mcpURL, _, _ := startGatedServer(t, okRetriever{})
	sid := parityInitSession(t, mcpURL)

	for _, body := range []string{
		cancelBody(4242),
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled"}`,
	} {
		resp := paritySendRPC(t, mcpURL, sid, body, nil)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("cancellation %q status=%d want=202", body, resp.StatusCode)
		}
	}
}
