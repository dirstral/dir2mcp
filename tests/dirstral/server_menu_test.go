package test

import (
	"reflect"
	"strings"
	"testing"

	"dir2mcp/internal/dirstral/app"
	tea "github.com/charmbracelet/bubbletea"
)

var expectedServerMenu = []string{"Start MCP Server", "MCP Server Status", "Remote MCP Status", "View Logs", "Stop MCP Server", "Back"}

func TestServerMenuItemsOrder(t *testing.T) {
	if got := app.ServerMenuItems(); !reflect.DeepEqual(got, expectedServerMenu) {
		t.Fatalf("unexpected server options: got %v want %v", got, expectedServerMenu)
	}
}

func TestServerMenuConfigIncludesLogsByDefault(t *testing.T) {
	cfg := app.ServerMenuConfig()
	got := make([]string, 0, len(cfg.Items))
	for _, item := range cfg.Items {
		got = append(got, item.Value)
	}
	if !reflect.DeepEqual(got, expectedServerMenu) {
		t.Fatalf("unexpected server menu config items: got %v want %v", got, expectedServerMenu)
	}
}

func TestServerMenuControlsAreKeyboardFirst(t *testing.T) {
	cfg := app.ServerMenuConfig()
	if !strings.Contains(cfg.Controls, "j/k") {
		t.Fatalf("expected j/k controls, got %q", cfg.Controls)
	}
	if !strings.Contains(cfg.Controls, "esc/q") {
		t.Fatalf("expected esc/q controls, got %q", cfg.Controls)
	}
}

// TestServerMenuHelpOverlayToggleVisibility verifies help visibility toggles in Server menu.
func TestServerMenuHelpOverlayToggleVisibility(t *testing.T) {
	m := app.NewMenuModel(app.ServerMenuConfig())
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 90, Height: 28})
	m = updated.(app.MenuModel)

	if strings.Contains(m.View(), "Server Keymap") {
		t.Fatalf("expected help overlay hidden by default")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	withHelp := updated.(app.MenuModel)
	if !strings.Contains(withHelp.View(), "Server Keymap") {
		t.Fatalf("expected help overlay visible after ?")
	}

	updated, _ = withHelp.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	withoutHelp := updated.(app.MenuModel)
	if strings.Contains(withoutHelp.View(), "Server Keymap") {
		t.Fatalf("expected help overlay hidden after second ?")
	}
}
