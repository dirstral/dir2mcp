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
	server   *Server
	listener net.Listener
	certFile string
	keyFile  string
}

// NewSDKTransport constructs an SDKTransport. certFile and keyFile are
// optional; both must be non-empty to enable TLS.
func NewSDKTransport(server *Server, listener net.Listener, certFile, keyFile string) *SDKTransport {
	return &SDKTransport{
		server:   server,
		listener: listener,
		certFile: certFile,
		keyFile:  keyFile,
	}
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

	sdkServer := t.server.buildSDKServer()
	sdkHandler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server {
		return sdkServer
	}, &sdkmcp.StreamableHTTPOptions{
		JSONResponse: true,
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

	wrapped := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodOptions || req.URL.Path != t.server.cfg.MCPPath {
			handler.ServeHTTP(w, req)
			return
		}
		t.serveHTTPRequest(w, req, sdkHandler)
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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	if !t.checkPreRequest(w, req) {
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

func (t *SDKTransport) checkPreRequest(w http.ResponseWriter, req *http.Request) bool {
	if t.server.rateLimiter != nil {
		if !t.server.rateLimiter.allow(realIP(req, t.server.rateLimiter)) {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, nil, -32000, "rate limit exceeded", protocol.ErrorCodeRateLimitExceeded, true)
			return false
		}
	}
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return false
	}
	ct := req.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, nil, -32600, "Content-Type must be application/json", "INVALID_FIELD", false)
		return false
	}
	if ok, _ := t.server.authorize(w, req); !ok {
		return false
	}
	if !t.server.allowOrigin(w, req) {
		return false
	}
	if strings.TrimSpace(req.Header.Get("Accept")) == "" {
		req.Header.Set("Accept", "application/json, text/event-stream")
	}
	return true
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
	if !t.validateSDKSession(w, req, parsedReq.Method, id) {
		return
	}
	switch parsedReq.Method {
	case protocol.RPCMethodInitialize:
		t.handleSDKInitialize(w, req, id, hasID, sdkHandler)
	case protocol.RPCMethodNotificationsInitialized:
		t.handleSDKNotificationsInitialized(w, id, hasID)
	case protocol.RPCMethodToolsList:
		t.handleSDKToolsList(w, req, hasID, sdkHandler)
	case protocol.RPCMethodToolsCall:
		t.handleSDKToolsCall(w, req, parsedReq.Params, id, hasID, sdkHandler)
	default:
		t.handleSDKUnknownMethod(w, id, hasID)
	}
}

func (t *SDKTransport) validateSDKSession(w http.ResponseWriter, req *http.Request, method string, id interface{}) bool {
	// Validate session immediately before dispatch to eliminate the TOCTOU
	// window between a prior check and handler invocation. initialize does not
	// require a pre-existing session — it creates one.
	if method == protocol.RPCMethodInitialize {
		return true
	}
	sessionID := strings.TrimSpace(req.Header.Get(protocol.MCPSessionHeader))
	if sessionID == "" {
		writeError(w, http.StatusNotFound, id, -32001, "session not found", protocol.ErrorCodeSessionNotFound, false)
		return false
	}
	if ok, reason := t.server.hasActiveSession(sessionID, time.Now()); !ok {
		if reason != "" {
			w.Header().Set(protocol.MCPSessionExpiredHeader, reason)
		}
		writeError(w, http.StatusNotFound, id, -32001, "session not found", protocol.ErrorCodeSessionNotFound, false)
		return false
	}
	return true
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

func (t *SDKTransport) handleSDKNotificationsInitialized(w http.ResponseWriter, id interface{}, hasID bool) {
	if !hasID {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeResult(w, http.StatusOK, id, map[string]interface{}{})
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
		t.server.handleToolsCallRequest(req.Context(), w, req, rawParams, id)
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
			res, toolErr := td.handler(ctx, args)
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
		default:
			out.Content = append(out.Content, &sdkmcp.TextContent{Text: item.Text})
		}
	}
	return out
}
