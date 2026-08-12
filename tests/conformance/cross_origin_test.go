package conformance

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/protocol"
)

// This file covers configured cross-origin access on the PRODUCTION transport
// (issue #652). A browser client never sends a preflight alone: it sends the
// preflight and then the real request. Both have to pass, and the real response
// has to carry Access-Control-Allow-Origin, or the browser drops the body.
//
// The production transport gives MCP-path POST and DELETE to SDKTransport, so
// these assertions run through the SDK's own cross-origin guard as well as
// through the repository's allowed_origins check. A test that only drives
// Server.Handler() cannot see either.

// allowedTestOrigin is an explicitly configured, cross-site browser origin.
const allowedTestOrigin = "https://allowed.example.com"

// browserCrossSiteHeaders returns the headers a browser attaches to a cross-site
// fetch. Sec-Fetch-Site is the marker the SDK's cross-origin guard reads first.
func browserCrossSiteHeaders(origin string) map[string]string {
	return map[string]string{
		"Origin":         origin,
		"Sec-Fetch-Site": "cross-site",
		"Sec-Fetch-Mode": "cors",
	}
}

// initSessionFromOrigin runs initialize with the given headers and returns the
// session id. It fails the test when the status is not 200 or the response
// omits Access-Control-Allow-Origin for the given origin.
func initSessionFromOrigin(t *testing.T, mcpURL, origin string, headers map[string]string) string {
	t.Helper()
	resp := sendRPC(t, mcpURL, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, headers)
	sid := resp.Header.Get(protocol.MCPSessionHeader)
	acao := resp.Header.Get("Access-Control-Allow-Origin")
	status := resp.StatusCode
	body := readBody(t, resp)
	if status != http.StatusOK {
		t.Fatalf("initialize from %s: status=%d want=200 body=%s", origin, status, body)
	}
	if acao != origin {
		t.Fatalf("initialize from %s: Access-Control-Allow-Origin=%q want=%q", origin, acao, origin)
	}
	if sid == "" {
		t.Fatalf("initialize from %s returned no session id", origin)
	}
	return sid
}

// TestProduction_CORSAllowedOriginPreflightThenPostAreServed pins the whole
// browser sequence for an explicitly allowed origin: the preflight, then the
// real POST, then the CORS header on the POST response.
//
// This replaces the preflight-only assertion. The preflight passed on its own
// while the POST that follows it got 403, so the suite reported green on a
// configuration that no browser client can use (issue #652).
func TestProduction_CORSAllowedOriginPreflightThenPostAreServed(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.AllowedOrigins = append(cfg.AllowedOrigins, allowedTestOrigin)
	srv := newServer(t, cfg)
	defer srv.Close()
	mcpURL := srv.URL + cfg.MCPPath

	// 1. The preflight. OPTIONS is the one MCP-path verb the SDK transport hands
	// back to Server.Handler(), so this also pins that routing.
	req, err := http.NewRequest(http.MethodOptions, mcpURL, nil)
	if err != nil {
		t.Fatalf("create OPTIONS request: %v", err)
	}
	req.Header.Set("Origin", allowedTestOrigin)
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("do OPTIONS: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight: status=%d want=204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != allowedTestOrigin {
		t.Fatalf("preflight Access-Control-Allow-Origin=%q want=%q", got, allowedTestOrigin)
	}
	// The transport serves DELETE session termination, so the preflight must
	// advertise it. A browser that reads only "POST, OPTIONS" never sends the
	// DELETE that ends the session.
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodOptions} {
		if acam := resp.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(acam, method) {
			t.Errorf("preflight Access-Control-Allow-Methods=%q must contain %s", acam, method)
		}
	}

	// 2. The real POST that the preflight authorized.
	headers := browserCrossSiteHeaders(allowedTestOrigin)
	sid := initSessionFromOrigin(t, mcpURL, allowedTestOrigin, headers)

	// 3. A tool call on the same session, from the same origin.
	call := sendRPC(t, mcpURL, sid, statsCallBody(60), headers)
	acao := call.Header.Get("Access-Control-Allow-Origin")
	status := call.StatusCode
	body := readBody(t, call)
	if status != http.StatusOK || !hasResult(body) {
		t.Fatalf("tools/call from an allowed origin: status=%d body=%s", status, body)
	}
	if acao != allowedTestOrigin {
		t.Fatalf("tools/call Access-Control-Allow-Origin=%q want=%q", acao, allowedTestOrigin)
	}
}

// TestProduction_CORSAllowedOriginWithoutSecFetchSiteIsServed covers a client
// that sends Origin but no Sec-Fetch-Site marker. The guard then compares Origin
// against Host, which never matches a cross-origin browser client, so the
// configured allowlist has to decide.
func TestProduction_CORSAllowedOriginWithoutSecFetchSiteIsServed(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.AllowedOrigins = append(cfg.AllowedOrigins, allowedTestOrigin)
	srv := newServer(t, cfg)
	defer srv.Close()

	initSessionFromOrigin(t, srv.URL+cfg.MCPPath, allowedTestOrigin,
		map[string]string{"Origin": allowedTestOrigin})
}

// TestProduction_CORSPortlessAllowlistEntryMatchesAnyPort covers the default
// configuration. `allowed_origins` ships with the portless entry
// "http://localhost", which matches any port, so a browser app served from
// http://localhost:5173 is an allowed origin.
func TestProduction_CORSPortlessAllowlistEntryMatchesAnyPort(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.AllowedOrigins = []string{"http://localhost"}
	srv := newServer(t, cfg)
	defer srv.Close()

	const origin = "http://localhost:5173"
	initSessionFromOrigin(t, srv.URL+cfg.MCPPath, origin, browserCrossSiteHeaders(origin))
}

// TestProduction_CORSBareHostAllowlistEntryStaysNarrow covers the bare host
// allowlist form, and asserts the property the whole design rests on: the SDK's
// trusted-origin set is never wider than the configured policy.
//
// A bare host entry ("localhost") matches any scheme and any port, so it cannot
// become an exact trusted origin. It is therefore judged per request by the
// allowlist. This test pins both halves of that from the outside: the entry
// serves a port the config never named, and it refuses every other host.
func TestProduction_CORSBareHostAllowlistEntryStaysNarrow(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.AllowedOrigins = []string{"localhost"}
	srv := newServer(t, cfg)
	defer srv.Close()
	mcpURL := srv.URL + cfg.MCPPath

	const origin = "http://localhost:5173"
	initSessionFromOrigin(t, mcpURL, origin, browserCrossSiteHeaders(origin))

	const other = "https://localhost.evil.example.com"
	resp := sendRPC(t, mcpURL, "", `{"jsonrpc":"2.0","id":67,"method":"initialize","params":{}}`,
		browserCrossSiteHeaders(other))
	acao := resp.Header.Get("Access-Control-Allow-Origin")
	status := resp.StatusCode
	body := readBody(t, resp)
	if status != http.StatusForbidden {
		t.Fatalf("origin %s: status=%d want=403 body=%s", other, status, body)
	}
	if acao != "" {
		t.Fatalf("origin %s: Access-Control-Allow-Origin=%q want empty", other, acao)
	}
	if !bytes.Contains(body, []byte("FORBIDDEN_ORIGIN")) {
		t.Fatalf("origin %s: body=%s want the canonical FORBIDDEN_ORIGIN contract", other, body)
	}
}

// TestProduction_CORSAllowlistEntryCaseIsIgnored covers an allowlist entry that
// an operator wrote with capitals. A browser lowercases the scheme and the host
// in the Origin header, so the entry has to be matched without case.
func TestProduction_CORSAllowlistEntryCaseIsIgnored(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.AllowedOrigins = []string{"HTTPS://Allowed.Example.COM"}
	srv := newServer(t, cfg)
	defer srv.Close()

	initSessionFromOrigin(t, srv.URL+cfg.MCPPath, allowedTestOrigin,
		browserCrossSiteHeaders(allowedTestOrigin))
}

// TestProduction_CORSAllowedOriginDeleteTerminatesTheSession covers the DELETE
// half of the same policy: a browser that ends its session cross-origin.
func TestProduction_CORSAllowedOriginDeleteTerminatesTheSession(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.AllowedOrigins = append(cfg.AllowedOrigins, allowedTestOrigin)
	srv := newServer(t, cfg)
	defer srv.Close()
	mcpURL := srv.URL + cfg.MCPPath

	headers := browserCrossSiteHeaders(allowedTestOrigin)
	sid := initSessionFromOrigin(t, mcpURL, allowedTestOrigin, headers)

	req, err := http.NewRequest(http.MethodDelete, mcpURL, nil)
	if err != nil {
		t.Fatalf("create DELETE request: %v", err)
	}
	req.Header.Set(protocol.MCPSessionHeader, sid)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("do DELETE: %v", err)
	}
	acao := resp.Header.Get("Access-Control-Allow-Origin")
	status := resp.StatusCode
	body := readBody(t, resp)
	if status != http.StatusNoContent {
		t.Fatalf("cross-origin DELETE: status=%d want=204 body=%s", status, body)
	}
	if acao != allowedTestOrigin {
		t.Fatalf("cross-origin DELETE Access-Control-Allow-Origin=%q want=%q", acao, allowedTestOrigin)
	}

	after := sendRPC(t, mcpURL, sid, statsCallBody(61), headers)
	afterBody := readBody(t, after)
	if after.StatusCode != http.StatusNotFound {
		t.Fatalf("replay after DELETE: status=%d want=404 body=%s", after.StatusCode, afterBody)
	}
}

// TestProduction_DisallowedOriginIsRefused proves the fix does not read as
// "allow everything". Every origin the allowlist does not name is still refused
// with the canonical FORBIDDEN_ORIGIN contract and no CORS header, whatever the
// browser markers say.
func TestProduction_DisallowedOriginIsRefused(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.AllowedOrigins = []string{"http://localhost", allowedTestOrigin}
	srv := newServer(t, cfg)
	defer srv.Close()
	mcpURL := srv.URL + cfg.MCPPath

	cases := []struct {
		name    string
		origin  string
		headers map[string]string
	}{
		{
			name:    "unrelated origin, cross-site marker",
			origin:  "https://evil.example.com",
			headers: browserCrossSiteHeaders("https://evil.example.com"),
		},
		{
			name:    "unrelated origin, no marker",
			origin:  "https://evil.example.com",
			headers: map[string]string{"Origin": "https://evil.example.com"},
		},
		{
			// A portless allowlist entry matches any port of that host, and
			// nothing else: it is not a suffix wildcard.
			name:    "host that only looks allowed",
			origin:  "http://localhost.evil.example.com",
			headers: browserCrossSiteHeaders("http://localhost.evil.example.com"),
		},
		{
			// The allowlist entry names https; the scheme must match.
			name:    "allowed host on the wrong scheme",
			origin:  "http://allowed.example.com",
			headers: browserCrossSiteHeaders("http://allowed.example.com"),
		},
		{
			// A caller controls its own request headers, so it can claim any
			// Sec-Fetch-Site value. The allowlist decides on the origin alone.
			name:   "disallowed origin that claims to be same-origin",
			origin: "https://evil.example.com",
			headers: map[string]string{
				"Origin":         "https://evil.example.com",
				"Sec-Fetch-Site": "same-origin",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := sendRPC(t, mcpURL, "", `{"jsonrpc":"2.0","id":62,"method":"initialize","params":{}}`, tc.headers)
			acao := resp.Header.Get("Access-Control-Allow-Origin")
			status := resp.StatusCode
			body := readBody(t, resp)
			if status != http.StatusForbidden {
				t.Fatalf("origin %s: status=%d want=403 body=%s", tc.origin, status, body)
			}
			if acao != "" {
				t.Fatalf("origin %s: Access-Control-Allow-Origin=%q want empty", tc.origin, acao)
			}
			if !bytes.Contains(body, []byte("FORBIDDEN_ORIGIN")) {
				t.Fatalf("origin %s: body=%s want the canonical FORBIDDEN_ORIGIN contract", tc.origin, body)
			}
		})
	}
}

// TestProduction_DisallowedOriginPreflightGetsNoHeaders covers the preflight for
// an origin the allowlist does not name. The preflight still answers 204, because
// OPTIONS is a safe method, but it must not advertise the origin.
func TestProduction_DisallowedOriginPreflightGetsNoHeaders(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.AllowedOrigins = append(cfg.AllowedOrigins, allowedTestOrigin)
	srv := newServer(t, cfg)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodOptions, srv.URL+cfg.MCPPath, nil)
	if err != nil {
		t.Fatalf("create OPTIONS request: %v", err)
	}
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("do OPTIONS: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	for _, header := range []string{
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Methods",
		"Access-Control-Allow-Headers",
	} {
		if got := resp.Header.Get(header); got != "" {
			t.Errorf("disallowed preflight %s=%q want empty", header, got)
		}
	}
}

// TestProduction_DisallowedOriginDeleteIsRefused covers the DELETE verb for an
// origin the allowlist does not name. A live session id must not let a
// disallowed origin end the session.
func TestProduction_DisallowedOriginDeleteIsRefused(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.AllowedOrigins = append(cfg.AllowedOrigins, allowedTestOrigin)
	srv := newServer(t, cfg)
	defer srv.Close()
	mcpURL := srv.URL + cfg.MCPPath

	sid := initSession(t, mcpURL)

	req, err := http.NewRequest(http.MethodDelete, mcpURL, nil)
	if err != nil {
		t.Fatalf("create DELETE request: %v", err)
	}
	req.Header.Set(protocol.MCPSessionHeader, sid)
	for k, v := range browserCrossSiteHeaders("https://evil.example.com") {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("do DELETE: %v", err)
	}
	acao := resp.Header.Get("Access-Control-Allow-Origin")
	status := resp.StatusCode
	body := readBody(t, resp)
	if status != http.StatusForbidden {
		t.Fatalf("disallowed DELETE: status=%d want=403 body=%s", status, body)
	}
	if acao != "" {
		t.Fatalf("disallowed DELETE Access-Control-Allow-Origin=%q want empty", acao)
	}
	if !bytes.Contains(body, []byte("FORBIDDEN_ORIGIN")) {
		t.Fatalf("disallowed DELETE body=%s want the canonical FORBIDDEN_ORIGIN contract", body)
	}

	// The session survives the refused DELETE.
	after := sendRPC(t, mcpURL, sid, statsCallBody(66), nil)
	afterBody := readBody(t, after)
	if after.StatusCode != http.StatusOK || !hasResult(afterBody) {
		t.Fatalf("session after a refused DELETE: status=%d body=%s", after.StatusCode, afterBody)
	}
}

// TestProduction_CORSHeaderIsOnAnErrorResponseToo covers the error half of the
// contract. A browser reads an error envelope through the same rule as a result
// envelope, so a response without Access-Control-Allow-Origin hides the reason
// for the failure from the client.
func TestProduction_CORSHeaderIsOnAnErrorResponseToo(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.AllowedOrigins = append(cfg.AllowedOrigins, allowedTestOrigin)
	srv := newServer(t, cfg)
	defer srv.Close()

	// An unknown session is the canonical SESSION_NOT_FOUND error path.
	resp := sendRPC(t, srv.URL+cfg.MCPPath, "sess_does_not_exist", statsCallBody(65),
		browserCrossSiteHeaders(allowedTestOrigin))
	acao := resp.Header.Get("Access-Control-Allow-Origin")
	status := resp.StatusCode
	body := readBody(t, resp)
	if status != http.StatusNotFound {
		t.Fatalf("unknown session: status=%d want=404 body=%s", status, body)
	}
	if acao != allowedTestOrigin {
		t.Fatalf("error response Access-Control-Allow-Origin=%q want=%q body=%s", acao, allowedTestOrigin, body)
	}
	if !hasError(body) {
		t.Fatalf("error response body=%s want a JSON-RPC error envelope", body)
	}
}

// TestProduction_CrossSiteMarkerWithoutOriginIsRefused pins the guard we keep.
// The SDK's cross-origin protection stays enabled, so a request that declares
// itself cross-site and sends no Origin at all is still refused. The allowlist
// cannot adjudicate a request with no origin, and this case must not become
// reachable when the two checks are made to agree.
//
// The refusal also has to be readable. The SDK answers this case with a bare
// text body from inside its handler, so the transport runs the same check itself
// and answers with the canonical FORBIDDEN_ORIGIN envelope. An opaque 403 that
// names no code is what made issue #652 hard to diagnose.
func TestProduction_CrossSiteMarkerWithoutOriginIsRefused(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.AllowedOrigins = append(cfg.AllowedOrigins, allowedTestOrigin)
	srv := newServer(t, cfg)
	defer srv.Close()

	resp := sendRPC(t, srv.URL+cfg.MCPPath, "", `{"jsonrpc":"2.0","id":63,"method":"initialize","params":{}}`,
		map[string]string{"Sec-Fetch-Site": "cross-site"})
	status := resp.StatusCode
	body := readBody(t, resp)
	if status != http.StatusForbidden {
		t.Fatalf("cross-site marker without an Origin: status=%d want=403 body=%s", status, body)
	}
	if !hasError(body) || !bytes.Contains(body, []byte("FORBIDDEN_ORIGIN")) {
		t.Fatalf("cross-site marker without an Origin: body=%s want the canonical FORBIDDEN_ORIGIN envelope", body)
	}
}

// TestProduction_CrossOriginStillNeedsTheToken pins the order of the two gates.
// Authorization runs before the origin decision, so an allowed origin is not a
// way past authentication. This matters because the shipped allowlist names the
// local host, and a scheme+host entry matches any port: any local page is an
// allowed origin, and only the bearer token keeps it out.
func TestProduction_CrossOriginStillNeedsTheToken(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.AuthMode = "token"
	cfg.ResolvedAuthToken = "supersecret"
	cfg.AllowedOrigins = append(cfg.AllowedOrigins, allowedTestOrigin)
	srv := newServer(t, cfg)
	defer srv.Close()
	mcpURL := srv.URL + cfg.MCPPath

	headers := browserCrossSiteHeaders(allowedTestOrigin)
	resp := sendRPC(t, mcpURL, "", `{"jsonrpc":"2.0","id":68,"method":"initialize","params":{}}`, headers)
	status := resp.StatusCode
	body := readBody(t, resp)
	if status != http.StatusUnauthorized {
		t.Fatalf("allowed origin without a token: status=%d want=401 body=%s", status, body)
	}

	// The same request with the token is served, so the token is the only thing
	// that was missing.
	headers["Authorization"] = "Bearer supersecret"
	initSessionFromOrigin(t, mcpURL, allowedTestOrigin, headers)
}

// TestProduction_CharsetQualifiedContentTypeIsServed is a regression test for
// issue #841, which this PR does not fix. It is committed and skipped so the
// conformance suite records the gap instead of staying green over it.
//
// The SDK compares Content-Type for exact equality with "application/json",
// while this server accepts the media type plus parameters. So a client that
// sends the RFC-valid "application/json; charset=utf-8" gets a plain-text 415
// from inside the SDK. Remove the skip with the #841 fix.
func TestProduction_CharsetQualifiedContentTypeIsServed(t *testing.T) {
	t.Skip("blocked on #841: the SDK requires an exact application/json Content-Type")
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+cfg.MCPPath,
		strings.NewReader(`{"jsonrpc":"2.0","id":69,"method":"initialize","params":{}}`))
	if err != nil {
		t.Fatalf("create POST request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("do POST: %v", err)
	}
	status := resp.StatusCode
	body := readBody(t, resp)
	if status != http.StatusOK || !hasResult(body) {
		t.Fatalf("charset-qualified Content-Type: status=%d body=%s", status, body)
	}
}

// TestProduction_DNSRebindingProtectionIsKept pins the second SDK guard. A
// loopback listener refuses a request whose Host header names another host, which
// is the DNS-rebinding mitigation. Configuring the cross-origin guard must not
// switch it off.
//
// This test passes before and after the #652 fix, for the same reason as the test
// above: it protects the fix, it does not measure the bug.
func TestProduction_DNSRebindingProtectionIsKept(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.AllowedOrigins = append(cfg.AllowedOrigins, allowedTestOrigin)
	srv := newServer(t, cfg)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+cfg.MCPPath,
		strings.NewReader(`{"jsonrpc":"2.0","id":64,"method":"initialize","params":{}}`))
	if err != nil {
		t.Fatalf("create POST request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// The connection is to 127.0.0.1; the Host header claims another name.
	req.Host = "rebound.example.com"
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("do POST: %v", err)
	}
	status := resp.StatusCode
	body := readBody(t, resp)
	if status != http.StatusForbidden {
		t.Fatalf("rebound Host header: status=%d want=403 body=%s", status, body)
	}
}
