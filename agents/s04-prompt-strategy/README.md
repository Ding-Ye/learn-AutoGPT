# s04 · Prompt strategies & response parsing

> **zh** s01-s03 把 user prompt 直接塞给 Provider；s04 把 prompt 构建提到 `PromptStrategy` 接口后面：strategy 决定 system prompt（角色 + 工具列表 + best-practices）和 messages，并负责把响应解析成 `ActionProposal`（含 native `tool_use` 与 ```json 围栏回退两条路径）。这是 AutoGPT classic 8 个 prompt 策略的入口；我们只实现 `OneShotStrategy`，Reflexion 留给 s10。
> **en** s01-s03 fed the user prompt straight to the Provider. s04 lifts prompt construction behind a `PromptStrategy` interface: the strategy decides the system prompt (role + tool list + best-practices) and the initial messages, then parses the response into an `ActionProposal` — supporting both native `tool_use` blocks and a ```json fenced-code fallback. This is the seam to AutoGPT classic's 8-strategy menagerie; we ship only `OneShotStrategy` here. Reflexion arrives in s10.

## Files

| file | role |
|---|---|
| `provider.go` / `provider_openai.go` / `provider_mock.go` | three Provider impls — verbatim from s03 |
| `provider_anthropic_test.go` / `provider_openai_test.go` / `provider_mock_test.go` | provider tests — verbatim |
| `tools.go` / `tools_test.go` | `EchoTool` + `MathTool` — verbatim |
| `registry.go` / `registry_test.go` | `Registry` — verbatim |
| `strategy.go` | **NEW** — `PromptStrategy` iface + `OneShotStrategy` + `ActionProposal` + `Episode` placeholder + 5 best-practices |
| `strategy_test.go` | **NEW** — 8 tests covering BuildSystem rendering, BuildPrompt shape, ParseResponse native + JSON fallback paths |
| `loop.go` | **MODIFIED** — `Loop.Strategy PromptStrategy`; `Run` calls `strategy.BuildPrompt` to construct initial messages and `strategy.ParseResponse` on each tool turn |
| `loop_test.go` | s03 tests + 1 new `stubStrategy` test verifying the strategy is invoked |
| `main.go` | adds `-strategy oneshot` flag (the only option in s04; reserved for s10's `reflexion`) |
| `testdata/golden_response.json` | sample Anthropic-shape response with native `tool_use` |
| `testdata/golden_response_json_fallback.json` | sample response where the model emits ```json `{"command":...,"args":...}` instead of native tool_use |

## Run / 运行

```bash
cd agents/s04-prompt-strategy

# Anthropic native + oneshot (default)
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "use the math tool to add 7 and 35"

# DeepSeek + oneshot (smaller models more often need the JSON-fence fallback)
export DEEPSEEK_API_KEY=sk-...
go run . -provider deepseek -v "echo back hi"

# Local vLLM / SGLang / llama.cpp on localhost
export OPENAI_API_KEY=anything
go run . -provider local -model llama-3.3 "compute 6 * 7"
```

## Test / 测试

```bash
go test -v ./...
```

Expect ~36 tests across 9 files: tools (echo + math), registry, loop (5 + 1 strategy invocation), three provider tests (anthropic httptest + openai translation + mock), strategy (8 covering BuildSystem rendering + ParseResponse native and fallback paths).

## Key teaching points / 学习要点

1. **One seam, two parsers / 一个接口，两条解析路径** — `PromptStrategy.ParseResponse` tries native `tool_use` first, falls back to ```json fenced-code parsing. Smaller open-weight models (older llama, some vLLM checkpoints) emit fenced JSON instead of true tool_calls; the fallback keeps them usable. 小模型经常用围栏式 JSON 替代 native tool_use；这条回退让它们仍可用。
2. **System prompt vs messages / 系统提示与消息分离** — Anthropic carries `system` as a top-level request field (not a Message). `OneShotStrategy.BuildSystem` returns the string; `BuildPrompt` returns []Message. The Loop wires both. 上游 system 是顶层字段，不是消息；strategy 通过两个方法分别返回。
3. **5 best-practices, not 7 / 五条最佳实践，不是七条** — upstream's `OneShotAgentPromptConfiguration.DEFAULT_BODY_TEMPLATE` ships 7 efficiency guidelines. We trim to 5 — items 6 (CODE STYLE) and 7 (SECURITY) only become meaningful once s06's Workspace and s08's web/file components arrive. 上游 7 条；我们留 5 条，s06/s08 引入 Workspace 与组件后再加最后两条。
4. **Episode placeholder / Episode 占位符** — s04's `BuildPrompt` signature already accepts `[]*Episode`. The struct is empty; s05 fills it in. This is a forward-compat seam — we don't change strategy's signature in s05. s04 接口已经接受 `[]*Episode`，但 struct 是空的；s05 才填字段。
5. **No prefill, no Pydantic validation / 不做 prefill 不做 schema 校验** — upstream uses Anthropic's `prefill_response` mechanism plus Pydantic `OneShotAgentActionProposal.model_validate` to coerce the JSON. Our `ActionProposal` is a flat struct with `map[string]interface{}` for Args. Tools handle their own input validation in `Execute`. 上游用 prefill + Pydantic 校验；我们靠 tool 自己 validate input。

## Read more / 深入阅读

- 中文：[`docs/zh/s04-prompt-strategy.md`](../../docs/zh/s04-prompt-strategy.md)
- English: [`docs/en/s04-prompt-strategy.md`](../../docs/en/s04-prompt-strategy.md)
- Upstream excerpt: [`upstream-readings/s04-prompt-strategy.py`](../../upstream-readings/s04-prompt-strategy.py)
