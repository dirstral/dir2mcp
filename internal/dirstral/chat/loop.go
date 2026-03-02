package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"dir2mcp/internal/dirstral/mcp"
	"dir2mcp/internal/dirstral/ui"
	"dir2mcp/internal/protocol"
)

var ErrNilMCPClient = errors.New("nil mcp.Client")

type ParsedInput struct {
	Quit  bool
	Help  bool
	Clear bool
	Tool  string
	Args  map[string]any
}

type PlanStep struct {
	Tool string
	Args map[string]any
}

type TurnPlan struct {
	Quit      bool
	Help      bool
	Clear     bool
	Steps     []PlanStep
	Synthesis string
}

type TurnPlanner interface {
	Plan(input string) (TurnPlan, error)
}

type modelProfile struct {
	AskTopK        int
	SearchTopK     int
	UseSearchFirst bool
	Synthesis      string
}

type heuristicPlanner struct {
	profile modelProfile
}

type ToolExecution struct {
	Tool       string
	Args       map[string]any
	Result     *mcp.ToolCallResult
	Output     string
	Citations  []string
	NeedsHuman bool
}

type TurnExecution struct {
	Executions []*ToolExecution
	Output     string
	Citations  []string
}

func NewPlanner(model string) TurnPlanner {
	return heuristicPlanner{profile: profileForModel(model)}
}

func profileForModel(model string) modelProfile {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(m, "large"):
		return modelProfile{AskTopK: 12, SearchTopK: 8, UseSearchFirst: true, Synthesis: "analytical"}
	case strings.Contains(m, "medium"):
		return modelProfile{AskTopK: 12, SearchTopK: 6, UseSearchFirst: true, Synthesis: "balanced"}
	case strings.Contains(m, "small"), strings.Contains(m, "mini"):
		return modelProfile{AskTopK: 6, SearchTopK: 4, UseSearchFirst: false, Synthesis: "concise"}
	default:
		return modelProfile{AskTopK: 8, SearchTopK: 5, UseSearchFirst: false, Synthesis: "balanced"}
	}
}

func (p heuristicPlanner) Plan(input string) (TurnPlan, error) {
	trimmed := strings.TrimSpace(input)
	switch {
	case trimmed == "":
		return TurnPlan{}, nil
	case trimmed == "/quit" || trimmed == "/exit":
		return TurnPlan{Quit: true}, nil
	case trimmed == "/help":
		return TurnPlan{Help: true}, nil
	case trimmed == "/clear":
		return TurnPlan{Clear: true}, nil
	case strings.HasPrefix(trimmed, "/list"):
		prefix := strings.TrimSpace(strings.TrimPrefix(trimmed, "/list"))
		return TurnPlan{Steps: []PlanStep{{Tool: protocol.ToolNameListFiles, Args: map[string]any{"path_prefix": prefix, "limit": 30}}}}, nil
	case strings.HasPrefix(trimmed, "/search "):
		query := strings.TrimSpace(strings.TrimPrefix(trimmed, "/search"))
		return TurnPlan{Steps: []PlanStep{{Tool: protocol.ToolNameSearch, Args: map[string]any{"query": query, "k": 8}}}}, nil
	case strings.HasPrefix(trimmed, "/open"):
		args := strings.Fields(strings.TrimPrefix(trimmed, "/open"))
		if len(args) == 0 {
			return TurnPlan{}, fmt.Errorf("usage: /open <rel_path>")
		}
		return TurnPlan{Steps: []PlanStep{{Tool: protocol.ToolNameOpenFile, Args: map[string]any{"rel_path": args[0]}}}}, nil
	case trimmed == "/transcribe_and_ask" || strings.HasPrefix(trimmed, "/transcribe_and_ask "):
		args := strings.TrimSpace(strings.TrimPrefix(trimmed, "/transcribe_and_ask"))
		parts := strings.Fields(args)
		if len(parts) < 2 {
			return TurnPlan{}, fmt.Errorf("usage: /transcribe_and_ask <rel_path> <question>")
		}
		path := parts[0]
		question := strings.TrimSpace(strings.TrimPrefix(args, path))
		return TurnPlan{Steps: []PlanStep{{Tool: protocol.ToolNameTranscribeAndAsk, Args: map[string]any{"rel_path": path, "question": question, "k": p.profile.AskTopK}}}}, nil
	case trimmed == "/transcribe" || strings.HasPrefix(trimmed, "/transcribe "):
		path := strings.TrimSpace(strings.TrimPrefix(trimmed, "/transcribe"))
		if path == "" {
			return TurnPlan{}, fmt.Errorf("usage: /transcribe <rel_path>")
		}
		return TurnPlan{Steps: []PlanStep{{Tool: protocol.ToolNameTranscribe, Args: map[string]any{"rel_path": path}}}}, nil
	case trimmed == "/annotate" || strings.HasPrefix(trimmed, "/annotate "):
		args := strings.TrimSpace(strings.TrimPrefix(trimmed, "/annotate"))
		parts := strings.Fields(args)
		if len(parts) < 2 {
			return TurnPlan{}, fmt.Errorf("usage: /annotate <rel_path> <schema_json>")
		}
		path := parts[0]
		schemaStr := strings.TrimSpace(strings.TrimPrefix(args, path))
		var schema map[string]any
		if err := json.Unmarshal([]byte(schemaStr), &schema); err != nil {
			return TurnPlan{}, fmt.Errorf("invalid schema json: %w", err)
		}
		return TurnPlan{Steps: []PlanStep{{Tool: protocol.ToolNameAnnotate, Args: map[string]any{"rel_path": path, "schema_json": schema}}}}, nil
	default:
		steps := make([]PlanStep, 0, 2)
		if p.profile.UseSearchFirst {
			steps = append(steps, PlanStep{Tool: protocol.ToolNameSearch, Args: map[string]any{"query": trimmed, "k": p.profile.SearchTopK}})
		}
		steps = append(steps, PlanStep{Tool: protocol.ToolNameAsk, Args: map[string]any{"question": trimmed, "k": p.profile.AskTopK}})
		return TurnPlan{Steps: steps, Synthesis: p.profile.Synthesis}, nil
	}
}

func ParseInput(input, model string) ParsedInput {
	planner := NewPlanner(model)
	plan, err := planner.Plan(input)
	if err != nil {
		return ParsedInput{Help: true}
	}
	parsed := ParsedInput{Quit: plan.Quit, Help: plan.Help, Clear: plan.Clear}
	if len(plan.Steps) == 0 {
		return parsed
	}
	parsed.Tool = plan.Steps[0].Tool
	parsed.Args = plan.Steps[0].Args
	return parsed
}

func AskTopKForModel(model string) int {
	return profileForModel(model).AskTopK
}

func ExecuteParsed(ctx context.Context, client *mcp.Client, parsed ParsedInput) (*ToolExecution, error) {
	if parsed.Tool == "" {
		return &ToolExecution{}, nil
	}
	if client == nil {
		return nil, ErrNilMCPClient
	}
	res, err := client.CallTool(ctx, parsed.Tool, parsed.Args)
	if err != nil {
		return nil, err
	}
	output := renderResultString(parsed.Tool, res)
	if res.IsError {
		output = ui.Errorf("%s returned an error\n%s", parsed.Tool, output)
	}
	return &ToolExecution{
		Tool:      parsed.Tool,
		Args:      parsed.Args,
		Result:    res,
		Output:    output,
		Citations: citationsFor(parsed.Tool, res.StructuredContent),
	}, nil
}

func needsApproval(tool string) bool {
	return !autoApprove[tool]
}

func RequiresApproval(tool string) bool {
	return needsApproval(tool)
}

func PlanTurn(input, model string) (TurnPlan, error) {
	return NewPlanner(model).Plan(input)
}

func ExecutePlan(ctx context.Context, client *mcp.Client, plan TurnPlan) (*TurnExecution, error) {
	if len(plan.Steps) == 0 {
		return &TurnExecution{}, nil
	}

	executions := make([]*ToolExecution, 0, len(plan.Steps))
	seen := map[string]bool{}
	allCitations := make([]string, 0)
	for _, step := range plan.Steps {
		execRes, err := ExecuteParsed(ctx, client, ParsedInput{Tool: step.Tool, Args: step.Args})
		if err != nil {
			return nil, err
		}
		executions = append(executions, execRes)
		for _, c := range execRes.Citations {
			if seen[c] {
				continue
			}
			seen[c] = true
			allCitations = append(allCitations, c)
		}
	}
	sort.Strings(allCitations)

	return &TurnExecution{
		Executions: executions,
		Output:     synthesizeTurnOutput(plan, executions),
		Citations:  allCitations,
	}, nil
}

func synthesizeTurnOutput(plan TurnPlan, executions []*ToolExecution) string {
	if len(executions) == 0 {
		return ""
	}
	last := executions[len(executions)-1]
	if len(executions) == 1 {
		return last.Output
	}

	if plan.Synthesis == "analytical" {
		tools := make([]string, 0, len(executions))
		for _, ex := range executions {
			tools = append(tools, ex.Tool)
		}
		return fmt.Sprintf("Planner path: %s\n\n%s", strings.Join(tools, " -> "), last.Output)
	}

	if plan.Synthesis == "concise" {
		return last.Output
	}

	return fmt.Sprintf("Used %d tools before final answer.\n\n%s", len(executions), last.Output)
}

func citationsFor(tool string, sc map[string]any) []string {
	seen := map[string]bool{}
	list := []string{}
	add := func(c string) {
		if c == "" || seen[c] {
			return
		}
		seen[c] = true
		list = append(list, c)
	}

	switch tool {
	case protocol.ToolNameSearch:
		hits, _ := sc["hits"].([]any)
		for _, h := range hits {
			m, ok := h.(map[string]any)
			if !ok {
				continue
			}
			path := asString(m["rel_path"])
			span, _ := m["span"].(map[string]any)
			add(mcp.CitationForSpan(path, span))
		}
	case protocol.ToolNameOpenFile, protocol.ToolNameTranscribe, protocol.ToolNameAnnotate:
		path := asString(sc["rel_path"])
		span, _ := sc["span"].(map[string]any)
		add(mcp.CitationForSpan(path, span))
		if segments, ok := sc["segments"].([]any); ok {
			for _, s := range segments {
				if seg, ok := s.(map[string]any); ok {
					if segmentSpan, ok := seg["span"].(map[string]any); ok {
						add(mcp.CitationForSpan(path, segmentSpan))
					}
				}
			}
		}
	case protocol.ToolNameAsk, protocol.ToolNameTranscribeAndAsk:
		citations, _ := sc["citations"].([]any)
		for _, it := range citations {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			path := asString(m["rel_path"])
			span, _ := m["span"].(map[string]any)
			add(mcp.CitationForSpan(path, span))
		}
	}
	sort.Strings(list)
	return list
}

func connectedBanner(url, transport, session, model, startupHint string) []string {
	msgs := []string{
		ui.Info("Connected to", url),
		ui.Info("Transport:", transport),
		ui.Info("Model:", model),
	}
	if strings.TrimSpace(session) != "" {
		msgs = append(msgs, ui.Info("Session:", session))
	}
	if strings.TrimSpace(startupHint) != "" {
		msgs = append(msgs, ui.Yellow.Render("Warning: "+startupHint))
	}
	msgs = append(msgs, ui.Dim("Type /help for commands, /quit to exit."))
	return msgs
}
