package main

import (
	"context"
	"fmt"
	"strings"
)

// Loop is the agent loop. The s09 redesign extracts a `runStep` method
// from `Run` so the new `RunInteractionLoop` wrapper (interaction_loop.go)
// can drive one think→act→observe step at a time without duplicating the
// strategy/parse/permission/execute sequence.
//
// What changed vs s08:
//
//   - extracted `runStep(ctx, schemas, registry, system, userPrompt)`
//     returning `(stepResult, error)`. `Run` still loops up to MaxTurns
//     and is unchanged for tests inherited from s08.
//   - added `stepResult` struct so RunInteractionLoop can read both the
//     final answer (when end_turn) and per-step outputs (proposal +
//     ActionResult slice) without re-parsing the response.
//
// The Provider, Components, Strategy, History, Permissions, Asker,
// MaxTurns, and Verbose fields are unchanged from s08. The new
// LoopOpts/UIProvider types live in interaction_loop.go and ui.go so
// `Loop` itself stays simple.
//
// Why not collapse everything into RunInteractionLoop and delete Run?
// Because Run is a stable contract (every test from s01–s08 calls it)
// and a per-cycle wrapper around runStep is exactly what tests want for
// "no UI, no signal, just iterate" cases. Keeping both surfaces lets
// the wrapper add features without forcing every consumer to upgrade.
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

// stepResult is the per-step return value from runStep. Exactly one of
// Done/Continue is true:
//
//   - Done=true: the model emitted end_turn (or a recoverable
//     equivalent); FinalAnswer holds the assistant text. RunInteractionLoop
//     surfaces this to the caller.
//   - Continue=true: a tool_use turn ran; Proposal/Results carry what
//     just happened so the UI can render thoughts and outcomes.
//
// A step that errored returns (stepResult{}, err); the wrapper decides
// whether to stop or absorb.
type stepResult struct {
	Done        bool
	FinalAnswer string

	Continue bool
	Proposal ActionProposal
	Results  []ActionResult
}

// Run preserves the s08 contract: drive runStep up to MaxTurns and
// return the final answer (or an error). Existing tests call Run, so
// its behavior stays bug-for-bug compatible with s08.
func (l *Loop) Run(ctx context.Context, userPrompt string) (string, error) {
	l.ensureDefaults()

	registry := l.Components.Registry()
	directives := l.Components.Directives()
	preMessages := l.Components.Messages()
	schemas := registry.All()

	system := ""
	if oss, ok := l.Strategy.(*OneShotStrategy); ok {
		system = oss.BuildSystem(schemas, directives)
	}

	for turn := 0; turn < l.MaxTurns; turn++ {
		// MessageProvider injection happens only on the first turn (and
		// only when history is still empty); same rule as s08.
		injectPre := turn == 0 && len(*l.History) == 0 && len(preMessages) > 0

		out, err := l.runStep(ctx, runStepArgs{
			turn:       turn,
			schemas:    schemas,
			registry:   registry,
			directives: directives,
			system:     system,
			userPrompt: userPrompt,
			preMessages: func() []Message {
				if injectPre {
					return preMessages
				}
				return nil
			}(),
		})
		if err != nil {
			return "", err
		}
		if out.Done {
			return out.FinalAnswer, nil
		}
		// Continue: another turn.
	}
	return "", fmt.Errorf("loop exceeded MaxTurns=%d without end_turn", l.MaxTurns)
}

// runStepArgs bundles the per-step inputs so we don't grow runStep's
// signature into a parameter zoo as later sessions add fields.
type runStepArgs struct {
	turn        int
	schemas     []ToolSchema
	registry    *Registry
	directives  []string
	system      string
	userPrompt  string
	preMessages []Message
}

// runStep performs one think→act→observe cycle: Strategy.BuildPrompt →
// Provider.CreateMessage → Strategy.ParseResponse → Permissions.Check →
// Tool.Execute → record into History. Returns Done when the response
// stop_reason is end_turn (or stop_sequence); Continue otherwise.
//
// Errors are returned (not surfaced as ActionResults) when the issue is
// at the protocol layer — provider RPC failure, max_tokens truncation,
// or an unrecognized stop_reason with no JSON-fallback action. Tool
// failures (unknown tool, Tool.Execute error) become ActionResults with
// Status="error" so the model can see the failure on its next turn.
func (l *Loop) runStep(ctx context.Context, args runStepArgs) (stepResult, error) {
	messages := l.Strategy.BuildPrompt(*l.History, args.schemas, args.directives, args.userPrompt)

	if len(args.preMessages) > 0 {
		combined := make([]Message, 0, len(args.preMessages)+len(messages))
		combined = append(combined, args.preMessages...)
		combined = append(combined, messages...)
		messages = combined
	}

	resp, err := l.Provider.CreateMessage(ctx, CreateMessageRequest{
		System:   args.system,
		Messages: messages,
		Tools:    args.schemas,
	})
	if err != nil {
		return stepResult{}, fmt.Errorf("turn %d: %w", args.turn, err)
	}

	if l.Verbose {
		l.dumpAssistant(args.turn, resp)
	}

	switch resp.StopReason {
	case "end_turn", "stop_sequence":
		return stepResult{Done: true, FinalAnswer: extractText(resp.Content)}, nil

	case "tool_use":
		ep := &Episode{}
		l.History.Append(ep)

		proposal, perr := l.Strategy.ParseResponse(resp.Content)
		if perr == nil {
			ep.Actions = append(ep.Actions, proposal)
		} else if l.Verbose {
			fmt.Printf("[turn %d] ParseResponse error (continuing via direct dispatch): %v\n", args.turn, perr)
		}
		if l.Verbose && perr == nil {
			fmt.Printf("[turn %d] proposal: cmd=%s thoughts=%q\n",
				args.turn, proposal.Command, truncate(proposal.Thoughts, 120))
		}

		results, err := l.runTools(ctx, args.registry, resp.Content, args.turn)
		if err != nil {
			return stepResult{}, err
		}
		ep.Results = append(ep.Results, results...)
		return stepResult{Continue: true, Proposal: proposal, Results: results}, nil

	case "max_tokens":
		return stepResult{}, fmt.Errorf("hit max_tokens at turn %d (response was truncated)", args.turn)

	default:
		// JSON-fence fallback path.
		if proposal, perr := l.Strategy.ParseResponse(resp.Content); perr == nil && proposal.Command != "" {
			ep := &Episode{}
			l.History.Append(ep)
			ep.Actions = append(ep.Actions, proposal)

			if l.Verbose {
				fmt.Printf("[turn %d] JSON-fallback proposal: cmd=%s\n", args.turn, proposal.Command)
			}
			result, err := l.runFallbackTool(ctx, args.registry, proposal, args.turn)
			if err != nil {
				return stepResult{}, err
			}
			ep.Results = append(ep.Results, result)
			return stepResult{Continue: true, Proposal: proposal, Results: []ActionResult{result}}, nil
		}
		return stepResult{}, fmt.Errorf("unexpected stop_reason %q at turn %d", resp.StopReason, args.turn)
	}
}

// ensureDefaults wires up nil fields the same way s08's Run did at the
// top of its body. Pulled out so RunInteractionLoop can call the same
// default-init path without duplicating the lines.
func (l *Loop) ensureDefaults() {
	if l.Strategy == nil {
		l.Strategy = NewOneShotStrategy()
	}
	if l.History == nil {
		h := History{}
		l.History = &h
	}
	if l.Components == nil {
		l.Components = NewComponentBus()
	}
}

// runTools dispatches each tool_use block. Same s08 gate semantics:
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
