package main

import (
	"context"
	"fmt"
	"strings"
)

// Loop is the agent loop. The s10 redesign adds a `Pipeline` field so
// the runStep path can fire AfterParse hooks (between ParseResponse and
// the permission gate) and AfterExecute hooks (after each tool result).
// Pipeline is OPTIONAL — a nil pipeline preserves s09's behavior
// exactly, so every test inherited from s09 still passes without
// modification.
//
// What changed vs s09:
//
//   - new `Pipeline *Pipeline` field. Hooks are decoupled cross-cutting
//     concerns: Reflexion, validation, redaction, audit logging — all
//     things that observe (and may revise) proposals/results without
//     belonging in any single Strategy class.
//   - runStep now invokes `Pipeline.RunAfterParse(&proposal)` after
//     parsing each tool_use turn (and after the JSON-fallback parse),
//     and `Pipeline.RunAfterExecute(&result)` after each tool result.
//   - hook errors halt the step and propagate to the Loop's caller —
//     this is what lets a "kill switch" hook abort cleanly.
//
// Per dossier: pipelines + reflexion are the most architecturally
// distinctive AutoGPT mechanism. The Loop's fields stay small (one new
// optional field) precisely BECAUSE Pipeline absorbs everything that
// would otherwise bloat the Loop or every Strategy variant.
type Loop struct {
	Provider    Provider
	Components  *ComponentBus
	Strategy    PromptStrategy
	History     *History
	Permissions *Permissions
	Asker       Asker
	Pipeline    *Pipeline // s10 NEW: AfterParse / AfterExecute hooks; nil = no hooks
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

// Run preserves the s09 contract: drive runStep up to MaxTurns and
// return the final answer (or an error). Existing tests call Run, so
// its behavior stays bug-for-bug compatible with s09.
func (l *Loop) Run(ctx context.Context, userPrompt string) (string, error) {
	l.ensureDefaults()

	registry := l.Components.Registry()
	directives := l.Components.Directives()
	preMessages := l.Components.Messages()
	schemas := registry.All()

	system := ""
	if oss, ok := l.Strategy.(*OneShotStrategy); ok {
		system = oss.BuildSystem(schemas, directives)
	} else if r, ok := l.Strategy.(*ReflexionStrategy); ok {
		// Reflexion delegates to its base strategy; if that base is a
		// OneShotStrategy, we still want the system prompt rendered.
		if oss, ok := r.base.(*OneShotStrategy); ok {
			system = oss.BuildSystem(schemas, directives)
		}
	}

	for turn := 0; turn < l.MaxTurns; turn++ {
		// MessageProvider injection happens only on the first turn (and
		// only when history is still empty); same rule as s09.
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
// Provider.CreateMessage → Strategy.ParseResponse → Pipeline.AfterParse →
// Permissions.Check → Tool.Execute → Pipeline.AfterExecute → record
// into History.
//
// s10 INSERTION POINTS: AfterParse runs immediately after ParseResponse,
// BEFORE the permission gate, so a hook (e.g. Reflexion) that revises
// the proposal sees its rewrite go through the same gate the original
// would have. AfterExecute runs once per result, AFTER tool dispatch
// but BEFORE the result is appended to the episode, so a hook that
// truncates/redacts output flows into history.
//
// Errors are returned (not surfaced as ActionResults) when the issue is
// at the protocol layer — provider RPC failure, max_tokens truncation,
// pipeline hook failure, or an unrecognized stop_reason with no JSON-
// fallback action. Tool failures (unknown tool, Tool.Execute error)
// become ActionResults with Status="error" so the model can see the
// failure on its next turn.
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
			// s10 NEW: AfterParse hook runs against the parsed proposal
			// before any permissions/dispatch happens. This is where
			// Reflexion intervenes to rewrite unsound actions.
			if hookErr := l.Pipeline.RunAfterParse(ctx, &proposal); hookErr != nil {
				return stepResult{}, fmt.Errorf("turn %d: %w", args.turn, hookErr)
			}
			ep.Actions = append(ep.Actions, proposal)
		} else if l.Verbose {
			fmt.Printf("[turn %d] ParseResponse error (continuing via direct dispatch): %v\n", args.turn, perr)
		}
		if l.Verbose && perr == nil {
			fmt.Printf("[turn %d] proposal: cmd=%s thoughts=%q\n",
				args.turn, proposal.Command, truncate(proposal.Thoughts, 120))
		}

		results, err := l.runTools(ctx, args.registry, resp.Content, proposal, args.turn)
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

			// s10 NEW: AfterParse hook runs on the JSON-fallback path
			// too. Hooks should not care which parse path produced the
			// proposal; they see the same shape either way.
			if hookErr := l.Pipeline.RunAfterParse(ctx, &proposal); hookErr != nil {
				return stepResult{}, fmt.Errorf("turn %d (fallback): %w", args.turn, hookErr)
			}
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

// ensureDefaults wires up nil fields the same way s09's Run did at the
// top of its body.
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
	// l.Pipeline stays nil if not set — RunAfterParse / RunAfterExecute
	// are nil-safe and act as no-ops.
}

// runTools dispatches each tool_use block. Same s09 gate semantics:
// permission check → ask if needed → execute. s10 ADDITION: after each
// successful (or errored) result, the Pipeline.AfterExecute hook fires
// so post-processing can mutate the result before the caller appends
// it to history.
//
// We accept the parsed `proposal` so a hook that rewrote the command
// (Reflexion's pattern) is honored: dispatch uses proposal.Command and
// proposal.Args rather than the raw block.Name / block.Input. When
// reflexion is OFF or the proposal is empty, we fall back to the
// block's original name/input — preserving s09 behavior bug-for-bug.
func (l *Loop) runTools(ctx context.Context, registry *Registry, content []ContentBlock, proposal ActionProposal, turn int) ([]ActionResult, error) {
	var results []ActionResult
	for _, block := range content {
		if block.Type != "tool_use" {
			continue
		}

		// Resolve which (name, args) to dispatch. If the parsed proposal
		// has a command set, prefer it (reflexion may have rewritten
		// it). Otherwise use the raw tool_use block.
		dispatchName := block.Name
		dispatchArgs := block.Input
		if proposal.Command != "" {
			dispatchName = proposal.Command
			if proposal.Args != nil {
				dispatchArgs = proposal.Args
			}
		}

		// Permission gate. nil Permissions → no gate.
		if l.Permissions != nil {
			decision := l.Permissions.Check(dispatchName, dispatchArgs)
			if l.Verbose {
				fmt.Printf("[turn %d] permission check: %s → %v\n", turn, dispatchName, decision)
			}
			switch decision {
			case Allow:
				// proceed below
			case Deny:
				res := ActionResult{
					Status: "denied",
					Output: fmt.Sprintf("permission denied: %s", dispatchName),
				}
				if hookErr := l.Pipeline.RunAfterExecute(ctx, &res); hookErr != nil {
					return nil, fmt.Errorf("turn %d: %w", turn, hookErr)
				}
				results = append(results, res)
				continue
			case Ask:
				if l.Asker == nil {
					return nil, fmt.Errorf("permission required for %s but no Asker configured", dispatchName)
				}
				ans := l.Asker.Ask(dispatchName, dispatchArgs)
				if l.Verbose {
					fmt.Printf("[turn %d] asker replied: %v\n", turn, ans)
				}
				if ans != Allow {
					res := ActionResult{
						Status: "denied",
						Output: fmt.Sprintf("permission denied (user): %s", dispatchName),
					}
					if hookErr := l.Pipeline.RunAfterExecute(ctx, &res); hookErr != nil {
						return nil, fmt.Errorf("turn %d: %w", turn, hookErr)
					}
					results = append(results, res)
					continue
				}
			}
		}

		tool, ok := registry.Lookup(dispatchName)
		if !ok {
			res := ActionResult{
				Status: "error",
				Output: fmt.Sprintf("unknown tool: %q", dispatchName),
			}
			if hookErr := l.Pipeline.RunAfterExecute(ctx, &res); hookErr != nil {
				return nil, fmt.Errorf("turn %d: %w", turn, hookErr)
			}
			results = append(results, res)
			continue
		}
		if l.Verbose {
			fmt.Printf("[turn %d] -> %s %v\n", turn, dispatchName, dispatchArgs)
		}
		out, err := tool.Execute(ctx, dispatchArgs)
		var res ActionResult
		if err != nil {
			if l.Verbose {
				fmt.Printf("[turn %d] <- error: %v\n", turn, err)
			}
			res = ActionResult{Status: "error", Output: err.Error()}
		} else {
			if l.Verbose {
				fmt.Printf("[turn %d] <- %s\n", turn, truncate(out, 240))
			}
			res = ActionResult{Status: "ok", Output: out}
		}

		// s10 NEW: AfterExecute hook fires for every result (success
		// or error). Hook errors halt the step.
		if hookErr := l.Pipeline.RunAfterExecute(ctx, &res); hookErr != nil {
			return nil, fmt.Errorf("turn %d: %w", turn, hookErr)
		}
		results = append(results, res)
	}
	return results, nil
}

// runFallbackTool handles the JSON-fence path. Same gate logic. s10
// ADDITION: AfterExecute fires on the synthesized result.
func (l *Loop) runFallbackTool(ctx context.Context, registry *Registry, p ActionProposal, turn int) (ActionResult, error) {
	if l.Permissions != nil {
		decision := l.Permissions.Check(p.Command, p.Args)
		switch decision {
		case Allow:
			// proceed
		case Deny:
			res := ActionResult{Status: "denied", Output: fmt.Sprintf("permission denied: %s", p.Command)}
			if err := l.Pipeline.RunAfterExecute(ctx, &res); err != nil {
				return ActionResult{}, fmt.Errorf("turn %d: %w", turn, err)
			}
			return res, nil
		case Ask:
			if l.Asker == nil {
				return ActionResult{}, fmt.Errorf("permission required for %s but no Asker configured", p.Command)
			}
			if l.Asker.Ask(p.Command, p.Args) != Allow {
				res := ActionResult{Status: "denied", Output: fmt.Sprintf("permission denied (user): %s", p.Command)}
				if err := l.Pipeline.RunAfterExecute(ctx, &res); err != nil {
					return ActionResult{}, fmt.Errorf("turn %d: %w", turn, err)
				}
				return res, nil
			}
		}
	}
	tool, ok := registry.Lookup(p.Command)
	if !ok {
		res := ActionResult{Status: "error", Output: fmt.Sprintf("unknown tool: %q", p.Command)}
		if err := l.Pipeline.RunAfterExecute(ctx, &res); err != nil {
			return ActionResult{}, fmt.Errorf("turn %d: %w", turn, err)
		}
		return res, nil
	}
	out, err := tool.Execute(ctx, p.Args)
	var res ActionResult
	if err != nil {
		res = ActionResult{Status: "error", Output: err.Error()}
	} else {
		res = ActionResult{Status: "ok", Output: out}
	}
	if hookErr := l.Pipeline.RunAfterExecute(ctx, &res); hookErr != nil {
		return ActionResult{}, fmt.Errorf("turn %d: %w", turn, hookErr)
	}
	_ = turn
	return res, nil
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
