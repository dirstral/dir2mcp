package chat

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"dir2mcp/internal/dirstral/mcp"
	"dir2mcp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
)

type Options struct {
	MCPURL      string
	Transport   string
	Model       string
	Verbose     bool
	JSON        bool
	StartupHint string
}

var requiredTools = []string{
	protocol.ToolNameListFiles,
	protocol.ToolNameSearch,
	protocol.ToolNameOpenFile,
	protocol.ToolNameStats,
	protocol.ToolNameAsk,
}

var autoApprove = map[string]bool{
	protocol.ToolNameSearch:           true,
	protocol.ToolNameAsk:              true,
	protocol.ToolNameAskAudio:         true,
	protocol.ToolNameOpenFile:         true,
	protocol.ToolNameListFiles:        true,
	protocol.ToolNameStats:            true,
	protocol.ToolNameTranscribe:       true,
	protocol.ToolNameAnnotate:         true,
	protocol.ToolNameTranscribeAndAsk: true,
}

func Run(ctx context.Context, opts Options) error {
	if opts.Transport == "" {
		opts.Transport = "streamable-http"
	}
	if opts.Model == "" {
		opts.Model = "mistral-small-latest"
	}

	client := mcp.NewWithTransport(opts.MCPURL, opts.Transport, opts.Verbose)
	defer func() {
		_ = client.Close()
	}()
	if err := client.Initialize(ctx); err != nil {
		return fmt.Errorf("mcp initialize failed: %w", err)
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		return fmt.Errorf("tools/list failed: %w", err)
	}
	if err := validateTools(tools); err != nil {
		return err
	}
	opts.StartupHint = startupStatsHint(ctx, client)
	if opts.JSON {
		return RunJSONLoopWithIO(ctx, client, opts, os.Stdin, os.Stdout)
	}

	p := tea.NewProgram(initialModel(ctx, client, opts), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		return err
	}
	return nil
}

func AskQuestion(ctx context.Context, client *mcp.Client, question string) (string, error) {
	res, err := client.CallTool(ctx, protocol.ToolNameAsk, map[string]any{"question": question, "k": AskTopKForModel("")})
	if err != nil {
		return "", err
	}
	if answer, ok := res.StructuredContent["answer"].(string); ok && strings.TrimSpace(answer) != "" {
		return answer, nil
	}
	for _, c := range res.Content {
		if c.Text != "" {
			return c.Text, nil
		}
	}
	return "", nil
}

type jsonEvent struct {
	Version string         `json:"version"`
	Type    string         `json:"type"`
	Data    map[string]any `json:"data"`
}

func runJSONLoop(ctx context.Context, client *mcp.Client, opts Options, in io.Reader, out io.Writer) error {
	planner := NewPlanner(opts.Model)
	enc := json.NewEncoder(out)
	write := func(kind string, data map[string]any) error {
		return enc.Encode(jsonEvent{Version: "v1", Type: kind, Data: data})
	}

	if err := write("session", map[string]any{
		"mcp_url":    opts.MCPURL,
		"transport":  opts.Transport,
		"model":      opts.Model,
		"session_id": client.SessionID(),
	}); err != nil {
		return err
	}

	s := bufio.NewScanner(in)
	for s.Scan() {
		input := strings.TrimSpace(s.Text())
		if input == "" {
			continue
		}
		plan, err := planner.Plan(input)
		if err != nil {
			if err := write("error", map[string]any{"message": err.Error()}); err != nil {
				return err
			}
			continue
		}
		if plan.Quit {
			if err := write("exit", map[string]any{"reason": "user"}); err != nil {
				return err
			}
			return nil
		}
		if plan.Help {
			if err := write("help", map[string]any{"text": formatHelpPlain()}); err != nil {
				return err
			}
			continue
		}
		if plan.Clear {
			if err := write("cleared", map[string]any{"status": "ok"}); err != nil {
				return err
			}
			continue
		}
		if len(plan.Steps) == 0 {
			continue
		}

		var approvalTool string
		for _, step := range plan.Steps {
			if needsApproval(step.Tool) {
				approvalTool = step.Tool
				break
			}
		}
		if approvalTool != "" {
			if err := write("approval_required", map[string]any{"tool": approvalTool, "approved": false}); err != nil {
				return err
			}
			continue
		}
		execRes, err := ExecutePlan(ctx, client, plan)
		if err != nil {
			if err := write("error", map[string]any{"tool": plan.Steps[0].Tool, "message": err.Error()}); err != nil {
				return err
			}
			continue
		}
		usedTools := make([]string, 0, len(execRes.Executions))
		for _, ex := range execRes.Executions {
			usedTools = append(usedTools, ex.Tool)
		}
		if len(execRes.Executions) == 0 {
			if err := write("tool_result", map[string]any{
				"tool":               "",
				"args":               map[string]any{},
				"tools":              usedTools,
				"is_error":           false,
				"output":             execRes.Output,
				"citations":          execRes.Citations,
				"structured_content": map[string]any{},
			}); err != nil {
				return err
			}
			continue
		}
		last := execRes.Executions[len(execRes.Executions)-1]
		structuredContent := map[string]any{}
		if last.Result != nil && last.Result.StructuredContent != nil {
			structuredContent = last.Result.StructuredContent
		}
		if err := write("tool_result", map[string]any{
			"tool":               last.Tool,
			"args":               last.Args,
			"tools":              usedTools,
			"is_error":           last.Result != nil && last.Result.IsError,
			"output":             execRes.Output,
			"citations":          execRes.Citations,
			"structured_content": structuredContent,
		}); err != nil {
			return err
		}
	}
	if err := s.Err(); err != nil {
		return err
	}
	return nil
}

func RunJSONLoopWithIO(ctx context.Context, client *mcp.Client, opts Options, in io.Reader, out io.Writer) error {
	return runJSONLoop(ctx, client, opts, in, out)
}

func validateTools(tools []mcp.Tool) error {
	got := map[string]bool{}
	for _, t := range tools {
		got[t.Name] = true
	}
	missing := make([]string, 0)
	for _, req := range requiredTools {
		if !got[req] {
			missing = append(missing, req)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("required tools missing: %s", strings.Join(missing, ", "))
	}
	return nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func startupStatsHint(ctx context.Context, client *mcp.Client) string {
	statsCtx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer cancel()
	res, err := client.CallTool(statsCtx, protocol.ToolNameStats, map[string]any{})
	if err != nil {
		return ""
	}
	return startupStatsHintFromContent(res.StructuredContent)
}

func startupStatsHintFromContent(sc map[string]any) string {
	indexing, _ := sc["indexing"].(map[string]any)
	running, _ := indexing["running"].(bool)
	if !running {
		return ""
	}
	return "Indexing is still running; results may be partial."
}
