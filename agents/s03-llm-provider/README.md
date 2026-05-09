# s03 · LLM Provider with multiple backends

> **zh** s01 已经写好 Anthropic + OpenAI-compat 两个 Provider 实现，s03 不再加新代码——把多后端的 *教学* 真正做透：解释 `OpenAIProvider` 里 30 行翻译层做什么、为什么 Go 版不需要上游 `MultiProvider` 的 lazy 缓存、把 `provider_test.go` 拆成 `provider_anthropic_test.go` 让三个 provider 的测试文件名各司其职。
> **en** s01 already shipped both Anthropic and OpenAI-compat Provider implementations. s03 adds no new code paths — the *teaching* is what's new: deep-dive the 30-line translation layer in `OpenAIProvider`, explain why our Go version skips upstream `MultiProvider`'s lazy cache, and split `provider_test.go` into `provider_anthropic_test.go` so each provider's test file is named for its target.

The chapter doc (`docs/{zh,en}/s03-llm-provider.md`) is the headline deliverable. The code below is the full self-contained snapshot the doc references.

## Files

| file | role |
|---|---|
| `provider.go` | `Provider` interface + `AnthropicProvider` (native) — verbatim from s02 |
| `provider_openai.go` | `OpenAIProvider` — Anthropic↔OpenAI translation layer |
| `provider_mock.go` | `MockProvider` — fixture replay for tests |
| `provider_anthropic_test.go` | **NEW NAME** (s02 had `provider_test.go`) — Anthropic httptest round-trip |
| `provider_openai_test.go` | OpenAI translation tests (request + response shapes) |
| `provider_mock_test.go` | Mock provider playback + exhaustion tests |
| `tools.go` | `Tool` iface + `EchoTool` + `MathTool` |
| `registry.go` | `Registry.Register / Lookup / All` |
| `loop.go` | the agent loop, dispatching tools via Registry |
| `main.go` | full 8-profile picker — see profiles below |
| `testdata/golden_response_openai.json` | sample OpenAI response (with `tool_calls`) |
| `testdata/golden_response_anthropic.json` | sample Anthropic response (with `tool_use` block) |

## 8 provider profiles / 8 个后端档案

| `-provider` | base URL | env var | default model |
|---|---|---|---|
| `anthropic` | `https://api.anthropic.com` (native) | `ANTHROPIC_API_KEY` | `claude-sonnet-4-6` |
| `openai` | `https://api.openai.com/v1` | `OPENAI_API_KEY` | `gpt-4o-mini` |
| `deepseek` | `https://api.deepseek.com/v1` | `DEEPSEEK_API_KEY` | `deepseek-chat` |
| `moonshot` | `https://api.moonshot.cn/v1` | `MOONSHOT_API_KEY` | `moonshot-v1-8k` |
| `qwen` | `https://dashscope.aliyuncs.com/compatible-mode/v1` | `DASHSCOPE_API_KEY` | `qwen-plus` |
| `groq` | `https://api.groq.com/openai/v1` | `GROQ_API_KEY` | `llama-3.3-70b-versatile` |
| `openrouter` | `https://openrouter.ai/api/v1` | `OPENROUTER_API_KEY` | `openai/gpt-4o-mini` |
| `local` | `http://localhost:8000/v1` | `OPENAI_API_KEY` | `local-model` |

## Run / 运行

```bash
cd agents/s03-llm-provider

# Anthropic native
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "add 7 and 35 using the math tool"

# Any OpenAI-compat backend (DeepSeek shown; same goes for openai/qwen/moonshot/groq/openrouter/local)
export DEEPSEEK_API_KEY=sk-...
go run . -provider deepseek -v "echo back 'hi'"

# Self-hosted vLLM / SGLang / llama.cpp on localhost
export OPENAI_API_KEY=sk-anything
go run . -provider local -model llama-3.3 "compute 6 * 7"
```

## Test / 测试

```bash
go test -v ./...
```

Expect ~28 tests across 8 files: tools (echo + math), registry, loop, anthropic provider (3 httptest cases), openai-compat translators (10 cases for both directions), mock provider.

## Key teaching points / 学习要点

1. **One internal shape, many wire formats / 内部一种形状，对外多种线协议** — internal `Message` / `ContentBlock` is Anthropic-flavored. `OpenAIProvider.CreateMessage` translates at the wire boundary so Loop, Tools, Strategy never branch on which provider is in use. 内部统一是 Anthropic 的 block 联合体；OpenAIProvider 在边界做翻译，Loop 不感知。
2. **No lazy provider cache / 不需要懒加载缓存** — upstream `MultiProvider` has a `_provider_instances: dict[ModelProviderName, ChatModelProvider]` that lazily inits each backend on first use. Go zero-init structs cost ~nothing; `NewAnthropicProvider(...)` is microseconds. We just construct directly. 上游用 dict 懒缓存 provider 实例；Go 零值初始化成本为零，直接构造即可。
3. **Three test files, three responsibilities / 三个测试文件，三个角色** — `provider_anthropic_test.go` (renamed from s02's `provider_test.go`), `provider_openai_test.go`, `provider_mock_test.go`. With three Provider impls in one package, naming each test file after its target avoids the "wait, which provider does this exercise?" wobble. 三个 Provider 实现共存时，按 target 命名测试文件比 `provider_test.go` 清晰。
4. **`finish_reason` mapping is the smallest surface that matters / `finish_reason` 映射是翻译层最小的关键面** — `"stop"→"end_turn"`, `"tool_calls"→"tool_use"`, `"length"→"max_tokens"`. Get this 3-row table wrong and Loop's switch hits the default branch on every turn. 这 3 行映射写错，Loop 每一轮都会落到 default 分支报错。
5. **`baseURL` lives as a struct field, not a constant / `baseURL` 是字段不是常量** — both providers carry `baseURL string`. Production fills it from env, tests inject `httptest.NewServer.URL`. The plan's catalogued httptest pattern depends on this seam being there from s01 onward. 测试要靠 httptest 注入 URL，所以 `baseURL` 必须是 struct 字段而不是常量。

## Read more / 深入阅读

- 中文：[`docs/zh/s03-llm-provider.md`](../../docs/zh/s03-llm-provider.md)
- English: [`docs/en/s03-llm-provider.md`](../../docs/en/s03-llm-provider.md)
- Upstream excerpt: [`upstream-readings/s03-multi-provider.py`](../../upstream-readings/s03-multi-provider.py)
