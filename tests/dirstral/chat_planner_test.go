package test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"dir2mcp/internal/dirstral/chat"
	"dir2mcp/internal/protocol"
)

func TestPlannerModelProfileAffectsStrategyAndSynthesis(t *testing.T) {
	largePlan, err := chat.PlanTurn("explain indexing", "mistral-large-latest")
	if err != nil {
		t.Fatalf("large plan error: %v", err)
	}
	if len(largePlan.Steps) != 2 {
		t.Fatalf("expected large model to use two-step plan, got %d", len(largePlan.Steps))
	}
	if largePlan.Steps[0].Tool != protocol.ToolNameSearch || largePlan.Steps[1].Tool != protocol.ToolNameAsk {
		t.Fatalf("unexpected large model tool sequence: %#v", largePlan.Steps)
	}
	if largePlan.Synthesis != "analytical" {
		t.Fatalf("expected analytical synthesis for large model, got %q", largePlan.Synthesis)
	}

	smallPlan, err := chat.PlanTurn("explain indexing", "mistral-small-latest")
	if err != nil {
		t.Fatalf("small plan error: %v", err)
	}
	if len(smallPlan.Steps) != 1 {
		t.Fatalf("expected small model to use one-step plan, got %d", len(smallPlan.Steps))
	}
	if smallPlan.Steps[0].Tool != protocol.ToolNameAsk {
		t.Fatalf("expected small model to ask directly, got %q", smallPlan.Steps[0].Tool)
	}
	if smallPlan.Synthesis != "concise" {
		t.Fatalf("expected concise synthesis for small model, got %q", smallPlan.Synthesis)
	}
}

func TestPlannerSupportsClearCommand(t *testing.T) {
	plan, err := chat.PlanTurn("/clear", "mistral-small-latest")
	if err != nil {
		t.Fatalf("plan error: %v", err)
	}
	if !plan.Clear {
		t.Fatal("expected /clear to set Clear=true")
	}
	if len(plan.Steps) != 0 {
		t.Fatalf("expected no tool steps for /clear, got %d", len(plan.Steps))
	}
}

func TestParseInputMapsCommonCommands(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  chat.ParsedInput
	}{
		{name: "quit", input: "/quit", want: chat.ParsedInput{Quit: true}},
		{name: "help", input: "/help", want: chat.ParsedInput{Help: true}},
		{name: "clear", input: "/clear", want: chat.ParsedInput{Clear: true}},
		{name: "transcribe", input: "/transcribe file.mp3", want: chat.ParsedInput{Tool: protocol.ToolNameTranscribe, Args: map[string]any{"rel_path": "file.mp3"}}},
		{name: "transcribe_and_ask", input: "/transcribe_and_ask file.mp3 what?", want: chat.ParsedInput{Tool: protocol.ToolNameTranscribeAndAsk, Args: map[string]any{"rel_path": "file.mp3", "question": "what?", "k": 6}}}, // Assuming mistral-small-latest is used with k=6
		{name: "annotate", input: "/annotate file.txt {\"type\":\"object\"}", want: chat.ParsedInput{Tool: protocol.ToolNameAnnotate, Args: map[string]any{"rel_path": "file.txt", "schema_json": map[string]any{"type": "object"}}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := chat.ParseInput(tc.input, "mistral-small-latest")
			if got.Quit != tc.want.Quit || got.Help != tc.want.Help || got.Clear != tc.want.Clear || got.Tool != tc.want.Tool {
				t.Fatalf("unexpected parsed flags: got %+v want %+v", got, tc.want)
			}
			if tc.want.Tool != "" && got.Tool == tc.want.Tool {
				if !reflect.DeepEqual(got.Args, tc.want.Args) {
					t.Fatalf("unexpected args: got %+v want %+v", got.Args, tc.want.Args)
				}
			}
		})
	}
}

func TestParseInputOpenWithoutPathFallsBackToHelp(t *testing.T) {
	got := chat.ParseInput("/open", "mistral-small-latest")
	if !got.Help {
		t.Fatalf("expected /open without path to map to help, got %+v", got)
	}
}

func TestRequiresApprovalPolicy(t *testing.T) {
	if chat.RequiresApproval(protocol.ToolNameSearch) {
		t.Fatal("search should be auto-approved")
	}
	if !chat.RequiresApproval("dir2mcp.delete_everything") {
		t.Fatal("unknown tools should require approval")
	}
}

func TestExecutePlanEmptyReturnsNoOutput(t *testing.T) {
	res, err := chat.ExecutePlan(context.Background(), nil, chat.TurnPlan{})
	if err != nil {
		t.Fatalf("expected no error for empty plan, got %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil result for empty plan")
	}
	if strings.TrimSpace(res.Output) != "" {
		t.Fatalf("expected empty output for empty plan, got %q", res.Output)
	}
}
