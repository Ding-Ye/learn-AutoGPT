# s02 · explicit command registry

> **zh** s01 把 tool 列表硬编码成一个 `[]Tool{NewEchoTool()}`。这一节把它替换成显式的 `Registry`：任何文件都能 `reg.Register(myTool)`，名字唯一、顺序稳定、找不到也不会崩。
> **en** s01 hard-coded the tool list as `[]Tool{NewEchoTool()}`. This chapter replaces it with an explicit `Registry`: any file can call `reg.Register(myTool)`, names are unique, order is deterministic, missing names recover gracefully.

## Files

| file | role |
|---|---|
| `provider.go` / `provider_openai.go` / `provider_mock.go` | verbatim copies from s01 — locked Provider contract |
| `tools.go` | s01's `EchoTool` + s02's new `MathTool` (operation: add/sub/mul/div) |
| `registry.go` | NEW — `Registry.Register / Lookup / All`; insertion-order preserving |
| `loop.go` | s01's loop with `Tools []Tool` → `Tools *Registry`; dispatch via `Lookup` |
| `main.go` | constructs the Registry explicitly: `reg.Register(NewEchoTool())` etc. |
| `testdata/golden_response.json` | sample tool_use response choosing the math tool |

## Run / 运行

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s02-command-registry

# default Anthropic — let the model pick echo or math
go run . -v "add 2 and 3 using the math tool"

# OpenAI-compatible (DeepSeek as example)
export DEEPSEEK_API_KEY=...
go run . -provider deepseek -v "echo back 'hi'"
```

## Test / 测试

```bash
go test -v ./...
```

Expect ~22 tests across 6 files: tools (echo + math), registry, loop, anthropic provider, openai-compat translators, mock provider.

## Key teaching points / 学习要点

1. **Explicit > implicit registration / 显式注册胜过隐式装饰器** — AutoGPT classic uses `@command` (Python decorator) so any function is auto-discovered. Go has no decorators; even if it did, decorator-based registration hides dependencies. Every `Register(...)` call in `main.go` is a visible dependency edge — grep finds them. 装饰器把 dependency 藏在 import 顺序里；显式 Register 让依赖关系在源代码里 grep 得到。
2. **Names are the contract, not pointers / 名字才是契约，不是指针** — the model emits `block.Name`; the registry resolves it to a Tool. Two tools means the resolution is observable: with one tool, "always return the only one" and "look up by name" are indistinguishable. 必须有两个 tool 才看得出 Registry 在做事。
3. **Lookup returns `(Tool, bool)`, not `(Tool, error)` / Lookup 返回布尔，不返错** — "not found" is a routine condition the caller decides what to do with (Loop turns it into a friendly tool_result for the model to recover from). It's not Lookup's failure. 找不到不是 Lookup 的"错"，是 caller 要分支处理的常态。
4. **Insertion order matters / 插入顺序要保留** — `All()` returns schemas in the order they were registered. Tool lists shown to the model depend on order being stable, and golden tests pin it. 给模型的 tool 列表顺序要稳定，否则 prompt 会抖动。
5. **Duplicate names error, not overwrite / 重名报错，不静默覆盖** — silent overwrites cause "I added foo, why is the old foo running?" mysteries. The registry refuses, loudly. 重名静默覆盖最难调试；显式拒绝。

## Read more / 深入阅读

- 中文：[`docs/zh/s02-command-registry.md`](../../docs/zh/s02-command-registry.md)
- English: [`docs/en/s02-command-registry.md`](../../docs/en/s02-command-registry.md)
- Upstream excerpt: [`upstream-readings/s02-command.py`](../../upstream-readings/s02-command.py)
