---
title: "s01 · Minimal think→act→observe loop"
chapter: 1
slug: s01-minimal-loop
est_read_min: 12
---

# s01 · Minimal think→act→observe loop

> What this teaches: a ~250-line Go reproduction of the **smallest recognizable form** of AutoGPT classic's `run_interaction_loop`. The mechanism is **agent_loop** — the kernel that every later chapter extends.

---

## Problem

AutoGPT classic's entry point is `run_interaction_loop` in `classic/original_autogpt/autogpt/app/main.py`: a `while cycles_remaining > 0:` wrapped around `propose_action() → execute()`, padded with signal handlers, UI rendering, cycle budgets, permission checks, and four `stop_reason` recovery branches. Reading those 100+ lines cold makes it hard to see *what is the skeleton* and *what is decoration*.

This chapter strips off all the decoration and leaves the skeleton runnable on its own. We pull the core think→act→observe — the LLM call, tool dispatch, tool_result feedback, end_turn termination — and drop everything else: the cycle budget (deferred to s09), permissions (s07), history (s05), components (s08), UI provider (s09). After running s01, the reader has a minimal mental model for *why an agent can decide for itself*.

## Solution

Decompose the loop into **three orthogonal interfaces**: `Provider` (the LLM call), `Tool` (a capability), and `Loop` (the control flow). Between them, the internal data shape is **Anthropic's content-block protocol** — `text` / `tool_use` / `tool_result` is a clean tagged union, easier to teach than OpenAI's "tool_calls is a field on the assistant message; tool results are a separate role" patchwork.

Key design decisions:

1. **Protocol shape vs vendor wire shape are separate** — internally the loop speaks Anthropic blocks; `OpenAIProvider` translates at the boundary. So `Loop` never knows which vendor it's talking to, and s03 can add backends without touching `loop.go`.
2. **MaxTurns is a hard cap, not a cycle budget** — upstream's `cycles_remaining` has branches like "human interrupt doesn't decrement". We simplify to "if N turns pass without end_turn, surface an error". The conditional-decrement complexity returns in s09.
3. **Only EchoTool, no BashTool** — a shell tool means `rm -rf .` is one prompt-injection away. Sandboxing is s06's job (`Workspace`). s01's job is "see the loop clearly", not "do useful work".

## How It Works

```ascii-anim frames=2
┌────────────────────────────────────────────────────────────┐
│   user prompt                                              │
│        │                                                   │
│        ▼                                                   │
│   ┌─────────┐  CreateMessage  ┌────────────┐              │
│   │  Loop   │ ───────────────▶│  Provider  │              │
│   │         │◀─────────────── │  (LLM)     │              │
│   └─────────┘   {Content,     └────────────┘              │
│        │       StopReason}                                 │
│        │                                                   │
│        │  switch StopReason:                              │
│        ├── end_turn  ──▶ return text                      │
│        ├── tool_use  ──▶ for each block.Name in toolsByName│
│        │                  Execute(input) → tool_result    │
│        │                  append as user msg, loop again  │
│        └── max_tokens / unknown ──▶ error                 │
│                                                            │
│   MaxTurns guard prevents infinite loops                   │
└────────────────────────────────────────────────────────────┘
```

The core 30-60 lines (excerpt from [`agents/s01-minimal-loop/loop.go`](https://github.com/yeding/learn-AutoGPT/blob/main/agents/s01-minimal-loop/loop.go)):

```go
func (l *Loop) Run(ctx context.Context, userPrompt string) (string, error) {
    toolByName := map[string]Tool{}
    schemas := make([]ToolSchema, 0, len(l.Tools))
    for _, t := range l.Tools {
        s := t.Schema()
        toolByName[s.Name] = t
        schemas = append(schemas, s)
    }

    messages := []Message{
        {Role: "user", Content: []ContentBlock{{Type: "text", Text: userPrompt}}},
    }

    for turn := 0; turn < l.MaxTurns; turn++ {
        resp, err := l.Provider.CreateMessage(ctx, CreateMessageRequest{
            Messages: messages,
            Tools:    schemas,
        })
        if err != nil {
            return "", fmt.Errorf("turn %d: %w", turn, err)
        }

        // 1. Even if the assistant turn contains tool_use blocks, it must
        //    live in history — the protocol requires it.
        messages = append(messages, Message{Role: "assistant", Content: resp.Content})

        // 2. stop_reason decides what we do next.
        switch resp.StopReason {
        case "end_turn", "stop_sequence":
            return extractText(resp.Content), nil

        case "tool_use":
            toolResults, err := l.runTools(ctx, resp.Content, toolByName, turn)
            if err != nil {
                return "", err
            }
            // tool_result blocks are sent back as a USER message, one per
            // tool_use the assistant emitted.
            messages = append(messages, Message{Role: "user", Content: toolResults})

        case "max_tokens":
            return "", fmt.Errorf("hit max_tokens at turn %d (response was truncated)", turn)

        default:
            return "", fmt.Errorf("unexpected stop_reason %q at turn %d", resp.StopReason, turn)
        }
    }
    return "", fmt.Errorf("loop exceeded MaxTurns=%d without end_turn", l.MaxTurns)
}
```

**4 non-obvious points**:

1. **The assistant's tool_use must hit history before its tool_result** — the protocol guarantees that every `tool_use_id` in a user message can be resolved against the prior assistant message. Append assistant first, then user(tool_result); never swap.
2. **tool_result is a `user` role, not `system`, not `tool`** — Anthropic's wire format packs tool feedback into user message content blocks. OpenAI uses a separate `role: "tool"`. We pick the Anthropic shape; `OpenAIProvider` translates outbound.
3. **An unknown tool name does not raise — it feeds back so the model can recover** — `loop.go::runTools` synthesizes an `"unknown tool: %q"` tool_result when `byName[block.Name]` misses. Returning an error directly would deny the model the signal needed to learn it must avoid that name.
4. **Hitting MaxTurns returns an error, not a partial result** — never let the caller mistake "loop ran out" for "agent succeeded". Failure must be explicit.

## What Changed (vs. previous)

(none — this is the first chapter)

## Try It

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s01-minimal-loop

# simplest demo: ask the model to use the echo tool
go run . -v "use the echo tool to say 'banana'"

# swap to DeepSeek (OpenAI-compat) and watch the same loop on a different backend
export DEEPSEEK_API_KEY=...
go run . -provider deepseek -v "use the echo tool to say 'hello'"

# run all tests
go test -v ./...
```

Expected output shape:

```
[s01-minimal-loop] provider=anthropic model=claude-sonnet-4-6 url=
[turn 0] assistant: I'll use the echo tool now.
[turn 0] -> echo map[message:banana]
[turn 0] <- banana
[turn 1] assistant: Here you go: banana.
Here you go: banana.
```

If you see `[turn 0] -> echo` but no `<- banana`, the tool errored. If you see a wall of `[turn N]` lines until MaxTurns, the model never sent end_turn — usually the prompt is too open-ended or `max_tokens` is too small.

## Upstream Source Reading

The equivalent mechanism in AutoGPT classic is `classic/original_autogpt/autogpt/app/main.py::run_interaction_loop`. Compared to our mini, the difference is mostly *decoration*: cycle budget, signal handling, UI provider, continuous mode, the `AgentFinished` path. The core think→act→observe shape is the same on both sides.

```upstream:classic/original_autogpt/autogpt/app/main.py
# Source: classic/original_autogpt/autogpt/app/main.py (run_interaction_loop)
# Simplified: stripped logger, UI rendering, AgentFinished branch, TTS.
# Kept: the plan→execute spine.

async def run_interaction_loop(agent, ui_provider=None):
    app_config = agent.app_config
    ai_profile = agent.state.ai_profile

    # cycle_budget = how many autonomous cycles the user permits.
    # None ≡ infinity (continuous mode).
    cycle_budget = cycles_remaining = _get_cycle_budget(
        app_config.continuous_mode, app_config.continuous_limit
    )
    stop_reason = None

    # First SIGINT  → clamp cycles_remaining to 1 (let the current cycle finish cleanly)
    # Second SIGINT → AgentTerminated (hard exit)
    def graceful_agent_interrupt(signum, frame):
        nonlocal cycles_remaining, stop_reason
        if stop_reason:
            sys.exit()
        if cycles_remaining in [0, 1]:
            stop_reason = AgentTerminated("Interrupt signal received")
        else:
            cycles_remaining = 1

    def handle_stop_signal():
        if stop_reason:
            raise stop_reason

    signal.signal(signal.SIGINT, graceful_agent_interrupt)

    consecutive_failures = 0

    while cycles_remaining > 0:
        ########
        # Plan #
        ########
        handle_stop_signal()
        # Only propose a new action if we don't have an in-flight episode.
        if not (_ep := agent.event_history.current_episode) or _ep.result:
            async with ui_provider.show_spinner("Thinking..."):
                try:
                    action_proposal = await agent.propose_action()
                except InvalidAgentResponseError as e:
                    consecutive_failures += 1
                    if consecutive_failures >= 3:
                        raise AgentTerminated("...3 invalid thoughts in a row...")
                    continue
        else:
            action_proposal = _ep.action

        consecutive_failures = 0

        ###################
        # Execute Command #
        ###################
        if not action_proposal.use_tool:
            continue

        try:
            result = await agent.execute(action_proposal)
        except AgentFinished as e:
            # ... interactively prompt user for next task ...
            continue

        # Key: human deny doesn't burn a cycle. So feedback can't accelerate
        # the budget exhaustion.
        if result.status != "interrupted_by_human":
            cycles_remaining -= 1
```

```upstream:classic/original_autogpt/autogpt/agents/agent.py
# Source: classic/original_autogpt/autogpt/agents/agent.py
# (propose_action + complete_and_parse — simplified by removing the
# directives gathering and exception branches).

async def propose_action(self) -> AnyActionProposal:
    self.reset_trace()

    # Pipeline hooks: gather directives / commands / messages from all components.
    self.commands = await self.run_pipeline(CommandProvider.get_commands)
    messages = await self.run_pipeline(MessageProvider.get_messages)

    prompt: ChatPrompt = self.prompt_strategy.build_prompt(
        messages=messages,
        task=self.state.task,
        ai_profile=self.state.ai_profile,
        ai_directives=self.state.directives,
        commands=function_specs_from_commands(self.commands),
    )

    output = await self.complete_and_parse(prompt)
    self.config.cycle_count += 1
    return output

async def complete_and_parse(self, prompt: ChatPrompt) -> AnyActionProposal:
    # ↓ The actual LLM call site. Equivalent to our mini's Provider.CreateMessage.
    response = await self.llm_provider.create_chat_completion(
        prompt.messages,
        model_name=self.llm.name,
        completion_parser=self.prompt_strategy.parse_response_content,
        functions=prompt.functions,
        prefill_response=prompt.prefill_response,
    )
    result = response.parsed_result

    # AfterParse hook: components can mutate / evaluate the proposal post-LLM
    # (Reflexion uses this — see s10).
    await self.run_pipeline(AfterParse.after_parse, result)

    return result
```

**Reading notes**:

- **`while cycles_remaining > 0` vs `for turn := 0; turn < MaxTurns`** — upstream uses a "budget" model (continuous mode = ∞, otherwise = user-supplied N), we use a hard cap. The conditional-decrement (don't decrement on human interrupt) returns in s09's continuous-mode chapter.
- **`propose_action` vs `Provider.CreateMessage`** — upstream's `propose_action` bundles "gather commands/messages → build ChatPrompt → LLM call → parse" into one method. In s01 we collapse all four into "Loop builds messages inline + LLM call = Provider". The Strategy abstraction (s04) is what splits "build/parse" back out.
- **We don't have `run_pipeline(AfterParse.after_parse)`** — upstream runs an AfterParse hook every time the LLM produces a result; that's where Reflexion does its second-pass critique. s10 introduces `Pipeline` + `AfterParseHook`.
- **Error recovery**: upstream has four dedicated channels — `consecutive_failures >= 3 → AgentTerminated`, `InvalidAgentResponseError`, `AgentFinished`, `interrupted_by_human`. s01 has exactly one — "unknown tool feeds an error tool_result back so the model can self-correct". The other edge cases are deliberately omitted to keep the skeleton readable.
- **A "correct but imperfect" decision we deliberately keep**: hitting MaxTurns returns an error and discards the in-flight history. Upstream lets the caller inspect the partial run. We expose history in s05 (episodic-history) and s09 (continuous-mode).

**Read further**: start at `app/main.py:run_interaction_loop` (this chapter's center), follow `agent.propose_action` into `agents/agent.py:290-356`, then read `agents/agent.py:358-387` (`complete_and_parse`) to land on the actual LLM call site. That trace is the real-source map for s01 → s04 (prompt strategy) → s10 (reflexion hooks).

---

**Next**: s02 replaces the hard-coded `[]Tool{NewEchoTool()}` slice with an explicit `Registry`. Upstream uses Python `@command` decorators for auto-registration; Go has no decorators, so we need a registration center any file can call `Register(myTool)` against. It's also a prerequisite for s07 (permissions) and s08 (components).
