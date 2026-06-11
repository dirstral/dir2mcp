package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/provider"
	"github.com/dirstral/dir2mcp/internal/secrets"
	"github.com/dirstral/dir2mcp/internal/setupwizard"
)

func (a *App) emitConfigCreatedMessage(global globalOptions, configPath string, created bool) {
	if global.quiet || global.jsonOutput {
		return
	}
	s := a.sty(false)
	if created {
		writef(a.stdout, "%s created %s with baseline settings\n", s.Success.Render("✓"), configPath)
	} else {
		writef(a.stdout, "%s updated %s and ensured baseline settings are present\n", s.Success.Render("✓"), configPath)
	}
}

// confirmDestructive asks the user to confirm a destructive action. On an
// interactive TTY (and not --non-interactive/--json/--quiet) it shows a
// brand-themed yes/no prompt and returns the choice; otherwise it returns true
// so scripts and automation proceed without blocking. A cancelled prompt
// (Ctrl-C) declines.
func (a *App) confirmDestructive(global globalOptions, title, description string) bool {
	if global.nonInteractive || global.jsonOutput || global.quiet ||
		!isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return true
	}
	ok, err := setupwizard.Confirm(title, description, false)
	if err != nil {
		return false
	}
	return ok
}

// setupWizardEligible reports whether the interactive huh setup form should
// run: only on a real TTY and never under --non-interactive / --json / --quiet
// (which must stay scriptable and prompt-free).
func (a *App) setupWizardEligible(global globalOptions) bool {
	if global.nonInteractive || global.jsonOutput || global.quiet {
		return false
	}
	return isTerminal(os.Stdin) && isTerminal(os.Stdout)
}

// secretWriter returns the credential-upsert function for the chosen
// destination: the .env.local writer for DestFile, or an OS keychain writer for
// DestKeychain (path argument ignored).
func secretWriter(dest setupwizard.SecretDest) func(path, keyName, value string) error {
	if dest == setupwizard.DestKeychain {
		return func(_ string, keyName, value string) error {
			return secrets.Set(secrets.DefaultService, keyName, value)
		}
	}
	return saveEnvLocalKey
}

// emitWizardSummary reports what the wizard persisted (saved credentials, where,
// and the applied corpus profile). No-op when nothing was collected or under
// quiet/JSON output.
func (a *App) emitWizardSummary(global globalOptions, envPath string, savedKeys []string, dest setupwizard.SecretDest, profile setupwizard.Profile) {
	if global.quiet || global.jsonOutput {
		return
	}
	s := a.sty(false)
	if len(savedKeys) > 0 {
		location := envPath
		if dest == setupwizard.DestKeychain {
			location = fmt.Sprintf("the OS keychain (service %q)", secrets.DefaultService)
		}
		writef(a.stdout, "%s saved %s to %s\n", s.Success.Render("✓"), strings.Join(savedKeys, ", "), location)
	}
	if profile != "" && profile != setupwizard.ProfileGeneral && profile != setupwizard.ProfileKeep {
		writef(a.stdout, "%s applied corpus profile: %s\n", s.Success.Render("✓"), profile)
	}
}

// containsString reports whether s is present in list.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (a *App) runConfig(ctx context.Context, global globalOptions, args []string) int {
	if len(args) == 0 {
		writeln(a.stdout, "config command: supported subcommands are init, print, set-secret, rm-secret, secrets")
		return exitSuccess
	}
	switch args[0] {
	case "init":
		return a.runConfigInit(global, args[1:])
	case "set-secret":
		return a.runConfigSetSecret(global, args[1:])
	case "rm-secret":
		return a.runConfigRmSecret(global, args[1:])
	case "secrets":
		return a.runConfigListSecrets(global, args[1:])
	case "print":
		cfg, err := loadConfigWithGlobalOptions(global)
		if err != nil {
			writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("load config: %v", err))
			return exitConfigInvalid
		}
		if global.quiet {
			return exitSuccess
		}
		embedProvider, embedReady := "none", false
		if ep, perr := cfg.Providers().Resolve(provider.CapEmbed); perr == nil {
			embedProvider, embedReady = ep.Name, true
		}
		writef(
			a.stdout,
			"root=%s state_dir=%s listen=%s mcp_path=%s embed_provider=%s embed_ready=%t\n",
			cfg.RootDir,
			cfg.StateDir,
			cfg.ListenAddr,
			cfg.MCPPath,
			embedProvider,
			embedReady,
		)
	default:
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("unknown config subcommand: %s", args[0]))
		return exitConfigInvalid
	}
	return exitSuccess
}

func (a *App) runConfigInit(global globalOptions, args []string) int {
	if len(args) > 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("config init does not accept arguments: %s", strings.Join(args, " ")))
		return exitConfigInvalid
	}

	configPath := resolveConfigPath(global)
	cfg := config.Default()
	cfg = applyGlobalPathOverrides(cfg, global)
	created := false

	if _, err := os.Stat(configPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("stat config: %v", err))
			return exitGeneric
		}
		created = true
	} else {
		existing, err := config.LoadFile(configPath)
		if err != nil {
			writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("load config file: %v", err))
			return exitConfigInvalid
		}
		cfg = existing
	}
	cfg = applyGlobalPathOverrides(cfg, global)

	// Interactive setup wizard (SPEC §§ config init / charmbracelet/huh): on a
	// TTY, collect provider credentials and a corpus profile before persisting
	// the config so the chosen profile lands in the snapshot. Credentials go to
	// .env.local only — never the .dir2mcp.yaml snapshot. Non-TTY / --json /
	// --quiet / --non-interactive paths skip the form and keep prior behavior.
	envPath := filepath.Join(filepath.Dir(configPath), ".env.local")
	savedKeys, chosenProfile, dest, exitCode := a.runConfigInitWizard(global, configPath, envPath, !created, &cfg)
	if exitCode >= 0 {
		return exitCode
	}

	if err := config.SaveFile(configPath, cfg); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("save config file: %v", err))
		return exitGeneric
	}

	a.emitConfigCreatedMessage(global, configPath, created)
	a.emitWizardSummary(global, envPath, savedKeys, dest, chosenProfile)
	if a.setupWizardEligible(global) {
		a.emitSetupVerification(global)
	}

	apiKeySaved := containsString(savedKeys, "MISTRAL_API_KEY")

	// Base the credential hint on whether an embed provider actually resolves
	// (honoring .env.local and any configured provider), not just the current
	// process env — otherwise we nag for MISTRAL_API_KEY even when setup is
	// already usable (e.g. key in .env.local, or Gemini/OpenAI configured).
	nextSteps := []string{}
	if !a.embedProviderResolves(global) {
		nextSteps = append(nextSteps, "Set env: export MISTRAL_API_KEY=<your-key>")
		nextSteps = append(nextSteps, "Or add MISTRAL_API_KEY=<key> to .env.local in this directory")
	}
	nextSteps = append(nextSteps, "Run: dir2mcp up")

	if global.jsonOutput {
		payload := map[string]interface{}{
			"path":          configPath,
			"created":       created,
			"updated":       !created,
			"api_key_saved": apiKeySaved,
			"next_steps":    nextSteps,
		}
		if err := emitJSON(a.stdout, payload); err != nil {
			writeCLIError(a.stderr, true, exitGeneric, fmt.Sprintf("encode config init json: %v", err))
			return exitGeneric
		}
		return exitSuccess
	}

	if !global.quiet && len(nextSteps) > 0 {
		s := a.sty(false)
		writef(a.stdout, "\n%s\n", s.sectionHeader("Next steps"))
		for _, step := range nextSteps {
			writef(a.stdout, "  %s %s\n", s.dim("•"), step)
		}
	}
	writeln(a.stdout)
	return exitSuccess
}

// runConfigInitWizard runs the interactive setup form when eligible, applies
// the chosen corpus profile to cfg, and persists collected credentials to
// .env.local. It returns the saved env-var names and chosen profile, plus an
// exitCode: a negative exitCode means "continue" (form ran, was skipped, or the
// user aborted into a baseline write); a non-negative exitCode is terminal and
// the caller should return it.
func (a *App) runConfigInitWizard(global globalOptions, configPath, envPath string, configExisted bool, cfg *config.Config) (savedKeys []string, profile setupwizard.Profile, dest setupwizard.SecretDest, exitCode int) {
	exitCode = -1
	dest = setupwizard.DestFile
	if !a.setupWizardEligible(global) {
		return savedKeys, profile, dest, exitCode
	}

	res, err := setupwizard.Run(setupwizard.Input{
		ExistingKeys:  setupwizard.DetectExistingKeys(envPath),
		ConfigExisted: configExisted,
	})
	switch {
	case errors.Is(err, huh.ErrUserAborted):
		writeln(a.stderr, "setup cancelled; writing baseline config only")
	case err != nil:
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("setup wizard: %v", err))
		return savedKeys, profile, dest, exitGeneric
	default:
		setupwizard.ApplyCorpusProfile(cfg, res.Profile)
		profile = res.Profile
		dest = res.Destination
		savedKeys, err = setupwizard.PersistKeys(envPath, res.Keys, secretWriter(dest))
		if err != nil {
			writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("save credentials: %v", err))
			return savedKeys, profile, dest, exitGeneric
		}
		if dest == setupwizard.DestFile {
			a.protectSecretsFromGit(filepath.Dir(configPath))
		}
	}
	return savedKeys, profile, dest, exitCode
}

// protectSecretsFromGit appends .env.local and the state dir to .gitignore when
// dir is inside a git repository, so freshly-saved credentials are not committed
// by accident. Best-effort: failures are surfaced as a warning, not fatal.
func (a *App) protectSecretsFromGit(dir string) {
	if !insideGitRepo(dir) {
		return // not a git repo — nothing to protect
	}
	if err := setupwizard.EnsureGitignoreEntries(dir, ".env.local", ".dir2mcp/"); err != nil {
		writeln(a.stderr, fmt.Sprintf("warning: could not update .gitignore: %v", err))
	}
}

// insideGitRepo reports whether dir or any ancestor contains a .git entry — a
// directory for a normal clone, or a file for a worktree/submodule. Walking the
// ancestry matters because `config init` / `up` may run from a subdirectory of
// the repo.
func insideGitRepo(dir string) bool {
	dir = filepath.Clean(dir)
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// embedProviderResolves reports whether an embedding provider resolves from the
// freshly written config (loading .env.local + any providers: block). Used to
// decide whether to print the credential next-step hint.
func (a *App) embedProviderResolves(global globalOptions) bool {
	cfg, err := loadConfigWithGlobalOptions(global)
	if err != nil {
		return false
	}
	_, rerr := cfg.Providers().Resolve(provider.CapEmbed)
	return rerr == nil
}

// emitSetupVerification resolves the embed/chat providers from the freshly
// written config and prints a one-line readiness summary (resolution only, no
// network calls — matching `dir2mcp doctor`). Best-effort and silent under
// quiet/JSON output.
func (a *App) emitSetupVerification(global globalOptions) {
	if global.quiet || global.jsonOutput {
		return
	}
	cfg, err := loadConfigWithGlobalOptions(global)
	if err != nil {
		return
	}
	s := a.sty(false)
	for _, c := range []struct {
		label string
		cap   provider.Capability
	}{
		{"embed", provider.CapEmbed},
		{"chat", provider.CapChat},
	} {
		chk := providerCheck(cfg, c.label, c.cap, false)
		switch chk.Status {
		case doctorStatusOK:
			writef(a.stdout, "%s %s provider ready (%s)\n", s.Success.Render("✓"), c.label, chk.Detail)
		default:
			writef(a.stdout, "%s %s provider not configured\n", s.dim("•"), c.label)
		}
	}
}

// runConfigSetSecret stores a provider credential in the OS keychain (SPEC
// §16.1.1 source #2), so it lives encrypted at rest instead of in a plaintext
// .env.local. The value is read from a hidden TTY prompt, or from stdin when
// piped (e.g. `op read ... | dir2mcp config set-secret MISTRAL_API_KEY`).
func (a *App) runConfigSetSecret(global globalOptions, args []string) int {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, "usage: dir2mcp config set-secret <ENV_VAR>")
		return exitConfigInvalid
	}
	key := strings.TrimSpace(args[0])
	value, code := a.readSecretValue(global, key)
	if code != exitSuccess {
		return code
	}
	if err := secrets.Set(secrets.DefaultService, key, value); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("store secret in keychain: %v", err))
		return exitGeneric
	}
	if !global.quiet && !global.jsonOutput {
		s := a.sty(false)
		writef(a.stdout, "%s stored %s in the OS keychain (service %q)\n", s.Success.Render("✓"), key, secrets.DefaultService)
		if !secrets.IsManaged(key) {
			writef(a.stdout, "  note: %s is not a built-in provider credential, so it is not auto-loaded at startup.\n", key)
		}
	}
	return exitSuccess
}

// runConfigRmSecret removes a credential previously stored with set-secret.
func (a *App) runConfigRmSecret(global globalOptions, args []string) int {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, "usage: dir2mcp config rm-secret <ENV_VAR>")
		return exitConfigInvalid
	}
	key := strings.TrimSpace(args[0])
	if err := secrets.Delete(secrets.DefaultService, key); err != nil {
		if secrets.IsNotFound(err) {
			writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("%s is not stored in the keychain", key))
			return exitGeneric
		}
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("remove secret from keychain: %v", err))
		return exitGeneric
	}
	if !global.quiet && !global.jsonOutput {
		s := a.sty(false)
		writef(a.stdout, "%s removed %s from the OS keychain\n", s.Success.Render("✓"), key)
	}
	return exitSuccess
}

// runConfigListSecrets reports, for each built-in provider credential, whether
// it is present in the keychain and/or the current environment. Never prints
// secret values.
func (a *App) runConfigListSecrets(global globalOptions, args []string) int {
	if len(args) > 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, "config secrets does not accept arguments")
		return exitConfigInvalid
	}
	type secretStatus struct {
		Key        string `json:"key"`
		InKeychain bool   `json:"in_keychain"`
		InEnv      bool   `json:"in_env"`
	}
	managed := secrets.ManagedEnvVars()
	statuses := make([]secretStatus, 0, len(managed))
	for _, k := range managed {
		statuses = append(statuses, secretStatus{
			Key:        k,
			InKeychain: secrets.Has(secrets.DefaultService, k),
			InEnv:      strings.TrimSpace(os.Getenv(k)) != "",
		})
	}

	if global.jsonOutput {
		if err := emitJSON(a.stdout, map[string]interface{}{"service": secrets.DefaultService, "secrets": statuses}); err != nil {
			writeCLIError(a.stderr, true, exitGeneric, fmt.Sprintf("encode secrets json: %v", err))
			return exitGeneric
		}
		return exitSuccess
	}
	if global.quiet {
		return exitSuccess
	}

	s := a.sty(false)
	writef(a.stdout, "%s (keychain service %q)\n", s.sectionHeader("Provider credentials"), secrets.DefaultService)
	for _, st := range statuses {
		mark := func(present bool) string {
			if present {
				return s.Success.Render("✓")
			}
			return s.dim("·")
		}
		writef(a.stdout, "  %s keychain  %s env   %s\n", mark(st.InKeychain), mark(st.InEnv), st.Key)
	}
	return exitSuccess
}

// readSecretValue reads a secret from a hidden TTY prompt when interactive, or
// from piped stdin otherwise. It never blocks on a terminal in non-interactive
// mode (that would hang automation): a TTY with no piped value is an error.
func (a *App) readSecretValue(global globalOptions, key string) (string, int) {
	if isTerminal(os.Stdin) && isTerminal(os.Stdout) && !global.nonInteractive {
		value, err := setupwizard.PromptSecret(key, "stored encrypted in the OS keychain")
		if err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, "cancelled; nothing stored")
				return "", exitConfigInvalid
			}
			writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("read secret: %v", err))
			return "", exitGeneric
		}
		if value == "" {
			writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, "empty value; nothing stored")
			return "", exitConfigInvalid
		}
		return value, exitSuccess
	}

	// Non-interactive: require a piped/redirected value rather than blocking on
	// a terminal read.
	if isTerminal(os.Stdin) {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid,
			"no value provided: pipe the secret on stdin (e.g. `echo $KEY | dir2mcp config set-secret ...`) or run interactively")
		return "", exitConfigInvalid
	}

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("read secret from stdin: %v", err))
		return "", exitGeneric
	}
	value := strings.TrimSpace(line)
	if value == "" {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, "no value provided on stdin")
		return "", exitConfigInvalid
	}
	return value, exitSuccess
}
