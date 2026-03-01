package app

import (
	"fmt"
	"os"

	"dir2mcp/internal/dirstral/host"
	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"
)

// appResult holds the outcome of a single tea.Program run.
type appResult struct {
	// Screen that was active when the program exited.
	Screen screenID
	// Chosen value (menu item value).
	Chosen string
}

type screenID int

const (
	screenStart  screenID = iota
	screenServer screenID = iota
)

// appModel is the top-level bubbletea model that manages screen transitions.
type appModel struct {
	screen        screenID
	menu          MenuModel
	result        appResult
	statusLoaded  bool
	serverRunning bool
	tipIndex      int
}

func newAppModel(screen screenID) appModel {
	var cfg MenuConfig
	switch screen {
	case screenServer:
		cfg = ServerMenuConfig()
	default:
		cfg = StartMenuConfig()
	}
	return appModel{
		screen: screen,
		menu:   NewMenuModel(cfg),
	}
}

func (m appModel) Init() tea.Cmd {
	// Kick off both the initial status check and the menu reveal animation.
	menuInit := m.menu.Init()
	if m.screen == screenStart {
		return tea.Batch(pollServerStatus, menuInit, tickStartupTip())
	}
	return tea.Batch(pollServerStatus, menuInit)
}

func (m appModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case serverStatusMsg:
		m.statusLoaded = true
		m.serverRunning = msg.Health.Alive
		m.updateDynamicItems(msg)
		// Schedule next poll.
		return m, tickServerStatus()
	case tipTickMsg:
		if m.screen != screenStart {
			return m, nil
		}
		m.tipIndex++
		m.refreshStartupIntro()
		return m, tickStartupTip()
	default:
		updated, cmd := m.menu.Update(msg)
		m.menu = updated.(MenuModel)

		// If the menu quit, capture result.
		if m.menu.Chosen() != "" || m.menu.Quitted() {
			m.result = appResult{
				Screen: m.screen,
				Chosen: m.menu.Chosen(),
			}
		}
		return m, cmd
	}
}

func (m *appModel) refreshStartupIntro() {
	if m.screen != screenStart {
		return
	}
	m.menu.SetIntro([]string{
		"Launch mode: Chat (chat), Voice (voice), Start/Stop MCP Server (host MCP)",
		StartupTip(m.tipIndex),
	})
}

// updateDynamicItems adjusts menu items based on server status.
func (m *appModel) updateDynamicItems(msg serverStatusMsg) {
	switch m.screen {
	case screenStart:
		// Add a status badge to the server item.
		items := StartMenuConfig().Items
		for i := range items {
			if items[i].Value == string(ChoiceServer) {
				if !m.statusLoaded {
					items[i].Badge = "..."
				} else if msg.Health.Alive {
					s := styleGreen
					items[i].Badge = "running"
					items[i].BadgeStyle = &s
				} else {
					items[i].Badge = "stopped"
				}
			}
		}
		m.menu.SetItems(items)

	case screenServer:
		// Show/hide Start/Stop based on current state.
		m.menu.SetIntro(buildServerIntro(msg.Health))
		m.menu.SetItems(serverItemsForHealth(msg.Health))
	}
}

func serverItemsForHealth(health host.HealthInfo) []MenuItem {
	if health.Alive {
		return []MenuItem{
			{Label: serverActionStop, Description: "Terminate managed dir2mcp", Value: serverActionStop},
			{Label: serverActionStatus, Description: "Check process and endpoint health", Value: serverActionStatus},
			{Label: serverActionRemote, Description: "Probe configured remote MCP endpoint", Value: serverActionRemote},
			{Label: serverActionLogs, Description: "Tail dir2mcp output", Value: serverActionLogs},
			{Label: serverActionBack, Description: "Return to main menu", Value: serverActionBack},
		}
	}

	return []MenuItem{
		{Label: serverActionStart, Description: "Launch dir2mcp in background", Value: serverActionStart},
		{Label: serverActionStatus, Description: "Check process and endpoint health", Value: serverActionStatus},
		{Label: serverActionRemote, Description: "Probe configured remote MCP endpoint", Value: serverActionRemote},
		{Label: serverActionLogs, Description: "Tail dir2mcp output", Value: serverActionLogs},
		{Label: serverActionBack, Description: "Return to main menu", Value: serverActionBack},
	}
}

func buildServerIntro(health host.HealthInfo) []string {
	if !health.Found {
		return []string{
			"Manage local dir2mcp host process (remote checks use DIRSTRAL_MCP_URL).",
			"Status: stopped",
			"Endpoint: not running",
		}
	}
	state := "stopped"
	if health.Alive {
		state = "running"
	}
	endpoint := host.OrUnknown(health.MCPURL)
	protocol := host.OrUnknown(health.ProtocolHeader)
	auth := host.OrUnknown(health.AuthSourceType)
	lines := []string{
		"Manage local dir2mcp host process (remote checks use DIRSTRAL_MCP_URL).",
		"Status: " + state,
		"Endpoint: " + endpoint,
		"Protocol: " + protocol + " · Auth source: " + auth,
	}
	if health.AuthDiagnostic != "" {
		lines = append(lines, "Diagnostic: "+health.AuthDiagnostic)
	}
	return lines
}

func (m appModel) View() string {
	return m.menu.View()
}

// Result returns the captured result after program exit.
func (m appModel) Result() appResult {
	return m.result
}

// RunMenu launches an interactive bubbletea menu and returns the result.
// If stdin is not a terminal, falls back to a line-based menu.
func RunMenu(screen screenID) (appResult, error) {
	if !isInteractiveTerminal() {
		return runFallbackMenu(screen)
	}

	model := newAppModel(screen)
	p := tea.NewProgram(model, tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		return appResult{}, err
	}
	return final.(appModel).Result(), nil
}

// runFallbackMenu provides a numbered-list menu for non-interactive terminals.
func runFallbackMenu(screen screenID) (appResult, error) {
	var cfg MenuConfig
	switch screen {
	case screenServer:
		cfg = ServerMenuConfig()
	default:
		cfg = StartMenuConfig()
	}

	if cfg.ShowLogo {
		fmt.Println(RenderLogo(TerminalWidth()))
	}
	if cfg.Title != "" {
		fmt.Println(cfg.Title)
	}
	for _, line := range cfg.Intro {
		fmt.Println(line)
	}
	fmt.Println()
	for i, item := range cfg.Items {
		desc := ""
		if item.Description != "" {
			desc = " — " + item.Description
		}
		badge := ""
		if item.Badge != "" {
			badge = " [" + item.Badge + "]"
		}
		fmt.Printf("  %d) %s%s%s\n", i+1, item.Label, badge, desc)
	}

	return appResult{Screen: screen, Chosen: readFallbackChoice(cfg)}, nil
}

func readFallbackChoice(cfg MenuConfig) string {
	var input string
	for {
		fmt.Printf("Select [1-%d] or q: ", len(cfg.Items))
		_, err := fmt.Scanln(&input)
		if err != nil {
			return cfg.Items[len(cfg.Items)-1].Value // last item is quit/back
		}
		if input == "q" || input == "quit" {
			return cfg.Items[len(cfg.Items)-1].Value
		}
		n := 0
		for _, c := range input {
			if c >= '0' && c <= '9' {
				n = n*10 + int(c-'0')
			} else {
				n = -1
				break
			}
		}
		if n >= 1 && n <= len(cfg.Items) {
			return cfg.Items[n-1].Value
		}
	}
}

func isInteractiveTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}
