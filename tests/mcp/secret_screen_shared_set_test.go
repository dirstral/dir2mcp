package tests

import (
	"net/http/httptest"
	"testing"

	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// TestMCPAnnotate_UsesTheRetrieverSecretSet pins that annotate gates its text
// on the same compiled set that open_file, search, ask and related use (spec
// 0.93.0). The pattern is set on the retrieval service only, not in
// cfg.SecretPatterns, so a FORBIDDEN result can come only from the shared set.
func TestMCPAnnotate_UsesTheRetrieverSecretSet(t *testing.T) {
	const credential = "shared-set-credential-7731"
	cfg, st, _ := setupMCPToolStore(t, "note.txt", "text", []byte("the token is "+credential))

	ret := retrieval.NewService(st, nil, nil, nil)
	if err := ret.SetSecretPatterns([]string{`shared-set-credential-[0-9]{4}`}); err != nil {
		t.Fatalf("SetSecretPatterns: %v", err)
	}

	server := httptest.NewServer(mcp.NewServer(cfg, ret, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":931,"method":"tools/call","params":{"name":"dir2mcp_annotate","arguments":{"rel_path":"note.txt","schema_json":{"type":"object"}}}}`)
	defer func() { _ = resp.Body.Close() }()
	assertToolCallErrorCodeAndMessage(t, resp, protocol.ErrorCodeForbidden, nil, []string{credential})
}
