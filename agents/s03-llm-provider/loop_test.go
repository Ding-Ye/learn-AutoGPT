package main

import (
	"context"
	"strings"
	"testing"
)

// newTestRegistry — small helper. Most loop tests need a Registry with
// Echo (and optionally Math) registered; centralizing that keeps the
// individual tests focused on the protocol behavior they're asserting.
func newTestRegistry(t *testing.T, tools ...Tool) *Registry {
	t.Helper()
	r := NewRegistry()
	for _, tool := range tools {
		if err := r.Register(tool); err != nil {
			t.Fatalf("Register %T: %v", tool, err)
		}
	}
	return r
}

// TestLoop_TerminatesOnEndTurn — the simplest possible run: provider says
// end_turn on the very first call, Loop returns the text content, no tools
// involved. This verifies the happy path of `extractText` + the end_turn
// branch of the switch.
func TestLoop_TerminatesOnEndTurn(t *testing.T) {
	p := NewMockProvider(&CreateMessageResponse{
		StopReason: "end_turn",
		Content:    []ContentBlock{{Type: "text", Text: "hi"}},
	})
	loop := &Loop{Provider: p, Tools: newTestRegistry(t, NewEchoTool()), MaxTurns: 5}
	got, err := loop.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hi" {
		t.Errorf("got %q, want %q", got, "hi")
	}
	if len(p.Requests) != 1 {
		t.Errorf("expected 1 provider call, got %d", len(p.Requests))
	}
}

// TestLoop_DispatchesToolUseAndFeedsResult — the protocol's whole point.
// Turn 0: provider emits tool_use(echo). Turn 1: provider emits end_turn.
// Assert the SECOND request to the provider carries a tool_result block
// referencing the original tool_use id. This is the "observe" half of the
// think→act→observe loop.
func TestLoop_DispatchesToolUseAndFeedsResult(t *testing.T) {
	const toolUseID = "toolu_test_01"
	p := NewMockProvider(
		&CreateMessageResponse{
			StopReason: "tool_use",
			Content: []ContentBlock{
				{Type: "tool_use", ID: toolUseID, Name: "echo", Input: map[string]interface{}{"message": "hi"}},
			},
		},
		&CreateMessageResponse{
			StopReason: "end_turn",
			Content:    []ContentBlock{{Type: "text", Text: "done"}},
		},
	)
	loop := &Loop{Provider: p, Tools: newTestRegistry(t, NewEchoTool()), MaxTurns: 5}

	got, err := loop.Run(context.Background(), "ask")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "done" {
		t.Errorf("final answer = %q, want %q", got, "done")
	}
	if len(p.Requests) != 2 {
		t.Fatalf("expected 2 provider calls, got %d", len(p.Requests))
	}

	// The second request's last message should be the user-role tool_result.
	second := p.Requests[1]
	last := second.Messages[len(second.Messages)-1]
	if last.Role != "user" {
		t.Errorf("last message role = %q, want %q (tool_result is sent as a user turn)", last.Role, "user")
	}
	var found bool
	for _, b := range last.Content {
		if b.Type == "tool_result" && b.ToolUseID == toolUseID {
			found = true
			if s, ok := b.ToolContent.(string); !ok || s != "hi" {
				t.Errorf("tool_result content = %v, want %q", b.ToolContent, "hi")
			}
		}
	}
	if !found {
		t.Errorf("second request has no tool_result with id %q: %+v", toolUseID, last.Content)
	}
}

// TestLoop_GracefulOnUnknownTool — the model fabricated a tool name. Loop
// must not crash; it must feed an "unknown tool" tool_result back so the
// model can recover. We send a third end_turn so the loop can terminate.
//
// Also exercises the s02 path: the unknown-tool branch goes through
// Registry.Lookup (returning ok=false) rather than the s01 map miss.
func TestLoop_GracefulOnUnknownTool(t *testing.T) {
	const toolUseID = "toolu_bogus_01"
	p := NewMockProvider(
		&CreateMessageResponse{
			StopReason: "tool_use",
			Content: []ContentBlock{
				{Type: "tool_use", ID: toolUseID, Name: "nonexistent_tool", Input: map[string]interface{}{"x": 1}},
			},
		},
		&CreateMessageResponse{
			StopReason: "end_turn",
			Content:    []ContentBlock{{Type: "text", Text: "sorry"}},
		},
	)
	loop := &Loop{Provider: p, Tools: newTestRegistry(t, NewEchoTool()), MaxTurns: 5}

	if _, err := loop.Run(context.Background(), "ask"); err != nil {
		t.Fatalf("loop should not error on unknown tool: %v", err)
	}
	if len(p.Requests) != 2 {
		t.Fatalf("expected 2 provider calls, got %d", len(p.Requests))
	}

	// The second request must carry a tool_result whose body contains "unknown tool".
	second := p.Requests[1]
	last := second.Messages[len(second.Messages)-1]
	var foundMsg string
	for _, b := range last.Content {
		if b.Type == "tool_result" && b.ToolUseID == toolUseID {
			if s, ok := b.ToolContent.(string); ok {
				foundMsg = s
			}
		}
	}
	if foundMsg == "" {
		t.Fatalf("no tool_result with id %q in second request: %+v", toolUseID, last.Content)
	}
	if !strings.Contains(foundMsg, "unknown tool") {
		t.Errorf("tool_result content %q must contain 'unknown tool'", foundMsg)
	}
}

// TestLoop_FailsOnMaxTurns — provider never stops emitting tool_use. After
// MaxTurns we must surface an error mentioning MaxTurns so the operator
// knows what hit them rather than waiting forever.
func TestLoop_FailsOnMaxTurns(t *testing.T) {
	// Provide more responses than MaxTurns so the mock never exhausts first.
	const max = 3
	resps := make([]*CreateMessageResponse, max+5)
	for i := range resps {
		resps[i] = &CreateMessageResponse{
			StopReason: "tool_use",
			Content: []ContentBlock{
				{Type: "tool_use", ID: "t", Name: "echo", Input: map[string]interface{}{"message": "loop"}},
			},
		}
	}
	p := NewMockProvider(resps...)
	loop := &Loop{Provider: p, Tools: newTestRegistry(t, NewEchoTool()), MaxTurns: max}

	_, err := loop.Run(context.Background(), "ask")
	if err == nil {
		t.Fatal("expected MaxTurns error, got nil")
	}
	if !strings.Contains(err.Error(), "MaxTurns") {
		t.Errorf("error %q should mention 'MaxTurns'", err.Error())
	}
	if len(p.Requests) != max {
		t.Errorf("expected %d provider calls (one per turn), got %d", max, len(p.Requests))
	}
}

// TestLoop_RegistryToolsFlowToProviderRequest — new in s02. Build a
// Registry with Echo + Math, run a single end_turn pass, and assert
// that the schemas observed by the Provider are the schemas the
// Registry produced — same names, same order. This is what protects
// against silent regressions where Loop forgets to forward the tool
// list to the model.
func TestLoop_RegistryToolsFlowToProviderRequest(t *testing.T) {
	p := NewMockProvider(&CreateMessageResponse{
		StopReason: "end_turn",
		Content:    []ContentBlock{{Type: "text", Text: "ok"}},
	})
	reg := newTestRegistry(t, NewEchoTool(), NewMathTool())
	loop := &Loop{Provider: p, Tools: reg, MaxTurns: 1}

	if _, err := loop.Run(context.Background(), "noop"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.Requests) != 1 {
		t.Fatalf("expected 1 provider call, got %d", len(p.Requests))
	}
	tools := p.Requests[0].Tools
	if len(tools) != 2 {
		t.Fatalf("provider received %d schemas, want 2", len(tools))
	}
	if tools[0].Name != "echo" || tools[1].Name != "math" {
		t.Errorf("schemas = [%s, %s], want [echo, math] (Registry.All must preserve insertion order)", tools[0].Name, tools[1].Name)
	}
}
