package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"dir2mcp/internal/dirstral/breeze"
	"dir2mcp/internal/dirstral/config"
	"dir2mcp/internal/dirstral/host"
	"dir2mcp/internal/dirstral/mcp"
	"dir2mcp/internal/dirstral/settings"
	"dir2mcp/internal/dirstral/tempest"
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
				case ChoiceBreeze:
					printModeHeader("Breeze")
					printModeFeedback("Breeze", runBreeze(cmd.Context(), cfg))
				case ChoiceTempest:
					printModeHeader("Tempest")
					mcpURL := ResolveMCPURL(cfg.MCP.URL, "", false, cfg.MCP.Transport)
					opts := BuildTempestOptions(cfg, mcpURL, cfg.ElevenLabs.Voice, "", false, cfg.Verbose, cfg.ElevenLabs.BaseURL)
					printModeFeedback("Tempest", tempest.Run(cmd.Context(), opts))
				case ChoiceLighthouse:
					if err := runLighthouseMenu(cmd.Context(), cfg); err != nil {
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

	root.Version = Version
	root.AddCommand(newBreezeCommand(cfg))
	root.AddCommand(newTempestCommand(cfg))
	root.AddCommand(newLighthouseCommand(cfg))
	root.AddCommand(newManifestCommand(cfg))
	root.AddCommand(newVersionCommand())
	return root
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, args []string) {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "dirstral "+Version)
		},
	}
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

func newBreezeCommand(cfg config.Config) *cobra.Command {
	options := breeze.Options{MCPURL: cfg.MCP.URL, Transport: cfg.MCP.Transport, Model: cfg.Model, Verbose: cfg.Verbose, JSON: false}
	cmd := &cobra.Command{
		Use:   "breeze",
		Short: "Start text-to-text mode",
		RunE: func(cmd *cobra.Command, args []string) error {
			runOptions := options
			runOptions.MCPURL = ResolveMCPURL(cfg.MCP.URL, options.MCPURL, cmd.Flags().Changed("mcp"), runOptions.Transport)
			return breeze.Run(cmd.Context(), runOptions)
		},
	}
	cmd.Flags().StringVar(&options.MCPURL, "mcp", options.MCPURL, "MCP server URL (streamable-http) or stdio command")
	cmd.Flags().StringVar(&options.Transport, "transport", options.Transport, "MCP transport (streamable-http|stdio)")
	cmd.Flags().StringVar(&options.Model, "model", options.Model, "Model name")
	cmd.Flags().BoolVar(&options.Verbose, "verbose", options.Verbose, "Verbose MCP logging")
	cmd.Flags().BoolVar(&options.JSON, "json", options.JSON, "Output NDJSON events instead of TUI")
	return cmd
}

func newTempestCommand(cfg config.Config) *cobra.Command {
	var mcpURL string
	var voice string
	var device string
	var mute bool
	var verbose bool
	var baseURL string

	mcpURL = cfg.MCP.URL
	voice = cfg.ElevenLabs.Voice
	verbose = cfg.Verbose
	baseURL = cfg.ElevenLabs.BaseURL

	cmd := &cobra.Command{
		Use:   "tempest",
		Short: "Start voice mode",
		RunE: func(cmd *cobra.Command, args []string) error {
			resolvedMCPURL := ResolveMCPURL(cfg.MCP.URL, mcpURL, cmd.Flags().Changed("mcp"), cfg.MCP.Transport)
			opts := BuildTempestOptions(cfg, resolvedMCPURL, voice, device, mute, verbose, baseURL)
			return tempest.Run(cmd.Context(), opts)
		},
	}
	cmd.Flags().StringVar(&mcpURL, "mcp", mcpURL, "MCP server URL")
	cmd.Flags().StringVar(&voice, "voice", voice, "Voice id or name")
	cmd.Flags().StringVar(&device, "device", "", "Audio input device")
	cmd.Flags().BoolVar(&mute, "mute", false, "Disable TTS playback")
	cmd.Flags().BoolVar(&verbose, "verbose", verbose, "Verbose logging")
	cmd.Flags().StringVar(&baseURL, "elevenlabs-base-url", baseURL, "ElevenLabs base URL")
	return cmd
}

func newLighthouseCommand(cfg config.Config) *cobra.Command {
	var dir string
	var port int
	var listen string
	var mcpPath string
	var asJSON bool
	var remoteMCP string

	cmd := &cobra.Command{Use: "lighthouse", Short: "Manage local host or probe remote MCP"}
	up := &cobra.Command{
		Use:   "up",
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
	up.Flags().StringVar(&dir, "dir", "", "Directory to serve/index")
	up.Flags().IntVar(&port, "port", 0, "Port for dir2mcp listen")
	up.Flags().StringVar(&listen, "listen", cfg.Host.Listen, "Listen host:port")
	up.Flags().StringVar(&mcpPath, "mcp-path", cfg.Host.MCPPath, "MCP endpoint path")
	up.Flags().BoolVar(&asJSON, "json", false, "Pass --json to dir2mcp up")

	status := &cobra.Command{Use: "status", Short: "Show local host status", RunE: func(cmd *cobra.Command, args []string) error {
		return host.Status()
	}}
	down := &cobra.Command{Use: "down", Short: "Stop local managed dir2mcp process", RunE: func(cmd *cobra.Command, args []string) error {
		return host.Down()
	}}
	remote := &cobra.Command{Use: "remote", Short: "Probe remote MCP endpoint health", RunE: func(cmd *cobra.Command, args []string) error {
		return host.StatusRemote(cmd.Context(), strings.TrimSpace(remoteMCP))
	}}
	remoteMCP = cfg.MCP.URL
	remote.Flags().StringVar(&remoteMCP, "mcp", remoteMCP, "Remote MCP URL (default: DIRSTRAL_MCP_URL/config mcp.url)")

	cmd.AddCommand(up, status, down, remote)
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

func runBreeze(ctx context.Context, cfg config.Config) error {
	mcpURL := ResolveMCPURL(cfg.MCP.URL, "", false, cfg.MCP.Transport)
	return breeze.Run(ctx, breeze.Options{MCPURL: mcpURL, Transport: cfg.MCP.Transport, Model: cfg.Model, Verbose: cfg.Verbose})
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

func BuildTempestOptions(cfg config.Config, mcpURL, voice, device string, mute, verbose bool, baseURL string) tempest.Options {
	if mcpURL == "" {
		mcpURL = cfg.MCP.URL
	}
	if voice == "" {
		voice = cfg.ElevenLabs.Voice
	}
	if baseURL == "" {
		baseURL = cfg.ElevenLabs.BaseURL
	}
	return tempest.Options{
		MCPURL:    mcpURL,
		Transport: cfg.MCP.Transport,
		Model:     cfg.Model,
		Voice:     voice,
		Device:    device,
		Mute:      mute,
		Verbose:   verbose,
		BaseURL:   baseURL,
	}
}

func runLighthouseMenu(ctx context.Context, cfg config.Config) error {
	for {
		result, err := RunMenu(screenLighthouse)
		if err != nil {
			return err
		}
		switch result.Chosen {
		case lighthouseActionStart:
			printModeHeader("Lighthouse / Start Server")
			if err := host.UpDetached(ctx, host.UpOptions{Listen: cfg.Host.Listen, MCPPath: cfg.Host.MCPPath}); err != nil {
				printModeFeedbackTo("Lighthouse start", err, "Lighthouse menu")
				continue
			}
			if health := host.CheckHealth(); strings.TrimSpace(health.MCPURL) != "" {
				fmt.Println(statusLine("Lighthouse", "Active endpoint: "+health.MCPURL))
			}
			fmt.Println(styleMuted.Render("Server started in background. Use Status for readiness details."))
			printReturnTo("Lighthouse menu")
		case lighthouseActionStatus:
			printModeHeader("Lighthouse / Server Status")
			if err := host.Status(); err != nil {
				printModeFeedbackTo("Lighthouse status", err, "Lighthouse menu")
				continue
			}
			printReturnTo("Lighthouse menu")
		case lighthouseActionLogs:
			printModeHeader("Lighthouse / Logs")
			if err := runLogViewer(); err != nil {
				printModeFeedbackTo("Lighthouse logs", err, "Lighthouse menu")
				continue
			}
			printReturnTo("Lighthouse menu")
		case lighthouseActionRemote:
			printModeHeader("Lighthouse / Remote MCP")
			if err := host.StatusRemote(ctx, strings.TrimSpace(cfg.MCP.URL)); err != nil {
				printModeFeedbackTo("Lighthouse remote", err, "Lighthouse menu")
				continue
			}
			printReturnTo("Lighthouse menu")
		case lighthouseActionStop:
			printModeHeader("Lighthouse / Stop Server")
			if err := host.Down(); err != nil {
				printModeFeedbackTo("Lighthouse stop", err, "Lighthouse menu")
				continue
			}
			printReturnTo("Lighthouse menu")
		default:
			return nil
		}
	}
}
