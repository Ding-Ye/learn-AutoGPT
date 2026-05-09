# s09 · Continuous mode & UI feedback

> **zh** s01–s08 的 `Loop.Run` 是一个有界单次循环（跑到 end_turn 或撞 MaxTurns 就停）。s09 在它外面包一层 `RunInteractionLoop`：cycle 预算（`-cycles N`，0 = 无限）、SIGINT 信号处理（`os/signal.Notify` → 取消 ctx）、可选每步审批（`-ask-each-step`）、UI 反馈接口（`UIProvider` 的 `Spinner`/`RenderThought`/`RenderResult`）。从 `Loop.Run` 抽出 `runStep` 方法供包装器复用，老 `Run` 行为完全不变。对标上游 `app/main.py:655-768` 的 `run_interaction_loop`。
> **en** s01–s08's `Loop.Run` is a bounded single-loop driver (stops at end_turn or MaxTurns). s09 wraps it with `RunInteractionLoop`: cycle budgeting (`-cycles N`, 0 = infinite), SIGINT handling (`os/signal.Notify` → ctx cancel), optional per-step approval (`-ask-each-step`), and a `UIProvider` interface (`Spinner` / `RenderThought` / `RenderResult`). Extracts a `runStep` method from `Loop.Run` so the wrapper reuses one step impl; `Run` itself stays bug-for-bug compatible. Mirrors upstream `app/main.py:655-768`'s `run_interaction_loop`.

## Files

| file | role |
|---|---|
| `provider.go` / `provider_openai.go` / `provider_mock.go` | three Provider impls — verbatim from s08 |
| `provider_anthropic_test.go` / `provider_openai_test.go` / `provider_mock_test.go` | provider tests — verbatim |
| `tools.go` / `tools_test.go` | `EchoTool` + `MathTool` — verbatim |
| `tools_file.go` / `tools_file_test.go` | `ReadFileTool` + `WriteFileTool` — verbatim |
| `workspace.go` / `workspace_test.go` | `Workspace` iface + `LocalWorkspace` — verbatim |
| `registry.go` / `registry_test.go` | `Registry` — verbatim |
| `history.go` / `history_test.go` | `Episode` / `History` — verbatim |
| `permissions.go` / `permissions_test.go` | permissions — verbatim |
| `strategy.go` / `strategy_test.go` | OneShot strategy — verbatim |
| `component.go` / `component_test.go` | `Component` marker + bus — verbatim |
| `component_filemgr.go` / `_test.go` | `FileManagerComponent` — verbatim |
| `component_web.go` / `_test.go` | `WebFetchComponent` — verbatim |
| `loop.go` | **MODIFIED** — extracts `runStep` method; `Run` still s08-compatible |
| `loop_test.go` | verbatim from s08 (Run still works) |
| `interaction_loop.go` | **NEW** — `RunInteractionLoop` + signal handler + cycle counter |
| `interaction_loop_test.go` | **NEW** — 6 tests |
| `ui.go` | **NEW** — `UIProvider` iface + `ConsoleUI` + `NoopUI` |
| `ui_test.go` | **NEW** — 4 tests |
| `main.go` | **MODIFIED** — adds `-cycles N` and `-ask-each-step`; calls `RunInteractionLoop` |
| `testdata/golden_response.json` | sample Anthropic-shape response |
| `testdata/permissions.yaml` | sample permissions config |

## Run / 运行

```bash
cd agents/s09-continuous-mode

# Single Run (cycles=0 + immediate end_turn) — same shape as s08
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "say hi"

# Continuous mode: 5 steps then stop
go run . -v -cycles 5 \
    "research the AutoGPT classic README and write a 3-line summary to summary.md"

# Per-step operator approval
go run . -v -cycles 10 -ask-each-step -ask stdin \
    "fetch https://example.com and write the title to notes.md"

# Infinite + Ctrl-C exit
go run . -v -cycles 0 \
    "loop forever; Ctrl-C me"
```

## Test / 测试

```bash
go test -v ./...
```

Adds **interaction_loop (6)** + **ui (4)** on top of s08's inheritance.

## Key teaching points / 学习要点

1. **`runStep` extraction** — `Loop.Run`'s body is split into `runStep(ctx, args) (stepResult, error)` (one think→act→observe step) and a thin loop wrapper. `RunInteractionLoop` reuses `runStep` directly so cycle budgeting + signals + UI add zero duplication. 抽出 runStep 让两种循环驱动方式共享步骤实现。
2. **Channel-based signal handling** — `signal.Notify(sigCh, os.Interrupt)` + a goroutine that `cancel()`s a wrapper-local ctx. Compared to Python's `signal.signal` + `nonlocal` mutations: no shared-state coordination, no "last writer wins" registry collision, `defer signal.Stop` for clean teardown. Channel-based 信号比 nonlocal 全局处理器干净得多。
3. **The cycle-budget rule** — `cyclesLeft--` ONLY when no result has Status=="interrupted_by_human". A user-vetoed step doesn't burn budget; matches upstream's load-bearing `if result.status != "interrupted_by_human": cycles_remaining -= 1`. 否决的步骤不消耗预算。
4. **`UIProvider` returns a stop fn** — `stop := ui.Spinner("..."); defer stop()` is Go's idiomatic answer to Python's `async with ui_provider.show_spinner(...)`. The stop fn uses `sync.Once` for idempotency so error paths are safe. Spinner 用「返回 stop」+ `sync.Once` 取代 async with。
5. **Tests simulate signals via ctx-cancel** — sending real SIGINT in unit tests is flaky; we cancel the ctx directly because `signal → cancel` is one inspectable line. The signal-to-cancel hop is tested by code reading; the cancel-to-exit logic is tested by ctx-cancel injection. 单元测试不发真 SIGINT,直接 cancel ctx。

## Read more / 深入阅读

- 中文：[`docs/zh/s09-continuous-mode.md`](../../docs/zh/s09-continuous-mode.md)
- English: [`docs/en/s09-continuous-mode.md`](../../docs/en/s09-continuous-mode.md)
- Upstream excerpt: [`upstream-readings/s09-interaction-loop.py`](../../upstream-readings/s09-interaction-loop.py)
