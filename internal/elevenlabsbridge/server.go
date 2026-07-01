package elevenlabsbridge

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/protocol"
)

const inboundSecretHeader = "X-Bridge-Secret"

const defaultAskK = 3
const maxBridgeRequestBodyBytes int64 = 1 << 20

type bridge struct {
	cfg         Config
	client      *Client
	token       string
	tokenSource string
	tokenPath   string
	mux         *http.ServeMux
}

// New constructs a bridge with resolved token configuration and HTTP routes.
func New(cfg Config) (*bridge, error) {
	token, source, tokenPath, err := ResolveToken(cfg)
	if err != nil {
		return nil, err
	}
	b := &bridge{
		cfg:         cfg,
		client:      NewClient(cfg.MCPURL, token),
		token:       token,
		tokenSource: source,
		tokenPath:   tokenPath,
		mux:         http.NewServeMux(),
	}
	b.routes()
	return b, nil
}

// Handler returns the HTTP handler tree for the bridge.
func (b *bridge) Handler() http.Handler {
	return b.mux
}

// TokenSource reports whether the bridge token came from MCP_TOKEN, the state
// directory token file, or was omitted entirely.
func (b *bridge) TokenSource() string {
	if b == nil {
		return ""
	}
	return b.tokenSource
}

// TokenPath returns the absolute token file path when token file discovery was
// successful.
func (b *bridge) TokenPath() string {
	if b == nil {
		return ""
	}
	return b.tokenPath
}

// MCPURL returns the configured backend MCP endpoint.
func (b *bridge) MCPURL() string {
	if b == nil {
		return ""
	}
	return b.cfg.MCPURL
}

// InboundAuthEnabled reports whether protected routes require a shared secret.
func (b *bridge) InboundAuthEnabled() bool {
	if b == nil {
		return false
	}
	return b.cfg.InboundSecretConfigured()
}

func (b *bridge) routes() {
	// /health is intentionally unauthenticated: it returns no corpus data.
	b.mux.HandleFunc("/health", b.handleHealth)
	b.mux.HandleFunc("/ask", b.protected(http.MethodPost, b.handleAsk))
	b.mux.HandleFunc("/search", b.protected(http.MethodPost, b.handleSearch))
	b.mux.HandleFunc("/list_files", b.protected(http.MethodGet, b.handleListFiles, http.MethodPost))
	b.mux.HandleFunc("/stats", b.protected(http.MethodGet, b.handleStats, http.MethodPost))
}

// protected wraps a handler with the inbound-auth check followed by the HTTP
// method guard, so unauthenticated callers are rejected before any work runs.
func (b *bridge) protected(allowed string, fn http.HandlerFunc, extra ...string) http.HandlerFunc {
	return b.requireInboundAuth(b.methodGuard(allowed, fn, extra...))
}

// requireInboundAuth rejects requests that do not present the configured shared
// secret. When no secret is configured the handler passes through; in that case
// the non-loopback guard (ValidateListenSecurity) prevents the bridge from
// starting on a publicly reachable address.
func (b *bridge) requireInboundAuth(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !b.cfg.InboundSecretConfigured() {
			fn(w, r)
			return
		}
		if !b.inboundSecretMatches(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="elevenlabs-bridge"`)
			writeJSONError(w, http.StatusUnauthorized, "missing or invalid inbound credentials")
			return
		}
		fn(w, r)
	}
}

// inboundSecretMatches performs a constant-time comparison of the presented
// credential against the configured secret. It accepts either an
// "Authorization: Bearer <secret>" header or an "X-Bridge-Secret: <secret>"
// header.
func (b *bridge) inboundSecretMatches(r *http.Request) bool {
	secret := strings.TrimSpace(b.cfg.InboundSecret)
	if secret == "" {
		return false
	}
	presented := strings.TrimSpace(r.Header.Get(inboundSecretHeader))
	if presented == "" {
		if auth := strings.TrimSpace(r.Header.Get("Authorization")); auth != "" {
			if rest, ok := cutBearerPrefix(auth); ok {
				presented = strings.TrimSpace(rest)
			}
		}
	}
	if presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(secret)) == 1
}

// cutBearerPrefix strips a case-insensitive "Bearer " prefix from an
// Authorization header value.
func cutBearerPrefix(value string) (string, bool) {
	const prefix = "bearer "
	if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return "", false
	}
	return value[len(prefix):], true
}

func (b *bridge) methodGuard(allowed string, fn http.HandlerFunc, extra ...string) http.HandlerFunc {
	allowedMethods := map[string]struct{}{allowed: {}}
	for _, method := range extra {
		allowedMethods[method] = struct{}{}
	}
	allowHeader := buildAllowHeader(allowedMethods)
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := allowedMethods[r.Method]; !ok {
			w.Header().Set("Allow", allowHeader)
			writeJSONError(w, http.StatusMethodNotAllowed, fmt.Sprintf("%s not allowed", r.Method))
			return
		}
		fn(w, r)
	}
}

func (b *bridge) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (b *bridge) handleAsk(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBridgeRequestBodyBytes)
	var req struct {
		Question string `json:"question"`
		Query    string `json:"query"`
		K        int    `json:"k"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	question := strings.TrimSpace(req.Question)
	if question == "" {
		question = strings.TrimSpace(req.Query)
	}
	if question == "" {
		writeJSONError(w, http.StatusBadRequest, "question is required")
		return
	}
	k := req.K
	if k <= 0 {
		k = defaultAskK
	}

	result, err := b.client.CallTool(r.Context(), protocol.ToolNameAsk, map[string]interface{}{
		"question": question,
		"k":        k,
	})
	if err != nil {
		b.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"result":     result.Text(),
		"structured": result.StructuredContent,
	})
}

func (b *bridge) handleSearch(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBridgeRequestBodyBytes)
	var req struct {
		Query    string `json:"query"`
		Question string `json:"question"`
		K        int    `json:"k"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		query = strings.TrimSpace(req.Question)
	}
	if query == "" {
		writeJSONError(w, http.StatusBadRequest, "query is required")
		return
	}
	k := req.K
	if k <= 0 {
		k = defaultAskK
	}

	result, err := b.client.CallTool(r.Context(), protocol.ToolNameSearch, map[string]interface{}{
		"query": query,
		"k":     k,
	})
	if err != nil {
		b.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"result":     result.Text(),
		"structured": result.StructuredContent,
	})
}

func (b *bridge) handleListFiles(w http.ResponseWriter, r *http.Request) {
	result, err := b.client.CallTool(r.Context(), protocol.ToolNameListFiles, map[string]interface{}{})
	if err != nil {
		b.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"result":     result.Text(),
		"structured": result.StructuredContent,
	})
}

func (b *bridge) handleStats(w http.ResponseWriter, r *http.Request) {
	result, err := b.client.CallTool(r.Context(), protocol.ToolNameStats, map[string]interface{}{})
	if err != nil {
		b.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"result":     result.Text(),
		"structured": result.StructuredContent,
	})
}

func (b *bridge) writeError(w http.ResponseWriter, err error) {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		writeRawHTTPError(w, httpErr)
		return
	}
	writeJSONError(w, http.StatusBadGateway, err.Error())
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]string{
			"message": message,
		},
	})
}

func writeRawHTTPError(w http.ResponseWriter, err *HTTPError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(err.StatusCode)
	if len(err.Body) == 0 {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"message": fmt.Sprintf("upstream MCP returned HTTP %d", err.StatusCode),
			},
		})
		return
	}
	_, _ = w.Write(err.Body)
}

// Run starts the bridge HTTP server and blocks until the context is canceled
// or the server fails to start.
func Run(ctx context.Context, cfg Config, listenAddr string) error {
	b, err := New(cfg)
	if err != nil {
		return err
	}
	return RunWithBridge(ctx, b, listenAddr)
}

// RunWithBridge starts the bridge HTTP server with a pre-constructed bridge and
// blocks until the context is canceled or the server fails to start.
func RunWithBridge(ctx context.Context, b *bridge, listenAddr string) error {
	if err := ValidateListenSecurity(listenAddr, b.cfg.InboundSecretConfigured(), b.cfg.ForceInsecure); err != nil {
		return err
	}
	server := &http.Server{
		Addr:              strings.TrimSpace(listenAddr),
		Handler:           b.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return ctx.Err()
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func buildAllowHeader(allowedMethods map[string]struct{}) string {
	methods := make([]string, 0, len(allowedMethods))
	for method := range allowedMethods {
		methods = append(methods, method)
	}
	sort.Strings(methods)
	return strings.Join(methods, ", ")
}
