package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/protocol"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
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

// TestSDKTransport_RecoversPanic drives the *production* SDK transport closure
// (buildSDKServer's AddTool handler) end-to-end: the go-sdk runs tool handlers in
// a background goroutine with no recover of its own, so a panic that escapes the
// closure crashes the whole daemon. Before the fix the closure called
// td.handler directly, bypassing invokeToolHandler's recover guard on the default
// (x402-off) path. This asserts a panicking handler now returns a clean isError
// tool result instead of taking down the process. Regression for issue #401.
func TestSDKTransport_RecoversPanic(t *testing.T) {
	ctx := context.Background()

	s := &Server{
		cfg: config.Config{ServerName: "test"},
		tools: map[string]toolDefinition{
			protocol.ToolNameSearch: {
				Name:         protocol.ToolNameSearch,
				Description:  "panicking tool for test",
				InputSchema:  map[string]interface{}{"type": "object"},
				OutputSchema: map[string]interface{}{"type": "object"},
				handler: func(context.Context, map[string]interface{}) (toolCallResult, *toolExecutionError) {
					panic("synthetic handler panic via SDK transport")
				},
			},
		},
	}

	sdkServer := s.buildSDKServer()

	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := sdkServer.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()

	// The call must return a normal (in-band) error result, not a transport
	// error or a crashed daemon.
	res, err := clientSession.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      protocol.ToolNameSearch,
		Arguments: map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("CallTool returned a transport error (handler panic not recovered): %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected an isError tool result after a handler panic, got %+v", res)
	}

	var text string
	for _, c := range res.Content {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			text = tc.Text
			break
		}
	}
	if want := "INTERNAL_ERROR"; !strings.Contains(text, want) {
		t.Fatalf("expected error result to mention %q, got %q", want, text)
	}
}
