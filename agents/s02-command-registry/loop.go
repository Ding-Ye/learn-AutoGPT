package main

import (
	"context"
	"fmt"
	"strings"
)

// Loop is the agent loop. The shape changed in s02: where s01 carried a
// `Tools []Tool` slice and built the name → tool map on every Run, s02
// carries a `*Registry`. The map is now built once at registration time;
// the loop just calls Lookup. This is also the seam s07 (permissions)
// and s08 (components) will plug into.
type Loop struct {
	Provider Provider
	Tools    *Registry
	MaxTurns int
	Verbose  bool
}

func (l *Loop) Run(ctx context.Context, userPrompt string) (string, error) {
	schemas := l.Tools.All()

	messages := []Message{
		{
			Role:    "user",
			Content: []ContentBlock{{Type: "text", Text: userPrompt}},
		},
	}

	for turn := 0; turn < l.MaxTurns; turn++ {
		resp, err := l.Provider.CreateMessage(ctx, CreateMessageRequest{
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
