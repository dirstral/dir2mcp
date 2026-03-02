package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"dir2mcp/internal/dirstral/mcp"
	"dir2mcp/internal/dirstral/ui"
	"dir2mcp/internal/protocol"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type mcpResponseMsg struct {
	output string
	err    error
	quit   bool
	clear  bool
}

type chatModel struct {
	client    *mcp.Client
	ctx       context.Context
	modelName string
	viewport  viewport.Model
	textInput textinput.Model
	spinner   spinner.Model
	messages  []string
	banner    []string
	isLoading bool
	ready     bool
	width     int
	height    int
	showHelp  bool

	// For confirmation
	confirmingPlan TurnPlan
}

func initialModel(ctx context.Context, client *mcp.Client, opts Options) chatModel {
	ti := textinput.New()
	ti.Placeholder = "Ask a question or type /help..."
	ti.Focus()
	ti.CharLimit = 1000
	ti.Width = 80

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(ui.ClrBrand)

	msgs := connectedBanner(opts.MCPURL, opts.Transport, client.SessionID(), opts.Model, opts.StartupHint)

	return chatModel{
		client:    client,
		ctx:       ctx,
		modelName: opts.Model,
		textInput: ti,
		spinner:   s,
		messages:  msgs,
		banner:    append([]string(nil), msgs...),
	}
}

func (m chatModel) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, m.spinner.Tick)
}

func (m chatModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var (
		tiCmd tea.Cmd
		vpCmd tea.Cmd
		spCmd tea.Cmd
	)

	m.spinner, spCmd = m.spinner.Update(msg)

	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "?" || msg.String() == "ctrl+k" {
			m.showHelp = !m.showHelp
			m.applyWindowSize(m.width, m.height)
			return m, spCmd
		}
		if m.showHelp {
			switch msg.String() {
			case "esc", "q", "?", "ctrl+k":
				m.showHelp = false
				m.applyWindowSize(m.width, m.height)
			}
			return m, spCmd
		}
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			return m, tea.Quit
		case tea.KeyEnter:
			if m.isLoading {
				return m, spCmd
			}

			input := strings.TrimSpace(m.textInput.Value())

			if len(m.confirmingPlan.Steps) > 0 {
				m.textInput.SetValue("")
				inputLower := strings.ToLower(input)
				approvalTool := firstApprovalTool(m.confirmingPlan)
				if approvalTool == "" {
					approvalTool = "pending plan"
				}
				if inputLower == "y" || inputLower == "yes" {
					m.messages = append(m.messages, ui.Brand.Render("Approving "+approvalTool+"..."))
					m.isLoading = true
					plan := m.confirmingPlan
					m.confirmingPlan = TurnPlan{}
					m.viewport.SetContent(strings.Join(m.messages, "\n\n"))
					m.viewport.GotoBottom()
					return m, tea.Batch(m.runPlanCmd(plan), m.spinner.Tick)
				}
				m.messages = append(m.messages, ui.Dim("Cancelled "+approvalTool+"."))
				m.confirmingPlan = TurnPlan{}
				m.viewport.SetContent(strings.Join(m.messages, "\n\n"))
				m.viewport.GotoBottom()
				return m, spCmd
			}

			if input == "" {
				return m, spCmd
			}
			m.textInput.SetValue("")

			m.messages = append(m.messages, ui.Prompt("chat")+input)

			m.isLoading = true
			m.viewport.SetContent(strings.Join(m.messages, "\n\n"))
			m.viewport.GotoBottom()

			return m, tea.Batch(m.processInputCmd(input), m.spinner.Tick)
		}

	case tea.WindowSizeMsg:
		m.applyWindowSize(msg.Width, msg.Height)

	case mcpResponseMsg:
		m.isLoading = false
		if msg.quit {
			return m, tea.Quit
		}
		if msg.clear {
			m.messages = append([]string(nil), m.banner...)
			m.viewport.SetContent(strings.Join(m.messages, "\n\n"))
			m.viewport.GotoBottom()
			return m, nil
		}
		if msg.err != nil {
			errLine := ui.Errorf("%v", msg.err)
			if hint := mcp.ActionableMessageFromError(msg.err); hint != "" {
				errLine += "\n" + ui.Dim("Hint: "+hint)
			}
			m.messages = append(m.messages, errLine)
		} else if msg.output != "" {
			m.messages = append(m.messages, msg.output)
		}
		m.viewport.SetContent(strings.Join(m.messages, "\n\n"))
		m.viewport.GotoBottom()
		return m, nil

	case approvalReqMsg:
		m.isLoading = false
		m.confirmingPlan = msg.plan
		approvalTool := firstApprovalTool(msg.plan)
		if approvalTool == "" {
			approvalTool = "pending plan"
		}
		m.messages = append(m.messages, ui.Yellow.Render("Approval required for ")+ui.Brand.Render(approvalTool))
		m.viewport.SetContent(strings.Join(m.messages, "\n\n"))
		m.viewport.GotoBottom()
		return m, nil
	}

	m.textInput, tiCmd = m.textInput.Update(msg)
	m.viewport, vpCmd = m.viewport.Update(msg)

	return m, tea.Batch(tiCmd, vpCmd, spCmd)
}

func (m chatModel) View() string {
	if !m.ready {
		return "\n  Initializing..."
	}

	var b strings.Builder

	b.WriteString(m.viewport.View())
	b.WriteString("\n")
	if m.showHelp {
		help := m.renderHelpBlock(m.width, m.height)
		b.WriteString(help)
		b.WriteString("\n")
	}

	if len(m.confirmingPlan.Steps) > 0 {
		approvalTool := firstApprovalTool(m.confirmingPlan)
		if approvalTool == "" {
			approvalTool = "pending plan"
		}
		prompt := fmt.Sprintf("%s %s %s", ui.Yellow.Render("Run tool"), ui.Brand.Render(approvalTool+"?"), ui.Dim("[y/N]: "))
		b.WriteString(lipgloss.NewStyle().MaxWidth(max(m.width-2, 20)).Render(prompt))
		b.WriteString(m.textInput.View())
	} else {
		if m.isLoading {
			b.WriteString(m.spinner.View() + " ")
		} else {
			b.WriteString(ui.Prompt("chat"))
		}
		b.WriteString(m.textInput.View())
	}
	b.WriteString("\n")
	b.WriteString(ui.Dim("? help"))

	return b.String()
}

func (m *chatModel) applyWindowSize(width, height int) {
	if width <= 0 || height <= 0 {
		return
	}

	m.width = width
	m.height = height

	vpWidth := max(width-2, 1)
	m.textInput.Width = max(width-16, 1)

	reservedHeight := 4
	if m.showHelp {
		helpBlock := m.renderHelpBlock(width, height)
		reservedHeight += lipgloss.Height(helpBlock) + 1
	}
	vpHeight := max(height-reservedHeight, 1)

	if !m.ready {
		m.viewport = viewport.New(vpWidth, vpHeight)
		m.viewport.SetContent(strings.Join(m.messages, "\n\n"))
		m.ready = true
		return
	}

	m.viewport.Width = vpWidth
	m.viewport.Height = vpHeight
}

func (m chatModel) renderHelpBlock(width, height int) string {
	helpText := formatHelp()
	if width < 56 || height < 14 {
		helpText = formatHelpCompact()
	}

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ui.ClrSubtle).
		Padding(0, 1).
		MaxWidth(max(width-2, 1)).
		Render(helpText)
}

type helpCommand struct {
	name        string
	description string
}

var helpCommands = []helpCommand{
	{name: "/help", description: "Show help"},
	{name: "/quit", description: "Exit Chat"},
	{name: "/clear", description: "Clear chat history"},
	{name: "/list [prefix]", description: "List indexed files"},
	{name: "/search <query>", description: "Search corpus"},
	{name: "/open <rel_path>", description: "Open file from index"},
	{name: "/transcribe <rel_path>", description: "Force transcribe audio"},
	{name: "/annotate <rel_path> <schema_json>", description: "Extract structured JSON"},
	{name: "/transcribe_and_ask <rel_path> <question>", description: "Transcribe & Ask"},
}

func formatHelpCompact() string {
	var b strings.Builder
	b.WriteString(ui.Brand.Render("Help:\n"))
	for _, command := range helpCommands {
		fmt.Fprintf(&b, "  %s  %s\n", ui.Keyword.Render(command.name), ui.Muted.Render(command.description))
	}
	b.WriteString(ui.Dim("  Any other text is sent to " + protocol.ToolNameAsk))
	b.WriteString("\n")
	b.WriteString(ui.Dim("  Press ? to close."))
	return b.String()
}

func (m *chatModel) processInputCmd(input string) tea.Cmd {
	return func() tea.Msg {
		plan, err := PlanTurn(input, m.modelName)
		if err != nil {
			return mcpResponseMsg{err: err}
		}
		if plan.Quit {
			return mcpResponseMsg{quit: true}
		}
		if plan.Help {
			return mcpResponseMsg{output: formatHelp()}
		}
		if plan.Clear {
			return mcpResponseMsg{clear: true}
		}
		if len(plan.Steps) == 0 {
			return mcpResponseMsg{}
		}
		return m.checkApprovalAndRunPlan(plan)
	}
}

func (m *chatModel) checkApprovalAndRunPlan(plan TurnPlan) tea.Msg {
	for _, step := range plan.Steps {
		if needsApproval(step.Tool) {
			return approvalReqMsg{plan: plan}
		}
	}
	execRes, err := ExecutePlan(m.ctx, m.client, plan)
	if err != nil {
		return mcpResponseMsg{err: err}
	}
	return mcpResponseMsg{output: execRes.Output}
}

func (m *chatModel) runPlanCmd(plan TurnPlan) tea.Cmd {
	return func() tea.Msg {
		execRes, err := ExecutePlan(m.ctx, m.client, plan)
		if err != nil {
			return mcpResponseMsg{err: err}
		}
		return mcpResponseMsg{output: execRes.Output}
	}
}

type approvalReqMsg struct {
	plan TurnPlan
}

func firstApprovalTool(plan TurnPlan) string {
	for _, step := range plan.Steps {
		if needsApproval(step.Tool) {
			return step.Tool
		}
	}
	if len(plan.Steps) == 0 {
		return ""
	}
	return plan.Steps[0].Tool
}

func formatHelp() string {
	var b strings.Builder
	b.WriteString(ui.Brand.Render("Commands:\n"))
	for _, command := range helpCommands {
		fmt.Fprintf(&b, "  %s  %s\n", ui.Keyword.Render(command.name), ui.Muted.Render(command.description))
	}
	b.WriteString(ui.Dim("  Any other text is sent to " + protocol.ToolNameAsk))
	return b.String()
}

func formatHelpPlain() string {
	lines := make([]string, 0, len(helpCommands)+1)
	for _, command := range helpCommands {
		lines = append(lines, command.name+" - "+command.description)
	}
	lines = append(lines, "Any other text is sent to "+protocol.ToolNameAsk)
	return strings.Join(lines, "\n")
}

func renderResultString(tool string, res *mcp.ToolCallResult) string {
	switch tool {
	case protocol.ToolNameListFiles:
		return renderListFilesString(res.StructuredContent)
	case protocol.ToolNameSearch:
		return renderSearchString(res.StructuredContent)
	case protocol.ToolNameOpenFile:
		return renderOpenFileString(res.StructuredContent)
	case protocol.ToolNameAsk, protocol.ToolNameTranscribeAndAsk:
		return renderAskString(tool, res.StructuredContent)
	case protocol.ToolNameAnnotate:
		return renderAnnotateString(res.StructuredContent)
	case protocol.ToolNameTranscribe:
		return renderTranscribeString(res.StructuredContent)
	default:
		var b strings.Builder
		for _, c := range res.Content {
			if c.Text != "" {
				b.WriteString(c.Text + "\n")
			}
		}
		return b.String()
	}
}

func renderListFilesString(sc map[string]any) string {
	files, _ := sc["files"].([]any)
	if len(files) == 0 {
		return ui.Dim("(no files)")
	}
	var b strings.Builder
	for i, f := range files {
		if i >= 20 {
			b.WriteString(ui.Dim("...\n"))
			break
		}
		m, ok := f.(map[string]any)
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "  %s %s\n", ui.Cyan.Render(asString(m["rel_path"])), ui.Dim("("+asString(m["doc_type"])+")"))
	}
	return b.String()
}

func renderSearchString(sc map[string]any) string {
	hits, _ := sc["hits"].([]any)
	if len(hits) == 0 {
		return ui.Dim("(no hits)")
	}
	var b strings.Builder
	for i, h := range hits {
		if i >= 8 {
			break
		}
		m, ok := h.(map[string]any)
		if !ok {
			continue
		}
		path := asString(m["rel_path"])
		snippet := strings.TrimSpace(asString(m["snippet"]))
		score := m["score"]
		citation := ""
		if span, ok := m["span"].(map[string]any); ok {
			citation = mcp.CitationForSpan(path, span)
		}
		fmt.Fprintf(&b, "%s score=%s %s\n", ui.Brand.Render(fmt.Sprintf("%d)", i+1)), ui.Score(score), ui.Citation(citation))
		if snippet != "" {
			fmt.Fprintf(&b, "   %s\n", ui.Muted.Render(snippet))
		}
	}
	return b.String()
}

func renderOpenFileString(sc map[string]any) string {
	path := asString(sc["rel_path"])
	content := asString(sc["content"])
	var b strings.Builder
	if span, ok := sc["span"].(map[string]any); ok {
		b.WriteString(ui.Citation(mcp.CitationForSpan(path, span)) + "\n")
	}
	b.WriteString(content)
	return b.String()
}

func renderAskString(toolName string, sc map[string]any) string {
	var b strings.Builder
	answer := strings.TrimSpace(asString(sc["answer"]))
	if answer != "" {
		b.WriteString(answer + "\n")
	}
	if ordered := citationsFor(toolName, sc); len(ordered) > 0 {
		styled := make([]string, len(ordered))
		for i, c := range ordered {
			styled[i] = ui.Citation(c)
		}
		fmt.Fprintf(&b, "%s %s\n", ui.Dim("Sources:"), strings.Join(styled, ui.Dim(", ")))
	}
	return strings.TrimSpace(b.String())
}

func renderAnnotateString(sc map[string]any) string {
	var b strings.Builder
	path := asString(sc["rel_path"])
	b.WriteString(ui.Cyan.Render("Annotated: "+path) + "\n")
	if text, ok := sc["annotation_text_preview"].(string); ok && text != "" {
		b.WriteString(ui.Dim("Preview: "+text) + "\n")
	}
	if jsonObj, ok := sc["annotation_json"]; ok && jsonObj != nil {
		bytes, err := json.MarshalIndent(jsonObj, "", "  ")
		if err == nil {
			b.WriteString(string(bytes))
		} else {
			compact, compactErr := json.Marshal(jsonObj)
			if compactErr == nil {
				fmt.Fprintf(&b, "annotation_json (marshal error: %v): %s", err, string(compact))
			} else {
				fmt.Fprintf(&b, "annotation_json (marshal error: %v; fallback error: %v): %v", err, compactErr, jsonObj)
			}
		}
	}
	if ordered := citationsFor(protocol.ToolNameAnnotate, sc); len(ordered) > 0 {
		styled := make([]string, len(ordered))
		for i, c := range ordered {
			styled[i] = ui.Citation(c)
		}
		fmt.Fprintf(&b, "\n%s %s\n", ui.Dim("Sources:"), strings.Join(styled, ui.Dim(", ")))
	}
	return strings.TrimSpace(b.String())
}

func renderTranscribeString(sc map[string]any) string {
	var b strings.Builder
	path := asString(sc["rel_path"])
	b.WriteString(ui.Cyan.Render("Transcribed: "+path) + "\n")
	if provider := asString(sc["provider"]); provider != "" {
		b.WriteString(ui.Dim("Provider: "+provider) + "\n")
	}
	if segments, ok := sc["segments"].([]any); ok && len(segments) > 0 {
		for _, s := range segments {
			seg, ok := s.(map[string]any)
			if !ok {
				continue
			}
			text := asString(seg["text"])
			if text == "" {
				text = asString(seg["transcript"])
			}
			timeRange := ""
			if start, ok := seg["start"]; ok {
				timeRange = formatTime(start)
				if end, ok := seg["end"]; ok {
					timeRange += "-" + formatTime(end)
				}
			} else if t, ok := seg["time"]; ok {
				timeRange = formatTime(t)
			}

			citation := ""
			if span, ok := seg["span"].(map[string]any); ok {
				citation = mcp.CitationForSpan(path, span)
			}

			if timeRange != "" {
				fmt.Fprintf(&b, "%s ", ui.Dim(timeRange))
			}
			b.WriteString(text)
			if citation != "" {
				fmt.Fprintf(&b, " %s", ui.Citation(citation))
			}
			b.WriteString("\n")
		}
	} else {
		text := asString(sc["text"])
		if text == "" {
			text = asString(sc["transcript"])
		}
		timeRange := ""
		if start, ok := sc["start"]; ok {
			timeRange = formatTime(start)
			if end, ok := sc["end"]; ok {
				timeRange += "-" + formatTime(end)
			}
		} else if t, ok := sc["time"]; ok {
			timeRange = formatTime(t)
		}

		citation := ""
		if span, ok := sc["span"].(map[string]any); ok {
			citation = mcp.CitationForSpan(path, span)
		}

		if text != "" || citation != "" || timeRange != "" {
			if timeRange != "" {
				fmt.Fprintf(&b, "%s ", ui.Dim(timeRange))
			}
			b.WriteString(text)
			if citation != "" {
				fmt.Fprintf(&b, " %s", ui.Citation(citation))
			}
		} else {
			b.WriteString("Transcription complete.")
		}
	}
	return strings.TrimSpace(b.String())
}

func formatTime(v any) string {
	s := asString(v)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	totalSeconds := int(f)
	minutes := totalSeconds / 60
	seconds := totalSeconds % 60
	return fmt.Sprintf("%02d:%02d", minutes, seconds)
}
