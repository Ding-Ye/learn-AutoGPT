---
title: "附录 B · 上游源码导读地图"
chapter: appendix-b
slug: appendix-b-upstream-map
est_read_min: 12
---

# 附录 B · 上游源码导读地图

> 这是一本「读上游 AutoGPT classic 源码的指南」。你已经在 Go 里实现了一遍，现在把这套理解搬到 Python 上游——再做 5 个练习把 mini 推向 production-ready。

---

## 1. 上游 classic/ 子目录的全景

```
classic/
├── original_autogpt/                    [核心 agent 应用]
│   ├── autogpt/
│   │   ├── app/
│   │   │   ├── main.py            ← run_interaction_loop, CLI entry
│   │   │   ├── cli.py             ← Click 命令路由
│   │   │   ├── config.py          ← AppConfig (smart_llm/fast_llm/...)
│   │   │   └── ui/                ← Rich 终端渲染
│   │   ├── agents/
│   │   │   ├── agent.py           ← Agent 类 + propose_action + execute
│   │   │   └── prompt_strategies/
│   │   │       ├── base.py        ← PromptStrategy ABC
│   │   │       ├── one_shot.py    ← 默认 strategy
│   │   │       ├── reflexion.py   ← 反思 strategy（600+ 行）
│   │   │       ├── rewoo.py
│   │   │       ├── plan_execute.py
│   │   │       ├── tree_of_thoughts.py
│   │   │       ├── lats.py
│   │   │       └── multi_agent_debate.py
│   │   └── plugins/               ← 第三方插件加载器
│   ├── tests/                     ← integration/ + unit/
│   └── .env.template              ← 配置模板
│
├── forge/                               [agent 框架，被 original_autogpt 复用]
│   ├── forge/
│   │   ├── agent/
│   │   │   ├── base.py            ← BaseAgent
│   │   │   ├── components.py      ← AgentComponent + run_pipeline (反射注册)
│   │   │   └── protocols.py       ← AfterParse / AfterExecute / Command/Directive/MessageProvider
│   │   ├── command/
│   │   │   ├── command.py         ← Command 类
│   │   │   ├── decorator.py       ← @command 装饰器
│   │   │   └── parameter.py       ← CommandParameter
│   │   ├── llm/providers/
│   │   │   ├── multi.py           ← MultiProvider + lazy provider cache
│   │   │   ├── openai.py
│   │   │   ├── anthropic.py
│   │   │   └── groq.py
│   │   ├── components/
│   │   │   ├── action_history/    ← Episode + EpisodicActionHistory
│   │   │   ├── file_manager/
│   │   │   ├── code_executor/
│   │   │   ├── web/
│   │   │   ├── system/
│   │   │   └── ... (共 19 个 subdir)
│   │   ├── file_storage/
│   │   │   ├── base.py            ← FileStorage ABC + restrict_to_root
│   │   │   ├── local.py           ← LocalFileStorage + _sanitize_path
│   │   │   ├── s3.py
│   │   │   └── gcs.py
│   │   └── permissions.py         ← CommandPermissionManager + ApprovalScope
│   └── tests/
│
├── direct_benchmark/                    [基准测试 harness]
└── README.md / SECURITY.md / TROUBLESHOOTING.md
```

## 2. 推荐阅读顺序（12 个文件）

按学习仓库的章节顺序读上游，每文件配一句指针：

| # | 上游文件 | 对应章节 | 读什么 |
|---|---|---|---|
| 1 | `classic/README.md` | (orientation) | 项目定位 + 当前已 deprecated 的提醒 |
| 2 | `original_autogpt/autogpt/app/main.py:655-833` | s01, s09 | `run_interaction_loop` + signal handler + cycle decrement |
| 3 | `original_autogpt/autogpt/agents/agent.py:1-100` | s01 | Agent 类的字段与导入——所有组件都汇集在这里 |
| 4 | `original_autogpt/autogpt/agents/agent.py:270-300` | s01, s10 | `propose_action` 中的 `create_chat_completion` 调用 + `run_pipeline(AfterParse.after_parse, result)` |
| 5 | `forge/forge/command/command.py` + `decorator.py` | s02 | `@command` 装饰器实现 + Command 类 + parameter validation |
| 6 | `forge/forge/llm/providers/multi.py` | s03 | `MultiProvider.CHAT_MODELS` + lazy `_provider_instances` cache + `get_model_provider` |
| 7 | `original_autogpt/autogpt/agents/prompt_strategies/one_shot.py` | s04 | `OneShotAgentPromptStrategy.build_prompt` + `parse_response_content` |
| 8 | `forge/forge/components/action_history/action_history.py` + `model.py` | s05 | `Episode` dataclass + `EpisodicActionHistory.prepare_messages`（含 LLM 总结的 hook） |
| 9 | `forge/forge/file_storage/base.py` + `local.py` | s06 | `FileStorage` ABC + `LocalFileStorage._sanitize_path` |
| 10 | `forge/forge/permissions.py` | s07 | `CommandPermissionManager.check_command` + `_pattern_matches` + `ApprovalScope` 4-level 枚举 |
| 11 | `forge/forge/agent/protocols.py` + `forge/components/file_manager/__init__.py` | s08 | 三个 protocol ABC + 一个 component 的具体实现 |
| 12 | `original_autogpt/autogpt/agents/prompt_strategies/reflexion.py` | s10 | `ReflexionPromptStrategy` 完整 600+ 行——Reflexion paper 的实现 |

读 1-3 就有上游 agent 的整体感；读到 7 你能跑全过；读完 12 你完成了 mini → upstream 的对照阅读。

## 3. 5 个进阶练习（基于 mini 推向 production）

每个练习从 learn-AutoGPT 的 Go 实现出发，复用 80% 代码 + 加 20% 新东西。

### 练习 #1：跨轮 Reflexion 与 episodic memory

**目标**：把 s10 的 single-turn 反思扩展为「反思持久化进 history、跨轮检索」的版本。

**起点**：`agents/s10-reflexion-hooks/` 复制为 `agents/sX-reflexion-memory/`。

**改动**：
- `Episode` 加 `Reflections []string` 字段
- ReflexionStrategy.afterParseHook 把 `verdict.Reason` 当作一条 reflection 入档
- `Strategy.BuildPrompt` 把 history 中所有 reflection 渲染为 `<past_lessons>` 块插进 system message
- 测试：连续 3 轮跑同一个错误的 tool，第 3 轮提案应该被前两轮的 reflection 改写

**对照上游**：`prompt_strategies/reflexion.py` 中的 `ReflexionMemory` 和 `Reflection.persist()`。

### 练习 #2：向量 memory backend

**目标**：把 s05 的 `History.RenderMessages` 接到一个本地向量存储，做语义检索而非按时间顺序输出。

**起点**：`agents/s05-episodic-history/` 复制；引入 `github.com/coder/hnsw`（Go 的 HNSW 库）。

**改动**：
- `History.Append` 异步把 episode 文本 embed 后塞 HNSW
- `RenderMessages(query string)` 改签名，按 query 做 KNN top-k
- Strategy 端把当前 task 当 query
- 测试：构造 100 个 episode，验证检索召回率 > 0.8

**对照上游**：`forge/components/memory/` 的 ChromaDB / Pinecone 适配（在 forge 中实际是空 stub，需要看 multi-line 文档）。

### 练习 #3：Agent Protocol REST 服务

**目标**：包一个 HTTP 服务，让别人通过 REST 投任务、轮询状态，实现 [Agent Protocol](https://github.com/AI-Engineer-Foundation/agent-protocol)。

**起点**：`agents/s09-continuous-mode/` 复制；引入 `net/http` + `github.com/go-chi/chi/v5`。

**改动**：
- POST `/ap/v1/agent/tasks` 创建任务（body: `{input: string}`），异步起一个 RunInteractionLoop goroutine
- GET `/ap/v1/agent/tasks/{id}` 返回状态 + history
- POST `/ap/v1/agent/tasks/{id}/steps` 显式推进一步（用于 manual mode）
- 测试：用 Agent Protocol 官方 test suite 的子集

**对照上游**：`original_autogpt/autogpt/app/serve.py` + FastAPI 路由。

### 练习 #4：动态 Plugin loader

**目标**：把 s08 的硬编码组件列表换成「扫描 ./plugins/ 目录加载 .so 插件」的运行时机制。

**起点**：`agents/s08-components/` 复制；用 Go 的 `plugin` 包（注意：仅 Linux/Mac 支持）。

**改动**：
- 每个 plugin 是一个 .so 暴露 `New() Component`
- `main.go` 启动时 `filepath.Walk("./plugins/")`，对每个 .so 调 `plugin.Open` + `Lookup("New")`
- 把返回的 Component 加入 ComponentBus
- 写一个 sample plugin（独立 module）实现 CommandProvider 输出一个 `weather` 工具
- 测试：plugin 加载顺序、错误的 plugin 不阻塞启动

**对照上游**：`original_autogpt/plugins/` 的 Python plugin loader（用 importlib + entry_points）。

### 练习 #5：完整 4-level 权限 scope

**目标**：把 s07 的 2-level (Allow/Deny/Ask) 升级成上游的完整 4-level (`ONCE` / `AGENT` / `WORKSPACE` / `DENY`)。

**起点**：`agents/s07-permissions/` 复制。

**改动**：
- `Decision` 加 4 个值
- `Permissions` 加 `Save(scope Scope)` 方法把 ask 的回答持久化到 `~/.autogpt/agents/{id}/permissions.yaml`（agent scope）或 `./permissions.yaml`（workspace scope）
- StdinAsker 给用户 4 个选项（One time / This agent / This workspace / Deny forever）
- 测试：scope=AGENT 持久化后，第二次 Check 不再问；scope=ONCE 不持久化

**对照上游**：`forge/forge/permissions.py` 的 `ApprovalScope` 枚举 + `_save_permission` 方法。

---

## 4. 进一步阅读

- AutoGPT Platform 文档（不在我们的 derivative 范围，但可以读）：https://docs.agpt.co/
- LangGraph 教程：[langchain-ai/langgraph](https://github.com/langchain-ai/langgraph)
- Anthropic Agents SDK / Claude Agent SDK 文档：https://docs.claude.com/en/docs/claude-code
- Reflexion paper：https://arxiv.org/abs/2303.11366
- ReAct paper：https://arxiv.org/abs/2210.03629
