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

	colorBrandStrong = "\033[38;5;208m" // #ff8700 — warm orange (SVG top stop ~#F2911A)
	colorBrand       = "\033[38;5;166m" // #d75f00 — orange-red (SVG mid stop ~#E8601B)
	colorMuted       = "\033[38;5;245m"
	colorSubtle      = "\033[38;5;242m"
	colorError       = "\033[38;5;203m"
	colorBold        = "\033[1m"

	// Logo tints: top→bottom gradient matching the SVG brand gradient (#F2911A → #C71B18).
	colorTint1 = "\033[38;5;208m" // #ff8700 — warm orange
	colorTint2 = "\033[38;5;202m" // #ff5f00 — orange-red
	colorTint3 = "\033[38;5;166m" // #d75f00 — darker orange-red
	colorTint4 = "\033[38;5;160m" // #d70000 — deep red
	colorTint5 = "\033[38;5;124m" // #af0000 — crimson
	colorTint6 = "\033[38;5;124m" // #af0000 — crimson (SVG bottom stop ~#C71B18)
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
