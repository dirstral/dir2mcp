package elevenlabsbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dirstral/dir2mcp/internal/protocol"
)

const (
	bridgeProtocolVersion = protocol.ProtocolDefaultVersion
	bridgeClientName      = "elevenlabs-bridge"
	bridgeClientVersion   = "1.0.0"
)

type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// HTTPError preserves HTTP status codes returned by the upstream MCP server.
type HTTPError struct {
	StatusCode int
	Body       []byte
}

func (e *HTTPError) Error() string {
	if e == nil {
		return "mcp request failed"
	}
	if len(e.Body) == 0 {
		return fmt.Sprintf("mcp request failed with HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("mcp request failed with HTTP %d: %s", e.StatusCode, strings.TrimSpace(string(e.Body)))
}

// ToolContentItem mirrors MCP tool call content items.
type ToolContentItem struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
	Data     string `json:"data,omitempty"`
}

// ToolResult captures the result payload from an MCP tools/call response.
type ToolResult struct {
	IsError           bool                   `json:"isError"`
	Content           []ToolContentItem      `json:"content"`
	StructuredContent map[string]interface{} `json:"structuredContent"`
}

// Text returns the first text content item, falling back to structured answer
// fields when the tool result only exposes structured data.
func (r ToolResult) Text() string {
	for _, item := range r.Content {
		if strings.EqualFold(strings.TrimSpace(item.Type), "text") && strings.TrimSpace(item.Text) != "" {
			return strings.TrimSpace(item.Text)
		}
	}
	if r.StructuredContent != nil {
		if answer, ok := r.StructuredContent["answer"].(string); ok && strings.TrimSpace(answer) != "" {
			return strings.TrimSpace(answer)
		}
	}
	return ""
}

// Client speaks MCP over HTTP to the dir2mcp server.
type Client struct {
	endpoint   string
	token      string
	httpClient *http.Client

	mu        sync.Mutex
	initMu    sync.Mutex
	sessionID string
	nextID    atomic.Int64
}

// NewClient constructs a bridge client for the supplied MCP endpoint.
func NewClient(endpoint, token string) *Client {
	return &Client{
		endpoint: strings.TrimSpace(endpoint),
		token:    strings.TrimSpace(token),
		httpClient: &http.Client{
			Timeout:       45 * time.Second,
			CheckRedirect: protocol.RefuseRedirect,
		},
	}
}

func (c *Client) nextRequestID() int64 {
	return c.nextID.Add(1)
}

func (c *Client) authHeader() string {
	if c == nil || strings.TrimSpace(c.token) == "" {
		return ""
	}
	return "Bearer " + c.token
}

func (c *Client) sessionHeader() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

func (c *Client) setSessionHeader(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessionID = strings.TrimSpace(sessionID)
}

func (c *Client) clearSession() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessionID = ""
}

func (c *Client) doRPC(ctx context.Context, payload rpcRequest, includeSession bool) (*http.Response, []byte, error) {
	if strings.TrimSpace(c.endpoint) == "" {
		return nil, nil, fmt.Errorf("MCP_URL is required")
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal rpc request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("create rpc request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(protocol.MCPProtocolVersionHeader, bridgeProtocolVersion)
	if auth := c.authHeader(); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if includeSession {
		if sessionID := c.sessionHeader(); sessionID != "" {
			req.Header.Set(protocol.MCPSessionHeader, sessionID)
		}
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("do rpc request: %w", err)
	}
	defer func() {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()

	// The bound covers every status. An over-limit error body is rejected here,
	// so it never reaches HTTPError and the bridge never proxies it downstream
	// (issue #711).
	raw, readErr := protocol.ReadLimitedResponseBody(resp.Body)
	if readErr != nil {
		return resp, nil, readErr
	}
	return resp, raw, nil
}

func (c *Client) ensureSession(ctx context.Context) error {
	if strings.TrimSpace(c.sessionHeader()) != "" {
		return nil
	}
	// Serialize initialize so concurrent calls don't race on session creation.
	c.initMu.Lock()
	defer c.initMu.Unlock()
	if strings.TrimSpace(c.sessionHeader()) != "" {
		return nil
	}

	resp, body, err := c.doRPC(ctx, rpcRequest{
		JSONRPC: "2.0",
		ID:      c.nextRequestID(),
		Method:  protocol.RPCMethodInitialize,
		Params: map[string]interface{}{
			"protocolVersion": bridgeProtocolVersion,
			"capabilities":    map[string]interface{}{},
			"clientInfo": map[string]interface{}{
				"name":    bridgeClientName,
				"version": bridgeClientVersion,
			},
		},
	}, false)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{StatusCode: resp.StatusCode, Body: body}
	}

	var envelope rpcResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode initialize response: %w", err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("initialize failed: %s", strings.TrimSpace(envelope.Error.Message))
	}

	sessionID := strings.TrimSpace(resp.Header.Get(protocol.MCPSessionHeader))
	if sessionID == "" {
		return fmt.Errorf("initialize response missing %s", protocol.MCPSessionHeader)
	}
	c.setSessionHeader(sessionID)
	return nil
}

// CallTool invokes an MCP tool and decodes the tool result payload.
func (c *Client) CallTool(ctx context.Context, name string, arguments map[string]interface{}) (ToolResult, error) {
	if err := c.ensureSession(ctx); err != nil {
		return ToolResult{}, err
	}

	call := rpcRequest{
		JSONRPC: "2.0",
		ID:      c.nextRequestID(),
		Method:  protocol.RPCMethodToolsCall,
		Params: map[string]interface{}{
			"name":      name,
			"arguments": arguments,
		},
	}

	resp, body, err := c.doRPC(ctx, call, true)
	if err != nil {
		// A read failure or an over-limit body hides the JSON-RPC payload, so
		// only the status and the headers are left. Drop the session on an auth
		// status or on an expired-session header. Otherwise the bridge would
		// keep a dead session for good. The body is nil here, so the check uses
		// the header alone.
		if isAuthStatus(resp) || isSessionExpired(resp, nil) {
			c.clearSession()
		}
		return ToolResult{}, err
	}
	if isAuthStatus(resp) || isSessionExpired(resp, body) {
		c.clearSession()
		return ToolResult{}, &HTTPError{StatusCode: resp.StatusCode, Body: body}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ToolResult{}, &HTTPError{StatusCode: resp.StatusCode, Body: body}
	}

	var envelope rpcResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ToolResult{}, fmt.Errorf("decode tools/call response: %w", err)
	}
	if envelope.Error != nil {
		return ToolResult{}, fmt.Errorf("tools/call failed: %s", strings.TrimSpace(envelope.Error.Message))
	}
	if len(envelope.Result) == 0 {
		return ToolResult{}, fmt.Errorf("tools/call response missing result")
	}

	var result ToolResult
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		return ToolResult{}, fmt.Errorf("decode tool result: %w", err)
	}
	return result, nil
}

// isAuthStatus reports whether the upstream refused the request for an
// authentication or authorization reason.
func isAuthStatus(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	return resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden
}

func isSessionExpired(resp *http.Response, body []byte) bool {
	if resp == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(resp.Header.Get(protocol.MCPSessionExpiredHeader)), "true") {
		return true
	}
	if resp.StatusCode != http.StatusNotFound {
		return false
	}
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "session not found") ||
		strings.Contains(lower, strings.ToLower(protocol.ErrorCodeSessionNotFound))
}
