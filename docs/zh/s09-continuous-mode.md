---
title: "s09 · 持续运行模式与 UI"
chapter: 9
slug: s09-continuous-mode
est_read_min: 13
---

# s09 · 持续运行模式与 UI

> 教什么：把 s01–s08 的「单次 Run」升级成「N 轮自主 + Ctrl-C 优雅退出 + UI 反馈」。引入 `LoopOpts{Cycles, AskEachStep, OnInterrupt}` 与顶层包装器 `RunInteractionLoop(ctx, *Loop, UIProvider, LoopOpts)`：cycle 预算追踪、`os/signal.Notify` 信号处理（SIGINT → ctx.cancel）、`UIProvider` 接口（`RenderThought` / `RenderResult` / `Spinner`）。从 `Loop.Run` 抽出 `runStep` 方法供包装器复用，老的 `Run` 行为完全不变以保兼容。对标上游 `app/main.py:655-768` 的 `run_interaction_loop`。

---

## Problem / 问题

到 s08 末尾，`Loop.Run(ctx, prompt)` 是一个「跑到 end_turn 或撞到 MaxTurns 就停」的有界循环。但真实用户想要的是：

1. **N 轮自主执行**——告诉 agent「跑 5 步然后停」（`-cycles 5`），不需要每步都批一次；
2. **Ctrl-C 优雅退出**——按一下中断键，正在写到一半的文件不能留下半截；
3. **每步可选审批**——`-ask-each-step` 模式下，每步执行前问操作员一次「这步可以跑吗？」；
4. **UI 反馈**——在 LLM 思考时显示 spinner，思考结束后渲染思路、动作结果。

s08 的 Loop 没有这些。它只有一个硬 `MaxTurns`，没信号处理、没 UI 钩子。

AutoGPT classic 的答案在 `classic/original_autogpt/autogpt/app/main.py:655-768` 的 `run_interaction_loop`：

```python
async def run_interaction_loop(agent, ui_provider=None):
    cycle_budget = cycles_remaining = _get_cycle_budget(
        app_config.continuous_mode, app_config.continuous_limit
    )
    spinner = Spinner("Thinking...", ...)
    stop_reason = None

    def graceful_agent_interrupt(signum, frame):
        nonlocal cycles_remaining, stop_reason
        # ... two-stage interrupt: first lower cycles to 1, second raise stop_reason

    signal.signal(signal.SIGINT, graceful_agent_interrupt)

    while cycles_remaining > 0:
        handle_stop_signal()
        async with ui_provider.show_spinner("Thinking..."):
            action_proposal = await agent.propose_action()
        await ui_provider.display_thoughts(...)
        result = await agent.execute(action_proposal)
        if result.status != "interrupted_by_human":
            cycles_remaining -= 1
        await ui_provider.display_result(...)
```

三件事：cycle 预算、信号处理、Rich UI。Python 用 `signal.signal` 注册全局处理器（在信号投递线程上跑，用 `nonlocal` 改主循环的状态变量），用 `async with` 管理 spinner 上下文，用 Rich 库画 spinner。

s09 把这套搬到 Go——但用 Go 惯用的 channel-based 信号、`defer stop()` 替代 async with、最朴素的「[busy] ...」单行 spinner。

## Solution / 解决方案

```go
// 新增类型：
type LoopOpts struct {
    Cycles      int                    // 0 = infinite (until ctx cancel)
    AskEachStep bool                   // 每步执行前问 Asker 一次
    OnInterrupt func() error           // ctx 取消时的清理回调
}

type UIProvider interface {
    RenderThought(text string)
    RenderResult(r ActionResult)
    Spinner(label string) func()       // 返回 stop 函数
}

// ConsoleUI: 朴素的 stderr 输出（"💭 ..." / "✓ ..." / "✗ ..."）
type ConsoleUI struct {
    out io.Writer
}
func NewConsoleUI(out io.Writer) *ConsoleUI

// 顶层包装器：
func RunInteractionLoop(
    ctx context.Context,
    l *Loop,
    ui UIProvider,
    opts LoopOpts,
) (string, error)
```

`Loop` 本身不变（结构与 s08 一致），但内部从 `Run` 抽出一个 `runStep(ctx, args) (stepResult, error)` 方法供 `RunInteractionLoop` 复用。`Run` 还在，行为与 s08 完全一致——所有 s01–s08 的测试照样通过。

`main.go` 加两个 flag：

```bash
-cycles N         # 0 = 无限（靠 Ctrl-C 退出）
-ask-each-step    # 每步问一下 Asker
```

并把 `loop.Run(ctx, prompt)` 替换成：

```go
SetUserPrompt(prompt)
final, err := RunInteractionLoop(ctx, loop, NewConsoleUI(os.Stderr), LoopOpts{
    Cycles:      *cycles,
    AskEachStep: *askEachStep,
})
```

## How It Works / 工作原理

```ascii-anim frames=4
┌────────────────────────────────────────────────────────────────────────┐
│ STARTUP                                                                  │
│   wctx, cancel := context.WithCancel(ctx)  ──── 包装器局部 ctx          │
│   signal.Notify(sigCh, os.Interrupt)                                    │
│   go func() { <-sigCh; cancel() }                                       │
│        │                                                                 │
│        ▼                                                                 │
│ FOR EACH ITERATION                                                       │
│                                                                         │
│   ┌──── select { case <-wctx.Done(): return handleInterrupt() }        │
│   │      // 早退检查：信号已到就直接退                                 │
│   │                                                                     │
│   ▼                                                                     │
│   if AskEachStep:                                                       │
│       if Asker.Ask("step") != Allow:                                    │
│           ui.RenderResult({"denied", "step skipped"})                   │
│           continue   ◀── 不消耗 cycle，回到 select                      │
│        │                                                                │
│        ▼                                                                │
│   stop := ui.Spinner("Thinking...")                                     │
│   out, err := loop.runStep(wctx, args)                                  │
│   stop()                                                                │
│        │                                                                │
│        ▼                                                                │
│   if err != nil:                                                        │
│       if wctx.Err() != nil: return handleInterrupt()                    │
│       return err                                                        │
│   if out.Done:                                                          │
│       ui.RenderThought(out.FinalAnswer)                                 │
│       return out.FinalAnswer, nil                                       │
│        │                                                                │
│        ▼                                                                │
│   ui.RenderThought(out.Proposal.Thoughts)                               │
│   for r := range out.Results:                                           │
│       ui.RenderResult(r)                                                │
│        │                                                                │
│        ▼                                                                │
│   if !anyInterrupted(out.Results) && Cycles > 0:                        │
│       cyclesLeft--                                                      │
│       if cyclesLeft <= 0: return                                        │
│        │                                                                │
│        └─── turn++ ────► 回到 select                                    │
└────────────────────────────────────────────────────────────────────────┘
```

### 信号处理：channel-based

```go
sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
defer signal.Stop(sigCh)
go func() {
    select {
    case <-sigCh:
        cancel()
    case <-wctx.Done():
    }
}()
```

四行做了 Python `signal.signal` + `nonlocal` 的所有事：

1. SIGINT 到达 → 写入 channel
2. 后台 goroutine 收到 → cancel 包装器局部 ctx
3. 主循环下一次 select 看到 ctx.Done() → 走 `handleInterrupt`
4. `defer signal.Stop` 清理注册，避免下次调用泄漏

**为什么用包装器局部 ctx 而不是直接 cancel 调用方的 ctx？** 因为调用方可能想在 Ctrl-C 之后继续做别的事（比如打印日志、保存 history）。本地 ctx 让我们「就地止血」，不污染上游。

### `runStep` 抽离：复用胜过复制

```go
type stepResult struct {
    Done        bool                    // end_turn → 终止
    FinalAnswer string

    Continue bool                       // tool_use → 继续
    Proposal ActionProposal
    Results  []ActionResult
}

func (l *Loop) runStep(ctx, args runStepArgs) (stepResult, error) {
    messages := l.Strategy.BuildPrompt(...)
    resp, err := l.Provider.CreateMessage(ctx, ...)
    // ... 解析 stop_reason, dispatch tools, gate permissions
    return stepResult{...}, nil
}
```

`Loop.Run` 现在是一层薄包装：

```go
for turn := 0; turn < l.MaxTurns; turn++ {
    out, err := l.runStep(ctx, ...)
    if err != nil { return "", err }
    if out.Done { return out.FinalAnswer, nil }
}
```

`RunInteractionLoop` 直接调 `runStep`，加 cycle 预算、信号、UI——但**不重复**任何 strategy/parse/permission 逻辑。这是关键：把单步从循环里挖出来，使两种循环驱动方式都能复用同一份步骤实现。

### Cycle 预算的承重语义

```go
if !anyInterrupted(out.Results) && opts.Cycles > 0 {
    cyclesLeft--
    if cyclesLeft <= 0 { return "", nil }
}
```

**只有真正执行成功的步骤才扣预算**——和上游 `if result.status != "interrupted_by_human": cycles_remaining -= 1` 完全一致。如果用户在 Asker 那里 Deny 了一步（synthetic 「permission denied (user)」结果），那一步不消耗 cycle。

为什么这样？因为 cycle 预算的语义是「愿意为多少步真正的工作买单」。一个被否决的步骤不代表 agent 已经做了什么；下一次 propose_action 应该重新尝试，而不是已经欠了一份预算。

### `UIProvider`：返回 stop 函数的 Spinner

Python 用 `async with ui_provider.show_spinner(...)` 上下文管理器；Go 没有 `async with`。s09 的设计：

```go
type UIProvider interface {
    Spinner(label string) func()  // 返回 stop 函数
}

// 调用方：
stop := ui.Spinner("Thinking...")
// ... do work
stop()
```

`stop()` 是 idempotent 的（用 `sync.Once`）——错误路径里 `defer stop()` 安全。

`ConsoleUI` 的 Spinner 不动画——只写一行 `[busy] <label>...`，stop fn 写 CR + 空格清行。教学优先：动画需要 ticker + goroutine + cancel，把循环教学拉偏。

### 三个非显然之处

1. **不模拟真 SIGINT 的测试用 ctx-cancel 代替**

   `interaction_loop_test.go` 注释里写：

   ```go
   // 为什么用 ctx-cancel 模拟而不是 syscall.Kill(self, SIGINT)？
   // 在 macOS 上发真 SIGINT 给单元测试有竞态：goroutine 调度器
   // 可能在断言之前还没投递信号；CI runner 可能把它解释为
   // 「构建被中止」。生产代码里 SIGINT → cancel() 只有一行，
   // 用直接 cancel 测「cancel 之后的所有行为」就够了。
   ```

   信号 → cancel 那一跳是一行代码，靠 inspection 就能验证。

2. **`SetUserPrompt` 是包级变量**

   `RunInteractionLoop` 的签名只有 4 个参数（ctx, loop, ui, opts），把 prompt 通过包级 `SetUserPrompt(prompt)` 传进去。为什么？因为加第 5 个位置参数只为了一个字符串太重，加到 LoopOpts 里又混淆「运行时配置」和「输入数据」的边界。注释里写明：未来如果需要并发跑多个循环，把它升级为 LoopOpts 字段或 RunInteractionLoop 参数。

3. **Loop.Run 没动**

   抽出 runStep 的同时，`Run` 仍调 `runStep` 但保持原来的 MaxTurns 循环结构——所有 s01–s08 的测试**逐字节**复制到 s09 仍然通过。这是 s09 的稳定性承诺：「加一层包装器」不能破坏「单次 Run」的接口。

## What Changed / 与 s08 的变化

```diff
 agents/s09-continuous-mode/
 ├── provider.go                       # 与 s08 一字不差
 ├── provider_*.go / _test.go          # 与 s08 一字不差
 ├── tools.go / tools_file.go          # 与 s08 一字不差
 ├── workspace.go / _test.go           # 与 s08 一字不差
 ├── registry.go / _test.go            # 与 s08 一字不差
 ├── history.go / _test.go             # 与 s08 一字不差
 ├── permissions.go / _test.go         # 与 s08 一字不差
 ├── component*.go / _test.go          # 与 s08 一字不差
 ├── strategy.go / _test.go            # 与 s08 一字不差
 ├── loop.go                           # 改:抽 runStep 方法,Run 仍兼容 s08 测试
 ├── loop_test.go                      # 与 s08 一字不差
+├── interaction_loop.go               # 新:RunInteractionLoop + 信号处理 + cycle 预算
+├── interaction_loop_test.go          # 新:6 个测试
+├── ui.go                             # 新:UIProvider + ConsoleUI + NoopUI
+├── ui_test.go                        # 新:4 个测试
 └── main.go                           # 改:加 -cycles, -ask-each-step;调 RunInteractionLoop
```

类型 catalog 新增：

```go
type LoopOpts struct {
    Cycles      int
    AskEachStep bool
    OnInterrupt func() error
}

type UIProvider interface {
    RenderThought(text string)
    RenderResult(r ActionResult)
    Spinner(label string) func()
}

type ConsoleUI struct{ out io.Writer }
type NoopUI struct{ /* 测试用 */ }

type stepResult struct {
    Done        bool
    FinalAnswer string
    Continue    bool
    Proposal    ActionProposal
    Results     []ActionResult
}
```

`Loop` 字段不变。新增方法：`Loop.runStep`、`Loop.ensureDefaults`、`Loop.handleInterrupt`。

## Try It / 动手试一试

```bash
cd agents/s09-continuous-mode

# 1. 默认单 Run（cycles=0 + 立即 end_turn 的提示）
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "say hi"

# 2. 持续模式：跑 5 步然后停
go run . -v -cycles 5 \
    "research the AutoGPT classic README and write a 3-line summary to summary.md"

# 3. 每步审批
go run . -v -cycles 10 -ask-each-step -ask stdin \
    "fetch https://example.com and write the title to notes.md"
# 每步执行前会在 stderr 打印:
#   permission required: step(map[turn:0]) [y/N]:
# 输入 y 让它跑,n 跳过那一步(不消耗 cycle)。

# 4. Ctrl-C 优雅退出
go run . -v -cycles 0 \
    "this loops forever; Ctrl-C me"
# 按一下 Ctrl-C,看到 "interaction loop interrupted" 后退出。

# 5. 跑全部测试
go test -v ./...
```

期望输出（详情模式）：

```
[s09-continuous-mode] provider=anthropic ... cycles=5 ask-each-step=false
[busy] Thinking...
💭 I'll fetch the URL first to get the content.
✓ <!doctype html>...
[busy] Thinking...
💭 Now I'll write the summary to summary.md.
✓ wrote 142 bytes to summary.md
[busy] Thinking...
💭 Done.
Done.
```

注意 ConsoleUI 把 spinner / 思路 / 结果都写到 stderr——stdout 只有最终答案。这是为了 `s09-continuous-mode '...' | jq .` 之类的 pipe 能干净工作。

### Cycle 预算的细微之处

```bash
# Cycles=3,model 第一步就 end_turn → 提前退出,2 个预算没用
go run . -v -cycles 3 "say hi"
# stdout: hi

# Cycles=3,model 跑了 3 步 tool_use 然后第 4 步打算继续
go run . -v -cycles 3 "do 5 things"
# stderr: 看到 3 个 spinner + 3 组思路/结果
# stdout: (空,因为没 end_turn,只是 cycle 用完了)
```

第二种情况返回 `("", nil)`——空 final answer + nil error。包装器认为「cycle 用完不是错误,是预期退出」;调用方应当区分 final 是空字符串(预算用完)还是 ctx 取消(error 非 nil)。

## Upstream Source Reading / 上游源码阅读

完整双语注解版见 [`upstream-readings/s09-interaction-loop.py`](../../upstream-readings/s09-interaction-loop.py)。这里只摘最关键的几行。

```upstream:classic/original_autogpt/autogpt/app/main.py:681-718
cycle_budget = cycles_remaining = _get_cycle_budget(
    app_config.continuous_mode, app_config.continuous_limit
)
spinner = Spinner("Thinking...", ...)
stop_reason = None

def graceful_agent_interrupt(signum, frame):
    nonlocal cycles_remaining, stop_reason
    if stop_reason:
        sys.exit()
    if cycles_remaining in [0, 1]:
        stop_reason = AgentTerminated("Interrupt signal received")
    else:
        cycles_remaining = 1   # ← graceful drain: 让正在跑的步骤跑完

signal.signal(signal.SIGINT, graceful_agent_interrupt)

# [→ s09: Go 版用 signal.Notify(ch, SIGINT) + goroutine + cancel(),
#    避免 nonlocal + 多线程 spinner 状态同步。]
```

```upstream:classic/original_autogpt/autogpt/app/main.py:780-820
result = await agent.execute(action_proposal)

# ★ 承重一行: 只在不是 "interrupted_by_human" 时才扣预算
if result.status != "interrupted_by_human":
    cycles_remaining -= 1

if result.status == "success":
    await ui_provider.display_result(str(result), is_error=False)
elif result.status == "error":
    await ui_provider.display_result(...)

# [→ s09: anyInterrupted(out.Results) 检查,语义完全相同。
#    Go 版的 ui.RenderResult(r) 一行做了上面的 if/elif 分支。]
```

### 对照阅读要点

- **`signal.signal` vs `signal.Notify`**：Python 注册器是进程全局的，第二次调用覆盖第一次（多个库竞争 SIGINT 是 Python 常见 footgun）。Go 用 channel——多个 goroutine 可以共用一个 channel 接收信号，没有「last writer wins」的歧义。

- **两阶段中断 vs 单阶段**：上游设计了「第一次 Ctrl-C 优雅 drain；第二次 Ctrl-C 立即退出」的两阶段策略。s09 简化为单阶段——一次 Ctrl-C 立即 cancel ctx + 触发 `OnInterrupt` 回调。教学优先;两阶段在 Go 里也能做(用 atomic counter 数信号到达次数),但加进来会让 60 行的核心循环变成 90 行。

- **Rich UI vs ANSI 单行**：上游用 Rich 库画 spinner（动画 + 颜色面板）、用 `Panel` 框住 thoughts、用 `Markdown` 渲染 result。s09 的 ConsoleUI 朴素到只用 ASCII 前缀（`💭` / `✓` / `✗`）+ CR 清行。原因：Go 的 TUI 库（charmbracelet/lipgloss、rivo/tview）依赖、构建复杂度都比 Python Rich 高得多——把 200 行 styling 代码塞进 60 行的 ui.go 会让「UIProvider 是个 seam」的 lesson 被 styling 噪声淹没。如果你想做漂亮的 TUI,把 ui.go 的 ConsoleUI 替换成 lipgloss 实现,Loop / RunInteractionLoop 都不用改。

- **Spinner: async with vs stop fn**：Python `async with ui_provider.show_spinner("..."): await ...` 是上下文管理。Go `stop := ui.Spinner("..."); defer stop()` 是同样的语义、同样的清理保证、同样的 idempotent 性（`sync.Once` 让 stop 多次调用安全）。

- **ActionResult.status 词汇**：上游有 `success` / `error` / `interrupted_by_human`。s09 沿用 `ok` / `error` / `denied`,新增 `interrupted_by_human` 但当前实现中没有 ActionResult 会自动设为这个 status——是给 s10/Reflexion 留的扩展点（一个 hook 可以在 AfterParse 阶段把人工拒绝标记成 interrupted_by_human）。

**深入阅读**：[`upstream-readings/s09-interaction-loop.py`](../../upstream-readings/s09-interaction-loop.py) 含 `run_interaction_loop` 全文注解 + Go 翻译表 + 「为什么 ctx 比 nonlocal 更干净」的对比。然后预览 s10：到 s09 末尾，agent 已经能持续运行 + 优雅中断；s10 引入 `Pipeline` (AfterParse / AfterExecute hooks) 和 `ReflexionStrategy`（让 LLM 在执行前再审视一遍自己的提案）——这是 AutoGPT 最具特色的「两阶段思考」机制。

---

**下一节预告**：s10 引入 `AfterParseHook` / `AfterExecuteHook` 与 `Pipeline.RegisterHook`，把交叉关注（验证、Reflexion、metrics）从 strategy 解耦。`ReflexionStrategy` 包一层 OneShot，注册一个 hook 在 propose → execute 之间发第二次 LLM 调用做自我评估。对标上游 `prompt_strategies/reflexion.py` + `agent/protocols.py::AfterParse`。
