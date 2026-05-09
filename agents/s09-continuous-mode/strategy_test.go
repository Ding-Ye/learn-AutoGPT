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
// tool's NAME appears in the system prompt.
func TestOneShotStrategy_BuildSystem_RendersAllTools(t *testing.T) {
	s := NewOneShotStrategy()
	sys := s.BuildSystem(makeTools(), nil)
	for _, want := range []string{"echo", "math"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing tool name %q. Full prompt:\n%s", want, sys)
		}
	}
}

// TestOneShotStrategy_BuildSystem_RendersAllBestPractices — each of the
// five default best-practices must show up verbatim in the system prompt.
func TestOneShotStrategy_BuildSystem_RendersAllBestPractices(t *testing.T) {
	s := NewOneShotStrategy()
	sys := s.BuildSystem(makeTools(), nil)

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
// are registered, the strategy must still produce a coherent system prompt.
func TestOneShotStrategy_BuildSystem_EmptyToolsStillWorks(t *testing.T) {
	s := NewOneShotStrategy()
	sys := s.BuildSystem(nil, nil)
	if sys == "" {
		t.Fatal("BuildSystem returned empty string with no tools")
	}
	if !strings.Contains(sys, "no tools available") {
		t.Errorf("expected 'no tools available' marker in empty-tools prompt; got:\n%s", sys)
	}
	if !strings.Contains(sys, "UNDERSTAND BEFORE ACTING") {
		t.Errorf("best-practices missing from empty-tools prompt; got:\n%s", sys)
	}
}

// TestOneShotStrategy_BuildPrompt_WrapsTaskAsUserMessage — BuildPrompt
// emits exactly one user-role Message containing a single text block.
func TestOneShotStrategy_BuildPrompt_WrapsTaskAsUserMessage(t *testing.T) {
	s := NewOneShotStrategy()
	msgs := s.BuildPrompt(nil, makeTools(), nil, "compute 2 + 3")

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

// TestOneShotStrategy_ParseResponse_NativeToolUse — happy path 1.
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

// TestOneShotStrategy_ParseResponse_JSONFenceFallback — happy path 2.
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
	if strings.Contains(got.Thoughts, "```") {
		t.Errorf("Thoughts %q should not contain the fenced block markers", got.Thoughts)
	}
	if !strings.Contains(got.Thoughts, "Sure, I'll echo it back") {
		t.Errorf("Thoughts %q should retain prose around the fence", got.Thoughts)
	}
}

// TestOneShotStrategy_ParseResponse_PlainFenceWithoutLangTag — small
// open-weight models sometimes drop the "json" tag and emit just ```.
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

// TestOneShotStrategy_ParseResponse_ErrorsWhenNeither — sad path.
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

// TestOneShotStrategy_ParseResponse_NativeWinsOverFence — native priority.
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

// ──────────────────────────────────────────────────────────────────────
// s08 NEW test — directives are rendered into the system prompt
// ──────────────────────────────────────────────────────────────────────

// TestOneShotStrategy_BuildSystem_RendersDirectives — when directives
// are non-empty, BuildSystem must produce a "## Directives" section
// with each line numbered. When empty, no section is emitted at all.
//
// This is the core s08 contract: ComponentBus → directives → strategy
// → system message. If a future component returns a directive like
// "Always read a file before editing it.", the model must see that line
// verbatim in its system prompt.
func TestOneShotStrategy_BuildSystem_RendersDirectives(t *testing.T) {
	s := NewOneShotStrategy()

	// Empty directives → no section header.
	sys := s.BuildSystem(makeTools(), nil)
	if strings.Contains(sys, "## Directives") {
		t.Errorf("empty directives produced a 'Directives' section; got:\n%s", sys)
	}

	// Non-empty directives → numbered list under "## Directives".
	directives := []string{
		"Always read a file before editing it.",
		"Use list_files to discover before reading.",
	}
	sys = s.BuildSystem(makeTools(), directives)
	if !strings.Contains(sys, "## Directives") {
		t.Fatalf("non-empty directives missing '## Directives' header; got:\n%s", sys)
	}
	for i, want := range directives {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing directive %d %q", i, want)
		}
	}
	// Numbered list — assert the "1." prefix appears at the head of one
	// of the lines (rendered as "1. <directive>").
	if !strings.Contains(sys, "1. Always read a file before editing it.") {
		t.Errorf("directive 1 not rendered as numbered list; got:\n%s", sys)
	}
	if !strings.Contains(sys, "2. Use list_files to discover before reading.") {
		t.Errorf("directive 2 not rendered as numbered list; got:\n%s", sys)
	}

	// Directives must come AFTER best practices (so the model sees the
	// component-specific rules as more recent / more salient).
	bpIdx := strings.Index(sys, "Best practices")
	dirIdx := strings.Index(sys, "Directives")
	if bpIdx < 0 || dirIdx < 0 {
		t.Fatalf("system layout malformed; got:\n%s", sys)
	}
	if dirIdx <= bpIdx {
		t.Errorf("Directives section must appear after Best practices; bp@%d dir@%d", bpIdx, dirIdx)
	}
}
