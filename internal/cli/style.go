package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"

	"dir2mcp/internal/dirstral/ui"
)

// palette holds the ANSI-256 color values used throughout the CLI.
// Shared colors are imported from internal/dirstral/ui to ensure consistency.
var (
	clrBrand  = ui.ClrBrand
	clrGreen  = ui.ClrGreen
	clrRed    = ui.ClrRed
	clrYellow = ui.ClrYellow
	clrCyan   = ui.ClrCyan
	clrDim    = ui.ClrMuted
	clrWhite  = lipgloss.Color("255")
)

// styles wraps lipgloss renderers that respect TTY detection.
// When output is not a terminal (piped, redirected, --json), all
// styling is disabled and raw text is emitted.
type styles struct {
	enabled bool

	Brand  lipgloss.Style
	Green  lipgloss.Style
	Red    lipgloss.Style
	Yellow lipgloss.Style
	Cyan   lipgloss.Style
	Dim    lipgloss.Style
	Bold   lipgloss.Style

	// Composite styles
	Header  lipgloss.Style // section headers
	Key     lipgloss.Style // label in key=value output
	Value   lipgloss.Style // value in key=value output
	URL     lipgloss.Style // clickable URLs
	Warning lipgloss.Style // warning lines
	Error   lipgloss.Style // error prefix
	Success lipgloss.Style // success messages
}

// newStyles creates a styles instance. Colors are enabled only when w
// points to a terminal file descriptor and jsonMode is false.
func newStyles(w io.Writer, jsonMode bool) styles {
	enabled := false
	if !jsonMode {
		if f, ok := w.(*os.File); ok {
			enabled = term.IsTerminal(int(f.Fd()))
		}
	}

	s := styles{enabled: enabled}

	if !enabled {
		noop := lipgloss.NewStyle()
		s.Brand = noop
		s.Green = noop
		s.Red = noop
		s.Yellow = noop
		s.Cyan = noop
		s.Dim = noop
		s.Bold = noop
		s.Header = noop
		s.Key = noop
		s.Value = noop
		s.URL = noop
		s.Warning = noop
		s.Error = noop
		s.Success = noop
		return s
	}

	s.Brand = lipgloss.NewStyle().Foreground(clrBrand)
	s.Green = lipgloss.NewStyle().Foreground(clrGreen)
	s.Red = lipgloss.NewStyle().Foreground(clrRed)
	s.Yellow = lipgloss.NewStyle().Foreground(clrYellow)
	s.Cyan = lipgloss.NewStyle().Foreground(clrCyan)
	s.Dim = lipgloss.NewStyle().Foreground(clrDim)
	s.Bold = lipgloss.NewStyle().Bold(true)

	s.Header = lipgloss.NewStyle().Bold(true).Foreground(clrBrand)
	s.Key = lipgloss.NewStyle().Foreground(clrDim)
	s.Value = lipgloss.NewStyle().Foreground(clrWhite)
	s.URL = lipgloss.NewStyle().Foreground(clrCyan).Underline(true)
	s.Warning = lipgloss.NewStyle().Foreground(clrYellow).Bold(true)
	s.Error = lipgloss.NewStyle().Foreground(clrRed).Bold(true)
	s.Success = lipgloss.NewStyle().Foreground(clrGreen)

	return s
}

// banner returns the dir2mcp startup banner with version.
func (s styles) banner() string {
	if !s.enabled {
		return "dir2mcp"
	}
	return s.Brand.Render("dir2mcp")
}

// kv formats a key-value pair like "  Key:  value".
func (s styles) kv(key, value string) string {
	if !s.enabled {
		return fmt.Sprintf("  %-14s %s", key+":", value)
	}
	return fmt.Sprintf("  %s %s",
		s.Key.Render(fmt.Sprintf("%-14s", key+":")),
		s.Value.Render(value),
	)
}

// sectionHeader returns the rendered title when styles are enabled,
// otherwise it returns the plain title.
func (s styles) sectionHeader(title string) string {
	if !s.enabled {
		return title
	}
	return s.Header.Render(title)
}

// dim wraps text in dim/muted styling.
func (s styles) dim(text string) string {
	if !s.enabled {
		return text
	}
	return s.Dim.Render(text)
}

// errPrefix returns a styled "ERROR:" prefix.
func (s styles) errPrefix() string {
	if !s.enabled {
		return "ERROR:"
	}
	return s.Error.Render("ERROR:")
}

// warnPrefix returns a styled "WARNING:" prefix.
func (s styles) warnPrefix() string {
	if !s.enabled {
		return "WARNING:"
	}
	return s.Warning.Render("WARNING:")
}

// stat formats a labeled statistic like "scanned=412".
func (s styles) stat(label string, value interface{}) string {
	if !s.enabled {
		return fmt.Sprintf("%s=%v", label, value)
	}
	return fmt.Sprintf("%s=%s", s.Dim.Render(label), s.Value.Render(fmt.Sprintf("%v", value)))
}

// separator returns a thin horizontal rule.
func (s styles) separator(width int) string {
	if width <= 0 {
		width = 40
	}
	line := strings.Repeat("─", width)
	if !s.enabled {
		return line
	}
	return s.Dim.Render(line)
}
