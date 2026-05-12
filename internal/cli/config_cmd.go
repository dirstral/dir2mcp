package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dirstral/dir2mcp/internal/config"
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
		writef(
			a.stdout,
			"root=%s state_dir=%s listen=%s mcp_path=%s mistral_base_url=%s mistral_api_key_set=%t\n",
			cfg.RootDir,
			cfg.StateDir,
			cfg.ListenAddr,
			cfg.MCPPath,
			cfg.MistralBaseURL,
			cfg.MistralAPIKey != "",
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
