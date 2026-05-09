---
title: "多模型接入指南（DeepSeek / Qwen / Moonshot / 自托管 …）"
slug: multi-model
est_read_min: 8
---

# 多模型接入指南

> learn-AutoGPT 从 s01 起每节都内置 8 个 LLM provider profile。一行命令切换 Anthropic Claude / OpenAI / DeepSeek / Moonshot Kimi / Qwen / Groq / OpenRouter / 本地 vLLM。本文是这套机制的速查手册。

---

## 8 个 profile 一览

| `-provider` | 默认模型 | 必需的环境变量 | 备注 |
|---|---|---|---|
| `anthropic` (默认) | `claude-sonnet-4-6` | `ANTHROPIC_API_KEY` | native 协议（content-block tagged union） |
| `openai` | `gpt-4o-mini` | `OPENAI_API_KEY` | 走我们的 OpenAI-compat 翻译层 |
| `deepseek` | `deepseek-chat` | `DEEPSEEK_API_KEY` | 国内首选：便宜 + tool-use 支持好 |
| `moonshot` | `moonshot-v1-8k` | `MOONSHOT_API_KEY` | Kimi；长上下文 |
| `qwen` | `qwen-plus` | `DASHSCOPE_API_KEY` | 通义千问，DashScope OpenAI-compat 端点 |
| `groq` | `llama-3.3-70b-versatile` | `GROQ_API_KEY` | 推理速度快 |
| `openrouter` | `openai/gpt-4o-mini` | `OPENROUTER_API_KEY` | 一个 key 跑所有模型 |
| `local` | `local-model` | `OPENAI_API_KEY`（占位） | 本地 vLLM / SGLang，base URL `http://localhost:8000/v1` |

## 怎么用

### 1) 默认 Anthropic

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s01-minimal-loop
go run . -v "say hi via the echo tool"
```

### 2) 切换到 DeepSeek（最便宜的全功能选项）

```bash
export DEEPSEEK_API_KEY=sk-...
go run . -provider deepseek -v "say hi via the echo tool"
```

### 3) 用 OpenRouter 跑任意模型

```bash
export OPENROUTER_API_KEY=sk-or-...
go run . -provider openrouter -model "anthropic/claude-sonnet-4.6" -v "say hi"
go run . -provider openrouter -model "google/gemini-2.0-flash-exp" -v "say hi"
```

### 4) 自托管 vLLM / SGLang

启动一个 vLLM server：
```bash
vllm serve meta-llama/Llama-3.3-70B-Instruct --port 8000
```
然后：
```bash
export OPENAI_API_KEY=dummy
go run . -provider local -model "meta-llama/Llama-3.3-70B-Instruct" -v "say hi"
```
（`OPENAI_API_KEY` 在本地模式下只是占位；vLLM 默认不验证。）

### 5) 自定义 base URL

任何 OpenAI-compat 端点都可以：
```bash
go run . -provider openai -base-url "https://你自己的代理.com/v1" -v "say hi"
```

## 这背后是怎么实现的

**关键洞察**：我们的内部类型用 Anthropic content-block shape（`Message` 的 `Content []ContentBlock` 是 tagged union over `Type ∈ {"text", "tool_use", "tool_result"}`）；`OpenAIProvider` 在**provider 边界**做协议翻译。

```
Loop ─→ Provider iface ─→ AnthropicProvider ─→ api.anthropic.com (native)
                       │
                       └→ OpenAIProvider ─→ translateRequest → /chat/completions ─→ translateResponse
                              ↑                                                        ↓
                              └── 8KB 双向翻译，content-block ↔ messages+tool_calls ──┘
```

具体翻译规则（每节 s01-s10 的 provider_openai.go 都一致）：

- **请求**：Anthropic 的 `system` 字段变成 OpenAI 第一条 `{role: "system"}` message；assistant 的 `tool_use` block 变成 `tool_calls`；user 的 `tool_result` block 变成独立的 `{role: "tool", tool_call_id: ...}` message。
- **响应**：OpenAI 的 `tool_calls` 变回 `tool_use` content block；`finish_reason: stop|tool_calls|length` 映射到 `stop_reason: end_turn|tool_use|max_tokens`。
- **特殊处理**：DeepSeek 偶尔返回 content 为 `[{type:"text", text:"..."}]` 数组（而不是 string）——`contentToString` 兼容这种结构化 content。

## 排错

**"unknown -provider": `<name>`**：profile 拼错了。可选名字见上表。

**"<KEY> is not set"**：你忘了 export 对应 profile 的环境变量。

**`401 Unauthorized` from provider**：API key 错或过期。

**响应里 tool_use 不解析**：模型不支持 function-calling。换个支持的（gpt-4o-mini / claude / deepseek-chat / qwen-plus / llama-3.3-70b 都支持）。

**vLLM/SGLang 返回 404**：检查 `-base-url` 是否对（要带 `/v1`）；vLLM 默认是 `/v1/chat/completions`。

**输出截断**：用 `-max-turns 50` 或 main.go 加 `MaxTokens: 8192` 字段。

## 在测试里 mock

每节都有 `MockProvider` 用于测试：

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

Mock 不发任何网络请求；按顺序回放 fixture 直到耗尽（耗尽返回 error）。

如果你要测试**真实**的 Anthropic / OpenAI HTTP 行为（路径、headers、body），用 `httptest.NewServer` 搭假服务器并通过 `provider.baseURL = srv.URL` 注入。s01 的 `provider_test.go` 是模板。
