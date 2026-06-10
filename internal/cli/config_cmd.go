package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/provider"
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

// setupWizardEligible reports whether the interactive huh setup form should
// run: only on a real TTY and never under --non-interactive / --json / --quiet
// (which must stay scriptable and prompt-free).
func (a *App) setupWizardEligible(global globalOptions) bool {
	if global.nonInteractive || global.jsonOutput || global.quiet {
		return false
	}
	return isTerminal(os.Stdin) && isTerminal(os.Stdout)
}

// persistWizardKeys writes each non-empty collected credential to .env.local
// (secrets only ever live here, never in the YAML snapshot). It iterates in the
// wizardProviderKeys order for deterministic writes and returns the env var
// names that were saved.
func persistWizardKeys(envPath string, keys map[string]string) ([]string, error) {
	saved := make([]string, 0, len(keys))
	for _, spec := range wizardProviderKeys {
		v, ok := keys[spec.EnvVar]
		if !ok || strings.TrimSpace(v) == "" {
			continue
		}
		if err := saveEnvLocalKey(envPath, spec.EnvVar, strings.TrimSpace(v)); err != nil {
			return saved, err
		}
		saved = append(saved, spec.EnvVar)
	}
	return saved, nil
}

// emitWizardSummary reports what the wizard persisted (saved credentials and
// the applied corpus profile). No-op when nothing was collected or under
// quiet/JSON output.
func (a *App) emitWizardSummary(global globalOptions, envPath string, savedKeys []string, profile corpusProfile) {
	if global.quiet || global.jsonOutput {
		return
	}
	s := a.sty(false)
	if len(savedKeys) > 0 {
		writef(a.stdout, "%s saved %s to %s\n", s.Success.Render("✓"), strings.Join(savedKeys, ", "), envPath)
	}
	if profile != "" && profile != corpusProfileGeneral {
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
		writeln(a.stdout, "config command: supported subcommands are init and print")
		return exitSuccess
	}
	switch args[0] {
	case "init":
		return a.runConfigInit(global, args[1:])
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
	savedKeys, chosenProfile, exitCode := a.runConfigInitWizard(global, envPath, &cfg)
	if exitCode >= 0 {
		return exitCode
	}

	if err := config.SaveFile(configPath, cfg); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("save config file: %v", err))
		return exitGeneric
	}

	a.emitConfigCreatedMessage(global, configPath, created)
	a.emitWizardSummary(global, envPath, savedKeys, chosenProfile)

	mistralPresent := strings.TrimSpace(os.Getenv("MISTRAL_API_KEY")) != "" || containsString(savedKeys, "MISTRAL_API_KEY")
	apiKeySaved := containsString(savedKeys, "MISTRAL_API_KEY")

	nextSteps := []string{}
	if !mistralPresent {
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
func (a *App) runConfigInitWizard(global globalOptions, envPath string, cfg *config.Config) (savedKeys []string, profile corpusProfile, exitCode int) {
	exitCode = -1
	if !a.setupWizardEligible(global) {
		return savedKeys, profile, exitCode
	}

	res, err := runSetupWizard()
	switch {
	case errors.Is(err, huh.ErrUserAborted):
		writeln(a.stderr, "setup cancelled; writing baseline config only")
	case err != nil:
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("setup wizard: %v", err))
		return savedKeys, profile, exitGeneric
	default:
		applyCorpusProfile(cfg, res.Profile)
		profile = res.Profile
		savedKeys, err = persistWizardKeys(envPath, res.Keys)
		if err != nil {
			writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("save .env.local: %v", err))
			return savedKeys, profile, exitGeneric
		}
	}
	return savedKeys, profile, exitCode
}
