package main

import (
	"context"
	"fmt"
	"strings"
)

// Loop is the agent loop. The s08 redesign collapses s07's seven
// fields back to two-and-a-half: the Provider, a *ComponentBus, and a
// few cross-cutting concerns (Permissions, Asker, Strategy, History,
// MaxTurns, Verbose) that don't fit the "component" model.
//
// What changed vs s07:
//
//   - the `Tools *Registry` field is GONE; the Loop synthesizes its
//     Registry from `l.Components.Registry()` at the top of Run.
//   - new field `Components *ComponentBus`. Required.
//   - the strategy now receives `directives []string` from the bus —
//     the OneShotStrategy renders them into the system prompt's
//     "## Directives" section (see strategy.go).
//   - if the bus contributes Messages() (s08 MessageProvider), they're
//     prepended to the BuildPrompt result on the FIRST turn only —
//     subsequent turns rely on history rendering, so prepending each
//     time would duplicate the messages on every round.
//
// Why does Permissions / Asker / Strategy / History stay as Loop
// fields rather than become components? Because they're not
// "capabilities the agent has" — they're cross-cutting concerns that
// gate what the agent does. A component contributes commands or
// directives; a strategy chooses how to compose the prompt; a
// permission gate decides what to allow. The shapes are distinct.
//
// AutoGPT upstream's `Agent` class makes the same call: components
// are stored in a list and discovered via reflection, but the
// LLMProvider, the Strategy, the FileStorage, etc. are first-class
// fields on the Agent. We mirror that.
type Loop struct {
	Provider    Provider
	Components  *ComponentBus
	Strategy    PromptStrategy
	History     *History
	Permissions *Permissions
	Asker       Asker
	MaxTurns    int
	Verbose     bool
}

func (l *Loop) Run(ctx context.Context, userPrompt string) (string, error) {
	if l.Strategy == nil {
		l.Strategy = NewOneShotStrategy()
	}
	if l.History == nil {
		h := History{}
		l.History = &h
	}
	if l.Components == nil {
		// Empty bus is legal — produces an empty Registry and an empty
		// directive list. Mirrors s07's "no tools" fallback.
		l.Components = NewComponentBus()
	}

	// Derive Registry and directive list from the bus once at startup.
	// Both are stable across the run — components are constructed up
	// front and don't mutate during a Loop.Run call.
	registry := l.Components.Registry()
	directives := l.Components.Directives()
	preMessages := l.Components.Messages()
	schemas := registry.All()

	// Build the system message via the strategy. We use a type
	// assertion to OneShotStrategy's BuildSystem (which takes
	// directives) when available; a custom strategy that doesn't
	// expose BuildSystem will fall back to an empty system string —
	// callers wanting a system message must supply a strategy that
	// implements (the unexported convention) BuildSystem.
	system := ""
	if oss, ok := l.Strategy.(*OneShotStrategy); ok {
		system = oss.BuildSystem(schemas, directives)
	}

	for turn := 0; turn < l.MaxTurns; turn++ {
		messages := l.Strategy.BuildPrompt(*l.History, schemas, directives, userPrompt)

		// First-turn-only: prepend any MessageProvider-supplied messages.
		// The check is `turn == 0 && len(*l.History) == 0` so a Loop
		// re-entered with pre-populated history doesn't double-inject.
		if turn == 0 && len(*l.History) == 0 && len(preMessages) > 0 {
			combined := make([]Message, 0, len(preMessages)+len(messages))
			combined = append(combined, preMessages...)
			combined = append(combined, messages...)
			messages = combined
		}

		resp, err := l.Provider.CreateMessage(ctx, CreateMessageRequest{
			System:   system,
			Messages: messages,
			Tools:    schemas,
		})
		if err != nil {
			return "", fmt.Errorf("turn %d: %w", turn, err)
		}

		if l.Verbose {
			l.dumpAssistant(turn, resp)
		}

		switch resp.StopReason {
		case "end_turn", "stop_sequence":
			return extractText(resp.Content), nil

		case "tool_use":
			ep := &Episode{}
			l.History.Append(ep)

			proposal, perr := l.Strategy.ParseResponse(resp.Content)
			if perr == nil {
				ep.Actions = append(ep.Actions, proposal)
			} else if l.Verbose {
				fmt.Printf("[turn %d] ParseResponse error (continuing via direct dispatch): %v\n", turn, perr)
			}
			if l.Verbose && perr == nil {
				fmt.Printf("[turn %d] proposal: cmd=%s thoughts=%q\n",
					turn, proposal.Command, truncate(proposal.Thoughts, 120))
			}

			results, err := l.runTools(ctx, registry, resp.Content, turn)
			if err != nil {
				return "", err
			}
			ep.Results = append(ep.Results, results...)

		case "max_tokens":
			return "", fmt.Errorf("hit max_tokens at turn %d (response was truncated)", turn)

		default:
			// JSON-fence fallback path.
			if proposal, perr := l.Strategy.ParseResponse(resp.Content); perr == nil && proposal.Command != "" {
				ep := &Episode{}
				l.History.Append(ep)
				ep.Actions = append(ep.Actions, proposal)

				if l.Verbose {
					fmt.Printf("[turn %d] JSON-fallback proposal: cmd=%s\n", turn, proposal.Command)
				}
				result, err := l.runFallbackTool(ctx, registry, proposal, turn)
				if err != nil {
					return "", err
				}
				ep.Results = append(ep.Results, result)
				continue
			}
			return "", fmt.Errorf("unexpected stop_reason %q at turn %d", resp.StopReason, turn)
		}
	}
	return "", fmt.Errorf("loop exceeded MaxTurns=%d without end_turn", l.MaxTurns)
}

// runTools dispatches each tool_use block. Same s07 gate semantics:
// permission check → ask if needed → execute. Difference: takes the
// registry as a parameter rather than reading from a Loop field, so
// the bus-derived registry is never confused with a stale one.
func (l *Loop) runTools(ctx context.Context, registry *Registry, content []ContentBlock, turn int) ([]ActionResult, error) {
	var results []ActionResult
	for _, block := range content {
		if block.Type != "tool_use" {
			continue
		}

		// Permission gate. nil Permissions → no gate.
		if l.Permissions != nil {
			decision := l.Permissions.Check(block.Name, block.Input)
			if l.Verbose {
				fmt.Printf("[turn %d] permission check: %s → %v\n", turn, block.Name, decision)
			}
			switch decision {
			case Allow:
				// proceed below
			case Deny:
				results = append(results, ActionResult{
					Status: "denied",
					Output: fmt.Sprintf("permission denied: %s", block.Name),
				})
				continue
			case Ask:
				if l.Asker == nil {
					return nil, fmt.Errorf("permission required for %s but no Asker configured", block.Name)
				}
				ans := l.Asker.Ask(block.Name, block.Input)
				if l.Verbose {
					fmt.Printf("[turn %d] asker replied: %v\n", turn, ans)
				}
				if ans != Allow {
					results = append(results, ActionResult{
						Status: "denied",
						Output: fmt.Sprintf("permission denied (user): %s", block.Name),
					})
					continue
				}
			}
		}

		tool, ok := registry.Lookup(block.Name)
		if !ok {
			results = append(results, ActionResult{
				Status: "error",
				Output: fmt.Sprintf("unknown tool: %q", block.Name),
			})
			continue
		}
		if l.Verbose {
			fmt.Printf("[turn %d] -> %s %v\n", turn, block.Name, block.Input)
		}
		out, err := tool.Execute(ctx, block.Input)
		if err != nil {
			if l.Verbose {
				fmt.Printf("[turn %d] <- error: %v\n", turn, err)
			}
			results = append(results, ActionResult{
				Status: "error",
				Output: err.Error(),
			})
			continue
		}
		if l.Verbose {
			fmt.Printf("[turn %d] <- %s\n", turn, truncate(out, 240))
		}
		results = append(results, ActionResult{Status: "ok", Output: out})
	}
	return results, nil
}

// runFallbackTool handles the JSON-fence path. Same gate logic.
func (l *Loop) runFallbackTool(ctx context.Context, registry *Registry, p ActionProposal, turn int) (ActionResult, error) {
	if l.Permissions != nil {
		decision := l.Permissions.Check(p.Command, p.Args)
		switch decision {
		case Allow:
			// proceed
		case Deny:
			return ActionResult{Status: "denied", Output: fmt.Sprintf("permission denied: %s", p.Command)}, nil
		case Ask:
			if l.Asker == nil {
				return ActionResult{}, fmt.Errorf("permission required for %s but no Asker configured", p.Command)
			}
			if l.Asker.Ask(p.Command, p.Args) != Allow {
				return ActionResult{Status: "denied", Output: fmt.Sprintf("permission denied (user): %s", p.Command)}, nil
			}
		}
	}
	tool, ok := registry.Lookup(p.Command)
	if !ok {
		return ActionResult{Status: "error", Output: fmt.Sprintf("unknown tool: %q", p.Command)}, nil
	}
	out, err := tool.Execute(ctx, p.Args)
	if err != nil {
		return ActionResult{Status: "error", Output: err.Error()}, nil
	}
	_ = turn
	return ActionResult{Status: "ok", Output: out}, nil
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
