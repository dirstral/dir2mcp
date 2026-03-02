// Package ui provides shared terminal styling for all dirstral modes.
package ui

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Color palette (256-color).
var (
	ClrBrand  = lipgloss.Color("214") // orange
	ClrMuted  = lipgloss.Color("245") // gray
	ClrSubtle = lipgloss.Color("242") // darker gray
	ClrGreen  = lipgloss.Color("114") // green
	ClrRed    = lipgloss.Color("203") // red/error
	ClrCyan   = lipgloss.Color("81")  // cyan for citations
	ClrYellow = lipgloss.Color("220") // yellow for scores
)

// Reusable styles.
var (
	Bold    = lipgloss.NewStyle().Bold(true)
	Brand   = lipgloss.NewStyle().Foreground(ClrBrand).Bold(true)
	Muted   = lipgloss.NewStyle().Foreground(ClrMuted)
	Subtle  = lipgloss.NewStyle().Foreground(ClrSubtle)
	Green   = lipgloss.NewStyle().Foreground(ClrGreen)
	Red     = lipgloss.NewStyle().Foreground(ClrRed)
	Cyan    = lipgloss.NewStyle().Foreground(ClrCyan)
	Yellow  = lipgloss.NewStyle().Foreground(ClrYellow)
	Keyword = lipgloss.NewStyle().Foreground(ClrBrand)
)

// Prompt renders a mode prompt like "chat> " with color.
func Prompt(mode string) string {
	return Brand.Render(mode+">") + " "
}

// Error formats an error message.
func Error(msg string) string {
	return Red.Render("error: " + msg)
}

// Errorf formats an error with printf-style formatting.
func Errorf(format string, a ...any) string {
	return Error(fmt.Sprintf(format, a...))
}

// Info formats an informational label with details.
func Info(label, detail string) string {
	return Brand.Render(label) + " " + Muted.Render(detail)
}

// Citation formats a file citation string.
func Citation(text string) string {
	return Cyan.Render(text)
}

// Score formats a relevance score.
func Score(v any) string {
	return Yellow.Render(fmt.Sprintf("%v", v))
}

// Dim renders dimmed/muted text.
func Dim(text string) string {
	return Subtle.Render(text)
}

// Enabled reports whether color output is enabled.
func Enabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return strings.ToLower(strings.TrimSpace(os.Getenv("TERM"))) != "dumb"
}
