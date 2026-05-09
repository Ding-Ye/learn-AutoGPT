package main

import (
	"context"
	"strings"
	"testing"
)

// newTestRegistry — same as prior sessions: most loop tests need a
// Registry with Echo (and optionally Math) registered; centralizing
// keeps tests focused on protocol behavior.
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
	if loop.History == nil {
		t.Error("loop.History should be auto-initialized at Run, got nil")
	} else if len(*loop.History) != 0 {
		t.Errorf("history len = %d after end_turn-only run, want 0", len(*loop.History))
	}
}

// TestLoop_DispatchesToolUseAndFeedsResult — turn 0 emits tool_use,
// turn 1 ends. Verify tool_result appears in the second request.
// Permissions==nil here keeps s06 behavior.
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

	content, ok := findToolResultContent(p.Requests[1].Messages)
	if !ok {
		t.Fatalf("second request has no tool_result block: %+v", p.Requests[1].Messages)
	}
	if content != "hi" {
		t.Errorf("tool_result content = %q, want %q", content, "hi")
	}
}

// TestLoop_GracefulOnUnknownTool — model fabricated a tool name. Loop
// must not crash; it must feed an "unknown tool" error result back.
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

// TestLoop_FailsOnMaxTurns — provider never stops emitting tool_use.
// After MaxTurns we surface a clear error.
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
	if got := len(*loop.History); got != max {
		t.Errorf("history len = %d, want %d (one Episode per tool_use turn)", got, max)
	}
}

// TestLoop_RegistryToolsFlowToProviderRequest — Registry's schemas
// reach the model; insertion order is preserved.
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

// stubStrategy from prior sessions: BuildPrompt records calls; Parse
// converts the first tool_use into a proposal.
type stubStrategy struct {
	buildCalls  int
	parseCalls  int
	lastTask    string
	lastTools   []ToolSchema
	lastHistory History
}

func (s *stubStrategy) BuildPrompt(history []*Episode, tools []ToolSchema, task string) []Message {
	s.buildCalls++
	s.lastTask = task
	s.lastTools = tools
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

// TestLoop_HistoryGrowsAfterEachTurn — N tool_use turns → N Episodes,
// each with one Action and one Result.
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
// s07 NEW tests — permission gate
// ──────────────────────────────────────────────────────────────────────

// TestLoop_PermissionDenyBlocksExecute — when Permissions returns Deny
// for a tool_use, Loop must NOT call Execute. Instead it synthesizes a
// "permission denied" tool_result that the model sees on the next turn.
//
// We use a custom Tool that records every Execute; assert it was never
// called. We also verify the next provider request carries the
// "denied" payload in a tool_result.
func TestLoop_PermissionDenyBlocksExecute(t *testing.T) {
	rec := &recordingTool{}
	reg := newTestRegistry(t, rec)

	perms := NewPermissions()
	perms.AddDeny("danger: **") // every danger() call is denied

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
		Tools:       reg,
		Permissions: perms,
		MaxTurns:    5,
	}

	if _, err := loop.Run(context.Background(), "do dangerous thing"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if rec.calls != 0 {
		t.Errorf("Execute was called %d times despite Deny; want 0", rec.calls)
	}
	// The history should have one episode with one denied result.
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

	// And the model sees the denial on its next turn.
	if len(p.Requests) < 2 {
		t.Fatalf("expected >= 2 provider calls, got %d", len(p.Requests))
	}
	content, ok := findToolResultContent(p.Requests[1].Messages)
	if !ok {
		t.Fatal("second request has no tool_result block")
	}
	if !strings.Contains(content, "denied") {
		t.Errorf("tool_result on second request = %q, want contains \"denied\"", content)
	}
}

// TestLoop_PermissionAskDelegatesToAsker — when Permissions returns Ask,
// Loop consults the configured Asker. A StubAsker that returns Allow
// must result in Execute being called; a StubAsker that returns Deny
// must NOT.
func TestLoop_PermissionAskDelegatesToAsker(t *testing.T) {
	t.Run("Asker says Allow → Execute runs", func(t *testing.T) {
		rec := &recordingTool{}
		reg := newTestRegistry(t, rec)
		perms := NewPermissions() // empty → Check returns Ask for everything
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
		loop := &Loop{Provider: p, Tools: reg, Permissions: perms, Asker: asker, MaxTurns: 5}
		if _, err := loop.Run(context.Background(), "do thing"); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if rec.calls != 1 {
			t.Errorf("Execute calls = %d, want 1 after Asker→Allow", rec.calls)
		}
		if len(asker.Calls) != 1 {
			t.Errorf("Asker.Calls = %d, want 1", len(asker.Calls))
		}
		if asker.Calls[0].Cmd != "danger" {
			t.Errorf("Asker.Calls[0].Cmd = %q, want \"danger\"", asker.Calls[0].Cmd)
		}
	})

	t.Run("Asker says Deny → Execute skipped", func(t *testing.T) {
		rec := &recordingTool{}
		reg := newTestRegistry(t, rec)
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
		loop := &Loop{Provider: p, Tools: reg, Permissions: perms, Asker: asker, MaxTurns: 5}
		if _, err := loop.Run(context.Background(), "do thing"); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if rec.calls != 0 {
			t.Errorf("Execute calls = %d, want 0 after Asker→Deny", rec.calls)
		}
		if len(asker.Calls) != 1 {
			t.Errorf("Asker.Calls = %d, want 1", len(asker.Calls))
		}

		// And history records the denial with status="denied".
		if len(*loop.History) != 1 {
			t.Fatalf("history len = %d, want 1", len(*loop.History))
		}
		res := (*loop.History)[0].Results
		if len(res) != 1 || res[0].Status != "denied" {
			t.Errorf("result status = %q, want \"denied\"", res[0].Status)
		}
	})

	t.Run("Permissions.Ask with no Asker → Run errors", func(t *testing.T) {
		rec := &recordingTool{}
		reg := newTestRegistry(t, rec)
		perms := NewPermissions() // Ask for everything
		// no Asker

		p := NewMockProvider(
			&CreateMessageResponse{
				StopReason: "tool_use",
				Content: []ContentBlock{
					{Type: "tool_use", ID: "t1", Name: "danger",
						Input: map[string]interface{}{"path": "ok.md"}},
				},
			},
		)
		loop := &Loop{Provider: p, Tools: reg, Permissions: perms, MaxTurns: 5}
		_, err := loop.Run(context.Background(), "do thing")
		if err == nil {
			t.Fatal("expected error when Ask with no Asker, got nil")
		}
		if !strings.Contains(err.Error(), "no Asker") {
			t.Errorf("error = %q, want contains \"no Asker\"", err.Error())
		}
		if rec.calls != 0 {
			t.Errorf("Execute should not run when no Asker; got %d calls", rec.calls)
		}
	})
}

// recordingTool is a Tool that records how many times Execute was
// called. Used by the permission-gate tests above to verify the gate
// blocks dispatch (rather than letting the tool see args).
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
