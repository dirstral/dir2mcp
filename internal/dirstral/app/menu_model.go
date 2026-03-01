package app

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// MenuItem describes a single menu entry.
type MenuItem struct {
	Label       string
	Description string
	Value       string
	// Badge is optional text rendered after the label (e.g. "[running]").
	Badge string
	// BadgeStyle is the lipgloss style for the badge. If nil, styleSubtle is used.
	BadgeStyle *lipgloss.Style
}

// MenuConfig holds static data for a menu screen.
type MenuConfig struct {
	Title    string
	Intro    []string
	Items    []MenuItem
	ShowLogo bool
	Controls string
}

// MenuModel is a bubbletea Model for an interactive menu.
type MenuModel struct {
	config        MenuConfig
	cursor        int
	chosen        string
	width         int
	height        int
	quitted       bool
	helpVisible   bool
	revealedCount int  // how many items have been revealed (-1 = all)
	animate       bool // whether staggered reveal is active
}

// NewMenuModel creates a MenuModel from a config.
func NewMenuModel(cfg MenuConfig) MenuModel {
	if cfg.Controls == "" {
		cfg.Controls = "arrows navigate · enter select · q/esc back"
	}
	animate := animationsEnabled()
	revealed := -1 // show all by default
	if animate {
		revealed = 0
	}
	return MenuModel{
		config:        cfg,
		width:         DefaultTerminalWidth,
		animate:       animate,
		revealedCount: revealed,
	}
}

// Chosen returns the selected value after the model quits.
func (m MenuModel) Chosen() string { return m.chosen }

// Quitted returns whether the user quit/escaped.
func (m MenuModel) Quitted() bool { return m.quitted }

// SetItems replaces the menu items (used for dynamic updates).
func (m *MenuModel) SetItems(items []MenuItem) {
	m.config.Items = items
	if m.cursor >= len(items) {
		m.cursor = len(items) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// SetIntro replaces the intro lines shown above the menu.
func (m *MenuModel) SetIntro(intro []string) {
	m.config.Intro = intro
}

func (m MenuModel) Init() tea.Cmd {
	if m.animate {
		return tickReveal()
	}
	return nil
}

func (m MenuModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case revealTickMsg:
		if m.revealedCount >= 0 && m.revealedCount < len(m.config.Items) {
			m.revealedCount++
			if m.revealedCount < len(m.config.Items) {
				return m, tickReveal()
			}
			// All revealed — mark complete.
			m.revealedCount = -1
		}
		return m, nil

	case tea.KeyMsg:
		if msg.String() == "?" || msg.String() == "ctrl+k" {
			m.helpVisible = !m.helpVisible
			return m, nil
		}
		if m.helpVisible {
			switch msg.String() {
			case "esc", "q", "?", "ctrl+k":
				m.helpVisible = false
			}
			return m, nil
		}
		// If still revealing, skip to showing all items on any key press.
		if m.revealedCount >= 0 {
			m.revealedCount = -1
		}
		switch msg.String() {
		case "up", "k":
			m.cursor--
			if m.cursor < 0 {
				m.cursor = len(m.config.Items) - 1
			}
		case "down", "j":
			m.cursor++
			if m.cursor >= len(m.config.Items) {
				m.cursor = 0
			}
		case "enter":
			if m.cursor >= 0 && m.cursor < len(m.config.Items) {
				m.chosen = m.config.Items[m.cursor].Value
			}
			return m, tea.Quit
		case "q", "esc", "ctrl+c":
			m.quitted = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m MenuModel) View() string {
	if m.width == 0 {
		return ""
	}
	viewWidth := maxInt(m.width, 1)
	menuHorizChrome := 6 // border + default horizontal padding + breathing room
	if viewWidth >= 56 {
		menuHorizChrome = 10 // border + wider horizontal padding + breathing room
	}
	contentWidth := clampInt(viewWidth-menuHorizChrome, 12, 96)
	panelWidth := contentWidth + menuHorizChrome - 2
	showLogo := m.config.ShowLogo && m.width >= 60 && (m.height == 0 || m.height >= 20)
	tinyHeight := m.height > 0 && m.height < 14
	compactRows := contentWidth < 28

	menuStyle := styleMenuBox
	if m.width < 56 {
		menuStyle = menuStyle.Padding(0, 1)
	} else {
		menuStyle = menuStyle.Padding(1, 3)
	}
	if m.height > 0 && m.height < 18 {
		menuStyle = menuStyle.MarginTop(0).MarginBottom(0)
	}

	var headerItems []string
	if m.config.Title != "" {
		title := styleTitle.MaxWidth(contentWidth).Render(m.config.Title)
		headerItems = append(headerItems, lipgloss.PlaceHorizontal(panelWidth, lipgloss.Center, title))
	}
	for _, line := range m.config.Intro {
		intro := styleMuted.MaxWidth(contentWidth).Render(line)
		headerItems = append(headerItems, lipgloss.PlaceHorizontal(panelWidth, lipgloss.Center, intro))
	}
	header := joinVerticalNonEmpty(lipgloss.Center, headerItems...)

	var menuLines []string
	labelWidth := clampInt(contentWidth/3, 8, 28)
	gutterWidth := 5
	descWidth := maxInt(contentWidth-labelWidth-gutterWidth-4, 0)

	showCount := len(m.config.Items)
	if m.revealedCount >= 0 {
		showCount = m.revealedCount
	}
	rowGap := 0
	if !tinyHeight && !compactRows {
		rowGap = 1
	}

	for i, item := range m.config.Items {
		if i >= showCount {
			break
		}

		isSelected := i == m.cursor

		badgePlain := ""
		badgeStyled := ""
		if item.Badge != "" {
			badgePlain = " [" + item.Badge + "]"
			badgeStyle := styleSubtle
			if item.BadgeStyle != nil {
				badgeStyle = *item.BadgeStyle
			}
			badgeStyled = " [" + badgeStyle.Render(item.Badge) + "]"
		}

		if compactRows || descWidth < 10 {
			label := truncateText(item.Label+badgePlain, maxInt(contentWidth-6, 4))
			if badgeStyled != "" && !isSelected {
				label = strings.Replace(label, badgePlain, badgeStyled, 1)
			}
			if isSelected {
				menuLines = append(menuLines, fmt.Sprintf(" %s %s", styleSelected.Render(">"), styleSelectedRow.Render(" "+label+" ")))
			} else {
				menuLines = append(menuLines, fmt.Sprintf("   %s", styleMuted.Render(label)))
			}
			continue
		}

		paddedLabel := fitText(item.Label+badgePlain, labelWidth)
		if badgeStyled != "" && !isSelected {
			paddedLabel = strings.Replace(paddedLabel, badgePlain, badgeStyled, 1)
		}
		desc := fitText(item.Description, descWidth)
		marker := styleMuted.Render(" ")
		labelCell := styleMuted.Width(labelWidth).Render(paddedLabel)
		descCell := styleDescription.Width(descWidth).Render(desc)
		if isSelected {
			marker = styleSelected.Render(">")
			labelCell = styleSelectedRow.Width(labelWidth).Render(paddedLabel)
			descCell = styleSelectedDesc.Width(descWidth).Render(desc)
		}
		row := fmt.Sprintf("  %s %s%s%s", marker, labelCell, strings.Repeat(" ", gutterWidth), descCell)
		menuLines = append(menuLines, row)
		if rowGap == 1 && i < showCount-1 {
			menuLines = append(menuLines, "")
		}
	}
	if len(menuLines) == 0 {
		menuLines = append(menuLines, styleSubtle.Render("  (no options)"))
	}

	menuBox := menuStyle.MaxWidth(panelWidth).Render(lipgloss.JoinVertical(lipgloss.Left, menuLines...))
	controls := m.config.Controls + " · ? help"
	footer := styleSubtle.MaxWidth(contentWidth).Render(truncateText(controls, contentWidth))

	var body string
	if tinyHeight {
		body = joinVerticalNonEmpty(lipgloss.Left, header, menuBox)
	} else {
		body = joinVerticalNonEmpty(lipgloss.Center, header, menuBox)
	}
	if m.helpVisible {
		helpText := menuHelpText(contentWidth, m.config.Title)
		if tinyHeight {
			helpText = styleMuted.Render(truncateText("Keys: up/down or j/k move · enter choose · esc/q back · ? toggle help", contentWidth))
		}
		helpBox := menuStyle.MaxWidth(panelWidth).Render(helpText)
		// Help is an overlay panel: replace menu content so keymap text stays visible
		// even on shorter terminals where stacking panels would clip the footer area.
		if tinyHeight {
			body = joinVerticalNonEmpty(lipgloss.Left, header, helpBox)
		} else {
			body = joinVerticalNonEmpty(lipgloss.Center, header, helpBox)
		}
	}
	content := composeWithPinnedFooter(body, footer, m.height)

	if showLogo {
		logo := RenderLogo(viewWidth)
		var b strings.Builder
		b.WriteString(logo)
		b.WriteByte('\n')
		b.WriteByte('\n')

		contentLines := strings.Split(content, "\n")
		tier := ChooseTier(viewWidth)
		if tier == LogoCompact {
			for _, line := range contentLines {
				b.WriteString(padLine(line, compactLeftPad))
				b.WriteByte('\n')
			}
		} else {
			for _, line := range centerBlockLines(contentLines, viewWidth) {
				b.WriteString(line)
				b.WriteByte('\n')
			}
		}
		return composeWithPinnedFooter(b.String(), "", m.height)
	}

	if m.height <= 0 {
		return content
	}
	vAlign := lipgloss.Center
	if tinyHeight {
		vAlign = lipgloss.Top
	}
	return lipgloss.Place(viewWidth, m.height, lipgloss.Center, vAlign, content)
}

// menuHelpText renders the shared menu keymap panel with screen context.
func menuHelpText(width int, screenTitle string) string {
	title := strings.TrimSpace(screenTitle)
	if title == "" {
		title = "Keymap"
	} else {
		title += " Keymap"
	}

	lines := []string{
		styleBrandStrong.Render(title),
		styleMuted.Render("up/down or j/k  move selection"),
		styleMuted.Render("enter           choose item"),
		styleMuted.Render("esc/q           back/quit"),
		styleMuted.Render("? or ctrl+k     toggle this help"),
	}
	return lipgloss.NewStyle().MaxWidth(maxInt(width-2, 12)).Render(strings.Join(lines, "\n"))
}

func composeWithPinnedFooter(body, footer string, height int) string {
	if height <= 0 {
		return joinVerticalNonEmpty(lipgloss.Left, body, footer)
	}
	bodyLines := splitLines(body)
	footerLines := splitLines(footer)
	if len(footerLines) >= height {
		return strings.Join(footerLines[:height], "\n")
	}
	bodyBudget := height - len(footerLines)
	if bodyBudget < len(bodyLines) {
		if bodyBudget <= 0 {
			bodyLines = nil
		} else {
			bodyLines = bodyLines[:bodyBudget]
		}
	}
	lines := make([]string, 0, len(bodyLines)+len(footerLines))
	lines = append(lines, bodyLines...)
	lines = append(lines, footerLines...)
	return strings.Join(lines, "\n")
}

func joinVerticalNonEmpty(pos lipgloss.Position, items ...string) string {
	nonEmpty := make([]string, 0, len(items))
	for _, item := range items {
		if strings.TrimSpace(item) == "" {
			continue
		}
		nonEmpty = append(nonEmpty, item)
	}
	if len(nonEmpty) == 0 {
		return ""
	}
	return lipgloss.JoinVertical(pos, nonEmpty...)
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func truncateText(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(strings.TrimSpace(s))
	if len(r) <= width {
		return string(r)
	}
	if width <= 3 {
		return string(r[:width])
	}
	return string(r[:width-3]) + "..."
}

func clampInt(v, minV, maxV int) int {
	if v < minV {
		return minV
	}
	if v > maxV {
		return maxV
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func fitText(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(strings.TrimSpace(s))
	if len(r) <= width {
		return string(r) + strings.Repeat(" ", width-len(r))
	}
	if width <= 3 {
		return string(r[:width])
	}
	return string(r[:width-3]) + "..."
}
