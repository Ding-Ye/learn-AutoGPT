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

// findToolResult walks the messages looking for a tool_result block
// whose ToolUseID matches the given suffix-condition (s05 synthesizes
// IDs like "ep<msgIdx>_act<i>", so the original toolUseID the model
// emitted is no longer carried verbatim into the next request — the
// strategy reconstructs the conversation from history rather than
// echoing the original blocks).
//
// The s05 test contract: the tool_result content (the string the tool
// produced or the error text) MUST be observable in the next
// provider request, regardless of which exact id we synthesize for it.
func findToolResultContent(messages []Message) (string, bool) {
	for _, m := range messages {
		for _, b := range m.Content {
			if b.Type == "tool_result" {
				if s, ok := b.ToolContent.(string); ok {
					return s, true
				}
			}
		}
	}
	return "", false
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
	// History should still be empty — pure end_turn doesn't create an
	// Episode.
	if loop.History == nil {
		t.Error("loop.History should be auto-initialized at Run, got nil")
	} else if len(*loop.History) != 0 {
		t.Errorf("history len = %d after end_turn-only run, want 0", len(*loop.History))
	}
}

// TestLoop_DispatchesToolUseAndFeedsResult — the protocol's whole point.
// Turn 0: provider emits tool_use(echo). Turn 1: provider emits end_turn.
// Assert that the SECOND request to the provider carries a tool_result
// somewhere in its messages — it now lives in a user message rendered
// by history.RenderMessages, not the literal last message (s05 appends
// the task as the last user message).
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

	// The second request should have a tool_result somewhere in its
	// messages with content "hi" (the EchoTool's output).
	content, ok := findToolResultContent(p.Requests[1].Messages)
	if !ok {
		t.Fatalf("second request has no tool_result block: %+v", p.Requests[1].Messages)
	}
	if content != "hi" {
		t.Errorf("tool_result content = %q, want %q", content, "hi")
	}
}

// TestLoop_GracefulOnUnknownTool — the model fabricated a tool name. Loop
// must not crash; it must feed an "unknown tool" error result back so the
// model can recover. We send a third end_turn so the loop can terminate.
//
// In s05 the unknown-tool error lands in the Episode's Results as
// {Status:"error", Output:"unknown tool: ..."}, and RenderMessages
// turns it into a tool_result with body "tool error: unknown tool: ...".
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
	content, ok := findToolResultContent(p.Requests[1].Messages)
	if !ok {
		t.Fatalf("no tool_result in second request: %+v", p.Requests[1].Messages)
	}
	if !strings.Contains(content, "unknown tool") {
		t.Errorf("tool_result content %q must contain 'unknown tool'", content)
	}
}

// TestLoop_FailsOnMaxTurns — provider never stops emitting tool_use. After
// MaxTurns we must surface an error mentioning MaxTurns so the operator
// knows what hit them rather than waiting forever.
func TestLoop_FailsOnMaxTurns(t *testing.T) {
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
	// And every turn appended an Episode — so history len == max.
	if got := len(*loop.History); got != max {
		t.Errorf("history len = %d, want %d (one Episode per tool_use turn)", got, max)
	}
}

// TestLoop_RegistryToolsFlowToProviderRequest — Registry's schemas
// reach the model. Build a Loop with Registry of Echo + Math, run a
// single end_turn pass, assert the provider saw both schemas in
// registration order.
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

// stubStrategy records every call so the test can assert the Loop wired
// the strategy through. BuildPrompt always returns a sentinel user msg
// so we can spot it in p.Requests; ParseResponse is a pass-through that
// counts invocations.
type stubStrategy struct {
	buildCalls   int
	parseCalls   int
	lastTask     string
	lastTools    []ToolSchema
	lastHistory  History
}

func (s *stubStrategy) BuildPrompt(history []*Episode, tools []ToolSchema, task string) []Message {
	s.buildCalls++
	s.lastTask = task
	s.lastTools = tools
	s.lastHistory = History(history) // capture for assertions
	return []Message{{
		Role:    "user",
		Content: []ContentBlock{{Type: "text", Text: "STUB:" + task}},
	}}
}

func (s *stubStrategy) ParseResponse(content []ContentBlock) (ActionProposal, error) {
	s.parseCalls++
	for _, b := range content {
		if b.Type == "tool_use" {
			return ActionProposal{Command: b.Name, Args: b.Input}, nil
		}
	}
	return ActionProposal{}, nil
}

// TestLoop_InvokesStrategy — verify that when a Strategy is plugged in,
// the Loop:
//
//  1. calls BuildPrompt at least once (in s05 it's called per turn);
//  2. forwards the strategy's []Message into the Provider request
//     verbatim (we look for the "STUB:" sentinel);
//  3. calls ParseResponse on each tool_use turn (here: one).
func TestLoop_InvokesStrategy(t *testing.T) {
	const toolUseID = "toolu_strategy_01"
	p := NewMockProvider(
		&CreateMessageResponse{
			StopReason: "tool_use",
			Content: []ContentBlock{
				{Type: "tool_use", ID: toolUseID, Name: "echo", Input: map[string]interface{}{"message": "hello"}},
			},
		},
		&CreateMessageResponse{
			StopReason: "end_turn",
			Content:    []ContentBlock{{Type: "text", Text: "ok"}},
		},
	)
	stub := &stubStrategy{}
	loop := &Loop{
		Provider: p,
		Tools:    newTestRegistry(t, NewEchoTool(), NewMathTool()),
		Strategy: stub,
		MaxTurns: 5,
	}
	if _, err := loop.Run(context.Background(), "do thing"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if stub.buildCalls < 1 {
		t.Errorf("BuildPrompt invocations = %d, want >= 1", stub.buildCalls)
	}
	if stub.lastTask != "do thing" {
		t.Errorf("BuildPrompt task = %q, want %q", stub.lastTask, "do thing")
	}
	if len(stub.lastTools) != 2 {
		t.Errorf("BuildPrompt tools count = %d, want 2", len(stub.lastTools))
	}
	if stub.parseCalls < 1 {
		t.Errorf("ParseResponse should be called on at least one tool_use turn, got %d", stub.parseCalls)
	}

	// The provider's first request must include the stub's sentinel.
	if len(p.Requests) < 1 {
		t.Fatal("expected at least 1 provider call")
	}
	first := p.Requests[0]
	if len(first.Messages) == 0 || len(first.Messages[0].Content) == 0 {
		t.Fatalf("first request has no messages: %+v", first)
	}
	if !strings.HasPrefix(first.Messages[0].Content[0].Text, "STUB:") {
		t.Errorf("first request user text = %q, want STUB: prefix (proves strategy.BuildPrompt drove construction)",
			first.Messages[0].Content[0].Text)
	}
}

// TestLoop_HistoryGrowsAfterEachTurn — the s05 contract. After N
// tool_use turns the History contains N Episodes, each with one
// Action and one matching Result. This is the test that proves the
// Loop's per-turn `Append + record proposal + record result` dance is
// observable from the outside.
func TestLoop_HistoryGrowsAfterEachTurn(t *testing.T) {
	// Three tool_use turns followed by an end_turn — so we expect 3
	// Episodes recorded.
	p := NewMockProvider(
		&CreateMessageResponse{
			StopReason: "tool_use",
			Content: []ContentBlock{
				{Type: "tool_use", ID: "t1", Name: "echo", Input: map[string]interface{}{"message": "one"}},
			},
		},
		&CreateMessageResponse{
			StopReason: "tool_use",
			Content: []ContentBlock{
				{Type: "tool_use", ID: "t2", Name: "echo", Input: map[string]interface{}{"message": "two"}},
			},
		},
		&CreateMessageResponse{
			StopReason: "tool_use",
			Content: []ContentBlock{
				{Type: "tool_use", ID: "t3", Name: "echo", Input: map[string]interface{}{"message": "three"}},
			},
		},
		&CreateMessageResponse{
			StopReason: "end_turn",
			Content:    []ContentBlock{{Type: "text", Text: "done"}},
		},
	)
	loop := &Loop{Provider: p, Tools: newTestRegistry(t, NewEchoTool()), MaxTurns: 10}

	if _, err := loop.Run(context.Background(), "ask"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if loop.History == nil {
		t.Fatal("loop.History was not initialized")
	}
	if got := len(*loop.History); got != 3 {
		t.Fatalf("history len = %d, want 3 (one Episode per tool_use turn)", got)
	}
	for i, ep := range *loop.History {
		if len(ep.Actions) != 1 {
			t.Errorf("episode %d: actions len = %d, want 1", i, len(ep.Actions))
		}
		if len(ep.Results) != 1 {
			t.Errorf("episode %d: results len = %d, want 1", i, len(ep.Results))
		}
		if ep.Results[0].Status != "ok" {
			t.Errorf("episode %d: result status = %q, want \"ok\"", i, ep.Results[0].Status)
		}
	}
	// Also: after 3 tool turns + 1 end_turn, the provider was called 4 times.
	if len(p.Requests) != 4 {
		t.Errorf("provider calls = %d, want 4", len(p.Requests))
	}
	// And by turn 2 (the third tool turn), the request's messages MUST
	// include rendered prior episodes — easily checked by looking for
	// the first turn's tool_result content "one" in the third request.
	content, ok := findToolResultContent(p.Requests[2].Messages)
	if !ok {
		t.Fatalf("third request had no tool_result blocks: %+v", p.Requests[2].Messages)
	}
	// The first tool_result in the rendered history should be "one"
	// (chronological order).
	if content != "one" {
		t.Errorf("third request first tool_result content = %q, want %q (proves history renders in chronological order)", content, "one")
	}
}
