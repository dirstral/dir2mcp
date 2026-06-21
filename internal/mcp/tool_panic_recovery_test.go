package mcp

import (
	"context"
	"testing"
)

// TestInvokeToolHandler_RecoversPanic verifies that a panicking tool handler is
// converted into a clean INTERNAL_ERROR tool result rather than propagating the
// panic out through net/http (which would drop the client connection and surface
// as an opaque "Failed to call tool"). Regression for issue #356.
func TestInvokeToolHandler_RecoversPanic(t *testing.T) {
	s := &Server{}
	tool := toolDefinition{
		Name: "boom",
		handler: func(context.Context, map[string]interface{}) (toolCallResult, *toolExecutionError) {
			panic("synthetic handler panic")
		},
	}

	var (
		result  toolCallResult
		toolErr *toolExecutionError
	)
	// The call itself must not panic.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("invokeToolHandler propagated a panic instead of recovering: %v", r)
			}
		}()
		result, toolErr = s.invokeToolHandler(context.Background(), tool, tool.Name, nil)
	}()

	if toolErr == nil {
		t.Fatal("expected a tool execution error after a handler panic, got nil")
	}
	if toolErr.Code != "INTERNAL_ERROR" {
		t.Fatalf("expected INTERNAL_ERROR code, got %q", toolErr.Code)
	}
	if toolErr.Retryable {
		t.Fatal("a handler panic should be reported as non-retryable")
	}
	if len(result.Content) != 0 || result.StructuredContent != nil {
		t.Fatalf("expected an empty result on panic, got %+v", result)
	}
}

// TestInvokeToolHandler_PassThrough verifies the wrapper is transparent for a
// normal (non-panicking) handler: its result and nil error are returned
// unchanged.
func TestInvokeToolHandler_PassThrough(t *testing.T) {
	s := &Server{}
	want := toolCallResult{Content: []toolContentItem{{Type: "text", Text: "ok"}}}
	tool := toolDefinition{
		Name: "fine",
		handler: func(context.Context, map[string]interface{}) (toolCallResult, *toolExecutionError) {
			return want, nil
		},
	}

	result, toolErr := s.invokeToolHandler(context.Background(), tool, tool.Name, nil)
	if toolErr != nil {
		t.Fatalf("unexpected tool error: %+v", toolErr)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "ok" {
		t.Fatalf("result not passed through unchanged: %+v", result)
	}
}
