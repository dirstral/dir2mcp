package cli

import (
	"bufio"
	"context"
	"encoding/xml"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dirstral/dir2mcp/internal/config"
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

// runService dispatches `dir2mcp service <subcommand>`.
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

// validateServiceName rejects name overrides that could produce an invalid
// launchd label or a dangerous plist filename. Auto-derived names are already
// safe; this guard covers explicit --name overrides that bypass that derivation.
// Allowed characters: A-Za-z0-9 plus dot, underscore, and hyphen — the same
// set permitted in reverse-DNS launchd labels.
func validateServiceName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("service name must not be empty or whitespace-only")
	}
	for _, r := range name {
		ok := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if !ok {
			return fmt.Errorf("service name %q contains disallowed character %q (allowed: A-Za-z0-9._-)", name, string(r))
		}
	}
	return nil
}

// resolveServiceContext loads config, derives the per-corpus label /
// binary / paths, and validates any explicit name override for path safety.
// It returns the loaded config alongside the context so callers can
// extract provider env-var refs for credential auto-persist.
func (a *App) resolveServiceContext(global globalOptions, nameOverride string) (serviceContext, config.Config, error) {
	if t := strings.TrimSpace(nameOverride); t != "" {
		if err := validateServiceName(t); err != nil {
			return serviceContext{}, config.Config{}, err
		}
	}
	cfg, err := loadConfigWithGlobalOptions(global)
	if err != nil {
		return serviceContext{}, config.Config{}, fmt.Errorf("load config: %w", err)
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
	if !filepath.IsAbs(stateDir) {
		stateDir = filepath.Join(abs, stateDir)
	}
	bin, err := os.Executable()
	if err != nil {
		return serviceContext{}, config.Config{}, fmt.Errorf("locate dir2mcp executable: %w", err)
	}
	return serviceContext{
		label:      serviceLabel(name),
		serverName: name,
		workingDir: abs,
		stateDir:   stateDir,
		binaryPath: bin,
		logPath:    filepath.Join(stateDir, "service.log"),
	}, cfg, nil
}

// runServiceInstall handles `dir2mcp service install`.
func (a *App) runServiceInstall(global globalOptions, args []string) int {
	name, code := a.parseServiceFlags("service install", global, args)
	if code != exitSuccess {
		return code
	}
	sc, cfg, mgr, code := a.serviceContextAndManager(global, name)
	if code != exitSuccess {
		return code
	}
	if err := os.MkdirAll(sc.stateDir, 0o700); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("create state dir %s: %v", sc.stateDir, err))
		return exitGeneric
	}
	savedCreds, failedCreds := a.persistAndWarnCredentials(sc.workingDir, cfg, global.jsonOutput, global.quiet)

	// Propagate global flag overrides so the installed service restarts
	// with the exact same config/state-dir the operator used for install,
	// rather than re-discovering defaults from the working directory alone.
	serviceArgs := []string{"up", "--foreground"}
	if p := strings.TrimSpace(global.configPath); p != "" {
		serviceArgs = append(serviceArgs, "--config", p)
	}
	if p := strings.TrimSpace(global.stateDir); p != "" {
		serviceArgs = append(serviceArgs, "--state-dir", p)
	}

	unitPath, err := mgr.install(serviceSpec{
		Label:      sc.label,
		BinaryPath: sc.binaryPath,
		WorkingDir: sc.workingDir,
		Args:       serviceArgs,
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
			"persisted_credentials": savedCreds,
			"failed_credentials":    failedCreds,
		})
	}
	if !global.quiet {
		writef(a.stdout, "installed %s service %q\n", mgr.backendName(), sc.label)
		writef(a.stdout, "  unit:    %s\n", unitPath)
		writef(a.stdout, "  workdir: %s\n", sc.workingDir)
		writeln(a.stdout, "the daemon will start now and again at every login")
	}
	return exitSuccess
}

// runServiceUninstall handles `dir2mcp service uninstall`.
func (a *App) runServiceUninstall(global globalOptions, args []string) int {
	name, code := a.parseServiceFlags("service uninstall", global, args)
	if code != exitSuccess {
		return code
	}
	if !a.confirmDestructive(global, "Remove the dir2mcp background service?", "Stops and unregisters the launchd/systemd agent for this corpus; it will no longer auto-start at login.") {
		writeln(a.stderr, "service uninstall aborted")
		return exitSuccess
	}
	sc, _, mgr, code := a.serviceContextAndManager(global, name)
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
	if !global.quiet {
		if removed {
			writef(a.stdout, "removed %s service %q (%s)\n", mgr.backendName(), sc.label, unitPath)
		} else {
			writef(a.stdout, "no %s service %q was installed\n", mgr.backendName(), sc.label)
		}
	}
	return exitSuccess
}

// runServiceStatus handles `dir2mcp service status`.
func (a *App) runServiceStatus(global globalOptions, args []string) int {
	name, code := a.parseServiceFlags("service status", global, args)
	if code != exitSuccess {
		return code
	}
	sc, _, mgr, code := a.serviceContextAndManager(global, name)
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
	if !global.quiet {
		writef(a.stdout, "%s service %q\n", mgr.backendName(), sc.label)
		writef(a.stdout, "  installed: %t\n", state.Installed)
		writef(a.stdout, "  running:   %t\n", state.Running)
		if strings.TrimSpace(state.Detail) != "" {
			writef(a.stdout, "  detail:    %s\n", state.Detail)
		}
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
// backend, mapping their errors to the right exit codes. The loaded
// config is also returned so callers can access provider env-var refs.
func (a *App) serviceContextAndManager(global globalOptions, name string) (serviceContext, config.Config, serviceManager, int) {
	sc, cfg, err := a.resolveServiceContext(global, name)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, err.Error())
		return serviceContext{}, config.Config{}, nil, exitConfigInvalid
	}
	mgr, err := newServiceManager()
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, err.Error())
		return serviceContext{}, config.Config{}, nil, exitGeneric
	}
	return sc, cfg, mgr, exitSuccess
}

// emitServiceJSON marshals payload as JSON to stdout, returning the
// appropriate exit code.
func (a *App) emitServiceJSON(payload map[string]interface{}) int {
	if err := emitJSON(a.stdout, payload); err != nil {
		writeCLIError(a.stderr, true, exitGeneric, fmt.Sprintf("encode service json: %v", err))
		return exitGeneric
	}
	return exitSuccess
}

// persistAndWarnCredentials is called during `service install`. It looks
// for provider credentials in the current process environment (from the
// operator's `export KEY=...` session) and persists any it finds to
// .env.local in the corpus working directory, so the launchd service can
// read them after a reboot — launchd does not inherit shell exports.
//
// In JSON or quiet mode all normal output is suppressed; the caller
// receives (saved, failed) to include in structured output instead.
// Write failures and invalid values (containing newlines) are collected
// into failed and reported as warnings in text mode.
func (a *App) persistAndWarnCredentials(workingDir string, cfg config.Config, jsonOut, quiet bool) (saved, failed []string) {
	envPath := filepath.Join(workingDir, ".env.local")
	refs := cfg.ProviderEnvVarRefs()
	saved, failed = persistCredentialsFromEnv(envPath, refs)

	if jsonOut || quiet {
		return
	}
	for _, key := range saved {
		writef(a.stdout, "  saved %s to %s (will survive reboots)\n", key, envPath)
	}
	for _, key := range failed {
		writef(a.stderr, "  warning: failed to persist %s to %s (check file permissions or key value)\n", key, envPath)
	}
	if len(saved) > 0 {
		return
	}
	// Nothing in the current environment to auto-save. Fall back to the
	// warning if there's also nothing already persisted in a dotenv file.
	if persistentCredentialInDotenv(workingDir, refs) {
		return
	}
	writef(a.stderr, "warning: no persisted provider credential found in %s\n", envPath)
	writeln(a.stderr, "  The service starts from a clean environment and will NOT inherit credentials exported in your shell.")
	writeln(a.stderr, "  Run `dir2mcp config init` to persist the credential to .env.local (or add a providers: block to .dir2mcp.yaml),")
	writeln(a.stderr, "  otherwise the daemon will fail at boot with \"no embedding provider configured\".")
	return
}

// persistCredentialsFromEnv writes each key in envVarRefs that is found
// non-empty in os.Getenv into the dotenv file at envPath, using the same
// upsert semantics as `dir2mcp config init`. Returns (saved, failed):
// saved are keys that were written successfully; failed are keys whose
// values contained newlines (would corrupt the dotenv file) or whose
// write failed (permissions, disk full, etc.).
func persistCredentialsFromEnv(envPath string, envVarRefs []string) (saved, failed []string) {
	for _, key := range envVarRefs {
		val := strings.TrimSpace(os.Getenv(key))
		if val == "" {
			continue
		}
		if strings.ContainsAny(val, "\r\n") {
			failed = append(failed, key)
			continue
		}
		if err := saveEnvLocalKey(envPath, key, val); err != nil {
			failed = append(failed, key)
			continue
		}
		saved = append(saved, key)
	}
	return
}

// persistentCredentialInDotenv reports whether the corpus dir holds a
// dotenv file (.env.local preferred, then .env) that defines a non-empty
// value for any of the given credential env var names.
func persistentCredentialInDotenv(dir string, refs []string) bool {
	for _, name := range []string{".env.local", ".env"} {
		if dotenvHasCredential(filepath.Join(dir, name), refs) {
			return true
		}
	}
	return false
}

// dotenvHasCredential scans a single dotenv file and returns true if it
// contains at least one non-empty assignment for a key in refs. Returns false
// on any I/O or scan error (conservative: assume no credential rather than
// masking the read failure as a credential hit).
func dotenvHasCredential(path string, refs []string) bool {
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
		if !ok || !slices.Contains(refs, strings.TrimSpace(key)) {
			continue
		}
		if strings.Trim(strings.TrimSpace(value), `"'`) != "" {
			return true
		}
	}
	// A scan error means we couldn't read the full file. Treat as no
	// credential found (conservative) — if the warning fires the operator
	// can explicitly confirm credentials are configured another way.
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

// writePlistString writes a <key>/<string> pair into a plist builder,
// XML-escaping both the key and value.
func writePlistString(b *strings.Builder, key, value string) {
	b.WriteString("  <key>")
	xmlEscape(b, key)
	b.WriteString("</key>\n  <string>")
	xmlEscape(b, value)
	b.WriteString("</string>\n")
}

// xmlEscape writes s XML-escaped into b, so special characters in corpus
// paths (& < >) cannot corrupt the plist document.
func xmlEscape(b *strings.Builder, s string) {
	_ = xml.EscapeText(b, []byte(s))
}
