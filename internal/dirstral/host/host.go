package host

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"dir2mcp/internal/buildinfo"
	coreconfig "dir2mcp/internal/config"
	"dir2mcp/internal/dirstral/config"
	"dir2mcp/internal/dirstral/ui"
	"dir2mcp/internal/protocol"
)

type State struct {
	PID       int      `json:"pid"`
	StartedAt string   `json:"started_at"`
	Command   []string `json:"command"`
	WorkDir   string   `json:"workdir"`
	RootDir   string   `json:"root_dir,omitempty"`
	MCPURL    string   `json:"mcp_url"`
}

type UpOptions struct {
	Dir     string
	Listen  string
	MCPPath string
	Port    int
	JSON    bool
}

var errUnhealthy = errors.New("mcp server not ready")

const (
	defaultEndpointCaptureTimeout      = 20 * time.Second
	defaultEndpointCapturePollInterval = 150 * time.Millisecond
)

func Up(ctx context.Context, opts UpOptions) error {
	baseCommand, baseArgs, workDir, err := resolveDir2MCPCommand()
	if err != nil {
		return err
	}

	args := append([]string{}, baseArgs...)
	args = append(args, "up")
	if opts.Dir != "" {
		args = append(args, "--dir", opts.Dir)
	}
	listen := strings.TrimSpace(opts.Listen)
	if listen == "" && opts.Port > 0 {
		listen = fmt.Sprintf("127.0.0.1:%d", opts.Port)
	}
	if listen != "" {
		args = append(args, "--listen", listen)
	}
	if opts.MCPPath != "" {
		args = append(args, "--mcp-path", opts.MCPPath)
	}
	if opts.JSON {
		args = append(args, "--json")
	}

	effectiveListen := effectiveListen(opts.Listen, opts.Port)
	effectivePath := normalizeMCPPath(opts.MCPPath)
	derivedURL, deterministic := ComputeMCPURL(effectiveListen, effectivePath)

	cmd := exec.CommandContext(ctx, baseCommand, args...)
	if workDir != "" {
		cmd.Dir = workDir
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	state := State{
		PID:       cmd.Process.Pid,
		StartedAt: time.Now().Format(time.RFC3339),
		Command:   append([]string{baseCommand}, args...),
		WorkDir:   workDir,
		RootDir:   resolveRootDir(opts.Dir, cmd.Dir),
		MCPURL:    derivedURL,
	}
	if err := SaveState(state); err != nil {
		return errors.Join(err, stopStartedCommand(cmd, stdout, stderr))
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	stopCapture := make(chan struct{})
	if !deterministic {
		go captureEndpoint(stopCapture, state.PID, state.RootDir, defaultEndpointCaptureTimeout, defaultEndpointCapturePollInterval)
	}

	go streamLogs(stdout, "")
	go streamLogs(stderr, "[dir2mcp] ")

	fmt.Println(ui.Info("mcp server:", fmt.Sprintf("started dir2mcp (pid=%d). Press Ctrl+C to stop.", cmd.Process.Pid)))

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case err := <-done:
		close(stopCapture)
		_ = ClearState()
		if err != nil {
			return fmt.Errorf("dir2mcp exited: %w", err)
		}
		fmt.Println(ui.Info("mcp server:", "dir2mcp stopped"))
		return nil
	case <-sigCh:
		close(stopCapture)
		fmt.Println("\n" + ui.Info("mcp server:", "shutting down dir2mcp..."))
		if err := terminateProcess(cmd.Process.Pid); err != nil {
			return err
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
		_ = ClearState()
		fmt.Println(ui.Info("mcp server:", "stopped"))
		return nil
	}
}

// LogPath returns the path to the MCP server log file.
func LogPath() string {
	return filepath.Join(os.TempDir(), "dirstral-mcp-server.log")
}

// UpDetached starts dir2mcp as a managed background process and returns immediately.
func UpDetached(ctx context.Context, opts UpOptions) error {
	baseCommand, baseArgs, workDir, err := resolveDir2MCPCommand()
	if err != nil {
		return err
	}

	args := append([]string{}, baseArgs...)
	args = append(args, "up")
	if opts.Dir != "" {
		args = append(args, "--dir", opts.Dir)
	}
	listen := strings.TrimSpace(opts.Listen)
	if listen == "" && opts.Port > 0 {
		listen = fmt.Sprintf("127.0.0.1:%d", opts.Port)
	}
	if listen != "" {
		args = append(args, "--listen", listen)
	}
	if opts.MCPPath != "" {
		args = append(args, "--mcp-path", opts.MCPPath)
	}
	if opts.JSON {
		args = append(args, "--json")
	}

	effectiveListen := effectiveListen(opts.Listen, opts.Port)
	effectivePath := normalizeMCPPath(opts.MCPPath)
	derivedURL, deterministic := ComputeMCPURL(effectiveListen, effectivePath)

	cmd := exec.CommandContext(ctx, baseCommand, args...)
	if workDir != "" {
		cmd.Dir = workDir
	}

	logFile, err := os.OpenFile(LogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return err
	}

	state := State{
		PID:       cmd.Process.Pid,
		StartedAt: time.Now().Format(time.RFC3339),
		Command:   append([]string{baseCommand}, args...),
		WorkDir:   workDir,
		RootDir:   resolveRootDir(opts.Dir, cmd.Dir),
		MCPURL:    derivedURL,
	}
	if err := SaveState(state); err != nil {
		cleanupErr := stopStartedCommand(cmd)
		_ = logFile.Close()
		if cleanupErr != nil {
			return errors.Join(err, cleanupErr)
		}
		return err
	}

	go func() {
		_ = cmd.Wait()
		_ = logFile.Close()
	}()

	if !deterministic {
		// captureEndpoint exits when one of the following happens:
		// - connection.json appears with a valid endpoint
		// - the managed process exits
		// - the capture timeout elapses (prevents long-lived goroutine leaks)
		go captureEndpoint(make(chan struct{}), state.PID, state.RootDir, defaultEndpointCaptureTimeout, defaultEndpointCapturePollInterval)
	}

	return nil
}

func Status() error {
	health := CheckHealth()
	if !health.Found {
		fmt.Println(ui.Dim("mcp server: no managed dir2mcp process"))
		return fmt.Errorf("%w: no managed process", errUnhealthy)
	}

	alive := health.Alive
	aliveStr := ui.Red.Render("false")
	if alive {
		aliveStr = ui.Green.Render("true")
	}
	fmt.Printf("%s pid=%d alive=%s\n", ui.Brand.Render("mcp server:"), health.PID, aliveStr)

	safeEndpoint := sanitizeEndpointForLog(health.MCPURL)
	if safeEndpoint == "" {
		fmt.Printf("%s endpoint=%s reachable=%s mcp_ready=%s\n", ui.Brand.Render("mcp server:"), ui.Dim("unknown"), ui.Red.Render("false"), ui.Red.Render("false"))
		return fmt.Errorf("%w: endpoint unknown", errUnhealthy)
	}

	reachStr := ui.Red.Render("false")
	if health.Reachable {
		reachStr = ui.Green.Render("true")
	}
	readyStr := ui.Red.Render("false")
	if health.MCPReady {
		readyStr = ui.Green.Render("true")
	}
	fmt.Printf("%s endpoint=%s reachable=%s mcp_ready=%s\n", ui.Brand.Render("mcp server:"), ui.Cyan.Render(safeEndpoint), reachStr, readyStr)
	fmt.Printf("%s protocol=%s session_header=%s\n", ui.Brand.Render("mcp server:"), ui.Cyan.Render(OrUnknown(health.ProtocolHeader)), ui.Cyan.Render(OrUnknown(health.SessionHeaderName)))
	fmt.Printf("%s auth_source=%s\n", ui.Brand.Render("mcp server:"), ui.Cyan.Render(OrUnknown(health.AuthSourceType)))
	if health.AuthDiagnostic != "" {
		fmt.Printf("%s auth_diagnostic=%s\n", ui.Brand.Render("mcp server:"), ui.Yellow.Render(health.AuthDiagnostic))
	}
	if health.LastError != "" {
		fmt.Printf("%s detail=%s\n", ui.Brand.Render("mcp server:"), ui.Dim(health.LastError))
	}

	if !health.Ready {
		if health.LastError != "" {
			return fmt.Errorf("%w: %s", errUnhealthy, health.LastError)
		}
		if health.AuthDiagnostic != "" {
			return fmt.Errorf("%w: %s", errUnhealthy, health.AuthDiagnostic)
		}
		return fmt.Errorf("%w", errUnhealthy)
	}
	return nil
}

func StatusRemote(ctx context.Context, endpoint string) error {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return fmt.Errorf("%w: remote endpoint is empty", errUnhealthy)
	}
	safeEndpoint := sanitizeEndpointForLog(endpoint)

	reachable := endpointReachable(endpoint)
	mcpReady := false
	lastErr := ""
	if reachable {
		token := strings.TrimSpace(os.Getenv("DIR2MCP_AUTH_TOKEN"))
		mcpReady, lastErr = probeMCPReady(ctx, endpoint, token)
	} else {
		lastErr = "endpoint not reachable"
	}

	reachStr := ui.Red.Render("false")
	if reachable {
		reachStr = ui.Green.Render("true")
	}
	readyStr := ui.Red.Render("false")
	if mcpReady {
		readyStr = ui.Green.Render("true")
	}
	fmt.Printf("%s endpoint=%s reachable=%s mcp_ready=%s\n", ui.Brand.Render("mcp server(remote):"), ui.Cyan.Render(safeEndpoint), reachStr, readyStr)
	if lastErr != "" {
		fmt.Printf("%s detail=%s\n", ui.Brand.Render("mcp server(remote):"), ui.Dim(lastErr))
	}

	if !reachable || !mcpReady {
		if lastErr != "" {
			return fmt.Errorf("%w: %s", errUnhealthy, lastErr)
		}
		return fmt.Errorf("%w", errUnhealthy)
	}
	return nil
}

func Down() error {
	state, err := LoadState()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Println(ui.Dim("mcp server: nothing to stop"))
			return nil
		}
		return err
	}
	if !processAlive(state.PID) {
		_ = ClearState()
		fmt.Println(ui.Dim("mcp server: process already stopped"))
		return nil
	}
	if err := terminateProcess(state.PID); err != nil {
		return err
	}
	_ = ClearState()
	fmt.Println(ui.Info("mcp server:", fmt.Sprintf("stopped pid=%d", state.PID)))
	return nil
}

func sanitizeEndpointForLog(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return trimmed
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func SaveState(state State) error {
	path, err := statePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func LoadState() (State, error) {
	path, err := statePath()
	if err != nil {
		return State{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, err
	}
	return s, nil
}

func ClearState() error {
	path, err := statePath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func statePath() (string, error) {
	dir, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "host_state.json"), nil
}

func streamLogs(r io.ReadCloser, prefix string) {
	defer func() {
		_ = r.Close()
	}()
	s := bufio.NewScanner(r)
	for s.Scan() {
		fmt.Println(prefix + s.Text())
	}
}

func resolveDir2MCPCommand() (command string, args []string, workDir string, err error) {
	if path, lookErr := exec.LookPath("dir2mcp"); lookErr == nil {
		return path, nil, "", nil
	}
	cwd, cwdErr := os.Getwd()
	if cwdErr != nil {
		return "", nil, "", cwdErr
	}
	tryDirs := []struct {
		probeDir string
		runDir   string
	}{
		{probeDir: filepath.Join(cwd, "cmd", "dir2mcp"), runDir: cwd},
		{probeDir: filepath.Join(cwd, "..", "cmd", "dir2mcp"), runDir: filepath.Clean(filepath.Join(cwd, ".."))},
		{probeDir: filepath.Join(cwd, "dir2mcp"), runDir: filepath.Join(cwd, "dir2mcp")},
		{probeDir: filepath.Join(cwd, "..", "dir2mcp"), runDir: filepath.Join(cwd, "..", "dir2mcp")},
	}
	for _, d := range tryDirs {
		if st, statErr := os.Stat(d.probeDir); statErr == nil && st.IsDir() {
			return "go", []string{"run", "./cmd/dir2mcp"}, d.runDir, nil
		}
	}
	return "", nil, "", fmt.Errorf("could not locate dir2mcp binary or source directory")
}

func stopStartedCommand(cmd *exec.Cmd, pipes ...io.Closer) error {
	if cmd == nil {
		return nil
	}
	for _, pipe := range pipes {
		if pipe == nil {
			continue
		}
		_ = pipe.Close()
	}
	if cmd.Process == nil {
		return nil
	}
	killErr := cmd.Process.Kill()
	waitErr := cmd.Wait()
	if killErr == nil && waitErr == nil {
		return nil
	}
	if killErr != nil && errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil
	}
	if killErr == nil {
		return waitErr
	}
	if waitErr == nil {
		return killErr
	}
	return errors.Join(killErr, waitErr)
}

func terminateProcess(pid int) error {
	if pid <= 0 {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		if waitForProcessExit(pid, 3*time.Second) {
			return nil
		}
		return fmt.Errorf("process %d did not exit after kill", pid)
	}
	if err := proc.Signal(syscall.SIGINT); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	if waitForProcessExit(pid, 4*time.Second) {
		return nil
	}
	if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	if waitForProcessExit(pid, 2*time.Second) {
		return nil
	}
	return fmt.Errorf("process %d did not terminate within timeout", pid)
}

func waitForProcessExit(pid int, timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !processAlive(pid)
}

// HealthInfo describes the current state of a managed dir2mcp process.
type HealthInfo struct {
	Found             bool   // true if a state file exists
	PID               int    // process ID from state file
	Alive             bool   // true if the process is running
	MCPURL            string // MCP endpoint URL from state
	Reachable         bool   // true if the MCP endpoint is reachable over TCP
	MCPReady          bool   // true if initialize + tools/list succeed
	Ready             bool   // true if alive, reachable, and MCPReady
	ProtocolHeader    string // configured MCP protocol header value
	SessionHeaderName string // configured session header name
	AuthSourceType    string // configured auth source type
	AuthDiagnostic    string // diagnostic message for auth source
	LastError         string // last readiness probe error (if any)
}

// CheckHealth returns the current health of the managed dir2mcp process.
func CheckHealth() HealthInfo {
	state, err := LoadState()
	if err != nil {
		return HealthInfo{}
	}
	alive := processAlive(state.PID)
	reachable := false
	mcpReady := false
	lastErr := ""
	protocolHeader, sessionHeaderName, authSourceType := readConnectionContractDetails(state)

	token := resolveProbeToken(state)
	authSourceType = strings.ToLower(strings.TrimSpace(authSourceType))
	authDiagnostic := ""
	if authSourceType != "" && authSourceType != "none" && authSourceType != "unknown" {
		if !isSupportedAuthSource(authSourceType) {
			authDiagnostic = "invalid/unknown auth source type: " + authSourceType
		} else if token == "" {
			authDiagnostic = "missing required auth token for contract type: " + authSourceType
		}
	}

	if state.MCPURL != "" {
		reachable = endpointReachable(state.MCPURL)
		if reachable {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			mcpReady, lastErr = probeMCPReady(ctx, state.MCPURL, token)
		} else {
			lastErr = "endpoint not reachable"
		}
	} else {
		lastErr = "endpoint not available in state"
	}
	ready := alive && reachable && mcpReady
	if !alive && lastErr == "" {
		lastErr = "process not alive"
	}
	return HealthInfo{
		Found:             true,
		PID:               state.PID,
		Alive:             alive,
		MCPURL:            state.MCPURL,
		Reachable:         reachable,
		MCPReady:          mcpReady,
		Ready:             ready,
		ProtocolHeader:    protocolHeader,
		SessionHeaderName: sessionHeaderName,
		AuthSourceType:    authSourceType,
		AuthDiagnostic:    authDiagnostic,
		LastError:         lastErr,
	}
}

func OrUnknown(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "unknown"
	}
	return trimmed
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if runtime.GOOS == "windows" {
		return processAliveWindows(pid)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func endpointReachable(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if strings.TrimSpace(u.Host) == "" {
		u, err = url.Parse("http://" + raw)
		if err != nil {
			return false
		}
	}
	hostname := strings.TrimSpace(u.Hostname())
	if hostname == "" {
		return false
	}
	port := strings.TrimSpace(u.Port())
	if port == "" {
		switch strings.ToLower(strings.TrimSpace(u.Scheme)) {
		case "https":
			port = "443"
		default:
			port = "80"
		}
	}
	address := net.JoinHostPort(hostname, port)
	conn, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ComputeMCPURL derives the effective local MCP URL from listen + mcp-path.
// It returns deterministic=false when listen uses an ephemeral port.
func ComputeMCPURL(listen, mcpPath string) (endpoint string, deterministic bool) {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return "", false
	}
	host, portStr, err := net.SplitHostPort(listen)
	if err != nil {
		return "", false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 {
		return "", false
	}
	if port == 0 {
		return "", false
	}
	host = strings.TrimSpace(host)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	path := normalizeMCPPath(mcpPath)
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, strconv.Itoa(port)), Path: path}).String(), true
}

func effectiveListen(listen string, port int) string {
	trimmed := strings.TrimSpace(listen)
	if trimmed != "" {
		return trimmed
	}
	if port > 0 {
		return fmt.Sprintf("127.0.0.1:%d", port)
	}
	return ""
}

func normalizeMCPPath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return config.DefaultHostMCPPath
	}
	if !strings.HasPrefix(trimmed, "/") {
		return "/" + trimmed
	}
	return trimmed
}

func resolveRootDir(dirFlag, cmdDir string) string {
	if strings.TrimSpace(dirFlag) != "" {
		if abs, err := filepath.Abs(dirFlag); err == nil {
			return abs
		}
		return dirFlag
	}
	if strings.TrimSpace(cmdDir) != "" {
		if abs, err := filepath.Abs(cmdDir); err == nil {
			return abs
		}
		return cmdDir
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

func captureEndpoint(stop <-chan struct{}, pid int, rootDir string, timeout, pollInterval time.Duration) {
	if pid <= 0 || strings.TrimSpace(rootDir) == "" {
		return
	}
	if timeout <= 0 {
		timeout = defaultEndpointCaptureTimeout
	}
	if pollInterval <= 0 {
		pollInterval = defaultEndpointCapturePollInterval
	}
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if time.Now().After(deadline) {
				return
			}
			if !processAlive(pid) {
				return
			}
			endpoint, ok := readConnectionURL(rootDir)
			if !ok {
				continue
			}
			_ = updateStateEndpoint(pid, endpoint)
			return
		}
	}
}

func readConnectionURL(rootDir string) (string, bool) {
	path := filepath.Join(rootDir, ".dir2mcp", "connection.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var payload struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		return "", false
	}
	u := strings.TrimSpace(payload.URL)
	if u == "" {
		return "", false
	}
	if _, err := url.Parse(u); err != nil {
		return "", false
	}
	return u, true
}

type connectionDetailsFile struct {
	Headers     map[string]string `json:"headers"`
	Session     connectionSession `json:"session"`
	TokenSource string            `json:"token_source"`
	TokenFile   string            `json:"token_file"`
}

type connectionSession struct {
	UsesMCPSessionID bool   `json:"uses_mcp_session_id"`
	HeaderName       string `json:"header_name"`
}

func readConnectionContractDetails(state State) (protocolHeader, sessionHeaderName, authSourceType string) {
	root := strings.TrimSpace(state.RootDir)
	if root == "" {
		workDir := strings.TrimSpace(state.WorkDir)
		if workDir == "" {
			return "", "", ""
		}
		root = resolveRootDir("", workDir)
	}
	if root == "" {
		return "", "", ""
	}
	b, err := os.ReadFile(filepath.Join(root, ".dir2mcp", "connection.json"))
	if err != nil {
		return "", "", ""
	}
	var payload connectionDetailsFile
	if err := json.Unmarshal(b, &payload); err != nil {
		return "", "", ""
	}
	if payload.Headers != nil {
		protocolHeader = strings.TrimSpace(payload.Headers[protocol.MCPProtocolVersionHeader])
	}
	if payload.Session.UsesMCPSessionID {
		sessionHeaderName = strings.TrimSpace(payload.Session.HeaderName)
	}
	authSourceType = deriveAuthSourceType(payload.TokenSource, payload.TokenFile)
	return protocolHeader, sessionHeaderName, authSourceType
}

func deriveAuthSourceType(tokenSource, tokenFile string) string {
	source := strings.TrimSpace(tokenSource)
	if source != "" {
		return source
	}
	if strings.TrimSpace(tokenFile) != "" {
		return "file"
	}
	return ""
}

func updateStateEndpoint(pid int, endpoint string) error {
	state, err := LoadState()
	if err != nil {
		return err
	}
	if state.PID != pid {
		return nil
	}
	state.MCPURL = endpoint
	return SaveState(state)
}

func resolveProbeToken(state State) string {
	if token := strings.TrimSpace(os.Getenv("DIR2MCP_AUTH_TOKEN")); token != "" {
		return token
	}
	root := strings.TrimSpace(state.RootDir)
	if root == "" {
		root = resolveRootDir("", state.WorkDir)
	}
	if root == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(root, ".dir2mcp", "secret.token"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func probeMCPReady(ctx context.Context, endpoint, token string) (bool, string) {
	client := &http.Client{Timeout: 2 * time.Second}
	sessionID, err := initializeMCP(ctx, client, endpoint, token)
	if err != nil {
		return false, err.Error()
	}
	if err := notifyInitialized(ctx, client, endpoint, token, sessionID); err != nil {
		return false, err.Error()
	}
	if err := listTools(ctx, client, endpoint, token, sessionID); err != nil {
		return false, err.Error()
	}
	return true, ""
}

func initializeMCP(ctx context.Context, client *http.Client, endpoint, token string) (string, error) {
	body, status, headers, err := rpcCall(ctx, client, endpoint, token, "", true, protocol.RPCMethodInitialize, map[string]any{
		"protocolVersion": coreconfig.DefaultProtocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"clientInfo":      map[string]any{"name": "dirstral-mcp-server", "version": buildinfo.Version},
	})
	if err != nil {
		return "", fmt.Errorf("initialize failed: %w", err)
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("initialize failed with status %d", status)
	}
	sessionID := strings.TrimSpace(headers.Get(protocol.MCPSessionHeader))
	if sessionID == "" {
		return "", fmt.Errorf("initialize missing %s", protocol.MCPSessionHeader)
	}
	if _, ok := body["result"].(map[string]any); !ok {
		return "", fmt.Errorf("initialize returned invalid payload")
	}
	return sessionID, nil
}

func notifyInitialized(ctx context.Context, client *http.Client, endpoint, token, sessionID string) error {
	_, status, _, err := rpcCall(ctx, client, endpoint, token, sessionID, false, protocol.RPCMethodNotificationsInitialized, map[string]any{})
	if err != nil {
		return fmt.Errorf("notifications/initialized failed: %w", err)
	}
	if status != http.StatusAccepted && (status < 200 || status >= 300) {
		return fmt.Errorf("notifications/initialized returned status %d", status)
	}
	return nil
}

func listTools(ctx context.Context, client *http.Client, endpoint, token, sessionID string) error {
	body, status, _, err := rpcCall(ctx, client, endpoint, token, sessionID, true, protocol.RPCMethodToolsList, map[string]any{})
	if err != nil {
		return fmt.Errorf("tools/list failed: %w", err)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("tools/list failed with status %d", status)
	}
	result, ok := body["result"].(map[string]any)
	if !ok {
		return fmt.Errorf("tools/list returned invalid payload")
	}
	if _, ok := result["tools"].([]any); !ok {
		return fmt.Errorf("tools/list missing tools array")
	}
	return nil
}

func rpcCall(ctx context.Context, client *http.Client, endpoint, token, sessionID string, withID bool, method string, params map[string]any) (map[string]any, int, http.Header, error) {
	reqBody := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}
	if withID {
		reqBody["id"] = 1
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(protocol.MCPProtocolVersionHeader, coreconfig.DefaultProtocolVersion)
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if sessionID != "" {
		req.Header.Set(protocol.MCPSessionHeader, sessionID)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, resp.Header, err
	}
	if len(bodyBytes) == 0 {
		return map[string]any{}, resp.StatusCode, resp.Header, nil
	}
	var envelope map[string]any
	if err := json.Unmarshal(bodyBytes, &envelope); err != nil {
		return nil, resp.StatusCode, resp.Header, err
	}
	if rpcErr, ok := envelope["error"].(map[string]any); ok && len(rpcErr) > 0 {
		msg, _ := rpcErr["message"].(string)
		code := 0
		if codeFloat, ok := rpcErr["code"].(float64); ok {
			code = int(codeFloat)
		}
		return nil, resp.StatusCode, resp.Header, fmt.Errorf("json-rpc error %d: %s", code, msg)
	}
	return envelope, resp.StatusCode, resp.Header, nil
}

func isSupportedAuthSource(s string) bool {
	switch s {
	case "env", "keychain", "file", "secret", "prompt", "contract":
		return true
	default:
		return false
	}
}
