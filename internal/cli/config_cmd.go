package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/provider"
	"github.com/dirstral/dir2mcp/internal/secrets"
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

func (a *App) promptAndSaveMistralAPIKey(global globalOptions, configPath string, apiKeySet bool) (saved bool) {
	if global.nonInteractive || global.jsonOutput || apiKeySet ||
		!isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return false
	}
	s := a.sty(false)
	writef(a.stdout, "\n%s\n", s.sectionHeader("Mistral API Key"))
	writef(a.stdout, "  Get one free at https://console.mistral.ai/api-keys\n\n")
	writef(a.stdout, "  MISTRAL_API_KEY (leave blank to skip): ")
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	key := strings.TrimSpace(line)
	if key == "" {
		return false
	}
	envPath := filepath.Join(filepath.Dir(configPath), ".env.local")
	if err := saveEnvLocalKey(envPath, "MISTRAL_API_KEY", key); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("save .env.local: %v", err))
		return false
	}
	if !global.quiet {
		writef(a.stdout, "%s saved MISTRAL_API_KEY to %s\n", s.Success.Render("✓"), envPath)
	}
	return true
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

	if err := config.SaveFile(configPath, cfg); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("save config file: %v", err))
		return exitGeneric
	}

	// Print config file result immediately so it appears before any prompt.
	a.emitConfigCreatedMessage(global, configPath, created)

	// Prompt for the Mistral API key when running interactively and the key is
	// not already present in the environment.
	apiKeySet := strings.TrimSpace(os.Getenv("MISTRAL_API_KEY")) != ""
	apiKeySaved := a.promptAndSaveMistralAPIKey(global, configPath, apiKeySet)

	nextSteps := []string{}
	if !apiKeySet && !apiKeySaved {
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
	statuses := make([]secretStatus, 0, len(secrets.ManagedEnvVars))
	for _, k := range secrets.ManagedEnvVars {
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
// from the first line of stdin when piped/non-interactive.
func (a *App) readSecretValue(global globalOptions, key string) (string, int) {
	if isTerminal(os.Stdin) && !global.nonInteractive {
		writef(a.stderr, "Enter value for %s (input hidden): ", key)
		raw, err := term.ReadPassword(int(os.Stdin.Fd()))
		writeln(a.stderr)
		if err != nil {
			writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("read secret: %v", err))
			return "", exitGeneric
		}
		value := strings.TrimSpace(string(raw))
		if value == "" {
			writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, "empty value; nothing stored")
			return "", exitConfigInvalid
		}
		return value, exitSuccess
	}

	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	value := strings.TrimSpace(line)
	if value == "" {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, "no value provided on stdin")
		return "", exitConfigInvalid
	}
	return value, exitSuccess
}
