// Package providerhttp_test covers the shared hardened HTTP path that every
// provider adapter uses (issue #670).
package providerhttp_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/providerhttp"
)

func TestReadLimitedBodyRejectsAnOverLimitBody(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(strings.Repeat("a", 64))),
	}
	_, err := providerhttp.ReadLimitedBody(resp, 10, "TEST_FAILED")
	if err == nil {
		t.Fatal("an over-limit body must fail")
	}
	var pErr *model.ProviderError
	if !errors.As(err, &pErr) {
		t.Fatalf("want a *model.ProviderError, got %T", err)
	}
	if pErr.Code != "TEST_FAILED" {
		t.Fatalf("code = %q, want TEST_FAILED", pErr.Code)
	}
	if pErr.Retryable {
		t.Fatal("an over-limit body must be non-retryable")
	}
	// The error must name the limit only. Body content must never leak.
	if strings.Contains(pErr.Message, "aaa") {
		t.Fatalf("the error message leaks body content: %q", pErr.Message)
	}
	if !strings.Contains(pErr.Message, "10-byte limit") {
		t.Fatalf("the error message must name the limit, got %q", pErr.Message)
	}
}

func TestReadLimitedBodyReturnsAWithinLimitBody(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("hello")),
	}
	got, err := providerhttp.ReadLimitedBody(resp, 1000, "TEST_FAILED")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("body = %q, want %q", got, "hello")
	}
}

func TestReadLimitedBodyAcceptsAnExactLimitBody(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("0123456789")),
	}
	got, err := providerhttp.ReadLimitedBody(resp, 10, "TEST_FAILED")
	if err != nil {
		t.Fatalf("a body of exactly the limit must pass: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("len(body) = %d, want 10", len(got))
	}
}

func TestJSONHelperUsesTheJSONCap(t *testing.T) {
	if providerhttp.MaxJSONResponseBytes != 64<<20 {
		t.Fatalf("MaxJSONResponseBytes = %d, want %d", providerhttp.MaxJSONResponseBytes, int64(64<<20))
	}
	if providerhttp.MaxAudioResponseBytes != 256<<20 {
		t.Fatalf("MaxAudioResponseBytes = %d, want %d", providerhttp.MaxAudioResponseBytes, int64(256<<20))
	}
}

// TestConstructorsInstallTheRedirectPolicy pins the shared half of issue #670:
// every constructor must return a client that refuses a redirect.
func TestConstructorsInstallTheRedirectPolicy(t *testing.T) {
	base := &http.Client{Timeout: 5 * time.Second}
	cases := map[string]*http.Client{
		"NewClient":               providerhttp.NewClient(time.Second),
		"WithTimeout(nil)":        providerhttp.WithTimeout(nil, time.Second),
		"WithTimeout(base)":       providerhttp.WithTimeout(base, time.Second),
		"ClientOrDefault(nil)":    providerhttp.ClientOrDefault(nil, time.Second),
		"ClientOrDefault(base)":   providerhttp.ClientOrDefault(base, time.Second),
		"ClientOrDefault(custom)": providerhttp.ClientOrDefault(&http.Client{}, time.Second),
	}
	for name, client := range cases {
		if client.CheckRedirect == nil {
			t.Fatalf("%s returned a client with no redirect policy", name)
		}
		if err := client.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
			t.Fatalf("%s: redirect policy returned %v, want http.ErrUseLastResponse", name, err)
		}
	}
	if base.CheckRedirect != nil {
		t.Fatal("the helpers must not mutate the caller's client")
	}
	if base.Timeout != 5*time.Second {
		t.Fatal("the helpers must not mutate the caller's timeout")
	}
}

func TestWithTimeoutOverridesAndClientOrDefaultKeepsTheTimeout(t *testing.T) {
	base := &http.Client{Timeout: 5 * time.Second}
	if got := providerhttp.WithTimeout(base, 30*time.Second).Timeout; got != 30*time.Second {
		t.Fatalf("WithTimeout timeout = %v, want 30s", got)
	}
	if got := providerhttp.ClientOrDefault(base, 30*time.Second).Timeout; got != 5*time.Second {
		t.Fatalf("ClientOrDefault timeout = %v, want the caller's 5s", got)
	}
	if got := providerhttp.ClientOrDefault(nil, 30*time.Second).Timeout; got != 30*time.Second {
		t.Fatalf("ClientOrDefault(nil) timeout = %v, want the fallback 30s", got)
	}
}

// TestHardenedClientStopsAtTheRedirect proves the policy end to end: the client
// returns the 3xx response and sends nothing to the redirect target.
func TestHardenedClientStopsAtTheRedirect(t *testing.T) {
	var hits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	resp, err := providerhttp.NewClient(5 * time.Second).Get(origin.URL)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", resp.StatusCode)
	}
	if hits != 0 {
		t.Fatalf("the redirect target got %d requests, want 0", hits)
	}
}
