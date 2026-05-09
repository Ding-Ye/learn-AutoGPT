---
title: "s04 · Prompt strategies & response parsing"
chapter: 4
slug: s04-prompt-strategy
est_read_min: 14
---

# s04 · Prompt strategies & response parsing

> What this teaches: lift prompt construction out of the Loop and behind a `PromptStrategy` interface. The strategy decides the system prompt (role + tool list + 5 best-practices) and the initial messages, then parses the model response into an `ActionProposal` — supporting both native `tool_use` blocks and a ```json fenced-code fallback. This is the seam to AutoGPT classic's 8-strategy menagerie; we ship only `OneShotStrategy`. Reflexion is deferred to s10.

---

## Problem

s01 / s02 / s03's Loop opens a conversation like this:

```go
messages := []Message{{
    Role: "user",
    Content: []ContentBlock{{Type: "text", Text: userPrompt}},
}}
```

One user prompt, no system prompt, no role description, no hard-wired best-practices like "read files before editing", no human-readable tool list (the schemas travel through `req.Tools`, but the system instruction never says "you can ONLY use these commands").

This minimal prompt was fine for s01/02/03 — we only tested two things: (a) can the model call a tool, (b) does the round-trip work. Real agent use needs three more pieces:

1. **No role definition** — the model doesn't know it's an autonomous agent, doesn't know what to say (or *not* say) when the task is done, doesn't know when to stop.
2. **No best-practice injection** — AutoGPT classic's `OneShotAgentPromptConfiguration.DEFAULT_BODY_TEMPLATE` ships 7 efficiency guidelines ("UNDERSTAND BEFORE ACTING", "PARALLEL EXECUTION", "FIX ROOT CAUSE"). These encode hard-won lessons. Without them the model misbehaves: writes half-finished code, edits files without reading them, tries to "fix" failing tests instead of its own bug.
3. **No fallback parser** — strong models (claude-sonnet, gpt-4o, deepseek-chat) emit reliable native tool_use; smaller models (older llama, certain self-hosted vLLM checkpoints, some Mistral derivatives) often fall back to a ```json `{"command":..., "args":...}` ``` fenced-code block in the assistant text. s01-s03's Loop sees `stop_reason="end_turn"` and terminates, dropping the model's tool intent on the floor.

s04 solves all three with one abstraction: the `PromptStrategy` interface.

## Solution

**The interface**:

```go
type ActionProposal struct {
    Thoughts string                 // free-text reasoning, may be empty
    Command  string                 // tool name; empty if no action proposed
    Args     map[string]interface{} // tool input
}

type PromptStrategy interface {
    BuildPrompt(history []*Episode, tools []ToolSchema, task string) []Message
    ParseResponse(content []ContentBlock) (ActionProposal, error)
}
```

Two methods, one per direction:

- **BuildPrompt** — owns the opening conversational shape. s04's `OneShotStrategy.BuildPrompt` returns one user message (the task verbatim); the system prompt is delivered separately via `BuildSystem(tools)` because Anthropic carries `system` as a top-level request field, not a `Message`. The Loop wires `BuildSystem`'s output into `CreateMessageRequest.System`.
- **ParseResponse** — translates the assistant's `[]ContentBlock` into an `ActionProposal`. Two paths, in priority order:
  1. Any block with `Type == "tool_use"`: native tool call, lift Name/Input directly.
  2. No tool_use, but a text block contains a ```json ... ``` fence whose JSON has shape `{"command": "...", "args": {...}}`: fallback parse.
  3. Neither: return error.

**Episode is a placeholder.** s04's `BuildPrompt(history []*Episode, ...)` always receives nil. s05 will fill the Episode struct with Actions/Results fields and fold history back into the prompt. This is a **forward-compat seam** — the strategy interface signature is already correct; s05 only changes implementations, not the interface.

**Why 5 best-practices, not 7**: upstream's items 6 (CODE STYLE) and 7 (SECURITY: never log secrets) only become meaningful once s06's Workspace and s08's web/file components arrive — the s04 agent can only echo+math, so "mimic code conventions" and "don't log secrets" are premature. Injecting them now makes the prompt lie. We add them back when they have ground truth.

## How It Works

```ascii-anim frames=2
┌────────────────────────────────────────────────────────────────────┐
│   Loop.Run(ctx, "compute 2+3")                                     │
│         │                                                          │
│         ▼                                                          │
│   strategy := l.Strategy                  ← s04 new field          │
│   if strategy == nil:                                              │
│       strategy = NewOneShotStrategy()    ← default fallback        │
│         │                                                          │
│         ▼                                                          │
│   schemas := l.Tools.All()                                         │
│   messages := strategy.BuildPrompt(nil, schemas, task)            │
│   system   := strategy.BuildSystem(schemas)                       │
│         │                                                          │
│         ▼                                                          │
│   for turn < MaxTurns:                                            │
│       resp := provider.CreateMessage(req{System, Messages, Tools})│
│       proposal := strategy.ParseResponse(resp.Content) ← new seam │
│       switch resp.StopReason:                                      │
│           "tool_use":                                              │
│               results := runTools(resp.Content)  ← native path    │
│               messages.append(user-role tool_results)              │
│           "end_turn":                                              │
│               return extractText(resp.Content)                     │
│           default:  ← s04 adds JSON-fence fallback branch         │
│               if proposal.Command != "":                          │
│                   results := runFallbackTool(proposal)            │
│                   messages.append(synthesized tool_result)         │
└────────────────────────────────────────────────────────────────────┘
```

### System prompt rendering (excerpt from `strategy.go`)

`OneShotStrategy.BuildSystem` concatenates three sections: a role intro, `## Commands`, and `## Best practices`:

```go
func (s *OneShotStrategy) BuildSystem(tools []ToolSchema) string {
    var b strings.Builder
    b.WriteString("You are a methodical autonomous agent. ")
    b.WriteString("Decide one or more tool calls per turn, observe the result, then continue. ")
    b.WriteString("When the task is complete, reply with plain text and no tool call.")

    b.WriteString("\n\n## Commands\n")
    if len(tools) == 0 {
        b.WriteString("(no tools available; respond with plain text)\n")
    } else {
        b.WriteString("These are the ONLY commands you can use. Any action you perform must be possible through one of these:\n")
        for i, t := range tools {
            schemaJSON, _ := json.Marshal(t.InputSchema)
            fmt.Fprintf(&b, "%d. **%s** — %s\n   input_schema: %s\n",
                i+1, t.Name, t.Description, string(schemaJSON))
        }
    }

    b.WriteString("\n## Best practices\n")
    for i, line := range s.BestPractices {
        fmt.Fprintf(&b, "%d. %s\n", i+1, line)
    }

    return strings.TrimRight(b.String(), "\n")
}
```

What it produces with echo+math registered (excerpt):

```text
You are a methodical autonomous agent. Decide one or more tool calls
per turn, observe the result, then continue. When the task is complete,
reply with plain text and no tool call.

  ## Commands
  These are the ONLY commands you can use. Any action you perform must be
  possible through one of these:
  1. **echo** — Echo back the input message verbatim. Useful for testing
     the tool-use round-trip without side effects.
     input_schema: {"properties":{"message":...},"required":["message"]}
  2. **math** — Evaluate a basic arithmetic operation (add | sub | mul | div)
     over two numbers. Returns the result as a string.
     input_schema: {"properties":{"operation":..., "a":..., "b":...},
                    "required":["operation","a","b"]}

  ## Best practices
  1. UNDERSTAND BEFORE ACTING: read all relevant files / inputs before
     making changes; never guess at interfaces.
  2. PARALLEL EXECUTION: when independent operations can run concurrently,
     request them in one turn rather than serializing.
  3. WRITE COMPLETE CODE: produce full working implementations — no stubs,
     TODOs, or placeholders.
  4. VERIFY AFTER CHANGES: after modifying state, verify the change took
     (re-read a file, re-run a check).
  5. FIX ROOT CAUSE: when something breaks, fix the underlying cause, not
     the symptom; if a test fails, the bug is in your code, not the test.
```

### ParseResponse — the two paths

```go
func (s *OneShotStrategy) ParseResponse(content []ContentBlock) (ActionProposal, error) {
    var thoughts []string
    var toolUseBlock *ContentBlock
    var allText []string

    for i := range content {
        b := &content[i]
        switch b.Type {
        case "tool_use":
            if toolUseBlock == nil { toolUseBlock = b }   // first tool_use wins
        case "text":
            allText = append(allText, b.Text)
            thoughts = append(thoughts, b.Text)
        }
    }

    // Path 1: native tool_use
    if toolUseBlock != nil {
        return ActionProposal{
            Thoughts: strings.TrimSpace(strings.Join(thoughts, "\n")),
            Command:  toolUseBlock.Name,
            Args:     toolUseBlock.Input,
        }, nil
    }

    // Path 2: ```json ... ``` fence fallback
    combined := strings.Join(allText, "\n")
    if match := fenceRegex.FindStringSubmatch(combined); len(match) > 1 {
        payload := strings.TrimSpace(match[1])
        var parsed struct {
            Command string                 `json:"command"`
            Args    map[string]interface{} `json:"args"`
        }
        if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
            return ActionProposal{}, fmt.Errorf("parse JSON fallback: %w (payload=%q)", err, payload)
        }
        if parsed.Command == "" {
            return ActionProposal{}, fmt.Errorf("parse JSON fallback: missing required field %q", "command")
        }
        thoughtsText := strings.TrimSpace(fenceRegex.ReplaceAllString(combined, ""))
        return ActionProposal{Thoughts: thoughtsText, Command: parsed.Command, Args: parsed.Args}, nil
    }

    // Path 3: neither → error
    return ActionProposal{}, fmt.Errorf("ParseResponse: response has neither tool_use block nor JSON-fenced action (content blocks: %d)", len(content))
}
```

### Three non-obvious points

1. **System prompt is not a Message** — Anthropic's `messages` API designs `system` as a top-level field (not a `role: "system"` message). So `BuildPrompt` returns `[]Message` *without* the system prompt; `BuildSystem` is a separate string method. The Loop type-asserts the strategy to `*OneShotStrategy` to call `BuildSystem` and feeds the result into `CreateMessageRequest.System`. Strategies that need a different system prompt add their own `BuildSystem` method (kept off the `PromptStrategy` interface so each strategy isn't forced to implement one).
2. **Native wins over Fence** — if the model emits both a native `tool_use` block AND a ```json fence in text (rare, but it happens), the native path takes priority. Otherwise ParseResponse would dispatch the same action twice — observable as the LLM "double-clicking" itself. The test `TestOneShotStrategy_ParseResponse_NativeWinsOverFence` locks this contract.
3. **The `default` branch in Loop** — s03's switch had a `default` that errored. s04 changes it to "try ParseResponse one more time" — because `stop_reason="end_turn"` while the model intended a tool call is exactly what the fence fallback exists to recover. `runFallbackTool` synthesizes a `tool_use_id` so the round-trip looks consistent to the Provider.

## What Changed (vs s03)

```diff
 agents/s04-prompt-strategy/
 ├── provider.go              # byte-identical to s03
 ├── provider_openai.go       # byte-identical to s03
 ├── provider_mock.go         # byte-identical to s03
 ├── provider_anthropic_test.go  # byte-identical to s03
 ├── provider_openai_test.go  # byte-identical to s03
 ├── provider_mock_test.go    # byte-identical to s03
 ├── tools.go / tools_test.go # byte-identical to s03
 ├── registry.go / registry_test.go  # byte-identical to s03
+├── strategy.go              # NEW: PromptStrategy + OneShotStrategy + ActionProposal + Episode placeholder
+├── strategy_test.go         # NEW: 8 tests covering BuildSystem/BuildPrompt/ParseResponse both paths
 ├── loop.go                  # MODIFIED: Loop adds Strategy field; Run calls strategy.BuildPrompt + ParseResponse; new fallback branch
 ├── loop_test.go             # s03 tests + new stubStrategy test verifying strategy is invoked
 └── main.go                  # adds -strategy oneshot flag (only option in s04; s10 adds reflexion)
```

Type catalog gains:

```go
type ActionProposal struct { Thoughts, Command string; Args map[string]interface{} }
type Episode      struct { /* s05 fills in */ }
type PromptStrategy interface { BuildPrompt(history []*Episode, tools []ToolSchema, task string) []Message; ParseResponse([]ContentBlock) (ActionProposal, error) }
type OneShotStrategy struct { BestPractices []string }
```

`Loop` gets a new `Strategy PromptStrategy` field; nil is safe — Run constructs a default `NewOneShotStrategy()` on the first call. So s03-style construction `&Loop{Provider: p, Tools: reg, MaxTurns: 5}` still works.

## Try It

```bash
cd agents/s04-prompt-strategy

# Anthropic native + oneshot (default)
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "use the math tool to add 7 and 35"

# DeepSeek (OpenAI-compat); watch the Loop translate the system prompt into an OpenAI role:"system" message
export DEEPSEEK_API_KEY=sk-...
go run . -provider deepseek -v "echo back hi"

# Local vLLM / SGLang — common fence-fallback trigger
export OPENAI_API_KEY=anything
go run . -provider local -model llama-3.3 "compute 6 * 7"

# Run all tests (47 should pass)
go test -v ./...
```

Expected output shape (Anthropic path):

```
[s04-prompt-strategy] provider=anthropic model=claude-sonnet-4-6 url= strategy=oneshot tools=2
[turn 0] assistant: I'll use the math tool to add 7 and 35.
[turn 0] proposal: cmd=math thoughts="I'll use the math tool to add 7 and 35."
[turn 0] -> math map[a:7 b:35 operation:add]
[turn 0] <- 42
[turn 1] assistant: 7 + 35 = 42.
7 + 35 = 42.
```

If the model used the fence-fallback path (small open-weight model that doesn't emit native tool_use):

```
[turn 0] assistant: I'll use the echo tool. ```json
{"command": "echo", "args": {"message": "hi"}}
```
[turn 0] JSON-fallback proposal: cmd=echo
[turn 1] assistant: hi
hi
```

The two paths are pinned by `TestOneShotStrategy_ParseResponse_NativeToolUse` and `TestOneShotStrategy_ParseResponse_JSONFenceFallback`.

## Upstream Source Reading

AutoGPT classic puts prompt strategies under `classic/original_autogpt/autogpt/agents/prompt_strategies/`, 8 files — `base.py` is the ABC and `one_shot.py` is the baseline (the others are reflexion / rewoo / plan_execute / lats / tree_of_thoughts / multi_agent_debate variants). We focus on `one_shot.py`'s three core methods:

```upstream:classic/original_autogpt/autogpt/agents/prompt_strategies/one_shot.py
# Source: classic/original_autogpt/autogpt/agents/prompt_strategies/one_shot.py
# Simplified: Pydantic validation + prefill hooks stripped; the three-section
# (system / task / response_format) shape is preserved verbatim.

class OneShotAgentPromptConfiguration(SystemConfiguration):
    DEFAULT_BODY_TEMPLATE: str = (
        "## Constraints\n{constraints}\n\n"
        "## Resources\n{resources}\n\n"
        "## Commands\n"
        "These are the ONLY commands you can use."
        " Any action you perform must be possible through one of these commands:\n"
        "{commands}\n\n"
        "## Best practices\n{best_practices}\n\n"
        "## Efficiency Guidelines\n"
        "1. UNDERSTAND BEFORE ACTING: Read ALL relevant files before making changes...\n"
        "2. PARALLEL EXECUTION: When multiple operations don't depend on each other, "
        "execute them simultaneously...\n"
        "3. WRITE COMPLETE CODE: Write complete, working implementations...\n"
        "4. VERIFY AFTER CHANGES: After modifying code, verify it works...\n"
        "5. FIX ROOT CAUSE: When debugging, fix the underlying issue, not symptoms...\n"
        "6. CODE STYLE: Mimic existing code conventions...\n"
        "7. SECURITY: Never expose, log, or commit secrets, API keys, or credentials."
    )
    body_template: str = UserConfigurable(default=DEFAULT_BODY_TEMPLATE)
    use_prefill: bool = True


class OneShotAgentPromptStrategy(PromptStrategy):
    def build_prompt(
        self, *, messages, task, ai_profile, ai_directives,
        commands, include_os_info, **extras,
    ) -> ChatPrompt:
        system_prompt, response_prefill = self.build_system_prompt(
            ai_profile=ai_profile, ai_directives=ai_directives,
            commands=commands, include_os_info=include_os_info,
        )
        final_instruction_msg = ChatMessage.user(self.config.choose_action_instruction)
        return ChatPrompt(
            messages=[
                ChatMessage.system(system_prompt),
                ChatMessage.user(f'"""{task}"""'),
                *messages,                 # ← history; corresponds to our []*Episode
                final_instruction_msg,
            ],
            prefill_response=response_prefill if self.config.use_prefill else "",
            functions=commands,
        )

    def parse_response_content(
        self, response: AssistantChatMessage,
    ) -> OneShotAgentActionProposal:
        if not response.content:
            # Some models (e.g. GPT-5) return tool_calls without text content.
            if response.tool_calls:
                assistant_reply_dict = {"thoughts": {
                    "observations": "", "text": "", "reasoning": "",
                    "self_criticism": "", "plan": [], "speak": "",
                }}
            else:
                raise InvalidAgentResponseError("Assistant response has no text content")
        else:
            assistant_reply_dict = extract_dict_from_json(response.content)

        if not response.tool_calls:
            raise InvalidAgentResponseError("Assistant did not use a tool")
        assistant_reply_dict["use_tool"] = response.tool_calls[0].function
        if len(response.tool_calls) > 1:
            assistant_reply_dict["use_tools"] = [
                tc.function for tc in response.tool_calls
            ]
        parsed_response = OneShotAgentActionProposal.model_validate(assistant_reply_dict)
        parsed_response.raw_message = response.model_copy()
        return parsed_response


class AssistantThoughts(ModelWithSummary):
    observations: str  = Field(description="Relevant observations from your last action")
    reasoning: str     = Field(description="Reasoning behind choosing this action")
    self_criticism: str = Field(description="Constructive self-criticism")
    plan: list[str]    = Field(description="Short list that conveys the long-term plan")


class OneShotAgentActionProposal(ActionProposal):
    thoughts: AssistantThoughts  # type: ignore
```

### Reading notes

- **Upstream's three-section system prompt vs our two-section**: upstream's `body_template` has 4 sections (Constraints / Resources / Commands / Best practices) plus Efficiency Guidelines plus Task plus Response Format — about 1500 chars total. We ship Commands + Best practices + a one-line role intro, ~400 chars. Why: `Constraints` and `Resources` upstream are populated via `AIDirectives`, where each component contributes a few entries; but s04 has no component system (s08 introduces it), so leaving those sections empty would just lie. We add them back when they have meaningful content.
- **No prefill** — upstream's `use_prefill: bool = True` makes the LLM's reply always start with `{\n    "thoughts":` so Pydantic can force the JSON shape. This only works on Anthropic (which supports prefill response) and only matters when the response is a structured JSON Pydantic model. We consume native `tool_use` directly (the args are already a JSON object), so prefill earns no value. When s10 introduces "model self-evaluates the proposal", a separate LLM round-trip is more honest than a prefill trick.
- **No `AssistantThoughts` modeling** — upstream uses Pydantic to force the response to include observations/reasoning/self_criticism/plan. Our `ActionProposal.Thoughts string` is a single free-text field; `ParseResponse` concatenates all text blocks. Why: (a) `AssistantThoughts`'s subfields have no real downstream consumers in the OneShot path (a long-running upstream dead-code risk); (b) a single field handles streaming / partial output more gracefully; (c) when Reflexion in s10 actually consumes thoughts, *then* we add structured fields.
- **`extract_dict_from_json` is the fence-parse equivalent** — upstream's `forge.json.parsing.extract_dict_from_json` is a forgiving JSON parser that "finds the first dict-like substring in this text". Our `fenceRegex` is the lighter version: only matches ```json``` fences, no aggressive search-anywhere-for-balanced-braces. This avoids the false positive where the model writes a `{ key: value }` example in prose and the parser misreads it as the action.
- **`use_tool` / `use_tools` dual fields** — upstream reserves both for parallel execution: a single tool call goes in `use_tool`, multiple in `use_tools[]`. s04 only takes the first tool_use block; parallel execution is upstream's `_execute_tools_parallel` territory, and we'll revisit it in s08 with the component system.

**Read further**: start at `classic/original_autogpt/autogpt/agents/prompt_strategies/base.py::PromptStrategy`, walk through `one_shot.py::OneShotAgentPromptStrategy.build_prompt`, then peek at `reflexion.py::ReflexionAgentPromptStrategy.build_prompt` — you'll find Reflexion is essentially a OneShot wrapper that appends "evaluate whether your previous action was sensible before acting" to the prompt. That's exactly the shape s10 will recreate in Go.

---

**Next**: s05 turns the Loop's implicit messages accumulator into an explicit `History` of `Episode{Actions, Results}`. s04's `BuildPrompt(history []*Episode, ...)` already reserved the history parameter; s05 only changes strategy implementations — no interface change. This is the smallest teachable version of AutoGPT upstream's `EpisodicActionHistory.prepare_messages`.
