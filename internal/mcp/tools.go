package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/dirstral/dir2mcp/internal/avutil"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/mistral"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/provider"
	"github.com/dirstral/dir2mcp/internal/providerfactory"
)

const (
	defaultEmbedTextModel = "mistral-embed"
	defaultEmbedCodeModel = "codestral-embed"
	defaultOCRModel       = mistral.DefaultOCRModel
	defaultSTTProvider    = "mistral"
	defaultSTTModel       = "voxtral-mini-latest"
	defaultChatModel      = mistral.DefaultChatModel

	// maximum combined character count of schema+text+prompt instructions
	// that will be sent to the Mistral client.  If an annotate request
	// creates a longer prompt we reject it rather than relying on the
	// provider to fail; this helps make errors predictable and avoids
	// accidental OOMs or context-length errors coming back from the
	// remote API.  The value is intentionally generous but still bounded.
	maxMistralContextChars = 200000
)

var toolOrder = []string{
	protocol.ToolNameSearch,
	protocol.ToolNameAsk,
	protocol.ToolNameAskAudio,
	protocol.ToolNameTranscribe,
	protocol.ToolNameAnnotate,
	protocol.ToolNameTranscribeAndAsk,
	protocol.ToolNameOpenFile,
	protocol.ToolNameOpenMediaClip,
	protocol.ToolNameListFiles,
	protocol.ToolNameStats,
}

type toolHandler func(context.Context, map[string]interface{}) (toolCallResult, *toolExecutionError)

type toolDefinition struct {
	Name         string                 `json:"name"`
	Description  string                 `json:"description"`
	InputSchema  map[string]interface{} `json:"inputSchema"`
	OutputSchema map[string]interface{} `json:"outputSchema,omitempty"`
	handler      toolHandler            `json:"-"`
}

type toolsCallParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments,omitempty"`
}

type toolCallResult struct {
	Content           []toolContentItem `json:"content"`
	StructuredContent interface{}       `json:"structuredContent,omitempty"`
	IsError           bool              `json:"isError,omitempty"`
}

type toolContentItem struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
}

type toolExecutionError struct {
	Code      string
	Message   string
	Retryable bool
}

type retrieverOpenFileWithMeta interface {
	OpenFileWithMeta(ctx context.Context, relPath string, span model.Span, maxChars int) (string, bool, error)
}

// storeChunkMediaSpan is the optional store capability that resolves a chunk id
// to its source media (rel_path/doc_type) and time span for
// dir2mcp_open_media_clip (SPEC §15.11). It is type-asserted on s.store so a
// store without media-chunk support degrades to the rel_path+range input path.
type storeChunkMediaSpan interface {
	ChunkMediaSpanByID(ctx context.Context, chunkID int64) (relPath, docType string, span model.Span, err error)
}

type voiceAwareTTSSynthesizer interface {
	SynthesizeWithVoice(ctx context.Context, text, voiceID string) ([]byte, error)
}

func (s *Server) buildToolRegistry() map[string]toolDefinition {
	return map[string]toolDefinition{
		protocol.ToolNameSearch: {
			Name:         protocol.ToolNameSearch,
			Description:  "Semantic retrieval across indexed content.",
			InputSchema:  searchInputSchema(),
			OutputSchema: searchOutputSchema(),
			handler:      s.handleSearchTool,
		},
		protocol.ToolNameAsk: {
			Name:         protocol.ToolNameAsk,
			Description:  "RAG answer with citations; can run search-only mode.",
			InputSchema:  askInputSchema(),
			OutputSchema: askOutputSchema(),
			handler:      s.handleAskTool,
		},
		protocol.ToolNameAskAudio: {
			Name:         protocol.ToolNameAskAudio,
			Description:  "RAG answer with optional ElevenLabs audio synthesis.",
			InputSchema:  askAudioInputSchema(),
			OutputSchema: askAudioOutputSchema(),
			handler:      s.handleAskAudioTool,
		},
		protocol.ToolNameTranscribe: {
			Name:         protocol.ToolNameTranscribe,
			Description:  "Force transcription for an indexed audio document.",
			InputSchema:  transcribeInputSchema(),
			OutputSchema: transcribeOutputSchema(),
			handler:      s.handleTranscribeTool,
		},
		protocol.ToolNameAnnotate: {
			Name:         protocol.ToolNameAnnotate,
			Description:  "Structured extraction using provided JSON schema.",
			InputSchema:  annotateInputSchema(),
			OutputSchema: annotateOutputSchema(),
			handler:      s.handleAnnotateTool,
		},
		protocol.ToolNameTranscribeAndAsk: {
			Name:         protocol.ToolNameTranscribeAndAsk,
			Description:  "Ensure transcript exists for audio file, then answer a question with citations.",
			InputSchema:  transcribeAndAskInputSchema(),
			OutputSchema: transcribeAndAskOutputSchema(),
			handler:      s.handleTranscribeAndAskTool,
		},
		protocol.ToolNameOpenFile: {
			Name:         protocol.ToolNameOpenFile,
			Description:  "Open an exact source slice for verification.",
			InputSchema:  openFileInputSchema(),
			OutputSchema: openFileOutputSchema(),
			handler:      s.handleOpenFileTool,
		},
		protocol.ToolNameOpenMediaClip: {
			Name:         protocol.ToolNameOpenMediaClip,
			Description:  "Extract the audio/video snippet for a media hit (time span); the media analogue of open_file.",
			InputSchema:  openMediaClipInputSchema(),
			OutputSchema: openMediaClipOutputSchema(),
			handler:      s.handleOpenMediaClipTool,
		},
		protocol.ToolNameListFiles: {
			Name:         protocol.ToolNameListFiles,
			Description:  "List files under root for navigation and filter selection.",
			InputSchema:  listFilesInputSchema(),
			OutputSchema: listFilesOutputSchema(),
			handler:      s.handleListFilesTool,
		},
		protocol.ToolNameStats: {
			Name:         protocol.ToolNameStats,
			Description:  "Status/progress/health for indexing and models.",
			InputSchema:  statsInputSchema(),
			OutputSchema: statsOutputSchema(),
			handler:      s.handleStatsTool,
		},
	}
}

func (s *Server) handleToolsList(w http.ResponseWriter, id interface{}) {
	tools := make([]toolDefinition, 0, len(s.tools))

	for _, name := range toolOrder {
		if tool, ok := s.tools[name]; ok {
			tools = append(tools, tool)
		}
	}

	if len(tools) == 0 {
		names := make([]string, 0, len(s.tools))
		for name := range s.tools {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			tools = append(tools, s.tools[name])
		}
	}

	writeResult(w, http.StatusOK, id, map[string]interface{}{
		"tools": tools,
	})
}

func (s *Server) handleToolsCall(ctx context.Context, w http.ResponseWriter, rawParams json.RawMessage, id interface{}) {
	result, statusCode, rpcErr := s.processToolsCall(ctx, rawParams)
	if rpcErr != nil {
		writeResponse(w, statusCode, rpcResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error:   rpcErr,
		})
		return
	}
	writeResult(w, statusCode, id, result)
}

func (s *Server) processToolsCall(ctx context.Context, rawParams json.RawMessage) (toolCallResult, int, *rpcError) {
	params, err := parseToolsCallParams(rawParams)
	if err != nil {
		canonicalCode := "INVALID_FIELD"
		var vErr validationError
		if errors.As(err, &vErr) && vErr.canonicalCode != "" {
			canonicalCode = vErr.canonicalCode
		}
		return toolCallResult{}, http.StatusBadRequest, &rpcError{
			Code:    -32600,
			Message: err.Error(),
			Data: &rpcErrorData{
				Code:      canonicalCode,
				Retryable: false,
			},
		}
	}

	tool, ok := s.tools[params.Name]
	if !ok {
		return newToolErrorResult(toolExecutionError{
			Code:      "METHOD_NOT_FOUND",
			Message:   fmt.Sprintf("unknown tool: %s", params.Name),
			Retryable: false,
		}), http.StatusOK, nil
	}

	result, toolErr := s.invokeToolHandler(ctx, tool, params.Name, params.Arguments)
	if toolErr != nil {
		return newToolErrorResult(*toolErr), http.StatusOK, nil
	}

	return result, http.StatusOK, nil
}

// invokeToolHandler runs a tool handler with panic recovery. A bug in any
// handler is converted into a clean INTERNAL_ERROR tool result (and the stack is
// logged) rather than propagating out through net/http, which would recover the
// panic at the server level by abruptly closing the connection — surfacing on
// the client as an opaque "Failed to call tool" with the stack written only to
// stderr. Returning a normal tool-error result instead lets the model read and
// report the failure, and the logged stack makes the underlying bug
// diagnosable. See issue #356.
func (s *Server) invokeToolHandler(ctx context.Context, tool toolDefinition, name string, args map[string]interface{}) (result toolCallResult, toolErr *toolExecutionError) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("mcp: tool %q handler panicked: %v\n%s", name, r, debug.Stack())
			result = toolCallResult{}
			toolErr = &toolExecutionError{
				Code:      "INTERNAL_ERROR",
				Message:   "internal error while executing tool",
				Retryable: false,
			}
		}
	}()
	return tool.handler(ctx, args)
}

func parseToolsCallParams(raw json.RawMessage) (toolsCallParams, error) {
	if len(raw) == 0 {
		return toolsCallParams{}, validationError{
			message:       "params is required",
			canonicalCode: "MISSING_FIELD",
		}
	}

	var params toolsCallParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return toolsCallParams{}, validationError{
			message:       "invalid tools/call params",
			canonicalCode: "INVALID_FIELD",
		}
	}

	params.Name = strings.TrimSpace(params.Name)
	if params.Name == "" {
		return toolsCallParams{}, validationError{
			message:       "tools/call params.name is required",
			canonicalCode: "MISSING_FIELD",
		}
	}
	if params.Arguments == nil {
		params.Arguments = map[string]interface{}{}
	}

	return params, nil
}

func newToolErrorResult(toolErr toolExecutionError) toolCallResult {
	text := fmt.Sprintf("ERROR: %s: %s", toolErr.Code, toolErr.Message)
	return toolCallResult{
		IsError: true,
		Content: []toolContentItem{
			{Type: "text", Text: text},
		},
		StructuredContent: map[string]interface{}{
			"error": map[string]interface{}{
				"code":      toolErr.Code,
				"message":   toolErr.Message,
				"retryable": toolErr.Retryable,
			},
		},
	}
}

func (s *Server) handleStatsTool(ctx context.Context, args map[string]interface{}) (toolCallResult, *toolExecutionError) {
	if err := assertNoUnknownArguments(args, map[string]struct{}{}); err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}

	retrievedStats := model.Stats{
		Root:            s.cfg.RootDir,
		StateDir:        s.cfg.StateDir,
		ProtocolVersion: s.cfg.ProtocolVersion,
		CorpusStats:     model.CorpusStats{DocCounts: map[string]int64{}},
	}
	statsFromRetriever := false
	if s.retriever != nil {
		stats, err := s.retriever.Stats(ctx)
		if err != nil {
			if !errors.Is(err, model.ErrNotImplemented) {
				return toolCallResult{}, &toolExecutionError{Code: "STORE_CORRUPT", Message: err.Error(), Retryable: false}
			}
		} else {
			statsFromRetriever = true
			retrievedStats = stats
			if retrievedStats.DocCounts == nil {
				retrievedStats.DocCounts = map[string]int64{}
			}
			if strings.TrimSpace(retrievedStats.Root) == "" {
				retrievedStats.Root = s.cfg.RootDir
			}
			if strings.TrimSpace(retrievedStats.StateDir) == "" {
				retrievedStats.StateDir = s.cfg.StateDir
			}
			if strings.TrimSpace(retrievedStats.ProtocolVersion) == "" {
				retrievedStats.ProtocolVersion = s.cfg.ProtocolVersion
			}
		}
	}

	snapshot := s.indexing.Snapshot()
	if !statsFromRetriever {
		retrievedStats.Scanned = snapshot.Scanned
		retrievedStats.Indexed = snapshot.Indexed
		retrievedStats.Skipped = snapshot.Skipped
		retrievedStats.Deleted = snapshot.Deleted
		retrievedStats.Representations = snapshot.Representations
		retrievedStats.ChunksTotal = snapshot.ChunksTotal
		retrievedStats.EmbeddedOK = snapshot.EmbeddedOK
		retrievedStats.Errors = snapshot.Errors
	}
	structured := map[string]interface{}{
		"root":             retrievedStats.Root,
		"state_dir":        retrievedStats.StateDir,
		"protocol_version": retrievedStats.ProtocolVersion,
		"doc_counts":       retrievedStats.DocCounts,
		"total_docs":       retrievedStats.TotalDocs,
		// indicates whether the above counts originate from the underlying
		// retriever.  when false the map will be zero-valued (not nil) and
		// total_docs will be 0, so consumers must not assume those values
		// represent an empty corpus without this flag.
		"doc_counts_available": statsFromRetriever,
		"indexing": map[string]interface{}{
			"job_id":          snapshot.JobID,
			"running":         snapshot.Running,
			"mode":            snapshot.Mode,
			"scanned":         retrievedStats.Scanned,
			"indexed":         retrievedStats.Indexed,
			"skipped":         retrievedStats.Skipped,
			"deleted":         retrievedStats.Deleted,
			"representations": retrievedStats.Representations,
			"chunks_total":    retrievedStats.ChunksTotal,
			"embedded_ok":     retrievedStats.EmbeddedOK,
			"errors":          retrievedStats.Errors,
		},
		"models": map[string]interface{}{
			"embed_text":   defaultEmbedTextModel,
			"embed_code":   defaultEmbedCodeModel,
			"ocr":          defaultOCRModel,
			"stt_provider": defaultSTTProvider,
			"stt_model":    defaultSTTModel,
			"chat": func() string {
				cp, err := s.cfg.Providers().Resolve(provider.CapChat)
				if err != nil {
					// No chat provider resolves; report the built-in default.
					return defaultChatModel
				}
				if m := strings.TrimSpace(cp.ChatModel); m != "" {
					return m
				}
				// Resolved but no explicit model: the adapter uses its
				// own provider-kind default — don't misreport Mistral's.
				return "(" + cp.Name + " provider default)"
			}(),
		},
	}
	if failures := loadRecentFailuresForStats(ctx, s.store); len(failures) > 0 {
		structured["recent_failures"] = failures
	}

	s.sessionMu.RLock()
	sessionItems := make([]map[string]interface{}, 0, len(s.sessions))
	for id, si := range s.sessions {
		sessionItems = append(sessionItems, map[string]interface{}{
			"id":             maskSessionID(id),
			"created_unix":   si.created.Unix(),
			"last_seen_unix": si.lastSeen.Unix(),
		})
	}
	s.sessionMu.RUnlock()
	sort.Slice(sessionItems, func(i, j int) bool {
		left, _ := sessionItems[i]["id"].(string)
		right, _ := sessionItems[j]["id"].(string)
		return left < right
	})
	structured["sessions"] = map[string]interface{}{
		"active": len(sessionItems),
		"items":  sessionItems,
	}

	text := fmt.Sprintf(
		"indexing running=%t scanned=%d indexed=%d errors=%d",
		snapshot.Running,
		retrievedStats.Scanned,
		retrievedStats.Indexed,
		retrievedStats.Errors,
	)

	return toolCallResult{
		Content: []toolContentItem{
			{Type: "text", Text: text},
		},
		StructuredContent: structured,
	}, nil
}

// statsRecentFailuresLimit caps the recent_failures array per the spec
// (SPEC §15.6 "Implementations SHOULD cap at 20 entries by default").
const statsRecentFailuresLimit = 20

// statsErrorMessageRedactors is a small safety net of common credential
// shapes applied at the MCP boundary before emitting recent_failures.
// The write-side already runs the operator's configured
// `secret_patterns` against err.Error() (see ingest.RedactSecretsInMessage,
// PR #212) — this is belt-and-suspenders for the cases where the
// operator hasn't configured any patterns or where a non-SQLite store
// backend persists raw text. SPEC §15.6: "error_message MUST NOT
// contain secrets". Kept tight (high-confidence shapes only) to avoid
// false positives that would hide the actionable failure description.
var statsErrorMessageRedactors = []*regexp.Regexp{
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-+/=]{20,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),                                                 // AWS access key
	regexp.MustCompile(`sk-[A-Za-z0-9_\-]{20,}`),                                           // OpenAI / Mistral / Anthropic style
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9_]{20,}`),                                      // GitHub PAT / OAuth
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),                                     // Slack
	regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-.]{10,}\.[A-Za-z0-9_\-]{5,}`), // JWT
}

// redactStatsErrorMessage runs the high-confidence credential
// redactors over msg, replacing each match with the literal
// "[REDACTED]". Returns msg unchanged when no pattern matches.
func redactStatsErrorMessage(msg string) string {
	for _, rx := range statsErrorMessageRedactors {
		msg = rx.ReplaceAllString(msg, "[REDACTED]")
	}
	return msg
}

// loadRecentFailuresForStats returns the per-doc projection emitted in
// dir2mcp_stats's optional recent_failures array: rel_path, doc_type,
// mtime_unix, error_message — the spec-required item shape, no more
// (additionalProperties:false on the item schema).
//
// Type-asserts the store rather than touching the model.Store interface
// so mocks/in-memory backends used in tests don't need an extra method.
// Returns nil (not empty slice) when stats are unavailable or when the
// store has no failures; the caller omits the field entirely in that
// case, matching the spec's "MAY omit when no failures are recorded".
//
// Errors from the underlying query are silently swallowed (return nil)
// — a healthy stats call must not fail because the failure-aggregation
// side query had a transient issue. The MCP layer has no logger handle
// here; if richer diagnostics are needed later, plumb one through.
func loadRecentFailuresForStats(ctx context.Context, st model.Store) []map[string]interface{} {
	if st == nil {
		return nil
	}
	type recentFailuresLister interface {
		RecentFailures(ctx context.Context, limit int) ([]model.Document, error)
	}
	rf, ok := st.(recentFailuresLister)
	if !ok {
		return nil
	}
	docs, err := rf.RecentFailures(ctx, statsRecentFailuresLimit)
	if err != nil || len(docs) == 0 {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(docs))
	for _, d := range docs {
		out = append(out, map[string]interface{}{
			"rel_path":      d.RelPath,
			"doc_type":      d.DocType,
			"mtime_unix":    d.MTimeUnix,
			"error_message": redactStatsErrorMessage(d.ErrorMessage),
		})
	}
	return out
}

func (s *Server) handleListFilesTool(ctx context.Context, args map[string]interface{}) (toolCallResult, *toolExecutionError) {
	if err := assertNoUnknownArguments(args, map[string]struct{}{
		"path_prefix": {}, "glob": {}, "limit": {}, "offset": {}, "include_hidden": {},
	}); err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	pathPrefix, glob, limit, offset, includeHidden, toolErr := parseListFilesArgs(args)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}

	var (
		docs  []model.Document
		total int64
	)
	if s.store == nil {
		docs = []model.Document{}
		total = 0
	} else {
		var (
			listedDocs  []model.Document
			listedTotal int64
			listErr     error
		)
		listedDocs, listedTotal, listErr = s.listFilesFiltered(ctx, pathPrefix, glob, limit, offset, includeHidden)
		if listErr != nil && !errors.Is(listErr, model.ErrNotImplemented) {
			return toolCallResult{}, &toolExecutionError{
				Code:      "STORE_CORRUPT",
				Message:   listErr.Error(),
				Retryable: false,
			}
		}
		if listErr == nil {
			docs = listedDocs
			total = listedTotal
		}
	}

	files := make([]map[string]interface{}, 0, len(docs))
	for _, doc := range docs {
		status := normalizeFileStatus(doc.Status)
		entry := map[string]interface{}{
			"rel_path":   doc.RelPath,
			"doc_type":   doc.DocType,
			"size_bytes": doc.SizeBytes,
			"mtime_unix": doc.MTimeUnix,
			"status":     status,
			"deleted":    doc.Deleted,
		}
		if title := strings.TrimSpace(doc.Title); title != "" {
			entry["title"] = title
		}
		files = append(files, entry)
	}

	structured := map[string]interface{}{
		"limit":  limit,
		"offset": offset,
		"total":  total,
		"files":  files,
	}

	text := fmt.Sprintf("listed %d file(s) (total=%d, limit=%d, offset=%d)", len(files), total, limit, offset)
	return toolCallResult{
		Content: []toolContentItem{
			{Type: "text", Text: text},
		},
		StructuredContent: structured,
	}, nil
}

// listFilesFiltered paginates s.store.ListFiles and skips entries that violate
// the list_files contract. Skipped entries are:
//   - hidden paths (when includeHidden is false), so internal artifacts under
//     dot-prefixed directories don't leak into the public listing.
//   - filesystem-source documents whose rel_path no longer resolves to a real
//     file under the configured root. Without this filter, stale rows left
//     behind by older buggy ingest paths or manual edits would surface here
//     and then 404 on the round-trip through open_file (issue #176).
//
// The total is the count of entries that survived filtering — the surviving
// "real" total from the agent's perspective — not the raw store count.
func (s *Server) listFilesFiltered(ctx context.Context, pathPrefix, glob string, limit, offset int, includeHidden bool) ([]model.Document, int64, error) {
	const pageSize = 500
	collected := make([]model.Document, 0, limit)
	visibleSeen := 0
	storeOffset := 0
	var storeTotal int64

	// Resolve root once up front. On large corpora the per-document gate would
	// otherwise re-run filepath.Abs + EvalSymlinks(root) for every row, which
	// is syscall-heavy enough to show up on `list_files`.
	var (
		rootResolvable bool
		rootAbs        string
		rootReal       string
	)
	if abs, resolved, ok := s.resolveRoot(); ok {
		rootResolvable = true
		rootAbs = abs
		rootReal = resolved
	}

	for {
		docs, total, err := s.store.ListFiles(ctx, pathPrefix, glob, pageSize, storeOffset)
		if err != nil {
			return nil, 0, err
		}
		storeTotal = total
		if len(docs) == 0 {
			break
		}
		for _, doc := range docs {
			if !includeHidden && isListFilesNoisePath(doc.RelPath) {
				continue
			}
			if rootResolvable && !isResolvableSourceWithRoot(doc, rootAbs, rootReal) {
				continue
			}
			if visibleSeen >= offset && len(collected) < limit {
				collected = append(collected, doc)
			}
			visibleSeen++
		}
		storeOffset += len(docs)
		if int64(storeOffset) >= storeTotal {
			break
		}
	}

	return collected, int64(visibleSeen), nil
}

// resolveRoot resolves the configured RootDir to its absolute and
// symlink-evaluated forms. The bool return reports whether RootDir points to
// a real directory on disk; when false, callers should skip any per-document
// resolution check so that misconfigured deployments still return whatever
// the store has rather than an empty list.
func (s *Server) resolveRoot() (rootAbs, rootReal string, ok bool) {
	rootDir := strings.TrimSpace(s.cfg.RootDir)
	if rootDir == "" {
		return "", "", false
	}

	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return "", "", false
	}

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", false
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", "", false
	}
	if !info.IsDir() {
		return "", "", false
	}

	return abs, resolved, true
}

// canResolveRoot is a thin wrapper over resolveRoot for callers that only
// need the boolean signal.
func (s *Server) canResolveRoot() bool {
	_, _, ok := s.resolveRoot()
	return ok
}

// isResolvableSourceWithRoot reports whether doc's rel_path should be surfaced
// by list_files, using caller-cached rootAbs/rootReal values to avoid re-running
// filepath.Abs + EvalSymlinks(root) per document. Archive members are virtual
// paths (the backing file is the archive itself) so they're always considered
// resolvable.
//
// The filter is deliberately fail-OPEN (issue #286 Bug A): a document is only
// excluded when we can AFFIRMATIVELY determine its source is gone (the path does
// not exist) or genuinely lives outside the configured root. On any resolution
// ambiguity — EvalSymlinks failing for a reason other than "not exist", or a
// filepath.Rel error — we INCLUDE the document. This keeps real corpora visible
// under symlinked roots (macOS /Users↔/private, /tmp→/private/tmp, or a corpus
// reached through a symlinked path component), where the older fail-closed check
// silently emptied the whole listing for files that actually exist.
//
// Both the root (rootReal, already EvalSymlinks-resolved by resolveRoot) and the
// candidate target are compared in their symlink-resolved form so equivalent
// symlinked paths match.
func isResolvableSourceWithRoot(doc model.Document, rootAbs, rootReal string) bool {
	if strings.EqualFold(strings.TrimSpace(doc.SourceType), "archive_member") {
		return true
	}
	normalized := filepath.ToSlash(filepath.Clean(strings.TrimSpace(doc.RelPath)))
	if normalized == "." || normalized == ".." || strings.HasPrefix(normalized, "../") || filepath.IsAbs(doc.RelPath) {
		// An affirmatively malformed rel_path (traversal / absolute) can never
		// round-trip through open_file, so excluding it is safe.
		return false
	}
	absPath := filepath.Join(rootAbs, filepath.FromSlash(normalized))
	targetReal, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		// Only treat a definitive "does not exist" as proof the source is gone.
		// Any other error (permissions, transient I/O, symlink-evaluation
		// ambiguity) is inconclusive, so we fail open and keep the document.
		if errors.Is(err, fs.ErrNotExist) {
			return false
		}
		return true
	}
	rel, err := filepath.Rel(rootReal, targetReal)
	if err != nil {
		// We could not compute a relationship to the root; that is ambiguous,
		// not proof the file is outside the root, so fail open and include it.
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// The resolved target sits outside the resolved root: affirmatively
		// out-of-bounds, so exclude it.
		return false
	}
	return true
}

func (s *Server) handleSearchTool(ctx context.Context, args map[string]interface{}) (toolCallResult, *toolExecutionError) {
	if err := assertNoUnknownArguments(args, map[string]struct{}{
		"query": {}, "k": {}, "index": {}, "path_prefix": {}, "file_glob": {}, "doc_types": {}, "speaker": {}, "languages": {},
	}); err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	query, ok, err := parseRequiredString(args, "query")
	if err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	if !ok {
		return toolCallResult{}, &toolExecutionError{Code: "MISSING_FIELD", Message: "query is required", Retryable: false}
	}
	k, toolErr := parseKArg(args)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}
	indexName, toolErr := parseIndexArg(args)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}
	pathPrefix, fileGlob, docTypes, toolErr := parseSearchFilters(args)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}
	speaker, err := parseOptionalString(args, "speaker")
	if err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	languages, toolErr := parseLanguagesArg(args)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}
	if s.retriever == nil {
		return toolCallResult{}, &toolExecutionError{Code: protocol.ErrorCodeIndexNotReady, Message: "retriever not configured", Retryable: false}
	}
	hits, searchErr := s.retriever.Search(ctx, model.SearchQuery{
		Query: query, K: k, Index: indexName, PathPrefix: pathPrefix, FileGlob: fileGlob, DocTypes: docTypes, Speaker: speaker, Languages: languages,
	})
	if searchErr != nil {
		return toolCallResult{}, mapSearchError(searchErr)
	}
	indexUsed := "text"
	switch indexName {
	case "code":
		indexUsed = "code"
	case "both":
		indexUsed = "both"
	}
	indexingComplete := true
	if ic, err := s.retriever.IndexingComplete(ctx); err == nil {
		indexingComplete = ic
	}
	structured := map[string]interface{}{
		"query": query, "k": k, "index_used": indexUsed,
		"hits": serializeSearchHits(hits), "indexing_complete": indexingComplete,
	}
	return toolCallResult{
		Content:           []toolContentItem{{Type: "text", Text: fmt.Sprintf("found %d result(s)", len(hits))}},
		StructuredContent: structured,
	}, nil
}

func (s *Server) handleAskTool(ctx context.Context, args map[string]interface{}) (toolCallResult, *toolExecutionError) {
	if err := assertNoUnknownArguments(args, map[string]struct{}{
		"question": {}, "k": {}, "mode": {}, "index": {}, "path_prefix": {}, "file_glob": {}, "doc_types": {}, "languages": {},
	}); err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	question, ok, err := parseRequiredString(args, "question")
	if err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	if !ok {
		return toolCallResult{}, &toolExecutionError{Code: "MISSING_FIELD", Message: "question is required", Retryable: false}
	}
	k, toolErr := parseKArg(args)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}
	mode, toolErr := parseModeArg(args)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}
	indexName, toolErr := parseIndexArg(args)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}
	pathPrefix, fileGlob, docTypes, toolErr := parseSearchFilters(args)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}
	languages, toolErr := parseLanguagesArg(args)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}
	if s.retriever == nil {
		return toolCallResult{}, &toolExecutionError{Code: protocol.ErrorCodeIndexNotReady, Message: "retriever not configured", Retryable: false}
	}
	sq := model.SearchQuery{Query: question, K: k, Index: indexName, PathPrefix: pathPrefix, FileGlob: fileGlob, DocTypes: docTypes, Languages: languages}
	if mode == "search_only" {
		return s.runSearchOnlyMode(ctx, question, sq)
	}
	askResult, askErr := s.retriever.Ask(ctx, question, sq)
	if askErr != nil {
		return toolCallResult{}, mapSearchError(askErr)
	}
	return toolCallResult{
		Content:           []toolContentItem{{Type: "text", Text: askResult.Answer}},
		StructuredContent: buildAskStructuredContent(askResult),
	}, nil
}

func (s *Server) handleAskAudioTool(ctx context.Context, args map[string]interface{}) (toolCallResult, *toolExecutionError) {
	if err := assertNoUnknownArguments(args, map[string]struct{}{
		"question": {}, "k": {}, "mode": {}, "index": {}, "path_prefix": {}, "file_glob": {}, "doc_types": {}, "voice_id": {},
	}); err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	question, ok, err := parseRequiredString(args, "question")
	if err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	if !ok {
		return toolCallResult{}, &toolExecutionError{Code: "MISSING_FIELD", Message: "question is required", Retryable: false}
	}
	k, toolErr := parseKArg(args)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}
	mode, toolErr := parseModeArg(args)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}
	indexName, toolErr := parseIndexArg(args)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}
	pathPrefix, fileGlob, docTypes, toolErr := parseSearchFilters(args)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}
	voiceID, err := parseOptionalString(args, "voice_id")
	if err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	if s.retriever == nil {
		return toolCallResult{}, &toolExecutionError{Code: protocol.ErrorCodeIndexNotReady, Message: "retriever not configured", Retryable: false}
	}
	sq := model.SearchQuery{Query: question, K: k, Index: indexName, PathPrefix: pathPrefix, FileGlob: fileGlob, DocTypes: docTypes}
	if mode == "search_only" {
		return s.runSearchOnlyMode(ctx, question, sq)
	}
	return s.runAskAudioAnswer(ctx, question, voiceID, sq)
}

func (s *Server) runAskAudioAnswer(ctx context.Context, question, voiceID string, sq model.SearchQuery) (toolCallResult, *toolExecutionError) {
	askResult, askErr := s.retriever.Ask(ctx, question, sq)
	if askErr != nil {
		if errors.Is(askErr, model.ErrNotImplemented) {
			fallback := map[string]interface{}{
				"question": question, "answer": "", "citations": []interface{}{}, "hits": []interface{}{}, "indexing_complete": false,
			}
			return toolCallResult{
				Content:           []toolContentItem{{Type: "text", Text: fmt.Sprintf("ask_audio is not available yet; use %s while ask generation is being implemented", protocol.ToolNameSearch)}},
				StructuredContent: fallback,
			}, nil
		}
		return toolCallResult{}, mapSearchError(askErr)
	}
	if strings.TrimSpace(askResult.Question) == "" {
		askResult.Question = question
	}
	structured := buildAskStructuredContent(askResult)
	answerText := strings.TrimSpace(askResult.Answer)
	if answerText == "" {
		answerText = "no answer text returned"
	}
	if s.tts == nil {
		text := answerText + "\n\nAudio synthesis is disabled. Set ELEVENLABS_API_KEY to enable " + protocol.ToolNameAskAudio + " voice output."
		return toolCallResult{Content: []toolContentItem{{Type: "text", Text: text}}, StructuredContent: structured}, nil
	}
	audioBytes, synthErr := s.synthesizeAnswer(ctx, voiceID, answerText)
	if synthErr != nil {
		return toolCallResult{
			Content:           []toolContentItem{{Type: "text", Text: answerText + "\n\nAudio synthesis failed, returning text-only response."}},
			StructuredContent: structured,
		}, nil
	}
	encodedAudio := base64.StdEncoding.EncodeToString(audioBytes)
	structured["audio"] = map[string]interface{}{"mime_type": "audio/mpeg", "data": encodedAudio}
	return toolCallResult{
		Content:           []toolContentItem{{Type: "text", Text: answerText}, {Type: "audio", MIMEType: "audio/mpeg", Data: encodedAudio}},
		StructuredContent: structured,
	}, nil
}

func (s *Server) handleTranscribeTool(ctx context.Context, args map[string]interface{}) (toolCallResult, *toolExecutionError) {
	if err := assertNoUnknownArguments(args, map[string]struct{}{
		"rel_path":     {},
		"language":     {},
		"timestamps":   {},
		"retranscribe": {},
	}); err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}

	relPath, ok, err := parseRequiredString(args, "rel_path")
	if err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	if !ok {
		return toolCallResult{}, &toolExecutionError{Code: "MISSING_FIELD", Message: "rel_path is required", Retryable: false}
	}
	language, err := parseOptionalString(args, "language")
	if err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	timestamps, err := parseOptionalBool(args, "timestamps", true)
	if err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	retranscribe, err := parseOptionalBool(args, "retranscribe", false)
	if err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}

	doc, toolErr := s.lookupOrInitAudioDocumentForTool(ctx, relPath)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}

	// if a language override is provided we always force a retranscription.
	// this ensures that callers can request a specific language, but note that
	// even if a cached transcript already exists the request will re-run the
	// transcription step. repeated calls with the same language value therefore
	// defeat caching and may incur extra API/cost overhead; clients should avoid
	// doing so when possible.
	if strings.TrimSpace(language) != "" {
		retranscribe = true
	}
	transcript, transcribedNow, indexed, toolErr := s.ensureTranscriptForAudioDoc(ctx, doc, retranscribe, language)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}
	transcribed := strings.TrimSpace(transcript) != ""

	segments := make([]map[string]interface{}, 0)
	if timestamps {
		for _, seg := range ingest.ChunkTranscriptByTime(transcript) {
			segments = append(segments, map[string]interface{}{
				"start_ms": seg.Span.StartMS,
				"end_ms":   seg.Span.EndMS,
				"text":     seg.Text,
			})
		}
	}

	structured := map[string]interface{}{
		"rel_path":        relPath,
		"provider":        defaultSTTProvider,
		"model":           defaultSTTModel,
		"indexed":         indexed,
		"segments":        segments,
		"transcribed":     transcribed,
		"transcribed_now": transcribedNow,
	}

	return toolCallResult{
		Content:           []toolContentItem{{Type: "text", Text: fmt.Sprintf("transcribed %s", relPath)}},
		StructuredContent: structured,
	}, nil
}

func parseAnnotateArgs(args map[string]interface{}) (relPath string, schemaJSON map[string]interface{}, indexFlattenedText bool, maxChars int, toolErr *toolExecutionError) {
	if err := assertNoUnknownArguments(args, map[string]struct{}{
		"rel_path":             {},
		"schema_json":          {},
		"index_flattened_text": {},
		"max_chars":            {},
	}); err != nil {
		return "", nil, false, 0, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	relPath, ok, err := parseRequiredString(args, "rel_path")
	if err != nil {
		return "", nil, false, 0, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	if !ok {
		return "", nil, false, 0, &toolExecutionError{Code: "MISSING_FIELD", Message: "rel_path is required", Retryable: false}
	}
	schemaJSON, ok, err = parseRequiredObject(args, "schema_json")
	if err != nil {
		return "", nil, false, 0, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	if !ok {
		return "", nil, false, 0, &toolExecutionError{Code: "MISSING_FIELD", Message: "schema_json is required", Retryable: false}
	}
	indexFlattenedText, err = parseOptionalBool(args, "index_flattened_text", true)
	if err != nil {
		return "", nil, false, 0, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	maxChars, toolErr = parseMaxCharsArg(args, 32000, 200, 200000)
	return relPath, schemaJSON, indexFlattenedText, maxChars, toolErr
}

func (s *Server) handleAnnotateTool(ctx context.Context, args map[string]interface{}) (toolCallResult, *toolExecutionError) {
	relPath, schemaJSON, indexFlattenedText, maxChars, toolErr := parseAnnotateArgs(args)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}

	doc, toolErr := s.lookupDocumentForTool(ctx, relPath)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}

	sourceText, sourceRep, toolErr := s.sourceTextForAnnotation(ctx, doc)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}
	if strings.TrimSpace(sourceText) == "" {
		return toolCallResult{}, &toolExecutionError{Code: "ANNOTATE_FAILED", Message: "no source text available for annotation", Retryable: false}
	}
	runes := []rune(sourceText)
	if len(runes) > maxChars {
		sourceText = string(runes[:maxChars])
	}

	client, toolErr := s.newGenerator()
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}

	schemaBytes, err := json.Marshal(schemaJSON)
	if err != nil {
		// schemaJSON should be a valid object but if marshaling fails we
		// can't safely include it in the prompt.  Fail early with a
		// descriptive error rather than sending malformed JSON to the
		// model.
		return toolCallResult{}, &toolExecutionError{
			Code:      "ANNOTATE_FAILED",
			Message:   fmt.Sprintf("failed to marshal schema JSON: %v", err),
			Retryable: false,
		}
	}
	prompt := strings.Join([]string{
		"Extract a JSON object that strictly conforms to this schema:",
		string(schemaBytes),
		"Return only valid JSON object, no markdown, no prose.",
		"Document content:",
		sourceText,
	}, "\n\n")

	// guard against overly large inputs that would blow past the model's
	// context window.  We compute the rune count of the prompt because the
	// provider limits are generally character‑based; using bytes could be
	// slightly off when multi‑byte UTF‑8 is involved, but the constant is
	// high enough that differences don't matter.  If the prompt is too long
	// we fail with a mapped tool error rather than invoking client.Generate
	// which would likely return a provider error.
	if len([]rune(prompt)) > maxMistralContextChars {
		// This is a local validation failure; return a toolExecutionError rather
		// than mapping it to a provider error so callers know it didn't involve
		// the external API.
		return toolCallResult{}, &toolExecutionError{
			Code:      "ANNOTATE_FAILED",
			Message:   fmt.Sprintf("prompt length %d exceeds max context %d", len([]rune(prompt)), maxMistralContextChars),
			Retryable: false,
		}
	}

	generated, genErr := client.Generate(ctx, prompt)
	if genErr != nil {
		return toolCallResult{}, s.mapToolErrorFromProvider("ANNOTATE_FAILED", genErr)
	}
	annotationObj, parseErr := parseJSONObjectFromModelOutput(generated)
	if parseErr != nil {
		return toolCallResult{}, &toolExecutionError{Code: "ANNOTATE_FAILED", Message: parseErr.Error(), Retryable: false}
	}

	// create a request-scoped ingest service to avoid cross-request mutation of
	// OCR/transcriber settings under concurrency.
	ing, err := ingest.NewService(s.cfg, s.store)
	if err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "CONFIG_INVALID", Message: err.Error(), Retryable: false}
	}
	preview, persistErr := ing.StoreAnnotationRepresentations(ctx, doc, annotationObj, indexFlattenedText)
	if persistErr != nil {
		return toolCallResult{}, &toolExecutionError{Code: "STORE_CORRUPT", Message: persistErr.Error(), Retryable: false}
	}

	structured := map[string]interface{}{
		"rel_path":                relPath,
		"stored":                  true,
		"flattened_indexed":       indexFlattenedText,
		"annotation_json":         annotationObj,
		"annotation_text_preview": preview,
		"source_doc_type":         doc.DocType,
		"source_rep":              sourceRep,
	}

	return toolCallResult{
		Content:           []toolContentItem{{Type: "text", Text: fmt.Sprintf("annotation stored for %s", relPath)}},
		StructuredContent: structured,
	}, nil
}

func (s *Server) handleTranscribeAndAskTool(ctx context.Context, args map[string]interface{}) (toolCallResult, *toolExecutionError) {
	if err := assertNoUnknownArguments(args, map[string]struct{}{
		"rel_path": {},
		"question": {},
		"k":        {},
	}); err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}

	relPath, ok, err := parseRequiredString(args, "rel_path")
	if err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	if !ok {
		return toolCallResult{}, &toolExecutionError{Code: "MISSING_FIELD", Message: "rel_path is required", Retryable: false}
	}
	question, ok, err := parseRequiredString(args, "question")
	if err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	if !ok {
		return toolCallResult{}, &toolExecutionError{Code: "MISSING_FIELD", Message: "question is required", Retryable: false}
	}
	k, toolErr := parseKArg(args)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}
	if s.retriever == nil {
		return toolCallResult{}, &toolExecutionError{Code: protocol.ErrorCodeIndexNotReady, Message: "retriever not configured", Retryable: false}
	}
	doc, toolErr := s.lookupOrInitAudioDocumentForTool(ctx, relPath)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}
	transcriptText, transcribedNow, _, toolErr := s.ensureTranscriptForAudioDoc(ctx, doc, false, "")
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}
	askResult, askErr := s.retriever.Ask(ctx, question, model.SearchQuery{
		Query: question, K: k, Index: "text", FileGlob: escapeGlobLiteral(relPath),
	})
	if askErr != nil {
		return toolCallResult{}, mapSearchError(askErr)
	}

	structured := buildAskStructuredContent(askResult)
	structured["transcript_provider"] = defaultSTTProvider
	structured["transcript_model"] = defaultSTTModel
	structured["transcribed"] = strings.TrimSpace(transcriptText) != ""
	structured["transcribed_now"] = transcribedNow

	answerText := strings.TrimSpace(askResult.Answer)
	if answerText == "" {
		answerText = "no answer text returned"
	}
	return toolCallResult{
		Content:           []toolContentItem{{Type: "text", Text: answerText}},
		StructuredContent: structured,
	}, nil
}

func (s *Server) handleOpenFileTool(ctx context.Context, args map[string]interface{}) (toolCallResult, *toolExecutionError) {
	if err := assertNoUnknownArguments(args, map[string]struct{}{
		"rel_path": {}, "start_line": {}, "end_line": {}, "page": {}, "start_ms": {}, "end_ms": {}, "max_chars": {},
	}); err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	relPath, ok, err := parseRequiredString(args, "rel_path")
	if err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	if !ok {
		return toolCallResult{}, &toolExecutionError{Code: "MISSING_FIELD", Message: "rel_path is required", Retryable: false}
	}
	maxChars := 20000
	if raw, ok := args["max_chars"]; ok {
		parsed, parseErr := parseInteger(raw, "max_chars")
		if parseErr != nil {
			return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: parseErr.Error(), Retryable: false}
		}
		maxChars = parsed
	}
	if maxChars < 200 || maxChars > 50000 {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: "max_chars must be between 200 and 50000", Retryable: false}
	}
	span, toolErr := parseOpenFileSpan(args)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}
	if s.retriever == nil {
		return toolCallResult{}, &toolExecutionError{Code: protocol.ErrorCodeIndexNotReady, Message: "retriever not configured", Retryable: false}
	}
	var (
		content   string
		truncated bool
		openErr   error
	)
	if withMeta, ok := s.retriever.(retrieverOpenFileWithMeta); ok {
		content, truncated, openErr = withMeta.OpenFileWithMeta(ctx, relPath, span, maxChars)
	} else {
		content, openErr = s.retriever.OpenFile(ctx, relPath, span, maxChars)
		// OpenFile already truncates to maxChars internally and does not
		// return a truncation flag, so we cannot know the pre-truncation
		// length here. Accurate truncation info requires OpenFileWithMeta.
		truncated = false
	}
	if openErr != nil {
		return toolCallResult{}, mapOpenFileError(openErr)
	}
	docType := inferDocType(relPath)
	structured := map[string]interface{}{
		"rel_path": relPath, "doc_type": docType, "content": content, "truncated": truncated,
	}
	if looksLikeBinaryContent(content) {
		return toolCallResult{}, &toolExecutionError{Code: "DOC_TYPE_UNSUPPORTED", Message: binaryContentMessageForDocType(docType), Retryable: false}
	}
	if strings.TrimSpace(span.Kind) != "" {
		structured["span"] = buildOpenFileSpan(span)
	} else if isBinaryDocTypeForOpenFile(docType) {
		// For PDFs/audio with no requested span, the retriever has returned the
		// cached OCR / transcript markdown. Surface this to the caller as
		// span.kind=document so they can distinguish a full-document
		// representation from a paged/timed slice.
		structured["span"] = map[string]interface{}{"kind": "document"}
	}
	return toolCallResult{
		Content:           []toolContentItem{{Type: "text", Text: content}},
		StructuredContent: structured,
	}, nil
}

// mediaClipRequest is the resolved target of an open_media_clip call: the
// source media rel_path/doc_type and the [startMS, endMS) span to extract.
type mediaClipRequest struct {
	relPath string
	docType string
	startMS int
	endMS   int
}

// handleOpenMediaClipTool implements dir2mcp_open_media_clip (SPEC §15.11): the
// time-media analogue of open_file. It resolves a chunk_id (or rel_path + range)
// to a source media file and time span, enforces the configured clip bounds,
// extracts the snippet via the injectable extractSegment seam, and returns the
// bytes inline (base64 + an audio/video content item). reference mode is not yet
// materialized, so it falls back to inline and notes it.
func (s *Server) handleOpenMediaClipTool(ctx context.Context, args map[string]interface{}) (toolCallResult, *toolExecutionError) {
	if err := assertNoUnknownArguments(args, map[string]struct{}{
		"chunk_id": {}, "rel_path": {}, "start_ms": {}, "end_ms": {}, "return": {},
	}); err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	returnMode, toolErr := parseMediaClipReturnArg(args)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}
	req, toolErr := s.resolveMediaClipRequest(ctx, args)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}
	if !isMediaDocType(req.docType) {
		return toolCallResult{}, &toolExecutionError{Code: "DOC_TYPE_UNSUPPORTED", Message: "open_media_clip requires an audio/video document; " + req.relPath + " is " + req.docType, Retryable: false}
	}
	if req.startMS >= req.endMS {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_RANGE", Message: "start_ms must be < end_ms", Retryable: false}
	}

	maxDurationMS := s.cfg.MediaClipMaxDurationMS
	if maxDurationMS <= 0 {
		maxDurationMS = config.DefaultMediaClipMaxDurationMS
	}
	maxBytes := s.cfg.MediaClipMaxBytes
	if maxBytes <= 0 {
		maxBytes = config.DefaultMediaClipMaxBytes
	}
	if req.endMS-req.startMS > maxDurationMS {
		return toolCallResult{}, &toolExecutionError{Code: "CLIP_TOO_LARGE", Message: fmt.Sprintf("requested clip duration %dms exceeds max %dms; request a shorter span", req.endMS-req.startMS, maxDurationMS), Retryable: false}
	}

	absPath, pathErr := s.resolveDocumentPath(req.relPath)
	if pathErr != nil {
		return toolCallResult{}, mapPathError(pathErr)
	}
	extract := s.extractSegment
	if extract == nil {
		extract = avutil.ExtractSegment
	}
	data, extractErr := extract(ctx, absPath, req.startMS, req.endMS)
	if extractErr != nil {
		// Bytes/URLs are never echoed; map every extraction failure (including a
		// missing ffmpeg) to the non-secret MEDIA_CLIP_FAILED contract. It is
		// non-retryable: re-issuing the same request will not change the outcome
		// (a missing ffmpeg needs operator action; a decode failure for these
		// bytes/span is deterministic) — distinct from OCR_NOT_READY.
		return toolCallResult{}, &toolExecutionError{Code: "MEDIA_CLIP_FAILED", Message: "media clip extraction failed", Retryable: false}
	}
	if len(data) > maxBytes {
		return toolCallResult{}, &toolExecutionError{Code: "CLIP_TOO_LARGE", Message: fmt.Sprintf("extracted clip is %d bytes, exceeds max %d; request a shorter span", len(data), maxBytes), Retryable: false}
	}

	mimeType := mediaClipMIMEForPath(req.relPath, req.docType)
	encoded := base64.StdEncoding.EncodeToString(data)
	structured := map[string]interface{}{
		"rel_path":    req.relPath,
		"doc_type":    req.docType,
		"span":        buildOpenFileSpan(model.Span{Kind: "time", StartMS: req.startMS, EndMS: req.endMS}),
		"mime_type":   mimeType,
		"duration_ms": req.endMS - req.startMS,
		"size_bytes":  len(data),
		"return":      "inline",
		"data":        encoded,
	}
	// reference was requested but is not yet supported: fall back to inline and
	// note it (never error solely because reference was asked for, per §15.11).
	if returnMode == "reference" {
		structured["reference_fallback"] = "reference return not supported; falling back to inline"
	}
	contentType := "audio"
	if strings.EqualFold(strings.TrimSpace(req.docType), "video") {
		contentType = "video"
	}
	return toolCallResult{
		// Media bytes travel only via the typed data item, never a text item.
		Content:           []toolContentItem{{Type: contentType, Data: encoded, MIMEType: mimeType}},
		StructuredContent: structured,
	}, nil
}

// parseMediaClipReturnArg parses the optional "return" argument (inline|reference).
func parseMediaClipReturnArg(args map[string]interface{}) (string, *toolExecutionError) {
	mode, err := parseOptionalString(args, "return")
	if err != nil {
		return "", &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "inline"
	}
	switch mode {
	case "inline", "reference":
		return mode, nil
	default:
		return "", &toolExecutionError{Code: "INVALID_FIELD", Message: "return must be one of inline,reference", Retryable: false}
	}
}

// resolveMediaClipRequest resolves the open_media_clip selection rules into a
// concrete media target + span. A chunk_id resolves to its source media and
// chunk span; an explicit start_ms/end_ms provided alongside chunk_id overrides
// the chunk span. Otherwise rel_path + start_ms + end_ms must all be provided.
func (s *Server) resolveMediaClipRequest(ctx context.Context, args map[string]interface{}) (mediaClipRequest, *toolExecutionError) {
	chunkID, hasChunkID, err := parseOptionalIntegerWithPresence(args, "chunk_id")
	if err != nil {
		return mediaClipRequest{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	relPath, err := parseOptionalString(args, "rel_path")
	if err != nil {
		return mediaClipRequest{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	relPath = strings.TrimSpace(relPath)
	startMS, hasStart, err := parseOptionalIntegerWithPresence(args, "start_ms")
	if err != nil {
		return mediaClipRequest{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	endMS, hasEnd, err := parseOptionalIntegerWithPresence(args, "end_ms")
	if err != nil {
		return mediaClipRequest{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	if (hasStart && startMS < 0) || (hasEnd && endMS < 0) {
		return mediaClipRequest{}, &toolExecutionError{Code: "INVALID_RANGE", Message: "start_ms/end_ms must be >= 0", Retryable: false}
	}
	if hasStart != hasEnd {
		return mediaClipRequest{}, &toolExecutionError{Code: "INVALID_FIELD", Message: "both start_ms and end_ms must be provided together", Retryable: false}
	}

	if hasChunkID {
		return s.resolveMediaClipByChunk(ctx, chunkID, hasStart, startMS, endMS)
	}

	// rel_path + range path.
	if relPath == "" {
		return mediaClipRequest{}, &toolExecutionError{Code: "MISSING_FIELD", Message: "provide chunk_id, or rel_path with start_ms and end_ms", Retryable: false}
	}
	if !hasStart {
		return mediaClipRequest{}, &toolExecutionError{Code: "MISSING_FIELD", Message: "rel_path requires start_ms and end_ms", Retryable: false}
	}
	doc, toolErr := s.lookupDocumentForTool(ctx, relPath)
	if toolErr != nil {
		return mediaClipRequest{}, toolErr
	}
	docType := strings.TrimSpace(doc.DocType)
	if docType == "" {
		docType = ingest.ClassifyDocType(doc.RelPath)
	}
	return mediaClipRequest{relPath: doc.RelPath, docType: docType, startMS: startMS, endMS: endMS}, nil
}

// resolveMediaClipByChunk resolves a chunk_id to its source media and span,
// applying an explicit start_ms/end_ms override (still bounded to that media).
func (s *Server) resolveMediaClipByChunk(ctx context.Context, chunkID int, hasRange bool, startMS, endMS int) (mediaClipRequest, *toolExecutionError) {
	if chunkID <= 0 {
		return mediaClipRequest{}, &toolExecutionError{Code: "INVALID_FIELD", Message: "chunk_id must be > 0", Retryable: false}
	}
	resolver, ok := s.store.(storeChunkMediaSpan)
	if !ok || s.store == nil {
		return mediaClipRequest{}, &toolExecutionError{Code: "DOC_TYPE_UNSUPPORTED", Message: "chunk_id resolution is not supported by this store; pass rel_path with start_ms and end_ms", Retryable: false}
	}
	relPath, docType, span, err := resolver.ChunkMediaSpanByID(ctx, int64(chunkID))
	if err != nil {
		if errors.Is(err, model.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return mediaClipRequest{}, &toolExecutionError{Code: protocol.ErrorCodeFileNotFound, Message: "chunk not found", Retryable: false}
		}
		return mediaClipRequest{}, &toolExecutionError{Code: "STORE_CORRUPT", Message: err.Error(), Retryable: false}
	}
	req := mediaClipRequest{
		relPath: strings.TrimSpace(relPath),
		docType: strings.TrimSpace(docType),
		startMS: span.StartMS,
		endMS:   span.EndMS,
	}
	if req.docType == "" {
		req.docType = ingest.ClassifyDocType(req.relPath)
	}
	// An explicit range overrides the chunk's span (still bounded to this media).
	if hasRange {
		req.startMS = startMS
		req.endMS = endMS
	} else if strings.TrimSpace(span.Kind) != "time" {
		// Non-time chunk with no explicit range: there is no span to clip.
		return mediaClipRequest{}, &toolExecutionError{Code: "INVALID_RANGE", Message: "chunk has no time span; provide start_ms and end_ms", Retryable: false}
	}
	return req, nil
}

// isMediaDocType reports whether docType is a clip-able audio/video type.
func isMediaDocType(docType string) bool {
	switch strings.ToLower(strings.TrimSpace(docType)) {
	case "audio", "video":
		return true
	default:
		return false
	}
}

// mediaClipMIMEForPath picks a container MIME type for an extracted clip from
// the source file extension (stream-copy keeps the source muxer), falling back
// to a generic audio/video type by doc_type when the extension is unknown.
func mediaClipMIMEForPath(relPath, docType string) string {
	switch strings.ToLower(filepath.Ext(strings.TrimSpace(relPath))) {
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".m4a", ".aac":
		return "audio/mp4"
	case ".flac":
		return "audio/flac"
	case ".ogg", ".opus":
		return "audio/ogg"
	case ".mp4":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	}
	if strings.EqualFold(strings.TrimSpace(docType), "video") {
		return "video/mp4"
	}
	return "audio/mpeg"
}

// isBinaryDocTypeForOpenFile reports whether docType is one whose default
// open_file response is served from an OCR / transcript cache rather than raw
// file bytes (see issue #177). Keeping this private to mcp avoids leaking the
// retriever-side notion of "binary doc type" into the public API surface.
func isBinaryDocTypeForOpenFile(docType string) bool {
	switch strings.ToLower(strings.TrimSpace(docType)) {
	case "pdf", "audio":
		return true
	default:
		return false
	}
}

// binaryContentMessageForDocType picks a DOC_TYPE_UNSUPPORTED message tailored
// to the inferred doc_type. The historical message hardcoded "use transcribe
// for audio files", which was misleading when the same path was reached for a
// PDF (or anything else) whose payload tripped the binary-content guard.
func binaryContentMessageForDocType(docType string) string {
	switch strings.ToLower(strings.TrimSpace(docType)) {
	case "audio":
		return "open_file does not support binary content; use transcribe for audio files"
	case "pdf":
		return "open_file does not support binary content; pass page=N to read a specific page once OCR has run"
	default:
		return "open_file does not support binary content; the resolved payload is not text"
	}
}

// parseMaxCharsArg parses the "max_chars" argument with a default and range.
func parseMaxCharsArg(args map[string]interface{}, defaultVal, minVal, maxVal int) (int, *toolExecutionError) {
	result := defaultVal
	if raw, ok := args["max_chars"]; ok {
		parsed, parseErr := parseInteger(raw, "max_chars")
		if parseErr != nil {
			return 0, &toolExecutionError{Code: "INVALID_FIELD", Message: parseErr.Error(), Retryable: false}
		}
		result = parsed
	}
	if result < minVal || result > maxVal {
		return 0, &toolExecutionError{Code: "INVALID_FIELD", Message: fmt.Sprintf("max_chars must be between %d and %d", minVal, maxVal), Retryable: false}
	}
	return result, nil
}

// parseListFilesArgs parses all arguments for handleListFilesTool.
func parseListFilesArgs(args map[string]interface{}) (pathPrefix, glob string, limit, offset int, includeHidden bool, toolErr *toolExecutionError) {
	var err error
	pathPrefix, err = parseOptionalString(args, "path_prefix")
	if err != nil {
		return "", "", 0, 0, false, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	glob, err = parseOptionalString(args, "glob")
	if err != nil {
		return "", "", 0, 0, false, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	limit = 200
	if rawLimit, ok := args["limit"]; ok {
		parsedLimit, parseErr := parseInteger(rawLimit, "limit")
		if parseErr != nil {
			return "", "", 0, 0, false, &toolExecutionError{Code: "INVALID_FIELD", Message: parseErr.Error(), Retryable: false}
		}
		limit = parsedLimit
	}
	if limit < 1 || limit > 5000 {
		return "", "", 0, 0, false, &toolExecutionError{Code: "INVALID_RANGE", Message: "limit must be between 1 and 5000", Retryable: false}
	}
	if rawOffset, ok := args["offset"]; ok {
		parsedOffset, parseErr := parseInteger(rawOffset, "offset")
		if parseErr != nil {
			return "", "", 0, 0, false, &toolExecutionError{Code: "INVALID_FIELD", Message: parseErr.Error(), Retryable: false}
		}
		offset = parsedOffset
	}
	if offset < 0 {
		return "", "", 0, 0, false, &toolExecutionError{Code: "INVALID_RANGE", Message: "offset must be >= 0", Retryable: false}
	}
	includeHidden, err = parseOptionalBool(args, "include_hidden", false)
	if err != nil {
		return "", "", 0, 0, false, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	return pathPrefix, glob, limit, offset, includeHidden, nil
}

// parseKArg parses the "k" argument with default and range validation.
func parseKArg(args map[string]interface{}) (int, *toolExecutionError) {
	k := DefaultSearchK
	if rawK, exists := args["k"]; exists {
		parsedK, parseErr := parseInteger(rawK, "k")
		if parseErr != nil {
			return 0, &toolExecutionError{Code: "INVALID_FIELD", Message: parseErr.Error(), Retryable: false}
		}
		if parsedK > 0 {
			k = parsedK
		}
	}
	if k > 50 {
		return 0, &toolExecutionError{Code: "INVALID_RANGE", Message: "k must be between 1 and 50", Retryable: false}
	}
	return k, nil
}

// parseModeArg parses the "mode" argument (answer|search_only) with normalization.
func parseModeArg(args map[string]interface{}) (string, *toolExecutionError) {
	mode, err := parseOptionalString(args, "mode")
	if err != nil {
		return "", &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "answer"
	}
	switch mode {
	case "answer", "search_only":
	default:
		return "", &toolExecutionError{Code: "INVALID_FIELD", Message: "mode must be one of answer,search_only", Retryable: false}
	}
	return mode, nil
}

// parseIndexArg parses the "index" argument (auto|text|code|both) with normalization.
func parseIndexArg(args map[string]interface{}) (string, *toolExecutionError) {
	indexName, err := parseOptionalString(args, "index")
	if err != nil {
		return "", &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	indexName = strings.ToLower(strings.TrimSpace(indexName))
	if indexName == "" {
		indexName = "auto"
	}
	switch indexName {
	case "auto", "text", "code", "both":
	default:
		return "", &toolExecutionError{Code: "INVALID_FIELD", Message: "index must be one of auto,text,code,both", Retryable: false}
	}
	return indexName, nil
}

// parseSearchFilters parses path_prefix, file_glob, and doc_types arguments.
func parseSearchFilters(args map[string]interface{}) (pathPrefix, fileGlob string, docTypes []string, toolErr *toolExecutionError) {
	var err error
	pathPrefix, err = parseOptionalString(args, "path_prefix")
	if err != nil {
		return "", "", nil, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	fileGlob, err = parseOptionalString(args, "file_glob")
	if err != nil {
		return "", "", nil, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	docTypes, err = parseOptionalStringSlice(args, "doc_types")
	if err != nil {
		return "", "", nil, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	return pathPrefix, fileGlob, docTypes, nil
}

// parseLanguagesArg parses the optional per-language retrieval filter (SPEC
// §9.5): an array of BCP-47 language tags. Absent or empty ⇒ nil (no filtering,
// behaviour unchanged). Each entry must be a syntactically valid BCP-47 tag
// (model.IsValidLanguageTag); a malformed tag is INVALID_FIELD (§9.5/§14). A
// syntactically valid tag that simply matches nothing in the corpus is NOT an
// error — that is handled downstream by returning an empty hit list. The parsed
// tags are returned verbatim (trimmed); the retrieval filter normalizes to the
// primary subtag for case-insensitive matching.
func parseLanguagesArg(args map[string]interface{}) ([]string, *toolExecutionError) {
	languages, err := parseOptionalStringSlice(args, "languages")
	if err != nil {
		return nil, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	if len(languages) == 0 {
		return nil, nil
	}
	for _, tag := range languages {
		if !model.IsValidLanguageTag(tag) {
			return nil, &toolExecutionError{
				Code:      "INVALID_FIELD",
				Message:   fmt.Sprintf("languages contains an invalid BCP-47 language tag %q", tag),
				Retryable: false,
			}
		}
	}
	return languages, nil
}

// mapSearchError converts a search/ask error into a toolExecutionError.
func mapSearchError(err error) *toolExecutionError {
	if errors.Is(err, model.ErrIndexNotReady) || errors.Is(err, model.ErrIndexNotConfigured) {
		return &toolExecutionError{Code: protocol.ErrorCodeIndexNotReady, Message: "index not ready", Retryable: true}
	}
	return &toolExecutionError{Code: "INTERNAL_ERROR", Message: "internal server error", Retryable: true}
}

// mapOpenFileError converts an open-file error into a toolExecutionError.
func mapOpenFileError(err error) *toolExecutionError {
	switch {
	case errors.Is(err, model.ErrForbidden):
		return &toolExecutionError{Code: protocol.ErrorCodePermissionDenied, Message: "forbidden", Retryable: false}
	case errors.Is(err, model.ErrPathOutsideRoot):
		return &toolExecutionError{Code: "PATH_OUTSIDE_ROOT", Message: "path outside root", Retryable: false}
	case errors.Is(err, model.ErrDocTypeUnsupported):
		return &toolExecutionError{Code: "DOC_TYPE_UNSUPPORTED", Message: "doc type unsupported", Retryable: false}
	case errors.Is(err, model.ErrMediaNoText):
		return &toolExecutionError{Code: "MEDIA_NO_TEXT", Message: "media embedded directly with no text representation (multimodal replace mode); no text is available to open", Retryable: false}
	case errors.Is(err, model.ErrOCRNotReady):
		return &toolExecutionError{Code: "OCR_NOT_READY", Message: "ocr representation not yet available; retry once indexing completes or request a specific page/start_ms", Retryable: true}
	case errors.Is(err, os.ErrNotExist):
		return &toolExecutionError{Code: protocol.ErrorCodeFileNotFound, Message: "file not found", Retryable: false}
	default:
		return &toolExecutionError{Code: "INTERNAL_ERROR", Message: "internal server error", Retryable: true}
	}
}

// runSearchOnlyMode performs a search-only retrieval and formats the result.
func (s *Server) runSearchOnlyMode(ctx context.Context, question string, sq model.SearchQuery) (toolCallResult, *toolExecutionError) {
	hits, searchErr := s.retriever.Search(ctx, sq)
	if searchErr != nil {
		return toolCallResult{}, mapSearchError(searchErr)
	}
	hitMaps := make([]map[string]interface{}, 0, len(hits))
	for _, h := range hits {
		hitMaps = append(hitMaps, serializeHit(h))
	}
	indexingComplete := true
	if ic, err := s.retriever.IndexingComplete(ctx); err == nil {
		indexingComplete = ic
	}
	structured := map[string]interface{}{
		"question":          question,
		"answer":            "",
		"citations":         []interface{}{},
		"hits":              hitMaps,
		"indexing_complete": indexingComplete,
	}
	return toolCallResult{
		Content:           []toolContentItem{{Type: "text", Text: fmt.Sprintf("found %d supporting result(s)", len(hits))}},
		StructuredContent: structured,
	}, nil
}

// synthesizeAnswer runs TTS synthesis, selecting the voice-aware path when voiceID is non-empty.
func (s *Server) synthesizeAnswer(ctx context.Context, voiceID, answerText string) ([]byte, error) {
	if voiceID != "" {
		if va, ok := s.tts.(voiceAwareTTSSynthesizer); ok {
			return va.SynthesizeWithVoice(ctx, answerText, voiceID)
		}
	}
	return s.tts.Synthesize(ctx, answerText)
}

func parsePageArg(args map[string]interface{}) (page int, hasPage bool, toolErr *toolExecutionError) {
	raw, ok := args["page"]
	if !ok {
		return 0, false, nil
	}
	p, parseErr := parseInteger(raw, "page")
	if parseErr != nil {
		return 0, false, &toolExecutionError{Code: "INVALID_FIELD", Message: parseErr.Error(), Retryable: false}
	}
	if p <= 0 {
		return 0, false, &toolExecutionError{Code: "INVALID_FIELD", Message: "page must be > 0", Retryable: false}
	}
	return p, true, nil
}

// parseOpenFileSpan parses span-related arguments and builds a model.Span.
func parseOpenFileSpan(args map[string]interface{}) (model.Span, *toolExecutionError) {
	page, hasPage, toolErr := parsePageArg(args)
	if toolErr != nil {
		return model.Span{}, toolErr
	}
	startMS, hasStartMS, err := parseOptionalIntegerWithPresence(args, "start_ms")
	if err != nil {
		return model.Span{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	endMS, hasEndMS, err := parseOptionalIntegerWithPresence(args, "end_ms")
	if err != nil {
		return model.Span{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	startLine, hasStartLine, err := parseOptionalIntegerWithPresence(args, "start_line")
	if err != nil {
		return model.Span{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	endLine, hasEndLine, err := parseOptionalIntegerWithPresence(args, "end_line")
	if err != nil {
		return model.Span{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	hasTimeSpan := hasStartMS || hasEndMS
	hasLineSpan := hasStartLine || hasEndLine
	groups := 0
	if hasPage {
		groups++
	}
	if hasTimeSpan {
		groups++
	}
	if hasLineSpan {
		groups++
	}
	if groups > 1 {
		return model.Span{}, &toolExecutionError{Code: "INVALID_FIELD", Message: "conflicting span parameters: provide only one of page, start_ms/end_ms, or start_line/end_line", Retryable: false}
	}
	if hasPage {
		return model.Span{Kind: "page", Page: page}, nil
	}
	if hasTimeSpan {
		return buildTimeSpan(startMS, hasStartMS, endMS, hasEndMS)
	}
	if hasLineSpan {
		return buildLineSpan(startLine, hasStartLine, endLine, hasEndLine)
	}
	return model.Span{}, nil
}

func buildTimeSpan(startMS int, hasStart bool, endMS int, hasEnd bool) (model.Span, *toolExecutionError) {
	if hasStart != hasEnd {
		return model.Span{}, &toolExecutionError{Code: "INVALID_FIELD", Message: "both start_ms and end_ms must be provided", Retryable: false}
	}
	if (hasStart && startMS < 0) || (hasEnd && endMS < 0) {
		return model.Span{}, &toolExecutionError{Code: "INVALID_FIELD", Message: "start_ms/end_ms must be >= 0", Retryable: false}
	}
	if hasStart && hasEnd && startMS > endMS {
		return model.Span{}, &toolExecutionError{Code: "INVALID_FIELD", Message: "start_ms must be <= end_ms", Retryable: false}
	}
	return model.Span{Kind: "time", StartMS: startMS, EndMS: endMS}, nil
}

func buildLineSpan(startLine int, hasStart bool, endLine int, hasEnd bool) (model.Span, *toolExecutionError) {
	if hasStart != hasEnd {
		return model.Span{}, &toolExecutionError{Code: "INVALID_FIELD", Message: "both start_line and end_line must be provided", Retryable: false}
	}
	if (hasStart && startLine <= 0) || (hasEnd && endLine <= 0) {
		return model.Span{}, &toolExecutionError{Code: "INVALID_FIELD", Message: "start_line/end_line must be > 0", Retryable: false}
	}
	if hasStart && hasEnd && startLine > endLine {
		return model.Span{}, &toolExecutionError{Code: "INVALID_FIELD", Message: "start_line must be <= end_line", Retryable: false}
	}
	return model.Span{Kind: "lines", StartLine: startLine, EndLine: endLine}, nil
}

func assertNoUnknownArguments(args map[string]interface{}, allowed map[string]struct{}) error {
	for key := range args {
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("unknown argument: %s", key)
		}
	}
	return nil
}

func parseOptionalBool(args map[string]interface{}, key string, defaultValue bool) (bool, error) {
	raw, ok := args[key]
	if !ok {
		return defaultValue, nil
	}
	v, ok := raw.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return v, nil
}

func parseRequiredObject(args map[string]interface{}, key string) (map[string]interface{}, bool, error) {
	raw, ok := args[key]
	if !ok {
		return nil, false, nil
	}
	obj, ok := raw.(map[string]interface{})
	if !ok {
		return nil, true, fmt.Errorf("%s must be an object", key)
	}
	return obj, true, nil
}

func (s *Server) lookupDocumentForTool(ctx context.Context, relPath string) (model.Document, *toolExecutionError) {
	relPath = strings.TrimSpace(relPath)
	if relPath == "" {
		return model.Document{}, &toolExecutionError{Code: "MISSING_FIELD", Message: "rel_path is required", Retryable: false}
	}
	if s.store == nil {
		return model.Document{}, &toolExecutionError{Code: "STORE_CORRUPT", Message: "store not configured", Retryable: false}
	}
	doc, err := s.store.GetDocumentByPath(ctx, relPath)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist), errors.Is(err, model.ErrNotFound):
			return model.Document{}, &toolExecutionError{Code: protocol.ErrorCodeFileNotFound, Message: "file not found", Retryable: false}
		default:
			return model.Document{}, &toolExecutionError{Code: "STORE_CORRUPT", Message: err.Error(), Retryable: false}
		}
	}
	if doc.Deleted {
		return model.Document{}, &toolExecutionError{Code: protocol.ErrorCodeFileNotFound, Message: "file not found", Retryable: false}
	}
	return doc, nil
}

// lookupOrInitAudioDocumentForTool resolves an audio document for transcription
// tools. If the document hasn't been indexed yet but a valid audio file exists
// under root, it creates the document row on demand so transcription can run
// before background indexing reaches that path.
func (s *Server) lookupOrInitAudioDocumentForTool(ctx context.Context, relPath string) (model.Document, *toolExecutionError) {
	doc, toolErr := s.lookupDocumentForTool(ctx, relPath)
	if toolErr == nil {
		if doc.DocType != "audio" {
			return model.Document{}, &toolExecutionError{Code: "DOC_TYPE_UNSUPPORTED", Message: "document is not audio", Retryable: false}
		}
		return doc, nil
	}
	if toolErr.Code != protocol.ErrorCodeFileNotFound {
		return model.Document{}, toolErr
	}
	return s.initAudioDocumentOnDemand(ctx, strings.TrimSpace(relPath))
}

func (s *Server) initAudioDocumentOnDemand(ctx context.Context, normalizedRel string) (model.Document, *toolExecutionError) {
	if ingest.ClassifyDocType(normalizedRel) != "audio" {
		return model.Document{}, &toolExecutionError{Code: "DOC_TYPE_UNSUPPORTED", Message: "document is not audio", Retryable: false}
	}
	if s.store == nil {
		return model.Document{}, &toolExecutionError{Code: "STORE_CORRUPT", Message: "store not configured", Retryable: false}
	}
	absPath, pathErr := s.resolveDocumentPath(normalizedRel)
	if pathErr != nil {
		return model.Document{}, mapPathError(pathErr)
	}
	info, statErr := os.Stat(absPath)
	if statErr != nil {
		return model.Document{}, mapFileAccessError(statErr)
	}
	content, readErr := os.ReadFile(absPath)
	if readErr != nil {
		return model.Document{}, mapFileAccessError(readErr)
	}
	upsertDoc := model.Document{
		RelPath: normalizedRel, DocType: "audio", SourceType: "filesystem",
		SizeBytes: info.Size(), MTimeUnix: info.ModTime().Unix(),
		ContentHash: ingest.ComputeContentHash(content), Status: "ok", Deleted: false,
	}
	if err := s.store.UpsertDocument(ctx, upsertDoc); err != nil {
		return model.Document{}, &toolExecutionError{Code: "STORE_CORRUPT", Message: err.Error(), Retryable: false}
	}
	doc, toolErr := s.lookupDocumentForTool(ctx, normalizedRel)
	if toolErr != nil {
		return model.Document{}, toolErr
	}
	if doc.DocType != "audio" {
		return model.Document{}, &toolExecutionError{Code: "DOC_TYPE_UNSUPPORTED", Message: "document is not audio", Retryable: false}
	}
	return doc, nil
}

func mapPathError(err error) *toolExecutionError {
	switch {
	case errors.Is(err, model.ErrPathOutsideRoot):
		return &toolExecutionError{Code: "PATH_OUTSIDE_ROOT", Message: "path outside root", Retryable: false}
	case errors.Is(err, os.ErrNotExist):
		return &toolExecutionError{Code: protocol.ErrorCodeFileNotFound, Message: "file not found", Retryable: false}
	default:
		return &toolExecutionError{Code: protocol.ErrorCodePermissionDenied, Message: "permission denied", Retryable: false}
	}
}

func mapFileAccessError(err error) *toolExecutionError {
	if errors.Is(err, os.ErrNotExist) {
		return &toolExecutionError{Code: protocol.ErrorCodeFileNotFound, Message: "file not found", Retryable: false}
	}
	return &toolExecutionError{Code: protocol.ErrorCodePermissionDenied, Message: "permission denied", Retryable: false}
}

func mapReadDocumentError(err error) *toolExecutionError {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return &toolExecutionError{Code: protocol.ErrorCodeFileNotFound, Message: "file not found", Retryable: false}
	case errors.Is(err, model.ErrPathOutsideRoot):
		return &toolExecutionError{Code: "PATH_OUTSIDE_ROOT", Message: "path outside root", Retryable: false}
	default:
		return &toolExecutionError{Code: protocol.ErrorCodePermissionDenied, Message: "permission denied", Retryable: false}
	}
}

func (s *Server) ensureTranscriptForAudioDoc(ctx context.Context, doc model.Document, retranscribe bool, language string) (string, bool, bool, *toolExecutionError) {
	content, err := s.readDocumentContent(doc.RelPath)
	if err != nil {
		return "", false, false, mapReadDocumentError(err)
	}
	cachePath := filepath.Join(s.cfg.StateDir, "cache", "transcribe", ingest.ComputeContentHash(content)+ingest.TranscriptLangSuffix(language)+".txt")
	// Determine whether we already have a usable cache file. We initially
	// consider the cache "valid" if it exists on disk. When a retranscribe is
	// requested we treat the cached file as stale regardless of whether it
	// existed, so we force cacheValid=false and remove the file.
	cacheValid := true
	if _, statErr := os.Stat(cachePath); statErr != nil {
		cacheValid = false
	}
	if retranscribe {
		cacheValid = false
		// remove any existing cache so that future callers don't accidentally
		// read stale data; ignore not-exist errors since that's fine.
		if rmErr := os.Remove(cachePath); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			return "", false, false, &toolExecutionError{Code: "STORE_CORRUPT", Message: fmt.Sprintf("remove transcript cache: %v", rmErr), Retryable: false}
		}
	}

	// STT resolves through the provider model (SPEC 8.1.3); the legacy
	// stt.provider selector still maps onto a profile during the
	// transition. The per-request language override is threaded onto the
	// resolved profile so it reaches the wire.
	transcriber, tErr := ingest.TranscriberFromConfigWithLanguage(s.cfg, language)
	if tErr != nil {
		return "", false, false, &toolExecutionError{Code: "CONFIG_INVALID", Message: tErr.Error(), Retryable: false}
	}
	if transcriber == nil {
		return "", false, false, &toolExecutionError{Code: "CONFIG_INVALID", Message: "no speech-to-text provider configured", Retryable: false}
	}
	ing, err := ingest.NewService(s.cfg, s.store)
	if err != nil {
		return "", false, false, &toolExecutionError{Code: "CONFIG_INVALID", Message: err.Error(), Retryable: false}
	}
	ing.SetTranscriber(transcriber)

	// generate transcript text first so we can accurately determine whether
	// there is anything worth indexing.
	transcript, readErr := ing.ReadOrComputeTranscript(ctx, doc, content, language)
	if readErr != nil {
		return "", false, false, s.mapToolErrorFromProvider("TRANSCRIBE_FAILED", readErr)
	}

	indexed := false
	if strings.TrimSpace(transcript) != "" {
		// only attempt to persist a representation when we actually have text
		if genErr := ing.GenerateTranscriptRepresentation(ctx, doc, content); genErr != nil {
			return "", false, false, s.mapToolErrorFromProvider("TRANSCRIBE_FAILED", genErr)
		}
		indexed = true
	}
	return transcript, !cacheValid, indexed, nil
}

func (s *Server) sourceTextForAnnotation(ctx context.Context, doc model.Document) (string, string, *toolExecutionError) {
	switch doc.DocType {
	case "audio":
		text, _, _, toolErr := s.ensureTranscriptForAudioDoc(ctx, doc, false, "")
		if toolErr != nil {
			return "", "", toolErr
		}
		return text, ingest.RepTypeTranscript, nil
	case "pdf", "image", "document":
		content, err := s.readDocumentContent(doc.RelPath)
		if err != nil {
			switch {
			case errors.Is(err, os.ErrNotExist):
				return "", "", &toolExecutionError{Code: protocol.ErrorCodeFileNotFound, Message: "file not found", Retryable: false}
			case errors.Is(err, model.ErrPathOutsideRoot):
				return "", "", &toolExecutionError{Code: "PATH_OUTSIDE_ROOT", Message: err.Error(), Retryable: false}
			default:
				return "", "", &toolExecutionError{Code: protocol.ErrorCodePermissionDenied, Message: err.Error(), Retryable: false}
			}
		}
		extractor := ingest.DocumentExtractorFromConfigContext(ctx, s.cfg)
		if extractor == nil {
			return "", "", &toolExecutionError{
				Code:      "CONFIG_INVALID",
				Message:   "document extraction is not configured (install docling, set DIR2MCP_DOCLING_COMMAND, or set MISTRAL_API_KEY)",
				Retryable: false,
			}
		}
		ing, err := ingest.NewService(s.cfg, s.store)
		if err != nil {
			return "", "", &toolExecutionError{Code: "CONFIG_INVALID", Message: err.Error(), Retryable: false}
		}
		ing.SetDocumentExtractor(extractor)
		text, ocrErr := ing.ReadOrComputeOCR(ctx, doc, content)
		if ocrErr != nil {
			return "", "", s.mapToolErrorFromProvider("ANNOTATE_FAILED", ocrErr)
		}
		return text, ingest.RepTypeOCRMarkdown, nil
	default:
		content, err := s.readDocumentContent(doc.RelPath)
		if err != nil {
			switch {
			case errors.Is(err, os.ErrNotExist):
				return "", "", &toolExecutionError{Code: protocol.ErrorCodeFileNotFound, Message: "file not found", Retryable: false}
			case errors.Is(err, model.ErrPathOutsideRoot):
				return "", "", &toolExecutionError{Code: "PATH_OUTSIDE_ROOT", Message: err.Error(), Retryable: false}
			default:
				return "", "", &toolExecutionError{Code: protocol.ErrorCodePermissionDenied, Message: err.Error(), Retryable: false}
			}
		}
		return string(ingest.NormalizeUTF8(content)), ingest.RepTypeRawText, nil
	}
}

func (s *Server) readDocumentContent(relPath string) ([]byte, error) {
	targetReal, err := s.resolveDocumentPath(relPath)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(targetReal)
}

func (s *Server) resolveDocumentPath(relPath string) (string, error) {
	rootAbs, err := filepath.Abs(strings.TrimSpace(s.cfg.RootDir))
	if err != nil {
		return "", err
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", err
	}
	normalized := filepath.ToSlash(filepath.Clean(strings.TrimSpace(relPath)))
	if normalized == "." || normalized == ".." || strings.HasPrefix(normalized, "../") || filepath.IsAbs(relPath) {
		return "", model.ErrPathOutsideRoot
	}
	absPath := filepath.Join(rootAbs, filepath.FromSlash(normalized))
	targetReal, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rootReal, targetReal)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", model.ErrPathOutsideRoot
	}
	return targetReal, nil
}
func escapeGlobLiteral(input string) string {
	var b strings.Builder
	for _, r := range input {
		switch r {
		case '\\', '*', '?', '[', ']':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// newGenerator resolves the chat provider through the provider model
// (SPEC 8.1.3) and builds its adapter. It replaces the legacy
// Mistral-only annotate client: Mistral chat now routes through the
// OpenAI-compatible backbone like any other provider. The chat model
// comes from the resolved profile (providers:/model: config).
func (s *Server) newGenerator() (model.Generator, *toolExecutionError) {
	cp, err := s.cfg.Providers().Resolve(provider.CapChat)
	if err != nil {
		var ce *provider.ConfigError
		if errors.As(err, &ce) {
			return nil, &toolExecutionError{Code: "CONFIG_INVALID", Message: ce.Error(), Retryable: false}
		}
		return nil, &toolExecutionError{Code: "CONFIG_INVALID", Message: "no chat provider configured", Retryable: false}
	}
	gen, ferr := providerfactory.Generator(cp)
	if ferr != nil {
		return nil, &toolExecutionError{Code: "CONFIG_INVALID", Message: ferr.Error(), Retryable: false}
	}
	return gen, nil
}

func parseJSONObjectFromModelOutput(raw string) (map[string]interface{}, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("model returned empty output")
	}

	// We'll try a few candidate substrings in case the model output contains
	// extra prose (e.g. "Here's your JSON: {...}" or triple-backtick code
	// fences). The later generic search covers both ```json and non-markdown
	// wrappers by locating the first '{' and last '}' in the trimmed text.
	candidates := []string{trimmed}
	if start := strings.Index(trimmed, "{"); start >= 0 {
		if end := strings.LastIndex(trimmed, "}"); end > start {
			candidates = append(candidates, trimmed[start:end+1])
		}
	}

	for _, candidate := range candidates {
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(candidate), &obj); err == nil && obj != nil {
			return obj, nil
		}
	}
	return nil, errors.New("model output is not a valid JSON object")
}

// mapToolErrorFromProvider maps errors returned by downstream
// providers into sanitized toolExecutionError values.  Previously this
// helper relied on the global log package for diagnostics; we now emit
// structured events via the server's eventEmitter so callers can capture
// them in the NDJSON stream.
func (s *Server) mapToolErrorFromProvider(defaultCode string, err error) *toolExecutionError {
	if err == nil {
		return nil
	}
	var providerErr *model.ProviderError
	if errors.As(err, &providerErr) {
		msg := strings.TrimSpace(providerErr.Message)
		if msg == "" {
			msg = providerErr.Error()
		}
		return &toolExecutionError{
			Code:      defaultCode,
			Message:   msg,
			Retryable: providerErr.Retryable,
		}
	}
	if errors.Is(err, ingest.ErrTranscriptProviderFailure) {
		// provider failure is retriable but we avoid returning raw details
		if s.eventEmitter != nil {
			s.eventEmitter("error", "transcript_provider_failure", map[string]interface{}{
				"error": err.Error(),
				"code":  defaultCode,
				"msg":   "transcript provider failure",
			})
		}
		return &toolExecutionError{
			Code:      defaultCode,
			Message:   "transcript provider failure",
			Retryable: true,
		}
	}
	// generic fallback: emit structured event and return sanitized message
	if s.eventEmitter != nil {
		s.eventEmitter("error", "tool_error", map[string]interface{}{
			"error": err.Error(),
			"code":  defaultCode,
			"msg":   "internal server error",
		})
	}
	return &toolExecutionError{
		Code:      defaultCode,
		Message:   "internal server error",
		Retryable: false,
	}
}

func parseRequiredString(args map[string]interface{}, key string) (string, bool, error) {
	raw, ok := args[key]
	if !ok {
		return "", false, nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", true, fmt.Errorf("%s must be a string", key)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", true, fmt.Errorf("%s must be a non-empty string", key)
	}
	return value, true, nil
}

func parseOptionalString(args map[string]interface{}, key string) (string, error) {
	raw, ok := args[key]
	if !ok {
		return "", nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return strings.TrimSpace(value), nil
}

func inferDocType(relPath string) string {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(relPath)))
	switch ext {
	case ".go", ".js", ".jsx", ".ts", ".tsx",
		".py", ".java", ".rb", ".cpp", ".c", ".cs",
		".kt", ".kts", ".swift", ".php", ".scala", ".rs",
		".h", ".hpp", ".hh", ".m", ".mm", ".dart",
		".pl", ".pm", ".lua", ".r", ".jl", ".hs",
		".erl", ".ex", ".exs", ".sql", ".sh", ".zsh",
		".fish":
		return "code"
	case ".html", ".htm", ".css":
		return "html"
	case ".md":
		return "md"
	case ".txt", ".rst":
		return "text"
	case ".pdf":
		return "pdf"
	case ".mp3", ".wav", ".m4a", ".flac":
		return "audio"
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return "image"
	default:
		return "unknown"
	}
}

// BuildOpenFileSpan exposes buildOpenFileSpan for tests in the tests/ tree
// (the repo keeps all test files there, per AGENTS.md). It is a thin wrapper
// with no behavior of its own.
func BuildOpenFileSpan(span model.Span) map[string]interface{} {
	return buildOpenFileSpan(span)
}

func buildOpenFileSpan(span model.Span) map[string]interface{} {
	kind := strings.TrimSpace(span.Kind)
	switch kind {
	case "lines":
		return map[string]interface{}{
			"kind":       "lines",
			"start_line": span.StartLine,
			"end_line":   span.EndLine,
		}
	case "page":
		return map[string]interface{}{
			"kind": "page",
			"page": span.Page,
		}
	case "time":
		out := map[string]interface{}{
			"kind":     "time",
			"start_ms": span.StartMS,
			"end_ms":   span.EndMS,
		}
		// Additive (SPEC §8.6.8/§9.2): a diarized transcript's time span surfaces
		// the stable speaker id and optional label. Omitted when absent, so a
		// non-diarized span is byte-identical to before and consumers degrade to
		// a flat citation.
		if speaker := strings.TrimSpace(span.Speaker); speaker != "" {
			out["speaker"] = speaker
			if label := strings.TrimSpace(span.SpeakerLabel); label != "" {
				out["speaker_label"] = label
			}
		}
		return out
	case "region":
		return buildRegionSpan(span)
	default:
		return map[string]interface{}{
			"kind": kind,
		}
	}
}

// buildRegionSpan renders a region span per spec §15.1.1: page range plus the
// primary-page bounding box and section breadcrumb. A region span missing its
// payload or bbox degrades to a page span on the start page (or the document
// variant when even that is unavailable), so clients always get a usable
// citation.
func buildRegionSpan(span model.Span) map[string]interface{} {
	r := span.Region
	if r == nil || r.BBox == nil {
		page := 0
		if r != nil {
			page = r.StartPage
		}
		if page <= 0 {
			return map[string]interface{}{"kind": "document"}
		}
		return map[string]interface{}{"kind": "page", "page": page}
	}
	section := r.Section
	if section == nil {
		section = []string{}
	}
	return map[string]interface{}{
		"kind":       "region",
		"start_page": r.StartPage,
		"end_page":   r.EndPage,
		"bbox": map[string]interface{}{
			"page":         r.BBox.Page,
			"l":            r.BBox.L,
			"t":            r.BBox.T,
			"r":            r.BBox.R,
			"b":            r.BBox.B,
			"coord_origin": r.BBox.CoordOrigin,
		},
		"section": section,
	}
}

func parseInteger(value interface{}, field string) (int, error) {
	switch v := value.(type) {
	case float64:
		if math.Trunc(v) != v {
			return 0, fmt.Errorf("%s must be an integer", field)
		}
		if v < math.MinInt || v > math.MaxInt {
			return 0, fmt.Errorf("%s is out of range", field)
		}
		return int(v), nil
	case int:
		return v, nil
	case int64:
		if v < math.MinInt || v > math.MaxInt {
			return 0, fmt.Errorf("%s is out of range", field)
		}
		return int(v), nil
	default:
		return 0, fmt.Errorf("%s must be an integer", field)
	}
}

func parseOptionalIntegerWithPresence(args map[string]interface{}, key string) (int, bool, error) {
	raw, ok := args[key]
	if !ok {
		return 0, false, nil
	}
	v, err := parseInteger(raw, key)
	if err != nil {
		return 0, true, err
	}
	return v, true, nil
}

func parseOptionalStringSlice(args map[string]interface{}, key string) ([]string, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return nil, nil
	}

	switch typed := raw.(type) {
	case []interface{}:
		out := make([]string, 0, len(typed))
		for idx, item := range typed {
			v, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%s[%d] must be a string", key, idx)
			}
			v = strings.TrimSpace(v)
			if v == "" {
				return nil, fmt.Errorf("%s[%d] must be a non-empty string", key, idx)
			}
			out = append(out, v)
		}
		return out, nil
	case []string:
		out := make([]string, 0, len(typed))
		for idx, item := range typed {
			item = strings.TrimSpace(item)
			if item == "" {
				return nil, fmt.Errorf("%s[%d] must be a non-empty string", key, idx)
			}
			out = append(out, item)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s must be an array of strings", key)
	}
}

func normalizeFileStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "skipped":
		return "skipped"
	case "error":
		return "error"
	default:
		return "ok"
	}
}

func isListFilesNoisePath(relPath string) bool {
	normalized := strings.TrimSpace(filepath.ToSlash(relPath))
	if normalized == "" {
		return false
	}
	first := normalized
	if idx := strings.IndexByte(normalized, '/'); idx >= 0 {
		first = normalized[:idx]
	}
	if strings.HasPrefix(first, ".") {
		return true
	}
	return false
}

// looksLikeBinaryContent reports whether the payload is unsafe to surface as
// MCP `text` content. Some binary formats (mp3 frames, certain pdf byte
// ranges) can lack NUL bytes entirely, so the NUL-byte signal alone is not
// enough; we additionally reject payloads that aren't valid UTF-8 or whose
// non-whitespace control-character density exceeds binaryControlCharThreshold
// (sampled over the first binaryDetectionSampleBytes bytes to bound cost on
// large payloads). The threshold is intentionally permissive — well-formed
// markdown / source code is well under it, while typical binary data sails
// past it — and the goal is "this clearly isn't text the agent can use,"
// not perfect format detection.
func looksLikeBinaryContent(content string) bool {
	if content == "" {
		return false
	}
	if strings.IndexByte(content, 0) >= 0 {
		return true
	}
	sample := content
	if len(sample) > binaryDetectionSampleBytes {
		sample = sample[:binaryDetectionSampleBytes]
	}
	if !utf8.ValidString(sample) {
		return true
	}
	var controls, total int
	for _, r := range sample {
		total++
		// Standard whitespace (\t \n \r) is text, not "binary noise".
		if r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		// C0 controls and DEL.
		if r < 0x20 || r == 0x7f {
			controls++
		}
	}
	if total == 0 {
		return false
	}
	return float64(controls)/float64(total) > binaryControlCharThreshold
}

const (
	// binaryDetectionSampleBytes bounds how many bytes looksLikeBinaryContent
	// scans for the control-character heuristic so very large text payloads
	// (large OCR markdown, code dumps) don't pay an O(N) walk of the whole body.
	binaryDetectionSampleBytes = 4096
	// binaryControlCharThreshold is the ratio of non-whitespace C0 control bytes
	// (excluding \t \n \r) above which we treat content as binary. 0.30 is well
	// above realistic text payloads and well below typical binary streams.
	binaryControlCharThreshold = 0.30
)

func serializeHit(h model.SearchHit) map[string]interface{} {
	out := map[string]interface{}{
		"chunk_id": h.ChunkID,
		"rel_path": h.RelPath,
		"doc_type": h.DocType,
		"rep_type": h.RepType,
		"score":    h.Score,
		"snippet":  h.Snippet,
		"span":     buildOpenFileSpan(h.Span),
	}
	if title := strings.TrimSpace(h.Title); title != "" {
		out["title"] = title
	}
	if modality := strings.TrimSpace(h.Modality); modality != "" {
		out["modality"] = modality
	}
	if mediaRef := strings.TrimSpace(h.MediaRef); mediaRef != "" {
		out["media_ref"] = mediaRef
	}
	return out
}

func serializeSearchHits(hits []model.SearchHit) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(hits))
	for _, h := range hits {
		out = append(out, serializeHit(h))
	}
	return out
}

func buildAskStructuredContent(result model.AskResult) map[string]interface{} {
	citations := make([]map[string]interface{}, 0, len(result.Citations))
	for _, citation := range result.Citations {
		entry := map[string]interface{}{
			"chunk_id": citation.ChunkID,
			"rel_path": citation.RelPath,
			"span":     buildOpenFileSpan(citation.Span),
		}
		if title := strings.TrimSpace(citation.Title); title != "" {
			entry["title"] = title
		}
		citations = append(citations, entry)
	}

	hits := make([]map[string]interface{}, 0, len(result.Hits))
	for _, hit := range result.Hits {
		hits = append(hits, serializeHit(hit))
	}

	return map[string]interface{}{
		"question":          result.Question,
		"answer":            result.Answer,
		"citations":         citations,
		"hits":              hits,
		"indexing_complete": result.IndexingComplete,
	}
}

func spanDefinitionSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"oneOf": []interface{}{
			map[string]interface{}{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]interface{}{
					"kind":       map[string]interface{}{"const": "lines"},
					"start_line": map[string]interface{}{"type": "integer"},
					"end_line":   map[string]interface{}{"type": "integer"},
				},
				"required": []string{"kind", "start_line", "end_line"},
			},
			map[string]interface{}{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]interface{}{
					"kind": map[string]interface{}{"const": "page"},
					"page": map[string]interface{}{"type": "integer"},
				},
				"required": []string{"kind", "page"},
			},
			map[string]interface{}{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]interface{}{
					"kind":     map[string]interface{}{"const": "time"},
					"start_ms": map[string]interface{}{"type": "integer"},
					"end_ms":   map[string]interface{}{"type": "integer"},
					"speaker": map[string]interface{}{
						"type":        "string",
						"description": "Optional (SPEC §8.6.8): stable per-transcript speaker id on a diarized transcript.",
					},
					"speaker_label": map[string]interface{}{
						"type":        "string",
						"description": "Optional human-readable speaker name (SPEC §8.6.8).",
					},
				},
				"required": []string{"kind", "start_ms", "end_ms"},
			},
			map[string]interface{}{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]interface{}{
					"kind":       map[string]interface{}{"const": "region"},
					"start_page": map[string]interface{}{"type": "integer"},
					"end_page":   map[string]interface{}{"type": "integer"},
					"bbox": map[string]interface{}{
						"type":                 "object",
						"additionalProperties": false,
						"properties": map[string]interface{}{
							"page":         map[string]interface{}{"type": "integer"},
							"l":            map[string]interface{}{"type": "number"},
							"t":            map[string]interface{}{"type": "number"},
							"r":            map[string]interface{}{"type": "number"},
							"b":            map[string]interface{}{"type": "number"},
							"coord_origin": map[string]interface{}{"type": "string"},
						},
						"required": []string{"page", "l", "t", "r", "b", "coord_origin"},
					},
					"section": map[string]interface{}{
						"type":  "array",
						"items": map[string]interface{}{"type": "string"},
					},
				},
				"required": []string{"kind", "start_page", "end_page", "bbox", "section"},
			},
			map[string]interface{}{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]interface{}{
					"kind": map[string]interface{}{"const": "document"},
				},
				"required": []string{"kind"},
			},
		},
	}
}

func hitDefinitionSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"chunk_id": map[string]interface{}{"type": "integer"},
			"rel_path": map[string]interface{}{"type": "string"},
			"title":    map[string]interface{}{"type": "string"},
			"doc_type": map[string]interface{}{"type": "string"},
			"rep_type": map[string]interface{}{"type": "string"},
			"score":    map[string]interface{}{"type": "number"},
			"snippet":  map[string]interface{}{"type": "string"},
			"span":     map[string]interface{}{"$ref": "#/definitions/Span"},
		},
		"required": []string{"chunk_id", "rel_path", "score", "snippet", "span"},
	}
}

func sharedDefinitions() map[string]interface{} {
	return map[string]interface{}{
		"Span": spanDefinitionSchema(),
		"Hit":  hitDefinitionSchema(),
	}
}

func searchInputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"query":       map[string]interface{}{"type": "string", "minLength": 1},
			"k":           map[string]interface{}{"type": "integer", "minimum": 1, "maximum": MaxSearchK, "default": DefaultSearchK},
			"index":       map[string]interface{}{"type": "string", "enum": []string{"auto", "text", "code", "both"}, "default": "auto"},
			"path_prefix": map[string]interface{}{"type": "string"},
			"file_glob":   map[string]interface{}{"type": "string"},
			"doc_types":   map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
			"speaker": map[string]interface{}{
				"type":        "string",
				"description": "Optional (SPEC §8.6.8): restrict time-spanned transcript hits to this speaker id. A corpus without diarized transcripts returns no speaker-filtered hits.",
			},
			"languages": map[string]interface{}{
				"type":        "array",
				"items":       map[string]interface{}{"type": "string"},
				"description": "Optional (SPEC §9.5): restrict hits to representations recorded in any of these BCP-47 languages (case-insensitive primary-subtag match). Absent/empty = no filtering; unknown-language representations never match a specific filter.",
			},
		},
		"required": []string{"query"},
	}
}

func searchOutputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"query":             map[string]interface{}{"type": "string"},
			"k":                 map[string]interface{}{"type": "integer"},
			"index_used":        map[string]interface{}{"type": "string", "enum": []string{"text", "code", "both"}},
			"hits":              map[string]interface{}{"type": "array", "items": map[string]interface{}{"$ref": "#/definitions/Hit"}},
			"indexing_complete": map[string]interface{}{"type": "boolean"},
		},
		"required":    []string{"query", "k", "index_used", "hits", "indexing_complete"},
		"definitions": sharedDefinitions(),
	}
}

func askInputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"question":    map[string]interface{}{"type": "string", "minLength": 1},
			"k":           map[string]interface{}{"type": "integer", "minimum": 1, "maximum": MaxSearchK, "default": DefaultSearchK},
			"mode":        map[string]interface{}{"type": "string", "enum": []string{"answer", "search_only"}, "default": "answer"},
			"index":       map[string]interface{}{"type": "string", "enum": []string{"auto", "text", "code", "both"}, "default": "auto"},
			"path_prefix": map[string]interface{}{"type": "string"},
			"file_glob":   map[string]interface{}{"type": "string"},
			"doc_types":   map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
			"languages": map[string]interface{}{
				"type":        "array",
				"items":       map[string]interface{}{"type": "string"},
				"description": "Optional (SPEC §9.5): restrict retrieved contexts to representations recorded in any of these BCP-47 languages (case-insensitive primary-subtag match). Absent/empty = no filtering; unknown-language representations never match a specific filter.",
			},
		},
		"required": []string{"question"},
	}
}

func askOutputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"question": map[string]interface{}{"type": "string"},
			"answer":   map[string]interface{}{"type": "string"},
			"citations": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]interface{}{
						"chunk_id": map[string]interface{}{"type": "integer"},
						"rel_path": map[string]interface{}{"type": "string"},
						"title":    map[string]interface{}{"type": "string"},
						"span":     map[string]interface{}{"$ref": "#/definitions/Span"},
					},
					"required": []string{"chunk_id", "rel_path", "span"},
				},
			},
			"hits":              map[string]interface{}{"type": "array", "items": map[string]interface{}{"$ref": "#/definitions/Hit"}},
			"indexing_complete": map[string]interface{}{"type": "boolean"},
		},
		"required":    []string{"question", "answer", "citations", "hits", "indexing_complete"},
		"definitions": sharedDefinitions(),
	}
}

func askAudioInputSchema() map[string]interface{} {
	schema := askInputSchema()
	properties, ok := schema["properties"].(map[string]interface{})
	if !ok {
		return askInputSchema()
	}
	properties["voice_id"] = map[string]interface{}{"type": "string", "minLength": 1}
	return schema
}

func askAudioOutputSchema() map[string]interface{} {
	schema := askOutputSchema()
	properties, ok := schema["properties"].(map[string]interface{})
	if !ok {
		return askOutputSchema()
	}
	properties["audio"] = map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"mime_type": map[string]interface{}{"type": "string", "enum": []string{"audio/mpeg"}},
			"data":      map[string]interface{}{"type": "string"},
		},
		"required": []string{"mime_type", "data"},
	}
	return schema
}

func transcribeInputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"rel_path":     map[string]interface{}{"type": "string", "minLength": 1},
			"language":     map[string]interface{}{"type": "string"},
			"timestamps":   map[string]interface{}{"type": "boolean", "default": true},
			"retranscribe": map[string]interface{}{"type": "boolean", "default": false},
		},
		"required": []string{"rel_path"},
	}
}

func transcribeOutputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"rel_path":        map[string]interface{}{"type": "string"},
			"provider":        map[string]interface{}{"type": "string", "enum": []string{"mistral", "elevenlabs"}},
			"model":           map[string]interface{}{"type": "string"},
			"indexed":         map[string]interface{}{"type": "boolean"},
			"transcribed":     map[string]interface{}{"type": "boolean"},
			"transcribed_now": map[string]interface{}{"type": "boolean"},
			"segments": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]interface{}{
						"start_ms": map[string]interface{}{"type": "integer"},
						"end_ms":   map[string]interface{}{"type": "integer"},
						"text":     map[string]interface{}{"type": "string"},
					},
					"required": []string{"start_ms", "end_ms", "text"},
				},
			},
		},
		"required": []string{"rel_path", "provider", "model", "indexed", "transcribed"},
	}
}

func annotateInputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"rel_path":             map[string]interface{}{"type": "string", "minLength": 1},
			"schema_json":          map[string]interface{}{"type": "object"},
			"index_flattened_text": map[string]interface{}{"type": "boolean", "default": true},
			"max_chars":            map[string]interface{}{"type": "integer", "minimum": 200, "maximum": 200000, "default": 32000},
		},
		"required": []string{"rel_path", "schema_json"},
	}
}

func annotateOutputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"rel_path":                map[string]interface{}{"type": "string"},
			"stored":                  map[string]interface{}{"type": "boolean"},
			"flattened_indexed":       map[string]interface{}{"type": "boolean"},
			"annotation_json":         map[string]interface{}{"type": "object"},
			"annotation_text_preview": map[string]interface{}{"type": "string"},
			"source_doc_type":         map[string]interface{}{"type": "string"},
			"source_rep":              map[string]interface{}{"type": "string"},
		},
		"required": []string{"rel_path", "stored", "flattened_indexed", "annotation_json"},
	}
}

func transcribeAndAskInputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"rel_path": map[string]interface{}{"type": "string", "minLength": 1},
			"question": map[string]interface{}{"type": "string", "minLength": 1},
			"k":        map[string]interface{}{"type": "integer", "minimum": 1, "maximum": MaxSearchK, "default": DefaultSearchK},
		},
		"required": []string{"rel_path", "question"},
	}
}

func transcribeAndAskOutputSchema() map[string]interface{} {
	orig := askOutputSchema()
	// make a shallow copy of the top‑level map
	schema := make(map[string]interface{}, len(orig))
	for k, v := range orig {
		schema[k] = v
	}

	// copy properties map so we don't mutate orig
	properties := make(map[string]interface{})
	if origProps, ok := orig["properties"].(map[string]interface{}); ok {
		for k, v := range origProps {
			properties[k] = v
		}
	} else {
		// unexpected shape; fallback to asking again which will create a
		// new safe schema
		return askOutputSchema()
	}

	properties["transcript_provider"] = map[string]interface{}{"type": "string", "enum": []string{"mistral", "elevenlabs"}}
	properties["transcript_model"] = map[string]interface{}{"type": "string"}
	properties["transcribed"] = map[string]interface{}{"type": "boolean"}
	properties["transcribed_now"] = map[string]interface{}{"type": "boolean"}
	schema["properties"] = properties

	// handle required list; original is usually []string
	var requiredSlice []string
	if req, ok := orig["required"].([]string); ok {
		requiredSlice = append([]string(nil), req...)
	} else if reqIface, ok := orig["required"].([]interface{}); ok {
		for _, v := range reqIface {
			if s, ok := v.(string); ok {
				requiredSlice = append(requiredSlice, s)
			}
		}
	}
	if len(requiredSlice) > 0 {
		requiredSlice = append(requiredSlice, "transcript_provider", "transcript_model", "transcribed")
		schema["required"] = requiredSlice
	}

	return schema
}

func openFileInputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"rel_path":   map[string]interface{}{"type": "string", "minLength": 1},
			"start_line": map[string]interface{}{"type": "integer", "minimum": 1},
			"end_line":   map[string]interface{}{"type": "integer", "minimum": 1},
			"page":       map[string]interface{}{"type": "integer", "minimum": 1},
			"start_ms":   map[string]interface{}{"type": "integer", "minimum": 0},
			"end_ms":     map[string]interface{}{"type": "integer", "minimum": 0},
			"max_chars":  map[string]interface{}{"type": "integer", "minimum": 200, "maximum": 50000, "default": 20000},
		},
		"required": []string{"rel_path"},
	}
}

func openFileOutputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"rel_path":  map[string]interface{}{"type": "string"},
			"doc_type":  map[string]interface{}{"type": "string"},
			"span":      map[string]interface{}{"$ref": "#/definitions/Span"},
			"content":   map[string]interface{}{"type": "string"},
			"truncated": map[string]interface{}{"type": "boolean"},
		},
		"required":    []string{"rel_path", "doc_type", "content", "truncated"},
		"definitions": sharedDefinitions(),
	}
}

func openMediaClipInputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"chunk_id": map[string]interface{}{"type": "integer", "description": "Hit chunk id; resolved to its source media and time span."},
			"rel_path": map[string]interface{}{"type": "string", "minLength": 1},
			"start_ms": map[string]interface{}{"type": "integer", "minimum": 0},
			"end_ms":   map[string]interface{}{"type": "integer", "minimum": 0},
			"return":   map[string]interface{}{"type": "string", "enum": []string{"inline", "reference"}, "default": "inline"},
		},
		"anyOf": []interface{}{
			map[string]interface{}{"required": []string{"chunk_id"}},
			map[string]interface{}{"required": []string{"rel_path", "start_ms", "end_ms"}},
		},
	}
}

func openMediaClipOutputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"rel_path":           map[string]interface{}{"type": "string"},
			"doc_type":           map[string]interface{}{"type": "string"},
			"span":               map[string]interface{}{"$ref": "#/definitions/Span"},
			"mime_type":          map[string]interface{}{"type": "string"},
			"duration_ms":        map[string]interface{}{"type": "integer"},
			"size_bytes":         map[string]interface{}{"type": "integer"},
			"return":             map[string]interface{}{"type": "string", "enum": []string{"inline", "reference"}},
			"data":               map[string]interface{}{"type": "string", "contentEncoding": "base64", "description": "Present when return=inline: base64 clip bytes."},
			"uri":                map[string]interface{}{"type": "string", "description": "Present when return=reference: short-lived fetch URI."},
			"expires_unix":       map[string]interface{}{"type": "integer", "description": "Present when return=reference: expiry of uri."},
			"reference_fallback": map[string]interface{}{"type": "string", "description": "Set when reference was requested but inline was returned instead."},
		},
		"required": []string{"rel_path", "doc_type", "span", "mime_type", "return"},
		"allOf": []interface{}{
			map[string]interface{}{
				"if":   map[string]interface{}{"properties": map[string]interface{}{"return": map[string]interface{}{"const": "inline"}}, "required": []string{"return"}},
				"then": map[string]interface{}{"required": []string{"data"}},
			},
			map[string]interface{}{
				"if":   map[string]interface{}{"properties": map[string]interface{}{"return": map[string]interface{}{"const": "reference"}}, "required": []string{"return"}},
				"then": map[string]interface{}{"required": []string{"uri", "expires_unix"}},
			},
		},
		"definitions": sharedDefinitions(),
	}
}

func listFilesInputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"path_prefix":    map[string]interface{}{"type": "string"},
			"glob":           map[string]interface{}{"type": "string"},
			"limit":          map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 5000, "default": 200},
			"offset":         map[string]interface{}{"type": "integer", "minimum": 0, "default": 0},
			"include_hidden": map[string]interface{}{"type": "boolean", "default": false},
		},
	}
}

func listFilesOutputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"limit":  map[string]interface{}{"type": "integer"},
			"offset": map[string]interface{}{"type": "integer"},
			"total":  map[string]interface{}{"type": "integer"},
			"files": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]interface{}{
						"rel_path":   map[string]interface{}{"type": "string"},
						"title":      map[string]interface{}{"type": "string"},
						"doc_type":   map[string]interface{}{"type": "string"},
						"size_bytes": map[string]interface{}{"type": "integer"},
						"mtime_unix": map[string]interface{}{"type": "integer"},
						"status":     map[string]interface{}{"type": "string", "enum": []string{"ok", "skipped", "error"}},
						"deleted":    map[string]interface{}{"type": "boolean"},
					},
					"required": []string{"rel_path", "doc_type", "size_bytes", "mtime_unix", "status", "deleted"},
				},
			},
		},
		"required": []string{"limit", "offset", "total", "files"},
	}
}

func statsInputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
	}
}

func statsOutputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"root":             map[string]interface{}{"type": "string"},
			"state_dir":        map[string]interface{}{"type": "string"},
			"protocol_version": map[string]interface{}{"type": "string"},
			"doc_counts": map[string]interface{}{
				"type":                 "object",
				"additionalProperties": map[string]interface{}{"type": "integer"},
			},
			"total_docs":           map[string]interface{}{"type": "integer"},
			"doc_counts_available": map[string]interface{}{"type": "boolean"},
			"indexing": map[string]interface{}{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]interface{}{
					"job_id":          map[string]interface{}{"type": "string"},
					"running":         map[string]interface{}{"type": "boolean"},
					"mode":            map[string]interface{}{"type": "string", "enum": []string{"incremental", "full"}},
					"scanned":         map[string]interface{}{"type": "integer"},
					"indexed":         map[string]interface{}{"type": "integer"},
					"skipped":         map[string]interface{}{"type": "integer"},
					"deleted":         map[string]interface{}{"type": "integer"},
					"representations": map[string]interface{}{"type": "integer"},
					"chunks_total":    map[string]interface{}{"type": "integer"},
					"embedded_ok":     map[string]interface{}{"type": "integer"},
					"errors":          map[string]interface{}{"type": "integer"},
				},
				"required": []string{"job_id", "running", "mode", "scanned", "indexed", "skipped", "deleted", "representations", "chunks_total", "embedded_ok", "errors"},
			},
			"models": map[string]interface{}{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]interface{}{
					"embed_text":   map[string]interface{}{"type": "string"},
					"embed_code":   map[string]interface{}{"type": "string"},
					"ocr":          map[string]interface{}{"type": "string"},
					"stt_provider": map[string]interface{}{"type": "string", "enum": []string{"mistral", "elevenlabs"}},
					"stt_model":    map[string]interface{}{"type": "string"},
					"chat":         map[string]interface{}{"type": "string"},
				},
				"required": []string{"embed_text", "embed_code", "ocr", "stt_provider", "stt_model", "chat"},
			},
			"sessions": map[string]interface{}{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]interface{}{
					"active": map[string]interface{}{"type": "integer"},
					"items": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type":                 "object",
							"additionalProperties": false,
							"properties": map[string]interface{}{
								"id":             map[string]interface{}{"type": "string"},
								"created_unix":   map[string]interface{}{"type": "integer"},
								"last_seen_unix": map[string]interface{}{"type": "integer"},
							},
							"required": []string{"id", "created_unix", "last_seen_unix"},
						},
					},
				},
				"required": []string{"active", "items"},
			},
			// recent_failures: optional per SPEC §15.6 — implementations
			// MAY omit when no failures are recorded; clients MUST treat
			// omission as "no recent failures". additionalProperties:false
			// on the item mirrors the canonical schema mirror in
			// dirstral-spec/spec/tools/schemas/stats.json.
			"recent_failures": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]interface{}{
						"rel_path":      map[string]interface{}{"type": "string"},
						"doc_type":      map[string]interface{}{"type": "string"},
						"mtime_unix":    map[string]interface{}{"type": "integer"},
						"error_message": map[string]interface{}{"type": "string"},
					},
					"required": []string{"rel_path", "doc_type", "mtime_unix", "error_message"},
				},
			},
		},
		"required": []string{"root", "state_dir", "protocol_version", "doc_counts", "total_docs", "doc_counts_available", "indexing", "models", "sessions"},
	}
}
