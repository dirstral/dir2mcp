package cli

import (
	"bufio"
	"context"
	"encoding/xml"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// serviceSpec is the platform-agnostic description of the supervised
// daemon a `dir2mcp service install` writes. The OS backend turns it
// into a launchd plist (macOS) or, in a follow-up, a systemd user unit.
type serviceSpec struct {
	Label      string
	BinaryPath string
	WorkingDir string
	Args       []string
	LogPath    string
}

// serviceState is the backend's view of an installed service.
type serviceState struct {
	Installed bool
	Running   bool
	Detail    string
	UnitPath  string
}

// serviceManager abstracts the OS service backend (launchd today). The
// concrete implementation is selected at build time via newServiceManager
// in the per-GOOS file.
type serviceManager interface {
	backendName() string
	install(spec serviceSpec) (unitPath string, err error)
	uninstall(label string) (unitPath string, removed bool, err error)
	status(label string) (serviceState, error)
}

// serviceCredentialEnvKeys are the provider credential variables a
// persisted .env.local can carry. The launchd/systemd job does NOT
// inherit credentials exported in an interactive shell, so the install
// preflight checks these against the corpus dotenv to warn operators
// before the daemon fails at boot with "no embedding provider configured".
var serviceCredentialEnvKeys = []string{
	"MISTRAL_API_KEY",
	"OPENAI_API_KEY",
	"OPENROUTER_API_KEY",
	"ANTHROPIC_API_KEY",
	"GEMINI_API_KEY",
	"COHERE_API_KEY",
	"ELEVENLABS_API_KEY",
}

// serviceContext bundles the resolved per-corpus values both install and
// uninstall need: the supervisor label, the corpus working directory
// (also where .env.local must live so the booted daemon resolves
// credentials), the state dir, and the dir2mcp binary path.
type serviceContext struct {
	label      string
	serverName string
	workingDir string
	stateDir   string
	binaryPath string
	logPath    string
}

func (a *App) runService(_ context.Context, global globalOptions, args []string) int {
	if len(args) == 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid,
			"service requires a subcommand: install, uninstall, or status")
		return exitConfigInvalid
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "install":
		return a.runServiceInstall(global, rest)
	case "uninstall":
		return a.runServiceUninstall(global, rest)
	case "status":
		return a.runServiceStatus(global, rest)
	default:
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid,
			fmt.Sprintf("unknown service subcommand: %s", sub),
			"Use one of: install, uninstall, status")
		return exitConfigInvalid
	}
}

// serviceLabel derives a launchd/systemd-friendly reverse-DNS label from
// the auto-derived (or overridden) server name, which already encodes the
// corpus slug + path hash and so is stable and unique per directory.
func serviceLabel(serverName string) string {
	return "com.dirstral." + strings.TrimSpace(serverName)
}

func (a *App) resolveServiceContext(global globalOptions, nameOverride string) (serviceContext, error) {
	cfg, err := loadConfigWithGlobalOptions(global)
	if err != nil {
		return serviceContext{}, fmt.Errorf("load config: %w", err)
	}
	name := resolveClaudeServerName(&cfg, nameOverride)
	abs, err := filepath.Abs(cfg.RootDir)
	if err != nil {
		abs = cfg.RootDir
	}
	stateDir := strings.TrimSpace(cfg.StateDir)
	if stateDir == "" {
		stateDir = filepath.Join(abs, ".dir2mcp")
	}
	bin, err := os.Executable()
	if err != nil {
		return serviceContext{}, fmt.Errorf("locate dir2mcp executable: %w", err)
	}
	return serviceContext{
		label:      serviceLabel(name),
		serverName: name,
		workingDir: abs,
		stateDir:   stateDir,
		binaryPath: bin,
		logPath:    filepath.Join(stateDir, "service.log"),
	}, nil
}

func (a *App) runServiceInstall(global globalOptions, args []string) int {
	name, code := a.parseServiceFlags("service install", global, args)
	if code != exitSuccess {
		return code
	}
	sc, mgr, code := a.serviceContextAndManager(global, name)
	if code != exitSuccess {
		return code
	}
	if err := os.MkdirAll(sc.stateDir, 0o755); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("create state dir %s: %v", sc.stateDir, err))
		return exitGeneric
	}
	a.warnIfNoPersistentCredential(sc.workingDir)

	unitPath, err := mgr.install(serviceSpec{
		Label:      sc.label,
		BinaryPath: sc.binaryPath,
		WorkingDir: sc.workingDir,
		Args:       []string{"up", "--foreground"},
		LogPath:    sc.logPath,
	})
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("install service: %v", err))
		return exitGeneric
	}

	if global.jsonOutput {
		return a.emitServiceJSON(map[string]interface{}{
			"action": "install", "label": sc.label, "backend": mgr.backendName(),
			"unit_path": unitPath, "working_dir": sc.workingDir, "binary": sc.binaryPath,
		})
	}
	writef(a.stdout, "installed %s service %q\n", mgr.backendName(), sc.label)
	writef(a.stdout, "  unit:    %s\n", unitPath)
	writef(a.stdout, "  workdir: %s\n", sc.workingDir)
	writeln(a.stdout, "the daemon will start now and again at every login")
	return exitSuccess
}

func (a *App) runServiceUninstall(global globalOptions, args []string) int {
	name, code := a.parseServiceFlags("service uninstall", global, args)
	if code != exitSuccess {
		return code
	}
	sc, mgr, code := a.serviceContextAndManager(global, name)
	if code != exitSuccess {
		return code
	}
	unitPath, removed, err := mgr.uninstall(sc.label)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("uninstall service: %v", err))
		return exitGeneric
	}
	if global.jsonOutput {
		return a.emitServiceJSON(map[string]interface{}{
			"action": "uninstall", "label": sc.label, "backend": mgr.backendName(),
			"unit_path": unitPath, "removed": removed,
		})
	}
	if removed {
		writef(a.stdout, "removed %s service %q (%s)\n", mgr.backendName(), sc.label, unitPath)
	} else {
		writef(a.stdout, "no %s service %q was installed\n", mgr.backendName(), sc.label)
	}
	return exitSuccess
}

func (a *App) runServiceStatus(global globalOptions, args []string) int {
	name, code := a.parseServiceFlags("service status", global, args)
	if code != exitSuccess {
		return code
	}
	sc, mgr, code := a.serviceContextAndManager(global, name)
	if code != exitSuccess {
		return code
	}
	state, err := mgr.status(sc.label)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("service status: %v", err))
		return exitGeneric
	}
	if global.jsonOutput {
		return a.emitServiceJSON(map[string]interface{}{
			"action": "status", "label": sc.label, "backend": mgr.backendName(),
			"installed": state.Installed, "running": state.Running,
			"detail": state.Detail, "unit_path": state.UnitPath,
		})
	}
	writef(a.stdout, "%s service %q\n", mgr.backendName(), sc.label)
	writef(a.stdout, "  installed: %t\n", state.Installed)
	writef(a.stdout, "  running:   %t\n", state.Running)
	if strings.TrimSpace(state.Detail) != "" {
		writef(a.stdout, "  detail:    %s\n", state.Detail)
	}
	return exitSuccess
}

// parseServiceFlags parses the shared --name flag for a service
// subcommand and rejects positional arguments.
func (a *App) parseServiceFlags(usage string, global globalOptions, args []string) (string, int) {
	fs := flag.NewFlagSet(usage, flag.ContinueOnError)
	fs.SetOutput(ioDiscard{})
	name := fs.String("name", "", "service name override (defaults to the auto-derived server name)")
	if err := fs.Parse(args); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("invalid %s flags: %v", usage, err))
		return "", exitConfigInvalid
	}
	if len(fs.Args()) > 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid,
			fmt.Sprintf("%s does not accept positional arguments: %s", usage, strings.Join(fs.Args(), " ")))
		return "", exitConfigInvalid
	}
	return *name, exitSuccess
}

// serviceContextAndManager resolves the per-corpus context and the OS
// backend, mapping their errors to the right exit codes.
func (a *App) serviceContextAndManager(global globalOptions, name string) (serviceContext, serviceManager, int) {
	sc, err := a.resolveServiceContext(global, name)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, err.Error())
		return serviceContext{}, nil, exitConfigInvalid
	}
	mgr, err := newServiceManager()
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, err.Error())
		return serviceContext{}, nil, exitGeneric
	}
	return sc, mgr, exitSuccess
}

func (a *App) emitServiceJSON(payload map[string]interface{}) int {
	if err := emitJSON(a.stdout, payload); err != nil {
		writeCLIError(a.stderr, true, exitGeneric, fmt.Sprintf("encode service json: %v", err))
		return exitGeneric
	}
	return exitSuccess
}

// warnIfNoPersistentCredential surfaces the launchd/systemd credential
// trap: the supervised job starts from a clean environment, so a key that
// only lives as a shell `export` will be gone at boot. We warn (rather
// than block) so operators who configure credentials another way — e.g. a
// literal providers: api_key in .dir2mcp.yaml — are not stopped.
func (a *App) warnIfNoPersistentCredential(workingDir string) {
	if persistentCredentialInDotenv(workingDir) {
		return
	}
	writef(a.stderr, "warning: no persisted provider credential found in %s\n", filepath.Join(workingDir, ".env.local"))
	writeln(a.stderr, "  The service starts from a clean environment and will NOT inherit credentials exported in your shell.")
	writeln(a.stderr, "  Run `dir2mcp config init` to persist MISTRAL_API_KEY to .env.local (or add a providers: block to .dir2mcp.yaml),")
	writeln(a.stderr, "  otherwise the daemon will fail at boot with \"no embedding provider configured\".")
}

// persistentCredentialInDotenv reports whether the corpus dir holds a
// dotenv file (.env.local preferred, then .env) that defines a non-empty
// provider credential the booted daemon could read.
func persistentCredentialInDotenv(dir string) bool {
	for _, name := range []string{".env.local", ".env"} {
		if dotenvHasCredential(filepath.Join(dir, name)) {
			return true
		}
	}
	return false
}

func dotenvHasCredential(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok || !isServiceCredentialKey(strings.TrimSpace(key)) {
			continue
		}
		if strings.Trim(strings.TrimSpace(value), `"'`) != "" {
			return true
		}
	}
	return false
}

func isServiceCredentialKey(key string) bool {
	for _, k := range serviceCredentialEnvKeys {
		if key == k {
			return true
		}
	}
	return false
}

// renderLaunchdPlist builds a macOS LaunchAgent property list for spec.
// It lives in the shared (untagged) file so its output is unit-tested on
// every platform; only the launchctl invocation is darwin-gated. Every
// string value is XML-escaped so corpus paths containing & or < cannot
// corrupt the plist.
func renderLaunchdPlist(spec serviceSpec) string {
	program := append([]string{spec.BinaryPath}, spec.Args...)

	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	writePlistString(&b, "Label", spec.Label)
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, arg := range program {
		b.WriteString("    <string>")
		xmlEscape(&b, arg)
		b.WriteString("</string>\n")
	}
	b.WriteString("  </array>\n")
	writePlistString(&b, "WorkingDirectory", spec.WorkingDir)
	writePlistString(&b, "StandardOutPath", spec.LogPath)
	writePlistString(&b, "StandardErrorPath", spec.LogPath)
	b.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	b.WriteString("  <key>KeepAlive</key>\n  <true/>\n")
	writePlistString(&b, "ProcessType", "Background")
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func writePlistString(b *strings.Builder, key, value string) {
	b.WriteString("  <key>")
	xmlEscape(b, key)
	b.WriteString("</key>\n  <string>")
	xmlEscape(b, value)
	b.WriteString("</string>\n")
}

func xmlEscape(b *strings.Builder, s string) {
	_ = xml.EscapeText(b, []byte(s))
}
