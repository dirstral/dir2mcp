package app

import (
	"fmt"
	"os"
	"strings"
	"time"

	"dir2mcp/internal/dirstral/host"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

const (
	logMaxLines     = 100
	logTickInterval = 500 * time.Millisecond
)

type logTickMsg time.Time

type logModel struct {
	viewport     viewport.Model
	logPath      string
	lastSize     int64
	content      string
	ready        bool
	width        int
	height       int
	wasAtBottom  bool
	emptyMessage string
}

func newLogModel() logModel {
	logPath := host.LogPath()
	return logModel{
		logPath:      logPath,
		wasAtBottom:  true,
		emptyMessage: "No log output yet. Start a server first.",
	}
}

func (m logModel) Init() tea.Cmd {
	return tickLogs()
}

func tickLogs() tea.Cmd {
	return tea.Tick(logTickInterval, func(t time.Time) tea.Msg {
		return logTickMsg(t)
	})
}

func (m logModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc":
			return m, tea.Quit
		case "G":
			m.viewport.GotoBottom()
			m.wasAtBottom = true
			return m, nil
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		headerHeight := 2 // header + blank line
		footerHeight := 2 // blank line + footer
		viewportHeight := m.height - headerHeight - footerHeight
		if viewportHeight < 1 {
			viewportHeight = 1
		}
		if !m.ready {
			m.viewport = viewport.New(m.width, viewportHeight)
			m.viewport.SetContent(m.content)
			m.ready = true
		} else {
			m.viewport.Width = m.width
			m.viewport.Height = viewportHeight
		}
		return m, nil

	case logTickMsg:
		m.refreshContent()
		return m, tickLogs()
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	// Track whether user is at the bottom after any viewport movement.
	m.wasAtBottom = m.viewport.AtBottom()
	return m, cmd
}

func (m *logModel) refreshContent() {
	info, err := os.Stat(m.logPath)
	if err != nil || info.Size() == 0 {
		m.content = styleMuted.Render(m.emptyMessage)
		m.lastSize = 0
		if m.ready {
			m.viewport.SetContent(m.content)
		}
		return
	}

	// Only re-read if file size changed.
	if info.Size() == m.lastSize {
		return
	}

	data, err := os.ReadFile(m.logPath)
	if err != nil {
		m.content = styleMuted.Render("Error reading log file: " + err.Error())
		if m.ready {
			m.viewport.SetContent(m.content)
		}
		return
	}
	m.lastSize = info.Size()

	m.content = TailLogLines(string(data), logMaxLines)
	if m.ready {
		m.viewport.SetContent(m.content)
		if m.wasAtBottom {
			m.viewport.GotoBottom()
		}
	}
}

func TailLogLines(raw string, maxLines int) string {
	trimmed := strings.TrimRight(raw, "\n")
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}

func (m logModel) View() string {
	if !m.ready {
		return styleMuted.Render("Loading...")
	}

	header := fmt.Sprintf("%s  %s",
		styleBrandStrong.Render("MCP Server Logs"),
		styleSubtle.Render(m.logPath),
	)
	footer := styleMuted.Render("q/esc back · up/down scroll · G end")

	return header + "\n\n" + m.viewport.View() + "\n\n" + footer
}

func runServerLogViewer() error {
	model := newLogModel()
	p := tea.NewProgram(model, tea.WithAltScreen())
	_, err := p.Run()
	return err
}
