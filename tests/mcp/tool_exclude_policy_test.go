package tests

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/store"
)

// exampleAWSKey is a well-formed but non-live AWS access-key ID that matches the
// default AKIA secret pattern. It is only used to exercise the secret gate.
const exampleAWSKey = "AKIAIOSFODNN7EXAMPLE"

// newExcludeTestServer builds an MCP server rooted at a fresh temp dir with a
// live sqlite store, returning the config and root so callers can drop files.
func newExcludeTestServer(t *testing.T) (config.Config, *store.SQLiteStore, string) {
	t.Helper()
	rootDir := t.TempDir()
	stateDir := filepath.Join(rootDir, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Default()
	cfg.RootDir = rootDir
	cfg.StateDir = stateDir
	cfg.MCPPath = protocol.DefaultMCPPath
	cfg.AuthMode = "none"
	return cfg, st, rootDir
}

// TestMCPTranscribe_RefusesExcludedPath verifies that an operator-excluded audio
// file cannot be transcribed on demand (issue #407 part 1). The file is never
// indexed, so the request takes the on-demand init branch — which must now apply
// the same path-exclusion policy ingestion enforces.
func TestMCPTranscribe_RefusesExcludedPath(t *testing.T) {
	cfg, st, rootDir := newExcludeTestServer(t)
	cfg.PathExcludes = append(cfg.PathExcludes, "private/**")

	if err := os.MkdirAll(filepath.Join(rootDir, "private"), 0o755); err != nil {
		t.Fatalf("mkdir private: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "private", "voice.wav"), []byte("RIFF0000WAVEfmt data"), 0o644); err != nil {
		t.Fatalf("write audio: %v", err)
	}

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":701,"method":"tools/call","params":{"name":"dir2mcp_transcribe","arguments":{"rel_path":"private/voice.wav"}}}`)
	defer func() { _ = resp.Body.Close() }()
	assertToolCallErrorCode(t, resp, protocol.ErrorCodePermissionDenied)
}

// TestMCPTranscribe_RefusesOversizeAudio verifies the on-demand read is bounded
// by the ingest file-size cap (issue #407 part 1): a within-root audio file that
// exceeds the cap is refused rather than read unbounded into memory.
func TestMCPTranscribe_RefusesOversizeAudio(t *testing.T) {
	cfg, st, rootDir := newExcludeTestServer(t)
	cfg.IngestMaxFileMB = 1

	big := bytes.Repeat([]byte("A"), 1200*1024) // ~1.17 MiB > 1 MiB cap
	if err := os.WriteFile(filepath.Join(rootDir, "big.wav"), big, 0o644); err != nil {
		t.Fatalf("write audio: %v", err)
	}

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":702,"method":"tools/call","params":{"name":"dir2mcp_transcribe","arguments":{"rel_path":"big.wav"}}}`)
	defer func() { _ = resp.Body.Close() }()
	assertToolCallErrorCode(t, resp, "FILE_TOO_LARGE")
}

// TestMCPAnnotate_RefusesExcludedPath verifies annotate honours the exclusion
// policy for an indexed-but-excluded document (issue #407 part 1).
func TestMCPAnnotate_RefusesExcludedPath(t *testing.T) {
	// setupMCPToolStore upserts the doc and uses config.Default(), whose
	// PathExcludes already carry the default secret-file globs (**/*.key).
	cfg, st, _ := setupMCPToolStore(t, "secret.key", "text", []byte("private material"))

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":703,"method":"tools/call","params":{"name":"dir2mcp_annotate","arguments":{"rel_path":"secret.key","schema_json":{"type":"object"}}}}`)
	defer func() { _ = resp.Body.Close() }()
	assertToolCallErrorCode(t, resp, protocol.ErrorCodePermissionDenied)
}

// TestMCPAnnotate_RefusesSecretContent verifies annotate applies the same
// secret-pattern gate open_file enforces (issue #407 part 2): a document whose
// text matches secret_patterns is refused before any provider call, and the
// secret value is not echoed back in the error.
func TestMCPAnnotate_RefusesSecretContent(t *testing.T) {
	cfg, st, _ := setupMCPToolStore(t, "note.txt", "text", []byte("aws key "+exampleAWSKey))

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":704,"method":"tools/call","params":{"name":"dir2mcp_annotate","arguments":{"rel_path":"note.txt","schema_json":{"type":"object"}}}}`)
	defer func() { _ = resp.Body.Close() }()
	assertToolCallErrorCodeAndMessage(t, resp, protocol.ErrorCodePermissionDenied, nil, []string{exampleAWSKey})
}

// TestMCPTranscribe_RefusesSecretTranscript verifies the transcript output is
// gated by secret_patterns (issue #407 part 2): a transcript containing a secret
// is refused rather than returned as segments.
func TestMCPTranscribe_RefusesSecretTranscript(t *testing.T) {
	cfg, st, rootDir := newExcludeTestServer(t)
	if err := os.WriteFile(filepath.Join(rootDir, "voice.wav"), []byte("RIFF0000WAVEfmt data"), 0o644); err != nil {
		t.Fatalf("write audio: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"segments":[{"start":1,"end":2,"text":"my key is ` + exampleAWSKey + `"}]}`))
	}))
	defer upstream.Close()
	cfg = withMistralUpstream(t, cfg, "mistral-ocr", upstream.URL)

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":705,"method":"tools/call","params":{"name":"dir2mcp_transcribe","arguments":{"rel_path":"voice.wav"}}}`)
	defer func() { _ = resp.Body.Close() }()
	assertToolCallErrorCodeAndMessage(t, resp, protocol.ErrorCodePermissionDenied, nil, []string{exampleAWSKey})
}
