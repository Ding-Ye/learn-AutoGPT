---
title: "s09 · Continuous mode & UI feedback"
chapter: 9
slug: s09-continuous-mode
est_read_min: 13
---

# s09 · Continuous mode & UI feedback

> What this teaches: upgrade s01–s08's "single Run" into "N-cycle autonomy + clean Ctrl-C + UI feedback." Introduce `LoopOpts{Cycles, AskEachStep, OnInterrupt}` and a top-level wrapper `RunInteractionLoop(ctx, *Loop, UIProvider, LoopOpts)`: cycle-budget tracking, signal handling via `os/signal.Notify` (SIGINT → ctx.cancel), and a `UIProvider` interface (`RenderThought` / `RenderResult` / `Spinner`). Extract a `runStep` method from `Loop.Run` so the wrapper can drive one think→act→observe step at a time without duplicating logic; `Run` itself stays bug-for-bug compatible. Mirrors upstream `app/main.py:655-768`'s `run_interaction_loop`.

---

## Problem

By the end of s08, `Loop.Run(ctx, prompt)` is a bounded loop: run until end_turn or until MaxTurns. But real users want:

1. **N-cycle autonomy** — "run 5 steps then stop" (`-cycles 5`), no per-step approval needed;
2. **Clean Ctrl-C exit** — pressing the interrupt key shouldn't leave a half-written file in the workspace;
3. **Optional per-step approval** — `-ask-each-step` mode, where the operator vetoes individual steps before they execute;
4. **UI feedback** — show a spinner while the LLM is thinking, render thoughts and results once it returns.

s08's Loop has none of these. There's only a hard `MaxTurns`, no signal handling, no UI hooks.

AutoGPT classic answers this in `classic/original_autogpt/autogpt/app/main.py:655-768`'s `run_interaction_loop`:

```python
async def run_interaction_loop(agent, ui_provider=None):
    cycle_budget = cycles_remaining = _get_cycle_budget(
        app_config.continuous_mode, app_config.continuous_limit
    )
    spinner = Spinner("Thinking...", ...)
    stop_reason = None

    def graceful_agent_interrupt(signum, frame):
        nonlocal cycles_remaining, stop_reason
        # ... two-stage interrupt: first lower cycles to 1, second raise stop_reason

    signal.signal(signal.SIGINT, graceful_agent_interrupt)

    while cycles_remaining > 0:
        handle_stop_signal()
        async with ui_provider.show_spinner("Thinking..."):
            action_proposal = await agent.propose_action()
        await ui_provider.display_thoughts(...)
        result = await agent.execute(action_proposal)
        if result.status != "interrupted_by_human":
            cycles_remaining -= 1
        await ui_provider.display_result(...)
```

Three things going on: cycle budget, signal handling, Rich UI. Python uses `signal.signal` to register a global handler (which runs on the signal-delivery thread and mutates main-loop state via `nonlocal`), `async with` to manage the spinner context, and Rich for the spinner display.

s09 ports this to Go — but with idiomatic channel-based signals, `defer stop()` instead of async-with, and a deliberately minimal "[busy] ..." single-line spinner.

## Solution

```go
// New types:
type LoopOpts struct {
    Cycles      int                    // 0 = infinite (until ctx cancel)
    AskEachStep bool                   // ask the Asker before each step
    OnInterrupt func() error           // cleanup callback on ctx cancel
}

type UIProvider interface {
    RenderThought(text string)
    RenderResult(r ActionResult)
    Spinner(label string) func()       // returns a stop function
}

// ConsoleUI: minimal stderr output ("💭 ..." / "✓ ..." / "✗ ...")
type ConsoleUI struct {
    out io.Writer
}
func NewConsoleUI(out io.Writer) *ConsoleUI

// Top-level wrapper:
func RunInteractionLoop(
    ctx context.Context,
    l *Loop,
    ui UIProvider,
    opts LoopOpts,
) (string, error)
```

`Loop` itself is unchanged from s08, but a `runStep(ctx, args) (stepResult, error)` method is extracted from `Run` so `RunInteractionLoop` can reuse it. `Run` still exists and behaves exactly as in s08 — every s01–s08 test passes verbatim.

`main.go` gains two flags:

```bash
-cycles N         # 0 = infinite (Ctrl-C to exit)
-ask-each-step    # confirm every step via the Asker
```

and replaces `loop.Run(ctx, prompt)` with:

```go
SetUserPrompt(prompt)
final, err := RunInteractionLoop(ctx, loop, NewConsoleUI(os.Stderr), LoopOpts{
    Cycles:      *cycles,
    AskEachStep: *askEachStep,
})
```

## How It Works

```ascii-anim frames=4
┌────────────────────────────────────────────────────────────────────────┐
│ STARTUP                                                                  │
│   wctx, cancel := context.WithCancel(ctx)  ──── wrapper-local ctx       │
│   signal.Notify(sigCh, os.Interrupt)                                    │
│   go func() { <-sigCh; cancel() }                                       │
│        │                                                                 │
│        ▼                                                                 │
│ FOR EACH ITERATION                                                       │
│                                                                         │
│   ┌──── select { case <-wctx.Done(): return handleInterrupt() }        │
│   │      // early-exit check before any work                            │
│   │                                                                     │
│   ▼                                                                     │
│   if AskEachStep:                                                       │
│       if Asker.Ask("step") != Allow:                                    │
│           ui.RenderResult({"denied", "step skipped"})                   │
│           continue   ◀── no cycle decrement, back to select             │
│        │                                                                │
│        ▼                                                                │
│   stop := ui.Spinner("Thinking...")                                     │
│   out, err := loop.runStep(wctx, args)                                  │
│   stop()                                                                │
│        │                                                                │
│        ▼                                                                │
│   if err != nil:                                                        │
│       if wctx.Err() != nil: return handleInterrupt()                    │
│       return err                                                        │
│   if out.Done:                                                          │
│       ui.RenderThought(out.FinalAnswer)                                 │
│       return out.FinalAnswer, nil                                       │
│        │                                                                │
│        ▼                                                                │
│   ui.RenderThought(out.Proposal.Thoughts)                               │
│   for r := range out.Results:                                           │
│       ui.RenderResult(r)                                                │
│        │                                                                │
│        ▼                                                                │
│   if !anyInterrupted(out.Results) && Cycles > 0:                        │
│       cyclesLeft--                                                      │
│       if cyclesLeft <= 0: return                                        │
│        │                                                                │
│        └─── turn++ ────► back to select                                 │
└────────────────────────────────────────────────────────────────────────┘
```

### Signal handling: channel-based

```go
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
```

Four lines do everything Python's `signal.signal` + `nonlocal` does:

1. SIGINT arrives → write to channel
2. Background goroutine receives → `cancel()` the wrapper-local ctx
3. Main loop's next `select` sees `ctx.Done()` → routes to `handleInterrupt`
4. `defer signal.Stop` cleans up registration so a re-call doesn't leak handlers

**Why a wrapper-local ctx instead of cancelling the caller's ctx directly?** Because the caller may want to do more work after Ctrl-C (log a message, save history). A local ctx lets us "stop the bleeding right here" without polluting upstream.

### `runStep` extraction: reuse over duplication

```go
type stepResult struct {
    Done        bool                    // end_turn → terminate
    FinalAnswer string

    Continue bool                       // tool_use → continue
    Proposal ActionProposal
    Results  []ActionResult
}

func (l *Loop) runStep(ctx, args runStepArgs) (stepResult, error) {
    messages := l.Strategy.BuildPrompt(...)
    resp, err := l.Provider.CreateMessage(ctx, ...)
    // ... parse stop_reason, dispatch tools, gate permissions
    return stepResult{...}, nil
}
```

`Loop.Run` is now a thin wrapper:

```go
for turn := 0; turn < l.MaxTurns; turn++ {
    out, err := l.runStep(ctx, ...)
    if err != nil { return "", err }
    if out.Done { return out.FinalAnswer, nil }
}
```

`RunInteractionLoop` calls `runStep` directly, layering on cycle budgeting, signal handling, and UI — but **without duplicating** any strategy/parse/permission logic. That's the key: by extracting one step from the loop body, both loop drivers reuse a single step implementation.

### The load-bearing cycle-budget rule

```go
if !anyInterrupted(out.Results) && opts.Cycles > 0 {
    cyclesLeft--
    if cyclesLeft <= 0 { return "", nil }
}
```

**Only steps that actually ran successfully consume cycles** — exactly matching upstream's `if result.status != "interrupted_by_human": cycles_remaining -= 1`. A step the user vetoed via the Asker (synthetic "permission denied (user)" result) does NOT burn a cycle.

Why? Because the cycle budget represents "how many real units of work I'll pay for." A vetoed step doesn't represent work the agent did; the next propose_action should be a retry, not already in debt by one cycle.

### `UIProvider`: Spinner returns a stop function

Python uses `async with ui_provider.show_spinner(...)` for context management; Go has no `async with`. The s09 design:

```go
type UIProvider interface {
    Spinner(label string) func()  // returns a stop fn
}

// Caller:
stop := ui.Spinner("Thinking...")
// ... do work
stop()
```

`stop()` is idempotent (`sync.Once` guards the inner write) — error paths can `defer stop()` safely.

`ConsoleUI.Spinner` doesn't animate — it just writes a single `[busy] <label>...` line, and the stop fn writes CR + spaces to clear it. Pedagogy first: animation needs a ticker + goroutine + cancel, which would distract from the loop pedagogy.

### Three non-obvious things

1. **The signal-handler test simulates via ctx-cancel, not actual SIGINT**

   `interaction_loop_test.go` documents:

   ```go
   // Why simulate via ctx-cancel rather than syscall.Kill(self, SIGINT)?
   // Sending real SIGINT in a unit test is flaky on macOS (the
   // goroutine scheduler may not deliver before the assertion runs)
   // and dangerous on CI runners that interpret it as a build-aborted
   // signal. The signal handler in production wires SIGINT → cancel();
   // we exercise the cancel path directly. The signal-to-cancel hop
   // is one line of code and is itself trivially testable by inspection.
   ```

   Signal → cancel is a single line; verifying it via inspection is enough.

2. **`SetUserPrompt` is a package-level binding**

   `RunInteractionLoop`'s signature has only 4 parameters (ctx, loop, ui, opts), and the user prompt flows in via package-level `SetUserPrompt(prompt)`. Why? Adding a 5th positional parameter just for a string is heavier than this; folding it into LoopOpts blurs "runtime config" vs "input data." A comment notes that future sessions needing concurrent loops should promote this to a LoopOpts field or RunInteractionLoop parameter.

3. **`Loop.Run` didn't change**

   While extracting `runStep`, `Run` still calls it but keeps the original MaxTurns loop structure — every s01–s08 test is **byte-for-byte** copied into s09 and still passes. That's the s09 stability promise: "add a wrapper layer" cannot break the "single Run" surface.

## What Changed

```diff
 agents/s09-continuous-mode/
 ├── provider.go                       # verbatim from s08
 ├── provider_*.go / _test.go          # verbatim from s08
 ├── tools.go / tools_file.go          # verbatim from s08
 ├── workspace.go / _test.go           # verbatim from s08
 ├── registry.go / _test.go            # verbatim from s08
 ├── history.go / _test.go             # verbatim from s08
 ├── permissions.go / _test.go         # verbatim from s08
 ├── component*.go / _test.go          # verbatim from s08
 ├── strategy.go / _test.go            # verbatim from s08
 ├── loop.go                           # changed: extract runStep; Run still s08-compatible
 ├── loop_test.go                      # verbatim from s08
+├── interaction_loop.go               # NEW: RunInteractionLoop + signal handling + cycles
+├── interaction_loop_test.go          # NEW: 6 tests
+├── ui.go                             # NEW: UIProvider + ConsoleUI + NoopUI
+├── ui_test.go                        # NEW: 4 tests
 └── main.go                           # changed: add -cycles, -ask-each-step; call RunInteractionLoop
```

Type catalog additions:

```go
type LoopOpts struct {
    Cycles      int
    AskEachStep bool
    OnInterrupt func() error
}

type UIProvider interface {
    RenderThought(text string)
    RenderResult(r ActionResult)
    Spinner(label string) func()
}

type ConsoleUI struct{ out io.Writer }
type NoopUI struct{ /* test helper */ }

type stepResult struct {
    Done        bool
    FinalAnswer string
    Continue    bool
    Proposal    ActionProposal
    Results     []ActionResult
}
```

`Loop`'s fields are unchanged. New methods: `Loop.runStep`, `Loop.ensureDefaults`, `Loop.handleInterrupt`.

## Try It

```bash
cd agents/s09-continuous-mode

# 1. Default single Run (cycles=0 with an immediate end_turn prompt)
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "say hi"

# 2. Continuous mode: run 5 steps then stop
go run . -v -cycles 5 \
    "research the AutoGPT classic README and write a 3-line summary to summary.md"

# 3. Per-step approval
go run . -v -cycles 10 -ask-each-step -ask stdin \
    "fetch https://example.com and write the title to notes.md"
# Each step prints to stderr:
#   permission required: step(map[turn:0]) [y/N]:
# Type y to run, n to skip (no cycle burn).

# 4. Ctrl-C clean exit
go run . -v -cycles 0 \
    "this loops forever; Ctrl-C me"
# Press Ctrl-C; the binary prints "interaction loop interrupted" and exits.

# 5. Run all tests
go test -v ./...
```

Expected output (verbose mode):

```
[s09-continuous-mode] provider=anthropic ... cycles=5 ask-each-step=false
[busy] Thinking...
💭 I'll fetch the URL first to get the content.
✓ <!doctype html>...
[busy] Thinking...
💭 Now I'll write the summary to summary.md.
✓ wrote 142 bytes to summary.md
[busy] Thinking...
💭 Done.
Done.
```

Notice that ConsoleUI writes spinner / thoughts / results to stderr — stdout has only the final answer. That's so `s09-continuous-mode '...' | jq .` style pipes work cleanly.

### Cycle-budget subtleties

```bash
# Cycles=3, model end_turns on the first step → exits early, 2 cycles unspent
go run . -v -cycles 3 "say hi"
# stdout: hi

# Cycles=3, model runs 3 tool_use steps then would continue
go run . -v -cycles 3 "do 5 things"
# stderr: 3 spinners + 3 thought/result pairs
# stdout: (empty — no end_turn, just budget exhausted)
```

The second case returns `("", nil)` — empty final + nil error. The wrapper considers "ran out of budget" not an error, just an expected exit; callers should distinguish empty-final (budget) from non-nil error (ctx cancel).

## Upstream Source Reading

Full annotated reading at [`upstream-readings/s09-interaction-loop.py`](../../upstream-readings/s09-interaction-loop.py). This section excerpts only the load-bearing lines.

```upstream:classic/original_autogpt/autogpt/app/main.py:681-718
cycle_budget = cycles_remaining = _get_cycle_budget(
    app_config.continuous_mode, app_config.continuous_limit
)
spinner = Spinner("Thinking...", ...)
stop_reason = None

def graceful_agent_interrupt(signum, frame):
    nonlocal cycles_remaining, stop_reason
    if stop_reason:
        sys.exit()
    if cycles_remaining in [0, 1]:
        stop_reason = AgentTerminated("Interrupt signal received")
    else:
        cycles_remaining = 1   # ← graceful drain: let the running step finish

signal.signal(signal.SIGINT, graceful_agent_interrupt)

# [→ s09: Go uses signal.Notify(ch, SIGINT) + goroutine + cancel(),
#    avoiding nonlocal + multi-thread spinner-state coordination.]
```

```upstream:classic/original_autogpt/autogpt/app/main.py:780-820
result = await agent.execute(action_proposal)

# ★ THE LOAD-BEARING LINE: only decrement when not interrupted by human
if result.status != "interrupted_by_human":
    cycles_remaining -= 1

if result.status == "success":
    await ui_provider.display_result(str(result), is_error=False)
elif result.status == "error":
    await ui_provider.display_result(...)

# [→ s09: anyInterrupted(out.Results) check, semantically identical.
#    The Go ui.RenderResult(r) does the if/elif branch in one line.]
```

### Comparison highlights

- **`signal.signal` vs `signal.Notify`**: Python's registry is process-global; the second `signal.signal(SIGINT, ...)` call clobbers the first (libraries fighting over SIGINT is a frequent Python footgun). Go uses channels — multiple goroutines can share one signal channel with no "last writer wins" ambiguity.

- **Two-stage vs single-stage interrupt**: Upstream designs a "first Ctrl-C → graceful drain; second Ctrl-C → immediate exit" two-stage strategy. s09 simplifies to a single stage — one Ctrl-C cancels ctx + fires `OnInterrupt`. Pedagogy first; two-stage is doable in Go (atomic counter for signal arrivals) but would push the 60-line core loop to 90 lines.

- **Rich UI vs single-line ANSI**: Upstream's Rich library does animated spinners, color panels around thoughts, Markdown rendering for results. s09's ConsoleUI is plain ASCII prefixes (`💭` / `✓` / `✗`) + CR for clearing. Reason: Go's TUI libraries (charmbracelet/lipgloss, rivo/tview) are heavier in dependencies and build complexity than Python Rich — packing 200 lines of styling into a 60-line `ui.go` would drown out the "UIProvider is a seam" lesson. If you want a pretty TUI, swap ConsoleUI for a lipgloss impl; Loop and RunInteractionLoop don't need to change.

- **Spinner: async with vs stop fn**: Python's `async with ui_provider.show_spinner("..."): await ...` is context management. Go's `stop := ui.Spinner("..."); defer stop()` is the same semantics, the same cleanup guarantee, and the same idempotency (`sync.Once` makes multiple stop calls safe).

- **ActionResult.status vocabulary**: Upstream has `success` / `error` / `interrupted_by_human`. s09 uses `ok` / `error` / `denied`, plus `interrupted_by_human` is recognized but not currently produced — it's an extension point reserved for s10/Reflexion (a hook in AfterParse can mark a vetoed proposal as `interrupted_by_human`).

**Further reading**: [`upstream-readings/s09-interaction-loop.py`](../../upstream-readings/s09-interaction-loop.py) carries the full `run_interaction_loop` annotated + Go translation table + a "why ctx beats nonlocal" comparison. Then preview s10: by the end of s09 the agent runs continuously and exits cleanly; s10 introduces `Pipeline` (AfterParse / AfterExecute hooks) and `ReflexionStrategy` (let the LLM second-pass-evaluate its own proposal before executing) — AutoGPT's most distinctive "two-stage thinking" mechanism.

---

**Next up**: s10 introduces `AfterParseHook` / `AfterExecuteHook` and `Pipeline.RegisterHook`, decoupling cross-cutting concerns (validation, Reflexion, metrics) from the strategy. `ReflexionStrategy` wraps OneShot and registers a hook between propose and execute that issues a second LLM call for self-critique. Mirrors upstream `prompt_strategies/reflexion.py` + `agent/protocols.py::AfterParse`.
