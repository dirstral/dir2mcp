// Package providerhttp holds the one hardened HTTP path that every provider
// adapter uses.
//
// Issue #416 fixed two provider-client weaknesses, but the fix lived inside
// each adapter package. Adapters added later (anthropic, cohere, colbertrerank,
// whisperapi) were written without it, so the fix regressed (issue #670). This
// package removes the duplication: an adapter gets its *http.Client from one of
// the constructors here, and it reads every success body through
// ReadLimitedBody. A new adapter that skips this package fails the boundary
// test in tests/security.
//
// The package never logs. It never puts a credential, a request payload, or a
// response payload in an error message. An error names the byte limit and the
// HTTP status only.
package providerhttp

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
)

const (
	// MaxJSONResponseBytes caps a JSON success body (embeddings, chat, OCR,
	// rerank, transcription). An adapter buffers the body before it decodes
	// it, so an upstream that streams an endless or gzip-bombed 200 response
	// could otherwise drive the daemon out of memory. Providers send even
	// inline audio as base64 inside JSON, so this one generous cap holds
	// every legitimate response.
	MaxJSONResponseBytes int64 = 64 << 20 // 64 MiB

	// MaxAudioResponseBytes caps a binary audio success body (TTS). Audio is
	// legitimately larger than JSON, so it gets a higher ceiling. The read
	// stays bounded.
	MaxAudioResponseBytes int64 = 256 << 20 // 256 MiB
)

// RefuseRedirect stops an adapter at a 3xx response instead of following it.
//
// Go strips Authorization and Cookie when a redirect crosses to another host,
// but it copies every other request header, including a custom credential
// header such as Anthropic's x-api-key, ElevenLabs' xi-api-key or Gemini's
// x-goog-api-key. A compromised endpoint, a misconfigured base_url or an
// upstream open redirect could therefore move the API key to an attacker host.
// A direct provider API call never needs a redirect, so the safe policy is to
// refuse all of them. http.ErrUseLastResponse stops at the 3xx, which the
// adapter reports as a normal non-2xx provider error, and the key is never
// sent onward.
//
// internal/protocol holds the same policy for the two MCP clients (issues #704
// and #711). The two stay apart on purpose: a provider adapter must not depend
// on the MCP wire package, and an MCP client must not depend on the provider
// packages.
func RefuseRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

// NewClient returns a client with the given timeout and the redirect policy.
// Adapters use it in their NewClient constructors.
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: RefuseRedirect}
}

// WithTimeout returns a client that uses timeout and the redirect policy. It
// shallow-copies base, so the caller's Transport (and its connection pool) is
// kept and the caller's own client is never mutated. Adapters use it for a
// per-call timeout, for example the longer generation timeout of a chat call.
func WithTimeout(base *http.Client, timeout time.Duration) *http.Client {
	if base == nil {
		return NewClient(timeout)
	}
	cp := *base
	cp.Timeout = timeout
	cp.CheckRedirect = RefuseRedirect
	return &cp
}

// ClientOrDefault returns a client with the redirect policy. It keeps the
// timeout of base, because base carries the timeout that the caller chose. A
// nil base gets a new client with fallbackTimeout. Adapters use it where a
// call has no per-call timeout of its own.
func ClientOrDefault(base *http.Client, fallbackTimeout time.Duration) *http.Client {
	if base == nil {
		return NewClient(fallbackTimeout)
	}
	cp := *base
	cp.CheckRedirect = RefuseRedirect
	return &cp
}

// ReadLimitedBody buffers a response body up to limit bytes. It reads one byte
// past the cap, so it detects an over-limit body without buffering all of it.
// An over-limit body is a non-retryable error, because a retry would read the
// same oversized response again.
//
// code is the adapter's provider error code, for example "COHERE_FAILED". The
// error text holds the limit and the status only. It never holds body content.
func ReadLimitedBody(resp *http.Response, limit int64, code string) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		// net/http always gives a client response a non-nil body, so this
		// case means the caller built the response itself. A named error
		// beats a decode error that says "unexpected end of JSON input".
		return nil, &model.ProviderError{
			Code:      code,
			Message:   "response had no body",
			Retryable: false,
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, &model.ProviderError{
			Code:       code,
			Message:    "failed to read response",
			Retryable:  true,
			StatusCode: resp.StatusCode,
			Cause:      err,
		}
	}
	if int64(len(data)) > limit {
		return nil, &model.ProviderError{
			Code:       code,
			Message:    fmt.Sprintf("response exceeds %d-byte limit", limit),
			Retryable:  false,
			StatusCode: resp.StatusCode,
		}
	}
	return data, nil
}

// ReadLimitedJSONBody is ReadLimitedBody at the JSON cap. Most call sites use
// it, so they cannot pick a wrong limit.
func ReadLimitedJSONBody(resp *http.Response, code string) ([]byte, error) {
	return ReadLimitedBody(resp, MaxJSONResponseBytes, code)
}
