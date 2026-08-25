package conformance

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dirstral/dir2mcp/internal/protocol"
)

// This file pins the handshake enforcement added for issue #656 on the
// PRODUCTION transport: a session that skipped notifications/initialized gets
// its non-lifecycle requests rejected (bs-005), and a post-initialize request
// that names an unsupported MCP-Protocol-Version gets HTTP 400 (bs-004; MCP
// 2025-11-25 transport). A missing header stays accepted: bs-004 places that
// MUST on the client, and the canonical transport only mandates rejection of
// an invalid or unsupported value.

// decodeCanonicalCode extracts error.data.code from a JSON-RPC error envelope.
func decodeCanonicalCode(t *testing.T, body []byte) string {
	t.Helper()
	var envelope struct {
		Error *struct {
			Data *struct {
				Code string `json:"code"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode error envelope: %v body=%s", err, body)
	}
	if envelope.Error == nil || envelope.Error.Data == nil {
		t.Fatalf("expected error.data in body=%s", body)
	}
	return envelope.Error.Data.Code
}

// TestHandshake_RequestBeforeInitializedIsRejected_656 reproduces the issue
// #656 sequence: initialize succeeds, the client skips
// notifications/initialized, and tool traffic follows. The server must reject
// that traffic until the notification arrives, then serve it.
func TestHandshake_RequestBeforeInitializedIsRejected_656(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()
	mcpURL := srv.URL + cfg.MCPPath

	sid := initSessionWithoutInitialized(t, mcpURL)

	for name, body := range map[string]string{
		"tools/list": `{"jsonrpc":"2.0","id":30,"method":"tools/list","params":{}}`,
		"tools/call": statsCallBody(31),
	} {
		resp := sendRPC(t, mcpURL, sid, body, nil)
		raw := readBody(t, resp)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s before initialized: status=%d want=400 body=%s", name, resp.StatusCode, raw)
		}
		code, _ := decodeRPCError(t, raw)
		if code != -32002 {
			t.Fatalf("%s before initialized: code=%d want=-32002 body=%s", name, code, raw)
		}
		if canonical := decodeCanonicalCode(t, raw); canonical != protocol.ErrorCodeSessionNotInitialized {
			t.Fatalf("%s before initialized: data.code=%q want=%q body=%s", name, canonical, protocol.ErrorCodeSessionNotInitialized, raw)
		}
	}

	// The same session recovers as soon as the client completes the handshake.
	sendInitialized(t, mcpURL, sid)
	resp := sendRPC(t, mcpURL, sid, `{"jsonrpc":"2.0","id":32,"method":"tools/list","params":{}}`, nil)
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !hasResult(raw) {
		t.Fatalf("tools/list after completing the handshake: status=%d body=%s", resp.StatusCode, raw)
	}
}

// TestHandshake_DirectHandlerRejectsBeforeInitialized_656 pins the same bs-005
// gate on the direct handler chain, so the two chains cannot drift.
func TestHandshake_DirectHandlerRejectsBeforeInitialized_656(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newDirectHandlerServer(t, cfg)
	mcpURL := srv.URL + cfg.MCPPath

	sid := initSessionWithoutInitialized(t, mcpURL)
	resp := sendRPC(t, mcpURL, sid, `{"jsonrpc":"2.0","id":33,"method":"tools/list","params":{}}`, nil)
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("direct handler tools/list before initialized: status=%d want=400 body=%s", resp.StatusCode, raw)
	}
	if canonical := decodeCanonicalCode(t, raw); canonical != protocol.ErrorCodeSessionNotInitialized {
		t.Fatalf("direct handler data.code=%q want=%q body=%s", canonical, protocol.ErrorCodeSessionNotInitialized, raw)
	}

	sendInitialized(t, mcpURL, sid)
	ok := sendRPC(t, mcpURL, sid, `{"jsonrpc":"2.0","id":34,"method":"tools/list","params":{}}`, nil)
	okBody := readBody(t, ok)
	if ok.StatusCode != http.StatusOK || !hasResult(okBody) {
		t.Fatalf("direct handler tools/list after handshake: status=%d body=%s", ok.StatusCode, okBody)
	}
}

// TestHandshake_UnsupportedProtocolVersionIsRejected_656 verifies the bs-004
// header rule with this server's canonical error contract: a post-initialize
// request naming a version the server does not support gets HTTP 400 with
// UNSUPPORTED_PROTOCOL_VERSION, on tool traffic and on the initialized
// notification alike.
func TestHandshake_UnsupportedProtocolVersionIsRejected_656(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()
	mcpURL := srv.URL + cfg.MCPPath

	sid := initSession(t, mcpURL)
	wrong := map[string]string{protocol.MCPProtocolVersionHeader: "1999-01-01"}

	resp := sendRPC(t, mcpURL, sid, `{"jsonrpc":"2.0","id":40,"method":"tools/list","params":{}}`, wrong)
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("tools/list with unsupported version: status=%d want=400 body=%s", resp.StatusCode, raw)
	}
	code, _ := decodeRPCError(t, raw)
	if code != -32600 {
		t.Fatalf("tools/list with unsupported version: code=%d want=-32600 body=%s", code, raw)
	}
	if canonical := decodeCanonicalCode(t, raw); canonical != protocol.ErrorCodeUnsupportedProtocolVersion {
		t.Fatalf("data.code=%q want=%q body=%s", canonical, protocol.ErrorCodeUnsupportedProtocolVersion, raw)
	}

	// The rule covers every post-initialize message, notifications included.
	sid2 := initSessionWithoutInitialized(t, mcpURL)
	notif := sendRPC(t, mcpURL, sid2, `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`, wrong)
	notifBody := readBody(t, notif)
	if notif.StatusCode != http.StatusBadRequest {
		t.Fatalf("notifications/initialized with unsupported version: status=%d want=400 body=%s", notif.StatusCode, notifBody)
	}
}

// TestHandshake_MissingProtocolVersionHeaderStaysAccepted_656 documents the
// permissive half of the contract: bs-004 puts the MUST-send on the client,
// and the canonical MCP transport does not require a rejection when the
// version is identifiable from the session negotiated at initialize. External
// clients that omit the header keep working after a complete handshake.
func TestHandshake_MissingProtocolVersionHeaderStaysAccepted_656(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()
	mcpURL := srv.URL + cfg.MCPPath

	sid := initSession(t, mcpURL)
	resp := sendRPC(t, mcpURL, sid, `{"jsonrpc":"2.0","id":41,"method":"tools/list","params":{}}`, nil)
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !hasResult(raw) {
		t.Fatalf("tools/list without the version header: status=%d body=%s", resp.StatusCode, raw)
	}
}

// TestHandshake_ValidSequenceIsServed_656 pins the fully conformant wire
// sequence a client like the bundled CLI now sends: initialize, a 202 on
// notifications/initialized, then tool traffic carrying the pinned version
// header.
func TestHandshake_ValidSequenceIsServed_656(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()
	mcpURL := srv.URL + cfg.MCPPath
	pinned := map[string]string{protocol.MCPProtocolVersionHeader: protocol.ProtocolDefaultVersion}

	sid := initSessionWithoutInitialized(t, mcpURL)

	notif := sendRPC(t, mcpURL, sid, `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`, pinned)
	notifBody := readBody(t, notif)
	if notif.StatusCode != http.StatusAccepted {
		t.Fatalf("notifications/initialized: status=%d want=202 body=%s", notif.StatusCode, notifBody)
	}
	if len(notifBody) != 0 {
		t.Fatalf("notifications/initialized: expected empty 202 body, got %s", notifBody)
	}

	resp := sendRPC(t, mcpURL, sid, statsCallBody(42), pinned)
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !hasResult(raw) {
		t.Fatalf("tools/call after the valid handshake: status=%d body=%s", resp.StatusCode, raw)
	}
}
