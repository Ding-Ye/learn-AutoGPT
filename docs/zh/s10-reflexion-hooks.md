---
title: "s10 · Reflexion 与 AfterParse hooks"
chapter: 10
slug: s10-reflexion-hooks
est_read_min: 16
---

# s10 · Reflexion 与 AfterParse hooks

> 教什么：把"Strategy 之外的横切关注点"从 Loop 里挑出来——`Pipeline` 的 `AfterParse` / `AfterExecute` 钩子。Reflexion 是这套钩子的第一个用户：第二轮 LLM 评估，判定提案是否合理，必要时改写后再交给工具调度。这是整个课程最具架构意义的一节。

---

## Problem / 问题

s09 让 agent 能在多轮里自治运行，但 Loop 内部仍然写死了"strategy.Parse → permissions.Check → tool.Execute → history.Append"这条直线。如果想插入"在执行前再让 LLM 自检一遍提案"——典型的 Reflexion / Self-Refine 模式——只剩两种丑陋的选择：

1. 把第二轮 LLM 调用塞进 `OneShotStrategy.ParseResponse`，让一个 Strategy 实现承担两件事；
2. 改写一份 `ReflexionLoop` 把 s09 整段抄过来再加一段，跨节复制成本翻倍。

AutoGPT 上游早就走过这条路。它的答案在 `forge/agent/protocols.py`：定义抽象类 `AfterParse`、`AfterExecute`，让任何 component 实现 `after_parse(result)` / `after_execute(result)` 方法；`agent.py:282` 用 `await self.run_pipeline(AfterParse.after_parse, result)` 把所有实现这接口的组件依次跑一遍。Reflexion 因此不是一种"特殊 Loop"，而是一个 `AfterParse` 钩子。Go 版要重新发明这条缝。

## Solution / 解决方案

引入 `Pipeline` 类型——一个**有序的回调登记处**，提供两条挂载点：`AfterParseHook` 在 `strategy.Parse` 之后、permission gate 之前触发；`AfterExecuteHook` 在 `tool.Execute` 之后、`history.Append` 之前触发。两类钩子都接收**指向 proposal/result 的指针**，所以钩子可以**就地改写**输入。第一个返回非 nil error 的钩子停止管线并把错误冒泡到 Loop。

`ReflexionStrategy` 是这套机制的第一个客户。它的 `BuildPrompt` / `ParseResponse` 直接委托给底层 base strategy（默认 OneShot）——Reflexion 不改 prompt 构造。它在**构造时**把一个 `AfterParseHook` 注册到 Pipeline 上：每次该钩子触发，就发一条独立的 `Provider.CreateMessage` 让模型回答 `{"sound": bool, "revised"?: ActionProposal}`，必要时**就地把 Command/Args 换成修订版**。Loop 完全不知道 Reflexion 存在——它只是按 Pipeline 的契约把钩子跑一遍——这就是架构上的关键收益。

三个关键决策：

1. **钩子接收指针，不接受新值返回**。钩子的合同是"**就地修改**或返回 error"，不是"返回新对象"。这样多个钩子可以串联——validation 钩子先检查，metrics 钩子记录长度，Reflexion 钩子改写命令——全在同一个 ActionProposal 实例上。
2. **Pipeline 是 Loop 的可选字段**。`Loop.Pipeline == nil` 时整段是 no-op，s01-s09 的所有测试在 s10 module 里依然通过——零侵入升级。
3. **Reflexion 是 Strategy 又是钩子注册者**。这种双重身份是教学的目标：演示"一个 Strategy 变体可以复用横切的钩子机制，而不必把所有逻辑塞进 Strategy 自己"。这是**组合优于继承**的 Go 化表达。

## How It Works / 工作原理

```
                   ┌────────────────────────────────────────────────┐
                   │ Loop.runStep(ctx)                              │
                   │                                                │
   ┌───────────────│  msgs = strategy.BuildPrompt(history,...)      │
   │               │  resp = provider.CreateMessage(msgs)           │
   │               │  prop = strategy.ParseResponse(resp.Content)   │
   │               │                                                │
   │               │  pipeline.RunAfterParse(&prop)  ←── 钩子点 #1  │
   │               │       │                                        │
   │               │       ├── ReflexionHook: 第二轮 LLM, 改写 prop │
   │               │       ├── ValidationHook: 拒绝畸形 args        │
   │               │       └── AuditHook: 写日志                    │
   │               │                                                │
   │               │  permissions.Check(prop) → Allow/Deny/Ask      │
   │               │  result = registry.Lookup(prop.Command).Execute│
   │               │                                                │
   │               │  pipeline.RunAfterExecute(&result) ─ 钩子点 #2 │
   │               │       │                                        │
   │               │       ├── TruncateHook: 截断超长 web_fetch     │
   │               │       ├── RedactHook: 抹去 API keys / PII      │
   │               │       └── MetricsHook: 计数 + 延迟              │
   │               │                                                │
   │               │  history.Append(Episode{prop, result})         │
   └───────────────└────────────────────────────────────────────────┘
```

核心 30-60 行（节选自 [`agents/s10-reflexion-hooks/pipeline.go`](https://github.com/Ding-Ye/learn-AutoGPT/blob/main/agents/s10-reflexion-hooks/pipeline.go)）：

```go
type AfterParseHook func(ctx context.Context, proposal *ActionProposal) error
type AfterExecuteHook func(ctx context.Context, result *ActionResult) error

type Pipeline struct {
    afterParse   []AfterParseHook
    afterExecute []AfterExecuteHook
}

func (p *Pipeline) RegisterAfterParse(h AfterParseHook) {
    p.afterParse = append(p.afterParse, h)
}

func (p *Pipeline) RunAfterParse(ctx context.Context, prop *ActionProposal) error {
    if p == nil {
        return nil  // nil pipeline = pure no-op, by design
    }
    for i, h := range p.afterParse {
        if err := h(ctx, prop); err != nil {
            return fmt.Errorf("AfterParse hook %d: %w", i, err)
        }
    }
    return nil
}
```

ReflexionStrategy 注册钩子的部分（节选自 [`strategy_reflexion.go`](https://github.com/Ding-Ye/learn-AutoGPT/blob/main/agents/s10-reflexion-hooks/strategy_reflexion.go)）：

```go
func NewReflexionStrategy(base PromptStrategy, provider Provider, pipeline *Pipeline) *ReflexionStrategy {
    r := &ReflexionStrategy{base: base, provider: provider, pipeline: pipeline}
    if pipeline != nil {
        pipeline.RegisterAfterParse(r.afterParseHook)  // 自我注入
    }
    return r
}

func (r *ReflexionStrategy) afterParseHook(ctx context.Context, prop *ActionProposal) error {
    if prop == nil || prop.Command == "" { return nil }
    question := r.buildReflexionPrompt(prop)
    resp, err := r.provider.CreateMessage(ctx, CreateMessageRequest{
        Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: question}}}},
    })
    if err != nil { return fmt.Errorf("reflexion second-pass: %w", err) }
    verdict, parseErr := parseReflexionVerdict(resp.Content)
    if parseErr != nil { return nil }  // 二次解析失败不阻塞主线
    if !verdict.Sound && verdict.Revised != nil {
        prop.Command = verdict.Revised.Command       // 就地改写
        if verdict.Revised.Args != nil { prop.Args = verdict.Revised.Args }
        if verdict.Revised.Thoughts != "" { prop.Thoughts = verdict.Revised.Thoughts }
    }
    return nil
}
```

**4 个非显然之处**：

1. **空 Pipeline 是合法 no-op** —— `(*Pipeline)(nil).RunAfterParse(...)` 显式返回 nil。这让 Loop 端不必判空，s09 测试零修改通过。
2. **二次解析失败不阻塞主线** —— `parseReflexionVerdict` 返回 error 时，钩子忽略 error 让原 proposal 通过。理由：评估器 LLM 偶尔吐错的 JSON 不应该让 agent 整轮挂掉；测试用例 `TestReflexionStrategy_GarbledVerdictPassesThrough` 锁定这一行为。
3. **Reflexion 既是 Strategy 又注册钩子** —— 双重身份是设计目标。用户传 `-strategy=reflexion` 就同时获得 strategy 替换和钩子注入；构造一行搞定。
4. **指针接收方式让多钩子可串联** —— validation hook 改 args、reflexion hook 改 command、audit hook 写日志，三者按注册顺序在同一个 *ActionProposal 上叠加生效。这是 Go 在 Python 抽象基类之外的等价表达。

## What Changed / 与 s09 的变化

```diff
 // loop.go
 type Loop struct {
     Provider    Provider
     Components  *ComponentBus
     Strategy    PromptStrategy
     History     *History
     Permissions *Permissions
     Asker       Asker
+    Pipeline    *Pipeline   // s10: optional cross-cutting hooks
     MaxTurns    int
     Verbose     bool
 }

 func (l *Loop) runStep(ctx context.Context, ...) (...) {
     // ... strategy.BuildPrompt + provider + strategy.ParseResponse ...
     proposal, err := strategy.ParseResponse(resp.Content)
     if err != nil { return ..., err }

+    if err := l.Pipeline.RunAfterParse(ctx, &proposal); err != nil {
+        return ..., err
+    }

     decision := l.Permissions.Check(proposal.Command, proposal.Args)
     // ... permission handling + tool.Execute ...

+    if err := l.Pipeline.RunAfterExecute(ctx, &result); err != nil {
+        return ..., err
+    }

     l.History.Current().Results = append(l.History.Current().Results, result)
 }
```

新增文件：`pipeline.go`、`pipeline_test.go`、`strategy_reflexion.go`、`strategy_reflexion_test.go`。`main.go` 多了 `-strategy={oneshot|reflexion}` 标志。其他基础设施文件（provider/tools/registry/strategy/history/workspace/permissions/component/ui/interaction_loop）从 s09 字节级拷贝。

语义上的变化：从这一节开始，**任何想观察或修改 propose↔execute 边界的逻辑**（日志、指标、redact、govern、reflexion）都不再需要碰 Loop——只要写一个 `AfterParseHook` / `AfterExecuteHook` 注册上去。这就是 Pipeline 的全部价值。

## Try It / 动手试一试

```bash
export PATH="$HOME/sdk/go-1.26.3/bin:$PATH"
cd agents/s10-reflexion-hooks

# 1) 默认 oneshot 策略（Pipeline 为空，行为与 s09 相同）
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "echo hello"

# 2) 切换到 reflexion 策略——每个提案都被第二轮 LLM 评估
go run . -v -strategy=reflexion "Use the math tool to compute 7 * 13"

# 3) 跑测试
go test -v ./...
```

期望输出形态：

```
[s10-reflexion-hooks] provider=anthropic strategy=reflexion ...
💭 I'll use the math tool to compute 7 * 13.
[reflexion] second-pass: sound=true
✓ 91
```

如果 reflexion 判定提案不当，会看到提案被改写：

```
💭 I'll execute `bash` with `rm -rf /tmp`.
[reflexion] second-pass: sound=false → revised to {command: echo, ...}
✓ (revised proposal output)
```

测试覆盖 4 个钩子契约 + 5 个 reflexion 行为：注册顺序、可变性、错误传播、nil-pipeline、空 Command 跳过、JSON 解析容错、sound=true 透传、sound=false+revised 改写、provider 失败传播。

## Upstream Source Reading / 上游源码阅读

AutoGPT 上游的 pipeline 抽象藏在两个文件里：`forge/agent/protocols.py`（钩子的 ABC 类型）和 `original_autogpt/autogpt/agents/agent.py:282`（实际的 `run_pipeline` 调度）。Reflexion 的实现在 `original_autogpt/autogpt/agents/prompt_strategies/reflexion.py`，是个 600+ 行的多阶段 strategy；我们只蒸馏其中"第二轮评估即时改写"的核心，把"反思入 episodic memory 跨轮使用"这条线留给附录练习。

```upstream:classic/forge/forge/agent/protocols.py#L1-L46
from abc import abstractmethod
from typing import TYPE_CHECKING, Awaitable, Generic, Iterator

from forge.models.action import ActionResult, AnyProposal
from .components import AgentComponent

if TYPE_CHECKING:
    from forge.command.command import Command
    from forge.llm.providers import ChatMessage


class DirectiveProvider(AgentComponent):
    def get_constraints(self) -> Iterator[str]: return iter([])
    def get_resources(self) -> Iterator[str]: return iter([])
    def get_best_practices(self) -> Iterator[str]: return iter([])

class CommandProvider(AgentComponent):
    @abstractmethod
    def get_commands(self) -> Iterator["Command"]: ...

class MessageProvider(AgentComponent):
    @abstractmethod
    def get_messages(self) -> Iterator["ChatMessage"]: ...

# === 这两个抽象基类是本节的核心 ===
class AfterParse(AgentComponent, Generic[AnyProposal]):
    @abstractmethod
    def after_parse(self, result: AnyProposal) -> None | Awaitable[None]: ...

class ExecutionFailure(AgentComponent):
    @abstractmethod
    def execution_failure(self, error: Exception) -> None | Awaitable[None]: ...

class AfterExecute(AgentComponent):
    @abstractmethod
    def after_execute(self, result: "ActionResult") -> None | Awaitable[None]: ...
```

```upstream:classic/original_autogpt/autogpt/agents/prompt_strategies/reflexion.py#L1-L60
"""Reflexion Prompt Strategy.

This strategy implements the Reflexion pattern from research including:
- Reflexion: Verbal Reinforcement Learning (arxiv.org/abs/2303.11366)
- Self-Refine: Iterative Self-Feedback (arxiv.org/abs/2303.17651)
- Self-Reflection in LLM Agents (arxiv.org/abs/2405.06682)

Key benefits:
- 91% pass@1 on HumanEval (vs GPT-4's 80%)
- No training required - same LLM generates, critiques, refines
- Agents store reflections in episodic memory for better future decisions
- Supports 8 types of self-reflection that improve problem-solving

Pattern:
1. GENERATE: Propose action
2. EXECUTE: Run action
3. REFLECT: Critique result, extract lessons
4. RETRY: Use reflection to improve next attempt
"""

class ReflexionPhase(str, Enum):
    PROPOSING = "proposing"
    REFLECTING = "reflecting"

class EvaluationResult(ModelWithSummary):
    success: bool
    score: Optional[float]   # 0..1
    feedback: str
```

**对照阅读要点**：

- **Python 用抽象基类，Go 用函数类型**：upstream 的 `AfterParse` 是个 ABC，Component 通过继承+实现 `after_parse` 来注册；`run_pipeline` 用 `getattr` 反射查找。Go 没有反射注册的便利，所以我们直接定义 `type AfterParseHook func(...)` 把回调升格为类型。少一层抽象，更显式。
- **upstream 把 reflexion 编织进 propose_action**，Reflection / EvaluationResult / ReflexionMemory / ReflexionPhase 一整套类拉成一条 600+ 行的多阶段 strategy（含跨轮反思入 memory）。我们只做"第二轮即时评估+就地改写"这条最薄的实现；扩展为"反思入 history 跨轮使用"放到附录 B 练习题 #1。
- **upstream 同时存在 ExecutionFailure 钩子**（异常路径）。我们没做——Go 的 ActionResult 用 `Status: "error"` 字符串而非异常，所以 AfterExecuteHook 已经能处理失败路径；额外开一类 ExecutionFailure 是 Python 异常模型的产物。
- **`async/await` vs Go ctx**：upstream 钩子是 `None | Awaitable[None]`（同步或异步都行）；Go 直接拿 `context.Context` 走，统一同步签名+ctx 取消，更可预测。
- **JSON 解析容错的位置**：upstream `extract_dict_from_json` 是个全局工具，所有 strategy 共用；我们把宽松解析放在 `parseReflexionVerdict` 内部，因为 reflexion 是唯一一个调用方——延迟提取直到第二个用户出现，符合 KISS。

**想读更多**：从 `forge/agent/protocols.py` 入手看 ABC 定义，跟 `agent.py:282` 的 `run_pipeline` 调用进入 `forge/agent/components.py` 看 `getattr` 反射注册的细节，最后在 `prompt_strategies/reflexion.py` 看 ReflexionMemory 和 EvaluationResult 怎么把"反思"持久化到 episodic memory。这三步是 s10 → s_full → 附录 B 练习题 #1 的真实代码地图。

---

**下一节预告**：s_full 不写新代码，把 s01–s10 这十节的零件按 16 步执行轨迹串成一条端到端流，给出"上游一个真实任务在我们这个 mini 上是怎么跑完整的"的完整对照。
