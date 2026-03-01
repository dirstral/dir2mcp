package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"dir2mcp/internal/dirstral/chat"
	"dir2mcp/internal/dirstral/config"
	"dir2mcp/internal/dirstral/host"
	"dir2mcp/internal/dirstral/mcp"
	"dir2mcp/internal/dirstral/settings"
	"dir2mcp/internal/dirstral/voice"
	"github.com/spf13/cobra"
)

func Execute() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	root := newRootCommand(cfg)
	return root.Execute()
}

func newRootCommand(cfg config.Config) *cobra.Command {
	root := &cobra.Command{
		Use:   "dirstral",
		Short: "Terminal-first MCP coding companion",
		RunE: func(cmd *cobra.Command, args []string) error {
			for {
				result, err := RunMenu(screenStart)
				if err != nil {
					return err
				}
				choice := StartChoice(result.Chosen)
				switch choice {
				case ChoiceChat:
					printModeHeader("Chat")
					printModeFeedback("Chat", runChat(cmd.Context(), cfg))
				case ChoiceVoice:
					printModeHeader("Voice")
					mcpURL := ResolveMCPURL(cfg.MCP.URL, "", false, cfg.MCP.Transport)
					opts := BuildVoiceOptions(cfg, mcpURL, cfg.ElevenLabs.Voice, "", false, cfg.Verbose, cfg.ElevenLabs.BaseURL)
					printModeFeedback("Voice", voice.Run(cmd.Context(), opts))
				case ChoiceServer:
					if err := runServerMenu(cmd.Context(), cfg); err != nil {
						printUIError(err)
					}
				case ChoiceSettings:
					printModeHeader("Settings")
					err := settings.Run(cfg)
					if refreshed, loadErr := config.Load(); loadErr == nil {
						cfg = refreshed
					} else {
						printUIError(fmt.Errorf("reload config: %w", loadErr))
					}
					printModeFeedback("Settings", err)
				default:
					fmt.Println(styleMuted.Render("bye"))
					return nil
				}
			}
		},
	}

	root.AddCommand(newChatCommand(cfg))
	root.AddCommand(newVoiceCommand(cfg))
	root.AddCommand(newServerCommand(cfg))
	root.AddCommand(newManifestCommand(cfg))
	return root
}

func printModeHeader(mode string) {
	fmt.Println(statusLine("Opening", mode))
	fmt.Println()
}

func printReturnTo(label string) {
	fmt.Println()
	fmt.Println(statusLine("Transition", "Returning to "+label))
}

func printUIError(err error) {
	fmt.Println(errorLine(err))
}

// ModeFeedback is rendered after a mode exits to keep transitions predictable.
type ModeFeedback struct {
	Message  string
	Recovery string
	IsError  bool
}

// BuildModeFeedback classifies a mode result for friendly transition output.
func BuildModeFeedback(mode string, err error) ModeFeedback {
	if err == nil {
		return ModeFeedback{
			Message:  fmt.Sprintf("%s closed", mode),
			Recovery: fmt.Sprintf("Select %s again to continue, or choose Exit to leave Dirstral.", mode),
		}
	}
	if errors.Is(err, context.Canceled) {
		return ModeFeedback{
			Message:  fmt.Sprintf("%s canceled", mode),
			Recovery: fmt.Sprintf("Select %s again to retry, or choose another mode.", mode),
		}
	}
	return ModeFeedback{
		Message:  fmt.Sprintf("%s failed: %v", mode, err),
		Recovery: fmt.Sprintf("Select %s to retry after fixing config/network, or choose another mode.", mode),
		IsError:  true,
	}
}

func printModeFeedback(mode string, err error) {
	printModeFeedbackTo(mode, err, "home")
}

func printModeFeedbackTo(mode string, err error, destination string) {
	feedback := BuildModeFeedback(mode, err)
	if feedback.IsError {
		printUIError(errors.New(feedback.Message))
	} else {
		fmt.Println(statusLine("Transition", feedback.Message))
	}
	fmt.Println(styleMuted.Render(feedback.Recovery))
	printReturnTo(destination)
}

func newChatCommand(cfg config.Config) *cobra.Command {
	options := chat.Options{MCPURL: cfg.MCP.URL, Transport: cfg.MCP.Transport, Model: cfg.Model, Verbose: cfg.Verbose, JSON: false}
	cmd := &cobra.Command{
		Use:   "chat",
		Short: "Start chat mode",
		RunE: func(cmd *cobra.Command, args []string) error {
			runOptions := options
			runOptions.MCPURL = ResolveMCPURL(cfg.MCP.URL, options.MCPURL, cmd.Flags().Changed("mcp"), runOptions.Transport)
			return chat.Run(cmd.Context(), runOptions)
		},
	}
	cmd.Flags().StringVar(&options.MCPURL, "mcp", options.MCPURL, "MCP server URL (streamable-http) or stdio command")
	cmd.Flags().StringVar(&options.Transport, "transport", options.Transport, "MCP transport (streamable-http|stdio)")
	cmd.Flags().StringVar(&options.Model, "model", options.Model, "Model name")
	cmd.Flags().BoolVar(&options.Verbose, "verbose", options.Verbose, "Verbose MCP logging")
	cmd.Flags().BoolVar(&options.JSON, "json", options.JSON, "Output NDJSON events instead of TUI")
	return cmd
}

func newVoiceCommand(cfg config.Config) *cobra.Command {
	var mcpURL string
	var voiceID string
	var device string
	var mute bool
	var verbose bool
	var baseURL string

	mcpURL = cfg.MCP.URL
	voiceID = cfg.ElevenLabs.Voice
	verbose = cfg.Verbose
	baseURL = cfg.ElevenLabs.BaseURL

	cmd := &cobra.Command{
		Use:   "voice",
		Short: "Start voice mode",
		RunE: func(cmd *cobra.Command, args []string) error {
			resolvedMCPURL := ResolveMCPURL(cfg.MCP.URL, mcpURL, cmd.Flags().Changed("mcp"), cfg.MCP.Transport)
			opts := BuildVoiceOptions(cfg, resolvedMCPURL, voiceID, device, mute, verbose, baseURL)
			return voice.Run(cmd.Context(), opts)
		},
	}
	cmd.Flags().StringVar(&mcpURL, "mcp", mcpURL, "MCP server URL")
	cmd.Flags().StringVar(&voiceID, "voice", voiceID, "Voice id or name")
	cmd.Flags().StringVar(&device, "device", "", "Audio input device")
	cmd.Flags().BoolVar(&mute, "mute", false, "Disable TTS playback")
	cmd.Flags().BoolVar(&verbose, "verbose", verbose, "Verbose logging")
	cmd.Flags().StringVar(&baseURL, "elevenlabs-base-url", baseURL, "ElevenLabs base URL")
	return cmd
}

func newServerCommand(cfg config.Config) *cobra.Command {
	var dir string
	var port int
	var listen string
	var mcpPath string
	var asJSON bool
	var remoteMCP string

	cmd := &cobra.Command{Use: "server", Short: "Start/stop local host or probe remote MCP"}
	start := &cobra.Command{
		Use:   "start",
		Short: "Start local dir2mcp and stream logs",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := host.UpOptions{
				Dir:     dir,
				Port:    port,
				Listen:  listen,
				MCPPath: mcpPath,
				JSON:    asJSON,
			}
			return host.Up(cmd.Context(), opts)
		},
	}
	start.Flags().StringVar(&dir, "dir", "", "Directory to serve/index")
	start.Flags().IntVar(&port, "port", 0, "Port for dir2mcp listen")
	start.Flags().StringVar(&listen, "listen", cfg.Host.Listen, "Listen host:port")
	start.Flags().StringVar(&mcpPath, "mcp-path", cfg.Host.MCPPath, "MCP endpoint path")
	start.Flags().BoolVar(&asJSON, "json", false, "Pass --json to dir2mcp up")

	status := &cobra.Command{Use: "status", Short: "Show local host status", RunE: func(cmd *cobra.Command, args []string) error {
		return host.Status()
	}}
	stop := &cobra.Command{Use: "stop", Short: "Stop local managed dir2mcp process", RunE: func(cmd *cobra.Command, args []string) error {
		return host.Down()
	}}
	remote := &cobra.Command{Use: "remote", Short: "Probe remote MCP endpoint health", RunE: func(cmd *cobra.Command, args []string) error {
		return host.StatusRemote(cmd.Context(), strings.TrimSpace(remoteMCP))
	}}
	remoteMCP = cfg.MCP.URL
	remote.Flags().StringVar(&remoteMCP, "mcp", remoteMCP, "Remote MCP URL (default: DIRSTRAL_MCP_URL/config mcp.url)")

	cmd.AddCommand(start, status, stop, remote)
	return cmd
}

func newManifestCommand(cfg config.Config) *cobra.Command {
	var mcpURL string
	var transport string
	var asJSON bool
	var verbose bool

	mcpURL = cfg.MCP.URL
	transport = cfg.MCP.Transport
	verbose = cfg.Verbose

	cmd := &cobra.Command{
		Use:   "manifest",
		Short: "Print MCP capability manifest",
		RunE: func(cmd *cobra.Command, args []string) error {
			resolvedMCPURL := ResolveMCPURL(cfg.MCP.URL, mcpURL, cmd.Flags().Changed("mcp"), transport)
			client := mcp.NewWithTransport(resolvedMCPURL, transport, verbose)
			defer func() {
				_ = client.Close()
			}()

			if err := client.Initialize(cmd.Context()); err != nil {
				return fmt.Errorf("mcp initialize failed: %w", err)
			}
			manifest, err := mcp.BuildCapabilityManifest(cmd.Context(), client)
			if err != nil {
				return fmt.Errorf("manifest build failed: %w", err)
			}

			if asJSON {
				payload, err := mcp.RenderCapabilityManifestJSON(manifest)
				if err != nil {
					return fmt.Errorf("manifest render failed: %w", err)
				}
				fmt.Println(string(payload))
				return nil
			}

			fmt.Println(mcp.RenderCapabilityManifestHuman(manifest))
			return nil
		},
	}

	cmd.Flags().StringVar(&mcpURL, "mcp", mcpURL, "MCP server URL (streamable-http) or stdio command")
	cmd.Flags().StringVar(&transport, "transport", transport, "MCP transport (streamable-http|stdio)")
	cmd.Flags().BoolVar(&asJSON, "json", asJSON, "Print JSON manifest")
	cmd.Flags().BoolVar(&verbose, "verbose", verbose, "Verbose MCP logging")
	return cmd
}

func runChat(ctx context.Context, cfg config.Config) error {
	mcpURL := ResolveMCPURL(cfg.MCP.URL, "", false, cfg.MCP.Transport)
	return chat.Run(ctx, chat.Options{MCPURL: mcpURL, Transport: cfg.MCP.Transport, Model: cfg.Model, Verbose: cfg.Verbose})
}

func ResolveMCPURL(defaultURL, explicitURL string, explicitOverride bool, transport string) string {
	defaultURL = strings.TrimSpace(defaultURL)
	explicitURL = strings.TrimSpace(explicitURL)
	if explicitOverride {
		if explicitURL == "" {
			return defaultURL
		}
		return explicitURL
	}
	if strings.EqualFold(strings.TrimSpace(transport), "stdio") {
		return defaultURL
	}
	if !shouldPreferManagedHost(defaultURL) {
		return defaultURL
	}
	health := host.CheckHealth()
	activeURL := strings.TrimSpace(health.MCPURL)
	if health.Ready && activeURL != "" {
		return activeURL
	}
	return defaultURL
}

func shouldPreferManagedHost(defaultURL string) bool {
	trimmed := strings.TrimSpace(defaultURL)
	if trimmed == "" {
		return true
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return true
	}
	hostname := strings.TrimSpace(u.Hostname())
	if hostname == "" {
		return true
	}
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}

func BuildVoiceOptions(cfg config.Config, mcpURL, voiceName, device string, mute, verbose bool, baseURL string) voice.Options {
	if mcpURL == "" {
		mcpURL = cfg.MCP.URL
	}
	if voiceName == "" {
		voiceName = cfg.ElevenLabs.Voice
	}
	if baseURL == "" {
		baseURL = cfg.ElevenLabs.BaseURL
	}
	return voice.Options{
		MCPURL:    mcpURL,
		Transport: cfg.MCP.Transport,
		Model:     cfg.Model,
		Voice:     voiceName,
		Device:    device,
		Mute:      mute,
		Verbose:   verbose,
		BaseURL:   baseURL,
	}
}

func runServerMenu(ctx context.Context, cfg config.Config) error {
	for {
		result, err := RunMenu(screenServer)
		if err != nil {
			return err
		}
		switch result.Chosen {
		case serverActionStart:
			printModeHeader("Start/Stop MCP Server / Start MCP Server")
			if err := host.UpDetached(ctx, host.UpOptions{Listen: cfg.Host.Listen, MCPPath: cfg.Host.MCPPath}); err != nil {
				printModeFeedbackTo("MCP server start", err, "Start/Stop MCP Server menu")
				continue
			}
			if health := host.CheckHealth(); strings.TrimSpace(health.MCPURL) != "" {
				fmt.Println(statusLine("MCP Server", "Active endpoint: "+health.MCPURL))
			}
			fmt.Println(styleMuted.Render("MCP server started in background. Use Status for readiness details."))
			printReturnTo("Start/Stop MCP Server menu")
		case serverActionStatus:
			printModeHeader("Start/Stop MCP Server / Status")
			if err := host.Status(); err != nil {
				printModeFeedbackTo("MCP server status", err, "Start/Stop MCP Server menu")
				continue
			}
			printReturnTo("Start/Stop MCP Server menu")
		case serverActionLogs:
			printModeHeader("Start/Stop MCP Server / Logs")
			if err := runServerLogViewer(); err != nil {
				printModeFeedbackTo("MCP server logs", err, "Start/Stop MCP Server menu")
				continue
			}
			printReturnTo("Start/Stop MCP Server menu")
		case serverActionRemote:
			printModeHeader("Start/Stop MCP Server / Remote")
			if err := host.StatusRemote(ctx, strings.TrimSpace(cfg.MCP.URL)); err != nil {
				printModeFeedbackTo("MCP server remote", err, "Start/Stop MCP Server menu")
				continue
			}
			printReturnTo("Start/Stop MCP Server menu")
		case serverActionStop:
			printModeHeader("Start/Stop MCP Server / Stop MCP Server")
			if err := host.Down(); err != nil {
				printModeFeedbackTo("MCP server stop", err, "Start/Stop MCP Server menu")
				continue
			}
			printReturnTo("Start/Stop MCP Server menu")
		default:
			return nil
		}
	}
}
