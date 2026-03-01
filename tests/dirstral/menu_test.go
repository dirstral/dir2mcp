package test

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"dir2mcp/internal/dirstral/app"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestStartMenuItemsOrder(t *testing.T) {
	want := []string{"Chat", "Voice", "Start/Stop MCP Server", "Settings", "Exit"}
	if got := app.StartMenuItems(); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected startup menu options: got %v want %v", got, want)
	}
}

func TestChooseTierByWidth(t *testing.T) {
	if tier := app.ChooseTier(50); tier != app.LogoCompact {
		t.Fatalf("expected compact tier for narrow terminal, got %d", tier)
	}
	if tier := app.ChooseTier(80); tier != app.LogoMedium {
		t.Fatalf("expected medium tier for 80-col terminal, got %d", tier)
	}
	if tier := app.ChooseTier(220); tier != app.LogoFull {
		t.Fatalf("expected full tier for wide terminal, got %d", tier)
	}
}

func TestRenderLogoThreeTiers(t *testing.T) {
	// Compact: plain text
	if got := app.RenderLogo(50); !strings.Contains(got, app.CompactLogoText) {
		t.Fatalf("expected compact logo text for narrow width")
	}
	// Medium: block letters with folder+star
	med := app.RenderLogo(80)
	if !strings.Contains(med, "██████╗") {
		t.Fatalf("expected block letters in medium logo")
	}
	if !strings.Contains(med, "✦") {
		t.Fatalf("expected star glyph in medium logo")
	}
	// Full: large folder glyph
	full := app.RenderLogo(220)
	if !strings.Contains(full, "██████╗") {
		t.Fatalf("expected block letters in full logo")
	}
	if !strings.Contains(full, "▄████████▄") {
		t.Fatalf("expected folder tab glyph in full logo")
	}
	if !strings.Contains(full, "▀██████████████▀") {
		t.Fatalf("expected folder bottom edge in full logo")
	}
}

func TestTerminalWidthParsingAndFallback(t *testing.T) {
	// Force stdout to a non-TTY so TerminalWidth() consistently exercises
	// COLUMNS parsing/fallback instead of real terminal dimensions.
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	oldOut := os.Stdout
	os.Stdout = devNull
	t.Cleanup(func() {
		os.Stdout = oldOut
		_ = devNull.Close()
	})

	t.Setenv("COLUMNS", " 121 ")
	if got := app.TerminalWidth(); got != 121 {
		t.Fatalf("unexpected terminal width: %d", got)
	}

	t.Setenv("COLUMNS", "not-a-number")
	if got := app.TerminalWidth(); got != app.DefaultTerminalWidth {
		t.Fatalf("unexpected fallback width: %d", got)
	}
}

func TestNormalizeLeftSpacingRemovesSharedIndent(t *testing.T) {
	got := app.NormalizeLeftSpacing([]string{"    alpha", "      beta", "    gamma"})
	want := []string{"alpha", "  beta", "gamma"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected normalization: got %v want %v", got, want)
	}
}

func TestStartupTipRotationWraps(t *testing.T) {
	tips := app.StartupTips()
	if len(tips) < 2 {
		t.Fatalf("need at least two startup tips")
	}
	if got := app.StartupTip(len(tips)); got != tips[0] {
		t.Fatalf("expected wraparound tip %q, got %q", tips[0], got)
	}
}

func TestStartMenuControlsAreKeyboardFirst(t *testing.T) {
	cfg := app.StartMenuConfig()
	if !strings.Contains(cfg.Controls, "j/k") {
		t.Fatalf("expected j/k controls, got %q", cfg.Controls)
	}
	if !strings.Contains(cfg.Controls, "esc/q") {
		t.Fatalf("expected esc/q controls, got %q", cfg.Controls)
	}
}

func TestEnterWorksEvenDuringReveal(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")

	m := app.NewMenuModel(app.MenuConfig{
		Items: []app.MenuItem{{Label: "One", Value: "one"}, {Label: "Two", Value: "two"}},
	})

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	after := updated.(app.MenuModel)
	if after.Chosen() != "one" {
		t.Fatalf("expected enter to select first item during reveal, got %q", after.Chosen())
	}
}

func TestMenuViewIncludesHelpHint(t *testing.T) {
	m := app.NewMenuModel(app.StartMenuConfig())
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 90, Height: 28})
	view := updated.(app.MenuModel).View()
	if !strings.Contains(view, "? help") {
		t.Fatalf("expected menu view to include help hint, got %q", view)
	}
}

// TestMenuHelpOverlayToggle ensures '?' toggles the shared menu keymap overlay.
func TestMenuHelpOverlayToggle(t *testing.T) {
	m := app.NewMenuModel(app.StartMenuConfig())
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 90, Height: 28})
	m = updated.(app.MenuModel)

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	withHelp := updated.(app.MenuModel)
	if !strings.Contains(withHelp.View(), "Welcome to Dirstral Keymap") {
		t.Fatalf("expected help overlay to show keymap")
	}

	updated, _ = withHelp.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	withoutHelp := updated.(app.MenuModel)
	if strings.Contains(withoutHelp.View(), "Welcome to Dirstral Keymap") {
		t.Fatalf("expected help overlay to close after second ?")
	}
}

// TestMenuHelpOverlayBlocksEnterSelection ensures Enter is ignored while overlay is open.
func TestMenuHelpOverlayBlocksEnterSelection(t *testing.T) {
	m := app.NewMenuModel(app.StartMenuConfig())
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 90, Height: 28})
	m = updated.(app.MenuModel)

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	withHelp := updated.(app.MenuModel)

	updated, cmd := withHelp.Update(tea.KeyMsg{Type: tea.KeyEnter})
	afterEnter := updated.(app.MenuModel)
	if afterEnter.Chosen() != "" {
		t.Fatalf("expected no selection when help overlay is open, got %q", afterEnter.Chosen())
	}
	if cmd != nil {
		t.Fatalf("expected no quit command when help overlay is open")
	}
}

// TestMenuHelpOverlayCtrlKToggle ensures Ctrl+K toggles the shared menu keymap overlay.
func TestMenuHelpOverlayCtrlKToggle(t *testing.T) {
	m := app.NewMenuModel(app.StartMenuConfig())
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 90, Height: 28})
	m = updated.(app.MenuModel)

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlK})
	withHelp := updated.(app.MenuModel)
	if !strings.Contains(withHelp.View(), "Welcome to Dirstral Keymap") {
		t.Fatalf("expected help overlay to open on ctrl+k")
	}

	updated, _ = withHelp.Update(tea.KeyMsg{Type: tea.KeyCtrlK})
	withoutHelp := updated.(app.MenuModel)
	if strings.Contains(withoutHelp.View(), "Welcome to Dirstral Keymap") {
		t.Fatalf("expected help overlay to close on second ctrl+k")
	}
}

func TestMenuNarrowWidthTruncatesLongRows(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	cfg := app.MenuConfig{
		Title: "T",
		Items: []app.MenuItem{{
			Label:       "Extremely Long Menu Label That Should Be Truncated",
			Description: "Extremely long description text that should be truncated in narrow terminals",
			Value:       "v",
		}},
	}
	m := app.NewMenuModel(cfg)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	view := updated.(app.MenuModel).View()
	if !strings.Contains(view, "...") {
		t.Fatalf("expected truncated output in narrow width, got %q", view)
	}
}

func TestSelectedRowBadgeDoesNotBreakHighlight(t *testing.T) {
	badgeStyle := lipgloss.NewStyle().Padding(0, 1)

	m := app.NewMenuModel(app.MenuConfig{
		Title: "Menu",
		Items: []app.MenuItem{
			{
				Label:       "Server",
				Description: "Manage local MCP server and probe remote MCP",
				Value:       "server",
				Badge:       "stopped",
				BadgeStyle:  &badgeStyle,
			},
			{Label: "Exit", Description: "Leave", Value: "exit"},
		},
	})

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 24})
	m = updated.(app.MenuModel)
	// Any key reveals all animated rows; use a no-op key to keep cursor at row 0.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	m = updated.(app.MenuModel)

	view := m.View()
	selectedLine := ""
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "Server") {
			selectedLine = line
			break
		}
	}
	if selectedLine == "" {
		t.Fatalf("expected selected Server line in view: %q", view)
	}
	if !strings.Contains(selectedLine, "[stopped]") {
		t.Fatalf("expected plain badge token in selected row, got: %q", selectedLine)
	}
	if strings.Contains(selectedLine, "[ stopped ]") {
		t.Fatalf("expected selected row to ignore badge-specific spacing style, got: %q", selectedLine)
	}
}
