package mcp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/protocol"
)

type middleware func(http.Handler) http.Handler

type requestContextKey struct{}

type requestContext struct {
	req       rpcRequest
	id        interface{}
	hasID     bool
	authScope string
}

func (s *Server) withMiddlewares(h http.Handler, mws ...middleware) http.Handler {
	wrapped := h
	for i := len(mws) - 1; i >= 0; i-- {
		if mws[i] == nil {
			continue
		}
		wrapped = mws[i](wrapped)
	}
	return wrapped
}

func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.rateLimiter != nil {
			if !s.rateLimiter.allow(realIP(r, s.rateLimiter)) {
				w.Header().Set("Retry-After", "1")
				writeError(w, http.StatusTooManyRequests, nil, -32000, "rate limit exceeded", protocol.ErrorCodeRateLimitExceeded, true)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) protocolValidationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, nil, http.StatusMethodNotAllowed, "Method Not Allowed", "METHOD_NOT_ALLOWED", false)
			return
		}

		// The same media-type rule the SDK transport applies (issue #841), so the
		// two chains cannot disagree about which request is acceptable.
		if !hasJSONContentType(r.Header.Get("Content-Type")) {
			writeError(w, http.StatusUnsupportedMediaType, nil, -32600, "Content-Type must be application/json", "INVALID_FIELD", false)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, authScope := s.authorize(w, r)
		if !ok {
			return
		}
		// Store authScope in context for later use
		ctx := r.Context()
		if rc, exists := ctx.Value(requestContextKey{}).(requestContext); exists {
			rc.authScope = authScope
			ctx = context.WithValue(ctx, requestContextKey{}, rc)
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) originMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowOrigin(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) rpcEnvelopeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, parseErr := parseRequest(r.Body)
		if parseErr != nil {
			canonicalCode := "INVALID_FIELD"
			message := "failed to read request body"
			var vErr validationError
			if errors.As(parseErr, &vErr) && vErr.canonicalCode != "" {
				canonicalCode = vErr.canonicalCode
				message = vErr.Error()
			}
			writeError(w, http.StatusBadRequest, nil, -32600, message, canonicalCode, false)
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

		if req.Method != protocol.RPCMethodInitialize {
			sessionID := strings.TrimSpace(r.Header.Get(protocol.MCPSessionHeader))
			if sessionID == "" {
				writeError(w, http.StatusNotFound, id, -32001, "session not found", protocol.ErrorCodeSessionNotFound, false)
				return
			}
			if ok, reason := s.hasActiveSession(sessionID, time.Now()); !ok {
				if reason != "" {
					w.Header().Set(protocol.MCPSessionExpiredHeader, reason)
				}
				writeError(w, http.StatusNotFound, id, -32001, "session not found", protocol.ErrorCodeSessionNotFound, false)
				return
			}
		}

		// Preserve authScope from existing context if present
		var authScope string
		if existingRC, ok := r.Context().Value(requestContextKey{}).(requestContext); ok {
			authScope = existingRC.authScope
		}

		ctx := context.WithValue(r.Context(), requestContextKey{}, requestContext{
			req:       req,
			id:        id,
			hasID:     hasID,
			authScope: authScope,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) x402Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc, ok := r.Context().Value(requestContextKey{}).(requestContext)
		if !ok {
			writeError(w, http.StatusInternalServerError, nil, -32603, "request context missing", "", false)
			return
		}
		if rc.req.Method != protocol.RPCMethodToolsCall {
			next.ServeHTTP(w, r)
			return
		}
		if !rc.hasID {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		s.handleToolsCallRequest(r.Context(), w, r, rc.req.Params, rc.id)
	})
}
