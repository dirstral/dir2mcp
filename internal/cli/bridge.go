package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"dir2mcp/internal/elevenlabsbridge"
)

func (a *App) runBridge(ctx context.Context, global globalOptions, args []string) int {
	if len(args) == 0 {
		writeln(a.stdout, "bridge command: supported subcommands are elevenlabs")
		return exitSuccess
	}
	switch args[0] {
	case "elevenlabs":
		return a.runBridgeElevenLabs(ctx, global, args[1:])
	default:
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("unknown bridge subcommand: %s", args[0]))
		return exitConfigInvalid
	}
}

func (a *App) runBridgeElevenLabs(ctx context.Context, global globalOptions, args []string) int {
	defaultCfg, err := loadBridgeDefaultConfig(global)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, err.Error())
		return exitConfigInvalid
	}

	cfg, listen, parseCode := a.parseBridgeElevenLabsFlags(global, defaultCfg, args)
	if parseCode != exitSuccess {
		return parseCode
	}

	bridge, err := elevenlabsbridge.New(cfg)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, err.Error())
		return exitGeneric
	}

	if global.jsonOutput {
		if err := emitJSON(a.stdout, map[string]interface{}{
			"mode":         "bridge.elevenlabs",
			"listen":       listen,
			"mcp_url":      cfg.MCPURL,
			"state_dir":    cfg.StateDir,
			"token_source": bridge.TokenSource(),
		}); err != nil {
			writeCLIError(a.stderr, true, exitGeneric, fmt.Sprintf("encode bridge json: %v", err))
			return exitGeneric
		}
	} else if !global.quiet {
		_, _ = fmt.Fprintf(a.stdout, "elevenlabs-bridge: listening on %s\n", listen)
		_, _ = fmt.Fprintf(a.stdout, "MCP_URL=%s\n", cfg.MCPURL)
		_, _ = fmt.Fprintf(a.stdout, "STATE_DIR=%s\n", cfg.StateDir)
		_, _ = fmt.Fprintf(a.stdout, "MCP token source=%s\n", bridge.TokenSource())
	}

	if err := elevenlabsbridge.RunWithBridge(ctx, bridge, listen); err != nil && ctx.Err() == nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, err.Error())
		return exitGeneric
	}
	return exitSuccess
}

func loadBridgeDefaultConfig(global globalOptions) (elevenlabsbridge.Config, error) {
	env := map[string]string{
		"MCP_URL":   os.Getenv("MCP_URL"),
		"MCP_TOKEN": os.Getenv("MCP_TOKEN"),
		"STATE_DIR": os.Getenv("STATE_DIR"),
		"PORT":      os.Getenv("PORT"),
	}
	if v := strings.TrimSpace(global.stateDir); v != "" {
		env["STATE_DIR"] = v
	}
	return elevenlabsbridge.LoadConfigFromEnv(env)
}

func (a *App) parseBridgeElevenLabsFlags(global globalOptions, defaultCfg elevenlabsbridge.Config, args []string) (cfg elevenlabsbridge.Config, listen string, code int) {
	fs := flag.NewFlagSet("bridge elevenlabs", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	mcpURL := fs.String("mcp-url", defaultCfg.MCPURL, "dir2mcp MCP endpoint URL")
	mcpToken := fs.String("mcp-token", defaultCfg.MCPToken, "dir2mcp bearer token")
	stateDir := fs.String("state-dir", defaultCfg.StateDir, "dir2mcp state directory")
	port := fs.Int("port", defaultCfg.Port, "bridge listen port")
	listenAddr := fs.String("listen", "", "override listen address (host:port)")
	if err := fs.Parse(args); err != nil {
		return elevenlabsbridge.Config{}, "", exitConfigInvalid
	}
	if fs.NArg() > 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("bridge elevenlabs does not accept positional arguments: %s", strings.Join(fs.Args(), " ")))
		return elevenlabsbridge.Config{}, "", exitConfigInvalid
	}

	cfg = defaultCfg
	cfg.MCPURL = strings.TrimSpace(*mcpURL)
	cfg.MCPToken = strings.TrimSpace(*mcpToken)
	cfg.StateDir = strings.TrimSpace(*stateDir)
	portProvided := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "port" {
			portProvided = true
		}
	})
	if portProvided {
		cfg.Port = *port
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("invalid port %d", cfg.Port))
		return elevenlabsbridge.Config{}, "", exitConfigInvalid
	}
	if cfg.StateDir == "" {
		cfg.StateDir = elevenlabsbridge.DefaultStateDirName
	}

	listen = strings.TrimSpace(*listenAddr)
	if listen == "" {
		listen = cfg.EffectiveListenAddr()
	}
	return cfg, listen, exitSuccess
}
