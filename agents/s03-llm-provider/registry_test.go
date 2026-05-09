package main

import (
	"context"
	"strings"
	"testing"
)

// TestRegistry_RegisterThenLookup — happy path. Register one tool,
// look it up by name, get it back. This is the contract Loop relies on.
func TestRegistry_RegisterThenLookup(t *testing.T) {
	r := NewRegistry()
	echo := NewEchoTool()
	if err := r.Register(echo); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, ok := r.Lookup("echo")
	if !ok {
		t.Fatal("Lookup(echo) returned ok=false, want true")
	}
	// Pointer identity matters — the loop will Execute on the actual
	// instance we registered, not a copy.
	if got != echo {
		t.Errorf("Lookup returned different instance: got %p, want %p", got, echo)
	}
}

// TestRegistry_LookupMissingReturnsFalse — the second-return contract.
// Lookup never errors; missing is a routine condition the caller must
// distinguish from "found".
func TestRegistry_LookupMissingReturnsFalse(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(NewEchoTool()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, ok := r.Lookup("nonexistent")
	if ok {
		t.Fatalf("Lookup(nonexistent) returned ok=true, want false (got %v)", got)
	}
	if got != nil {
		t.Errorf("Lookup(missing) tool = %v, want nil", got)
	}
}

// TestRegistry_AllReturnsInsertionOrder — schemas come out in the order
// Register was called. A plain map would yield random order; we keep an
// explicit `names` slice so prompts that list tools to the model (and
// the golden tests that pin them) are reproducible.
func TestRegistry_AllReturnsInsertionOrder(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(NewMathTool()); err != nil {
		t.Fatalf("Register math: %v", err)
	}
	if err := r.Register(NewEchoTool()); err != nil {
		t.Fatalf("Register echo: %v", err)
	}
	all := r.All()
	if len(all) != 2 {
		t.Fatalf("All() len = %d, want 2", len(all))
	}
	if all[0].Name != "math" || all[1].Name != "echo" {
		t.Errorf("All() order = [%s, %s], want [math, echo]", all[0].Name, all[1].Name)
	}
}

// TestRegistry_DoubleRegisterErrors — the same name twice surfaces a
// clear error rather than silently overwriting. Silent overwrites
// produce the worst kind of bug ("I changed it but the old one still
// runs"); make the failure visible.
func TestRegistry_DoubleRegisterErrors(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(NewEchoTool()); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := r.Register(NewEchoTool())
	if err == nil {
		t.Fatal("second Register of same name returned nil, want error")
	}
	if !strings.Contains(err.Error(), "echo") || !strings.Contains(err.Error(), "already registered") {
		t.Errorf("error %q should mention 'echo' and 'already registered'", err.Error())
	}
}

// TestRegistry_LoopDispatchesViaRegistry — the integration test. Build a
// Loop with a Registry, send a tool_use through the mock provider, and
// assert that the resulting tool_result content matches what the
// registered MathTool actually computes. This is the single test that
// proves Loop and Registry are correctly wired.
func TestRegistry_LoopDispatchesViaRegistry(t *testing.T) {
	const toolUseID = "toolu_math_01"
	reg := NewRegistry()
	if err := reg.Register(NewEchoTool()); err != nil {
		t.Fatalf("Register echo: %v", err)
	}
	if err := reg.Register(NewMathTool()); err != nil {
		t.Fatalf("Register math: %v", err)
	}
	p := NewMockProvider(
		&CreateMessageResponse{
			StopReason: "tool_use",
			Content: []ContentBlock{{
				Type: "tool_use", ID: toolUseID, Name: "math",
				Input: map[string]interface{}{"operation": "add", "a": 2.0, "b": 3.0},
			}},
		},
		&CreateMessageResponse{
			StopReason: "end_turn",
			Content:    []ContentBlock{{Type: "text", Text: "5"}},
		},
	)
	loop := &Loop{Provider: p, Tools: reg, MaxTurns: 5}

	final, err := loop.Run(context.Background(), "compute 2+3")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final != "5" {
		t.Errorf("final = %q, want %q", final, "5")
	}
	if len(p.Requests) != 2 {
		t.Fatalf("expected 2 provider calls, got %d", len(p.Requests))
	}
	// Second request must contain the tool_result whose content is "5".
	last := p.Requests[1].Messages[len(p.Requests[1].Messages)-1]
	var foundContent string
	for _, b := range last.Content {
		if b.Type == "tool_result" && b.ToolUseID == toolUseID {
			if s, ok := b.ToolContent.(string); ok {
				foundContent = s
			}
		}
	}
	if foundContent != "5" {
		t.Errorf("tool_result content = %q, want %q (registry must dispatch math.add(2,3) = 5)", foundContent, "5")
	}
}
