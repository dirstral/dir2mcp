package tests

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestCorePath_ForegroundUpAskDown drives the core user path against a real
// dir2mcp binary on every OS, Windows included:
//
//   - `up --foreground` indexes a folder with nested directories,
//   - `status` reports the index,
//   - `list-files` and `ask` return forward-slash rel_paths with citations,
//   - `open-file` reads a nested file through its forward-slash rel_path,
//   - `down` stops the server from a second process and clears the pid file.
//
// The embed and chat provider is a local fake OpenAI-compatible server, so the
// test needs no network and no credentials. -short skips it, because it builds
// the binary.
func TestCorePath_ForegroundUpAskDown(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the dir2mcp binary; skipped with -short")
	}
	fake := newFakeOpenAIServer(t)
	bin := buildDir2mcpBinaryPortable(t)
	root := t.TempDir()
	writeCorePathCorpus(t, root, fake.URL+"/v1")
	env, home := corePathEnv(t)
	stateDir := filepath.Join(root, ".dir2mcp")

	upCmd := startForegroundUp(t, bin, root, env)
	waitForCorePathReady(t, stateDir, upCmd)
	waitForEmbedded(t, bin, root, env)

	assertListFilesForwardSlash(t, bin, root, env)
	assertAskCitesNestedFile(t, bin, root, env)
	assertOpenFileNested(t, bin, root, env)
	assertInstallClaudeDefaultPath(t, bin, root, env, home)
	assertDownStopsServer(t, bin, root, env, stateDir, upCmd)
	if fake.chatCalls.Load() == 0 {
		t.Error("ask never reached the chat provider")
	}
}

// fakeOpenAIServer answers the two OpenAI-compatible routes dir2mcp calls:
// embeddings and chat completions.
type fakeOpenAIServer struct {
	*httptest.Server
	chatCalls atomic.Int64
}

func newFakeOpenAIServer(t *testing.T) *fakeOpenAIServer {
	t.Helper()
	f := &fakeOpenAIServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		data := make([]map[string]any, len(req.Input))
		for i, in := range req.Input {
			data[i] = map[string]any{"index": i, "embedding": fakeVector(in)}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		f.chatCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"The policy grants twenty days of paid leave [1]."}}]}`))
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// fakeVector returns a deterministic, non-zero 8-dimension vector for s.
func fakeVector(s string) []float64 {
	sum := sha256.Sum256([]byte(s))
	vec := make([]float64, 8)
	for i := range vec {
		vec[i] = float64(sum[i]%97+1) / 100
	}
	return vec
}

func writeCorePathCorpus(t *testing.T, root, baseURL string) {
	t.Helper()
	files := map[string]string{
		filepath.Join("docs", "sub", "policy.md"): "# Leave policy\n\nThe leave policy grants twenty days of paid leave each year.\n",
		"top.txt": "A plain note at the top of the corpus.\n",
	}
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	cfg := fmt.Sprintf(`providers:
  fake:
    kind: openai
    base_url: %s
    api_key: test-key
    embed_text_model: fake-embed
    embed_code_model: fake-embed
    chat_model: fake-chat
model:
  embed:
    provider: fake
  chat:
    provider: fake
`, baseURL)
	if err := os.WriteFile(filepath.Join(root, ".dir2mcp.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// corePathEnv isolates the binary from the developer's own home, config and
// provider credentials, so only the fake provider can resolve.
func corePathEnv(t *testing.T) (env []string, home string) {
	t.Helper()
	home = t.TempDir()
	drop := map[string]bool{}
	for _, k := range []string{
		"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME",
		"MISTRAL_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY",
		"COHERE_API_KEY", "ELEVENLABS_API_KEY", "DIR2MCP_AUTH_TOKEN",
	} {
		drop[k] = true
	}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if drop[strings.ToUpper(k)] {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"HOME="+home,
		"USERPROFILE="+home,
		"APPDATA="+filepath.Join(home, "AppData", "Roaming"),
		"LOCALAPPDATA="+filepath.Join(home, "AppData", "Local"),
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
	)
	return env, home
}

func buildDir2mcpBinaryPortable(t *testing.T) string {
	t.Helper()
	name := "dir2mcp"
	if isWindows() {
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/dir2mcp")
	cmd.Dir = filepath.Join(wd, "..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build dir2mcp: %v\n%s", err, out)
	}
	return bin
}

// upProcess is the running foreground server plus a channel that closes when
// it exits.
type upProcess struct {
	cmd    *exec.Cmd
	log    *lockedWriter
	exited chan struct{}
}

// lockedWriter is a goroutine-safe buffer for the server output. The exec
// package copies stdout and stderr from separate goroutines.
type lockedWriter struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *lockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func isWindows() bool { return runtime.GOOS == "windows" }

func startForegroundUp(t *testing.T, bin, root string, env []string) *upProcess {
	t.Helper()
	cmd := exec.Command(bin, "up", "--foreground", "--non-interactive", "--listen", "127.0.0.1:0")
	cmd.Dir = root
	cmd.Env = env
	log := &lockedWriter{}
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatalf("start up: %v", err)
	}
	p := &upProcess{cmd: cmd, log: log, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-p.exited:
		default:
			_ = cmd.Process.Kill()
			<-p.exited
		}
	})
	return p
}

func waitForCorePathReady(t *testing.T, stateDir string, p *upProcess) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-p.exited:
			t.Fatalf("up exited before it became ready:\n%s", p.log)
		default:
		}
		raw, err := os.ReadFile(filepath.Join(stateDir, "connection.json"))
		_, pidErr := os.Stat(filepath.Join(stateDir, "server.pid"))
		if err == nil && pidErr == nil {
			var conn struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(raw, &conn) == nil && conn.URL != "" {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("up did not become ready in time:\n%s", p.log)
}

// runDir2mcp runs one CLI command to completion and returns its stdout.
func runDir2mcp(t *testing.T, bin, root string, env []string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = root
	cmd.Env = env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("%v: stderr=%s", err, stderr.String())
	}
	return string(out), nil
}

func waitForEmbedded(t *testing.T, bin, root string, env []string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		out, err := runDir2mcp(t, bin, root, env, "--json", "status")
		last = out
		if err == nil {
			var st struct {
				Snapshot struct {
					Indexing struct {
						Running         bool  `json:"running"`
						Indexed         int64 `json:"indexed"`
						EmbeddedOK      int64 `json:"embedded_ok"`
						EmbeddedPending int64 `json:"embedded_pending"`
						Errors          int64 `json:"errors"`
					} `json:"indexing"`
				} `json:"snapshot"`
			}
			decodeErr := json.Unmarshal([]byte(out), &st)
			ix := st.Snapshot.Indexing
			if decodeErr == nil && ix.Indexed >= 2 &&
				ix.EmbeddedOK > 0 && ix.EmbeddedPending == 0 {
				if ix.Errors != 0 {
					t.Fatalf("status reports %d errors: %s", ix.Errors, out)
				}
				return
			}
		} else {
			last = err.Error()
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("index did not finish embedding in time; last status: %s", last)
}

func assertListFilesForwardSlash(t *testing.T, bin, root string, env []string) {
	t.Helper()
	out, err := runDir2mcp(t, bin, root, env, "--json", "list-files")
	if err != nil {
		t.Fatalf("list-files: %v", err)
	}
	if !strings.Contains(out, `"docs/sub/policy.md"`) {
		t.Fatalf("list-files must report the nested file as docs/sub/policy.md, got: %s", out)
	}
	if strings.Contains(out, `\\`) {
		t.Fatalf("list-files output holds a backslash path: %s", out)
	}
}

func assertAskCitesNestedFile(t *testing.T, bin, root string, env []string) {
	t.Helper()
	out, err := runDir2mcp(t, bin, root, env, "--json", "ask", "What does the leave policy grant?")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	var payload struct {
		Answer    string `json:"answer"`
		Citations []struct {
			RelPath string `json:"rel_path"`
		} `json:"citations"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode ask output: %v\n%s", err, out)
	}
	if strings.TrimSpace(payload.Answer) == "" {
		t.Fatalf("ask returned no answer: %s", out)
	}
	found := false
	for _, c := range payload.Citations {
		if strings.Contains(c.RelPath, `\`) {
			t.Fatalf("citation rel_path holds a backslash: %q", c.RelPath)
		}
		if c.RelPath == "docs/sub/policy.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ask must cite docs/sub/policy.md, got: %s", out)
	}
}

func assertOpenFileNested(t *testing.T, bin, root string, env []string) {
	t.Helper()
	out, err := runDir2mcp(t, bin, root, env, "open-file", "docs/sub/policy.md")
	if err != nil {
		t.Fatalf("open-file: %v", err)
	}
	if !strings.Contains(out, "twenty days of paid leave") {
		t.Fatalf("open-file did not return the file text: %s", out)
	}
}

// assertInstallClaudeDefaultPath runs `install claude` with no --config-path
// and checks that the entry lands at the platform default path: %APPDATA%\Claude
// on Windows, ~/Library/Application Support/Claude elsewhere. The command
// needs bunx or npx on PATH; without them the test reports the gap and skips
// only this step.
func assertInstallClaudeDefaultPath(t *testing.T, bin, root string, env []string, home string) {
	t.Helper()
	if _, err := exec.LookPath("bunx"); err != nil {
		if _, err := exec.LookPath("npx"); err != nil {
			t.Log("install claude step not run: neither bunx nor npx is on PATH")
			return
		}
	}
	want := filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
	if isWindows() {
		want = filepath.Join(home, "AppData", "Roaming", "Claude", "claude_desktop_config.json")
	}
	out, err := runDir2mcp(t, bin, root, env, "--json", "install", "claude", "--name", "corepath")
	if err != nil {
		t.Fatalf("install claude: %v", err)
	}
	var payload struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode install claude output: %v\n%s", err, out)
	}
	if payload.Path != want {
		t.Fatalf("install claude wrote %q, want %q", payload.Path, want)
	}
	raw, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read Claude config: %v", err)
	}
	if !strings.Contains(string(raw), `"corepath"`) {
		t.Fatalf("Claude config lacks the corepath entry: %s", raw)
	}
	t.Logf("install claude wrote %s", want)
}

func assertDownStopsServer(t *testing.T, bin, root string, env []string, stateDir string, p *upProcess) {
	t.Helper()
	out, err := runDir2mcp(t, bin, root, env, "down")
	if err != nil {
		t.Fatalf("down: %v", err)
	}
	if !strings.Contains(out, "stopped") {
		t.Fatalf("down must report stopped, got: %s", out)
	}
	select {
	case <-p.exited:
	case <-time.After(20 * time.Second):
		t.Fatalf("server still runs after down:\n%s", p.log)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "server.pid")); !os.IsNotExist(err) {
		t.Fatalf("pid file must be gone after down; stat err=%v", err)
	}
	out, err = runDir2mcp(t, bin, root, env, "down")
	if err != nil {
		t.Fatalf("second down: %v", err)
	}
	if !strings.Contains(out, "no dir2mcp daemon registered") {
		t.Fatalf("second down must report no server, got: %s", out)
	}
}
