// strategy.go — the PromptStrategy seam.
//
// Up through s03, the Loop hard-coded prompt construction: take the user
// string, wrap it in a single user Message, hand it to the Provider. Real
// agents need more: a system message that lists the tool schemas, a set
// of best-practice directives, and parsing logic that copes when the
// model emits a JSON-in-fenced-code-block fallback instead of a native
// `tool_use` content block (smaller open-weight models do this).
//
// AutoGPT classic abstracts this with `PromptStrategy` and ships eight
// concrete strategies (one_shot, rewoo, reflexion, plan_execute, lats,
// tree_of_thoughts, multi_agent_debate, base). Per the dossier's
// anti-pattern list ("eight competing prompt strategies in one repo is
// pedagogically overwhelming"), we ship exactly ONE here — OneShotStrategy
// — and reference Reflexion in s10 where it slots in as a hook.
//
// Three new exported types live in this file:
//
//   - ActionProposal — the parsed shape of an LLM response (what command,
//     what args, what reasoning).
//   - PromptStrategy — the interface every strategy must satisfy.
//   - OneShotStrategy — the baseline implementation.
//   - Episode — placeholder struct introduced here so the strategy
//     interface signature is forward-compatible with s05's history.
package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ActionProposal is the post-parse shape of one LLM turn. The Loop reads
// (Command, Args) to dispatch the next tool call. Thoughts is non-empty
// when the model produced free-text reasoning alongside its tool choice
// — useful for verbose mode and for s10's Reflexion to evaluate.
//
// In native tool-use mode (Anthropic content-blocks, OpenAI tool_calls
// translated by s03's OpenAIProvider) the parser fills Command/Args from
// the protocol-native block. In JSON-fallback mode it parses a fenced
// code block in the assistant text (see ParseResponse below).
type ActionProposal struct {
	Thoughts string                 // free-text reasoning, may be empty
	Command  string                 // tool name; empty if no action proposed
	Args     map[string]interface{} // tool input
}

// Episode is a placeholder. s05 will fill this in with real fields
// (Actions []ActionProposal, Results []ActionResult, etc.). For s04 the
// strategy.BuildPrompt signature already accepts []*Episode so s05 can
// slot in without changing any interface — the slice is just always nil
// at this stage.
type Episode struct {
	// Filled in by s05 — for s04, this struct is just a placeholder
	// so the strategy interface signature is forward-compatible.
}

// PromptStrategy is the seam between the Loop and prompt construction.
// Two methods, one per direction:
//
//   - BuildPrompt assembles []Message that gets sent to the Provider.
//     The strategy decides how the system prompt looks (best-practices,
//     tool list, role description) and how history is folded in.
//   - ParseResponse turns the assistant's response content blocks into
//     a typed ActionProposal. This is where the JSON-fenced-code-block
//     fallback happens, and where s10's Reflexion strategy will hook a
//     second LLM pass to self-evaluate.
//
// BuildPrompt accepts []*Episode (forward-compat for s05); for s04, the
// Loop always passes nil so this slice is unused.
type PromptStrategy interface {
	BuildPrompt(history []*Episode, tools []ToolSchema, task string) []Message
	ParseResponse(content []ContentBlock) (ActionProposal, error)
}

// DefaultBestPractices is the 5-line directive list folded into every
// OneShotStrategy system prompt. We trim AutoGPT classic's 7-bullet
// "Efficiency Guidelines" down to the five that survive the translation
// (commands like "RUN linters/tests" only mean something once s06
// introduces a Workspace and exec-style tools — premature here).
//
// These are exposed as a package-level var (not const) so that a unit
// test can assert each line ends up in the rendered system prompt.
var DefaultBestPractices = []string{
	"UNDERSTAND BEFORE ACTING: read all relevant files / inputs before making changes; never guess at interfaces.",
	"PARALLEL EXECUTION: when independent operations can run concurrently, request them in one turn rather than serializing.",
	"WRITE COMPLETE CODE: produce full working implementations — no stubs, TODOs, or placeholders.",
	"VERIFY AFTER CHANGES: after modifying state, verify the change took (re-read a file, re-run a check).",
	"FIX ROOT CAUSE: when something breaks, fix the underlying cause, not the symptom; if a test fails, the bug is in your code, not the test.",
}

// OneShotStrategy is the baseline strategy: one system message with
// directives + tool list, one user message with the task, no history
// rendering for s04 (history is always nil at this stage).
//
// Field choices:
//
//   - BestPractices is mutable so tests (and future curriculum entries)
//     can swap in alternate directives without re-implementing the
//     strategy. Defaults to DefaultBestPractices when constructed via
//     NewOneShotStrategy.
type OneShotStrategy struct {
	BestPractices []string
}

// NewOneShotStrategy constructs the default strategy with the standard
// five best-practices. Tests that need a custom directive list construct
// the struct directly.
func NewOneShotStrategy() *OneShotStrategy {
	bp := make([]string, len(DefaultBestPractices))
	copy(bp, DefaultBestPractices)
	return &OneShotStrategy{BestPractices: bp}
}

// BuildPrompt renders the system + user messages.
//
// Output shape (always 1 user message; system prompt sets the tone):
//
//	[user]   <task verbatim>
//
// The system prompt itself is a single string built from three sections:
//
//  1. role intro
//  2. numbered tool list (name, description, JSON-encoded input schema)
//  3. numbered best-practices
//
// The Loop uses the returned []Message as the *initial* messages slice;
// every subsequent provider response and tool_result still appends to
// the same slice exactly as before. So strategy controls the *opening*
// conversational shape, not every turn.
//
// Note: the system prompt is *not* placed in []Message — it travels via
// CreateMessageRequest.System (Anthropic's top-level field). The Loop
// reads strategy.SystemPrompt(...) for that; here we keep things simple
// and emit only the user message. The system text is exposed for the
// Loop via a separate helper (BuildSystem) — see below.
func (s *OneShotStrategy) BuildPrompt(history []*Episode, tools []ToolSchema, task string) []Message {
	// history is reserved for s05; ignored here.
	_ = history
	return []Message{
		{
			Role:    "user",
			Content: []ContentBlock{{Type: "text", Text: task}},
		},
	}
}

// BuildSystem returns the system-prompt string for a request. Kept as a
// separate method (not part of PromptStrategy) because the Anthropic
// wire shape carries `system` as a top-level request field rather than
// a Message — exposing it as []Message would force the Provider layer
// to special-case the first element. s04's Loop reads BuildSystem.
//
// Sections, joined by "\n\n":
//
//   - role line ("You are a methodical autonomous agent...")
//   - "## Commands" + numbered list (or a "no tools available" line)
//   - "## Best practices" + numbered list
func (s *OneShotStrategy) BuildSystem(tools []ToolSchema) string {
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

	return strings.TrimRight(b.String(), "\n")
}

// fenceRegex captures the body of a ```json ... ``` (or plain ```) fenced
// code block. Used by the JSON fallback parse below. We accept the lang
// tag being either "json" or absent — small models often drop it.
var fenceRegex = regexp.MustCompile("(?s)```(?:json)?\\s*\\n?(.*?)```")

// ParseResponse turns an assistant CreateMessageResponse.Content into an
// ActionProposal, by trying the two paths in priority order:
//
//  1. NATIVE — if any block has Type=="tool_use", the model used the
//     protocol-native path. Lift Name/Input directly into Command/Args
//     and concatenate any text blocks into Thoughts.
//
//  2. JSON FALLBACK — no tool_use block, but a text block contains a
//     ```json ... ``` fence whose JSON has shape `{"command": "...",
//     "args": {...}}`. This is what smaller open-weight models do when
//     they don't reliably emit native tool_calls. Parse it, return
//     Command/Args from the JSON, Thoughts from the text *outside* the
//     fence.
//
// 3. otherwise — return an error so the Loop can surface a recoverable
// failure (the model produced neither a tool nor a JSON-fenced action).
//
// In native mode the Loop already has access to Content blocks for its
// own tool_use → tool_result round-trip; ParseResponse exists primarily
// so s10 has a single seam to introduce Reflexion's second-pass. The
// JSON fallback is the *immediate* practical payoff for s04.
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

	// JSON fallback path: scan concatenated text for a ```json ... ``` fence.
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
		// Strip the fenced block out of Thoughts so the user sees only the
		// model's reasoning prose, not its action JSON.
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
