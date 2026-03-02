package app

// ServerMenuConfig returns the menu configuration for the server submenu.
func ServerMenuConfig() MenuConfig {
	return MenuConfig{
		Title: "Start/Stop MCP Server",
		Intro: []string{"Manage local dir2mcp host process or probe remote MCP."},
		Items: []MenuItem{
			{Label: serverActionStart, Description: "Launch dir2mcp in background", Value: serverActionStart},
			{Label: serverActionStatus, Description: "Check process and endpoint health", Value: serverActionStatus},
			{Label: serverActionRemote, Description: "Probe configured remote MCP endpoint", Value: serverActionRemote},
			{Label: serverActionLogs, Description: "Tail dir2mcp output", Value: serverActionLogs},
			{Label: serverActionStop, Description: "Stop dir2mcp server", Value: serverActionStop},
			{Label: serverActionBack, Description: "Return to main menu", Value: serverActionBack},
		},
		ShowLogo: false,
		Controls: "up/down or j/k move · enter select · esc/q back",
	}
}
