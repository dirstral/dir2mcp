package app

var startupTips = []string{
	"Tip: Start/Stop MCP Server first is the fastest demo path.",
	"Tip: Set DIRSTRAL_MCP_URL to target a remote MCP endpoint.",
	"Tip: Press q any time to return or quit.",
	"Tip: Use j/k if arrow keys are awkward in your terminal.",
	"Tip: Chat is best for quick tool-driven lookups.",
}

// StartupTips returns startup hints shown on the home screen.
func StartupTips() []string {
	out := make([]string, len(startupTips))
	copy(out, startupTips)
	return out
}

// StartupTip returns the tip for a specific index.
func StartupTip(index int) string {
	if len(startupTips) == 0 {
		return ""
	}
	if index < 0 {
		index = 0
	}
	return startupTips[index%len(startupTips)]
}

// StartMenuConfig returns the menu configuration for the start screen.
func StartMenuConfig() MenuConfig {
	return MenuConfig{
		Title: "Welcome to Dirstral",
		Intro: []string{
			"Launch mode: Chat (chat), Voice (voice), Start/Stop MCP Server (local host + remote checks)",
			StartupTip(0),
		},
		Items: []MenuItem{
			{Label: string(ChoiceChat), Description: "Interactive text chat with MCP tools", Value: string(ChoiceChat)},
			{Label: string(ChoiceVoice), Description: "Voice-powered agent loop", Value: string(ChoiceVoice)},
			{Label: string(ChoiceServer), Description: "Manage local host and probe remote MCP", Value: string(ChoiceServer)},
			{Label: string(ChoiceSettings), Description: "Edit config and API settings", Value: string(ChoiceSettings)},
			{Label: string(ChoiceQuit), Description: "Exit Dirstral", Value: string(ChoiceQuit)},
		},
		ShowLogo: true,
		Controls: "up/down or j/k move · enter select · esc/q quit",
	}
}
