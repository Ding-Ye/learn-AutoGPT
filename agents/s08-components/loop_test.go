package main

import (
	"context"
	"strings"
	"testing"
)

// busFromTools wraps a slice of tools into a single CommandProvider
// component and constructs a ComponentBus around it. Most loop tests
// only care that "these tools exist"; this helper hides the wrapping
// so test bodies stay focused on protocol behavior.
func busFromTools(tools ...Tool) *ComponentBus {
	return NewComponentBus(&commandsOnlyComponent{tools: tools})
}

// findToolResultContent walks messages looking for a tool_result block
// and returns the first non-empty stringified content.
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

// TestLoop_TerminatesOnEndTurn — the simplest run: end_turn on the
// first call, no tools.
func TestLoop_TerminatesOnEndTurn(t *testing.T) {
	p := NewMockProvider(&CreateMessageResponse{
		StopReason: "end_turn",
		Content:    []ContentBlock{{Type: "text", Text: "hi"}},
	})
	loop := &Loop{Provider: p, Components: busFromTools(NewEchoTool()), MaxTurns: 5}
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
	if loop.History == nil {
		t.Error("loop.History should be auto-initialized at Run, got nil")
	} else if len(*loop.History) != 0 {
		t.Errorf("history len = %d after end_turn-only run, want 0", len(*loop.History))
	}
}

// TestLoop_DispatchesToolUseAndFeedsResult — turn 0 emits tool_use,
// turn 1 ends. Verify tool_result appears in the second request.
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
	loop := &Loop{Provider: p, Components: busFromTools(NewEchoTool()), MaxTurns: 5}

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

	content, ok := findToolResultContent(p.Requests[1].Messages)
	if !ok {
		t.Fatalf("second request has no tool_result block: %+v", p.Requests[1].Messages)
	}
	if content != "hi" {
		t.Errorf("tool_result content = %q, want %q", content, "hi")
	}
}

// TestLoop_GracefulOnUnknownTool — model fabricated a tool name.
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
	loop := &Loop{Provider: p, Components: busFromTools(NewEchoTool()), MaxTurns: 5}

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

// TestLoop_FailsOnMaxTurns — provider never stops emitting tool_use.
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
	loop := &Loop{Provider: p, Components: busFromTools(NewEchoTool()), MaxTurns: max}

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
	if got := len(*loop.History); got != max {
		t.Errorf("history len = %d, want %d (one Episode per tool_use turn)", got, max)
	}
}

// TestLoop_RegistryToolsFlowToProviderRequest — bus-derived schemas
// reach the model in component order.
func TestLoop_RegistryToolsFlowToProviderRequest(t *testing.T) {
	p := NewMockProvider(&CreateMessageResponse{
		StopReason: "end_turn",
		Content:    []ContentBlock{{Type: "text", Text: "ok"}},
	})
	bus := busFromTools(NewEchoTool(), NewMathTool())
	loop := &Loop{Provider: p, Components: bus, MaxTurns: 1}

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
		t.Errorf("schemas = [%s, %s], want [echo, math]", tools[0].Name, tools[1].Name)
	}
}

// stubStrategy from prior sessions — adapted for s08's BuildPrompt
// signature with the new directives parameter.
type stubStrategy struct {
	buildCalls     int
	parseCalls     int
	lastTask       string
	lastTools      []ToolSchema
	lastDirectives []string
	lastHistory    History
}

func (s *stubStrategy) BuildPrompt(history []*Episode, tools []ToolSchema, directives []string, task string) []Message {
	s.buildCalls++
	s.lastTask = task
	s.lastTools = tools
	s.lastDirectives = directives
	s.lastHistory = History(history)
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

// TestLoop_InvokesStrategy — verify Loop wires the strategy through.
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
		Provider:   p,
		Components: busFromTools(NewEchoTool(), NewMathTool()),
		Strategy:   stub,
		MaxTurns:   5,
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

	if len(p.Requests) < 1 {
		t.Fatal("expected at least 1 provider call")
	}
	first := p.Requests[0]
	if len(first.Messages) == 0 || len(first.Messages[0].Content) == 0 {
		t.Fatalf("first request has no messages: %+v", first)
	}
	if !strings.HasPrefix(first.Messages[0].Content[0].Text, "STUB:") {
		t.Errorf("first request user text = %q, want STUB: prefix", first.Messages[0].Content[0].Text)
	}
}

// TestLoop_HistoryGrowsAfterEachTurn — N tool_use turns → N Episodes.
func TestLoop_HistoryGrowsAfterEachTurn(t *testing.T) {
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
	loop := &Loop{Provider: p, Components: busFromTools(NewEchoTool()), MaxTurns: 10}

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
	if len(p.Requests) != 4 {
		t.Errorf("provider calls = %d, want 4", len(p.Requests))
	}
	content, ok := findToolResultContent(p.Requests[2].Messages)
	if !ok {
		t.Fatalf("third request had no tool_result blocks: %+v", p.Requests[2].Messages)
	}
	if content != "one" {
		t.Errorf("third request first tool_result content = %q, want %q", content, "one")
	}
}

// ──────────────────────────────────────────────────────────────────────
// s07-inherited tests — permission gate (now exercised through bus)
// ──────────────────────────────────────────────────────────────────────

// TestLoop_PermissionDenyBlocksExecute — when Permissions returns Deny,
// Loop must NOT call Execute.
func TestLoop_PermissionDenyBlocksExecute(t *testing.T) {
	rec := &recordingTool{}
	bus := busFromTools(rec)

	perms := NewPermissions()
	perms.AddDeny("danger: **")

	p := NewMockProvider(
		&CreateMessageResponse{
			StopReason: "tool_use",
			Content: []ContentBlock{
				{Type: "tool_use", ID: "t1", Name: "danger", Input: map[string]interface{}{"path": "secrets.txt"}},
			},
		},
		&CreateMessageResponse{
			StopReason: "end_turn",
			Content:    []ContentBlock{{Type: "text", Text: "ok"}},
		},
	)
	loop := &Loop{
		Provider:    p,
		Components:  bus,
		Permissions: perms,
		MaxTurns:    5,
	}

	if _, err := loop.Run(context.Background(), "do dangerous thing"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if rec.calls != 0 {
		t.Errorf("Execute was called %d times despite Deny; want 0", rec.calls)
	}
	if loop.History == nil || len(*loop.History) != 1 {
		t.Fatalf("history len = %d, want 1", len(*loop.History))
	}
	res := (*loop.History)[0].Results
	if len(res) != 1 {
		t.Fatalf("results len = %d, want 1", len(res))
	}
	if res[0].Status != "denied" {
		t.Errorf("result status = %q, want \"denied\"", res[0].Status)
	}
	if !strings.Contains(res[0].Output, "permission denied") {
		t.Errorf("result output = %q, want contains \"permission denied\"", res[0].Output)
	}
}

// TestLoop_PermissionAskDelegatesToAsker — Asker is consulted on Ask
// decisions; Allow → Execute, Deny → skip.
func TestLoop_PermissionAskDelegatesToAsker(t *testing.T) {
	t.Run("Asker says Allow → Execute runs", func(t *testing.T) {
		rec := &recordingTool{}
		bus := busFromTools(rec)
		perms := NewPermissions()
		asker := NewStubAsker(Allow)

		p := NewMockProvider(
			&CreateMessageResponse{
				StopReason: "tool_use",
				Content: []ContentBlock{
					{Type: "tool_use", ID: "t1", Name: "danger",
						Input: map[string]interface{}{"path": "ok.md"}},
				},
			},
			&CreateMessageResponse{
				StopReason: "end_turn",
				Content:    []ContentBlock{{Type: "text", Text: "ok"}},
			},
		)
		loop := &Loop{Provider: p, Components: bus, Permissions: perms, Asker: asker, MaxTurns: 5}
		if _, err := loop.Run(context.Background(), "do thing"); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if rec.calls != 1 {
			t.Errorf("Execute calls = %d, want 1 after Asker→Allow", rec.calls)
		}
		if len(asker.Calls) != 1 {
			t.Errorf("Asker.Calls = %d, want 1", len(asker.Calls))
		}
	})

	t.Run("Asker says Deny → Execute skipped", func(t *testing.T) {
		rec := &recordingTool{}
		bus := busFromTools(rec)
		perms := NewPermissions()
		asker := NewStubAsker(Deny)

		p := NewMockProvider(
			&CreateMessageResponse{
				StopReason: "tool_use",
				Content: []ContentBlock{
					{Type: "tool_use", ID: "t1", Name: "danger",
						Input: map[string]interface{}{"path": "ok.md"}},
				},
			},
			&CreateMessageResponse{
				StopReason: "end_turn",
				Content:    []ContentBlock{{Type: "text", Text: "ok"}},
			},
		)
		loop := &Loop{Provider: p, Components: bus, Permissions: perms, Asker: asker, MaxTurns: 5}
		if _, err := loop.Run(context.Background(), "do thing"); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if rec.calls != 0 {
			t.Errorf("Execute calls = %d, want 0 after Asker→Deny", rec.calls)
		}
	})
}

// recordingTool — helper from s07.
type recordingTool struct {
	calls int
}

func (r *recordingTool) Schema() ToolSchema {
	return ToolSchema{
		Name:        "danger",
		Description: "Test-only tool used for permission gate tests; never actually does anything.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}},
		},
	}
}

func (r *recordingTool) Execute(ctx context.Context, input map[string]interface{}) (string, error) {
	r.calls++
	return "executed", nil
}

// ──────────────────────────────────────────────────────────────────────
// s08 NEW tests — directives flow through the strategy
// ──────────────────────────────────────────────────────────────────────

// TestLoop_DirectivesFlowFromComponentsToStrategy — a component
// implementing DirectiveProvider contributes a line; the Loop fetches
// it from the bus and passes it to BuildPrompt; the stub strategy
// captures the slice and asserts the exact contents reached it.
func TestLoop_DirectivesFlowFromComponentsToStrategy(t *testing.T) {
	bus := NewComponentBus(
		&commandsOnlyComponent{tools: []Tool{NewEchoTool()}},
		&directivesOnlyComponent{lines: []string{"directive A", "directive B"}},
	)
	stub := &stubStrategy{}
	p := NewMockProvider(&CreateMessageResponse{
		StopReason: "end_turn",
		Content:    []ContentBlock{{Type: "text", Text: "ok"}},
	})
	loop := &Loop{Provider: p, Components: bus, Strategy: stub, MaxTurns: 1}

	if _, err := loop.Run(context.Background(), "noop"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(stub.lastDirectives) != 2 {
		t.Fatalf("strategy received %d directives, want 2", len(stub.lastDirectives))
	}
	if stub.lastDirectives[0] != "directive A" || stub.lastDirectives[1] != "directive B" {
		t.Errorf("directives = %v, want [\"directive A\", \"directive B\"]", stub.lastDirectives)
	}
}

// TestLoop_DirectivesAppearInSystemPrompt — end-to-end: a component's
// directive must show up in the OneShotStrategy's BuildSystem output,
// which is what the Provider receives as System.
func TestLoop_DirectivesAppearInSystemPrompt(t *testing.T) {
	bus := NewComponentBus(
		&commandsOnlyComponent{tools: []Tool{NewEchoTool()}},
		&directivesOnlyComponent{lines: []string{"VERY-DISTINCT-DIRECTIVE-MARKER"}},
	)
	p := NewMockProvider(&CreateMessageResponse{
		StopReason: "end_turn",
		Content:    []ContentBlock{{Type: "text", Text: "ok"}},
	})
	loop := &Loop{Provider: p, Components: bus, MaxTurns: 1}

	if _, err := loop.Run(context.Background(), "noop"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.Requests) != 1 {
		t.Fatalf("expected 1 provider call, got %d", len(p.Requests))
	}
	if !strings.Contains(p.Requests[0].System, "VERY-DISTINCT-DIRECTIVE-MARKER") {
		t.Errorf("system prompt does not contain directive; got:\n%s", p.Requests[0].System)
	}
	if !strings.Contains(p.Requests[0].System, "## Directives") {
		t.Errorf("system prompt missing '## Directives' header; got:\n%s", p.Requests[0].System)
	}
}
