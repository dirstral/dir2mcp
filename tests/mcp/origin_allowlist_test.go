package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

func TestOriginAllowlist_NoOriginHeaderPasses(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	rr := initializeRequest(t, cfg, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
}

func TestOriginAllowlist_DefaultLocalhostPasses(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	rr := initializeRequest(t, cfg, "http://localhost")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
}

func TestOriginAllowlist_DefaultConfigBlocksElevenLabs(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	rr := initializeRequest(t, cfg, "https://elevenlabs.io")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d want=%d body=%s", rr.Code, http.StatusForbidden, rr.Body.String())
	}
	assertCanonicalErrorCode(t, rr.Body.Bytes(), "FORBIDDEN_ORIGIN")
}

func TestOriginAllowlist_EnvAllowsElevenLabsAndKeepsLocalhost(t *testing.T) {
	tmp := t.TempDir()

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_ALLOWED_ORIGINS", "https://elevenlabs.io")

		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		cfg.AuthMode = "none"

		elevenLabsResp := initializeRequest(t, cfg, "https://elevenlabs.io")
		if elevenLabsResp.Code != http.StatusOK {
			t.Fatalf("status=%d want=%d body=%s", elevenLabsResp.Code, http.StatusOK, elevenLabsResp.Body.String())
		}

		localhostResp := initializeRequest(t, cfg, "http://localhost")
		if localhostResp.Code != http.StatusOK {
			t.Fatalf("status=%d want=%d body=%s", localhostResp.Code, http.StatusOK, localhostResp.Body.String())
		}
	})
}

func TestOriginAllowlist_MalformedOriginBlocked(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	rr := initializeRequest(t, cfg, "://bad-origin")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d want=%d body=%s", rr.Code, http.StatusForbidden, rr.Body.String())
	}
	assertCanonicalErrorCode(t, rr.Body.Bytes(), "FORBIDDEN_ORIGIN")
}

func initializeRequest(t *testing.T, cfg config.Config, origin string) *httptest.ResponseRecorder {
	t.Helper()

	reqBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	req := httptest.NewRequest(http.MethodPost, cfg.MCPPath, bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}

	rr := httptest.NewRecorder()
	mcp.NewServer(cfg, nil).Handler().ServeHTTP(rr, req)
	return rr
}

func assertCanonicalErrorCode(t *testing.T, payload []byte, wantCode string) {
	t.Helper()

	var resp struct {
		Error struct {
			Data struct {
				Code string `json:"code"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil {
		t.Fatalf("decode error payload: %v body=%s", err, string(payload))
	}
	if resp.Error.Data.Code != wantCode {
		t.Fatalf("error code=%q want=%q body=%s", resp.Error.Data.Code, wantCode, string(payload))
	}
}
