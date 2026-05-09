package main

import (
	"context"
	"fmt"
	"strings"
)

// Loop is the agent loop. The shape changed in s04: the Loop now holds a
// PromptStrategy field. At the start of Run, the strategy decides what
// the initial []Message looks like and what the system prompt is. After
// each provider response, the strategy.ParseResponse hook turns content
// blocks into a typed ActionProposal.
//
// In s04 the protocol-native (tool_use) path makes this indirection a
// near-no-op — Loop dispatches by ContentBlock just as before. The
// indirection EARNS its keep when:
//
//   - the model emits a JSON-fence fallback instead of native tool_use
//     (smaller open-weight models): ParseResponse recovers the action.
//   - s10 introduces Reflexion: the strategy wraps OneShot and
//     registers an AfterParse hook to re-evaluate the proposal.
//
// If Strategy is nil at Run time, the loop falls back to a default
// OneShotStrategy so existing s03-style construction (no strategy field)
// continues to work.
type Loop struct {
	Provider Provider
	Tools    *Registry
	Strategy PromptStrategy
	MaxTurns int
	Verbose  bool
}

func (l *Loop) Run(ctx context.Context, userPrompt string) (string, error) {
	if l.Strategy == nil {
		l.Strategy = NewOneShotStrategy()
	}
	schemas := l.Tools.All()

	// Strategy decides the opening conversational shape (system prompt
	// + initial messages). For s04 BuildPrompt always returns a single
	// user message; later strategies may inject history.
	messages := l.Strategy.BuildPrompt(nil, schemas, userPrompt)

	// System prompt lives outside []Message because Anthropic's wire
	// format carries it as a top-level request field. Strategies that
	// don't expose BuildSystem will leave system="" — totally fine.
	system := ""
	if oss, ok := l.Strategy.(*OneShotStrategy); ok {
		system = oss.BuildSystem(schemas)
	}

	for turn := 0; turn < l.MaxTurns; turn++ {
		resp, err := l.Provider.CreateMessage(ctx, CreateMessageRequest{
			System:   system,
			Messages: messages,
			Tools:    schemas,
		})
		if err != nil {
			return "", fmt.Errorf("turn %d: %w", turn, err)
		}

		// 1. Append the assistant turn — even if it contains tool_use blocks,
		// the protocol requires the assistant message to live in history.
		messages = append(messages, Message{Role: "assistant", Content: resp.Content})

		if l.Verbose {
			l.dumpAssistant(turn, resp)
		}

		// 2. The stop_reason tells us what to do next.
		switch resp.StopReason {
		case "end_turn", "stop_sequence":
			return extractText(resp.Content), nil

		case "tool_use":
			// Use the strategy's parser as the seam (no behavior change vs
			// s03 for native tool_use; sets up s10's Reflexion hook). If
			// ParseResponse errors and we have content blocks, we still
			// proceed by walking them directly — the legacy path.
			proposal, perr := l.Strategy.ParseResponse(resp.Content)
			if l.Verbose && perr == nil {
				fmt.Printf("[turn %d] proposal: cmd=%s thoughts=%q\n",
					turn, proposal.Command, truncate(proposal.Thoughts, 120))
			}
			toolResults, err := l.runTools(ctx, resp.Content, turn)
			if err != nil {
				return "", err
			}
			// Tool results are sent back as a *user* message with one
			// tool_result block per tool_use the assistant emitted.
			messages = append(messages, Message{Role: "user", Content: toolResults})

		case "max_tokens":
			return "", fmt.Errorf("hit max_tokens at turn %d (response was truncated)", turn)

		default:
			// Some providers (small open-weight, JSON-fence fallback path) end
			// with stop_reason "end_turn" but never emit tool_use; the strategy
			// can still recover via ParseResponse. Try one final parse before
			// surfacing an error.
			if proposal, perr := l.Strategy.ParseResponse(resp.Content); perr == nil && proposal.Command != "" {
				if l.Verbose {
					fmt.Printf("[turn %d] JSON-fallback proposal: cmd=%s\n", turn, proposal.Command)
				}
				toolResults, err := l.runFallbackTool(ctx, proposal, turn)
				if err != nil {
					return "", err
				}
				messages = append(messages, Message{Role: "user", Content: toolResults})
				continue
			}
			return "", fmt.Errorf("unexpected stop_reason %q at turn %d", resp.StopReason, turn)
		}
	}
	return "", fmt.Errorf("loop exceeded MaxTurns=%d without end_turn", l.MaxTurns)
}

func (l *Loop) runTools(ctx context.Context, content []ContentBlock, turn int) ([]ContentBlock, error) {
	var results []ContentBlock
	for _, block := range content {
		if block.Type != "tool_use" {
			continue
		}
		// The s02 change: dispatch by name through the Registry instead of a
		// per-call map. Unknown names still feed back to the model as a
		// recoverable tool_result (same semantics as s01).
		tool, ok := l.Tools.Lookup(block.Name)
		if !ok {
			results = append(results, ContentBlock{
				Type:        "tool_result",
				ToolUseID:   block.ID,
				ToolContent: fmt.Sprintf("unknown tool: %q", block.Name),
			})
			continue
		}
		if l.Verbose {
			fmt.Printf("[turn %d] -> %s %v\n", turn, block.Name, block.Input)
		}
		out, err := tool.Execute(ctx, block.Input)
		if err != nil {
			out = fmt.Sprintf("tool error: %v", err)
		}
		if l.Verbose {
			fmt.Printf("[turn %d] <- %s\n", turn, truncate(out, 240))
		}
		results = append(results, ContentBlock{
			Type:        "tool_result",
			ToolUseID:   block.ID,
			ToolContent: out,
		})
	}
	return results, nil
}

// runFallbackTool dispatches a single ActionProposal that came from the
// JSON-fence fallback path. We synthesize a tool_use_id since the model
// never gave us one, so the Provider sees consistent shapes.
func (l *Loop) runFallbackTool(ctx context.Context, p ActionProposal, turn int) ([]ContentBlock, error) {
	tool, ok := l.Tools.Lookup(p.Command)
	if !ok {
		return []ContentBlock{{
			Type:        "tool_result",
			ToolUseID:   fmt.Sprintf("fallback_%d", turn),
			ToolContent: fmt.Sprintf("unknown tool: %q", p.Command),
		}}, nil
	}
	out, err := tool.Execute(ctx, p.Args)
	if err != nil {
		out = fmt.Sprintf("tool error: %v", err)
	}
	return []ContentBlock{{
		Type:        "tool_result",
		ToolUseID:   fmt.Sprintf("fallback_%d", turn),
		ToolContent: out,
	}}, nil
}

func (l *Loop) dumpAssistant(turn int, resp *CreateMessageResponse) {
	for _, b := range resp.Content {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			fmt.Printf("[turn %d] assistant: %s\n", turn, b.Text)
		}
	}
}

func extractText(content []ContentBlock) string {
	var sb strings.Builder
	for _, b := range content {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
