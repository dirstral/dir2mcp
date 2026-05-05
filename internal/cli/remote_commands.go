package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dir2mcp/internal/protocol"
)

type remoteConnection struct {
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	TokenFile string            `json:"token_file,omitempty"`
}

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
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpToolCallResult struct {
	IsError           bool                   `json:"isError"`
	StructuredContent map[string]interface{} `json:"structuredContent"`
}

type remoteMCPClient struct {
	endpoint   string
	authHeader string
	connection remoteConnection
	httpClient *http.Client

	mu        sync.Mutex
	initMu    sync.Mutex
	sessionID string
	nextID    atomic.Int64
}

func newRemoteMCPClient(endpoint, authHeader string, connection remoteConnection) *remoteMCPClient {
	return &remoteMCPClient{
		endpoint:   strings.TrimSpace(endpoint),
		authHeader: strings.TrimSpace(authHeader),
		connection: connection,
		httpClient: &http.Client{Timeout: 45 * time.Second},
	}
}

func (c *remoteMCPClient) nextRequestID() int64 { return c.nextID.Add(1) }

func rpcErrorSummary(body []byte) string {
	var envelope rpcResponse
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error != nil && strings.TrimSpace(envelope.Error.Message) != "" {
		return strings.TrimSpace(envelope.Error.Message)
	}
	return "upstream request failed"
}

func (c *remoteMCPClient) getSessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

func (c *remoteMCPClient) setSessionID(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessionID = strings.TrimSpace(sessionID)
}

func (c *remoteMCPClient) doRPC(ctx context.Context, reqBody rpcRequest, includeSession bool) (*http.Response, []byte, error) {
	if strings.TrimSpace(c.endpoint) == "" {
		return nil, nil, errors.New("connection url is empty")
	}

	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal rpc request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, nil, fmt.Errorf("create rpc request: %w", err)
	}
	// Apply persisted connection headers first
	for key, value := range c.connection.Headers {
		req.Header.Set(key, value)
	}
	// Set default headers if not already present
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if req.Header.Get(protocol.MCPProtocolVersionHeader) == "" {
		req.Header.Set(protocol.MCPProtocolVersionHeader, protocol.ProtocolDefaultVersion)
	}
	// Override with auth header if provided (takes precedence)
	if c.authHeader != "" {
		req.Header.Set("Authorization", c.authHeader)
	}
	if includeSession {
		if sid := c.getSessionID(); sid != "" {
			req.Header.Set(protocol.MCPSessionHeader, sid)
		}
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, fmt.Errorf("read response: %w", err)
	}
	return resp, body, nil
}

func (c *remoteMCPClient) ensureSession(ctx context.Context) error {
	if c.getSessionID() != "" {
		return nil
	}
	c.initMu.Lock()
	defer c.initMu.Unlock()
	if c.getSessionID() != "" {
		return nil
	}

	resp, body, err := c.doRPC(ctx, rpcRequest{
		JSONRPC: "2.0",
		ID:      c.nextRequestID(),
		Method:  protocol.RPCMethodInitialize,
		Params: map[string]interface{}{
			"protocolVersion": protocol.ProtocolDefaultVersion,
			"capabilities":    map[string]interface{}{},
			"clientInfo": map[string]interface{}{
				"name":    "dir2mcp-cli",
				"version": "1.0.0",
			},
		},
	}, false)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("initialize failed with HTTP %d: %s", resp.StatusCode, rpcErrorSummary(body))
	}

	var envelope rpcResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode initialize response: %w", err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("initialize failed: %s", strings.TrimSpace(envelope.Error.Message))
	}

	sid := strings.TrimSpace(resp.Header.Get(protocol.MCPSessionHeader))
	if sid == "" {
		return fmt.Errorf("initialize response missing %s", protocol.MCPSessionHeader)
	}
	c.setSessionID(sid)
	return nil
}

func (c *remoteMCPClient) CallTool(ctx context.Context, name string, arguments map[string]interface{}) (map[string]interface{}, error) {
	if err := c.ensureSession(ctx); err != nil {
		return nil, err
	}

	resp, body, err := c.doRPC(ctx, rpcRequest{
		JSONRPC: "2.0",
		ID:      c.nextRequestID(),
		Method:  protocol.RPCMethodToolsCall,
		Params: map[string]interface{}{
			"name":      name,
			"arguments": arguments,
		},
	}, true)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tools/call failed with HTTP %d: %s", resp.StatusCode, rpcErrorSummary(body))
	}

	var envelope rpcResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode tools/call response: %w", err)
	}
	if envelope.Error != nil {
		return nil, fmt.Errorf("tools/call failed: %s", strings.TrimSpace(envelope.Error.Message))
	}
	if len(envelope.Result) == 0 {
		return nil, errors.New("tools/call response missing result")
	}

	var result mcpToolCallResult
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		return nil, fmt.Errorf("decode tool result: %w", err)
	}
	if result.IsError {
		errBody := "tool returned error"
		if result.StructuredContent != nil {
			if errorNode, ok := result.StructuredContent["error"].(map[string]interface{}); ok {
				if msg, ok := errorNode["message"].(string); ok && strings.TrimSpace(msg) != "" {
					errBody = strings.TrimSpace(msg)
				}
				if code, ok := errorNode["code"].(string); ok && strings.TrimSpace(code) != "" {
					errBody = strings.TrimSpace(code) + ": " + errBody
				}
			}
		}
		return nil, errors.New(errBody)
	}
	if result.StructuredContent == nil {
		return nil, errors.New("tool response missing structuredContent")
	}
	return result.StructuredContent, nil
}

func resolveRemoteConnection(global globalOptions) (remoteConnection, error) {
	cfg, err := loadConfigWithGlobalOptions(global)
	if err != nil {
		return remoteConnection{}, fmt.Errorf("load config: %w", err)
	}
	path := filepath.Join(cfg.StateDir, connectionFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return remoteConnection{}, fmt.Errorf("read %s: %w", path, err)
	}
	var conn remoteConnection
	if err := json.Unmarshal(raw, &conn); err != nil {
		return remoteConnection{}, fmt.Errorf("parse %s: %w", path, err)
	}
	conn.URL = strings.TrimSpace(conn.URL)
	if conn.URL == "" {
		return remoteConnection{}, fmt.Errorf("%s is missing url", path)
	}
	if conn.Headers == nil {
		conn.Headers = map[string]string{}
	}
	if conn.TokenFile != "" {
		conn.TokenFile = strings.TrimSpace(conn.TokenFile)
	}
	return conn, nil
}

func authHeaderFromConnection(conn remoteConnection) (string, error) {
	auth := strings.TrimSpace(conn.Headers["Authorization"])
	if auth != "" && !strings.Contains(auth, "<") {
		return auth, nil
	}
	if conn.TokenFile == "" {
		return "", nil
	}
	tokenRaw, err := os.ReadFile(conn.TokenFile)
	if err != nil {
		return "", fmt.Errorf("read token file %s: %w", conn.TokenFile, err)
	}
	token := strings.TrimSpace(string(tokenRaw))
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", conn.TokenFile)
	}
	return "Bearer " + token, nil
}

func (a *App) remoteToolClient(global globalOptions) (*remoteMCPClient, error) {
	conn, err := resolveRemoteConnection(global)
	if err != nil {
		return nil, err
	}
	authHeader, err := authHeaderFromConnection(conn)
	if err != nil {
		return nil, err
	}
	return newRemoteMCPClient(conn.URL, authHeader, conn), nil
}

func (a *App) runSearchRemote(ctx context.Context, global globalOptions, args []string) int {
	opts, err := parseAskOptions(args)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("invalid search flags: %v", err))
		return exitConfigInvalid
	}

	client, err := a.remoteToolClient(global)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("resolve server connection: %v", err))
		return exitGeneric
	}

	payload, err := client.CallTool(ctx, protocol.ToolNameSearch, map[string]interface{}{
		"query":       opts.question,
		"k":           opts.k,
		"index":       opts.index,
		"path_prefix": opts.pathPrefix,
		"file_glob":   opts.fileGlob,
		"doc_types":   opts.docTypes,
	})
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("search failed: %v", err))
		return exitGeneric
	}
	if err := emitJSON(a.stdout, payload); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("encode search json: %v", err))
		return exitGeneric
	}
	return exitSuccess
}

func (a *App) runAskRemote(ctx context.Context, global globalOptions, opts askOptions, client *remoteMCPClient) int {
	payload, err := client.CallTool(ctx, protocol.ToolNameAsk, map[string]interface{}{
		"question":    opts.question,
		"mode":        opts.mode,
		"k":           opts.k,
		"index":       opts.index,
		"path_prefix": opts.pathPrefix,
		"file_glob":   opts.fileGlob,
		"doc_types":   opts.docTypes,
	})
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("ask failed: %v", err))
		return exitGeneric
	}

	if global.jsonOutput {
		if err := emitJSON(a.stdout, payload); err != nil {
			writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("encode ask json: %v", err))
			return exitGeneric
		}
		return exitSuccess
	}
	if global.quiet {
		return exitSuccess
	}

	answer, _ := payload["answer"].(string)
	citations, _ := payload["citations"].([]interface{})
	hits, _ := payload["hits"].([]interface{})

	writeln(a.stdout)
	if strings.TrimSpace(answer) != "" {
		writeln(a.stdout, answer)
	}
	if len(citations) > 0 {
		s := a.sty(false)
		writeln(a.stdout)
		writef(a.stdout, "  %s\n", s.sectionHeader("Citations"))
		for i, item := range citations {
			citation, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			relPath, _ := citation["rel_path"].(string)
			chunkID, _ := citation["chunk_id"].(float64)
			spanText := "?"
			if span, ok := citation["span"].(map[string]interface{}); ok {
				startLine, _ := span["start_line"].(float64)
				endLine, _ := span["end_line"].(float64)
				spanText = fmt.Sprintf("L%d-L%d", int64(startLine), int64(endLine))
			}
			writef(a.stdout, "  %s %s  %s\n",
				s.Brand.Render(fmt.Sprintf("[%d]", i+1)),
				s.Cyan.Render(relPath),
				s.dim(fmt.Sprintf("chunk=%d span=%s", int64(chunkID), spanText)),
			)
		}
	}
	if opts.mode == "search_only" && len(hits) > 0 {
		s := a.sty(false)
		writeln(a.stdout)
		writef(a.stdout, "  %s %s\n\n", s.sectionHeader("Search results"), s.dim(fmt.Sprintf("(%d hits)", len(hits))))
		for i, item := range hits {
			hit, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			relPath, _ := hit["rel_path"].(string)
			score, _ := hit["score"].(float64)
			snippet, _ := hit["snippet"].(string)
			if strings.TrimSpace(snippet) == "" {
				snippet = "(no snippet)"
			}
			writef(a.stdout, "  %s %s  %s\n", s.Brand.Render(fmt.Sprintf("[%d]", i+1)), s.Cyan.Render(relPath), s.dim(fmt.Sprintf("score=%.4f", score)))
			writef(a.stdout, "      %s\n", s.dim(snippet))
		}
	}
	writeln(a.stdout)
	return exitSuccess
}

type openFileOptions struct {
	relPath   string
	startLine int
	endLine   int
	page      int
	startMS   int
	endMS     int
	maxChars  int
}

func parseOpenFileOptions(args []string) (openFileOptions, error) {
	opts := openFileOptions{maxChars: 20000}
	fs := flag.NewFlagSet("open-file", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.IntVar(&opts.startLine, "start-line", 0, "start line")
	fs.IntVar(&opts.endLine, "end-line", 0, "end line")
	fs.IntVar(&opts.page, "page", 0, "page")
	fs.IntVar(&opts.startMS, "start-ms", 0, "start ms")
	fs.IntVar(&opts.endMS, "end-ms", 0, "end ms")
	fs.IntVar(&opts.maxChars, "max-chars", opts.maxChars, "max chars")
	if err := fs.Parse(args); err != nil {
		return openFileOptions{}, err
	}
	if fs.NArg() != 1 {
		return openFileOptions{}, errors.New("open-file command requires exactly one <rel-path> argument")
	}
	opts.relPath = strings.TrimSpace(fs.Arg(0))
	if opts.relPath == "" {
		return openFileOptions{}, errors.New("open-file path cannot be empty")
	}
	return opts, nil
}

func (a *App) runOpenFileRemote(ctx context.Context, global globalOptions, args []string) int {
	opts, err := parseOpenFileOptions(args)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("invalid open-file flags: %v", err))
		return exitConfigInvalid
	}

	client, err := a.remoteToolClient(global)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("resolve server connection: %v", err))
		return exitGeneric
	}

	toolArgs := map[string]interface{}{
		"rel_path":  opts.relPath,
		"max_chars": opts.maxChars,
	}
	if opts.startLine > 0 {
		toolArgs["start_line"] = opts.startLine
	}
	if opts.endLine > 0 {
		toolArgs["end_line"] = opts.endLine
	}
	if opts.page > 0 {
		toolArgs["page"] = opts.page
	}
	if opts.startMS > 0 {
		toolArgs["start_ms"] = opts.startMS
	}
	if opts.endMS > 0 {
		toolArgs["end_ms"] = opts.endMS
	}

	payload, err := client.CallTool(ctx, protocol.ToolNameOpenFile, toolArgs)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("open-file failed: %v", err))
		return exitGeneric
	}
	if err := emitJSON(a.stdout, payload); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("encode open-file json: %v", err))
		return exitGeneric
	}
	return exitSuccess
}

type listFilesOptions struct {
	pathPrefix    string
	glob          string
	limit         int
	offset        int
	includeHidden bool
}

func parseListFilesOptions(args []string) (listFilesOptions, error) {
	opts := listFilesOptions{limit: 200}
	fs := flag.NewFlagSet("list-files", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.pathPrefix, "path-prefix", "", "path prefix")
	fs.StringVar(&opts.glob, "glob", "", "glob")
	fs.IntVar(&opts.limit, "limit", opts.limit, "limit")
	fs.IntVar(&opts.offset, "offset", 0, "offset")
	fs.BoolVar(&opts.includeHidden, "include-hidden", false, "include hidden files")
	if err := fs.Parse(args); err != nil {
		return listFilesOptions{}, err
	}
	if fs.NArg() != 0 {
		return listFilesOptions{}, fmt.Errorf("list-files does not accept positional arguments: %s", strings.Join(fs.Args(), " "))
	}
	if opts.limit < 1 || opts.limit > 5000 {
		return listFilesOptions{}, errors.New("limit must be between 1 and 5000")
	}
	if opts.offset < 0 {
		return listFilesOptions{}, errors.New("offset must be >= 0")
	}
	return opts, nil
}

func (a *App) runListFilesRemote(ctx context.Context, global globalOptions, args []string) int {
	opts, err := parseListFilesOptions(args)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("invalid list-files flags: %v", err))
		return exitConfigInvalid
	}

	client, err := a.remoteToolClient(global)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("resolve server connection: %v", err))
		return exitGeneric
	}

	payload, err := client.CallTool(ctx, protocol.ToolNameListFiles, map[string]interface{}{
		"path_prefix":    opts.pathPrefix,
		"glob":           opts.glob,
		"limit":          opts.limit,
		"offset":         opts.offset,
		"include_hidden": opts.includeHidden,
	})
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("list-files failed: %v", err))
		return exitGeneric
	}

	filesRaw, ok := payload["files"].([]interface{})
	if !ok {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, "list-files response missing files array")
		return exitGeneric
	}
	for i, raw := range filesRaw {
		fileObj, ok := raw.(map[string]interface{})
		if !ok {
			writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("list-files response has invalid file object at index %d", i))
			return exitGeneric
		}
		if err := emitJSON(a.stdout, fileObj); err != nil {
			writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("encode list-files ndjson: %v", err))
			return exitGeneric
		}
	}
	return exitSuccess
}
