---
title: "附录 A · Classic vs 现代 Agent 架构"
chapter: appendix-a
slug: appendix-a-classic-vs-modern
est_read_min: 14
---

# 附录 A · Classic vs 现代 Agent 架构

> 这是一篇心智模型文，不是代码教学。看完 s01–s_full 后回头读，你会理解 AutoGPT classic 在 2023 年是什么、为什么 Significant-Gravitas 转向了 Platform、现代框架（LangGraph / OpenAI Agents SDK / Anthropic Agents SDK）是怎么把同一组问题答得不一样的。

---

## 1. 2023 年的「自治 agent 时刻」

2023 年 4 月，AutoGPT 在 GitHub 上几天涨了 100k+ stars。它不是因为底层算法有多新——`ReAct`（reasoning + acting）论文 2022 年 10 月就发了——而是因为它把**一个完整的 think→act→observe 循环 + 自定义命令系统**包成了人人能跑的 Python 程序。

为什么这一刻才发生？三件事撞到一起：

1. **GPT-4** 的发布让 LLM 第一次具备"在没有微调的前提下做多步规划"的可靠性；
2. **OpenAI function-calling** 让结构化 tool 调用从 prompt-engineering hack 变成 first-class API；
3. **OSS Python**：AutoGPT 的早期版本不到 2k 行，本科生周末就能改。

但 2023 年的乐观——「让一个 GPT-4 自己 24 小时跑下去解决任何问题」——很快撞墙：

- 多步规划 hallucinate；中间状态 lost；
- 上下文窗口随 history 线性增长，几轮就溢出；
- shell 命令执行没有沙箱时极度危险；
- 长链路任务的成功率低于 50%（AGBenchmark 数据）。

这些是**所有后来 agent 框架都在解决的问题**。AutoGPT classic 是这条问题清单的第一份草稿，因此每个机制都有深刻的"为什么需要它"——这正是它适合做教学样本的原因。

## 2. AutoGPT classic 的标志性押注

把 classic 当作 2023 年的"agent 设计大全"，它有几个独特押注：

**a. 八种 prompt 策略并存**：`one_shot / rewoo / plan_execute / reflexion / tree_of_thoughts / lats / multi_agent_debate / base`。每种是一个 `PromptStrategy` 子类——同一份 agent 代码可以热切策略。

押注的是「**没有最好的策略，不同任务需要不同推理形态**」。事后看，这个押注没赢——2024 年起几乎所有生产框架收敛到「单一基线 + 工具/数据增强」，因为 8 种策略的维护成本远高于 1 种 + 充分的 tool 抽象。

**b. 插件式 abilities**：`@command` 装饰器让任何 Python 函数可以变成 agent tool；`plugins/` 目录支持运行时加载第三方包。

押注的是「**生态广度 > 内核强度**」。事后看，这个押注是对的，但形态变了——MCP 协议（2024 年起）把"插件式 abilities"标准化跨进程，比 AutoGPT 的 plugin loader 更通用。

**c. EpisodicActionHistory + 自动总结**：把每轮的 action+result 包成 Episode，提供 `prepare_messages()` 钩子在上下文溢出时调用 LLM 把旧 episode 总结成短句。

押注的是「**memory 是 agent 的一等公民**」。事后看，这个押注是对的，但实现走向了 vector memory + retrieval（chromadb / pinecone）路径，而非"每次都让 LLM 总结"。

**d. 8 路 LLM provider via litellm**：MultiProvider 在一个 dict 里维护 OpenAI、Anthropic、Groq、Llamafile 等模型。

押注的是「**multi-LLM 是常态**」。事后看，这个押注完全对——所以学习仓库 s01 一开始就有 8 个 profile（Phase G 提供）。

## 3. 为什么 Significant-Gravitas 转向 Platform

2024 年，团队把核心精力转向 `autogpt_platform/`（用 Polyform Shield 许可）。Platform 是个 graph executor + 可视化 builder：用户在 web UI 上拖拽节点，每个节点是一个 LLM / tool / 决策；运行时是一个并发 graph engine 而不是单进程 loop。

转向的原因不是 classic 不行，而是 classic 触到了**架构上的天花板**：

- **可视化是用户增长的杠杆**。CLI agent 的 TAM 远小于「拖拽节点的 no-code workflow」。
- **企业部署需要 multi-tenant + audit + scheduling**。这三件 classic 都没做（pyt 的 single-process loop 模型自然不支持）。
- **graph topology** 让并行子 agent / 条件分支 / 循环回路第一类化；prompt-strategy 形态做不到。
- **license 调整**：Polyform Shield 限制商业再分发，给团队留了商业化空间——MIT classic 不允许这种限制。

学习仓库**不动 platform**——一是 license 问题，二是 graph executor 的教学价值不在 agent 设计上而在 workflow engine 上，跨界太多。

## 4. 现代替代的设计

四个有代表性的现代 agent 框架：

**LangGraph**（LangChain 衍生，开源 MIT）

- 模型：**显式 state machine**。开发者定义节点、边、状态字段；框架执行图。
- 优点：流程可视化（mermaid 图）；状态持久化天然；并行/条件路径一类化。
- 缺点：定义图的 boilerplate 大；动态行为（LLM 自己决定下一步）需要把 node 定义成 router。
- 与 classic 的差别：classic 把决策权全交给 LLM 在每一步「选一个 tool」；LangGraph 把决策权分给图结构 + LLM——LLM 只在 router node 选下一节点。

**OpenAI Agents SDK**（2024-12 发布，MIT）

- 模型：**handoff + tool**。一个 Agent 是 instructions + tools + handoff_to_agents；执行时 LLM 在 tool 调用和 handoff 之间选择。
- 优点：极简；与 OpenAI Responses API 深度集成；guardrails / tracing 内置。
- 缺点：handoff 是隐式 graph——出错时调试不如显式 graph 直观。
- 与 classic 的差别：classic 的 tool list 全静态；Agents SDK 的 handoff 是「换 agent」级别的工具，自然支持多 agent 协同。

**Anthropic Agents SDK / Claude Agent SDK**（2024-12+，MIT/Apache）

- 模型：**claude-code 同款**。Agent 是一个会话 + tool use 循环 + 可选的 sub-agent；与 MCP server 深度互通。
- 优点：tool 协议（content-block tagged union）和 prompt cache 一等公民；sub-agent 隔离上下文窗口非常自然。
- 缺点：极偏 Anthropic 模型；多模型支持不如 OpenAI Agents SDK 干净。
- 与 classic 的差别：classic 用 OpenAI function-calling shape，AnthropicProvider 是后加的；Agents SDK 反过来——content-block 是 native，OpenAI 兼容是适配层。**learn-AutoGPT 的类型 catalog 借的是这套**。

**LlamaIndex Agents / Hypothesis Workflow**

- 模型：**事件驱动 + DAG**。事件触发 step；step 之间通过 typed event 传递数据。
- 优点：长任务（小时～天）天然可恢复；step 解耦；可视化好。
- 缺点：学习曲线高；over-engineered for short-task agents.
- 与 classic 的差别：classic 是同步 loop；Workflow 是异步事件总线。

## 5. 什么时候你仍然会选 classic-style

不是每个场景都需要 graph engine。Classic 风格（单进程 loop + tool registry + prompt strategy）在以下情况依然合适：

- **教学**：classic 的设计选择有清晰的"为什么这么做"；现代框架往往是问题驱动 + 工程驱动的混合，pedagogically 更难拆解。**这就是 learn-AutoGPT 的存在理由**。
- **快速 prototype**：写一个能解决 X 问题的小 agent，不需要 graph 的开销。
- **CLI / 本地 dev tool**：Claude Code / aider / cursor 内核都是 classic-style loop（虽然各自做了大量优化）。
- **学习其他领域时的"agent 模式"借鉴**：你在 RL / planning / control 系统设计里看到 think-act-observe 时，会发现这是 classic agent 留下的 universal idiom。

---

如果你看完这一节有「我也想从头实现一个现代框架」的冲动，附录 B 的练习 #3、#4 给了从 learn-AutoGPT 出发改写成 Agent Protocol REST 服务 + plugin loader 的两条具体路径。
