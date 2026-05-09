// strategy.go — the PromptStrategy seam.
//
// s08 widens the BuildPrompt signature to accept a `directives []string`
// parameter — the per-component policy notes that ComponentBus aggregates.
// Compared to s07's signature this is a breaking change: callers now
// pass directives between `tools` and `task`. OneShotStrategy renders
// directives into the system message (a new "## Directives" section
// after "## Best practices"), so component-supplied policy lines reach
// the model without any new wire fields.
//
// AutoGPT classic abstracts prompt construction with `PromptStrategy`
// and ships eight concrete strategies; we stay with one (OneShotStrategy)
// and reference Reflexion in s10. The Episode type lives in `history.go`.
package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ActionProposal is the post-parse shape of one LLM turn. The Loop reads
// (Command, Args) to dispatch the next tool call. Thoughts is non-empty
// when the model produced free-text reasoning alongside its tool choice.
type ActionProposal struct {
	Thoughts string                 // free-text reasoning, may be empty
	Command  string                 // tool name; empty if no action proposed
	Args     map[string]interface{} // tool input
}

// PromptStrategy is the seam between the Loop and prompt construction.
//
// s08 CHANGE: `BuildPrompt` now takes a `directives []string` parameter
// (added between `tools` and `task`). Component-supplied policy lines —
// e.g. "Always read a file before editing it." — flow from
// `ComponentBus.Directives()` into the strategy via this seam.
//
// Why a new parameter and not a Loop-side wrapper that prepends to the
// system message? Because the system prompt is built by the strategy
// itself (it lives in `BuildSystem`, not in BuildPrompt). Letting
// `BuildPrompt` see directives means the strategy can decide HOW to
// integrate them — OneShot drops them into a numbered "## Directives"
// list under best-practices, but a future strategy might inline them
// into each user turn instead. Keeping the integration in the strategy
// preserves that freedom.
type PromptStrategy interface {
	BuildPrompt(history []*Episode, tools []ToolSchema, directives []string, task string) []Message
	ParseResponse(content []ContentBlock) (ActionProposal, error)
}

// DefaultBestPractices is the 5-line directive list folded into every
// OneShotStrategy system prompt. We trim AutoGPT classic's 7-bullet
// "Efficiency Guidelines" down to the five that survive the translation.
var DefaultBestPractices = []string{
	"UNDERSTAND BEFORE ACTING: read all relevant files / inputs before making changes; never guess at interfaces.",
	"PARALLEL EXECUTION: when independent operations can run concurrently, request them in one turn rather than serializing.",
	"WRITE COMPLETE CODE: produce full working implementations — no stubs, TODOs, or placeholders.",
	"VERIFY AFTER CHANGES: after modifying state, verify the change took (re-read a file, re-run a check).",
	"FIX ROOT CAUSE: when something breaks, fix the underlying cause, not the symptom; if a test fails, the bug is in your code, not the test.",
}

// OneShotStrategy is the baseline strategy: one system message with
// directives + tool list, plus a [user, assistant, user, ...] flow
// rebuilt from the History, plus the user's task as the final user
// message.
type OneShotStrategy struct {
	BestPractices []string
}

// NewOneShotStrategy constructs the default strategy with the standard
// five best-practices.
func NewOneShotStrategy() *OneShotStrategy {
	bp := make([]string, len(DefaultBestPractices))
	copy(bp, DefaultBestPractices)
	return &OneShotStrategy{BestPractices: bp}
}

// BuildPrompt renders the messages slice that will be sent to the model.
//
// The s08 signature accepts `directives` from the Loop — those lines
// originate in component `Directives()` methods (see component.go) and
// the strategy folds them into the system prompt via BuildSystem.
// BuildPrompt itself emits only the [history..., user-task] message
// flow; the system message is delivered separately by BuildSystem so
// the Anthropic wire shape (where `system` is a top-level request
// field, not a Message) doesn't force the Provider to special-case the
// first element.
//
// `directives` is part of the BuildPrompt signature for symmetry with
// `tools`: the Loop fetches both from the bus and passes both in one
// call. The strategy chooses whether to use them — OneShot routes them
// into BuildSystem; a hypothetical "directive-as-user-prefix" strategy
// could splice them into the trailing user message instead.
func (s *OneShotStrategy) BuildPrompt(history []*Episode, tools []ToolSchema, directives []string, task string) []Message {
	// Convert []*Episode → History so we can call RenderMessages.
	h := History(history)
	msgs := h.RenderMessages()

	// We intentionally ignore `directives` here — they live in the
	// system message that the Loop reads via BuildSystem(tools,
	// directives). The signature receives them for symmetry with `tools`
	// and to keep the strategy as the single owner of "how directives
	// reach the model."
	_ = directives

	msgs = append(msgs, Message{
		Role:    "user",
		Content: []ContentBlock{{Type: "text", Text: task}},
	})
	return msgs
}

// BuildSystem returns the system-prompt string for a request.
//
// s08 CHANGE: takes `directives []string` as a second parameter and
// emits them as a numbered "## Directives" section AFTER the best
// practices. Empty directives produce no section (no header, no body)
// so a directive-less component setup renders identically to s07.
//
// Sections, joined by "\n\n":
//
//   - role line ("You are a methodical autonomous agent...")
//   - "## Commands" + numbered list (or a "no tools available" line)
//   - "## Best practices" + numbered list
//   - "## Directives" + numbered list (only if directives is non-empty)
func (s *OneShotStrategy) BuildSystem(tools []ToolSchema, directives []string) string {
	var b strings.Builder
	b.WriteString("You are a methodical autonomous agent. ")
	b.WriteString("Decide one or more tool calls per turn, observe the result, then continue. ")
	b.WriteString("When the task is complete, reply with plain text and no tool call.")

	b.WriteString("\n\n## Commands\n")
	if len(tools) == 0 {
		b.WriteString("(no tools available; respond with plain text)\n")
	} else {
		b.WriteString("These are the ONLY commands you can use. Any action you perform must be possible through one of these:\n")
		for i, t := range tools {
			schemaJSON, err := json.Marshal(t.InputSchema)
			if err != nil {
				schemaJSON = []byte("{}")
			}
			fmt.Fprintf(&b, "%d. **%s** — %s\n   input_schema: %s\n",
				i+1, t.Name, t.Description, string(schemaJSON))
		}
	}

	b.WriteString("\n## Best practices\n")
	for i, line := range s.BestPractices {
		fmt.Fprintf(&b, "%d. %s\n", i+1, line)
	}

	if len(directives) > 0 {
		b.WriteString("\n## Directives\n")
		b.WriteString("These are component-supplied policies you must follow:\n")
		for i, d := range directives {
			fmt.Fprintf(&b, "%d. %s\n", i+1, d)
		}
	}

	return strings.TrimRight(b.String(), "\n")
}

// fenceRegex captures the body of a ```json ... ``` (or plain ```) fenced
// code block.
var fenceRegex = regexp.MustCompile("(?s)```(?:json)?\\s*\\n?(.*?)```")

// ParseResponse turns an assistant CreateMessageResponse.Content into an
// ActionProposal, by trying the two paths in priority order:
//
//  1. NATIVE — if any block has Type=="tool_use", lift Name/Input directly
//     into Command/Args and concatenate any text blocks into Thoughts.
//  2. JSON FALLBACK — no tool_use block, but a text block contains a
//     ```json ... ``` fence whose JSON has shape `{"command": "...",
//     "args": {...}}`.
//  3. otherwise — return an error so the Loop can surface a recoverable
//     failure.
func (s *OneShotStrategy) ParseResponse(content []ContentBlock) (ActionProposal, error) {
	var thoughts []string
	var toolUseBlock *ContentBlock
	var allText []string

	for i := range content {
		b := &content[i]
		switch b.Type {
		case "tool_use":
			if toolUseBlock == nil {
				toolUseBlock = b
			}
		case "text":
			allText = append(allText, b.Text)
			thoughts = append(thoughts, b.Text)
		}
	}

	if toolUseBlock != nil {
		return ActionProposal{
			Thoughts: strings.TrimSpace(strings.Join(thoughts, "\n")),
			Command:  toolUseBlock.Name,
			Args:     toolUseBlock.Input,
		}, nil
	}

	combined := strings.Join(allText, "\n")
	if match := fenceRegex.FindStringSubmatch(combined); len(match) > 1 {
		payload := strings.TrimSpace(match[1])
		var parsed struct {
			Command string                 `json:"command"`
			Args    map[string]interface{} `json:"args"`
		}
		if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
			return ActionProposal{}, fmt.Errorf("parse JSON fallback: %w (payload=%q)", err, payload)
		}
		if parsed.Command == "" {
			return ActionProposal{}, fmt.Errorf("parse JSON fallback: missing required field %q", "command")
		}
		thoughtsText := strings.TrimSpace(fenceRegex.ReplaceAllString(combined, ""))
		return ActionProposal{
			Thoughts: thoughtsText,
			Command:  parsed.Command,
			Args:     parsed.Args,
		}, nil
	}

	return ActionProposal{}, fmt.Errorf("ParseResponse: response has neither tool_use block nor JSON-fenced action (content blocks: %d)", len(content))
}

// Compile-time check.
var _ PromptStrategy = (*OneShotStrategy)(nil)
