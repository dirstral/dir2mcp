package voice

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"dir2mcp/internal/dirstral/chat"
	"dir2mcp/internal/dirstral/mcp"
	"dir2mcp/internal/dirstral/ui"
	"dir2mcp/internal/protocol"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type appState int

const (
	stateIdle appState = iota
	stateRecording
	stateTranscribing
	stateThinking
	stateConfirming
	stateSpeaking
)

type voiceModel struct {
	opts           Options
	client         *mcp.Client
	ctx            context.Context
	viewport       viewport.Model
	spinner        spinner.Model
	state          appState
	messages       []string
	ready          bool
	width          int
	height         int
	showHelp       bool
	helpCache      string
	helpCacheWidth int
	pendingParsed  chat.ParsedInput
	hasPendingTool bool
	parseInputFn   func(input, model string) chat.ParsedInput
	executeParsed  func(ctx context.Context, client *mcp.Client, parsed chat.ParsedInput) (*chat.ToolExecution, error)
}

const (
	tinyResizeThresholdWidth  = 2
	tinyResizeThresholdHeight = 2
)

type recordDoneMsg struct {
	path string
	err  error
}

type transcribeDoneMsg struct {
	text string
	err  error
}

type thinkDoneMsg struct {
	answer    string
	audioPath string
	err       error
}

type approvalReqMsg struct {
	parsed chat.ParsedInput
}

type speakDoneMsg struct {
	err error
}

func initialModel(ctx context.Context, client *mcp.Client, opts Options) voiceModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(ui.ClrBrand)

	msgs := []string{
		ui.Info("Connected to", opts.MCPURL),
		ui.Dim("Voice mode: Press Enter to record (6s), Esc/Ctrl+C to quit."),
	}

	return voiceModel{
		opts:          opts,
		client:        client,
		ctx:           ctx,
		spinner:       s,
		messages:      msgs,
		state:         stateIdle,
		parseInputFn:  chat.ParseInput,
		executeParsed: chat.ExecuteParsed,
	}
}

func (m voiceModel) Init() tea.Cmd {
	return m.spinner.Tick
}

func (m voiceModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var (
		vpCmd tea.Cmd
		spCmd tea.Cmd
	)

	m.spinner, spCmd = m.spinner.Update(msg)

	switch msg := msg.(type) {
	case tea.KeyMsg:
		if m.state == stateConfirming {
			switch strings.ToLower(msg.String()) {
			case "y":
				if !m.hasPendingTool {
					m.state = stateIdle
					m.scrollToBottom()
					return m, nil
				}
				m.messages = append(m.messages, ui.Brand.Render("Approving "+m.pendingParsed.Tool+"..."))
				parsed := m.pendingParsed
				m.pendingParsed = chat.ParsedInput{}
				m.hasPendingTool = false
				m.state = stateThinking
				m.scrollToBottom()
				return m, tea.Batch(m.runParsedCmd(parsed), m.spinner.Tick)
			case "n":
				if m.hasPendingTool {
					m.messages = append(m.messages, ui.Dim("Cancelled "+m.pendingParsed.Tool+"."))
				}
				m.pendingParsed = chat.ParsedInput{}
				m.hasPendingTool = false
				m.state = stateIdle
				m.scrollToBottom()
				return m, nil
			}
		}

		if msg.String() == "?" || msg.String() == "ctrl+k" {
			m.showHelp = !m.showHelp
			m.invalidateHelpCache()
			m.relayout()
			return m, nil
		}
		if m.showHelp {
			switch msg.String() {
			case "esc", "q", "?", "ctrl+k":
				m.showHelp = false
				m.invalidateHelpCache()
				m.relayout()
			}
			return m, nil
		}
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			return m, tea.Quit
		case tea.KeyEnter:
			if m.state == stateIdle {
				m.state = stateRecording
				m.scrollToBottom()
				return m, tea.Batch(m.recordCmd(), m.spinner.Tick)
			}
		}

	case tea.WindowSizeMsg:
		m.applyWindowSize(msg.Width, msg.Height)

	case recordDoneMsg:
		if msg.err != nil {
			m.messages = append(m.messages, ui.Errorf("recording: %v", msg.err))
			m.state = stateIdle
			m.scrollToBottom()
			return m, nil
		}
		m.state = stateTranscribing
		m.scrollToBottom()
		return m, m.transcribeCmd(msg.path)

	case transcribeDoneMsg:
		if msg.err != nil {
			m.messages = append(m.messages, ui.Errorf("transcription: %v", msg.err))
			m.state = stateIdle
			m.scrollToBottom()
			return m, nil
		}
		m.messages = append(m.messages, ui.Dim("you said: ")+msg.text)

		if m.opts.TranscriptOnly {
			m.state = stateIdle
			m.scrollToBottom()
			return m, nil
		}

		m.state = stateThinking
		m.scrollToBottom()
		return m, m.thinkCmd(msg.text)

	case approvalReqMsg:
		m.pendingParsed = msg.parsed
		m.hasPendingTool = true
		m.state = stateConfirming
		m.messages = append(m.messages, ui.Yellow.Render("Approval required for ")+ui.Brand.Render(msg.parsed.Tool))
		m.scrollToBottom()
		return m, nil

	case thinkDoneMsg:
		if msg.err != nil {
			errLine := ui.Errorf("tool call: %v", msg.err)
			if hint := mcp.ActionableMessageFromError(msg.err); hint != "" {
				errLine += "\n" + ui.Dim("Hint: "+hint)
			}
			m.messages = append(m.messages, errLine)
			m.state = stateIdle
			m.scrollToBottom()
			return m, nil
		}
		m.messages = append(m.messages, ui.Brand.Render("assistant: ")+msg.answer)

		if m.opts.Mute || msg.audioPath == "" {
			if msg.audioPath != "" {
				_ = os.Remove(msg.audioPath)
			}
			m.state = stateIdle
			m.scrollToBottom()
			return m, nil
		}

		m.state = stateSpeaking
		m.scrollToBottom()
		return m, m.speakCmd(msg.audioPath)

	case speakDoneMsg:
		if msg.err != nil {
			m.messages = append(m.messages, ui.Errorf("playback: %v", msg.err))
		}
		m.state = stateIdle
		m.scrollToBottom()
		return m, nil
	}

	m.viewport, vpCmd = m.viewport.Update(msg)
	return m, tea.Batch(vpCmd, spCmd)
}

func (m *voiceModel) scrollToBottom() {
	m.viewport.SetContent(strings.Join(m.messages, "\n\n"))
	m.viewport.GotoBottom()
}

func (m voiceModel) View() string {
	if !m.ready {
		return "\n  Initializing..."
	}

	renderWidth := m.renderWidth()
	var b strings.Builder
	b.WriteString(m.viewport.View())
	b.WriteString("\n")
	if m.showHelp {
		help := m.helpForWidth(renderWidth)
		b.WriteString(help)
		b.WriteString("\n")
	}

	status := ""
	switch m.state {
	case stateIdle:
		status = ui.Subtle.Render("[ Idle: Press Enter to record ]")
	case stateRecording:
		status = ui.Red.Render(m.spinner.View() + " [🔴 Recording audio...]")
	case stateTranscribing:
		status = ui.Yellow.Render(m.spinner.View() + " [⏳ Transcribing...]")
	case stateThinking:
		status = ui.Cyan.Render(m.spinner.View() + " [🧠 Asking dir2mcp...]")
	case stateConfirming:
		status = ui.Yellow.Render("[ Approval required: run " + m.pendingParsed.Tool + "? y/N ]")
	case stateSpeaking:
		status = ui.Green.Render(m.spinner.View() + " [🔊 Speaking...]")
	}

	b.WriteString(lipgloss.NewStyle().MaxWidth(renderWidth).Render(status))
	b.WriteString("\n")
	b.WriteString(ui.Dim("? help"))
	return b.String()
}

func (m *voiceModel) applyWindowSize(width, height int) {
	if m.ready {
		if width <= tinyResizeThresholdWidth {
			width = m.width
		}
		if height <= tinyResizeThresholdHeight {
			height = m.height
		}
	}

	if width <= 0 {
		width = 1
	}
	if height <= 0 {
		height = 1
	}

	m.width = width
	m.height = height

	vpWidth := m.renderWidth()
	help := m.helpForWidth(vpWidth)
	vpHeight := maxInt(height-m.footerHeight(help), 1)

	if !m.ready {
		m.viewport = viewport.New(vpWidth, vpHeight)
		m.viewport.SetContent(strings.Join(m.messages, "\n\n"))
		m.ready = true
		return
	}

	m.viewport.Width = vpWidth
	m.viewport.Height = vpHeight
}

func (m *voiceModel) relayout() {
	if !m.ready {
		return
	}
	m.applyWindowSize(m.width, m.height)
}

func (m voiceModel) renderWidth() int {
	return maxInt(m.width-2, 1)
}

func (m voiceModel) footerHeight(help string) int {
	footerLines := 2
	if help != "" {
		footerLines += 1 + lipgloss.Height(help)
	}
	return footerLines
}

func (m *voiceModel) helpForWidth(width int) string {
	if !m.showHelp {
		return ""
	}
	if m.helpCache != "" && m.helpCacheWidth == width {
		return m.helpCache
	}
	m.helpCacheWidth = width
	m.helpCache = renderHelp(width)
	return m.helpCache
}

func (m *voiceModel) invalidateHelpCache() {
	m.helpCache = ""
	m.helpCacheWidth = 0
}

func renderHelp(width int) string {
	helpText := strings.Join([]string{
		ui.Brand.Render("Voice Keymap"),
		ui.Muted.Render("enter  start recording"),
		ui.Muted.Render("esc    quit"),
		ui.Muted.Render("?      toggle help"),
	}, "\n")

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ui.ClrSubtle).
		Padding(0, 1).
		MaxWidth(maxInt(width, 1)).
		Render(helpText)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (m *voiceModel) recordCmd() tea.Cmd {
	return func() tea.Msg {
		path, err := recordAudio(m.ctx, m.opts.Device)
		return recordDoneMsg{path: path, err: err}
	}
}

func (m *voiceModel) transcribeCmd(path string) tea.Cmd {
	return func() tea.Msg {
		defer func() {
			_ = os.Remove(path)
		}()
		text, err := transcribeElevenLabs(m.ctx, m.opts.BaseURL, path)
		return transcribeDoneMsg{text: text, err: err}
	}
}

func (m *voiceModel) thinkCmd(question string) tea.Cmd {
	return func() tea.Msg {
		parsed := m.parseInputFn(question, m.opts.Model)
		if parsed.Tool != "" && chat.RequiresApproval(parsed.Tool) {
			return approvalReqMsg{parsed: parsed}
		}
		answer, audioPath, err := m.executeParsedForVoice(parsed)
		if err != nil {
			return thinkDoneMsg{err: err}
		}
		return thinkDoneMsg{answer: answer, audioPath: audioPath}
	}
}

func (m *voiceModel) runParsedCmd(parsed chat.ParsedInput) tea.Cmd {
	return func() tea.Msg {
		answer, audioPath, err := m.executeParsedForVoice(parsed)
		if err != nil {
			return thinkDoneMsg{err: err}
		}
		return thinkDoneMsg{answer: answer, audioPath: audioPath}
	}
}

func (m *voiceModel) speakCmd(audioPath string) tea.Cmd {
	return func() tea.Msg {
		defer func() {
			_ = os.Remove(audioPath)
		}()
		err := playAudio(m.ctx, audioPath)
		return speakDoneMsg{err: err}
	}
}

func (m *voiceModel) executeParsedForVoice(parsed chat.ParsedInput) (string, string, error) {
	if parsed.Tool == "" {
		return "", "", nil
	}
	if parsed.Tool != protocol.ToolNameAsk {
		execRes, err := m.executeParsed(m.ctx, m.client, parsed)
		if err != nil {
			return "", "", err
		}
		return strings.TrimSpace(execRes.Output), "", nil
	}

	askAudioArgs := map[string]any{
		"question": asString(parsed.Args["question"]),
	}
	if askAudioArgs["question"] == "" {
		return "", "", fmt.Errorf("question is required")
	}
	if k, ok := parsed.Args["k"]; ok {
		askAudioArgs["k"] = k
	}
	if voiceID := strings.TrimSpace(m.opts.Voice); voiceID != "" {
		askAudioArgs["voice_id"] = voiceID
	}

	execRes, err := m.executeParsed(m.ctx, m.client, chat.ParsedInput{Tool: protocol.ToolNameAskAudio, Args: askAudioArgs})
	if err != nil {
		return "", "", err
	}
	answer, audioB64 := extractAskAudioPayload(execRes)
	if answer == "" {
		answer = strings.TrimSpace(execRes.Output)
	}
	if audioB64 == "" {
		if fallback := strings.TrimSpace(execRes.Output); fallback != "" {
			return fallback, "", nil
		}
		return answer, "", nil
	}

	audioData, decodeErr := base64.StdEncoding.DecodeString(audioB64)
	if decodeErr != nil {
		return answer, "", nil
	}
	path, writeErr := writeTempMP3(audioData)
	if writeErr != nil {
		return answer, "", nil
	}
	return answer, path, nil
}

func extractAskAudioPayload(execRes *chat.ToolExecution) (string, string) {
	if execRes == nil || execRes.Result == nil {
		return "", ""
	}
	res := execRes.Result
	answer := strings.TrimSpace(asString(res.StructuredContent["answer"]))
	audioB64 := extractAudioFromStructuredContent(res.StructuredContent)
	if audioB64 != "" {
		return answer, audioB64
	}
	for _, c := range res.Content {
		if answer == "" && c.Type == "text" {
			answer = strings.TrimSpace(c.Text)
		}
	}
	return answer, extractAudioFromRawResult(res.Raw)
}

func extractAudioFromStructuredContent(sc map[string]any) string {
	audio, _ := sc["audio"].(map[string]any)
	if audio == nil {
		return ""
	}
	return strings.TrimSpace(asString(audio["data"]))
}

func extractAudioFromRawResult(raw map[string]any) string {
	result, _ := raw["result"].(map[string]any)
	items, _ := result["content"].([]any)
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if asString(m["type"]) != "audio" {
			continue
		}
		if data := strings.TrimSpace(asString(m["data"])); data != "" {
			return data
		}
	}
	return ""
}

func writeTempMP3(audio []byte) (string, error) {
	out, err := os.CreateTemp(os.TempDir(), "dirstral-ask-audio-*.mp3")
	if err != nil {
		return "", err
	}
	defer func() {
		_ = out.Close()
	}()
	if _, err := out.Write(audio); err != nil {
		return "", err
	}
	return out.Name(), nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
