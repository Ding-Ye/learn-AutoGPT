---
title: "Multi-model guide (DeepSeek / Qwen / Moonshot / self-hosted …)"
slug: multi-model
est_read_min: 8
---

# Multi-model guide

> Every session in learn-AutoGPT — from s01 — ships with 8 LLM provider profiles built in. One flag swaps between Anthropic Claude / OpenAI / DeepSeek / Moonshot Kimi / Qwen / Groq / OpenRouter / self-hosted vLLM. This page is the cheat sheet.

---

## The 8 profiles at a glance

| `-provider` | Default model | Required env var | Notes |
|---|---|---|---|
| `anthropic` (default) | `claude-sonnet-4-6` | `ANTHROPIC_API_KEY` | native protocol (content-block tagged union) |
| `openai` | `gpt-4o-mini` | `OPENAI_API_KEY` | goes through our OpenAI-compat translation layer |
| `deepseek` | `deepseek-chat` | `DEEPSEEK_API_KEY` | top pick in mainland China: cheap + good tool-use support |
| `moonshot` | `moonshot-v1-8k` | `MOONSHOT_API_KEY` | Kimi; long context |
| `qwen` | `qwen-plus` | `DASHSCOPE_API_KEY` | Tongyi Qianwen, DashScope OpenAI-compat endpoint |
| `groq` | `llama-3.3-70b-versatile` | `GROQ_API_KEY` | very fast inference |
| `openrouter` | `openai/gpt-4o-mini` | `OPENROUTER_API_KEY` | one key, every model |
| `local` | `local-model` | `OPENAI_API_KEY` (placeholder) | local vLLM / SGLang at `http://localhost:8000/v1` |

## How to use

### 1) Default Anthropic

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s01-minimal-loop
go run . -v "say hi via the echo tool"
```

### 2) Swap to DeepSeek (cheapest full-featured option)

```bash
export DEEPSEEK_API_KEY=sk-...
go run . -provider deepseek -v "say hi via the echo tool"
```

### 3) Run any model via OpenRouter

```bash
export OPENROUTER_API_KEY=sk-or-...
go run . -provider openrouter -model "anthropic/claude-sonnet-4.6" -v "say hi"
go run . -provider openrouter -model "google/gemini-2.0-flash-exp" -v "say hi"
```

### 4) Self-hosted vLLM / SGLang

Start a vLLM server:
```bash
vllm serve meta-llama/Llama-3.3-70B-Instruct --port 8000
```
Then:
```bash
export OPENAI_API_KEY=dummy
go run . -provider local -model "meta-llama/Llama-3.3-70B-Instruct" -v "say hi"
```
(`OPENAI_API_KEY` is just a placeholder in `local` mode; vLLM defaults to no auth.)

### 5) Custom base URL

Any OpenAI-compatible endpoint will do:
```bash
go run . -provider openai -base-url "https://your-own-proxy.com/v1" -v "say hi"
```

## How it works internally

**Key insight**: our internal types use Anthropic content-block shape (`Message.Content []ContentBlock` is a tagged union over `Type ∈ {"text", "tool_use", "tool_result"}`); the `OpenAIProvider` does the protocol translation **at the provider boundary**.

```
Loop ─→ Provider iface ─→ AnthropicProvider ─→ api.anthropic.com (native)
                       │
                       └→ OpenAIProvider ─→ translateRequest → /chat/completions ─→ translateResponse
                              ↑                                                        ↓
                              └── 8KB bidirectional, content-block ↔ messages+tool_calls ─┘
```

Translation rules (identical across every session's `provider_openai.go`):

- **Request**: Anthropic's `system` field becomes the first OpenAI `{role: "system"}` message; assistant `tool_use` blocks become `tool_calls`; user `tool_result` blocks become standalone `{role: "tool", tool_call_id: ...}` messages.
- **Response**: OpenAI's `tool_calls` map back to `tool_use` content blocks; `finish_reason: stop|tool_calls|length` maps to `stop_reason: end_turn|tool_use|max_tokens`.
- **Edge case**: DeepSeek occasionally returns content as `[{type:"text", text:"..."}]` (an array, not a string) — `contentToString` accepts both shapes.

## Troubleshooting

**"unknown -provider": `<name>`**: typo. Valid names listed above.

**"<KEY> is not set"**: you forgot to export the env var for the chosen profile.

**`401 Unauthorized` from provider**: API key wrong or expired.

**`tool_use` not parsed in response**: the model doesn't support function-calling. Use a model that does (gpt-4o-mini / claude / deepseek-chat / qwen-plus / llama-3.3-70b all do).

**vLLM/SGLang returns 404**: check `-base-url` (must include `/v1`); vLLM exposes `/v1/chat/completions` by default.

**Output truncated**: pass `-max-turns 50`, or bump `MaxTokens: 8192` in the `CreateMessageRequest`.

## Mocking in tests

Every session ships a `MockProvider` for tests:

```go
mock := NewMockProvider(
    &CreateMessageResponse{
        Content: []ContentBlock{
            {Type: "tool_use", ID: "toolu_1", Name: "echo", Input: map[string]interface{}{"message": "hi"}},
        },
        StopReason: "tool_use",
    },
    &CreateMessageResponse{
        Content: []ContentBlock{{Type: "text", Text: "done"}},
        StopReason: "end_turn",
    },
)
loop := &Loop{Provider: mock, ...}
final, err := loop.Run(ctx, "say hi")
```

Mock doesn't make network calls; it replays fixtures in order until exhausted (then returns an error).

If you need to test **real** Anthropic / OpenAI HTTP behavior (paths, headers, bodies), use `httptest.NewServer` and inject the URL via `provider.baseURL = srv.URL`. s01's `provider_test.go` is the template.
