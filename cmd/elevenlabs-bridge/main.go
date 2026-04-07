package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"dir2mcp/internal/elevenlabsbridge"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	defaultCfg, err := elevenlabsbridge.LoadConfigFromOSEnv()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return 2
	}

	listenFlag := flag.NewFlagSet("elevenlabs-bridge", flag.ContinueOnError)
	listenFlag.SetOutput(os.Stderr)
	mcpURL := listenFlag.String("mcp-url", defaultCfg.MCPURL, "dir2mcp MCP endpoint URL")
	mcpToken := listenFlag.String("mcp-token", defaultCfg.MCPToken, "dir2mcp bearer token")
	stateDir := listenFlag.String("state-dir", defaultCfg.StateDir, "dir2mcp state directory")
	port := listenFlag.Int("port", defaultCfg.Port, "bridge listen port")
	listenAddr := listenFlag.String("listen", "", "override listen address (host:port)")
	if err := listenFlag.Parse(args); err != nil {
		return 2
	}

	cfg := defaultCfg
	if strings.TrimSpace(*mcpURL) != "" {
		cfg.MCPURL = strings.TrimSpace(*mcpURL)
	}
	if strings.TrimSpace(*mcpToken) != "" {
		cfg.MCPToken = strings.TrimSpace(*mcpToken)
	}
	if strings.TrimSpace(*stateDir) != "" {
		cfg.StateDir = strings.TrimSpace(*stateDir)
	}
	if *port > 0 {
		cfg.Port = *port
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		_, _ = fmt.Fprintf(os.Stderr, "ERROR: invalid port %d\n", cfg.Port)
		return 2
	}

	listen := strings.TrimSpace(*listenAddr)
	if listen == "" {
		listen = cfg.EffectiveListenAddr()
	}

	bridge, err := elevenlabsbridge.New(cfg)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintf(os.Stdout, "elevenlabs-bridge: listening on %s\n", listen)
	_, _ = fmt.Fprintf(os.Stdout, "MCP_URL=%s\n", cfg.MCPURL)
	_, _ = fmt.Fprintf(os.Stdout, "STATE_DIR=%s\n", cfg.StateDir)
	_, _ = fmt.Fprintf(os.Stdout, "MCP token source=%s\n", bridge.TokenSource())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := elevenlabsbridge.Run(ctx, cfg, listen); err != nil && ctx.Err() == nil {
		_, _ = fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return 1
	}
	return 0
}
