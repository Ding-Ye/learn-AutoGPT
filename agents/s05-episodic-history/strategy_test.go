package main

import (
	"strings"
	"testing"
)

// makeTools returns two non-trivial tool schemas for testing rendering.
func makeTools() []ToolSchema {
	return []ToolSchema{
		NewEchoTool().Schema(),
		NewMathTool().Schema(),
	}
}

// TestOneShotStrategy_BuildSystem_RendersAllTools — assert each registered
// tool's NAME appears in the system prompt. This is what makes the LLM
// aware of what it can call. If a tool is registered but never appears
// here, the model would never call it.
func TestOneShotStrategy_BuildSystem_RendersAllTools(t *testing.T) {
	s := NewOneShotStrategy()
	sys := s.BuildSystem(makeTools())
	for _, want := range []string{"echo", "math"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing tool name %q. Full prompt:\n%s", want, sys)
		}
	}
}

// TestOneShotStrategy_BuildSystem_RendersAllBestPractices — each of the
// five default best-practices must show up verbatim in the system prompt.
// We assert on a stable substring of each one, not the whole line, so
// minor wording tweaks don't break the assertion.
func TestOneShotStrategy_BuildSystem_RendersAllBestPractices(t *testing.T) {
	s := NewOneShotStrategy()
	sys := s.BuildSystem(makeTools())

	if got, want := len(s.BestPractices), 5; got != want {
		t.Fatalf("DefaultBestPractices count = %d, want %d", got, want)
	}
	for _, want := range []string{
		"UNDERSTAND BEFORE ACTING",
		"PARALLEL EXECUTION",
		"WRITE COMPLETE CODE",
		"VERIFY AFTER CHANGES",
		"FIX ROOT CAUSE",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing best-practice substring %q", want)
		}
	}
}

// TestOneShotStrategy_BuildSystem_EmptyToolsStillWorks — when no tools
// are registered (e.g. a degenerate session), the strategy must still
// produce a coherent system prompt. We verify it doesn't panic and
// doesn't claim to expose tools that don't exist.
func TestOneShotStrategy_BuildSystem_EmptyToolsStillWorks(t *testing.T) {
	s := NewOneShotStrategy()
	sys := s.BuildSystem(nil)
	if sys == "" {
		t.Fatal("BuildSystem returned empty string with no tools")
	}
	if !strings.Contains(sys, "no tools available") {
		t.Errorf("expected 'no tools available' marker in empty-tools prompt; got:\n%s", sys)
	}
	// Best practices should still appear even when tools are empty.
	if !strings.Contains(sys, "UNDERSTAND BEFORE ACTING") {
		t.Errorf("best-practices missing from empty-tools prompt; got:\n%s", sys)
	}
}

// TestOneShotStrategy_BuildPrompt_WrapsTaskAsUserMessage — BuildPrompt
// emits exactly one user-role Message containing a single text block.
// The system prompt is delivered separately via BuildSystem (Anthropic
// carries `system` as a top-level field, not a Message).
func TestOneShotStrategy_BuildPrompt_WrapsTaskAsUserMessage(t *testing.T) {
	s := NewOneShotStrategy()
	msgs := s.BuildPrompt(nil, makeTools(), "compute 2 + 3")

	if len(msgs) != 1 {
		t.Fatalf("BuildPrompt returned %d messages, want 1", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Errorf("Role = %q, want %q", msgs[0].Role, "user")
	}
	if len(msgs[0].Content) != 1 || msgs[0].Content[0].Type != "text" {
		t.Fatalf("Content = %+v, want one text block", msgs[0].Content)
	}
	if msgs[0].Content[0].Text != "compute 2 + 3" {
		t.Errorf("text = %q, want %q", msgs[0].Content[0].Text, "compute 2 + 3")
	}
}

// TestOneShotStrategy_ParseResponse_NativeToolUse — happy path 1: the
// assistant emitted a protocol-native tool_use block. Lift Name/Input
// directly; pull any text blocks into Thoughts.
func TestOneShotStrategy_ParseResponse_NativeToolUse(t *testing.T) {
	s := NewOneShotStrategy()
	got, err := s.ParseResponse([]ContentBlock{
		{Type: "text", Text: "I'll add them."},
		{Type: "tool_use", ID: "toolu_1", Name: "math", Input: map[string]interface{}{
			"operation": "add", "a": 2.0, "b": 3.0,
		}},
	})
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if got.Command != "math" {
		t.Errorf("Command = %q, want %q", got.Command, "math")
	}
	if got.Args["operation"] != "add" {
		t.Errorf("Args[operation] = %v, want \"add\"", got.Args["operation"])
	}
	if got.Thoughts != "I'll add them." {
		t.Errorf("Thoughts = %q, want %q", got.Thoughts, "I'll add them.")
	}
}

// TestOneShotStrategy_ParseResponse_JSONFenceFallback — happy path 2:
// model emitted no tool_use block, but its text contains a
// ```json {"command": "...", "args": {...}} ``` fence. Smaller models
// often do this. We must fall back to JSON parsing, and Thoughts should
// hold the *prose around* the fence (with the fence itself stripped).
func TestOneShotStrategy_ParseResponse_JSONFenceFallback(t *testing.T) {
	s := NewOneShotStrategy()
	body := "Sure, I'll echo it back.\n\n```json\n{\"command\": \"echo\", \"args\": {\"message\": \"hi\"}}\n```\n\nThat's it."
	got, err := s.ParseResponse([]ContentBlock{
		{Type: "text", Text: body},
	})
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if got.Command != "echo" {
		t.Errorf("Command = %q, want %q", got.Command, "echo")
	}
	if got.Args["message"] != "hi" {
		t.Errorf("Args[message] = %v, want %q", got.Args["message"], "hi")
	}
	// The fenced block must be stripped from Thoughts; only prose remains.
	if strings.Contains(got.Thoughts, "```") {
		t.Errorf("Thoughts %q should not contain the fenced block markers", got.Thoughts)
	}
	if !strings.Contains(got.Thoughts, "Sure, I'll echo it back") {
		t.Errorf("Thoughts %q should retain prose around the fence", got.Thoughts)
	}
}

// TestOneShotStrategy_ParseResponse_PlainFenceWithoutLangTag — small
// open-weight models sometimes drop the "json" tag and emit just ```
// ... ```. We must accept that too.
func TestOneShotStrategy_ParseResponse_PlainFenceWithoutLangTag(t *testing.T) {
	s := NewOneShotStrategy()
	body := "```\n{\"command\": \"math\", \"args\": {\"operation\": \"mul\", \"a\": 6, \"b\": 7}}\n```"
	got, err := s.ParseResponse([]ContentBlock{{Type: "text", Text: body}})
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if got.Command != "math" {
		t.Errorf("Command = %q, want %q", got.Command, "math")
	}
	if op, _ := got.Args["operation"].(string); op != "mul" {
		t.Errorf("Args[operation] = %v, want \"mul\"", got.Args["operation"])
	}
}

// TestOneShotStrategy_ParseResponse_ErrorsWhenNeither — sad path. The
// assistant produced text with no tool_use AND no parseable JSON-fence.
// ParseResponse must return an error so the Loop surfaces a clear
// "neither path available" failure rather than a silent empty proposal.
func TestOneShotStrategy_ParseResponse_ErrorsWhenNeither(t *testing.T) {
	s := NewOneShotStrategy()
	_, err := s.ParseResponse([]ContentBlock{
		{Type: "text", Text: "I'm just chatting, no tool call here."},
	})
	if err == nil {
		t.Fatal("expected error when no tool_use and no JSON fence, got nil")
	}
	if !strings.Contains(err.Error(), "neither") {
		t.Errorf("error %q should mention 'neither'", err.Error())
	}
}

// TestOneShotStrategy_ParseResponse_NativeWinsOverFence — if both a
// native tool_use block AND a JSON fence appear, the native path takes
// priority. This avoids the model double-specifying the same call and
// our parser executing it twice.
func TestOneShotStrategy_ParseResponse_NativeWinsOverFence(t *testing.T) {
	s := NewOneShotStrategy()
	got, err := s.ParseResponse([]ContentBlock{
		{Type: "text", Text: "```json\n{\"command\": \"echo\", \"args\": {\"message\": \"fence\"}}\n```"},
		{Type: "tool_use", ID: "toolu_x", Name: "math", Input: map[string]interface{}{"operation": "add", "a": 1.0, "b": 1.0}},
	})
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if got.Command != "math" {
		t.Errorf("Command = %q, want %q (native tool_use must win)", got.Command, "math")
	}
}
