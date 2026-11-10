package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
)

// dir2mcp_recheck_path (SPEC §15.13, optional extension). A client asks a
// server in watch mode to reconcile one path under the corpus root now. The
// call validates, classifies, enqueues and returns; the watcher's worker
// performs the reconcile. The tool is served only when the daemon handed the
// server a rechecker (see WithPathRechecker and buildToolRegistry).

// handleRecheckPathTool implements the call-time contract of §15.13.
func (s *Server) handleRecheckPathTool(ctx context.Context, args map[string]interface{}) (toolCallResult, *toolExecutionError) {
	if err := assertNoUnknownArguments(args, map[string]struct{}{"rel_path": {}, "recursive": {}}); err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	relPath, toolErr := parseRecheckRelPath(args)
	if toolErr != nil {
		return toolCallResult{}, toolErr
	}
	recursive, err := parseOptionalBool(args, "recursive", true)
	if err != nil {
		return toolCallResult{}, &toolExecutionError{Code: "INVALID_FIELD", Message: err.Error(), Retryable: false}
	}
	if s.rechecker == nil {
		return toolCallResult{}, toolUnavailableError(protocol.ToolNameRecheckPath)
	}
	res, err := s.rechecker.RecheckPath(ctx, relPath, recursive)
	if err != nil {
		return toolCallResult{}, mapRecheckError(err)
	}
	structured := map[string]interface{}{
		"rel_path": res.RelPath,
		"kind":     res.Kind,
		"queued":   res.Queued,
		"pending":  res.Pending,
	}
	var text string
	if res.Queued {
		text = fmt.Sprintf("queued a recheck of %s (%s); %d request(s) pending", res.RelPath, res.Kind, res.Pending)
	} else {
		text = fmt.Sprintf("a recheck of %s (%s) was already pending; %d request(s) pending", res.RelPath, res.Kind, res.Pending)
	}
	return toolCallResult{
		Content:           []toolContentItem{{Type: "text", Text: text}},
		StructuredContent: structured,
	}, nil
}

// parseRecheckRelPath reads rel_path. An absent or empty value is
// MISSING_FIELD (§15.13 step 1); a value of another type is INVALID_FIELD.
func parseRecheckRelPath(args map[string]interface{}) (string, *toolExecutionError) {
	raw, present := args["rel_path"]
	if !present {
		return "", &toolExecutionError{Code: "MISSING_FIELD", Message: "rel_path is required", Retryable: false}
	}
	value, ok := raw.(string)
	if !ok {
		return "", &toolExecutionError{Code: "INVALID_FIELD", Message: "rel_path must be a string", Retryable: false}
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", &toolExecutionError{Code: "MISSING_FIELD", Message: "rel_path is required", Retryable: false}
	}
	return value, nil
}

// mapRecheckError converts a rechecker error into the §14 code it names.
func mapRecheckError(err error) *toolExecutionError {
	switch {
	case errors.Is(err, ingest.ErrRecheckPathRequired):
		return &toolExecutionError{Code: "MISSING_FIELD", Message: "rel_path is required", Retryable: false}
	case errors.Is(err, model.ErrPathOutsideRoot):
		return &toolExecutionError{Code: "PATH_OUTSIDE_ROOT", Message: "path outside root", Retryable: false}
	case errors.Is(err, ingest.ErrWatchNotRunning):
		// The server serves the tool, so the daemon will run the watcher; the
		// loop has not started yet (or is shutting down). A retry can succeed.
		return &toolExecutionError{Code: protocol.ErrorCodeIndexNotReady, Message: "file watcher is not running yet; retry shortly", Retryable: true}
	case errors.Is(err, os.ErrPermission):
		return &toolExecutionError{Code: protocol.ErrorCodePermissionDenied, Message: "permission denied", Retryable: false}
	default:
		return &toolExecutionError{Code: "INTERNAL_ERROR", Message: "internal error while queuing the recheck", Retryable: false}
	}
}

// toolUnavailableError is the §14.3 TOOL_UNAVAILABLE result for a tool the
// spec defines but this server does not serve in its current mode.
func toolUnavailableError(name string) *toolExecutionError {
	return &toolExecutionError{
		Code:      protocol.ErrorCodeToolUnavailable,
		Message:   fmt.Sprintf("%s is defined but the running server does not serve it in its current mode", name),
		Retryable: false,
	}
}

// unavailableToolError reports TOOL_UNAVAILABLE for a tool name that the spec
// defines (it is in toolOrder) but that this server left out of its registry.
// Any other unknown name returns nil, so the caller keeps its unknown-tool
// behaviour. Both tools/call paths (the direct handler and the SDK transport)
// call it, so they cannot drift.
func (s *Server) unavailableToolError(name string) *toolExecutionError {
	if _, served := s.tools[name]; served {
		return nil
	}
	for _, known := range toolOrder {
		if known == name {
			return toolUnavailableError(name)
		}
	}
	return nil
}

// unavailableToolErrorForParams applies unavailableToolError to raw tools/call
// params. Params the SDK would reject anyway return nil, so the SDK keeps its
// own error contract for them.
func (s *Server) unavailableToolErrorForParams(rawParams json.RawMessage) *toolExecutionError {
	if len(rawParams) == 0 {
		return nil
	}
	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(rawParams, &params); err != nil {
		return nil
	}
	return s.unavailableToolError(strings.TrimSpace(params.Name))
}

// recheckPathInputSchema mirrors spec/tools/schemas/recheck_path.json "input".
func recheckPathInputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"rel_path": map[string]interface{}{
				"type":        "string",
				"minLength":   1,
				"description": "Path relative to the corpus root, with the grammar and normalization of dir2mcp_open_file's rel_path ('/' as the separator, no leading '/'). A file or a directory; '.' names the root itself. A path that resolves outside the root is PATH_OUTSIDE_ROOT. An excluded path is accepted and reconciled as ineligible. A path that does not exist is accepted: the reconcile tombstones what was indexed there.",
			},
			"recursive": map[string]interface{}{
				"type":        "boolean",
				"default":     true,
				"description": "For a directory: when true (default) reconcile the whole subtree; when false, only the regular files that are its direct children. Ignored for a file.",
			},
		},
		"required": []string{"rel_path"},
	}
}

// recheckPathOutputSchema mirrors spec/tools/schemas/recheck_path.json "output".
func recheckPathOutputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"rel_path": map[string]interface{}{
				"type":        "string",
				"description": "The normalized corpus-relative path the request resolved to ('.' for the root).",
			},
			"kind": map[string]interface{}{
				"type":        "string",
				"enum":        []string{ingest.RecheckKindFile, ingest.RecheckKindDirectory, ingest.RecheckKindMissing},
				"description": "What one stat of the resolved path found at call time: a regular file, a directory, or nothing (missing).",
			},
			"queued": map[string]interface{}{
				"type":        "boolean",
				"description": "true when this call added a reconcile request; false when a request for the same normalized rel_path was already pending (the queue holds at most one per path).",
			},
			"pending": map[string]interface{}{
				"type":        "integer",
				"minimum":     0,
				"description": "Number of reconcile requests pending after this call, this one included when queued is true.",
			},
		},
		"required": []string{"rel_path", "kind", "queued", "pending"},
	}
}
