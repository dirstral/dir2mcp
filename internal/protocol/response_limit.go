package protocol

import (
	"errors"
	"fmt"
	"io"
	"net/http"
)

// MaxResponseBytes bounds one MCP response body that a client buffers in
// memory. The MCP server bounds each tool result on its own side: open_file
// returns at most 50000 characters, list_files returns at most 5000 entries,
// and open_media_clip returns at most 26214400 clip bytes (about 34 MiB after
// base64 encoding). 64 MiB therefore holds every legitimate response and still
// stops a hostile or broken upstream from driving unbounded allocation.
//
// The value is a constant, not a config field. An operator has no legitimate
// reason to raise it, because the server contract already caps every result. A
// config field would only give a misconfiguration a way to open the memory
// exhaustion hole again, in two separate binaries. The constant matches
// recognizeMaxResponseBytes, which bounds the recognition backend the same way.
const MaxResponseBytes int64 = 64 << 20

// ErrResponseTooLarge reports that an upstream MCP response passed
// MaxResponseBytes. Callers can test for it with errors.Is. It keeps the
// "the upstream sent too much" case separate from a JSON decode failure, so an
// operator does not read a truncated body as malformed JSON.
var ErrResponseTooLarge = errors.New("mcp response too large")

// ReadLimitedResponseBody buffers an MCP response body up to MaxResponseBytes.
// It reads one byte past the cap to find an over-limit body without buffering
// all of it. The error names the limit and holds no response content.
//
// Both MCP clients in this repository use this function for initialize, for
// tools/call, and for error bodies. One shared reader keeps the two paths from
// drifting apart.
func ReadLimitedResponseBody(body io.Reader) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(body, MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read mcp response body: %w", err)
	}
	if int64(len(data)) > MaxResponseBytes {
		return nil, fmt.Errorf("%w: the response is larger than the %d-byte limit", ErrResponseTooLarge, MaxResponseBytes)
	}
	return data, nil
}

// RefuseRedirect stops an MCP client at a 3xx response instead of following it.
// Go copies headers that it does not know as credentials (for example a custom
// X-API-Key from connection.json) onto the redirect target, and it keeps
// Authorization when only the scheme changes. A compromised or misconfigured
// endpoint could use a redirect to move those credentials to another host. The
// client surfaces the 3xx as a non-2xx status instead. MCP over HTTP POST does
// not need redirects.
func RefuseRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}
