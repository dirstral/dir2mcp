package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/protocol"
)

// A live `up` daemon must report the extraction engine it really wired through
// dir2mcp_stats (issue #851).
//
// The unit-level guards live in tests/conformance and tests/ingest. This one
// closes the loop over the WHOLE production chain: `up` resolves the extractor,
// hands the decision to the MCP server, and a client reads the answer back off
// the wire. Without that hand-over the daemon below answers with its configured
// OCR profile, which extracted nothing, because `ingest_extractor: off` means
// nothing extracts at all.

// waitForConnectionURL polls for the connection.json `up` writes once it serves,
// and returns the MCP URL from it.
func waitForConnectionURL(t *testing.T, connectionPath string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(connectionPath)
		if err == nil {
			var connection struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(raw, &connection) == nil && connection.URL != "" {
				return connection.URL
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("daemon never wrote a usable %s", connectionPath)
	return ""
}

// statsModelsOverHTTP initializes an MCP session against a live daemon, calls
// dir2mcp_stats and returns the models block a client reads.
func statsModelsOverHTTP(t *testing.T, mcpURL string) map[string]interface{} {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}

	post := func(sessionID, body string) *http.Response {
		req, err := http.NewRequest(http.MethodPost, mcpURL, bytes.NewBufferString(body))
		if err != nil {
			t.Fatalf("create request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if sessionID != "" {
			req.Header.Set(protocol.MCPSessionHeader, sessionID)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		return resp
	}

	initResp := post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	sessionID := initResp.Header.Get(protocol.MCPSessionHeader)
	_ = initResp.Body.Close()
	if initResp.StatusCode != http.StatusOK || sessionID == "" {
		t.Fatalf("initialize: status=%d session=%q", initResp.StatusCode, sessionID)
	}

	// Complete the bs-005 handshake; the daemon rejects tools/* before it (#656).
	notifResp := post(sessionID, `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
	_ = notifResp.Body.Close()
	if notifResp.StatusCode != http.StatusAccepted {
		t.Fatalf("notifications/initialized: status=%d want=202", notifResp.StatusCode)
	}

	statsResp := post(sessionID, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+protocol.ToolNameStats+`","arguments":{}}}`)
	body, err := io.ReadAll(statsResp.Body)
	_ = statsResp.Body.Close()
	if err != nil {
		t.Fatalf("read stats body: %v", err)
	}
	if statsResp.StatusCode != http.StatusOK {
		t.Fatalf("stats: status=%d body=%s", statsResp.StatusCode, body)
	}
	var envelope struct {
		Result struct {
			StructuredContent struct {
				Models map[string]interface{} `json:"models"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode stats response: %v body=%s", err, body)
	}
	if envelope.Result.StructuredContent.Models == nil {
		t.Fatalf("stats carried no models block: %s", body)
	}
	return envelope.Result.StructuredContent.Models
}

// TestUpStatsReportsNoExtractionEngineWhenExtractionIsOff runs a real daemon with
// extraction turned off and asks it what extracts. The honest answer is "nothing".
//
// It fails without the fix: the daemon reports its configured OCR model, so an
// operator reads that OCR is active on a corpus where no document is extracted
// at all.
func TestUpStatsReportsNoExtractionEngineWhenExtractionIsOff(t *testing.T) {
	tmp := t.TempDir()
	// A credential is present on purpose: it makes an OCR profile resolve, so the
	// pre-fix answer is a real model id rather than an empty string.
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	configBody := "auth_mode: none\ningest_extractor: \"off\"\n"
	if err := os.WriteFile(filepath.Join(tmp, ".dir2mcp.yaml"), []byte(configBody), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	var models map[string]interface{}
	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan int, 1)
		go func() {
			done <- app.RunWithContext(ctx, []string{"--non-interactive", "up", "--listen", "127.0.0.1:0"})
		}()

		models = statsModelsOverHTTP(t, waitForConnectionURL(t, filepath.Join(tmp, ".dir2mcp", "connection.json")))

		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatal("daemon did not stop after cancellation")
		}
	})

	// The daemon's own stderr is deliberately NOT echoed here. It carries the
	// startup banner and config diagnostics, and a test failure message is not a
	// redaction-reviewed surface. The assertion needs only the served value.
	ocr, _ := models["ocr"].(string)
	if ocr != "(no extraction engine)" {
		t.Fatalf("models.ocr = %q on a daemon with ingest_extractor=off, want the explicit no-engine marker", ocr)
	}
}
