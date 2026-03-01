package app

import (
	"time"

	"dir2mcp/internal/dirstral/host"
	tea "github.com/charmbracelet/bubbletea"
)

const statusPollInterval = 3 * time.Second

// serverStatusMsg carries the latest health check result.
type serverStatusMsg struct {
	Health host.HealthInfo
}

// pollServerStatus returns a tea.Msg that checks server health and
// sends the result as a serverStatusMsg.
func pollServerStatus() tea.Msg {
	return serverStatusMsg{Health: host.CheckHealth()}
}

// tickServerStatus returns a tea.Cmd that waits for the poll interval
// then triggers another health check.
func tickServerStatus() tea.Cmd {
	return tea.Tick(statusPollInterval, func(t time.Time) tea.Msg {
		return serverStatusMsg{Health: host.CheckHealth()}
	})
}
