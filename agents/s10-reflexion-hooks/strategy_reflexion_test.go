package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestReflexion_DelegatesBuildAndParseToBase — the wrapper does NOT
// alter primary prompt construction or response parsing. Both methods
// must hand straight through to the base strategy. We use a stub base
// that records calls so we can assert delegation without inspecting
// outputs.
func TestReflexion_DelegatesBuildAndParseToBase(t *testing.T) {
	base := &recordingStrategy{
		buildResult: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "BASE"}}}},
		parseResult: ActionProposal{Command: "echo", Args: map[string]interface{}{"x": 1}},
	}
	pipe := NewPipeline()
	provider := NewMockProvider() // no responses needed for this test
	r := NewReflexionStrategy(base, provider, pipe)

	msgs := r.BuildPrompt(nil, []ToolSchema{{Name: "echo"}}, []string{"d1"}, "task-text")
	if base.buildCalls != 1 {
		t.Errorf("base.BuildPrompt calls = %d, want 1", base.buildCalls)
	}
	if base.lastTask != "task-text" {
		t.Errorf("base saw task = %q, want %q", base.lastTask, "task-text")
	}
	if len(msgs) != 1 || msgs[0].Content[0].Text != "BASE" {
		t.Errorf("BuildPrompt did not return base output, got %+v", msgs)
	}

	prop, err := r.ParseResponse([]ContentBlock{{Type: "text", Text: "ignored"}})
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if base.parseCalls != 1 {
		t.Errorf("base.ParseResponse calls = %d, want 1", base.parseCalls)
	}
	if prop.Command != "echo" {
		t.Errorf("ParseResponse Command = %q, want echo (must come from base)", prop.Command)
	}
}

// TestReflexion_RegistersAfterParseHookOnConstruction — the constructor
// must wire up the AfterParseHook so the Loop sees it without any
// further setup. Verify by counting hooks before/after.
func TestReflexion_RegistersAfterParseHookOnConstruction(t *testing.T) {
	pipe := NewPipeline()
	if got := len(pipe.afterParse); got != 0 {
		t.Fatalf("pipeline starts with %d AfterParse hooks, want 0", got)
	}
	provider := NewMockProvider()
	_ = NewReflexionStrategy(NewOneShotStrategy(), provider, pipe)
	if got := len(pipe.afterParse); got != 1 {
		t.Errorf("after construction: %d AfterParse hooks, want 1", got)
	}
	if got := len(pipe.afterExecute); got != 0 {
		t.Errorf("after construction: %d AfterExecute hooks, want 0 (reflexion only registers AfterParse)", got)
	}
}

// TestReflexion_RevisesProposalWhenLLMSaysUnsound — the load-bearing
// behavior. MockProvider returns a {sound:false, revised:{...}} verdict;
// the hook must mutate the proposal in place.
func TestReflexion_RevisesProposalWhenLLMSaysUnsound(t *testing.T) {
	verdictJSON := `{"sound": false, "reason": "command typo", "revised": {"command": "read_file", "args": {"path": "notes.md"}, "thoughts": "user meant read"}}`
	provider := NewMockProvider(&CreateMessageResponse{
		StopReason: "end_turn",
		Content:    []ContentBlock{{Type: "text", Text: verdictJSON}},
	})
	pipe := NewPipeline()
	_ = NewReflexionStrategy(NewOneShotStrategy(), provider, pipe)

	prop := &ActionProposal{
		Command:  "delete_file",
		Args:     map[string]interface{}{"path": "notes.md"},
		Thoughts: "user said read",
	}
	if err := pipe.RunAfterParse(context.Background(), prop); err != nil {
		t.Fatalf("RunAfterParse: %v", err)
	}

	if prop.Command != "read_file" {
		t.Errorf("Command = %q, want %q (reflexion did not rewrite)", prop.Command, "read_file")
	}
	if prop.Args["path"] != "notes.md" {
		t.Errorf("Args[path] = %v, want %q", prop.Args["path"], "notes.md")
	}
	if prop.Thoughts != "user meant read" {
		t.Errorf("Thoughts = %q, want revision text", prop.Thoughts)
	}

	if len(provider.Requests) != 1 {
		t.Fatalf("provider calls = %d, want 1 (one second-pass call)", len(provider.Requests))
	}
	// The reflexion prompt must mention the original proposal so the
	// LLM has context to evaluate.
	first := provider.Requests[0]
	if len(first.Messages) != 1 {
		t.Fatalf("reflexion call had %d messages, want 1", len(first.Messages))
	}
	body := first.Messages[0].Content[0].Text
	if !strings.Contains(body, "delete_file") {
		t.Errorf("reflexion prompt missing original command 'delete_file', got:\n%s", body)
	}
}

// TestReflexion_PassesThroughWhenSound — symmetric counterpart: a
// sound=true verdict leaves the proposal untouched.
func TestReflexion_PassesThroughWhenSound(t *testing.T) {
	provider := NewMockProvider(&CreateMessageResponse{
		StopReason: "end_turn",
		Content:    []ContentBlock{{Type: "text", Text: `{"sound": true, "reason": "looks fine"}`}},
	})
	pipe := NewPipeline()
	_ = NewReflexionStrategy(NewOneShotStrategy(), provider, pipe)

	prop := &ActionProposal{
		Command:  "read_file",
		Args:     map[string]interface{}{"path": "ok.md"},
		Thoughts: "reading the file",
	}
	original := *prop
	if err := pipe.RunAfterParse(context.Background(), prop); err != nil {
		t.Fatalf("RunAfterParse: %v", err)
	}

	if prop.Command != original.Command {
		t.Errorf("Command mutated to %q on sound verdict (was %q)", prop.Command, original.Command)
	}
	if prop.Thoughts != original.Thoughts {
		t.Errorf("Thoughts mutated to %q on sound verdict (was %q)", prop.Thoughts, original.Thoughts)
	}
}

// TestReflexion_HaltsPipelineOnProviderError — when the second-pass LLM
// call errors, the pipeline must surface the error (so the Loop knows
// the step failed at the verification stage). This protects callers
// from silently skipping reflexion when their API key is invalid.
func TestReflexion_HaltsPipelineOnProviderError(t *testing.T) {
	provider := NewMockProvider() // empty responses → next call errors
	pipe := NewPipeline()
	_ = NewReflexionStrategy(NewOneShotStrategy(), provider, pipe)

	// Add a second hook so we can observe whether it ran (it should NOT,
	// per the halt-on-error contract).
	var secondRan bool
	pipe.RegisterAfterParse(func(_ context.Context, _ *ActionProposal) error {
		secondRan = true
		return nil
	})

	prop := &ActionProposal{Command: "read_file"}
	err := pipe.RunAfterParse(context.Background(), prop)
	if err == nil {
		t.Fatal("expected error from RunAfterParse, got nil")
	}
	if !strings.Contains(err.Error(), "reflexion second-pass") {
		t.Errorf("error %q should mention 'reflexion second-pass'", err.Error())
	}
	if secondRan {
		t.Error("second AfterParse hook ran after reflexion error; pipeline did not halt")
	}
}

// TestReflexion_GarbledVerdictPassesThrough — a bonus invariant: if the
// second-pass response isn't valid JSON, we don't block the original
// action. Reflexion is a soft check, not a hard gate.
func TestReflexion_GarbledVerdictPassesThrough(t *testing.T) {
	provider := NewMockProvider(&CreateMessageResponse{
		StopReason: "end_turn",
		Content:    []ContentBlock{{Type: "text", Text: "I think it looks fine, dude."}},
	})
	pipe := NewPipeline()
	_ = NewReflexionStrategy(NewOneShotStrategy(), provider, pipe)

	prop := &ActionProposal{Command: "read_file", Args: map[string]interface{}{"path": "x"}}
	if err := pipe.RunAfterParse(context.Background(), prop); err != nil {
		t.Fatalf("RunAfterParse: %v (garbled verdict should not error)", err)
	}
	if prop.Command != "read_file" {
		t.Errorf("Command = %q on garbled verdict, want untouched", prop.Command)
	}
}

// recordingStrategy is a stub PromptStrategy that records every call so
// tests can assert delegation. Distinct from the loop test's
// stubStrategy — that one captures different fields.
type recordingStrategy struct {
	buildCalls  int
	parseCalls  int
	lastTask    string
	buildResult []Message
	parseResult ActionProposal
	parseErr    error
}

func (s *recordingStrategy) BuildPrompt(history []*Episode, tools []ToolSchema, directives []string, task string) []Message {
	s.buildCalls++
	s.lastTask = task
	return s.buildResult
}

func (s *recordingStrategy) ParseResponse(content []ContentBlock) (ActionProposal, error) {
	s.parseCalls++
	if s.parseErr != nil {
		return ActionProposal{}, s.parseErr
	}
	return s.parseResult, nil
}

// errProvider is unused here but the test scaffold keeps it for future
// expansion ("simulate provider 500 → check error wrapping"). Linked in
// by sentinel reference.
var _ = errors.New("keep errors imported for future tests")
