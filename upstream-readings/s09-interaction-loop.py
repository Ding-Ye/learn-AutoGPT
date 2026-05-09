# Source: classic/original_autogpt/autogpt/app/main.py
# Upstream URL:
#   https://github.com/Significant-Gravitas/AutoGPT/blob/master/classic/original_autogpt/autogpt/app/main.py
# License: MIT (the classic/ subtree is MIT-licensed).
#
# This file is the annotated reading for s09 of learn-AutoGPT. It pulls
# the body of `run_interaction_loop` (lines 655-833 of the upstream
# file) and adds inline notes mapping each Python construct to the Go
# session-9 implementation. Comments tagged [→ s09] indicate which Go
# file/type teaches the corresponding upstream concept.
#
# What's worth studying here:
#
#   1. Signal handler wiring (graceful_agent_interrupt + signal.signal):
#      Python uses a process-global handler via signal.signal; Go uses
#      signal.Notify with a channel. The upstream design is harder to
#      reason about (the handler is invoked on a *signal-handling
#      thread* and mutates `nonlocal` state), and is what motivates the
#      cleaner channel-based pattern in interaction_loop.go.
#
#   2. The `cycles_remaining` cycle budget. Initialized from
#      `_get_cycle_budget(continuous_mode, continuous_limit)`, which
#      returns `math.inf` in continuous mode or the user's --cycles
#      cap. Decremented at line 817 — and ONLY when result.status !=
#      "interrupted_by_human" so an interrupted command doesn't burn
#      budget.
#
#   3. The two-stage interrupt: first SIGINT lowers cycles_remaining
#      to 1 (graceful drain); a second SIGINT raises stop_reason and
#      sys.exit. We DO NOT replicate the two-stage drain in s09 — pedagogy
#      over polish; one signal cancels the ctx and the wrapper exits
#      cleanly via the deferred OnInterrupt callback.
#
#   4. Rich UI calls: `ui_provider.show_spinner` (async context
#      manager) and `ui_provider.display_thoughts` / `display_result`.
#      Our Go UIProvider is plain function-call shaped: Spinner returns
#      a stop fn instead of being a context manager, since Go has no
#      `with`/`async with` and `defer stop()` is the idiomatic
#      replacement.


# ─────────────────────────────────────────────────────────────────────────
# main.py:655-688 · function header + UI provider + cycle budget setup
# ─────────────────────────────────────────────────────────────────────────

async def run_interaction_loop(
    agent: "Agent",
    ui_provider: Optional["UIProvider"] = None,
) -> None:
    """Run the main interaction loop for the agent."""
    # [→ s09: Go's RunInteractionLoop signature is
    #    `RunInteractionLoop(ctx, *Loop, UIProvider, LoopOpts) (string, error)`.
    #    The first parameter is ctx (Go's cancellation primitive — what
    #    Python achieves via `signal.signal` mutating nonlocal state).
    #    The agent has been split into `*Loop` (which holds Provider,
    #    Components, Strategy, History, Permissions, Asker) and the
    #    runtime config moves into `LoopOpts`.]
    app_config = agent.app_config
    ai_profile = agent.state.ai_profile
    logger = logging.getLogger(__name__)

    # Create default UI provider if not provided
    if ui_provider is None:
        ui_provider = create_ui_provider(
            plain_output=app_config.logging.plain_console_output,
        )
    # [→ s09: ui.go's `NewConsoleUI(io.Writer)`. We require the caller
    #    to construct the UI; we don't auto-default because production
    #    binaries (main.go) want stderr output and tests want a NoopUI
    #    record. A nil UIProvider would be a programming error.]

    cycle_budget = cycles_remaining = _get_cycle_budget(
        app_config.continuous_mode, app_config.continuous_limit
    )
    # [→ s09: LoopOpts.Cycles. `_get_cycle_budget` returns math.inf in
    #    continuous mode; we model "infinite" as Cycles=0 (zero-value
    #    means feature off → treat as no cap) which is more idiomatic
    #    in Go than a sentinel like math.MaxInt.]

    spinner = Spinner(
        "Thinking...", plain_output=app_config.logging.plain_console_output
    )
    stop_reason = None


    # ─────────────────────────────────────────────────────────────────────
    # main.py:690-715 · the signal handler
    # ─────────────────────────────────────────────────────────────────────

    def graceful_agent_interrupt(signum: int, frame: Optional[FrameType]) -> None:
        nonlocal cycles_remaining, stop_reason
        if stop_reason:
            logger.error("Quitting immediately...")
            sys.exit()
        if cycles_remaining in [0, 1]:
            logger.warning("Interrupt signal received: shutting down gracefully.")
            logger.warning("Press Ctrl+C again if you want to stop AutoGPT immediately.")
            stop_reason = AgentTerminated("Interrupt signal received")
        else:
            restart_spinner = spinner.running
            if spinner.running:
                spinner.stop()
            logger.error("Interrupt signal received: stopping continuous command execution.")
            cycles_remaining = 1
            if restart_spinner:
                spinner.start()
        # [→ s09: interaction_loop.go's signal goroutine. Notice the
        #    Python handler mutates two nonlocal vars and toggles the
        #    spinner's running state — three pieces of shared state
        #    across two threads (main + signal-delivery). The Go pattern
        #    is far simpler: signal arrives on a channel, a goroutine
        #    calls cancel() on the wrapper-local ctx, and the next
        #    iteration of the for-loop sees ctx.Err() and exits via
        #    handleInterrupt(). Single point of mutation, no nonlocal,
        #    no spinner-state coordination across threads.]

    def handle_stop_signal() -> None:
        if stop_reason:
            raise stop_reason
        # [→ s09: implicit in `select { case <-ctx.Done() }` at the top
        #    of every for-loop iteration. We don't raise; we return a
        #    formatted error.]

    # Register the handler. signal.signal is process-global — only ONE
    # handler may be registered for a given signal at a time. If two
    # libraries both call signal.signal(SIGINT, ...) the second one
    # wins, which is a frequent footgun in Python.
    signal.signal(signal.SIGINT, graceful_agent_interrupt)
    # [→ s09: Go's signal.Notify(ch, os.Interrupt) is channel-based.
    #    Multiple goroutines can notify on the same channel with no
    #    "last writer wins" ambiguity. signal.Stop(ch) (deferred in
    #    interaction_loop.go) cleans up the registration so a re-run
    #    of the function doesn't leak handlers.]


    # ─────────────────────────────────────────────────────────────────────
    # main.py:720-770 · the main loop body — plan / display / execute
    # ─────────────────────────────────────────────────────────────────────

    consecutive_failures = 0

    while cycles_remaining > 0:
        # [→ s09: `for {}` with explicit ctx-done check at top. Cycles=0
        #    in Go means infinite; we don't pre-check `cyclesLeft > 0`
        #    because the Cycles=0 case must run forever.]
        logger.debug(f"Cycle budget: {cycle_budget}; remaining: {cycles_remaining}")

        # Plan
        handle_stop_signal()
        # [→ s09: select { case <-wctx.Done(): return l.handleInterrupt(...) }]

        if not (_ep := agent.event_history.current_episode) or _ep.result:
            async with ui_provider.show_spinner("Thinking..."):
                # [→ s09: ui.Spinner("Thinking...") returns stop fn;
                #    `defer stop()` is the Go equivalent of `async with`.]
                try:
                    action_proposal = await agent.propose_action()
                    # [→ s09: l.runStep(ctx, args) — handles Strategy
                    #    BuildPrompt + Provider.CreateMessage + Strategy
                    #    ParseResponse in one call.]
                except InvalidAgentResponseError as e:
                    logger.warning(f"The agent's thoughts could not be parsed: {e}")
                    consecutive_failures += 1
                    if consecutive_failures >= 3:
                        raise AgentTerminated(
                            f"The agent failed to output valid thoughts {consecutive_failures} times in a row."
                        )
                    continue
        else:
            action_proposal = _ep.action

        consecutive_failures = 0

        # Update User
        await ui_provider.display_thoughts(
            ai_name=ai_profile.ai_name,
            thoughts=action_proposal.thoughts,
            speak_mode=app_config.tts_config.speak_mode,
        )
        # [→ s09: ui.RenderThought(out.Proposal.Thoughts). We drop the
        #    speak_mode / tts plumbing — that's a cosmetic feature
        #    orthogonal to the loop pedagogy.]

        handle_stop_signal()

        # Execute Command
        if not action_proposal.use_tool:
            continue
        # [→ s09: stepResult.Done==true (from end_turn) returns; the
        #    Go path is symmetric. The "no tool" branch never decrements
        #    cycles_remaining; same in s09 — Done bypasses the cycle
        #    accounting.]

        handle_stop_signal()


    # ─────────────────────────────────────────────────────────────────────
    # main.py:780-833 · execute, handle finish, decrement cycles
    # ─────────────────────────────────────────────────────────────────────

        try:
            result = await agent.execute(action_proposal)
        except AgentFinished as e:
            # [→ s09: not exposed; the Go runStep treats end_turn as
            #    Done and returns the final answer. We don't model an
            #    explicit `finish` tool because the model's natural
            #    end_turn is enough.]
            ...

        # ★ THE LOAD-BEARING LINE: cycle_remaining only decrements when
        #   the step was NOT interrupted by the human via the permission
        #   gate. This is what makes "operator vetoes a step → no budget
        #   wasted" feel right.
        if result.status != "interrupted_by_human":
            cycles_remaining -= 1
        # [→ s09: interaction_loop.go's anyInterrupted() check before
        #    `cyclesLeft--`. Same exact semantics: Deny via Asker
        #    surfaces as Status="interrupted_by_human" (or in our case
        #    "denied"; we generalized) and the cycle survives.]

        if result.status == "interrupted_by_human" and result.feedback:
            await ui_provider.display_message(
                f"Feedback provided: {result.feedback}",
                title="USER:",
            )

        if result.status == "success":
            await ui_provider.display_result(str(result), is_error=False)
        elif result.status == "error":
            error_msg = (
                f"Command {action_proposal.use_tool.name} returned an error: "
                f"{result.error or result.reason}"
            )
            await ui_provider.display_result(error_msg, is_error=True)
        # [→ s09: ui.RenderResult(r ActionResult). Go's switch on
        #    r.Status is one line and dispatches to "✓"/"✗" prefixes.]


# ─────────────────────────────────────────────────────────────────────────
# Annotated diff: line counts
# ─────────────────────────────────────────────────────────────────────────
#
# Upstream run_interaction_loop:    ~180 lines (655-833 + helpers)
# Go RunInteractionLoop:             ~150 lines (interaction_loop.go)
# UIProvider iface + ConsoleUI:      ~90 lines (ui.go, including comments)
#
# Most of the line savings come from:
#
#   - ctx replaces nonlocal-mutating signal handler (saves ~25 lines)
#   - no Rich, no spinner state machine, no async context (saves ~20)
#   - no AgentFinished / restart-task path (saves ~30 lines)
#   - no consecutive_failures retry counter (saves ~15 lines, deferred
#     to s10 where Reflexion adds it more cleanly via AfterParseHook)
#
# What we KEEP intentionally:
#
#   - The cycles_remaining decrement-only-on-success rule (the load-
#     bearing semantic).
#   - The signal-cancels-ctx hop (the cleanest Go pattern).
#   - The spinner around the Provider call (the seam where future
#     "Reflexion second pass" timing measurements would live).
#   - The thought-before-result UI ordering (the s09 ui_test.go pins it).
