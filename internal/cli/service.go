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
	sc, err := serviceContextFromConfig(cfg, nameOverride, resolveConfigPath(global))
	if err != nil {
		return serviceContext{}, config.Config{}, err
	}
	return sc, cfg, nil
}

// absOrSelf returns the absolute form of p, falling back to p itself when the
// working directory cannot be read (the historical behavior of every
// filepath.Abs call on this path).
func absOrSelf(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

// isExistingDir reports whether p names an existing directory.
func isExistingDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// serviceWorkingDirBase picks the directory the supervised daemon is booted in.
//
// For a filesystem-backed corpus (local/nfs) this stays the absolute corpus
// root, unchanged: that is where the operator's .dir2mcp.yaml and .env.local
// live by convention, and both the config loader (relative ".dir2mcp.yaml") and
// the dotenv loader (relative ".env.local") resolve against the process working
// directory.
//
// For an object-store corpus there is no local root to boot in (SPEC §7.8), and
// rendering the ignored RootDir as launchd's WorkingDirectory / systemd's
// WorkingDirectory installs a unit that cannot start (#738). The config file's
// own directory is used instead: it is the one local directory that is
// guaranteed to be the source of the config the operator just installed from,
// so the booted daemon rediscovers the same .dir2mcp.yaml and the same
// .env.local (which is also where persistAndWarnCredentials writes the AWS
// credentials, and what systemd renders as EnvironmentFile).
func serviceWorkingDirBase(cfg config.Config, configPath string) string {
	if !sourceIsRemote(cfg) {
		return absOrSelf(cfg.RootDir)
	}
	p := strings.TrimSpace(configPath)
	if p == "" {
		p = defaultConfigFileName
	}
	return filepath.Dir(absOrSelf(p))
}

// resolveServiceDirs resolves the supervisor working directory and the absolute
// state directory together, because they are coupled: a relative state_dir is
// resolved by the booted daemon against its own working directory, so install
// must resolve it against the same base or it would create (and log into) a
// different directory than the daemon uses.
//
// When the config directory itself does not exist (an explicit --config naming a
// path under a missing directory), the working directory falls back to the state
// directory, which is always local (SPEC §7.8) and is created before the unit is
// written.
func resolveServiceDirs(cfg config.Config, configPath string) (workingDir, stateDir string) {
	base := serviceWorkingDirBase(cfg, configPath)
	stateDir = strings.TrimSpace(cfg.StateDir)
	if stateDir == "" {
		stateDir = filepath.Join(base, ".dir2mcp")
	}
	if !filepath.IsAbs(stateDir) {
		stateDir = filepath.Join(base, stateDir)
	}
	workingDir = base
	if sourceIsRemote(cfg) && !isExistingDir(base) {
		workingDir = stateDir
	}
	return workingDir, stateDir
}

// serviceContextFromConfig derives the service context from an already-loaded
// config, so callers holding a config (e.g. `down`) don't pay a redundant
// config reload. nameOverride is assumed pre-validated by the caller.
// configPath is the config path the caller resolved (see resolveConfigPath); it
// anchors the working directory when the corpus root is remote (#738).
func serviceContextFromConfig(cfg config.Config, nameOverride, configPath string) (serviceContext, error) {
	name := resolveClaudeServerName(&cfg, nameOverride)
	workingDir, stateDir := resolveServiceDirs(cfg, configPath)
	bin, err := os.Executable()
	if err != nil {
		return serviceContext{}, fmt.Errorf("locate dir2mcp executable: %w", err)
	}
	return serviceContext{
		label:      serviceLabel(name),
		serverName: name,
		workingDir: workingDir,
		stateDir:   stateDir,
		binaryPath: bin,
		logPath:    filepath.Join(stateDir, "service.log"),
	}, nil
}

// installServiceArgs builds the argv the supervisor launches, propagating the
// operator's global flag overrides so the installed service restarts with the
// exact same config/state-dir they used for install, rather than re-discovering
// defaults from the working directory alone.
//
// Both propagated paths are made ABSOLUTE (#738). A relative --config was
// previously copied verbatim into the unit and then re-resolved by the daemon
// against its own working directory, which is not the directory the operator ran
// install from — so the unit could point at a config that does not exist. The
// state directory is taken from the already-resolved serviceContext, which is
// the same directory install just created and writes service.log into.
func installServiceArgs(global globalOptions, sc serviceContext) []string {
	serviceArgs := []string{"up", "--foreground"}
	if p := strings.TrimSpace(global.configPath); p != "" {
		serviceArgs = append(serviceArgs, "--config", absOrSelf(p))
	}
	if p := strings.TrimSpace(global.stateDir); p != "" {
		serviceArgs = append(serviceArgs, "--state-dir", sc.stateDir)
	}
	return serviceArgs
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

	serviceArgs := installServiceArgs(global, sc)

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
	// KeepAlive as a dict with SuccessfulExit=false: launchd respawns the
	// daemon only after a NON-ZERO (crash) exit, never after a graceful stop
	// (exit 0). This makes `dir2mcp down` (which SIGTERMs the daemon into a
	// clean exit 0) durably stop a service-managed daemon, and stops the boot
	// crash-loop when a misconfigured daemon is stopped gracefully (#434).
	b.WriteString("  <key>KeepAlive</key>\n  <dict>\n    <key>SuccessfulExit</key>\n    <false/>\n  </dict>\n")
	// Widen launchd's default 10s respawn throttle so a daemon that crashes
	// deterministically at boot (e.g. a missing credential) backs off instead
	// of hot-looping.
	b.WriteString("  <key>ThrottleInterval</key>\n  <integer>30</integer>\n")
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

// renderSystemdUnit builds a systemd user-service unit (INI) for spec.
// Like renderLaunchdPlist it lives in the shared (untagged) file so its
// output is unit-tested on every platform; only the systemctl invocation
// is linux-gated.
//
// The crash-loop-safe supervision block is the systemd analog of the
// launchd KeepAlive{SuccessfulExit=false}+ThrottleInterval pair: Restart
// only on a non-zero/crash exit (a graceful `down` exits 0), backed off by
// RestartSec and bounded by StartLimit* so a deterministically-failing
// config eventually gives up instead of hot-looping. EnvironmentFile points
// at the corpus .env.local (leading `-` = optional) so the booted daemon
// resolves provider credentials the same way the launchd service does.
func renderSystemdUnit(spec serviceSpec) string {
	envFile := filepath.Join(spec.WorkingDir, ".env.local")

	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=" + iniEscape("dir2mcp knowledge server ("+spec.Label+")") + "\n")
	// Start-rate limits are UNIT-level directives — systemd ignores them under
	// [Service] — so a crash-looping unit is capped at 5 starts per 300s and then
	// held failed instead of hot-looping forever.
	b.WriteString("StartLimitIntervalSec=300\n")
	b.WriteString("StartLimitBurst=5\n\n")

	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	b.WriteString("WorkingDirectory=" + iniEscape(spec.WorkingDir) + "\n")
	b.WriteString("ExecStart=" + renderSystemdExecStart(spec) + "\n")
	// Leading `-` marks the file optional: the unit still starts when the
	// operator configured credentials via .dir2mcp.yaml instead.
	b.WriteString("EnvironmentFile=-" + iniEscape(envFile) + "\n")
	b.WriteString("StandardOutput=append:" + iniEscape(spec.LogPath) + "\n")
	b.WriteString("StandardError=append:" + iniEscape(spec.LogPath) + "\n")
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=30\n\n")

	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

// renderSystemdExecStart joins the binary and its args into an ExecStart
// value, quoting any token containing whitespace so systemd parses it as a
// single argument. Each token is INI-escaped first.
func renderSystemdExecStart(spec serviceSpec) string {
	tokens := append([]string{spec.BinaryPath}, spec.Args...)
	quoted := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		esc := iniEscape(tok)
		// Quote a token containing whitespace or a double quote. Within a
		// double-quoted token systemd processes \" and \\ escapes, so escape any
		// embedded quote (iniEscape already doubled backslashes) — otherwise a
		// path containing " would break out of the quoting and split the command.
		if strings.ContainsAny(esc, " \t\"") {
			esc = "\"" + strings.ReplaceAll(esc, `"`, `\"`) + "\""
		}
		quoted = append(quoted, esc)
	}
	return strings.Join(quoted, " ")
}

// iniEscape makes s safe to embed in a systemd unit value. systemd treats a
// literal `%` as the start of a specifier, so it must be doubled. This is the
// INI analog of the plist's XML escaping (xmlEscape does NOT apply here). A
// value containing a carriage return or newline would break the single-line
// key=value format and is rejected upstream (rejectMultilineSpec); iniEscape
// assumes that guard has already run.
// iniEscape makes a value safe to place after `Key=` in a systemd unit. It
// doubles backslashes first — a value ending in `\` would otherwise act as a
// line-continuation, and systemd unescapes `\\`→`\` — then doubles `%` (the
// specifier char). Newlines are rejected upstream by the install path.
func iniEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, "%", "%%")
}

// rejectMultilineSpec fails when any spec field that becomes a systemd unit
// value contains a carriage return or newline, which would break the INI
// key=value line format (the same guard persistCredentialsFromEnv applies to
// dotenv values). Returns nil when every value is single-line.
func rejectMultilineSpec(spec serviceSpec) error {
	for _, v := range append([]string{spec.Label, spec.BinaryPath, spec.WorkingDir, spec.LogPath}, spec.Args...) {
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("service value %q contains a newline, which is not allowed in a systemd unit", v)
		}
	}
	return nil
}
