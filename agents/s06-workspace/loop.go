package main

import (
	"context"
	"fmt"
	"strings"
)

// Loop is the agent loop. The shape changed in s05: the Loop now holds
// a `*History` field. At the start of every turn the Loop appends a
// fresh Episode; after the assistant proposes, the proposal is recorded
// on Current(); after the tool runs, the ActionResult is appended too.
// At the start of the NEXT turn, BuildPrompt receives the populated
// `[]*Episode` — so the strategy actually has prior episodes to fold in.
//
// What changed vs s04:
//
//   - new field `History *History` (pointer to a slice; nil-safe at Run).
//   - at the start of each turn, append a new Episode and record the
//     proposal on it.
//   - after Execute, append an ActionResult to the same Episode.
//   - BuildPrompt is called once per turn (s04 only called it once at
//     the start) so each turn's BuildPrompt sees prior episodes; the
//     full `[]Message` is rebuilt from history each time.
//
// If History is nil at Run, the Loop allocates a fresh empty one — so
// existing s04-style construction `&Loop{Provider: p, Tools: r,
// Strategy: s, MaxTurns: n}` still works unchanged.
//
// If Strategy is nil at Run, fallback to OneShotStrategy (same as s04).
type Loop struct {
	Provider Provider
	Tools    *Registry
	Strategy PromptStrategy
	History  *History
	MaxTurns int
	Verbose  bool
}

func (l *Loop) Run(ctx context.Context, userPrompt string) (string, error) {
	if l.Strategy == nil {
		l.Strategy = NewOneShotStrategy()
	}
	if l.History == nil {
		h := History{}
		l.History = &h
	}
	schemas := l.Tools.All()

	// System prompt — same shape as s04. Strategies that don't expose
	// BuildSystem (e.g. a custom Strategy in tests) leave system="".
	system := ""
	if oss, ok := l.Strategy.(*OneShotStrategy); ok {
		system = oss.BuildSystem(schemas)
	}

	for turn := 0; turn < l.MaxTurns; turn++ {
		// s05 difference: BuildPrompt is called EACH turn, with the
		// up-to-date history. RenderMessages inside BuildPrompt rebuilds
		// the full assistant/user message flow from prior episodes.
		messages := l.Strategy.BuildPrompt(*l.History, schemas, userPrompt)

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

		// Decide what to do based on stop_reason. The Episode is created
		// only on tool turns — pure end_turn responses don't need a
		// history entry (the Loop just returns the text).
		switch resp.StopReason {
		case "end_turn", "stop_sequence":
			return extractText(resp.Content), nil

		case "tool_use":
			// Begin a new Episode for THIS turn. We append the proposal
			// before running tools so RenderMessages mid-turn (e.g. the
			// next iteration's BuildPrompt) sees the assistant message
			// even before the result lands.
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

			results, err := l.runTools(ctx, resp.Content, turn)
			if err != nil {
				return "", err
			}
			// Record the per-action results (one ActionResult per tool_use
			// block in the original assistant message).
			ep.Results = append(ep.Results, results...)

		case "max_tokens":
			return "", fmt.Errorf("hit max_tokens at turn %d (response was truncated)", turn)

		default:
			// JSON-fence fallback path. The model emitted text with a
			// fenced action; runFallbackTool synthesizes a tool_use_id.
			if proposal, perr := l.Strategy.ParseResponse(resp.Content); perr == nil && proposal.Command != "" {
				ep := &Episode{}
				l.History.Append(ep)
				ep.Actions = append(ep.Actions, proposal)

				if l.Verbose {
					fmt.Printf("[turn %d] JSON-fallback proposal: cmd=%s\n", turn, proposal.Command)
				}
				result, err := l.runFallbackTool(ctx, proposal, turn)
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

// runTools dispatches tool_use blocks through the Registry and returns
// per-action ActionResults. The change from s04: instead of returning
// []ContentBlock for the next user message, we return []ActionResult
// so the Loop can append them to the current Episode. Message rendering
// is now history.RenderMessages's job.
func (l *Loop) runTools(ctx context.Context, content []ContentBlock, turn int) ([]ActionResult, error) {
	var results []ActionResult
	for _, block := range content {
		if block.Type != "tool_use" {
			continue
		}
		tool, ok := l.Tools.Lookup(block.Name)
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

// runFallbackTool dispatches a single ActionProposal (from the JSON
// fence path) and returns one ActionResult. Same intent as s04, but
// returning the result-shape now since history rendering is the
// destination.
func (l *Loop) runFallbackTool(ctx context.Context, p ActionProposal, turn int) (ActionResult, error) {
	tool, ok := l.Tools.Lookup(p.Command)
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
