package tests

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dir2mcp/internal/config"
	"dir2mcp/internal/mcp"
	"dir2mcp/internal/protocol"
)

func TestRateLimit_NotActiveWhenServerIsNotPublic(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.Public = false
	cfg.RateLimitRPS = 1
	cfg.RateLimitBurst = 1

	handler := mcp.NewServer(cfg, nil).Handler()
	for i := 0; i < 5; i++ {
		rr := initializeRequestFromIP(t, handler, cfg.MCPPath, "198.51.100.10", "127.0.0.1:5001")
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d status=%d want=%d body=%s", i, rr.Code, http.StatusOK, rr.Body.String())
		}
	}
}

func TestRateLimit_ExceedingLimitReturns429AndRetryAfter(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.Public = true
	cfg.RateLimitRPS = 1
	cfg.RateLimitBurst = 1

	handler := mcp.NewServer(cfg, nil).Handler()

	first := initializeRequestFromIP(t, handler, cfg.MCPPath, "198.51.100.20", "127.0.0.1:5002")
	if first.Code != http.StatusOK {
		t.Fatalf("first request status=%d want=%d body=%s", first.Code, http.StatusOK, first.Body.String())
	}

	second := initializeRequestFromIP(t, handler, cfg.MCPPath, "198.51.100.20", "127.0.0.1:5002")
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status=%d want=%d body=%s", second.Code, http.StatusTooManyRequests, second.Body.String())
	}
	if second.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After=%q want=%q", second.Header().Get("Retry-After"), "1")
	}
	assertCanonicalErrorCode(t, second.Body.Bytes(), protocol.ErrorCodeRateLimited)
}

func TestRateLimit_TrafficBelowLimitPasses(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.Public = true
	cfg.RateLimitRPS = 10
	cfg.RateLimitBurst = 1

	handler := mcp.NewServer(cfg, nil).Handler()

	first := initializeRequestFromIP(t, handler, cfg.MCPPath, "198.51.100.30", "127.0.0.1:5003")
	if first.Code != http.StatusOK {
		t.Fatalf("first request status=%d want=%d body=%s", first.Code, http.StatusOK, first.Body.String())
	}

	time.Sleep(150 * time.Millisecond)

	second := initializeRequestFromIP(t, handler, cfg.MCPPath, "198.51.100.30", "127.0.0.1:5003")
	if second.Code != http.StatusOK {
		t.Fatalf("second request status=%d want=%d body=%s", second.Code, http.StatusOK, second.Body.String())
	}
}

func TestRateLimit_LoopbackIPsAreAlwaysExempt(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.Public = true
	cfg.RateLimitRPS = 1
	cfg.RateLimitBurst = 1

	handler := mcp.NewServer(cfg, nil).Handler()

	for _, ip := range []string{"127.0.0.1", "::1", "localhost"} {
		for i := 0; i < 5; i++ {
			rr := initializeRequestFromIP(t, handler, cfg.MCPPath, ip, "127.0.0.1:5004")
			if rr.Code != http.StatusOK {
				t.Fatalf("loopback ip=%q req=%d status=%d want=%d body=%s", ip, i, rr.Code, http.StatusOK, rr.Body.String())
			}
		}
	}
}

func TestRateLimit_BucketsAreIndependentPerIP(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.Public = true
	cfg.RateLimitRPS = 1
	cfg.RateLimitBurst = 1

	handler := mcp.NewServer(cfg, nil).Handler()

	a1 := initializeRequestFromIP(t, handler, cfg.MCPPath, "198.51.100.40", "127.0.0.1:5005")
	if a1.Code != http.StatusOK {
		t.Fatalf("first request from ip A status=%d want=%d body=%s", a1.Code, http.StatusOK, a1.Body.String())
	}

	a2 := initializeRequestFromIP(t, handler, cfg.MCPPath, "198.51.100.40", "127.0.0.1:5005")
	if a2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request from ip A status=%d want=%d body=%s", a2.Code, http.StatusTooManyRequests, a2.Body.String())
	}

	b1 := initializeRequestFromIP(t, handler, cfg.MCPPath, "198.51.100.41", "127.0.0.1:5005")
	if b1.Code != http.StatusOK {
		t.Fatalf("first request from ip B status=%d want=%d body=%s", b1.Code, http.StatusOK, b1.Body.String())
	}
}

func TestRateLimit_UntrustedPeerIgnoresXFF(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.Public = true
	cfg.RateLimitRPS = 1
	cfg.RateLimitBurst = 1

	handler := mcp.NewServer(cfg, nil).Handler()

	first := initializeRequestFromIP(t, handler, cfg.MCPPath, "198.51.100.50", "203.0.113.9:8080")
	if first.Code != http.StatusOK {
		t.Fatalf("first request status=%d want=%d body=%s", first.Code, http.StatusOK, first.Body.String())
	}

	second := initializeRequestFromIP(t, handler, cfg.MCPPath, "198.51.100.51", "203.0.113.9:8080")
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status=%d want=%d body=%s", second.Code, http.StatusTooManyRequests, second.Body.String())
	}
}

func TestRateLimit_TrustedPeerHonorsXFF(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.Public = true
	cfg.RateLimitRPS = 1
	cfg.RateLimitBurst = 1
	cfg.TrustedProxies = []string{"10.0.0.0/8"}

	handler := mcp.NewServer(cfg, nil).Handler()

	first := initializeRequestFromIP(t, handler, cfg.MCPPath, "198.51.100.60", "10.1.2.3:8443")
	if first.Code != http.StatusOK {
		t.Fatalf("first request status=%d want=%d body=%s", first.Code, http.StatusOK, first.Body.String())
	}

	second := initializeRequestFromIP(t, handler, cfg.MCPPath, "198.51.100.61", "10.1.2.3:8443")
	if second.Code != http.StatusOK {
		t.Fatalf("second request status=%d want=%d body=%s", second.Code, http.StatusOK, second.Body.String())
	}
}

// TestRateLimit_SpoofedXFFIgnored verifies that an attacker cannot bypass
// per-IP rate limiting by injecting a fake IP at the left of X-Forwarded-For.
// The proxy appends the real client IP at the right; the rate limiter must use
// the right-most untrusted entry, not the left-most.
func TestRateLimit_SpoofedXFFIgnored(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.Public = true
	cfg.RateLimitRPS = 1
	cfg.RateLimitBurst = 1
	cfg.TrustedProxies = []string{"10.0.0.0/8"}

	handler := mcp.NewServer(cfg, nil).Handler()

	// Attacker sends "fakeip, realclient" in XFF from behind a trusted proxy.
	// Both requests come from the same real client (198.51.100.70); the second
	// must be rate-limited even though the attacker rotates the left-most value.
	first := initializeRequestFromXFF(t, handler, cfg.MCPPath, "1.2.3.4, 198.51.100.70", "10.1.2.3:8443")
	if first.Code != http.StatusOK {
		t.Fatalf("first request status=%d want=%d body=%s", first.Code, http.StatusOK, first.Body.String())
	}

	second := initializeRequestFromXFF(t, handler, cfg.MCPPath, "5.6.7.8, 198.51.100.70", "10.1.2.3:8443")
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status=%d want=%d body=%s (spoofed left-most XFF must not bypass limiter)", second.Code, http.StatusTooManyRequests, second.Body.String())
	}
}

func initializeRequestFromXFF(t *testing.T, handler http.Handler, path, xff, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()

	reqBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func initializeRequestFromIP(t *testing.T, handler http.Handler, path, ip, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()

	reqBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	if ip != "" {
		req.Header.Set("X-Forwarded-For", ip)
	}

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}
