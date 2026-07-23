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
	ToolNameOpenMediaClip    = "dir2mcp_open_media_clip"
	ToolNameRelated          = "dir2mcp_related"
)

const (
	ErrorCodeUnauthorized    = "UNAUTHORIZED"
	ErrorCodeSessionNotFound = "SESSION_NOT_FOUND"
	ErrorCodeIndexNotReady   = "INDEX_NOT_READY"
	ErrorCodeFileNotFound    = "FILE_NOT_FOUND"
	// ErrorCodeForbidden is the canonical §14.2 code for a path or content
	// blocked by policy (exclusion globs, secret-pattern match). Distinct from
	// ErrorCodePermissionDenied, which is reserved for OS-level access failures.
	ErrorCodeForbidden         = "FORBIDDEN"
	ErrorCodePermissionDenied  = "PERMISSION_DENIED"
	ErrorCodeRateLimitExceeded = "RATE_LIMIT_EXCEEDED"
	// ErrorCodeRateLimited is kept as a compatibility alias.
	ErrorCodeRateLimited = ErrorCodeRateLimitExceeded

	// Canonical §14 codes for ingest/startup/index failures. Each names a
	// specific, machine-recognizable failure so operators (and conformance
	// clients) get a stable code rather than only free-text prose.
	//
	//   ErrorCodeFileTooLarge         §14.4 — asset exceeds the ingest size cap.
	//   ErrorCodeBinarySkipped        §14.4 — asset skipped as non-textual binary.
	//   ErrorCodeIndexVersionMismatch §14.3 — on-disk index format ≠ this binary's.
	//   ErrorCodeBindFailed           §14.1 — the server could not bind its listener.
	//   ErrorCodeTLSConfigInvalid     §14.1 — TLS cert/key flags failed validation.
	ErrorCodeFileTooLarge         = "FILE_TOO_LARGE"
	ErrorCodeBinarySkipped        = "BINARY_SKIPPED"
	ErrorCodeIndexVersionMismatch = "INDEX_VERSION_MISMATCH"
	ErrorCodeBindFailed           = "BIND_FAILED"
	ErrorCodeTLSConfigInvalid     = "TLS_CONFIG_INVALID"
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

	// FormatVersion is the semver payload-shape signal dir2mcp stamps into the
	// self-describing payloads it writes at a boundary (SPEC §1.3 / df-000,
	// #468): connection.json (MUST) and the dir2mcp_stats output (SHOULD). It is
	// an INDEPENDENT version of the payload shape — deliberately NOT the MCP
	// protocolVersion (pinned, orthogonal) nor the spec document version — so a
	// consumer can detect an incompatible payload and adapt or reject. Bump on a
	// shape change per the df-000 additive/major rules.
	FormatVersion = "0.1.0"
)

const (
	RPCMethodInitialize               = "initialize"
	RPCMethodNotificationsInitialized = "notifications/initialized"
	RPCMethodNotificationsCancelled   = "notifications/cancelled"
	RPCMethodToolsList                = "tools/list"
	RPCMethodToolsCall                = "tools/call"
)
