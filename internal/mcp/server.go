package mcp

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"dir2mcp/internal/appstate"
	"dir2mcp/internal/buildinfo"
	"dir2mcp/internal/config"
	"dir2mcp/internal/model"
	"dir2mcp/internal/protocol"
	"dir2mcp/internal/x402"
)

const (
	authTokenEnvVar        = "DIR2MCP_AUTH_TOKEN"
	maxRequestBody         = 1 << 20
	sessionTTL             = 24 * time.Hour
	sessionCleanupInterval = time.Hour
	rateLimitCleanupEvery  = 5 * time.Minute
	rateLimitBucketMaxAge  = 10 * time.Minute
	// Replay outcomes are only useful while signatures are still valid.
	// Keep a small buffer over common short-lived signature windows.
	paymentOutcomeTTL             = 10 * time.Minute
	paymentOutcomeCleanupInterval = time.Minute
	paymentOutcomeMaxEntries      = 5000
)

// DefaultSearchK is used when tools/call search arguments omit k or provide
// a non-positive value.
const DefaultSearchK = 10

// MaxSearchK is the highest allowed k value for search/ask requests.
const MaxSearchK = 50

// sessionInfo holds metadata tracked for each active session.  `created` is the
// time the session was started; `lastSeen` is updated on each successful
// request.  The server uses both values to enforce inactivity timeouts and
// optional absolute lifetimes.
type sessionInfo struct {
	created  time.Time
	lastSeen time.Time
}

type Server struct {
	cfg       config.Config
	authToken string
	retriever model.Retriever
	store     model.Store
	indexing  *appstate.IndexingState
	tts       TTSSynthesizer
	tools     map[string]toolDefinition

	sessionMu sync.RWMutex
	// sessions maps session IDs to metadata.  lastSeen is updated on each
	// successful request; created represents the time the session was
	// initialized.  We use both values to enforce inactivity timeouts and
	// optional absolute lifetimes.
	sessions map[string]sessionInfo

	rateLimiter *ipRateLimiter

	x402Client      *x402.HTTPClient
	x402Requirement x402.Requirement
	x402Enabled     bool
	paymentLogPath  string
	paymentMu       sync.RWMutex
	paymentOutcomes map[string]paymentExecutionOutcome
	paymentTTL      time.Duration
	paymentMaxItems int

	// cached writer used by appendPaymentLog. protected by paymentLogMu.
	paymentLogMu     sync.Mutex
	paymentLogFile   *os.File
	paymentLogWriter *bufio.Writer

	// per-execution-key locks used to serialize payment handling for identical
	// signatures+params.  Map is protected by execMu.  execCond is a condition
	// variable that goroutines can wait on when they need to observe changes to
	// the ref counts stored in execKeyMu.  It is always created with execMu as
	// its Locker (see NewServer) so callers must hold execMu before calling
	// Wait/Signal/Broadcast.
	execMu    sync.Mutex
	execCond  *sync.Cond
	execKeyMu map[string]*keyMutex

	eventEmitter func(level, event string, data interface{})
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
}

type rpcError struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Data    *rpcErrorData `json:"data,omitempty"`
}

// rpcErrorData is attached to an rpcError when additional metadata is
// required by the JSON-RPC response.  All fields are exported so that the
// structure can be marshaled to JSON; presently the type contains only
// primitive values.  The copy helper in mcp/payment.go relies on this
// property to produce an independent duplicate.  If new fields are added that
// are themselves reference types (slices, maps, pointers, etc.) the cloning
// logic must be kept in sync (the deep-copy performed via JSON round-trip
// already handles such extensions, so tests should guard against regressions).
//
// Keeping rpcErrorData simple helps avoid accidental sharing of mutable
// state between the original and any clones.
//
// NOTE: because we use encoding/json for the deep copy, the layout of this
// struct must remain JSON-encodable.  Adding unexported fields or custom
// types will require updating cloneRPCError accordingly.
type rpcErrorData struct {
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
}

type validationError struct {
	message       string
	canonicalCode string
}

func (e validationError) Error() string {
	return e.message
}

type ServerOption func(*Server)

type TTSSynthesizer interface {
	Synthesize(ctx context.Context, text string) ([]byte, error)
}

func WithStore(store model.Store) ServerOption {
	return func(s *Server) {
		s.store = store
	}
}

func WithIndexingState(state *appstate.IndexingState) ServerOption {
	return func(s *Server) {
		s.indexing = state
	}
}

func WithTTS(tts TTSSynthesizer) ServerOption {
	return func(s *Server) {
		s.tts = tts
	}
}

func WithEventEmitter(fn func(level, event string, data interface{})) ServerOption {
	return func(s *Server) {
		s.eventEmitter = fn
	}
}

func NewServer(cfg config.Config, retriever model.Retriever, opts ...ServerOption) *Server {
	s := &Server{
		cfg:             cfg,
		authToken:       loadAuthToken(cfg),
		retriever:       retriever,
		sessions:        make(map[string]sessionInfo),
		paymentOutcomes: make(map[string]paymentExecutionOutcome),
		paymentTTL:      paymentOutcomeTTL,
		paymentMaxItems: paymentOutcomeMaxEntries,
		execKeyMu:       make(map[string]*keyMutex),
	}
	// cond must be set after the zero-value mutex has been created above
	s.execCond = sync.NewCond(&s.execMu)
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	if s.indexing == nil {
		s.indexing = appstate.NewIndexingState(appstate.ModeIncremental)
	}
	if cfg.Public && cfg.RateLimitRPS > 0 && cfg.RateLimitBurst > 0 {
		s.rateLimiter = newIPRateLimiter(float64(cfg.RateLimitRPS), cfg.RateLimitBurst, cfg.TrustedProxies)
	}
	s.initPaymentConfig()
	s.tools = s.buildToolRegistry()
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(s.cfg.MCPPath, s.handleMCP)
	return s.corsMiddleware(mux)
}

// corsMiddleware wraps the handler to support CORS preflight (OPTIONS) and
// response headers for the MCP endpoint. Required for browser-based MCP
// clients such as ElevenLabs Conversational AI.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin != "" && isOriginAllowed(origin, s.cfg.AllowedOrigins) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", fmt.Sprintf("Content-Type, Authorization, %s, %s, PAYMENT-SIGNATURE", protocol.MCPProtocolVersionHeader, protocol.MCPSessionHeader))
			w.Header().Set("Access-Control-Expose-Headers", protocol.MCPSessionHeader+", PAYMENT-REQUIRED, PAYMENT-RESPONSE, "+protocol.MCPSessionExpiredHeader)
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.Header().Set("Vary", "Origin")
		}

		accessControlRequestMethod := strings.TrimSpace(r.Header.Get("Access-Control-Request-Method"))
		accessControlRequestHeaders := strings.TrimSpace(r.Header.Get("Access-Control-Request-Headers"))
		isPreflight := r.Method == http.MethodOptions &&
			origin != "" &&
			(accessControlRequestMethod != "" || accessControlRequestHeaders != "")
		if isPreflight {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return err
	}

	certFile := strings.TrimSpace(s.cfg.ServerTLSCertFile)
	keyFile := strings.TrimSpace(s.cfg.ServerTLSKeyFile)
	if certFile == "" && keyFile == "" {
		return s.RunOnListener(ctx, ln)
	}
	if certFile == "" || keyFile == "" {
		_ = ln.Close()
		return errors.New("tls requires both server_tls_cert_file and server_tls_key_file")
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("load tls certificate/key: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
	}
	return s.RunOnListener(ctx, tls.NewListener(ln, tlsCfg))
}

func (s *Server) RunOnListener(ctx context.Context, ln net.Listener) error {
	return s.runOnListener(ctx, ln, "", "")
}

func (s *Server) RunOnListenerTLS(ctx context.Context, ln net.Listener, certFile, keyFile string) error {
	certFile = strings.TrimSpace(certFile)
	keyFile = strings.TrimSpace(keyFile)
	if certFile == "" || keyFile == "" {
		return errors.New("tls requires both certFile and keyFile")
	}
	return s.runOnListener(ctx, ln, certFile, keyFile)
}

func (s *Server) runOnListener(ctx context.Context, ln net.Listener, certFile, keyFile string) error {
	if ln == nil {
		return errors.New("nil listener passed to RunOnListener")
	}
	// make sure any cached payment-log resources are flushed when the server
	// stops; the deferred call is harmless if nothing was opened.  Log any
	// error instead of silently discarding it.
	defer func() {
		if err := s.Close(); err != nil {
			log.Printf("error closing payment log: %v", err)
		}
	}()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go s.runSessionCleanup(runCtx)
	if s.rateLimiter != nil {
		go s.runRateLimitCleanup(runCtx)
	}
	if s.x402Enabled {
		go s.runPaymentOutcomeCleanup(runCtx)
	}

	server := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() {
		var err error
		if certFile != "" && keyFile != "" {
			err = server.ServeTLS(ln, certFile, keyFile)
		} else {
			err = server.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-runCtx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if s.rateLimiter != nil {
		if !s.rateLimiter.allow(realIP(r, s.rateLimiter)) {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, nil, -32000, "rate limit exceeded", protocol.ErrorCodeRateLimitExceeded, true)
			return
		}
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, nil, -32600, "Content-Type must be application/json", "INVALID_FIELD", false)
		return
	}

	if !s.authorize(w, r) {
		return
	}

	if !s.allowOrigin(w, r) {
		return
	}

	req, parseErr := parseRequest(r.Body)
	if parseErr != nil {
		canonicalCode := "INVALID_FIELD"
		var vErr validationError
		if errors.As(parseErr, &vErr) && vErr.canonicalCode != "" {
			canonicalCode = vErr.canonicalCode
		}
		writeError(w, http.StatusBadRequest, nil, -32600, parseErr.Error(), canonicalCode, false)
		return
	}

	id, hasID, idErr := parseID(req.ID)
	if idErr != nil {
		canonicalCode := "INVALID_FIELD"
		var vErr validationError
		if errors.As(idErr, &vErr) && vErr.canonicalCode != "" {
			canonicalCode = vErr.canonicalCode
		}
		writeError(w, http.StatusBadRequest, nil, -32600, idErr.Error(), canonicalCode, false)
		return
	}

	if req.Method == "" {
		writeError(w, http.StatusBadRequest, id, -32600, "method is required", "MISSING_FIELD", false)
		return
	}

	if req.Method != "initialize" {
		sessionID := strings.TrimSpace(r.Header.Get(protocol.MCPSessionHeader))
		if sessionID == "" {
			writeError(w, http.StatusNotFound, id, -32001, "session not found", protocol.ErrorCodeSessionNotFound, false)
			return
		}
		if ok, reason := s.hasActiveSession(sessionID, time.Now()); !ok {
			// optional diagnostic header
			if reason != "" {
				w.Header().Set(protocol.MCPSessionExpiredHeader, reason)
			}
			writeError(w, http.StatusNotFound, id, -32001, "session not found", protocol.ErrorCodeSessionNotFound, false)
			return
		}
	}

	switch req.Method {
	case "initialize":
		s.handleInitialize(w, id, hasID)
	case "notifications/initialized":
		if !hasID {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeResult(w, http.StatusOK, id, map[string]interface{}{})
	case "tools/list":
		if !hasID {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		s.handleToolsList(w, id)
	case "tools/call":
		if !hasID {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		s.handleToolsCallRequest(r.Context(), w, r, req.Params, id)
	default:
		if !hasID {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeError(w, http.StatusOK, id, -32601, "method not found", "METHOD_NOT_FOUND", false)
	}
}

func (s *Server) handleInitialize(w http.ResponseWriter, id interface{}, hasID bool) {
	if !hasID {
		writeError(w, http.StatusBadRequest, nil, -32600, "initialize requires id", "MISSING_FIELD", false)
		return
	}

	sessionID, err := generateSessionID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, id, -32603, "failed to initialize session", "", false)
		return
	}
	s.storeSession(sessionID)

	w.Header().Set(protocol.MCPSessionHeader, sessionID)
	writeResult(w, http.StatusOK, id, map[string]interface{}{
		"protocolVersion": s.cfg.ProtocolVersion,
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{
				"listChanged": false,
			},
		},
		"serverInfo": map[string]interface{}{
			"name":    "dir2mcp",
			"title":   "dir2mcp: Directory RAG MCP Server",
			"version": buildinfo.Version,
		},
		"instructions": "Use tools/list then tools/call. Results include citations.",
	})
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	if strings.EqualFold(s.cfg.AuthMode, "none") {
		return true
	}

	expectedToken := s.authToken
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	const bearerPrefix = "bearer "

	if len(authHeader) < len(bearerPrefix) || strings.ToLower(authHeader[:len(bearerPrefix)]) != bearerPrefix {
		writeError(w, http.StatusUnauthorized, nil, -32000, "missing or invalid bearer token", protocol.ErrorCodeUnauthorized, false)
		return false
	}

	providedToken := strings.TrimSpace(authHeader[len(bearerPrefix):])
	if expectedToken == "" || providedToken == "" {
		writeError(w, http.StatusUnauthorized, nil, -32000, "missing or invalid bearer token", protocol.ErrorCodeUnauthorized, false)
		return false
	}

	if subtle.ConstantTimeCompare([]byte(providedToken), []byte(expectedToken)) != 1 {
		writeError(w, http.StatusUnauthorized, nil, -32000, "missing or invalid bearer token", protocol.ErrorCodeUnauthorized, false)
		return false
	}

	return true
}

func (s *Server) allowOrigin(w http.ResponseWriter, r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}

	if isOriginAllowed(origin, s.cfg.AllowedOrigins) {
		return true
	}

	writeError(w, http.StatusForbidden, nil, -32000, "origin is not allowed", "FORBIDDEN_ORIGIN", false)
	return false
}

func parseRequest(body io.ReadCloser) (rpcRequest, error) {
	defer func() { _ = body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(body, maxRequestBody+1))
	if err != nil {
		return rpcRequest{}, err
	}
	if len(raw) > maxRequestBody {
		return rpcRequest{}, validationError{message: "request body too large", canonicalCode: "INVALID_FIELD"}
	}

	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return rpcRequest{}, validationError{message: "empty request body", canonicalCode: "MISSING_FIELD"}
	}
	if strings.HasPrefix(trimmed, "[") {
		return rpcRequest{}, validationError{message: "batch requests are not supported", canonicalCode: "INVALID_FIELD"}
	}

	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return rpcRequest{}, validationError{message: "invalid json body", canonicalCode: "INVALID_FIELD"}
	}
	if req.JSONRPC != "2.0" {
		return rpcRequest{}, validationError{message: "jsonrpc must be \"2.0\"", canonicalCode: "INVALID_FIELD"}
	}
	return req, nil
}

func parseID(raw json.RawMessage) (interface{}, bool, error) {
	if raw == nil {
		return nil, false, nil
	}

	var value interface{}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, true, validationError{message: "invalid id", canonicalCode: "INVALID_FIELD"}
	}

	switch value.(type) {
	case nil, string, float64:
		return value, true, nil
	default:
		return nil, true, validationError{message: "id must be string, number, or null", canonicalCode: "INVALID_FIELD"}
	}
}

func writeResult(w http.ResponseWriter, statusCode int, id interface{}, result interface{}) {
	writeResponse(w, statusCode, rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	})
}

func writeError(w http.ResponseWriter, statusCode int, id interface{}, code int, message, canonicalCode string, retryable bool) {
	var errData *rpcErrorData
	if canonicalCode != "" {
		errData = &rpcErrorData{
			Code:      canonicalCode,
			Retryable: retryable,
		}
	}

	writeResponse(w, statusCode, rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &rpcError{
			Code:    code,
			Message: message,
			Data:    errData,
		},
	})
}

func writeResponse(w http.ResponseWriter, statusCode int, response rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(response)
}

// resolveSessionTimeouts returns the effective inactivity and max-life
// durations for sessions.  The helper centralizes the logic of choosing the
// hard‑coded default `sessionTTL` when a custom inactivity timeout isn't set
// and pulling the configured max lifetime.  Both hasActiveSession and
// cleanupExpiredSessions rely on the same rules so they call this helper to
// avoid divergence.
func (s *Server) resolveSessionTimeouts() (inactivity, maxLife time.Duration) {
	inactivity = sessionTTL
	if s.cfg.SessionInactivityTimeout > 0 {
		inactivity = s.cfg.SessionInactivityTimeout
	}
	maxLife = s.cfg.SessionMaxLifetime
	return
}

func (s *Server) hasActiveSession(id string, now time.Time) (bool, string) {
	// returns (active, reason) reason is empty when active or unknown
	// otherwise one of "inactivity" or "max-lifetime".

	inactivity, maxLife := s.resolveSessionTimeouts()

	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()

	si, ok := s.sessions[id]
	if !ok {
		return false, ""
	}
	if now.Sub(si.lastSeen) > inactivity {
		delete(s.sessions, id)
		log.Printf("session %s expired due to inactivity", maskSessionID(id))
		return false, "inactivity"
	}
	if maxLife > 0 && now.Sub(si.created) > maxLife {
		delete(s.sessions, id)
		log.Printf("session %s expired due to max lifetime", maskSessionID(id))
		return false, "max-lifetime"
	}

	// update lastSeen
	si.lastSeen = now
	s.sessions[id] = si
	return true, ""
}

func (s *Server) storeSession(id string) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	now := time.Now()
	s.sessions[id] = sessionInfo{created: now, lastSeen: now}
}

func (s *Server) runSessionCleanup(ctx context.Context) {
	ticker := time.NewTicker(s.sessionSweepInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.cleanupExpiredSessions(now)
		}
	}
}

func (s *Server) sessionSweepInterval() time.Duration {
	inactivity, maxLife := s.resolveSessionTimeouts()
	sweep := sessionCleanupInterval
	if inactivity < sweep {
		sweep = inactivity
	}
	if maxLife > 0 && maxLife < sweep {
		sweep = maxLife
	}
	// Sweep more aggressively than the timeout window to avoid stale sessions
	// lingering until the full timeout elapses.
	sweep /= 2
	if sweep < time.Second {
		sweep = time.Second
	}
	return sweep
}

func maskSessionID(id string) string {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return "<empty>"
	}
	sum := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(sum[:4])
}

func (s *Server) runRateLimitCleanup(ctx context.Context) {
	ticker := time.NewTicker(rateLimitCleanupEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.rateLimiter.cleanup(rateLimitBucketMaxAge)
		}
	}
}

func (s *Server) cleanupExpiredSessions(now time.Time) {
	// mirror the logic from hasActiveSession but without logging or updating
	inactivity, maxLife := s.resolveSessionTimeouts()

	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()

	for id, si := range s.sessions {
		if now.Sub(si.lastSeen) > inactivity {
			delete(s.sessions, id)
			continue
		}
		if maxLife > 0 && now.Sub(si.created) > maxLife {
			delete(s.sessions, id)
		}
	}
}

func (s *Server) runPaymentOutcomeCleanup(ctx context.Context) {
	ticker := time.NewTicker(paymentOutcomeCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.cleanupPaymentOutcomes(now)
		}
	}
}

func (s *Server) cleanupPaymentOutcomes(now time.Time) {
	s.paymentMu.Lock()
	defer s.paymentMu.Unlock()
	s.prunePaymentOutcomesLocked(now)
}

func (s *Server) prunePaymentOutcomesLocked(now time.Time) {
	ttl := s.paymentTTL
	if ttl <= 0 {
		ttl = paymentOutcomeTTL
	}

	cutoff := now.Add(-ttl)
	for key, outcome := range s.paymentOutcomes {
		if outcome.UpdatedAt.IsZero() || outcome.UpdatedAt.Before(cutoff) {
			delete(s.paymentOutcomes, key)
		}
	}

	maxItems := s.paymentMaxItems
	if maxItems <= 0 {
		maxItems = paymentOutcomeMaxEntries
	}
	if len(s.paymentOutcomes) <= maxItems {
		return
	}

	type entry struct {
		key string
		ts  time.Time
	}
	entries := make([]entry, 0, len(s.paymentOutcomes))
	for key, outcome := range s.paymentOutcomes {
		entries = append(entries, entry{key: key, ts: outcome.UpdatedAt})
	}
	// ensure deterministic eviction order when timestamps are equal by
	// performing a stable sort and using the key as a tie-breaker.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].ts.Equal(entries[j].ts) {
			return entries[i].key < entries[j].key
		}
		return entries[i].ts.Before(entries[j].ts)
	})

	toDrop := len(entries) - maxItems
	for i := 0; i < toDrop; i++ {
		delete(s.paymentOutcomes, entries[i].key)
	}
}

func generateSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "sess_" + hex.EncodeToString(b[:]), nil
}

func loadAuthToken(cfg config.Config) string {
	if token := strings.TrimSpace(cfg.ResolvedAuthToken); token != "" {
		return token
	}

	if token := strings.TrimSpace(os.Getenv(authTokenEnvVar)); token != "" {
		return token
	}

	path := filepath.Join(cfg.StateDir, "secret.token")
	content, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(content))
}

func isOriginAllowed(origin string, allowlist []string) bool {
	parsedOrigin, err := url.Parse(origin)
	if err != nil || parsedOrigin.Scheme == "" || parsedOrigin.Host == "" {
		return false
	}

	normalizedOrigin := parsedOrigin.Scheme + "://" + strings.ToLower(parsedOrigin.Host)
	originHost := strings.ToLower(parsedOrigin.Hostname())

	for _, allowed := range allowlist {
		allowed = strings.TrimSpace(allowed)
		if allowed == "" {
			continue
		}

		if strings.Contains(allowed, "://") {
			parsedAllowed, err := url.Parse(allowed)
			if err != nil || parsedAllowed.Scheme == "" || parsedAllowed.Host == "" {
				continue
			}
			if !strings.EqualFold(parsedAllowed.Scheme, parsedOrigin.Scheme) {
				continue
			}

			// Allow entries without an explicit port (e.g. http://localhost) to match any origin port.
			if parsedAllowed.Port() == "" {
				if strings.EqualFold(parsedAllowed.Hostname(), parsedOrigin.Hostname()) {
					return true
				}
				continue
			}

			normalizedAllowed := parsedAllowed.Scheme + "://" + strings.ToLower(parsedAllowed.Host)
			if strings.EqualFold(normalizedAllowed, normalizedOrigin) {
				return true
			}
			continue
		}

		if strings.EqualFold(allowed, originHost) || strings.EqualFold(allowed, parsedOrigin.Host) {
			return true
		}
	}
	return false
}

// Close flushes and closes any cached payment log writer and file.
//
// The method is safe to call multiple times (idempotent) and is typically
// invoked during server shutdown to guarantee that any buffered payments are
// persisted. It acquires the paymentLogMu mutex, clears
// s.paymentLogWriter and s.paymentLogFile under that lock, and returns a
// combined error using errors.Join if flushing or closing fails.
func (s *Server) Close() error {
	s.paymentLogMu.Lock()
	defer s.paymentLogMu.Unlock()

	var errs []error
	if s.paymentLogWriter != nil {
		if err := s.paymentLogWriter.Flush(); err != nil {
			errs = append(errs, fmt.Errorf("payment log flush: %w", err))
		}
		s.paymentLogWriter = nil
	}
	if s.paymentLogFile != nil {
		// ensure on-disk durability: sync before closing.
		if err := s.paymentLogFile.Sync(); err != nil {
			errs = append(errs, fmt.Errorf("payment log file sync: %w", err))
		}
		if err := s.paymentLogFile.Close(); err != nil {
			errs = append(errs, fmt.Errorf("payment log file close: %w", err))
		}
		s.paymentLogFile = nil
	}
	// errors.Join will return nil when "errs" is empty, which keeps behavior
	// consistent with the previous implementation.
	return errors.Join(errs...)
}
