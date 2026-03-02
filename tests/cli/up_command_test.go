package tests

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"dir2mcp/internal/cli"
	"dir2mcp/internal/config"
	"dir2mcp/internal/model"
	"dir2mcp/internal/protocol"
	"dir2mcp/internal/store"
)

var cwdMu sync.Mutex

// fakeErrorStore is a minimal model.Store implementation whose
// ListEmbeddedChunkMetadata method always returns the configured error.
// Other methods are no-ops so that runUp can exercise startup logic
// without panic.
type fakeErrorStore struct {
	err error
}

func (f *fakeErrorStore) Init(ctx context.Context) error { return nil }
func (f *fakeErrorStore) UpsertDocument(ctx context.Context, doc model.Document) error {
	return nil
}
func (f *fakeErrorStore) GetDocumentByPath(ctx context.Context, relPath string) (model.Document, error) {
	return model.Document{}, model.ErrNotFound
}
func (f *fakeErrorStore) ListFiles(ctx context.Context, prefix, glob string, limit, offset int) ([]model.Document, int64, error) {
	return nil, 0, nil
}
func (f *fakeErrorStore) Close() error { return nil }

// implement embeddedChunkLister so preloadEmbeddedChunkMetadata will hit our
// injected failure.
func (f *fakeErrorStore) ListEmbeddedChunkMetadata(ctx context.Context, indexKind string, limit, offset int) ([]model.ChunkTask, error) {
	return nil, f.err
}

func TestUpCreatesSecretTokenAndConnectionFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		code := app.RunWithContext(ctx, []string{"up", "--listen", "127.0.0.1:0"})
		if code != 0 {
			t.Fatalf("unexpected exit code: got=%d stderr=%s", code, stderr.String())
		}
	})

	secretTokenPath := filepath.Join(tmp, ".dir2mcp", "secret.token")
	tokenRaw, err := os.ReadFile(secretTokenPath)
	if err != nil {
		t.Fatalf("read secret token: %v", err)
	}
	token := strings.TrimSpace(string(tokenRaw))
	if len(token) != 64 {
		t.Fatalf("unexpected token length: got=%d want=64", len(token))
	}

	tokenInfo, err := os.Stat(secretTokenPath)
	if err != nil {
		t.Fatalf("stat secret token: %v", err)
	}
	if runtime.GOOS != "windows" && tokenInfo.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected token permissions: got=%#o want=%#o", tokenInfo.Mode().Perm(), 0o600)
	}

	connectionPath := filepath.Join(tmp, ".dir2mcp", "connection.json")
	connectionRaw, err := os.ReadFile(connectionPath)
	if err != nil {
		t.Fatalf("read connection.json: %v", err)
	}

	var connection struct {
		Transport string            `json:"transport"`
		URL       string            `json:"url"`
		Headers   map[string]string `json:"headers"`
		Public    bool              `json:"public"`
		Session   struct {
			UsesMCPSessionID     bool   `json:"uses_mcp_session_id"`
			HeaderName           string `json:"header_name"`
			AssignedOnInitialize bool   `json:"assigned_on_initialize"`
		} `json:"session"`
		TokenSource string `json:"token_source"`
		TokenFile   string `json:"token_file"`
	}
	if err := json.Unmarshal(connectionRaw, &connection); err != nil {
		t.Fatalf("unmarshal connection.json: %v", err)
	}

	if connection.Transport != "mcp_streamable_http" {
		t.Fatalf("unexpected transport: %q", connection.Transport)
	}
	if !strings.HasSuffix(connection.URL, protocol.DefaultMCPPath) {
		t.Fatalf("unexpected connection URL: %q", connection.URL)
	}
	if connection.Headers[protocol.MCPProtocolVersionHeader] != config.DefaultProtocolVersion {
		t.Fatalf("unexpected protocol version header: %q", connection.Headers[protocol.MCPProtocolVersionHeader])
	}
	if connection.TokenSource != "secret.token" {
		t.Fatalf("unexpected token_source: %q", connection.TokenSource)
	}
	if connection.TokenFile == "" {
		t.Fatal("expected token_file to be populated")
	}
	if connection.Public {
		t.Fatal("expected public=false by default")
	}
	if !connection.Session.UsesMCPSessionID {
		t.Fatal("expected session.uses_mcp_session_id=true")
	}
	if connection.Session.HeaderName != protocol.MCPSessionHeader {
		t.Fatalf("unexpected session.header_name: %q", connection.Session.HeaderName)
	}
	if !connection.Session.AssignedOnInitialize {
		t.Fatal("expected session.assigned_on_initialize=true")
	}
}

func TestUpSupportsGlobalDirAndStateDirFlags(t *testing.T) {
	tmp := t.TempDir()
	rootDir := filepath.Join(tmp, "workspace")
	stateDir := filepath.Join(tmp, "custom-state")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		code := app.RunWithContext(ctx, []string{
			"--dir", rootDir,
			"--state-dir", stateDir,
			"up",
			"--listen", "127.0.0.1:0",
		})
		if code != 0 {
			t.Fatalf("unexpected exit code: got=%d stderr=%s", code, stderr.String())
		}
	})

	if _, err := os.Stat(filepath.Join(stateDir, "connection.json")); err != nil {
		t.Fatalf("expected connection.json in custom state dir: %v", err)
	}
}

func TestUpTLSRequiresCertAndKeyTogether(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	certPath, _ := writeTestTLSCertPair(t, tmp)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"up", "--tls-cert", certPath})
		if code != 2 {
			t.Fatalf("unexpected exit code: got=%d want=2 stderr=%s", code, stderr.String())
		}
	})

	if !strings.Contains(stderr.String(), "--tls-cert and --tls-key must be provided together") {
		t.Fatalf("expected tls validation error, got: %s", stderr.String())
	}
}

func TestUpTLSConnectionURLUsesHTTPS(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	certPath, keyPath := writeTestTLSCertPair(t, tmp)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		code := app.RunWithContext(ctx, []string{
			"up",
			"--listen", "127.0.0.1:0",
			"--tls-cert", certPath,
			"--tls-key", keyPath,
		})
		if code != 0 {
			t.Fatalf("unexpected exit code: got=%d stderr=%s", code, stderr.String())
		}
	})

	connection := readConnectionPayload(t, filepath.Join(tmp, ".dir2mcp", "connection.json"))
	if !strings.HasPrefix(connection.URL, "https://") {
		t.Fatalf("expected https connection URL when TLS is enabled, got %q", connection.URL)
	}
}

// Up command prints a warning when both a direct facilitator token and a
// token file are supplied; the file takes precedence and the direct flag
// is ignored. This exercises the new CLI warning logic.
func TestUpWarnsAboutFacilitatorTokenConflict(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	// prepare a placeholder token file
	tokenPath := filepath.Join(tmp, "fac.txt")
	if err := os.WriteFile(tokenPath, []byte("filetoken"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		code := app.RunWithContext(ctx, []string{
			"up",
			"--listen", "127.0.0.1:0",
			"--x402-facilitator-token-file", tokenPath,
			"--x402-facilitator-token", "ignored",
		})
		if code != 0 {
			t.Fatalf("unexpected exit code: got=%d stderr=%s", code, stderr.String())
		}
	})

	if !strings.Contains(stderr.String(), "warning: --x402-facilitator-token ignored; using --x402-facilitator-token-file") {
		t.Fatalf("expected warning about token conflict, got stderr: %s", stderr.String())
	}
}

func TestUpNonInteractiveMissingConfigReturnsExitCode2(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	var code int
	withWorkingDir(t, tmp, func() {
		code = app.RunWithContext(context.Background(), []string{"--non-interactive", "up"})
	})

	if code != 2 {
		t.Fatalf("unexpected exit code: got=%d want=2", code)
	}

	errText := stderr.String()
	if !strings.Contains(errText, "CONFIG_INVALID") {
		t.Fatalf("expected CONFIG_INVALID in stderr, got: %s", errText)
	}
	if !strings.Contains(errText, "MISTRAL_API_KEY") {
		t.Fatalf("expected MISTRAL_API_KEY hint in stderr, got: %s", errText)
	}
	if !strings.Contains(errText, "dir2mcp config init") {
		t.Fatalf("expected config init hint in stderr, got: %s", errText)
	}
}

// Reindex command should also load configuration early and surface any
// errors from config.Load.  The config loader only returns an error when
// statting the path fails for some unexpected reason (for example permission
// denied on a dotenv file), so we simulate that by creating an unreadable
// ".env" file in the working directory.
func TestReindexConfigLoadErrorReturnsExitCode2(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file permission semantics differ on Windows")
	}

	tmp := t.TempDir()
	// ensure there is something to upset loadDotEnvFiles
	bad := filepath.Join(tmp, ".env")
	if err := os.WriteFile(bad, []byte("FOO=bar"), 0); err != nil {
		t.Fatalf("write bad env: %v", err)
	}
	if err := os.Chmod(bad, 0); err != nil {
		t.Fatalf("chmod bad env: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	var code int
	withWorkingDir(t, tmp, func() {
		code = app.RunWithContext(context.Background(), []string{"reindex"})
	})

	if code != 2 {
		t.Fatalf("unexpected exit code: got=%d want=2", code)
	}

	if !strings.Contains(stderr.String(), "load config") {
		t.Fatalf("expected load config error in stderr, got: %s", stderr.String())
	}
}

func TestReindexRejectsUnexpectedArguments(t *testing.T) {
	tmp := t.TempDir()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	var code int
	withWorkingDir(t, tmp, func() {
		code = app.RunWithContext(context.Background(), []string{"reindex", "extra"})
	})

	if code != 2 {
		t.Fatalf("unexpected exit code: got=%d want=2", code)
	}
	if !strings.Contains(stderr.String(), "reindex command does not accept arguments") {
		t.Fatalf("expected argument validation error, got: %s", stderr.String())
	}
}

// When a real configuration is available it should be passed through to the
// ingestor factory.  Previously runReindex always used config.Default(),
// causing the ingest service to be unaware of any environment overrides.
func TestReindexPassesConfigToNewIngestor(t *testing.T) {
	tmp := t.TempDir()
	// exercise a non-default value so we can distinguish default vs loaded
	t.Setenv("MISTRAL_API_KEY", "abc123")
	t.Setenv("MISTRAL_BASE_URL", "https://example.local/")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var seenCfg config.Config
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewIngestor: func(cfg config.Config, st model.Store) (model.Ingestor, error) {
			seenCfg = cfg
			// return the failingIngestor defined later in this file
			return failingIngestor{}, nil
		},
	})

	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		code := app.RunWithContext(ctx, []string{"reindex"})
		if code != 0 {
			t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
		}
	})

	if seenCfg.MistralAPIKey != "abc123" {
		t.Fatalf("config not propagated to ingestor: got api key %q", seenCfg.MistralAPIKey)
	}
	if seenCfg.MistralBaseURL != "https://example.local/" {
		t.Fatalf("config not propagated to ingestor: got base url %q", seenCfg.MistralBaseURL)
	}
}

func TestReindexClearsContentHashesBeforeRun(t *testing.T) {
	tmp := t.TempDir()

	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}

	// Seed sqlite with one document that has a content hash.
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init sqlite store: %v", err)
	}
	if err := st.UpsertDocument(context.Background(), model.Document{
		RelPath:     "docs/a.md",
		DocType:     "md",
		SizeBytes:   10,
		MTimeUnix:   1,
		ContentHash: "seeded-hash",
		Status:      "ready",
	}); err != nil {
		t.Fatalf("seed document: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close seeded store: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var hashAtReindexTime string
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewIngestor: func(cfg config.Config, st model.Store) (model.Ingestor, error) {
			return &capturingIngestor{
				store:        st,
				relPath:      "docs/a.md",
				capturedHash: &hashAtReindexTime,
			}, nil
		},
	})

	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		code := app.RunWithContext(ctx, []string{"reindex"})
		if code != 0 {
			t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
		}
	})

	verify := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := verify.Init(context.Background()); err != nil {
		t.Fatalf("init verify store: %v", err)
	}
	defer func() { _ = verify.Close() }()

	doc, err := verify.GetDocumentByPath(context.Background(), "docs/a.md")
	if err != nil {
		t.Fatalf("get seeded document: %v", err)
	}
	if doc.ContentHash != "" {
		t.Fatalf("expected content_hash to be cleared before reindex, got %q", doc.ContentHash)
	}
	if hashAtReindexTime != "" {
		t.Fatalf("expected content_hash to be cleared before ingestor Reindex, got %q", hashAtReindexTime)
	}
}

func TestUpJSONConnectionEventIncludesTokenSourceForFileAuth(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	customTokenPath := filepath.Join(tmp, "external.token")
	if err := os.WriteFile(customTokenPath, []byte("external-token-value\n"), 0o600); err != nil {
		t.Fatalf("write custom token: %v", err)
	}
	customTokenAbs := customTokenPath
	if absPath, err := filepath.Abs(customTokenPath); err == nil {
		customTokenAbs = absPath
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		code := app.RunWithContext(ctx, []string{
			"up",
			"--json",
			"--auth",
			"file:" + customTokenPath,
			"--listen",
			"127.0.0.1:0",
		})
		if code != 0 {
			t.Fatalf("unexpected exit code: got=%d stderr=%s", code, stderr.String())
		}
	})

	lines := scanLines(t, stdout.String())
	if len(lines) == 0 {
		t.Fatal("expected NDJSON output")
	}

	seenEvents := map[string]bool{}
	var connectionData map[string]interface{}
	for _, line := range lines {
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid NDJSON line: %q err=%v", line, err)
		}

		eventName, _ := event["event"].(string)
		seenEvents[eventName] = true

		if eventName == "connection" {
			data, ok := event["data"].(map[string]interface{})
			if !ok {
				t.Fatalf("connection event data has unexpected shape: %#v", event["data"])
			}
			connectionData = data
		}
	}

	for _, required := range []string{"index_loaded", "server_started", "connection", "scan_progress", "embed_progress"} {
		if !seenEvents[required] {
			t.Fatalf("missing required event: %s", required)
		}
	}

	if connectionData == nil {
		t.Fatal("missing connection event payload")
	}
	if connectionData["token_source"] != "file" {
		t.Fatalf("unexpected connection token_source: %#v", connectionData["token_source"])
	}
	if connectionData["token_file"] != customTokenAbs {
		t.Fatalf("unexpected connection token_file: got=%#v want=%#v", connectionData["token_file"], customTokenAbs)
	}
}

func TestUpJSONPreloadErrorEmitsWarningEvent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewStore: func(cfg config.Config) model.Store {
			return &fakeErrorStore{err: errors.New("boom")}
		},
	})

	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		code := app.RunWithContext(ctx, []string{"up", "--json", "--listen", "127.0.0.1:0"})
		if code != 0 {
			t.Fatalf("unexpected exit code: got=%d stderr=%s", code, stderr.String())
		}
	})

	lines := scanLines(t, stdout.String())
	found := false
	for _, line := range lines {
		var ev map[string]interface{}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev["event"] == "bootstrap_embedded_chunk_metadata" {
			found = true
			data, _ := ev["data"].(map[string]interface{})
			if data["message"] != "boom" {
				t.Fatalf("unexpected message: %#v", data["message"])
			}
		}
	}
	if !found {
		t.Fatal("expected bootstrap_embedded_chunk_metadata event")
	}
}

func TestUpReturnsExitCode4OnBindFailure(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve listener: %v", err)
	}
	defer func() {
		_ = listener.Close()
	}()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	var code int
	withWorkingDir(t, tmp, func() {
		code = app.RunWithContext(context.Background(), []string{
			"up",
			"--listen",
			listener.Addr().String(),
		})
	})

	if code != 4 {
		t.Fatalf("unexpected exit code: got=%d want=4 stderr=%s", code, stderr.String())
	}
}

func TestUpReturnsExitCode3OnIngestionFatal(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewIngestor: func(cfg config.Config, st model.Store) (model.Ingestor, error) {
			return failingIngestor{}, nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var code int
	withWorkingDir(t, tmp, func() {
		code = app.RunWithContext(ctx, []string{
			"up",
			"--listen",
			"127.0.0.1:0",
			"--json",
		})
	})

	if code != 3 {
		t.Fatalf("unexpected exit code: got=%d want=3 stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "ingestion failed") {
		t.Fatalf("expected ingestion error in stderr, got: %s", stderr.String())
	}
}

func TestUpDefaultListenStaysLoopbackWhenNotPublic(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		code := app.RunWithContext(ctx, []string{"up"})
		if code != 0 {
			t.Fatalf("unexpected exit code: got=%d stderr=%s", code, stderr.String())
		}
	})

	connection := readConnectionPayload(t, filepath.Join(tmp, ".dir2mcp", "connection.json"))
	host := connectionHost(t, connection.URL)
	if host != "127.0.0.1" {
		t.Fatalf("expected loopback host for default mode, got %q (url=%s)", host, connection.URL)
	}
	if connection.Public {
		t.Fatalf("expected connection.public=false by default, got true")
	}
}

func TestUpPublicWithoutListenBindsAllInterfaces(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		code := app.RunWithContext(ctx, []string{"up", "--public"})
		if code != 0 {
			t.Fatalf("unexpected exit code: got=%d stderr=%s", code, stderr.String())
		}
	})

	connection := readConnectionPayload(t, filepath.Join(tmp, ".dir2mcp", "connection.json"))
	host := connectionHost(t, connection.URL)
	if host != "0.0.0.0" {
		t.Fatalf("expected 0.0.0.0 host in public mode, got %q (url=%s)", host, connection.URL)
	}
	if !connection.Public {
		t.Fatalf("expected connection.public=true in public mode, got false")
	}
}

func TestUpPublicAuthNoneFailsWithoutForceInsecure(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	var code int
	withWorkingDir(t, tmp, func() {
		code = app.RunWithContext(context.Background(), []string{"up", "--public", "--auth", "none"})
	})

	if code != 2 {
		t.Fatalf("unexpected exit code: got=%d want=2 stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--public requires auth") {
		t.Fatalf("expected public auth guardrail message, got: %s", stderr.String())
	}
}

func TestUpPublicAuthNoneWithWhitespaceFailsWithoutForceInsecure(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	var code int
	withWorkingDir(t, tmp, func() {
		code = app.RunWithContext(context.Background(), []string{"up", "--public", "--auth", " none "})
	})

	if code != 2 {
		t.Fatalf("unexpected exit code: got=%d want=2 stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--public requires auth") {
		t.Fatalf("expected public auth guardrail message, got: %s", stderr.String())
	}
}

func TestUpPublicAuthNoneAllowedWithForceInsecure(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		code := app.RunWithContext(ctx, []string{"up", "--public", "--auth", "none", "--force-insecure", "--json"})
		if code != 0 {
			t.Fatalf("unexpected exit code: got=%d stderr=%s", code, stderr.String())
		}
	})
}

func TestUpX402RequiredMissingFieldsFailsFast(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	// ensure previous tests don't leak auth token
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	var code int
	withWorkingDir(t, tmp, func() {
		code = app.RunWithContext(context.Background(), []string{
			"up",
			"--x402", "required",
			"--x402-resource-base-url", "https://resource.example.com",
			"--x402-network", "solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d",
			"--x402-asset", "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
			"--x402-pay-to", "8N5A4rQU8vJrQmH3iiA7kE4m1df4WeyueXQqGb4G9tTj",
			// Intentionally missing --x402-facilitator-url.
		})
	})

	if code != 5 {
		t.Fatalf("unexpected exit code: got=%d want=5 stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "x402 facilitator URL is required") {
		t.Fatalf("expected x402 validation error, got: %s", stderr.String())
	}
}

func TestUpX402OnAllowsMissingFields(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	// ensure no leftover auth token influences behaviour
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		code := app.RunWithContext(ctx, []string{
			"up",
			"--x402", "on",
			"--listen", "127.0.0.1:0",
			"--json",
		})
		if code != 0 {
			t.Fatalf("unexpected exit code: got=%d stderr=%s", code, stderr.String())
		}
	})

	// verify server_started event in NDJSON output
	lines := scanLines(t, stdout.String())
	if len(lines) == 0 {
		t.Fatal("expected NDJSON output")
	}

	found := false
	for _, line := range lines {
		var ev map[string]interface{}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("invalid NDJSON line: %q err=%v", line, err)
		}
		if ev["event"] != "server_started" {
			continue
		}
		found = true
		// break early once we locate the desired event
		break
	}
	if !found {
		t.Fatal("missing server_started event")
	}
}

func TestUpPublicRespectsExplicitListen(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		code := app.RunWithContext(ctx, []string{"up", "--public", "--listen", "127.0.0.1:0"})
		if code != 0 {
			t.Fatalf("unexpected exit code: got=%d stderr=%s", code, stderr.String())
		}
	})

	connection := readConnectionPayload(t, filepath.Join(tmp, ".dir2mcp", "connection.json"))
	host := connectionHost(t, connection.URL)
	if host != "127.0.0.1" {
		t.Fatalf("expected explicit listen host to be preserved, got %q (url=%s)", host, connection.URL)
	}
	if !connection.Public {
		t.Fatalf("expected connection.public=true when --public is set")
	}
}

func TestUpPublicNDJSONServerStartedIncludesPublicField(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		code := app.RunWithContext(ctx, []string{"up", "--public", "--json", "--read-only"})
		if code != 0 {
			t.Fatalf("unexpected exit code: got=%d stderr=%s", code, stderr.String())
		}
	})

	lines := scanLines(t, stdout.String())
	if len(lines) == 0 {
		t.Fatal("expected NDJSON output")
	}

	foundServerStarted := false
	for _, line := range lines {
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid NDJSON line: %q err=%v", line, err)
		}
		if event["event"] != "server_started" {
			continue
		}
		foundServerStarted = true
		data, ok := event["data"].(map[string]interface{})
		if !ok {
			t.Fatalf("server_started event data has unexpected shape: %#v", event["data"])
		}
		publicValue, ok := data["public"].(bool)
		if !ok {
			t.Fatalf("expected server_started.data.public bool, got %#v", data["public"])
		}
		if !publicValue {
			t.Fatalf("expected server_started.data.public=true, got false")
		}
	}

	if !foundServerStarted {
		t.Fatal("missing server_started event")
	}
}

type connectionFilePayload struct {
	URL    string `json:"url"`
	Public bool   `json:"public"`
}

func writeTestTLSCertPair(t *testing.T, dir string) (string, string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()),
		Subject: pkix.Name{
			CommonName: "127.0.0.1",
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	return certPath, keyPath
}

func readConnectionPayload(t *testing.T, path string) connectionFilePayload {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read connection file: %v", err)
	}

	var payload connectionFilePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal connection file: %v", err)
	}
	return payload
}

func connectionHost(t *testing.T, rawURL string) string {
	t.Helper()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse connection URL: %v", err)
	}
	return parsed.Hostname()
}

func scanLines(t *testing.T, text string) []string {
	t.Helper()

	scanner := bufio.NewScanner(strings.NewReader(text))
	lines := make([]string, 0)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan output: %v", err)
	}
	return lines
}

func withWorkingDir(t *testing.T, dir string, fn func()) {
	t.Helper()

	cwdMu.Lock()
	defer cwdMu.Unlock()

	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() {
		if err := os.Chdir(original); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	}()

	fn()
}

type failingIngestor struct{}

func (f failingIngestor) Run(ctx context.Context) error {
	_ = ctx
	return errors.New("forced ingest failure")
}

func (f failingIngestor) Reindex(ctx context.Context) error {
	_ = ctx
	return nil
}

type capturingIngestor struct {
	store        model.Store
	relPath      string
	capturedHash *string
}

func (c *capturingIngestor) Run(ctx context.Context) error {
	_ = ctx
	return nil
}

func (c *capturingIngestor) Reindex(ctx context.Context) error {
	// misconfigured tests should fail early rather than silently succeed
	if c.store == nil || c.capturedHash == nil {
		return fmt.Errorf("capturingIngestor: missing store or capturedHash")
	}
	doc, err := c.store.GetDocumentByPath(ctx, c.relPath)
	if err != nil {
		return err
	}
	*c.capturedHash = doc.ContentHash
	return nil
}

// sanity check: misconfigured ingestor should surface an error rather than
// silently succeeding.
func TestCapturingIngestorReindexErrorOnMissingConfig(t *testing.T) {
	var ci capturingIngestor
	if err := ci.Reindex(context.Background()); err == nil {
		t.Fatal("expected error when store and capturedHash are nil")
	}
}
