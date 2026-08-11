package cli

import (
	"bufio"
	"context"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	creds := a.persistAndWarnCredentials(sc.workingDir, cfg, global.jsonOutput, global.quiet)

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
			"persisted_credentials": creds.saved,
			"failed_credentials":    creds.failed,
			// missing_credentials names required runtime secrets with no
			// persistent source: the service is installed but cannot boot
			// until each is resolvable from .env.local/keychain (#722).
			"missing_credentials": creds.missing,
		})
	}
	if !global.quiet {
		writef(a.stdout, "installed %s service %q\n", mgr.backendName(), sc.label)
		writef(a.stdout, "  unit:    %s\n", unitPath)
		writef(a.stdout, "  workdir: %s\n", sc.workingDir)
		if len(creds.missing) > 0 {
			writef(a.stdout, "the daemon will start at every login, but %d required credential(s) are still missing (see warnings above)\n",
				len(creds.missing))
		} else {
			writeln(a.stdout, "the daemon will start now and again at every login")
		}
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

// serviceCredentialReport is the outcome of the install-time credential sweep.
// Every field holds env var NAMES only — never values (#722).
type serviceCredentialReport struct {
	// saved were found in the current process environment and written to
	// .env.local, so the supervised daemon will see them after a reboot.
	saved []string
	// failed could not be written (bad value or write error).
	failed []string
	// missing are REQUIRED by the effective config and have no persistent
	// source at all: the installed service cannot boot until they get one.
	missing []string
}

// persistAndWarnCredentials is called during `service install`. It looks for the
// runtime credentials the effective config depends on in the current process
// environment (from the operator's `export KEY=...` session) and persists any it
// finds to .env.local in the corpus working directory, so the supervised service
// can read them after a reboot — launchd/systemd do not inherit shell exports.
//
// The sweep covers BOTH provider-profile `api_key` references (ProviderEnvVarRefs)
// and the non-provider runtime secrets of the effective config (RuntimeSecretRefs:
// S3 credentials, the Qdrant key, the pgvector DSN, the broker URL, the x402
// facilitator token). Before #722 only the provider refs were considered, so an
// `source.kind: s3` install validated against AWS credentials that existed only
// in the installing shell, then silently produced a service that could not boot.
//
// In JSON or quiet mode all normal output is suppressed; the caller receives the
// report to include in structured output instead.
func (a *App) persistAndWarnCredentials(workingDir string, cfg config.Config, jsonOut, quiet bool) serviceCredentialReport {
	envPath := filepath.Join(workingDir, ".env.local")
	providerRefs := cfg.ProviderEnvVarRefs()
	runtimeRefs := cfg.RuntimeSecretRefs()

	var rep serviceCredentialReport
	rep.saved, rep.failed = persistCredentialsFromEnv(envPath, credentialSweepKeys(providerRefs, runtimeRefs))
	rep.missing = unpersistedRequiredSecrets(workingDir, runtimeRefs, rep.saved)

	if jsonOut || quiet {
		return rep
	}
	a.reportCredentialSweep(envPath, rep, runtimeRefs)
	// The provider-credential warning below is about PROVIDER keys only, so it
	// must be suppressed only by a provider key: capturing an AWS credential
	// says nothing about whether an embedding provider will resolve at boot.
	if anyOf(rep.saved, providerRefs) || persistentCredentialInDotenv(workingDir, providerRefs) {
		return rep
	}
	// Nothing in the current environment to auto-save, and nothing already
	// persisted in a dotenv file.
	writef(a.stderr, "warning: no persisted provider credential found in %s\n", envPath)
	writeln(a.stderr, "  The service starts from a clean environment and will NOT inherit credentials exported in your shell.")
	writeln(a.stderr, "  Run `dir2mcp config init` to persist the credential to .env.local (or add a providers: block to .dir2mcp.yaml),")
	writeln(a.stderr, "  otherwise the daemon will fail at boot with \"no embedding provider configured\".")
	return rep
}

// anyOf reports whether haystack contains at least one member of wanted.
func anyOf(haystack, wanted []string) bool {
	for _, w := range wanted {
		if slices.Contains(haystack, w) {
			return true
		}
	}
	return false
}

// credentialSweepKeys merges the provider env var refs with the non-provider
// runtime secret names, preserving first-seen order and dropping duplicates so a
// key referenced by both is only written once.
func credentialSweepKeys(providerRefs []string, runtimeRefs []config.RuntimeSecretRef) []string {
	keys := make([]string, 0, len(providerRefs)+len(runtimeRefs))
	seen := make(map[string]struct{}, len(providerRefs)+len(runtimeRefs))
	add := func(name string) {
		if _, dup := seen[name]; dup || strings.TrimSpace(name) == "" {
			return
		}
		seen[name] = struct{}{}
		keys = append(keys, name)
	}
	for _, name := range providerRefs {
		add(name)
	}
	for _, ref := range runtimeRefs {
		add(ref.Name)
	}
	return keys
}

// unpersistedRequiredSecrets names the required runtime secrets the installed
// service will not be able to resolve at boot.
//
// A secret is satisfied when it was just persisted to .env.local, when the
// target directory's dotenv already defines it, or when the effective config
// resolved it from a source other than the current environment (the keychain).
// The keychain caveat is documented in the README: a background agent may not be
// able to unlock it unattended, so keychain-only credentials are still best
// mirrored into .env.local with `dir2mcp config init`.
func unpersistedRequiredSecrets(workingDir string, refs []config.RuntimeSecretRef, saved []string) []string {
	var missing []string
	for _, ref := range refs {
		if !ref.Required || slices.Contains(saved, ref.Name) {
			continue
		}
		fromEnv := strings.TrimSpace(os.Getenv(ref.Name)) != ""
		if !fromEnv && ref.Resolved {
			continue // resolved from a persistent source (keychain / dotenv)
		}
		if persistentCredentialInDotenv(workingDir, []string{ref.Name}) {
			continue
		}
		missing = append(missing, ref.Name)
	}
	return missing
}

// reportCredentialSweep prints the human-readable half of the sweep: what was
// persisted, what could not be, and which required secrets have no persistent
// source. Values are never printed.
func (a *App) reportCredentialSweep(envPath string, rep serviceCredentialReport, refs []config.RuntimeSecretRef) {
	for _, key := range rep.saved {
		writef(a.stdout, "  saved %s to %s (will survive reboots)\n", key, envPath)
	}
	for _, key := range rep.failed {
		writef(a.stderr, "  warning: failed to persist %s to %s (check file permissions or key value)\n", key, envPath)
	}
	if len(rep.missing) == 0 {
		return
	}
	writeln(a.stderr, "warning: the installed service is NOT bootable yet — required credentials have no persistent source:")
	for _, key := range rep.missing {
		writef(a.stderr, "  %s (required by %s)\n", key, secretFeature(refs, key))
	}
	writef(a.stderr, "  Export each one and re-run `dir2mcp service install` to capture it into %s,\n", envPath)
	writeln(a.stderr, "  or add it to .env.local yourself. The supervised daemon starts from a clean environment.")
}

// secretFeature returns the config setting that pulls name in, for messages.
func secretFeature(refs []config.RuntimeSecretRef, name string) string {
	for _, ref := range refs {
		if ref.Name == name {
			return ref.Feature
		}
	}
	return "the effective config"
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

// --- supervisor output classification (#723, #725) ---
//
// Both backends invoke their supervisor through runCmd, which merges stderr into
// stdout. A non-zero exit is NORMAL for several benign outcomes (booting out a
// service that is not loaded, asking whether an absent unit is active), so the
// exit status alone cannot tell a benign result from a real failure. The
// discriminator is the OUTPUT, and the rule is deny-by-default: only a
// positively recognized benign phrase is treated as benign; anything else — a
// dead user bus, a permission denial, a missing binary — is surfaced.

// containsAnyFold reports whether s contains any of phrases, case-insensitively.
// phrases must already be lowercase.
func containsAnyFold(s string, phrases []string) bool {
	lower := strings.ToLower(s)
	for _, p := range phrases {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// launchctlPrintAbsentPhrases are the substrings `launchctl print` emits for a
// service that is not registered in the domain. Kept deliberately narrow: this
// list decides whether `service status` reports a clean absent state or an
// operational error.
var launchctlPrintAbsentPhrases = []string{
	"could not find service",
	"not found in domain",
}

// launchctlBootoutAbsentPhrases are the substrings `launchctl bootout` emits
// when there is nothing to unload. It is a superset of the print phrases:
// bootout also reports the POSIX errno text ("No such process" for a service
// that is not booted in, "No such file or directory" for a plist path that does
// not exist), both of which mean the same thing — already absent.
var launchctlBootoutAbsentPhrases = append([]string{
	"no such process",
	"no such file or directory",
	"could not find specified service",
}, launchctlPrintAbsentPhrases...)

// launchctlPrintReportsAbsent reports whether a failed `launchctl print`
// positively identified the service as unregistered.
func launchctlPrintReportsAbsent(out string) bool {
	return containsAnyFold(out, launchctlPrintAbsentPhrases)
}

// launchctlBootoutReportsAbsent reports whether a failed `launchctl bootout`
// positively identified the service as already unloaded. Anything else — a
// permission denial, a broken domain — must NOT be swallowed (#723).
func launchctlBootoutReportsAbsent(out string) bool {
	return containsAnyFold(out, launchctlBootoutAbsentPhrases)
}

// systemctlTransportPhrases mark an invocation that never reached the user
// manager at all. They are checked FIRST because systemd reports a dead user bus
// as "Failed to connect to bus: No such file or directory" — text that would
// otherwise read as a missing unit and launder a transport failure into a benign
// absent state (#725).
var systemctlTransportPhrases = []string{
	"failed to connect to bus",
	"failed to get d-bus connection",
	"connection refused",
	"no medium found",
	"permission denied",
	"access denied",
	"interactive authentication required",
	"refusing to operate",
}

// systemctlAbsentPhrases are the substrings systemctl emits for a unit it does
// not know or that is not loaded.
var systemctlAbsentPhrases = []string{
	"does not exist",
	"not loaded",
	"no such unit",
	"no such file or directory",
}

// systemctlReportsAbsent reports whether a failed systemctl invocation
// positively identified the unit as absent/not loaded, rather than failing to
// reach systemd at all.
func systemctlReportsAbsent(out string) bool {
	if containsAnyFold(out, systemctlTransportPhrases) {
		return false
	}
	return containsAnyFold(out, systemctlAbsentPhrases)
}

// systemctlQuery describes one `systemctl --user is-*` probe: the documented
// one-word verdicts it can print, and the verdict a "unit does not exist"
// diagnostic stands for.
type systemctlQuery struct {
	name   string
	absent string
	states []string
}

var (
	// systemctlIsActive covers the ACTIVE STATE column of `systemctl list-units`.
	systemctlIsActive = systemctlQuery{
		name:   "is-active",
		absent: "inactive",
		states: []string{"active", "reloading", "inactive", "failed", "activating", "deactivating", "maintenance", "refreshing", "unknown"},
	}
	// systemctlIsEnabled covers the documented `is-enabled` verdicts.
	systemctlIsEnabled = systemctlQuery{
		name:   "is-enabled",
		absent: "not-found",
		states: []string{"enabled", "enabled-runtime", "linked", "linked-runtime", "alias", "masked", "masked-runtime", "static", "indirect", "disabled", "generated", "transient", "not-found", "bad"},
	}
)

// classify turns one is-* invocation into a verdict or an operational error.
//
// The verdict is located by matching a documented state word on ANY output line
// rather than by trusting the whole output: runCmd merges stderr in, and some
// systemd versions print a diagnostic ("Failed to get unit file state for
// x.service: No such file or directory") alongside — or instead of — the word.
// When no verdict is present, an absent-unit diagnostic maps to the query's
// absent verdict; everything else is a failure the caller must surface (#725).
func (q systemctlQuery) classify(out string, err error) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); slices.Contains(q.states, s) {
			return s, nil
		}
	}
	if systemctlReportsAbsent(out) {
		return q.absent, nil
	}
	detail := strings.TrimSpace(out)
	if detail == "" {
		detail = "(no output)"
	}
	if err != nil {
		return "", fmt.Errorf("systemctl --user %s: %w: %s", q.name, err, detail)
	}
	return "", fmt.Errorf("systemctl --user %s: unrecognized output: %s", q.name, detail)
}

// --- transactional definition replacement (#724) ---

// unitTxn captures a service definition file's prior contents so a failed
// install can put back exactly what was there before.
//
// It makes the FILE half of an install atomic: the replacement is staged in the
// same directory and renamed into place (a supervisor never observes a truncated
// definition), and a rollback restores the prior bytes byte-for-byte, or removes
// the file entirely when the install was a first-time one. The SUPERVISOR half —
// re-loading the previous service — is backend-specific and layered on top.
type unitTxn struct {
	path string
	// perm is the mode a freshly written definition gets.
	perm os.FileMode
	// prior/priorPerm/had describe what was on disk before write. priorPerm is
	// tracked separately from perm so a rollback restores the previous file's
	// own mode without the replacement inheriting it.
	prior     []byte
	priorPerm os.FileMode
	had       bool
}

// beginUnitTxn snapshots the definition currently at path. A missing file is not
// an error: it marks a first-time install, whose rollback is a removal.
func beginUnitTxn(path string, perm os.FileMode) (*unitTxn, error) {
	txn := &unitTxn{path: path, perm: perm, priorPerm: perm}
	body, err := os.ReadFile(path)
	switch {
	case err == nil:
		txn.prior, txn.had = body, true
		if info, statErr := os.Stat(path); statErr == nil {
			txn.priorPerm = info.Mode().Perm()
		}
		return txn, nil
	case errors.Is(err, os.ErrNotExist):
		return txn, nil
	default:
		return nil, err
	}
}

// write atomically replaces the definition with content.
func (t *unitTxn) write(content string) error {
	return t.writeBytes([]byte(content), t.perm)
}

func (t *unitTxn) writeBytes(content []byte, perm os.FileMode) error {
	dir := filepath.Dir(t.path)
	f, err := os.CreateTemp(dir, ".dir2mcp-unit-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, t.path)
}

// rollback restores the definition to its pre-write state.
func (t *unitTxn) rollback() error {
	if !t.had {
		if err := os.Remove(t.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return t.writeBytes(t.prior, t.priorPerm)
}

// describeRestored states what the definition on disk is after a rollback, so
// the operator does not have to guess.
func (t *unitTxn) describeRestored() string {
	if t.had {
		return fmt.Sprintf("the previous service definition at %s was restored", t.path)
	}
	return fmt.Sprintf("no service definition was left at %s", t.path)
}

// wrapInstallRollback annotates a failed install with the state the machine was
// left in: either a clean undo, or an explicit list of what could not be undone.
// A partially rolled back install is the one case an operator must never have to
// infer, so the problems are named rather than summarized.
func wrapInstallRollback(cause error, txn *unitTxn, problems []string) error {
	if len(problems) == 0 {
		return fmt.Errorf("%w (rolled back: %s)", cause, txn.describeRestored())
	}
	return fmt.Errorf("%w; ROLLBACK INCOMPLETE: %s", cause, strings.Join(problems, "; "))
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
	// Give the daemon its full graceful-stop budget before launchd escalates to
	// SIGKILL (issue #688). The launchd default is 20 seconds, but it is a
	// default: state it so the drain cannot be cut short by a changed default.
	b.WriteString("  <key>ExitTimeOut</key>\n  <integer>" +
		strconv.Itoa(serviceStopTimeoutSeconds) + "</integer>\n")
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
	b.WriteString("RestartSec=30\n")
	// Give the daemon its full graceful-stop budget before systemd escalates to
	// SIGKILL (issue #688). The systemd default is DefaultTimeoutStopSec, which
	// a distribution can lower below the budget, so state the value here.
	b.WriteString("TimeoutStopSec=" + strconv.Itoa(serviceStopTimeoutSeconds) + "\n\n")

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
