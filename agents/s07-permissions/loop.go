package main

import (
	"context"
	"fmt"
	"strings"
)

// Loop is the agent loop. The shape changed in s05 (added History) and
// changes again in s07: two new fields gate every tool dispatch.
//
// What changed vs s06:
//
//   - new field `Permissions *Permissions` (nil-safe; nil means
//     "permit everything", which is what s01-s06 effectively had);
//   - new field `Asker Asker` (interface; only consulted on
//     `Permissions.Check → Ask`. nil here is a config error: if the
//     loop hits an Ask decision with no Asker, Run returns an error
//     explaining what happened).
//   - per-block dispatch in `runTools` calls `Permissions.Check` BEFORE
//     `tool.Execute`. On Deny we synthesize an ActionResult with status
//     "denied" and body "permission denied: <cmd>", which RenderMessages
//     will pass through to the model on the next turn so it can adapt.
//
// Why gate at parse time, not inside Tool.Execute? Per the dossier's
// anti-pattern #2: cross-cutting concerns (permission, logging, audit)
// belong at the Loop's seams, not threaded through every Tool. If the
// gate were inside each Tool's Execute we'd duplicate it N times across
// the codebase and miss any tool that forgot to call it. One gate, one
// place.
type Loop struct {
	Provider    Provider
	Tools       *Registry
	Strategy    PromptStrategy
	History     *History
	Permissions *Permissions // nil → permit everything (s06 backward-compat)
	Asker       Asker        // consulted only on Ask decisions; required if any rule yields Ask
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
	schemas := l.Tools.All()

	system := ""
	if oss, ok := l.Strategy.(*OneShotStrategy); ok {
		system = oss.BuildSystem(schemas)
	}

	for turn := 0; turn < l.MaxTurns; turn++ {
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

			results, err := l.runTools(ctx, resp.Content, turn)
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

// runTools dispatches each tool_use block. The s07 change: BEFORE
// looking up + executing the tool, consult Permissions.Check and, on
// Ask, delegate to the Asker. On Deny (either directly or after Ask),
// synthesize a "permission denied" ActionResult so the model sees the
// rejection on its next turn.
//
// Permissions==nil keeps the s06 behavior (everything goes through) so
// the existing s06 loop tests still pass when copy-pasted.
func (l *Loop) runTools(ctx context.Context, content []ContentBlock, turn int) ([]ActionResult, error) {
	var results []ActionResult
	for _, block := range content {
		if block.Type != "tool_use" {
			continue
		}

		// Permission gate. nil Permissions → no gate (s06 behavior).
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

// runFallbackTool handles the JSON-fence path. Same gate logic as
// runTools — extract the permission check so both paths share semantics.
func (l *Loop) runFallbackTool(ctx context.Context, p ActionProposal, turn int) (ActionResult, error) {
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
