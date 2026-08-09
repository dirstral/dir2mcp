package tests

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/store"
)

// v2PaymentSignature697 builds a minimal x402 v2 PAYMENT-SIGNATURE value. The
// adapter reads the single-use authorization nonce and the validity window from
// it, so the test drives the real nonce path and not the opaque-token fallback.
func v2PaymentSignature697(nonce string, now time.Time) string {
	return fmt.Sprintf(
		`{"x402Version":2,"scheme":"exact","payload":{"authorization":{"nonce":%q,"validAfter":%d,"validBefore":%d}}}`,
		nonce, now.Add(-5*time.Second).Unix(), now.Add(5*time.Minute).Unix(),
	)
}

// agePersistedPaymentOutcomes697 moves every persisted payment outcome's
// UpdatedAt into the past. It only rewrites UpdatedAt: every other field of the
// record, including the nonce-aligned expiry, is written back unchanged. The
// consumed nonce keeps its own ledger expiry, which is at least 15 minutes, so
// an age past the 10-minute outcome TTL puts the two windows out of step.
func agePersistedPaymentOutcomes697(t *testing.T, st *store.SQLiteStore, age time.Duration) int {
	t.Helper()
	records, err := st.ListMCPPaymentOutcomes(t.Context())
	if err != nil {
		t.Fatalf("list payment outcomes: %v", err)
	}
	for _, rec := range records {
		rec.UpdatedAt = time.Now().UTC().Add(-age)
		if err := st.UpsertMCPPaymentOutcome(t.Context(), rec); err != nil {
			t.Fatalf("age payment outcome %s: %v", rec.ExecutionKey, err)
		}
	}
	return len(records)
}

// TestX402OutcomeExpirySurvivesRestart697 pins issue #697. A settled outcome
// carries a nonce-aligned ExpiresAt so it stays available for as long as its
// nonce stays consumed. That expiry was not persisted, so a restart restored the
// outcome with a zero expiry and pruning fell back to the fixed 10-minute TTL on
// UpdatedAt. The nonce ledger keeps a consumed nonce for at least 15 minutes, so
// inside that 5-minute gap an exact idempotent retry lost its outcome and was
// refused as "nonce already used" instead of getting the paid result.
//
// The test crosses the 10-minute boundary while the nonce is still live, then
// repeats the identical call against a new server on the same store.
func TestX402OutcomeExpirySurvivesRestart697(t *testing.T) {
	fac := newFacilitatorStub(t)
	facServer := httptest.NewServer(fac)
	defer facServer.Close()

	cfg := x402EnabledTestConfig("https://resource.example.com")
	cfg.AuthMode = "none"
	cfg.X402.FacilitatorURL = facServer.URL

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(t.Context()); err != nil {
		t.Fatalf("Init store: %v", err)
	}
	defer func() { _ = st.Close() }()

	const paidCall = `{"jsonrpc":"2.0","id":697,"method":"tools/call","params":{"name":"dir2mcp_stats","arguments":{}}}`
	sig := v2PaymentSignature697("nonce-697-a", time.Now().UTC())

	// First server: the paid call verifies, executes and settles exactly once.
	server1 := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	session1 := initializeSession(t, server1.URL+cfg.MCPPath)
	resp1 := postRPCWithHeaders(t, server1.URL+cfg.MCPPath, session1, paidCall, map[string]string{"PAYMENT-SIGNATURE": sig})
	body1, _ := io.ReadAll(resp1.Body)
	_ = resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("paid call status=%d want=200 body=%s", resp1.StatusCode, string(body1))
	}
	if fac.verifyCalls.Load() != 1 || fac.settleCalls.Load() != 1 {
		t.Fatalf("after the paid call: verify=%d settle=%d, want 1/1", fac.verifyCalls.Load(), fac.settleCalls.Load())
	}
	server1.Close()

	// Age the stored outcome past the fixed 10-minute fallback TTL. The nonce
	// ledger entry is untouched and still has about 4 minutes to run.
	if n := agePersistedPaymentOutcomes697(t, st, 11*time.Minute); n != 1 {
		t.Fatalf("persisted payment outcomes=%d, want 1", n)
	}

	// Second server on the same store. The identical call is an exact idempotent
	// retry of the same (nonce, request) pair, so it must return the recorded
	// result without a second verify, execution or settle.
	server2 := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server2.Close()
	session2 := initializeSession(t, server2.URL+cfg.MCPPath)
	resp2 := postRPCWithHeaders(t, server2.URL+cfg.MCPPath, session2, paidCall, map[string]string{"PAYMENT-SIGNATURE": sig})
	body2, _ := io.ReadAll(resp2.Body)
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("idempotent retry after restart status=%d want=200 body=%s", resp2.StatusCode, string(body2))
	}
	if strings.TrimSpace(resp2.Header.Get("PAYMENT-RESPONSE")) == "" {
		t.Error("idempotent retry is missing the PAYMENT-RESPONSE header")
	}
	if got := fac.verifyCalls.Load(); got != 1 {
		t.Errorf("verify calls after restart=%d, want 1 (no re-verification)", got)
	}
	if got := fac.settleCalls.Load(); got != 1 {
		t.Errorf("settle calls after restart=%d, want 1 (no re-settlement)", got)
	}
}

// TestX402RestoredOutcomeStillRefusesReplay697 is the safety half of #697. The
// fix keeps a settled outcome alive for longer, so it must not make a replay
// succeed. Replay classification keys off the authorization nonce and the
// logical request, never off the presence of a cached outcome: the retained
// outcome is reachable only under its own execution key, which is
// nonce + canonical request.
//
// The test reaches the exact state the fix creates, an outcome restored past the
// 10-minute fallback with a live consumed nonce, then presents the SAME nonce
// with a DIFFERENT request. The adapter must reject it and must not verify,
// execute or settle again.
func TestX402RestoredOutcomeStillRefusesReplay697(t *testing.T) {
	fac := newFacilitatorStub(t)
	facServer := httptest.NewServer(fac)
	defer facServer.Close()

	cfg := x402EnabledTestConfig("https://resource.example.com")
	cfg.AuthMode = "none"
	cfg.X402.FacilitatorURL = facServer.URL

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(t.Context()); err != nil {
		t.Fatalf("Init store: %v", err)
	}
	defer func() { _ = st.Close() }()

	const paidCall = `{"jsonrpc":"2.0","id":697,"method":"tools/call","params":{"name":"dir2mcp_stats","arguments":{}}}`
	// A different logical request under the same nonce: same tool, different
	// arguments, so the canonical request key differs.
	const replayedCall = `{"jsonrpc":"2.0","id":698,"method":"tools/call","params":{"name":"dir2mcp_stats","arguments":{"include_skipped":true}}}`
	sig := v2PaymentSignature697("nonce-697-b", time.Now().UTC())

	server1 := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	session1 := initializeSession(t, server1.URL+cfg.MCPPath)
	resp1 := postRPCWithHeaders(t, server1.URL+cfg.MCPPath, session1, paidCall, map[string]string{"PAYMENT-SIGNATURE": sig})
	body1, _ := io.ReadAll(resp1.Body)
	_ = resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("paid call status=%d want=200 body=%s", resp1.StatusCode, string(body1))
	}
	server1.Close()

	if n := agePersistedPaymentOutcomes697(t, st, 11*time.Minute); n != 1 {
		t.Fatalf("persisted payment outcomes=%d, want 1", n)
	}

	server2 := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server2.Close()
	session2 := initializeSession(t, server2.URL+cfg.MCPPath)

	verifyBefore := fac.verifyCalls.Load()
	settleBefore := fac.settleCalls.Load()

	resp2 := postRPCWithHeaders(t, server2.URL+cfg.MCPPath, session2, replayedCall, map[string]string{"PAYMENT-SIGNATURE": sig})
	body2, _ := io.ReadAll(resp2.Body)
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("replay status=%d want=402 body=%s", resp2.StatusCode, string(body2))
	}
	if !strings.Contains(string(body2), "nonce already used") {
		t.Errorf("replay response does not name the reused nonce: %s", string(body2))
	}
	if got := fac.verifyCalls.Load(); got != verifyBefore {
		t.Errorf("verify calls during the replay=%d, want %d (no re-verification)", got, verifyBefore)
	}
	if got := fac.settleCalls.Load(); got != settleBefore {
		t.Errorf("settle calls during the replay=%d, want %d (no second settlement)", got, settleBefore)
	}

	// The retained outcome must not leak into the replay response either.
	if strings.TrimSpace(resp2.Header.Get("PAYMENT-RESPONSE")) != "" {
		t.Error("replay response carries a PAYMENT-RESPONSE header")
	}

	// The legitimate retry of the ORIGINAL request still works, so the rejection
	// above is a request mismatch and not a lost outcome.
	resp3 := postRPCWithHeaders(t, server2.URL+cfg.MCPPath, session2, paidCall, map[string]string{"PAYMENT-SIGNATURE": sig})
	body3, _ := io.ReadAll(resp3.Body)
	defer func() { _ = resp3.Body.Close() }()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("idempotent retry status=%d want=200 body=%s", resp3.StatusCode, string(body3))
	}
}
