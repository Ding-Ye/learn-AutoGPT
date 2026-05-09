---
title: "s03 · LLM Provider 多后端"
chapter: 3
slug: s03-llm-provider
est_read_min: 14
---

# s03 · LLM Provider 多后端

> 教什么：把 s01/s02 已经写好的三个 Provider 实现（Anthropic 原生、OpenAI-compat 翻译层、Mock）拎出来真正讲透——内部 Anthropic-shape 一种，对外 8 种 wire format；为什么我们的 Go 版本不需要上游 `MultiProvider` 的 `_provider_instances` 懒缓存；并把测试文件按 target 重命名，让 `provider_anthropic_test.go` / `provider_openai_test.go` / `provider_mock_test.go` 三个文件名各司其职。

---

## Problem / 问题

s01 的 Loop 只认得一种 `Provider.CreateMessage` 调用。但真实用户拿不到 Anthropic key 的概率很高——他们手上更可能是 OpenAI、DeepSeek、Qwen、Moonshot、Groq、OpenRouter 的 key，或者本地起了个 vLLM/SGLang。AutoGPT classic 在 `classic/forge/forge/llm/providers/multi.py` 里用 `MultiProvider` 解决这个：维护一张 `CHAT_MODELS` dict 把 4 个后端（Anthropic / Groq / Llamafile / OpenAI）的所有模型聚合起来，按 `model_name` 路由到对应 provider，并用 `_provider_instances: dict[ModelProviderName, ChatModelProvider]` 做懒初始化缓存。

我们的问题清单不一样：
1. **不是支持 4 个 SDK，而是支持 1 个 native Anthropic + 7 个 OpenAI-compat** — 大多数现代 backend（DeepSeek、Qwen、Moonshot、Groq、OpenRouter、本地 vLLM）都讲 OpenAI Chat Completions 协议。我们把它们全归到一个 `OpenAIProvider` 里，用 base URL 区分。
2. **Loop 不应感知 provider** — `Loop.Run` 只调 `Provider.CreateMessage`；wire format 翻译要做完全在 Provider 内部做完，否则任何下游模块（s04 strategy、s05 history、s10 hooks）都得二次分支。
3. **测试名字要明确指向哪个 Provider** — s01/s02 时只有 `provider_test.go`，测的是 Anthropic。现在三个 Provider 实现共存，文件名得点名 target，否则一年后回来读代码连"这个测试在测哪个 provider" 都得 grep 才知道。

s03 的代码 *本身* 不是新的——`provider.go` / `provider_openai.go` / `provider_mock.go` 在 s01 就写好了。这一节真正的产出是这篇文档：把 30 行翻译层讲清楚，并把 `provider_test.go` 重命名为 `provider_anthropic_test.go`。

## Solution / 解决方案

**一种内部形状，多种线协议。** Loop 内部的 `Message` / `ContentBlock` 永远是 Anthropic 的 tagged-union 形状（`type: "text" | "tool_use" | "tool_result"`）。三个 Provider 在 `CreateMessage` 边界做翻译：

- `AnthropicProvider` — 内部就是 Anthropic shape，所以"翻译"是 identity。直接 `json.Marshal(req)` 就是合法的 Anthropic Messages API 请求体。
- `OpenAIProvider` — 翻译双向：
  - 出站：`translateRequestToOpenAI` 把内部 `Message` 拆成 OpenAI 的 `{role, content, tool_calls}` 列表，并把 `tool_use` blocks 提升到 assistant message 的 `tool_calls` 字段。
  - 入站：`translateResponseFromOpenAI` 把 `choices[0].message.tool_calls` 反向折回成 `ContentBlock{Type: "tool_use"}`，并把 `finish_reason` 映射回 `stop_reason`（`stop→end_turn`、`tool_calls→tool_use`、`length→max_tokens`）。
- `MockProvider` — 不调 HTTP，按构造时传入的响应队列依次返回，并记录每次的请求供测试断言。

**没有 lazy cache。** 上游 `MultiProvider._provider_instances: dict[...]` 是为了避免重复 `__init__` Python 对象（OpenAI SDK 的 client 可能要做 token 探测、credential 校验等"昂贵"事情）。Go 里 `NewAnthropicProvider(apiKey, model)` 就是填一个 struct + 创建一个 `*http.Client{Timeout: 120s}`——按微秒计费的成本，缓存反而增加复杂度。

**测试文件按 target 命名。** s03 这一节做的唯一文件名变更：把 s02 留下的 `provider_test.go` 改名为 `provider_anthropic_test.go`。三个 Provider 实现在同一 package，文件名直接告诉你"这个文件测的是哪个"。

## How It Works / 工作原理

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

**Request — Anthropic 原生**：

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

**Same request — OpenAI Chat Completions**：

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

**关键差异**：

| 维度 | Anthropic | OpenAI |
|---|---|---|
| `system` prompt | 顶层独立字段 | 顶层 messages[] 里一条 `role: "system"` |
| 用户消息 content | `[]ContentBlock` 数组（tagged union） | 单个字符串（或某些扩展是数组） |
| Assistant 调工具 | `content[]` 里出现 `type: "tool_use"` block | message 上挂 `tool_calls[]` 字段 |
| 工具参数 | 原生 JSON 对象 (`input: {...}`) | **JSON 编码成字符串** (`arguments: "{...}"`)！ |
| 工具结果回传 | user 消息里的 `tool_result` block | 独立的 `role: "tool"` 消息 |
| 工具定义 | 平铺 `{name, description, input_schema}` | 包一层 `{type: "function", function: {...}}` |
| 结束原因 | `stop_reason: "end_turn" / "tool_use" / "max_tokens"` | `finish_reason: "stop" / "tool_calls" / "length"` |

### 翻译层核心代码（节选自 `provider_openai.go`）

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

### 三个非显然之处

1. **Anthropic 一个 user 消息可以混 text + tool_result，OpenAI 不行** — 上面 `anthropicMessageToOpenAI` 的 user 分支：先把所有 text block 拼成一个 user 消息，再为每个 tool_result block 各自发一条 `role: "tool"` 消息。这是 **N→M 的扩展**，不是 1:1 翻译；翻译层必须意识到。
2. **`arguments` 是被 JSON 编码过两次的** — OpenAI 的 `function.arguments` 是一个 string，里面装着另一个 JSON 对象。所以出站要 `args, _ := json.Marshal(b.Input)` 然后 `Arguments: string(args)`；入站要 `json.Unmarshal([]byte(tc.Function.Arguments), &input)`。如果模型吐出的 arguments 不是合法 JSON（开源小模型偶尔会），我们把原文塞进 `_raw_arguments` 字段而不是丢弃——保留观测性。
3. **`finish_reason` 默认 `end_turn` 是兜底** — 有些 OpenAI-compat 后端（早期 DeepSeek、某些 vLLM 版本）会塞 `function_call`（旧式 single-function 协议）甚至空字符串。映射表的 `default: "end_turn"` 让 Loop 优雅退出而不是 panic。

## What Changed / 与 s02 的变化

```diff
 agents/s03-llm-provider/
 ├── provider.go              # 与 s02 一字不差
 ├── provider_openai.go       # 与 s02 一字不差
 ├── provider_mock.go         # 与 s02 一字不差
-├── provider_test.go         # ← 旧名字（s01/s02）
+├── provider_anthropic_test.go  # ← s03 的唯一文件名变更：明确指向 Anthropic
 ├── provider_openai_test.go  # 与 s02 一字不差
 ├── provider_mock_test.go    # 与 s02 一字不差
 ├── tools.go / registry.go / loop.go    # 与 s02 一字不差
 ├── main.go                   # usage string 的 "s02-command-registry" → "s03-llm-provider"
 └── README.md                 # 更新章节定位为"教学聚焦在翻译层"
```

**没有 API 变化、没有类型重命名、没有签名调整。** s03 是纯文档章节——把 30 行翻译层讲透，并把测试文件按 target 拆名。s01 已经能跑、s02 已经能跑、s03 也能跑，且 8 个 profile 一个不少。

新增 `TestAnthropicProvider_DefaultsModelAndMaxTokens` 测试用例，覆盖"调用方留空 Model/MaxTokens 时 Provider 必须填默认值"的契约——这个 default 链路 s01/s02 时漏测了。

## Try It / 动手试一试

```bash
cd agents/s03-llm-provider

# 默认 Anthropic 后端
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "use the math tool to add 7 and 35"

# DeepSeek（OpenAI-compat 路径，看翻译层在做事）
export DEEPSEEK_API_KEY=sk-...
go run . -provider deepseek -v "use math to multiply 6 and 7"

# Qwen via DashScope（同样 OpenAI-compat，只是 base URL 不同）
export DASHSCOPE_API_KEY=sk-...
go run . -provider qwen -v "echo back hi"

# 本地 vLLM / SGLang（任何 OpenAI-compat 服务都行）
export OPENAI_API_KEY=any-string-its-not-checked-locally
go run . -provider local -base-url http://localhost:8000/v1 -model llama-3.3 "what is 9 / 2"

# 跑全部测试（应该 37 通过）
go test -v ./...
```

期望输出形态（DeepSeek 路径）：

```
[s03-llm-provider] provider=deepseek model=deepseek-chat url=https://api.deepseek.com/v1 tools=2
[turn 0] assistant: I'll use the math tool to multiply.
[turn 0] -> math map[a:6 b:7 operation:mul]
[turn 0] <- 42
[turn 1] assistant: 6 × 7 = 42.
6 × 7 = 42.
```

如果 OpenAI-compat 后端把 `arguments` 字段塞了非 JSON 字符串（开源小模型偶现），你会看到 tool 报 `missing required field "operation"` 之类的错误，并被 Loop 翻译成 `tool_result: "tool error: ..."` 喂回模型——下一轮模型会重新生成合法的 tool call。这是 `_raw_arguments` 兜底路径的设计目的。

## Upstream Source Reading / 上游源码阅读

AutoGPT classic 在 `classic/forge/forge/llm/providers/` 下放了五个文件：`schema.py`（共享类型）、`anthropic.py` / `openai.py` / `groq.py` / `llamafile.py`（四个具体 provider）、`multi.py`（聚合层）。我们重点看 `multi.py`——上游怎么把多 provider 摆在同一个抽象后面。

```upstream:classic/forge/forge/llm/providers/multi.py
# Source: classic/forge/forge/llm/providers/multi.py
# 简化：去掉 generic typing 与 logging，保留聚合 + 路由 + 缓存的核心逻辑。

from .anthropic import ANTHROPIC_CHAT_MODELS, AnthropicProvider
from .groq import GROQ_CHAT_MODELS, GroqProvider
from .llamafile import LLAMAFILE_CHAT_MODELS, LlamafileProvider
from .openai import OPEN_AI_CHAT_MODELS, OpenAIProvider

# 关键 1 ── 全部 backend 的模型聚合到一张 dict 里。
# key 是 model_name (e.g. "gpt-4o-mini", "claude-sonnet-4-6"),
# value 是 ChatModelInfo（带 provider_name 字段，告诉路由层去哪个 provider）。
CHAT_MODELS = {
    **ANTHROPIC_CHAT_MODELS,
    **GROQ_CHAT_MODELS,
    **LLAMAFILE_CHAT_MODELS,
    **OPEN_AI_CHAT_MODELS,
}


class MultiProvider(BaseChatModelProvider):
    # 关键 2 ── lazy cache。第一次用某个 provider 才 __init__ 它，
    # 之后从 dict 拿现成实例。Python 实例化 OpenAI client 涉及
    # credential 校验等 IO，所以缓存值得花。
    _provider_instances: dict[ModelProviderName, ChatModelProvider]

    def __init__(self, settings=None, logger=None):
        super().__init__(settings=settings, logger=logger)
        self._budget = self._settings.budget or ModelProviderBudget()
        self._provider_instances = {}

    # 关键 3 ── 单一入口。create_chat_completion 接 model_name，
    # 路由到对应 provider，把所有参数原样转发。
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

    # 关键 4 ── 根据 model_name 找 provider_name，再去懒缓存里取。
    def get_model_provider(self, model: ModelName) -> ChatModelProvider:
        model_info = CHAT_MODELS[model]
        return self._get_provider(model_info.provider_name)

    def _get_provider(self, provider_name) -> ChatModelProvider:
        # 缓存命中：直接返回。
        provider = self._provider_instances.get(provider_name)
        if provider is not None:
            return provider

        # 缓存未命中：实例化、注入 budget、写回缓存。
        Provider = self._get_provider_class(provider_name)  # 字典查类
        settings = Provider.default_settings.model_copy(deep=True)
        settings.budget = self._budget
        # ... credential loading from env via Pydantic settings ...
        provider = Provider(settings=settings, logger=self._logger)
        self._provider_instances[provider_name] = provider
        return provider

    @classmethod
    def _get_provider_class(cls, provider_name):
        # provider_name → class 的硬编码映射。
        return {
            ModelProviderName.ANTHROPIC: AnthropicProvider,
            ModelProviderName.GROQ: GroqProvider,
            ModelProviderName.LLAMAFILE: LlamafileProvider,
            ModelProviderName.OPENAI: OpenAIProvider,
        }[provider_name]
```

```upstream:classic/forge/forge/llm/providers/openai.py
# Source: classic/forge/forge/llm/providers/openai.py
# 简化：只显示构造函数和参数构建钩子；翻译层在父类 BaseOpenAIChatProvider 里。

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

### 对照阅读要点

- **聚合方式不同**：上游用一张 `CHAT_MODELS` dict 把所有后端的模型聚合到一起，按 `model_name → provider_name` 二级路由。我们直接在 `main.go` 用 `-provider` flag 显式选 backend，没有 model 名到 provider 名的间接层。原因：上游有 4 个原生 SDK（每家都有自己的认证、tokenizer、错误码），需要硬路由；我们 7/8 后端共享 OpenAI Chat Completions 协议，base URL 就够了。
- **懒缓存这一层我们不要**：上游 `_provider_instances: dict` 在第一次用某个 provider 才 `__init__`。Python 的 OpenAI client 构造涉及 credential 探测、retry 配置等 IO 工作；Go 的 `NewAnthropicProvider(apiKey, model)` 就是填一个 struct + 一个 `&http.Client{Timeout: 120s}`——纳秒级开销，缓存只增加复杂度。
- **翻译层在哪里**：上游 `OpenAIProvider` 继承的是 `BaseOpenAIChatProvider`——翻译层在父类 `_get_chat_completion_args` 里，把 `ChatMessage` / `CompletionModelFunction` 转成 OpenAI SDK 期望的 `ChatCompletionMessageParam` / `CompletionCreateParams`。我们把翻译做在 `provider_openai.go` 同一文件里，不再继承层层套娃——80 行 Go 代码读完整条出入站路径都能看到。
- **`_functions_compat_fix_kwargs` 兼容老模型**：上游对 *没有* function-calling API 的模型有降级路径，把工具定义打成 TypeScript namespace 注入 system prompt。我们没做这个——所有现代 OpenAI-compat 后端都支持 `tool_calls`，开源 llama-3.3 也支持。等真碰到需要降级的场景，s04 的 prompt strategy 是更合适的扩展点。
- **litellm 没用**：研究笔记 risk #1 提到的——AutoGPT 上游通过 `litellm` 多套了一层 LLM 路由（用于 `OpenRouter` 等）。Go 没有 `litellm` 等价物，但因为 OpenRouter 本身就是 OpenAI-compat 协议，`-provider openrouter` 直接跑通。

**想读更多**：从 `classic/forge/forge/llm/providers/multi.py::MultiProvider` 开始，跟着 `_get_provider` 进入 `openai.py::OpenAIProvider.__init__`，再读 `BaseOpenAIChatProvider._get_chat_completion_args`（在 `_openai_base.py`）看真实的翻译实现——你会发现上游的翻译层比我们的复杂得多，主要是因为它要支持 streaming、reasoning_effort、structured outputs 这些我们 mini 版还没引入的特性。

---

**下一节预告**：s04 把 prompt 构建从 Loop 里剥离出来。s01-s03 的 Loop 直接把 user prompt 喂给 Provider；s04 引入 `PromptStrategy` 接口，由 strategy 决定怎么把 history、tool list、user task 渲染成 messages，并由 strategy 解析模型输出。这是 AutoGPT classic 的 8 种 prompt strategy 的入口（我们只实现 `OneShotStrategy`，Reflexion 留到 s10）。
