# s05 · Episodic action history

> **zh** s04 的 Loop 把 messages 当成扁平累积器；s05 把它换成 `History []*Episode`：每个 tool 轮 Append 一个 Episode，记录 `Actions []ActionProposal` 与 `Results []ActionResult`，下一轮的 BuildPrompt 通过 `RenderMessages()` 重建对话。这就是 AutoGPT 上游 `EpisodicActionHistory.prepare_messages` 的最小可教学版本——压缩钩子留作 (advanced) 注释，等 context 撑爆再来填。
> **en** s04's Loop treated messages as a flat accumulator. s05 replaces that with `History []*Episode`: each tool turn appends an Episode that captures `Actions []ActionProposal` and `Results []ActionResult`, and the next turn's BuildPrompt rebuilds the conversation via `RenderMessages()`. This is the minimum-viable take on AutoGPT upstream's `EpisodicActionHistory.prepare_messages` — compression is left as an `(advanced)` comment, fill it in once your context overflows.

## Files

| file | role |
|---|---|
| `provider.go` / `provider_openai.go` / `provider_mock.go` | three Provider impls — verbatim from s04 |
| `provider_anthropic_test.go` / `provider_openai_test.go` / `provider_mock_test.go` | provider tests — verbatim |
| `tools.go` / `tools_test.go` | `EchoTool` + `MathTool` — verbatim |
| `registry.go` / `registry_test.go` | `Registry` — verbatim |
| `strategy.go` | s04's strategy with one tweak: `BuildPrompt` now folds `history.RenderMessages()` ahead of the task message |
| `strategy_test.go` | verbatim from s04 (BuildSystem / BuildPrompt / ParseResponse paths) |
| `history.go` | **NEW** — `Episode`, `ActionResult`, `History` + `Append` / `Current` / `RenderMessages` / `TrimToLastN` |
| `history_test.go` | **NEW** — 5 tests (Append+Current; chronological order; empty→[]; mid-turn render; TrimToLastN) |
| `loop.go` | **MODIFIED** — `Loop.History *History`; per-turn `Append` of an Episode + record proposal + record result |
| `loop_test.go` | s04 tests adapted to the new render shape + 1 new `TestLoop_HistoryGrowsAfterEachTurn` |
| `main.go` | constructs `&History{}` and threads it into `Loop`; usage string updated |
| `testdata/golden_response.json` | sample Anthropic-shape response with native `tool_use` |
| `testdata/golden_response_json_fallback.json` | sample response with the JSON-fenced fallback path |

## Run / 运行

```bash
cd agents/s05-episodic-history

# Anthropic native + oneshot (default)
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "add 7 and 35, then echo the result"

# DeepSeek + oneshot
export DEEPSEEK_API_KEY=sk-...
go run . -provider deepseek -v "compute 6 * 7, then echo it"

# Local vLLM / SGLang / llama.cpp on localhost
export OPENAI_API_KEY=anything
go run . -provider local -model llama-3.3 "echo hi, then echo bye"
```

## Test / 测试

```bash
go test -v ./...
```

Expect ~37 tests across 9 files: tools (echo + math), registry, loop (5 + 1 history-grows + 1 strategy invocation), three provider tests (anthropic httptest + openai translation + mock), strategy (8), history (5).

## Key teaching points / 学习要点

1. **Episode is the compression unit / Episode 是压缩单位** — upstream's `EpisodicActionHistory` treats episodes as the *only* unit of summarization; we keep that boundary so when you eventually wire compression in, the seam is already at the right granularity. 上游一个 episode 一个总结点，s05 也保持同样粒度。
2. **`(advanced)` comment is the seam / 那行注释就是钩子** — `history.go` opens with a one-line `// (advanced) when context overflows, summarize old episodes here.` That's where AutoGPT's `handle_compression()` would land. We deliberately leave it empty. 压缩钩子留作进阶练习。
3. **`Loop.History` is nil-safe / Loop 的 History 字段允许零值** — `&Loop{Provider: p, Tools: r, Strategy: s, MaxTurns: 5}` (no History) still works because `Run` allocates a fresh `History{}` if nil. The s04 construction style keeps compiling. 旧的构造方式继续兼容。
4. **`Append` mutates / Current returns a pointer** — `History.Append` uses a pointer receiver to grow the slice in place; `Current()` returns the *Episode pointer so the Loop can mutate Actions/Results without re-fetching. 慎重选择指针接收器是为了让 Loop 的"先 append 提案，后 append 结果"在 in place 完成。
5. **`TrimToLastN` is exercised but unused / 暴露 TrimToLastN 但 Loop 不用它** — it's the pedagogical "this is where you'd plug in compression". The test runs it; the Loop never does. 测试跑它，Loop 不依赖它——压缩钩子留给读者填。

## Read more / 深入阅读

- 中文：[`docs/zh/s05-episodic-history.md`](../../docs/zh/s05-episodic-history.md)
- English: [`docs/en/s05-episodic-history.md`](../../docs/en/s05-episodic-history.md)
- Upstream excerpt: [`upstream-readings/s05-action-history.py`](../../upstream-readings/s05-action-history.py)
