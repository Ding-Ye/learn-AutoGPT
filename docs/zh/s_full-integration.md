---
title: "s_full · 端到端集成"
chapter: full
slug: s_full-integration
est_read_min: 18
---

# s_full · 端到端集成

> 这一节不写新代码。它把 s01–s10 这十节的零件按一条 16 步的上游典型 use case 串起来，让你能从「读完一节我懂了一个机制」上升到「整个 agent 怎么从一个用户提问跑成一份成果」。

---

## Problem / 问题

学完 s01–s10，你对每个机制都有独立的心智模型——但十个机制如何在一次执行里彼此协作仍是模糊的。具体说：

- 多步 tool 调用、permission gate、reflexion 评估、history 追加、UI 输出，**这些事件的精确时序**？
- 每条 LLM 请求里的 messages 列表是从哪些组件拼起来的？
- 当 agent 执行到一半被 SIGINT 打断，状态是怎么回滚 / 持久化的？
- 上游 AutoGPT 跑一个真实任务（比如「研究 LLM evaluation 论文，写成 markdown」）是怎么 16 步走完的？我们的 mini 在哪里跟它对应得上、哪里裁剪了？

s_full 的任务是把这些回答**全部塞进一张图 + 一份 16 步追踪表**里。

## Solution / 解决方案

把整套 agent 看作 4 层组合：

1. **入口层**：`main.go` + `RunInteractionLoop` (s09)
2. **决策层**：`Loop.runStep` orchestrating Provider + Strategy(可能是 Reflexion 包装) + Pipeline (s10)
3. **能力层**：`ComponentBus` → `Registry` → `Tools` (s08, s06, s02)
4. **状态层**：`History`, `Workspace`, `Permissions`, `Asker` (s05, s06, s07)

每一步执行都是这 4 层的一次贯穿——而每一节学过的机制对应**一层中的一个抽象点**。Reflexion 是 s10 注册到 Pipeline 上的 AfterParseHook；它在决策层中部触发；它通过 history 入档成为状态层的一部分；它的 prompt 复用 strategy 拼出来的 directives——所有这些关系在 16 步追踪里都能看到。

## How It Works / 工作原理

整个系统在运行时的组合关系：

```
                          ┌─ User CLI flags / env / -strategy / -cycles
                          │
                          v
   ┌──────────────────────────────────────────────────────────────────┐
   │ main.go                                                          │
   │   • providerProfiles[8] → Provider concrete (Anthropic|OpenAI…)  │
   │   • LocalWorkspace("./workspace/")                               │
   │   • Components = [FileMgr(ws), WebFetch()]                       │
   │   • Permissions ← permissions.json | defaults                    │
   │   • Pipeline = NewPipeline()                                     │
   │   • Strategy = OneShot OR Reflexion(OneShot, provider, pipeline) │
   │   • History = &History{}                                         │
   │   • Loop{Provider, Components, Strategy, History,                │
   │           Permissions, Asker(stdin), Pipeline,                   │
   │           MaxTurns, Verbose}                                     │
   │   • RunInteractionLoop(ctx, loop, ConsoleUI, LoopOpts)           │
   └────────────────────────────┬─────────────────────────────────────┘
                                │
                                v  per-step (cycle counter ↓ on success)
   ┌──────────────────────────────────────────────────────────────────┐
   │ Loop.runStep(ctx)                                                │
   │   1. registry  = Components.Registry()                           │
   │   2. directives = Components.Directives()                        │
   │   3. msgs = Strategy.BuildPrompt(History, registry.All(),        │
   │                                  directives, userTask)           │
   │   4. resp = Provider.CreateMessage(msgs)                         │
   │   5. proposal, err = Strategy.ParseResponse(resp.Content)        │
   │                                                                  │
   │   6. Pipeline.RunAfterParse(&proposal)        ◀─ Reflexion fires │
   │                                                                  │
   │   7. decision = Permissions.Check(proposal.Command, .Args)       │
   │       ├── Allow → continue                                       │
   │       ├── Deny  → result = {Status:"error", Output:"denied"}     │
   │       └── Ask   → Asker.Ask(...) → Allow|Deny                    │
   │                                                                  │
   │   8. tool, ok = registry.Lookup(proposal.Command)                │
   │   9. output, err = tool.Execute(ctx, proposal.Args)              │
   │  10. result = ActionResult{Status, Output}                       │
   │                                                                  │
   │  11. Pipeline.RunAfterExecute(&result)                           │
   │                                                                  │
   │  12. History.Append(Episode{Actions:[proposal], Results:[result]}│
   │  13. UI.RenderThought + UI.RenderResult                          │
   └──────────────────────────────────────────────────────────────────┘
```

## What Changed / 与 s10 的变化

s_full 不引入新代码。它的输出物是**追踪表 + 故意省略表**两张文档：

```diff
+ docs/zh/s_full-integration.md   ← 你正在读
+ docs/en/s_full-integration.md
```

如果你回头看 README 的课程地图，现在每一行都是 ✅。

## Try It / 动手试一试

跟着这条 16 步追踪走一遍——上游典型 use case「研究 LLM evaluation 最新论文，把摘要写成 markdown 文件」：

| 步 | 谁 | 做什么 | 涉及的节 / 文件 |
|---|---|---|---|
| 1 | 用户 | `go run . -strategy=reflexion -cycles 10 -v "研究 LLM evaluation 最新论文，写成 ./summary.md"` | main.go (s01-s10 inherits) |
| 2 | main | 解析 -provider 选 Anthropic, 读 ANTHROPIC_API_KEY | main.go (s01) |
| 3 | main | 构造 LocalWorkspace("./workspace/"), 自动 mkdir | s06 workspace.go |
| 4 | main | 构造 [FileMgr(ws), WebFetch(30s)] → ComponentBus | s08 component.go + component_filemgr.go + component_web.go |
| 5 | main | 加载 ./permissions.json (allow web_fetch / read / write) | s07 permissions.go |
| 6 | main | 构造 Pipeline + ReflexionStrategy(OneShot, provider, pipeline) | s10 pipeline.go + strategy_reflexion.go |
| 7 | RunInteractionLoop | for cycles_remaining { ... }; UI.Spinner("Thinking...") | s09 interaction_loop.go |
| 8 | Strategy.BuildPrompt | render system message: tools schemas (web_fetch, read_file, write_file) + directives ("read before write") + userTask | s04 strategy.go + s08 component.go (Directives) |
| 9 | Provider.CreateMessage | POST 到 Anthropic, Content-Type / x-api-key 头, 等待响应 | s01 provider.go |
| 10 | Strategy.ParseResponse | 提取 tool_use ContentBlock → ActionProposal{Command:"web_fetch", Args:{"url":"https://arxiv.org/list/cs.CL/recent"}} | s04 strategy.go |
| 11 | Pipeline.RunAfterParse | Reflexion 钩子触发：第二轮 LLM 评估 → "sound": true → 不改写 | s10 strategy_reflexion.go |
| 12 | Permissions.Check | "web_fetch: *" 匹配 allow → Allow | s07 permissions.go |
| 13 | Registry.Lookup("web_fetch").Execute | http.Get(url, 30s timeout); 截断到 8KB | s08 component_web.go |
| 14 | Pipeline.RunAfterExecute | (本例无附加 hook 注册) → 直接通过 | s10 pipeline.go |
| 15 | History.Append | Episode{Actions:[proposal], Results:[result]} 入档 | s05 history.go |
| 16 | UI | RenderThought + RenderResult 输出到 stderr; cycle counter -- | s09 ui.go |

下一轮（cycle 2 起）流程相同，但 Strategy.BuildPrompt 接收**已填充**的 History——s05 的 RenderMessages 把上一回合的 web_fetch 内容渲染成 [user, assistant, user] 结构传给 LLM；模型据此决定下一步。直到模型 emit `end_turn`（不再有 tool_use），RunInteractionLoop 退出，最终输出来自 ConsoleUI。

## Upstream Source Reading / 上游源码阅读

### 我们故意省略的上游特性 (Deliberate omissions)

| 上游有 | 我们没有 | 原因 |
|---|---|---|
| 8 个 Prompt 策略 (one_shot / rewoo / reflexion / plan_execute / tree_of_thoughts / lats / multi_agent_debate / base) | 只 ship one_shot；reflexion 作为 Strategy 包装 | 教学上「8 个并存策略」过载；s10 演示「策略可包装」就够了 |
| Vector memory backends (chromadb / pinecone / weaviate / redis) | 无 | 学习路径优先；vector memory 是 history 压缩的实现细节，留作 Appendix B 练习 #2 |
| Plugin loader (`original_autogpt/plugins/`) | 无 | Component 系统 (s08) 已演示 plugin 思路；动态加载留作 Appendix B 练习 #4 |
| Telemetry / 分析 opt-in | 无 | 教学仓库不需要遥测 |
| Agent Protocol REST 服务器 (`serve` 模式 + FastAPI 路由) | 无 | 留作 Appendix B 练习 #3 |
| `_execute_tools_parallel` 并行 tool 调度 | 顺序执行 | 单轮一个 tool_use 已能演示协议；并行是性能优化 |
| AppConfig + AgentSettings 持久化 (.autogpt/agents/{id}/state.json) | 单进程 in-memory | 跨进程 resume 是工程问题，不是教学要点 |
| AIProfile (name/role/goals 持久化) | 单 prompt 字符串 | 持久化 persona 不影响 loop 形态 |
| 4-level permission scopes (ONCE/AGENT/WORKSPACE/DENY) | 2-level (Allow/Deny/Ask) | 简化阅读；4-level 留作 Appendix B 练习 #5 |
| ReflexionMemory 跨轮反思持久化 | 单轮即时评估 | 我们演示了 hook 机制；跨轮反思是 s10 + s05 的进阶组合，留作 Appendix B 练习 #1 |

**对照阅读要点**：

- **整图心智**：上游 `agent.py` 是 542 行，把所有这些零件揉在一个 Agent 类里。我们用 10 个独立 module 拆开，每个 module ≈ 300 行。读上游时把 `agent.py` 当作"你已经读过的所有 sNN 的合集"。
- **执行入口**：上游 `app/main.py:run_interaction_loop` 对应我们 s09 的 `RunInteractionLoop`。差别是上游用 Rich 库做带帧动画的 UI；我们用最朴素的 ANSI 行输出。
- **Reflexion 在上游是独立 strategy**，在我们这里是 strategy + 钩子双重身份——Go 的 composition 表达更显式。
- **协议 shape**：上游用 OpenAI function-calling shape on the wire，我们的内部类型用 Anthropic content-block shape，s03 的 OpenAIProvider 在 provider 边界做 wire 翻译——这是**为什么我们的 Loop 代码比上游短 30%** 的关键原因。

**想读更多**：从 README 课程地图任意一个 ✅ 链接进文档，每节末尾都有具体的「想读更多」指针；附录 B 给出完整的上游源码阅读顺序。
