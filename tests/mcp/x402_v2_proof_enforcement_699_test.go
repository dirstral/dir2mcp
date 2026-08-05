package tests

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/mcp"
)

// #699: the adapter parsed the x402 v2 primitives as optional best effort. An
// opaque or partially malformed PAYMENT-SIGNATURE left HasNonce/HasWindow
// false, the replay key fell back to SHA-256 of the signature, the window check
// returned success, and the request proceeded to facilitator Verify and to tool
// execution EVEN IN `required` MODE.
//
// The adapter spec makes all of it normative: the authorization nonce "MUST be
// treated as single-use", "Replay detection MUST key off the authorization
// nonce", the adapter "MUST reject a proof whose validAfter/validBefore window
// does not cover the current time", and it "MUST enforce the matched
// PaymentRequirements.maxTimeoutSeconds as the maximum age between challenge and
// PAYMENT-SIGNATURE" while "MUST NOT rely on the facilitator alone for the time
// check".
//
// So a facilitator that approved a proof shape the adapter could not inspect
// bypassed every local control, and the signature-derived ledger entry expired
// on the local TTL, after which the identical timeless proof was classified
// fresh and could execute and settle again.
//
// Every test here asserts the facilitator's Verify was NEVER called. That is
// the actual requirement: rejecting after Verify would still have asked a third
// party to approve a proof the adapter is required to refuse itself.

// v2Payload builds a syntactically valid x402 v2 PAYMENT-SIGNATURE.
func v2Payload(t *testing.T, nonce string, validAfter, validBefore int64) string {
	t.Helper()
	auth := map[string]interface{}{}
	if nonce != "" {
		auth["nonce"] = nonce
	}
	if validAfter >= 0 {
		auth["validAfter"] = validAfter
	}
	if validBefore >= 0 {
		auth["validBefore"] = validBefore
	}
	// A complete v2 envelope, not just the authorization block: the facilitator
	// client detects the version from x402Version and refuses a payload without
	// it, so a fixture carrying only the authorization would be rejected
	// downstream and would tell us nothing about the adapter-side check.
	raw, err := json.Marshal(map[string]interface{}{
		"x402Version": 2,
		"scheme":      "exact",
		"network":     "eip155:8453",
		"payload":     map[string]interface{}{"authorization": auth},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func freshNonce() string {
	return "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
}

// callWithProof runs one paid tools/call in x402.mode=required and returns the
// HTTP status plus how many times the facilitator's Verify was invoked.
func callWithProof(t *testing.T, signature string) (int, int64, string) {
	t.Helper()
	fac := newFacilitatorStub(t)
	fac.verifyStatus = http.StatusOK
	fac.settleStatus = http.StatusOK
	fac.verifyBody = `{"ok":true,"isValid":true,"payer":"payer-1"}`
	fac.settleBody = `{"ok":true,"success":true,"transaction":"abc123","txHash":"abc123","network":"eip155:8453"}`
	facServer := httptest.NewServer(fac)
	defer facServer.Close()

	cfg := x402EnabledTestConfig("https://resource.example.com")
	cfg.AuthMode = "none"
	cfg.X402.Mode = "required"
	cfg.X402.FacilitatorURL = facServer.URL

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPCWithHeaders(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":700,"method":"tools/call","params":{"name":"dir2mcp_stats","arguments":{}}}`,
		map[string]string{"PAYMENT-SIGNATURE": signature})
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, fac.verifyCalls.Load(), string(body)
}

func TestRequiredModeRejectsProofsMissingAV2Primitive(t *testing.T) {
	now := time.Now().UTC().Unix()
	for _, tc := range []struct {
		name      string
		signature string
	}{
		{
			// The exact fixture the existing suite used to demonstrate success.
			name:      "opaque legacy token",
			signature: "signed-payment-payload",
		},
		{
			name:      "no nonce",
			signature: v2Payload(t, "", now-10, now+60),
		},
		{
			name:      "empty nonce",
			signature: v2Payload(t, "   ", now-10, now+60),
		},
		{
			name:      "no validity window",
			signature: v2Payload(t, freshNonce(), -1, -1),
		},
		{
			name:      "validBefore only",
			signature: v2Payload(t, freshNonce(), -1, now+60),
		},
		{
			// The age the spec mandates cannot be computed without validAfter,
			// which is how the requirement stayed nominal.
			name:      "validAfter unset (zero)",
			signature: v2Payload(t, freshNonce(), 0, now+60),
		},
		{
			name:      "window already expired",
			signature: v2Payload(t, freshNonce(), now-600, now-300),
		},
		{
			name:      "window not yet valid",
			signature: v2Payload(t, freshNonce(), now+300, now+600),
		},
		{
			name:      "empty window",
			signature: v2Payload(t, freshNonce(), now, now),
		},
		{
			// Opened arbitrarily far in the past, closes soon: the shape the
			// old remaining-time-only check let straight through.
			name:      "stale window closing soon",
			signature: v2Payload(t, freshNonce(), now-86400, now+30),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, verifyCalls, body := callWithProof(t, tc.signature)
			if status == http.StatusOK {
				t.Fatalf("required mode executed the tool for %s; body=%s", tc.name, body)
			}
			if verifyCalls != 0 {
				t.Fatalf("facilitator Verify was called %d times for %s; the adapter must refuse this itself rather than ask a third party",
					verifyCalls, tc.name)
			}
		})
	}
}

// TestRequiredModeAcceptsAWellFormedV2Proof is the other half: the enforcement
// must not refuse a proof that satisfies the spec, or it would simply break
// payments instead of securing them.
func TestRequiredModeAcceptsAWellFormedV2Proof(t *testing.T) {
	now := time.Now().UTC().Unix()
	status, verifyCalls, body := callWithProof(t, v2Payload(t, freshNonce(), now-5, now+60))
	if status != http.StatusOK {
		t.Fatalf("a well-formed v2 proof was rejected: status=%d body=%s", status, body)
	}
	if verifyCalls != 1 {
		t.Fatalf("facilitator Verify called %d times, want 1", verifyCalls)
	}
}

// TestModeOnKeepsItsDocumentedTolerance: `on` is specified as fail-open on
// incomplete input, and CLAUDE.md records that as its meaning. Tightening it
// here would change a documented mode's contract under cover of a security fix.
func TestModeOnKeepsItsDocumentedTolerance(t *testing.T) {
	fac := newFacilitatorStub(t)
	fac.verifyStatus = http.StatusOK
	fac.settleStatus = http.StatusOK
	fac.verifyBody = `{"ok":true,"isValid":true,"payer":"payer-1"}`
	fac.settleBody = `{"ok":true,"success":true,"transaction":"abc123","txHash":"abc123","network":"eip155:8453"}`
	facServer := httptest.NewServer(fac)
	defer facServer.Close()

	cfg := x402EnabledTestConfig("https://resource.example.com")
	cfg.AuthMode = "none"
	cfg.X402.Mode = "on"
	cfg.X402.FacilitatorURL = facServer.URL

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPCWithHeaders(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":701,"method":"tools/call","params":{"name":"dir2mcp_stats","arguments":{}}}`,
		map[string]string{"PAYMENT-SIGNATURE": "signed-payment-payload"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("mode=on rejected an opaque proof: status=%d body=%s", resp.StatusCode, payload)
	}
}

// TestRequiredModeRejectionNamesTheMissingPrimitive: an operator debugging a
// declined payment needs to know WHICH control refused it, and the message must
// not be so vague that a client cannot correct its payload.
func TestRequiredModeRejectionNamesTheMissingPrimitive(t *testing.T) {
	now := time.Now().UTC().Unix()
	for _, tc := range []struct {
		signature string
		want      string
	}{
		{v2Payload(t, "", now-10, now+60), "nonce"},
		{v2Payload(t, freshNonce(), -1, -1), "validity window"},
		{v2Payload(t, freshNonce(), now-86400, now+30), "older than"},
	} {
		_, _, body := callWithProof(t, tc.signature)
		if !strings.Contains(strings.ToLower(body), tc.want) {
			t.Fatalf("rejection body does not name %q: %s", tc.want, body)
		}
	}
}
