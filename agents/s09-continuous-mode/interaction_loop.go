// interaction_loop.go — the continuous-mode wrapper around Loop.
//
// AutoGPT classic's `run_interaction_loop` (`app/main.py:655-768`) wraps
// the per-step `propose_action → execute_command` sequence with three
// pieces of cycle bookkeeping:
//
//  1. `cycles_remaining` — defaults to `math.inf`; users may pass `-c N`
//     to cap autonomy. Each successful execute decrements; when it hits
//     0 the loop exits and asks the user for more cycles.
//  2. `signal.signal(SIGINT, handle_stop_signal)` — a Ctrl-C handler
//     that flips a global flag the loop checks each iteration so the
//     workspace doesn't end up half-written.
//  3. Rich-based UI — spinners around the LLM call, color panels around
//     the result.
//
// Our Go port collapses these into:
//
//   - `LoopOpts{Cycles, AskEachStep, OnInterrupt}` — Cycles=0 means
//     infinite (test uses ctx-cancel to break), AskEachStep gates each
//     step on the Loop's existing Asker, OnInterrupt is the cleanup
//     callback.
//   - `signal.Notify` (Go's channel-based variant) wired to cancel the
//     ctx; `select { case <-ctx.Done(): }` checks happen at the top of
//     each iteration.
//   - `UIProvider` interface (ui.go) for RenderThought/RenderResult/
//     Spinner; production code uses ConsoleUI, tests use NoopUI.
//
// The split between `Loop.Run` (the s08-compatible bounded loop) and
// `RunInteractionLoop` (the s09 cycle-budget wrapper) is intentional:
// `Run` is a stable contract every prior session's tests rely on, and
// `RunInteractionLoop` adds features without touching that surface.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// LoopOpts controls continuous-mode behavior. Zero-valued fields mean
// "feature off": `Cycles=0` is infinite, `AskEachStep=false` skips
// per-step gating, `OnInterrupt=nil` no-ops on ctx cancel.
type LoopOpts struct {
	// Cycles caps the number of tool-use steps the wrapper will run
	// before exiting. 0 means infinite — the wrapper relies on ctx
	// cancel (or end_turn from the model) to stop. The test suite
	// exercises both Cycles=N (count-bounded) and Cycles=0
	// (ctx-cancel-bounded) paths.
	//
	// Per AutoGPT upstream: cycles decrement only when a step actually
	// executes; "interrupted_by_human" results don't count. Our parity
	// rule: if a step's first result has Status "interrupted_by_human",
	// the cycle does NOT decrement.
	Cycles int

	// AskEachStep gates each step on Loop.Asker — between the LLM call
	// (think) and tool dispatch (act/observe), the wrapper invokes
	// Asker.Ask("step", ...). On Deny, the step is skipped (a synthetic
	// "denied by user" result is appended to history and rendered to UI)
	// and the cycle counter does NOT decrement.
	//
	// AutoGPT calls this "AUTHORISE COMMANDS automatically" mode in the
	// `--continuous` flag's complement. Our default is OFF (false), so a
	// `Cycles=N` call runs N steps without prompting unless the operator
	// opts in.
	AskEachStep bool

	// OnInterrupt fires when the wrapper exits via ctx cancellation.
	// Use it to flush state, persist history, or close file descriptors
	// before the binary returns. Errors from the callback are joined
	// into the wrapper's return error.
	OnInterrupt func() error
}

// RunInteractionLoop drives `Loop` step-by-step with cycle budgeting,
// signal handling, and per-step UI feedback. It is the s09 entry point
// for continuous-mode binaries; `Loop.Run` remains the simpler
// "bounded, no UI, no signals" path.
//
// Lifecycle:
//
//  1. Set up signal.Notify(ch, SIGINT, SIGTERM); spawn a goroutine that
//     cancels a wrapper-local ctx on signal. `defer signal.Stop` undoes
//     the registration before return so we don't leak handlers.
//  2. Default-init Loop fields (the same call Loop.Run makes); compute
//     bus-derived registry/directives/preMessages/schemas/system.
//  3. Loop:
//     a. select { case <-ctx.Done(): break } — exit cleanly on signal.
//     b. If AskEachStep, ask Asker — Deny skips this step.
//     c. ui.Spinner("Thinking..."); call runStep; stop spinner.
//     d. If Done, render thought and exit with the final answer.
//     e. Render thought + each result.
//     f. Decrement cycle counter unless any result was
//        "interrupted_by_human".
//     g. If Cycles>0 and counter reaches 0, exit normally.
//
// The function returns the final answer (or "" on cycle-budget exit /
// ctx cancel) and any non-nil error encountered.
func RunInteractionLoop(ctx context.Context, l *Loop, ui UIProvider, opts LoopOpts) (string, error) {
	// Wrapper-local ctx that we cancel on signal. The caller's ctx is
	// the parent so caller cancel still propagates.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Signal handler. We listen to SIGINT (Ctrl-C) and SIGTERM (clean
	// shutdown). On either, cancel the ctx; the loop's ctx-done check
	// at the top of each iteration handles the rest. Production might
	// want a "second SIGINT = hard kill" pattern; we keep it single-
	// shot for pedagogy.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-wctx.Done():
		}
	}()

	l.ensureDefaults()
	registry := l.Components.Registry()
	directives := l.Components.Directives()
	preMessages := l.Components.Messages()
	schemas := registry.All()

	system := ""
	if oss, ok := l.Strategy.(*OneShotStrategy); ok {
		system = oss.BuildSystem(schemas, directives)
	}

	cyclesLeft := opts.Cycles // 0 → infinite
	turn := 0

	for {
		// Check for cancellation BEFORE doing any work — a fast Ctrl-C
		// after a step should not waste a Provider call.
		select {
		case <-wctx.Done():
			return "", l.handleInterrupt(wctx, opts)
		default:
		}

		// Per-step approval gate. The wrapper-level ask is independent
		// of the Loop's per-tool permission gate: AskEachStep gives the
		// operator veto authority over EVERY step regardless of
		// allow/deny rules. Implementation: invoke Asker.Ask with
		// cmd="step" and an empty args map.
		if opts.AskEachStep {
			if l.Asker == nil {
				return "", fmt.Errorf("LoopOpts.AskEachStep requires Loop.Asker to be set")
			}
			if l.Asker.Ask("step", map[string]interface{}{"turn": turn}) != Allow {
				ui.RenderResult(ActionResult{
					Status: "denied",
					Output: "step skipped by operator",
				})
				// No decrement; not counted as a cycle.
				continue
			}
		}

		// Spinner during the LLM call. The stop fn is idempotent so
		// any error path can call it safely.
		stop := ui.Spinner("Thinking...")

		injectPre := turn == 0 && len(*l.History) == 0 && len(preMessages) > 0
		args := runStepArgs{
			turn:       turn,
			schemas:    schemas,
			registry:   registry,
			directives: directives,
			system:     system,
			userPrompt: pendingUserPrompt(opts, turn),
		}
		if injectPre {
			args.preMessages = preMessages
		}

		out, err := l.runStep(wctx, args)
		stop()

		if err != nil {
			// Distinguish ctx cancellation (clean exit) from other
			// errors (loud exit). Provider RPCs surface ctx errors
			// wrapped, so we check Err() on the ctx instead of the
			// returned error.
			if wctx.Err() != nil {
				return "", l.handleInterrupt(wctx, opts)
			}
			return "", err
		}

		if out.Done {
			ui.RenderThought(out.FinalAnswer)
			return out.FinalAnswer, nil
		}

		// Step completed. Render thoughts and each result in order so
		// the operator sees "what did the model think → what happened
		// when we ran it" — strictly thought-before-result so a UI
		// transcript reads top-down.
		ui.RenderThought(out.Proposal.Thoughts)
		for _, r := range out.Results {
			ui.RenderResult(r)
		}

		// Cycle accounting: if any result was interrupted_by_human,
		// don't decrement (matches AutoGPT upstream's
		// `cycles_remaining -= 1 if not interrupted_by_human`).
		if !anyInterrupted(out.Results) && opts.Cycles > 0 {
			cyclesLeft--
			if cyclesLeft <= 0 {
				return "", nil
			}
		}

		turn++

		// Belt-and-suspenders: a runaway provider might never return
		// end_turn. We don't enforce MaxTurns here (Cycles is the user-
		// facing cap), but a safety check on Loop.MaxTurns when it's
		// set keeps a misconfigured agent from hammering the API
		// forever.
		if l.MaxTurns > 0 && turn >= l.MaxTurns {
			return "", fmt.Errorf("interaction loop exceeded MaxTurns=%d", l.MaxTurns)
		}
	}
}

// pendingUserPrompt returns the user's task on turn 0 and "" thereafter —
// after the first turn the model gets context exclusively from history
// rendering. This matches Loop.Run's behavior (the s08 inherited test
// `TestLoop_HistoryGrowsAfterEachTurn` relies on this).
//
// Why a function instead of inlining? Future sessions may want the
// wrapper to pull a fresh prompt on each turn (interactive REPL). The
// indirection makes that one-line swap.
func pendingUserPrompt(_ LoopOpts, turn int) string {
	if turn == 0 {
		return runtimeUserPrompt
	}
	// Turns >0 inherit history; the strategy always appends an empty
	// user message to keep the conversational shape, but the model is
	// expected to derive its next move from history.RenderMessages
	// and the most-recent tool_result. We pass an empty string here
	// rather than re-pasting the original task, mirroring Loop.Run's
	// per-turn build path.
	return ""
}

// runtimeUserPrompt is a package-level binding so tests/main can pass
// the original prompt into RunInteractionLoop without growing the
// signature. It's set at the top of RunInteractionLoop's caller (main
// or test) before kicking off the loop.
//
// Why not a wrapper field? The wrapper is a function, not a struct.
// Why not a parameter? Adding a 5th positional parameter to a public
// API just to thread one string is heavier than this single var.
// The trade-off is "package-level state" vs "growing public surface" —
// for s09's pedagogical scope the former is the right call.
//
// NOTE: not safe for concurrent RunInteractionLoop invocations; tests
// that run in parallel must orchestrate access. None do.
var runtimeUserPrompt string

// SetUserPrompt sets the prompt that runStep will see on turn 0. Tests
// and `main` call this before RunInteractionLoop. If a future session
// needs concurrent loops, this becomes a parameter of RunInteractionLoop
// or a field on LoopOpts.
func SetUserPrompt(prompt string) { runtimeUserPrompt = prompt }

// anyInterrupted returns true if any result has Status=="interrupted_by_human".
// Matches the AutoGPT upstream check for whether the cycle counter
// should decrement.
func anyInterrupted(results []ActionResult) bool {
	for _, r := range results {
		if r.Status == "interrupted_by_human" {
			return true
		}
	}
	return false
}

// handleInterrupt fires the OnInterrupt callback (if set) and returns a
// formatted error so the caller knows the loop exited via signal/ctx
// cancel rather than naturally. Production binaries log this and exit
// nonzero so shell scripts can detect "user aborted".
func (l *Loop) handleInterrupt(_ context.Context, opts LoopOpts) error {
	if opts.OnInterrupt != nil {
		if err := opts.OnInterrupt(); err != nil {
			return fmt.Errorf("interaction loop interrupted; OnInterrupt also failed: %w", err)
		}
	}
	return fmt.Errorf("interaction loop interrupted")
}
