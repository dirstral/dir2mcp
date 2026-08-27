package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/buildinfo"
	"github.com/dirstral/dir2mcp/internal/protocol"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// SDKTransport serves MCP over HTTP using the official MCP Go SDK for the
// transport and protocol layers.  When x402 is enabled, tools/call requests are
// routed through the existing payment-aware path so that payment headers and
// replay behavior stay identical to the legacy implementation.
type SDKTransport struct {
	server        *Server
	listener      net.Listener
	certFile      string
	keyFile       string
	shutdownGrace time.Duration
	originGuard   *sdkOriginGuard
}

// DefaultShutdownGrace is how long Serve waits for in-flight MCP requests to
// finish after its context is cancelled.
const DefaultShutdownGrace = 5 * time.Second

// NewSDKTransport constructs an SDKTransport. certFile and keyFile are
// optional; both must be non-empty to enable TLS.
func NewSDKTransport(server *Server, listener net.Listener, certFile, keyFile string) *SDKTransport {
	transport := &SDKTransport{
		server:        server,
		listener:      listener,
		certFile:      certFile,
		keyFile:       keyFile,
		shutdownGrace: DefaultShutdownGrace,
	}
	if server != nil {
		// The SDK's cross-origin guard is built from the same allowed_origins
		// policy this server enforces, so the two cannot disagree (issue #652).
		transport.originGuard = newSDKOriginGuard(server.cfg.AllowedOrigins)
		// This transport serves DELETE session termination on the MCP path, so
		// the CORS preflight may advertise it.
		server.markSessionTerminationServed()
	}
	return transport
}

// SetShutdownGrace sets how long Serve waits for in-flight MCP requests after
// its context is cancelled. A value of zero or less keeps the current value.
//
// The caller owns the shutdown budget (issue #688). It must wait for Serve to
// return before it closes the index or the store that the in-flight handlers
// still read, so it also needs to know when Serve stops waiting.
func (t *SDKTransport) SetShutdownGrace(d time.Duration) {
	if t == nil || d <= 0 {
		return
	}
	t.shutdownGrace = d
}

// newMCPHTTPServer builds the http.Server for the MCP endpoint with timeouts
// suited to long-running, LLM/OCR-backed tool calls. Crucially WriteTimeout is
// 0 (disabled): the net/http write deadline spans the whole handler+write, so a
// fixed value would kill legitimately slow tool calls mid-flight and surface as
// "Failed to call tool" on the client (issue #362). ReadHeaderTimeout bounds the
// real slowloris vector and IdleTimeout reaps idle keep-alive connections;
// per-request cancellation is handled via context.
func newMCPHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       2 * time.Minute,
	}
}

// Serve implements Transport.
func (t *SDKTransport) Serve(ctx context.Context, handler Handler) error {
	if err := t.validateServeInputs(handler); err != nil {
		return err
	}
	if t.originGuard == nil {
		// NewSDKTransport builds the guard. This keeps the configured policy in
		// force even if a transport is assembled another way, because a nil guard
		// would leave the SDK to install its own unconfigured one (issue #652).
		t.originGuard = newSDKOriginGuard(t.server.cfg.AllowedOrigins)
	}

	sdkServer := t.server.buildSDKServer()
	sdkHandler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server {
		return sdkServer
	}, &sdkmcp.StreamableHTTPOptions{
		JSONResponse: true,
		// Without this the SDK builds a default, unconfigured cross-origin guard
		// that refuses every configured cross-origin request (issue #652). The
		// guard stays on; it is fed the allowed_origins policy.
		CrossOriginProtection: t.originGuard.protection(),
	})
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go t.server.runSessionCleanup(runCtx)
	if t.server.rateLimiter != nil {
		go t.server.runRateLimitCleanup(runCtx)
	}
	if t.server.x402Enabled {
		go t.server.runPaymentOutcomeCleanup(runCtx)
	}
	defer func() {
		if err := t.server.Close(); err != nil {
			// Match the legacy shutdown path: close errors are best-effort and
			// should not mask the transport shutdown result.
			log.Printf("error closing payment log: %v", err)
		}
	}()

	// corsMiddleware adds the CORS response headers a browser needs to read the
	// body it just received. Server.Handler() carries it for every other path and
	// for OPTIONS; the MCP-path POST and DELETE responses come from this
	// transport, so they need it here too (issue #652).
	mcpPathHandler := t.server.corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		t.serveHTTPRequest(w, req, sdkHandler)
	}))
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodOptions || req.URL.Path != t.server.cfg.MCPPath {
			handler.ServeHTTP(w, req)
			return
		}
		mcpPathHandler.ServeHTTP(w, req)
	})

	server := newMCPHTTPServer(wrapped)

	errCh := make(chan error, 1)
	go func() {
		var err error
		if t.certFile != "" && t.keyFile != "" {
			err = server.ServeTLS(t.listener, t.certFile, t.keyFile)
		} else {
			err = server.Serve(t.listener)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		// Shutdown stops accepting new connections first, then waits for the
		// active handlers. Serve returns only after that wait, so the caller
		// can treat "Serve returned" as "no MCP handler still uses the index
		// or the store" (issue #688).
		grace := t.shutdownGrace
		if grace <= 0 {
			grace = DefaultShutdownGrace
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func (t *SDKTransport) validateServeInputs(handler Handler) error {
	if t == nil {
		return errors.New("sdk transport: nil transport")
	}
	if t.server == nil {
		return errors.New("sdk transport: nil server")
	}
	if t.listener == nil {
		return errors.New("nil listener passed to SDKTransport.Serve")
	}
	if handler == nil {
		return errors.New("sdk transport: nil handler")
	}
	return nil
}

func (t *SDKTransport) serveHTTPRequest(w http.ResponseWriter, req *http.Request, sdkHandler http.Handler) {
	if !t.checkRateLimit(w, req) {
		return
	}
	switch req.Method {
	case http.MethodPost:
		t.servePost(w, req, sdkHandler)
	case http.MethodDelete:
		// DELETE is the spec's explicit session-termination verb. GET
		// (server->client SSE) is intentionally unsupported: this is a
		// request/response RAG server that pushes no unsolicited
		// notifications, so it 405s below with an accurate Allow header.
		t.serveSessionTermination(w, req, sdkHandler)
	default:
		w.Header().Set("Allow", "POST, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (t *SDKTransport) servePost(w http.ResponseWriter, req *http.Request, sdkHandler http.Handler) {
	if !t.checkPostPreRequest(w, req) {
		return
	}
	body, ok := readSDKBody(w, req)
	if !ok {
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	parsedReq, id, hasID, ok := t.parseSDKRequestAndID(w, body)
	if !ok {
		return
	}
	// Session validation is performed inside dispatchSDKRequest, immediately
	// before the handler is invoked, to close the TOCTOU window that would
	// exist if we checked here and then dispatched separately.
	t.dispatchSDKRequest(w, req, parsedReq, id, hasID, sdkHandler)
}

func (t *SDKTransport) checkRateLimit(w http.ResponseWriter, req *http.Request) bool {
	if t.server.rateLimiter != nil {
		if !t.server.rateLimiter.allow(realIP(req, t.server.rateLimiter)) {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, nil, -32000, "rate limit exceeded", protocol.ErrorCodeRateLimitExceeded, true)
			return false
		}
	}
	return true
}

func (t *SDKTransport) checkPostPreRequest(w http.ResponseWriter, req *http.Request) bool {
	if !hasJSONContentType(req.Header.Get("Content-Type")) {
		writeError(w, http.StatusUnsupportedMediaType, nil, -32600, "Content-Type must be application/json", "INVALID_FIELD", false)
		return false
	}
	if ok, _ := t.server.authorize(w, req); !ok {
		return false
	}
	if !t.server.allowOrigin(w, req) {
		return false
	}
	if !t.applyOriginGuard(w, req) {
		return false
	}
	// Both header repairs run last, after every gate of this server has read the
	// request as the client sent it. They exist only to satisfy the SDK handler
	// this request is about to reach.
	canonicalizeContentType(req)
	negotiateAccept(req)
	return true
}

// applyOriginGuard reconciles the SDK's cross-origin guard with the allowlist
// decision that allowOrigin has just made, and owns the refusal contract.
//
// Call it only after allowOrigin returned true. adjudicate tells the SDK guard
// that the allowlist accepted this origin. The check that follows keeps the SDK
// guard's remaining authority, mainly a request that declares itself cross-site
// and names no origin, but answers it with this server's canonical
// FORBIDDEN_ORIGIN contract instead of the SDK's bare text body (issue #652).
func (t *SDKTransport) applyOriginGuard(w http.ResponseWriter, req *http.Request) bool {
	t.originGuard.adjudicate(req)
	if err := t.originGuard.check(req); err != nil {
		writeError(w, http.StatusForbidden, nil, -32000, err.Error(), "FORBIDDEN_ORIGIN", false)
		return false
	}
	return true
}

// canonicalizeContentType rewrites the Content-Type header of an accepted POST
// to the bare media type, dropping any parameters.
//
// Call it only after hasJSONContentType accepted the header. The MCP Go SDK
// (v1.4.1 StreamableHTTPHandler.ServeHTTP) compares the header for exact
// equality with "application/json", so it answers a plain-text 415 to every
// spelling that carries a parameter, including the common
// "application/json; charset=utf-8" (issue #841). The SDK's only switch for that
// check also switches off its cross-origin guard, which must stay on (issue
// #652), so this server keeps the media-type decision and hands the SDK the one
// spelling it accepts. The parameters are dropped, not translated: RFC 8259 §11
// defines none for application/json, so nothing downstream reads them.
//
// This mirrors negotiateAccept below, which repairs the Accept header of the
// same request for the same handler.
func canonicalizeContentType(req *http.Request) {
	req.Header.Set("Content-Type", jsonMediaType)
}

// negotiateAccept guarantees the POST Accept header advertises BOTH
// application/json and text/event-stream, which the streamable-HTTP SDK
// requires (it 400s a POST that lists only one). A spec-conformant client that
// sends exactly "Accept: application/json" — reasonable, since the server runs
// JSONResponse — would otherwise be rejected, so a partial header is augmented
// rather than only injected when absent (issue #404). Detection mirrors the
// SDK's exact whole-token matching (not substring), so an unmatched token like
// "application/json;q=0.9" is treated as missing and the canonical token is
// appended, which the SDK then matches.
func negotiateAccept(req *http.Request) {
	const jsonType = "application/json"
	const sseType = "text/event-stream"

	values := req.Header.Values("Accept")
	jsonOK, streamOK := acceptSatisfies(values)
	if jsonOK && streamOK {
		return
	}
	parts := make([]string, 0, 3)
	if existing := strings.TrimSpace(strings.Join(values, ", ")); existing != "" {
		parts = append(parts, existing)
	}
	if !jsonOK {
		parts = append(parts, jsonType)
	}
	if !streamOK {
		parts = append(parts, sseType)
	}
	req.Header.Set("Accept", strings.Join(parts, ", "))
}

// acceptSatisfies reports whether the Accept header values already advertise
// application/json and text/event-stream using the same whole-token matching
// the SDK applies, so negotiateAccept only appends what is genuinely missing.
func acceptSatisfies(values []string) (jsonOK, streamOK bool) {
	for _, tok := range strings.Split(strings.Join(values, ","), ",") {
		switch strings.TrimSpace(tok) {
		case "application/json", "application/*":
			jsonOK = true
		case "text/event-stream", "text/*":
			streamOK = true
		case "*/*":
			jsonOK = true
			streamOK = true
		}
	}
	return jsonOK, streamOK
}

// serveSessionTermination handles the spec's DELETE session-termination verb.
// It validates the session against our own store first (so an unknown/expired
// session gets our canonical SESSION_NOT_FOUND contract), forwards to the SDK
// to tear down its per-session state, then forgets our copy so the id cannot be
// replayed. Both stores key off the same id minted during initialize.
func (t *SDKTransport) serveSessionTermination(w http.ResponseWriter, req *http.Request, sdkHandler http.Handler) {
	if ok, _ := t.server.authorize(w, req); !ok {
		return
	}
	if !t.server.allowOrigin(w, req) {
		return
	}
	// DELETE is not a safe method, so the SDK's guard checks it too (issue #652).
	if !t.applyOriginGuard(w, req) {
		return
	}
	// DELETE is a post-initialize request, so the bs-004 header rule applies to
	// it as well. Session termination is itself lifecycle cleanup, so it is NOT
	// gated on handshake completion: a client may abandon a half-open session.
	if !t.server.enforceProtocolVersion(w, req, nil) {
		return
	}
	sessionID := strings.TrimSpace(req.Header.Get(protocol.MCPSessionHeader))
	if sessionID == "" {
		writeError(w, http.StatusNotFound, nil, -32001, "session not found", protocol.ErrorCodeSessionNotFound, false)
		return
	}
	if ok, reason := t.server.hasActiveSession(sessionID, time.Now()); !ok {
		if reason != "" {
			w.Header().Set(protocol.MCPSessionExpiredHeader, reason)
		}
		writeError(w, http.StatusNotFound, nil, -32001, "session not found", protocol.ErrorCodeSessionNotFound, false)
		return
	}
	// Buffer the SDK's DELETE response so we can inspect its status before
	// deciding whether to forget our own record. Only a confirmed 204 (the
	// spec's success status for session termination) means the SDK actually
	// tore down its per-session state; on any non-success response
	// (error/timeout/id mismatch) we leave our record intact so the client can
	// retry, rather than bricking a still-live session and blocking retries.
	rec := newBufferedResponseWriter()
	sdkHandler.ServeHTTP(rec, req)
	if rec.status == http.StatusNoContent {
		t.server.forgetSession(sessionID)
	}
	copyBufferedResponse(w, rec)
}

func readSDKBody(w http.ResponseWriter, req *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(req.Body, maxRequestBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, nil, -32600, "failed to read request body", "INVALID_FIELD", false)
		return nil, false
	}
	if len(body) > maxRequestBody {
		writeError(w, http.StatusBadRequest, nil, -32600, "request body too large", "INVALID_FIELD", false)
		return nil, false
	}
	return body, true
}

func (t *SDKTransport) parseSDKRequestAndID(w http.ResponseWriter, body []byte) (rpcRequest, interface{}, bool, bool) {
	parsedReq, parseErr := parseRequest(io.NopCloser(bytes.NewReader(body)))
	if parseErr != nil {
		writeError(w, http.StatusBadRequest, nil, -32600, parseErr.Error(), parseCanonicalCode(parseErr), false)
		return rpcRequest{}, nil, false, false
	}
	id, hasID, idErr := parseID(parsedReq.ID)
	if idErr != nil {
		writeError(w, http.StatusBadRequest, nil, -32600, idErr.Error(), parseCanonicalCode(idErr), false)
		return rpcRequest{}, nil, false, false
	}
	if parsedReq.Method == "" {
		writeError(w, http.StatusBadRequest, id, -32600, "method is required", "MISSING_FIELD", false)
		return rpcRequest{}, nil, false, false
	}
	return parsedReq, id, hasID, true
}

func (t *SDKTransport) dispatchSDKRequest(w http.ResponseWriter, req *http.Request, parsedReq rpcRequest, id interface{}, hasID bool, sdkHandler http.Handler) {
	// gatePostInitialize validates the version header, the session and the
	// bs-005 handshake state immediately before dispatch, which keeps the
	// session TOCTOU window closed. It is the same helper the direct handler
	// chain runs, so the two chains cannot drift.
	if !t.server.gatePostInitialize(w, req, parsedReq.Method, id) {
		return
	}
	switch parsedReq.Method {
	case protocol.RPCMethodInitialize:
		t.handleSDKInitialize(w, req, id, hasID, sdkHandler)
	case protocol.RPCMethodNotificationsInitialized:
		t.handleSDKNotificationsInitialized(w, req, id, hasID)
	case protocol.RPCMethodNotificationsCancelled:
		t.handleSDKNotificationsCancelled(w, req, parsedReq.Params, id, hasID, sdkHandler)
	case protocol.RPCMethodToolsList:
		t.handleSDKToolsList(w, req, hasID, sdkHandler)
	case protocol.RPCMethodToolsCall:
		t.handleSDKToolsCall(w, req, parsedReq.Params, id, hasID, sdkHandler)
	default:
		t.handleSDKUnknownMethod(w, id, hasID)
	}
}

func (t *SDKTransport) handleSDKInitialize(w http.ResponseWriter, req *http.Request, id interface{}, hasID bool, sdkHandler http.Handler) {
	if !hasID {
		writeError(w, http.StatusBadRequest, nil, -32600, "initialize requires id", "MISSING_FIELD", false)
		return
	}
	rec := newBufferedResponseWriter()
	sdkHandler.ServeHTTP(rec, req)
	if sessionID := strings.TrimSpace(rec.header.Get(protocol.MCPSessionHeader)); sessionID != "" {
		// Extract authScope from the authorize call result
		_, authScope := t.server.authorize(nil, req)
		t.server.storeSession(sessionID, authScope)
	}
	copyBufferedResponse(w, rec)
}

func (t *SDKTransport) handleSDKNotificationsInitialized(w http.ResponseWriter, req *http.Request, id interface{}, hasID bool) {
	// Mark the handshake complete on both the well-formed notification and the
	// malformed request-shaped variant: the client's intent is the same, and
	// the response contract for each shape is unchanged. gatePostInitialize has
	// already confirmed the session exists.
	t.server.markSessionInitialized(strings.TrimSpace(req.Header.Get(protocol.MCPSessionHeader)))
	if !hasID {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeResult(w, http.StatusOK, id, map[string]interface{}{})
}

// handleSDKNotificationsCancelled forwards a cancellation notification to the
// SDK so it can cancel the context of the request it targets (matched by
// requestId within the session). Previously this fell through to
// handleSDKUnknownMethod and was 202'd without ever reaching the SDK, so a
// client cancelling a long ask/transcribe could not stop the server from
// continuing to spend provider quota (issue #404). Cancellation only reaches an
// in-flight tool call on the SDK-dispatched path (the default, non-x402 path).
//
// The x402 path routes tools/call outside the SDK, so the SDK cannot cancel it
// either; that call registers itself in server.paidInFlight and is cancelled
// here by (session, requestId) before the notification is forwarded (issue
// #657). The forward still happens in both cases, because the SDK owns
// cancellation for every non-gated request and must see the notification.
func (t *SDKTransport) handleSDKNotificationsCancelled(w http.ResponseWriter, req *http.Request, rawParams json.RawMessage, id interface{}, hasID bool, sdkHandler http.Handler) {
	if hasID {
		// notifications/cancelled is a notification; an id makes it malformed.
		// Preserve the JSON-RPC error contract rather than forwarding a
		// would-be request the SDK would treat differently.
		writeError(w, http.StatusOK, id, -32600, "notifications/cancelled must not carry an id", "INVALID_FIELD", false)
		return
	}
	// Session-scoped: a JSON-RPC id is unique only within a session, so an
	// unscoped lookup would let one client cancel another's paid work.
	t.server.cancelPaidToolCall(req, rawParams)
	sdkHandler.ServeHTTP(w, req)
}

func (t *SDKTransport) handleSDKToolsList(w http.ResponseWriter, req *http.Request, hasID bool, sdkHandler http.Handler) {
	if !hasID {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	sdkHandler.ServeHTTP(w, req)
}

func (t *SDKTransport) handleSDKToolsCall(w http.ResponseWriter, req *http.Request, rawParams json.RawMessage, id interface{}, hasID bool, sdkHandler http.Handler) {
	if !hasID {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if t.server.x402Enabled {
		// The gated path never enters the SDK, so the SDK cannot cancel it.
		// Register the call under (session, requestId) for exactly the window
		// in which cancelling is safe; handleToolsCallRequest releases the
		// entry before settlement begins (issue #657, paid_inflight.go).
		ctx, release, cancel := t.server.beginCancellableToolCall(req, id)
		defer cancel()
		defer release()
		t.server.handleToolsCallRequest(ctx, w, req, rawParams, id, release)
		return
	}
	sdkHandler.ServeHTTP(w, req)
}

func (t *SDKTransport) handleSDKUnknownMethod(w http.ResponseWriter, id interface{}, hasID bool) {
	if !hasID {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeError(w, http.StatusOK, id, -32601, "method not found", "METHOD_NOT_FOUND", false)
}

type bufferedResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newBufferedResponseWriter() *bufferedResponseWriter {
	return &bufferedResponseWriter{
		header: make(http.Header),
	}
}

func (w *bufferedResponseWriter) Header() http.Header {
	return w.header
}

func (w *bufferedResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *bufferedResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

func copyBufferedResponse(dst http.ResponseWriter, src *bufferedResponseWriter) {
	for name, values := range src.header {
		dst.Header()[name] = append([]string(nil), values...)
	}
	if src.status != 0 {
		dst.WriteHeader(src.status)
	}
	_, _ = dst.Write(src.body.Bytes())
}

func (s *Server) buildSDKServer() *sdkmcp.Server {
	sdkServer := sdkmcp.NewServer(&sdkmcp.Implementation{
		Name:    s.cfg.ServerName,
		Title:   "dir2mcp: Directory RAG MCP Server",
		Version: buildinfo.String(),
	}, &sdkmcp.ServerOptions{
		Instructions: "Use tools/list then tools/call. Results include citations.",
	})

	sdkServer.AddReceivingMiddleware(func(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
		return func(ctx context.Context, method string, req sdkmcp.Request) (sdkmcp.Result, error) {
			result, err := next(ctx, method, req)
			if err != nil {
				return result, err
			}
			if method != protocol.RPCMethodInitialize {
				return result, nil
			}
			initResult, ok := result.(*sdkmcp.InitializeResult)
			if !ok || initResult == nil || initResult.Capabilities == nil {
				return result, nil
			}
			// Pin the negotiated protocolVersion to the version this server
			// supports (SPEC §11.2) instead of echoing the client's requested
			// value. This also makes the configured protocol_version knob
			// (§5.5) take wire effect; it defaults to the spec version.
			pinned := strings.TrimSpace(s.cfg.ProtocolVersion)
			if pinned == "" {
				pinned = protocol.ProtocolDefaultVersion
			}
			initResult.ProtocolVersion = pinned
			initResult.Capabilities.Logging = nil
			if initResult.Capabilities.Tools == nil {
				initResult.Capabilities.Tools = &sdkmcp.ToolCapabilities{}
			}
			initResult.Capabilities.Tools.ListChanged = false
			return initResult, nil
		}
	})

	for _, name := range toolOrder {
		tool, ok := s.tools[name]
		if !ok {
			continue
		}
		td := tool
		sdkServer.AddTool(&sdkmcp.Tool{
			Name:         td.Name,
			Description:  td.Description,
			InputSchema:  td.InputSchema,
			OutputSchema: td.OutputSchema,
		}, func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			args := make(map[string]interface{})
			if len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
					return nil, fmt.Errorf("invalid tool arguments: %w", err)
				}
			}
			res, toolErr := s.invokeToolHandler(ctx, td, td.Name, args)
			if toolErr != nil {
				res = newToolErrorResult(*toolErr)
			}
			return convertToolCallResult(res), nil
		})
	}

	return sdkServer
}

func convertToolCallResult(res toolCallResult) *sdkmcp.CallToolResult {
	out := &sdkmcp.CallToolResult{
		StructuredContent: res.StructuredContent,
		IsError:           res.IsError,
	}
	if len(res.Content) == 0 {
		out.Content = []sdkmcp.Content{}
		return out
	}

	out.Content = make([]sdkmcp.Content, 0, len(res.Content))
	for _, item := range res.Content {
		switch item.Type {
		case "text":
			out.Content = append(out.Content, &sdkmcp.TextContent{Text: item.Text})
		case "audio":
			data, err := base64.StdEncoding.DecodeString(item.Data)
			if err != nil {
				log.Printf("warning: dropping invalid base64 audio content (mime=%q): %v", item.MIMEType, err)
				continue
			}
			out.Content = append(out.Content, &sdkmcp.AudioContent{
				MIMEType: item.MIMEType,
				Data:     data,
			})
		case "video":
			// MCP 2025-11-25 defines text, image, audio, resource_link and
			// resource. It defines NO video item, so SPEC §15.11 names an
			// embedded resource carrying the blob and a video/* mimeType as the
			// only valid carrier for inline video bytes (spec 0.49.0).
			//
			// This arm used to fall through to `default`, which produced a
			// TextContent from an item that has no Text. The call then reported
			// success and the client rendered nothing: no player, no text, no
			// error (#663). Audio worked, so the failure looked arbitrary.
			data, err := base64.StdEncoding.DecodeString(item.Data)
			if err != nil {
				log.Printf("warning: dropping invalid base64 video content (mime=%q): %v", item.MIMEType, err)
				continue
			}
			out.Content = append(out.Content, &sdkmcp.EmbeddedResource{
				Resource: &sdkmcp.ResourceContents{
					URI:      item.URI,
					MIMEType: item.MIMEType,
					Blob:     data,
				},
			})
		default:
			// An item that carries BYTES and no text must never become a text
			// item. That is the #663 failure mode, and §15.11 forbids it: a
			// blank text item drops the payload while reporting success. An
			// unmapped media type is a bug in this switch, so it is reported
			// rather than rendered as an empty item.
			if item.Data != "" && item.Text == "" {
				log.Printf("warning: no MCP content mapping for item type %q (mime=%q); dropping it rather than sending an empty text item",
					item.Type, item.MIMEType)
				continue
			}
			out.Content = append(out.Content, &sdkmcp.TextContent{Text: item.Text})
		}
	}
	return out
}
