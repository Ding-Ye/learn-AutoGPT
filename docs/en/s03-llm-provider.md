---
title: "s03 · LLM Provider with multiple backends"
chapter: 3
slug: s03-llm-provider
est_read_min: 14
---

# s03 · LLM Provider with multiple backends

> What this teaches: take the three Provider implementations s01/s02 already shipped (native Anthropic, OpenAI-compat translation layer, Mock) and *really* dig in — one internal Anthropic shape and 8 wire formats; why our Go version doesn't need upstream `MultiProvider`'s `_provider_instances` lazy cache; and rename the test files by target so `provider_anthropic_test.go` / `provider_openai_test.go` / `provider_mock_test.go` each name what they exercise.

---

## Problem

s01's Loop only knows how to call one `Provider.CreateMessage`. But realistically, most users don't have an Anthropic key — they're more likely to have OpenAI / DeepSeek / Qwen / Moonshot / Groq / OpenRouter keys, or a self-hosted vLLM/SGLang on localhost. AutoGPT classic solves this in `classic/forge/forge/llm/providers/multi.py` with `MultiProvider`: keep one `CHAT_MODELS` dict that aggregates models from 4 backends (Anthropic / Groq / Llamafile / OpenAI), route by `model_name`, and lazy-init each provider through a `_provider_instances: dict[ModelProviderName, ChatModelProvider]` cache.

Our problem set is shaped differently:
1. **Not 4 SDKs but 1 native Anthropic + 7 OpenAI-compat** — most modern backends (DeepSeek, Qwen, Moonshot, Groq, OpenRouter, local vLLM) all speak OpenAI Chat Completions. We collapse them into a single `OpenAIProvider` and disambiguate by base URL.
2. **Loop must not know about provider** — `Loop.Run` only calls `Provider.CreateMessage`; wire-format translation lives entirely inside the Provider, otherwise every downstream module (s04 strategy, s05 history, s10 hooks) ends up branching on backend.
3. **Test names should point at which provider they exercise** — s01/s02 had a single `provider_test.go` (testing Anthropic). With three Provider impls now coexisting, the file name should say which one — otherwise you'll need to grep "AnthropicProvider" in test bodies a year later just to recall.

The s03 *code* isn't new — `provider.go`, `provider_openai.go`, `provider_mock.go` were all written in s01. The real deliverable here is this doc: walk the 30-line translation layer slowly, and rename `provider_test.go` → `provider_anthropic_test.go` for clarity.

## Solution

**One internal shape, many wire formats.** The Loop's internal `Message` / `ContentBlock` is always the Anthropic tagged-union form (`type: "text" | "tool_use" | "tool_result"`). The three Providers each translate at the `CreateMessage` boundary:

- `AnthropicProvider` — internal IS the Anthropic shape, so "translation" is identity. `json.Marshal(req)` emits a valid Anthropic Messages API body verbatim.
- `OpenAIProvider` — translates both directions:
  - Outbound: `translateRequestToOpenAI` flattens internal `Message` blocks into OpenAI's `{role, content, tool_calls}` list, lifting `tool_use` blocks onto the assistant message's `tool_calls` field.
  - Inbound: `translateResponseFromOpenAI` folds `choices[0].message.tool_calls` back into `ContentBlock{Type: "tool_use"}` and maps `finish_reason` → `stop_reason` (`stop→end_turn`, `tool_calls→tool_use`, `length→max_tokens`).
- `MockProvider` — no HTTP; replays a list of canned responses in order and records the requests for tests to assert against.

**No lazy cache.** Upstream's `_provider_instances: dict[...]` exists to amortize Python `__init__` cost (the OpenAI SDK's client construction does credential probing, token detection, etc — non-trivial). In Go, `NewAnthropicProvider(apiKey, model)` is a struct literal + one `&http.Client{Timeout: 120s}` — microseconds. Caching adds complexity we don't need.

**Test files named by target.** s03's only structural change: rename s02's leftover `provider_test.go` → `provider_anthropic_test.go`. With three Provider impls in one package, the file name immediately answers "which provider does this test cover?".

## How It Works

```ascii-anim frames=2
┌────────────────────────────────────────────────────────────────────┐
│   User CLI                                                         │
│      go run . -provider deepseek "compute 2+3"                    │
│                          │                                         │
│                          ▼                                         │
│   main.go: switch *provider {                                     │
│      case "anthropic": p = NewAnthropicProvider(...)              │
│      default:          p = NewOpenAIProvider(...)                 │
│   }                                                                │
│                          │                                         │
│                          ▼                                         │
│   loop := &Loop{Provider: p, Tools: reg, ...}                     │
│   loop.Run(ctx, prompt)                                           │
│                          │                                         │
│                          ▼                                         │
│   Provider iface.CreateMessage(ctx, req)                          │
│      ┌───────────────┬──────────────────┬──────────────────┐      │
│      ▼               ▼                  ▼                  │      │
│   Anthropic       OpenAI-compat       Mock                 │      │
│   (native:        (translates:        (replays fixtures)   │      │
│   identity)        2 directions)                            │      │
│      │               │                                     │      │
│      ▼               ▼                                     │      │
│   /v1/messages    /chat/completions                        │      │
│   (Anthropic     (OpenAI / DeepSeek / Qwen / Moonshot /    │      │
│    only)          Groq / OpenRouter / local vLLM)          │      │
└────────────────────────────────────────────────────────────────────┘
```

### Side-by-side: Anthropic vs OpenAI wire shape

**Request — Anthropic native**:

```json
{
  "model": "claude-sonnet-4-6",
  "max_tokens": 4096,
  "system": "You are helpful.",
  "messages": [
    {"role": "user", "content": [
      {"type": "text", "text": "compute 2+3"}
    ]},
    {"role": "assistant", "content": [
      {"type": "text", "text": "I'll use math."},
      {"type": "tool_use", "id": "toolu_001", "name": "math",
       "input": {"operation": "add", "a": 2, "b": 3}}
    ]},
    {"role": "user", "content": [
      {"type": "tool_result", "tool_use_id": "toolu_001", "content": "5"}
    ]}
  ],
  "tools": [
    {"name": "math", "description": "...", "input_schema": {...}}
  ]
}
```

**Same request — OpenAI Chat Completions**:

```json
{
  "model": "deepseek-chat",
  "max_tokens": 4096,
  "messages": [
    {"role": "system",    "content": "You are helpful."},
    {"role": "user",      "content": "compute 2+3"},
    {"role": "assistant", "content": "I'll use math.",
     "tool_calls": [{"id": "toolu_001", "type": "function",
                     "function": {"name": "math",
                                  "arguments": "{\"operation\":\"add\",\"a\":2,\"b\":3}"}}]},
    {"role": "tool",      "tool_call_id": "toolu_001", "content": "5"}
  ],
  "tools": [
    {"type": "function",
     "function": {"name": "math", "description": "...", "parameters": {...}}}
  ]
}
```

**Key differences**:

| Dimension | Anthropic | OpenAI |
|---|---|---|
| `system` prompt | Top-level field | A `role: "system"` entry inside `messages[]` |
| User content | `[]ContentBlock` array (tagged union) | A single string (or sometimes an array) |
| Assistant tool call | A `type: "tool_use"` block in `content[]` | A `tool_calls[]` field on the message |
| Tool args | Native JSON object (`input: {...}`) | **JSON-encoded string** (`arguments: "{...}"`)! |
| Tool result | A `tool_result` block in a user message | A separate `role: "tool"` message |
| Tool definition | Flat `{name, description, input_schema}` | Wrapped: `{type: "function", function: {...}}` |
| Stop reason | `stop_reason: "end_turn" / "tool_use" / "max_tokens"` | `finish_reason: "stop" / "tool_calls" / "length"` |

### The translation layer (excerpt from `provider_openai.go`)

```go
// translateRequestToOpenAI converts our Anthropic-style request into an
// OpenAI Chat Completions request, in three passes:
//
//  1. system prompt becomes a {role:"system"} message
//  2. each Anthropic message expands into 1+ OpenAI messages; assistant
//     tool_use blocks become tool_calls on the assistant message; user
//     tool_result blocks become separate {role:"tool"} messages
//  3. tool definitions get wrapped under {type:"function", function:{...}}
func translateRequestToOpenAI(req CreateMessageRequest, model string, maxTokens int) openAIChatRequest {
    out := openAIChatRequest{Model: model, MaxTokens: maxTokens}
    if req.System != "" {
        out.Messages = append(out.Messages, openAIMessage{Role: "system", Content: req.System})
    }
    for _, m := range req.Messages {
        out.Messages = append(out.Messages, anthropicMessageToOpenAI(m)...)
    }
    for _, t := range req.Tools {
        out.Tools = append(out.Tools, openAITool{
            Type: "function",
            Function: openAIToolDef{
                Name:        t.Name,
                Description: t.Description,
                Parameters:  t.InputSchema,
            },
        })
    }
    return out
}

// anthropicMessageToOpenAI handles the four cases:
//   user + text       → one user message with concatenated text
//   user + tool_result → one tool message PER tool_result block
//   assistant + text   → one assistant message with content
//   assistant + tool_use → one assistant message with tool_calls
func anthropicMessageToOpenAI(m Message) []openAIMessage {
    var out []openAIMessage
    switch m.Role {
    case "user":
        var texts []string
        var tools []openAIMessage
        for _, b := range m.Content {
            switch b.Type {
            case "text":
                if b.Text != "" { texts = append(texts, b.Text) }
            case "tool_result":
                tools = append(tools, openAIMessage{
                    Role: "tool", ToolCallID: b.ToolUseID,
                    Content: stringifyToolResult(b.ToolContent),
                })
            }
        }
        if len(texts) > 0 {
            out = append(out, openAIMessage{Role: "user", Content: strings.Join(texts, "\n")})
        }
        out = append(out, tools...)
    case "assistant":
        // ... mirror image: text → content, tool_use → tool_calls
    }
    return out
}
```

### Three non-obvious points

1. **Anthropic allows mixing text + tool_result in one user message; OpenAI doesn't** — see the `user` branch in `anthropicMessageToOpenAI`: we first concatenate all text blocks into one user message, then emit a separate `role: "tool"` message per tool_result block. This is **N → M expansion**, not 1:1 — the translation layer must understand that.
2. **`arguments` is double-JSON-encoded** — OpenAI's `function.arguments` is a string containing another JSON object. So outbound: `args, _ := json.Marshal(b.Input); ... Arguments: string(args)`. Inbound: `json.Unmarshal([]byte(tc.Function.Arguments), &input)`. If a model emits malformed arguments (small open-weight models occasionally do), we stash the raw text in `_raw_arguments` rather than discarding — observability over silent loss.
3. **`finish_reason` defaulting to `end_turn` is a deliberate fallback** — some OpenAI-compat backends (early DeepSeek, certain vLLM versions) emit `function_call` (the legacy single-function protocol) or even an empty string. The `default: "end_turn"` mapping lets the Loop exit gracefully instead of panicking.

## What Changed (vs s02)

```diff
 agents/s03-llm-provider/
 ├── provider.go              # byte-identical to s02
 ├── provider_openai.go       # byte-identical to s02
 ├── provider_mock.go         # byte-identical to s02
-├── provider_test.go         # ← old name (s01/s02)
+├── provider_anthropic_test.go  # ← s03's only file rename: name points at Anthropic
 ├── provider_openai_test.go  # byte-identical to s02
 ├── provider_mock_test.go    # byte-identical to s02
 ├── tools.go / registry.go / loop.go    # byte-identical to s02
 ├── main.go                   # usage string "s02-command-registry" → "s03-llm-provider"
 └── README.md                 # framed around the translation-layer chapter
```

**No API changes, no type renames, no signature drift.** s03 is a doc-led chapter — the deliverable is walking the 30-line translation layer plus the test-file rename. s01 still runs, s02 still runs, s03 runs with all 8 profiles intact.

One new test case sneaks in: `TestAnthropicProvider_DefaultsModelAndMaxTokens` covers the "if the caller leaves Model/MaxTokens empty, the Provider must inject defaults" contract — a default-injection path s01/s02 forgot to test.

## Try It

```bash
cd agents/s03-llm-provider

# Default Anthropic
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "use the math tool to add 7 and 35"

# DeepSeek (OpenAI-compat path; you'll see the translation layer at work)
export DEEPSEEK_API_KEY=sk-...
go run . -provider deepseek -v "use math to multiply 6 and 7"

# Qwen via DashScope (also OpenAI-compat, just different base URL)
export DASHSCOPE_API_KEY=sk-...
go run . -provider qwen -v "echo back hi"

# Local vLLM / SGLang (any OpenAI-compat server works)
export OPENAI_API_KEY=any-string-its-not-checked-locally
go run . -provider local -base-url http://localhost:8000/v1 -model llama-3.3 "what is 9 / 2"

# Run all tests (37 should pass)
go test -v ./...
```

Expected output shape (DeepSeek path):

```
[s03-llm-provider] provider=deepseek model=deepseek-chat url=https://api.deepseek.com/v1 tools=2
[turn 0] assistant: I'll use the math tool to multiply.
[turn 0] -> math map[a:6 b:7 operation:mul]
[turn 0] <- 42
[turn 1] assistant: 6 × 7 = 42.
6 × 7 = 42.
```

If an OpenAI-compat backend emits malformed `arguments` (occasionally happens with smaller open-weight models), the tool surfaces a `missing required field "operation"` error, the Loop translates that into `tool_result: "tool error: ..."` and feeds it back to the model — the next turn typically self-corrects with a valid tool call. That's the `_raw_arguments` fallback path at work.

## Upstream Source Reading

AutoGPT classic puts five files under `classic/forge/forge/llm/providers/`: `schema.py` (shared types), `anthropic.py` / `openai.py` / `groq.py` / `llamafile.py` (four concrete providers), and `multi.py` (the aggregator). The interesting one for s03 is `multi.py` — that's where upstream lines up multiple providers behind a single abstraction.

```upstream:classic/forge/forge/llm/providers/multi.py
# Source: classic/forge/forge/llm/providers/multi.py
# Simplified: stripped generic typing & logging; preserves aggregation +
# routing + cache logic.

from .anthropic import ANTHROPIC_CHAT_MODELS, AnthropicProvider
from .groq import GROQ_CHAT_MODELS, GroqProvider
from .llamafile import LLAMAFILE_CHAT_MODELS, LlamafileProvider
from .openai import OPEN_AI_CHAT_MODELS, OpenAIProvider

# Key 1 ── one CHAT_MODELS dict aggregates every backend's models.
# key = model_name (e.g. "gpt-4o-mini", "claude-sonnet-4-6"),
# value = ChatModelInfo (carries provider_name → tells the router which
# provider owns this model).
CHAT_MODELS = {
    **ANTHROPIC_CHAT_MODELS,
    **GROQ_CHAT_MODELS,
    **LLAMAFILE_CHAT_MODELS,
    **OPEN_AI_CHAT_MODELS,
}


class MultiProvider(BaseChatModelProvider):
    # Key 2 ── lazy cache. We __init__ a provider only the first time
    # it's used; subsequent calls fetch from the dict. Constructing a
    # Python OpenAI client involves credential probing etc, so caching
    # is worth its cost.
    _provider_instances: dict[ModelProviderName, ChatModelProvider]

    def __init__(self, settings=None, logger=None):
        super().__init__(settings=settings, logger=logger)
        self._budget = self._settings.budget or ModelProviderBudget()
        self._provider_instances = {}

    # Key 3 ── single entry point. create_chat_completion takes a
    # model_name, routes to the right provider, forwards args verbatim.
    async def create_chat_completion(
        self, model_prompt, model_name, completion_parser=lambda _: None,
        functions=None, max_output_tokens=None, prefill_response="", **kwargs,
    ):
        return await self.get_model_provider(model_name).create_chat_completion(
            model_prompt=model_prompt, model_name=model_name,
            completion_parser=completion_parser, functions=functions,
            max_output_tokens=max_output_tokens, prefill_response=prefill_response,
            **kwargs,
        )

    # Key 4 ── model_name → provider_name (via CHAT_MODELS lookup),
    # then provider_name → cached instance.
    def get_model_provider(self, model: ModelName) -> ChatModelProvider:
        model_info = CHAT_MODELS[model]
        return self._get_provider(model_info.provider_name)

    def _get_provider(self, provider_name) -> ChatModelProvider:
        # Cache hit: return existing instance.
        provider = self._provider_instances.get(provider_name)
        if provider is not None:
            return provider

        # Cache miss: instantiate, inject budget, write back.
        Provider = self._get_provider_class(provider_name)  # dict-of-classes
        settings = Provider.default_settings.model_copy(deep=True)
        settings.budget = self._budget
        # ... credential loading from env via Pydantic settings ...
        provider = Provider(settings=settings, logger=self._logger)
        self._provider_instances[provider_name] = provider
        return provider

    @classmethod
    def _get_provider_class(cls, provider_name):
        # Hard-coded provider_name → class table.
        return {
            ModelProviderName.ANTHROPIC: AnthropicProvider,
            ModelProviderName.GROQ: GroqProvider,
            ModelProviderName.LLAMAFILE: LlamafileProvider,
            ModelProviderName.OPENAI: OpenAIProvider,
        }[provider_name]
```

```upstream:classic/forge/forge/llm/providers/openai.py
# Source: classic/forge/forge/llm/providers/openai.py
# Simplified: only the constructor. The real translation lives in the
# parent class BaseOpenAIChatProvider (see _get_chat_completion_args).

class OpenAIProvider(
    BaseOpenAIChatProvider[OpenAIModelName, OpenAISettings],
    BaseOpenAIEmbeddingProvider[OpenAIModelName, OpenAISettings],
):
    MODELS = OPEN_AI_MODELS
    CHAT_MODELS = OPEN_AI_CHAT_MODELS
    EMBEDDING_MODELS = OPEN_AI_EMBEDDING_MODELS

    def __init__(self, settings=None, logger=None):
        super().__init__(settings=settings, logger=logger)
        if self._credentials.api_type == SecretStr("azure"):
            from openai import AsyncAzureOpenAI
            self._client = AsyncAzureOpenAI(**self._credentials.get_api_access_kwargs())
        else:
            from openai import AsyncOpenAI
            self._client = AsyncOpenAI(**self._credentials.get_api_access_kwargs())
```

### Reading notes

- **Aggregation strategy differs**: upstream merges every backend's models into one `CHAT_MODELS` dict and routes via `model_name → provider_name`. We pick the backend explicitly through `-provider` in `main.go` — no model-name → provider-name indirection. Why: upstream has 4 *native* SDKs (each with its own auth, tokenizer, error codes) demanding hard routing; 7 of our 8 backends speak OpenAI Chat Completions, so just disambiguating by base URL is enough.
- **We skip the lazy cache**: upstream's `_provider_instances: dict` lazy-inits each provider on first use. The Python OpenAI client constructor does credential probing, retry config, etc — IO worth amortizing. In Go, `NewAnthropicProvider(apiKey, model)` is a struct literal + one `&http.Client{Timeout: 120s}` — nanoseconds. A cache here would only add complexity.
- **Where the translation lives**: upstream's `OpenAIProvider` extends `BaseOpenAIChatProvider`, and the translation logic lives in the base class's `_get_chat_completion_args` — converting `ChatMessage` / `CompletionModelFunction` into the OpenAI SDK's `ChatCompletionMessageParam` / `CompletionCreateParams`. We do all of it in one `provider_openai.go` file — 80 lines of Go and the entire round-trip is visible without chasing class hierarchies.
- **`_functions_compat_fix_kwargs` for older models**: upstream has a degradation path for models *without* a function-calling API — it formats tool definitions as a TypeScript namespace and stuffs them in the system prompt. We don't bother — every modern OpenAI-compat backend supports `tool_calls`, and llama-3.3 supports it too. If you ever need a degradation path, s04's prompt strategy is a much better fit for it.
- **No litellm**: research-notes risk #1 mentions it — upstream wraps `litellm` for additional routing (used by OpenRouter and friends). Go has no `litellm` analog, but since OpenRouter itself speaks OpenAI Chat Completions, `-provider openrouter` works directly without a routing library.

**Read further**: start at `classic/forge/forge/llm/providers/multi.py::MultiProvider`, follow `_get_provider` into `openai.py::OpenAIProvider.__init__`, then read `BaseOpenAIChatProvider._get_chat_completion_args` (in `_openai_base.py`) for the actual translation. You'll see upstream's translation layer is much heavier than ours — mostly because it supports streaming, `reasoning_effort`, structured outputs, and other features our mini hasn't introduced yet.

---

**Next**: s04 lifts prompt construction out of the Loop. s01–s03's Loop hands the user prompt directly to the Provider; s04 introduces a `PromptStrategy` interface where the strategy decides how to render history, the tool list, and the user task into messages, and how to parse the model's response. This is the entry to AutoGPT classic's 8 prompt-strategy menagerie (we ship only `OneShotStrategy`; Reflexion is deferred to s10).
