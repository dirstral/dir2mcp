package protocol

// MCP tool names. Underscore-separated rather than dot-separated because
// Claude Desktop validates MCP tool names against the regex
// ^[a-zA-Z0-9_-]{1,64}$ and rejects the entire tool list when any name
// contains characters outside that set. Underscores keep the dir2mcp_*
// family visually grouped while staying compatible with the strictest
// known frontend.
const (
	ToolNameSearch           = "dir2mcp_search"
	ToolNameAsk              = "dir2mcp_ask"
	ToolNameAskAudio         = "dir2mcp_ask_audio"
	ToolNameOpenFile         = "dir2mcp_open_file"
	ToolNameListFiles        = "dir2mcp_list_files"
	ToolNameStats            = "dir2mcp_stats"
	ToolNameTranscribe       = "dir2mcp_transcribe"
	ToolNameAnnotate         = "dir2mcp_annotate"
	ToolNameTranscribeAndAsk = "dir2mcp_transcribe_and_ask"
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
