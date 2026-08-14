package tests

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// These tests cover site 3 of #830 (the MCP on-demand document read) and the tool
// surface of site 2 (retrieval's source-byte hash).
//
// Both are reachable by an ordinary client request, so an unbounded read here is
// remote-triggerable: annotate on a local file was an os.ReadFile with no limit, and
// a file that had grown since discovery measured it was pulled whole into the server
// process.
//
// The fixtures below all seed the document row from a SMALL file and then grow the
// file on disk. That is the state a size check cannot catch: the row, the stat that
// produced it, and the bytes the read obtains are three separate things.

const mcpReadCapMB = 1
const mcpReadCapBytes = mcpReadCapMB * 1024 * 1024

// growFile overwrites the corpus file at relPath with size bytes of printable
// filler, simulating growth after discovery recorded the small original.
func growFile(t *testing.T, rootDir, relPath string, size int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(rootDir, relPath), bytes.Repeat([]byte("g"), size), 0o644); err != nil {
		t.Fatalf("grow %s: %v", relPath, err)
	}
}

// TestAnnotate_LocalFileGrownPastTheCapIsRefused is the core case for
// readDocumentContent: the document row says 5 bytes, the file is 4 MiB, the cap is
// 1 MiB, and the read must refuse instead of loading the file.
//
// FILE_TOO_LARGE (§14.4) is the existing code for this condition. Before the fix the
// read succeeded and the tool went on to call the model with the whole file.
func TestAnnotate_LocalFileGrownPastTheCapIsRefused(t *testing.T) {
	cfg, st, rootDir := setupMCPToolStore(t, "note.txt", "text", []byte("small"))
	cfg.IngestMaxFileMB = mcpReadCapMB
	growFile(t, rootDir, "note.txt", mcpReadCapBytes*4)

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":8301,"method":"tools/call","params":{"name":"dir2mcp_annotate","arguments":{"rel_path":"note.txt","schema_json":{"type":"object"}}}}`)
	defer func() { _ = resp.Body.Close() }()

	assertToolCallErrorCode(t, resp, "FILE_TOO_LARGE")
}

// TestAnnotate_LocalFileAtTheCapIsRead is the off-by-one guard on the same path: a
// file of exactly the cap must be READ, not refused.
//
// The read is observable without any provider: the bytes carry a secret-pattern
// match, so the #407 secret gate answers FORBIDDEN, which can only happen once the
// content has been read in full.
func TestAnnotate_LocalFileAtTheCapIsRead(t *testing.T) {
	const secret = "AKIA_AT_THE_CAP_SECRET"
	cfg, st, rootDir := setupMCPToolStore(t, "note.txt", "text", []byte("small"))
	cfg.IngestMaxFileMB = mcpReadCapMB
	cfg.SecretPatterns = []string{"AKIA_[A-Z_]+"}

	// Exactly the cap, with the marker at the very END: a truncated read would not
	// see it, so FORBIDDEN also proves the whole file arrived.
	filler := bytes.Repeat([]byte("f"), mcpReadCapBytes-len(secret))
	if err := os.WriteFile(filepath.Join(rootDir, "note.txt"), append(filler, []byte(secret)...), 0o644); err != nil {
		t.Fatalf("write at-cap file: %v", err)
	}

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":8302,"method":"tools/call","params":{"name":"dir2mcp_annotate","arguments":{"rel_path":"note.txt","schema_json":{"type":"object"}}}}`)
	defer func() { _ = resp.Body.Close() }()

	assertToolCallErrorCode(t, resp, protocol.ErrorCodeForbidden)
}

// TestOpenFile_LocalSourceGrownPastTheCapReportsFileTooLarge is the tool-surface half
// of site 2: retrieval bounds its source-byte hash, and the MCP layer reports the
// cap rather than a retryable INTERNAL_ERROR.
//
// The retrieval service is the real one, so this pins the whole chain: bounded read →
// corpusfs.ErrObjectTooLarge → §14.4 FILE_TOO_LARGE, non-retryable. A retry cannot
// help; the file is over the operator's cap until the file or the cap changes.
func TestOpenFile_LocalSourceGrownPastTheCapReportsFileTooLarge(t *testing.T) {
	cfg, st, rootDir := setupMCPToolStore(t, "act.pdf", "pdf", []byte("%PDF-1.4 small"))
	cfg.IngestMaxFileMB = mcpReadCapMB
	growFile(t, rootDir, "act.pdf", mcpReadCapBytes*4)

	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(cfg.RootDir)
	svc.SetStateDir(cfg.StateDir)
	svc.SetMaxFileBytes(int64(cfg.IngestMaxFileMB) * 1024 * 1024)

	server := httptest.NewServer(mcp.NewServer(cfg, svc, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":8303,"method":"tools/call","params":{"name":"dir2mcp_open_file","arguments":{"rel_path":"act.pdf"}}}`)
	defer func() { _ = resp.Body.Close() }()

	assertToolCallErrorCode(t, resp, "FILE_TOO_LARGE")
}

// TestAnnotate_DefaultCapStillBoundsTheRead pins the fail-closed default on the tool
// path: with `ingest.max_file_mb` left at its default the read is still bounded, so
// no deployment depends on setting the value to get a bound.
func TestAnnotate_DefaultCapStillBoundsTheRead(t *testing.T) {
	cfg, st, rootDir := setupMCPToolStore(t, "note.txt", "text", []byte("small"))
	defaults := config.Default()
	cfg.IngestMaxFileMB = defaults.IngestMaxFileMB
	growFile(t, rootDir, "note.txt", (cfg.IngestMaxFileMB*1024*1024)+1)

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":8304,"method":"tools/call","params":{"name":"dir2mcp_annotate","arguments":{"rel_path":"note.txt","schema_json":{"type":"object"}}}}`)
	defer func() { _ = resp.Body.Close() }()

	assertToolCallErrorCode(t, resp, "FILE_TOO_LARGE")
}
