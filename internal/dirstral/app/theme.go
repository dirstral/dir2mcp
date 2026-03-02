package app

import (
	"fmt"
	"os"

	"dir2mcp/internal/dirstral/ui"
	"github.com/charmbracelet/lipgloss"
)

// Legacy ANSI constants kept for logo rendering which pre-dates lipgloss.
const (
	ansiReset = "\033[0m"

	colorBrandStrong = "\033[38;5;214m"
	colorBrand       = "\033[38;5;208m"
	colorMuted       = "\033[38;5;245m"
	colorSubtle      = "\033[38;5;242m"
	colorError       = "\033[38;5;203m"
	colorBold        = "\033[1m"

	colorTint1 = "\033[38;5;220m"
	colorTint2 = "\033[38;5;215m"
	colorTint3 = "\033[38;5;214m"
	colorTint4 = "\033[38;5;209m"
	colorTint5 = "\033[38;5;208m"
	colorTint6 = "\033[38;5;202m"
)

// Lipgloss color palette.
var (
	clrBrandStrong = lipgloss.Color(ui.ClrBrand)
	clrMuted       = lipgloss.Color(ui.ClrMuted)
	clrSubtle      = lipgloss.Color(ui.ClrSubtle)
	clrGreen       = lipgloss.Color(ui.ClrGreen)
)

// Reusable lipgloss styles.
var (
	styleBrandStrong  = lipgloss.NewStyle().Foreground(clrBrandStrong).Bold(true)
	styleTitle        = lipgloss.NewStyle().Foreground(clrBrandStrong).Bold(true)
	styleMuted        = lipgloss.NewStyle().Foreground(clrMuted)
	styleSubtle       = lipgloss.NewStyle().Foreground(clrSubtle)
	styleSelected     = lipgloss.NewStyle().Foreground(clrBrandStrong).Bold(true)
	styleSelectedRow  = lipgloss.NewStyle().Foreground(lipgloss.Color("255")).Bold(true)
	styleDescription  = lipgloss.NewStyle().Foreground(clrSubtle)
	styleSelectedDesc = lipgloss.NewStyle().Foreground(clrMuted)
	styleGreen        = lipgloss.NewStyle().Foreground(clrGreen)
	styleMenuBox      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(clrMuted).Padding(1, 3).MarginTop(1).MarginBottom(1)
)

// paint wraps text in raw ANSI codes. Used by logo rendering.
// Respects NO_COLOR environment variable.
func paint(text string, styles ...string) string {
	if text == "" {
		return ""
	}
	if os.Getenv("NO_COLOR") != "" {
		return text
	}
	styled := ""
	for _, style := range styles {
		styled += style
	}
	return styled + text + ansiReset
}

func statusLine(label, details string) string {
	return fmt.Sprintf("%s %s", paint(label, colorBrandStrong, colorBold), paint(details, colorMuted))
}

func errorLine(err error) string {
	return paint(fmt.Sprintf("error: %v", err), colorError, colorBold)
}
