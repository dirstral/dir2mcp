package protocol

const (
	ToolNameSearch           = "dir2mcp.search"
	ToolNameAsk              = "dir2mcp.ask"
	ToolNameAskAudio         = "dir2mcp.ask_audio"
	ToolNameOpenFile         = "dir2mcp.open_file"
	ToolNameListFiles        = "dir2mcp.list_files"
	ToolNameStats            = "dir2mcp.stats"
	ToolNameTranscribe       = "dir2mcp.transcribe"
	ToolNameAnnotate         = "dir2mcp.annotate"
	ToolNameTranscribeAndAsk = "dir2mcp.transcribe_and_ask"
)

const (
	ErrorCodeUnauthorized      = "UNAUTHORIZED"
	ErrorCodeSessionNotFound   = "SESSION_NOT_FOUND"
	ErrorCodeIndexNotReady     = "INDEX_NOT_READY"
	ErrorCodeFileNotFound      = "FILE_NOT_FOUND"
	ErrorCodePermissionDenied  = "PERMISSION_DENIED"
	ErrorCodeRateLimitExceeded = "RATE_LIMIT_EXCEEDED"
	// ErrorCodeRateLimited is kept as a compatibility alias.
	ErrorCodeRateLimited = ErrorCodeRateLimitExceeded
)

const (
	DefaultListenAddr = "127.0.0.1:8087"
	DefaultMCPPath    = "/mcp"
	DefaultTransport  = "streamable-http"
	DefaultModel      = "mistral-small-latest"

	MCPSessionHeader         = "MCP-Session-Id"
	MCPSessionExpiredHeader  = "X-MCP-Session-Expired"
	MCPProtocolVersionHeader = "MCP-Protocol-Version"

	ProtocolDefaultVersion = "2025-11-25"
)

const (
	RPCMethodInitialize               = "initialize"
	RPCMethodNotificationsInitialized = "notifications/initialized"
	RPCMethodToolsList                = "tools/list"
	RPCMethodToolsCall                = "tools/call"
)
