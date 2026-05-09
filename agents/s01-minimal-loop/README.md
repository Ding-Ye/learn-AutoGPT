# s01 · minimal think→act→observe loop

> **zh** 用 ~250 行 Go 实现 AutoGPT 经典版的最小循环：think → act → observe，再回到 think。
> **en** A ~250-line Go implementation of AutoGPT classic's minimal loop: think → act → observe, repeat.

## Files

| file | role |
|---|---|
| `provider.go` | `Message`, `ContentBlock`, `ToolSchema`, `Provider` interface, `AnthropicProvider` (with `baseURL` for httptest injection) |
| `provider_openai.go` | OpenAI-compatible provider — works with OpenAI / DeepSeek / Moonshot / Qwen / Groq / OpenRouter / local vLLM |
| `provider_mock.go` | `MockProvider` — replays canned responses; reused by every later session |
| `tools.go` | `Tool` interface + `EchoTool` (no `BashTool` — sandboxing lands in s06) |
| `loop.go` | `Loop.Run` — the heart of the agent: dispatch tool_use, feed tool_result, terminate on end_turn |
| `main.go` | 8-profile CLI picker (`-provider {anthropic|openai|deepseek|moonshot|qwen|groq|openrouter|local}`) |
| `testdata/golden_response.json` | sample Anthropic-shape tool_use response — fixture for tests / doc reading |

## Run / 运行

```bash
# 1. set ONE of these env vars
export ANTHROPIC_API_KEY=sk-ant-...
# or  export OPENAI_API_KEY=sk-...
# or  export DEEPSEEK_API_KEY=...

cd agents/s01-minimal-loop

# default: Anthropic
go run . -v "echo back the word 'banana'"

# DeepSeek (cheap, OpenAI-compat)
go run . -provider deepseek -v "echo 'hello'"

# local vLLM/SGLang on http://localhost:8000/v1
go run . -provider local -model llama-3.3 -v "echo 'hi'"
```

## Test / 测试

```bash
go test -v ./...
```

Expect ~12 tests in 4 files: tools, loop (3 protocol cases + MaxTurns), Anthropic provider via httptest, MockProvider.

## Key teaching points / 学习要点

1. **The loop is tiny / 循环本身极小** — `Loop.Run` is ~50 LoC. The complexity lives in the *protocol* (tool_use ↔ tool_result), not the control flow. 真正的复杂度在 wire 协议，不在控制流。
2. **Provider is an interface from day one / Provider 接口从第一节就存在** — every later session swaps providers without touching the loop. 后面所有章节都靠这个 seam 注入 mock 或换 backend。
3. **Tool is locked across sessions / Tool 接口跨章节锁定** — `Schema() ToolSchema` + `Execute(ctx, input) (string, error)` never changes from s01 to s10. 后续章节增加新 Tool，但不重命名方法。
4. **No bash, no workspace / 无 bash、无 workspace** — security has to be designed in, not bolted on. We defer shell access to s06 once `Workspace` exists. 安全功能等 s06 沙箱就位后再加，避免 s01 写出会让人养成坏习惯的代码。
5. **Anthropic shape internally, OpenAI on the wire / 内部 Anthropic 形态，可对外切 OpenAI** — content blocks are a clean tagged union for tool_use/tool_result; `OpenAIProvider` does the wire translation. This decouples *protocol* from *vendor*. 协议形态（块）和厂商（HTTP API）分离。

## Read more / 深入阅读

- 中文：[`docs/zh/s01-minimal-loop.md`](../../docs/zh/s01-minimal-loop.md)
- English: [`docs/en/s01-minimal-loop.md`](../../docs/en/s01-minimal-loop.md)
- Upstream excerpt: [`upstream-readings/s01-app-main.py`](../../upstream-readings/s01-app-main.py)
