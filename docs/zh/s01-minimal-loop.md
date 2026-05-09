---
title: "s01 · 最小 think→act→observe 循环"
chapter: 1
slug: s01-minimal-loop
est_read_min: 12
---

# s01 · 最小 think→act→observe 循环

> 教什么：用 ~250 行 Go 复刻 AutoGPT 经典版 `run_interaction_loop` 的"最小可识别"形态。机制名是 **agent_loop**——也就是后续每一节都会再扩展的那个核心。

---

## Problem / 问题

AutoGPT 经典版的入口是 `classic/original_autogpt/autogpt/app/main.py` 里的 `run_interaction_loop`：一个 `while cycles_remaining > 0:` 包住 `propose_action() → execute()` 的状态机，外加信号处理、UI 渲染、cycle 预算、permission 检查、四种 stop_reason 的容错。一上来就读这 100 多行很难看清"什么是骨架，什么是装饰"。

这一节要解决的痛点：**剥掉所有装饰，让骨架可以单独运行**。我们要把 think→act→observe 的核心拎出来——LLM 调用、tool dispatch、tool_result 回填、end_turn 终止——其它都不要，包括 cycle budget（s09 才加）、permissions（s07）、history（s05）、components（s08）、UI provider（s09）。读者跑通 s01，就拥有了一个能看懂"为什么 agent 能自己决策"的最小心智模型。

## Solution / 解决方案

把循环拆成 **3 个完全正交的接口**：`Provider`（LLM 调用）、`Tool`（capability）、`Loop`（控制流）。三者之间用 **Anthropic 的 content-block 协议**作为内部数据形态——`text` / `tool_use` / `tool_result` 是一个干净的 tagged union，比 OpenAI 那种"`tool_calls` 是 assistant message 字段、tool 结果是单独 role"的拼贴形态更适合教学。

关键决策点：

1. **协议形态 vs 厂商 wire 形态分开** —— 内部走 Anthropic 块结构，对外通过 `OpenAIProvider` 做翻译。这样 `Loop` 完全不用感知厂商差异，s03 加更多 backend 时不动一行 `loop.go`。
2. **MaxTurns 是硬上限，不是 cycle budget** —— 上游的 `cycles_remaining` 有"用户中断不扣 cycle"等条件分支，我们简化成"循环 N 次还没 end_turn 就报错退出"。这个分支的复杂度留到 s09。
3. **只给 EchoTool，不给 BashTool** —— 有 shell 就有 `rm -rf .`。安全沙箱是 s06 `Workspace` 的工作。s01 任务是"看清楚循环"，不是"做有用的事"。

## How It Works / 工作原理

```ascii-anim frames=2
┌────────────────────────────────────────────────────────────┐
│   user prompt                                              │
│        │                                                   │
│        ▼                                                   │
│   ┌─────────┐  CreateMessage  ┌────────────┐              │
│   │  Loop   │ ───────────────▶│  Provider  │              │
│   │         │◀─────────────── │  (LLM)     │              │
│   └─────────┘   {Content,     └────────────┘              │
│        │       StopReason}                                 │
│        │                                                   │
│        │  switch StopReason:                              │
│        ├── end_turn  ──▶ return text                      │
│        ├── tool_use  ──▶ for each block.Name in toolsByName│
│        │                  Execute(input) → tool_result    │
│        │                  append as user msg, loop again  │
│        └── max_tokens / unknown ──▶ error                 │
│                                                            │
│   MaxTurns guard prevents infinite loops                   │
└────────────────────────────────────────────────────────────┘
```

核心 30-60 行（节选自 [`agents/s01-minimal-loop/loop.go`](https://github.com/yeding/learn-AutoGPT/blob/main/agents/s01-minimal-loop/loop.go)）：

```go
func (l *Loop) Run(ctx context.Context, userPrompt string) (string, error) {
    toolByName := map[string]Tool{}
    schemas := make([]ToolSchema, 0, len(l.Tools))
    for _, t := range l.Tools {
        s := t.Schema()
        toolByName[s.Name] = t
        schemas = append(schemas, s)
    }

    messages := []Message{
        {Role: "user", Content: []ContentBlock{{Type: "text", Text: userPrompt}}},
    }

    for turn := 0; turn < l.MaxTurns; turn++ {
        resp, err := l.Provider.CreateMessage(ctx, CreateMessageRequest{
            Messages: messages,
            Tools:    schemas,
        })
        if err != nil {
            return "", fmt.Errorf("turn %d: %w", turn, err)
        }

        // 1. 即使包含 tool_use 块，assistant 这一轮也必须进 history。
        messages = append(messages, Message{Role: "assistant", Content: resp.Content})

        // 2. stop_reason 决定下一步。
        switch resp.StopReason {
        case "end_turn", "stop_sequence":
            return extractText(resp.Content), nil

        case "tool_use":
            toolResults, err := l.runTools(ctx, resp.Content, toolByName, turn)
            if err != nil {
                return "", err
            }
            // tool_result 必须以 user 角色发回去，每个 tool_use 配一个 tool_result。
            messages = append(messages, Message{Role: "user", Content: toolResults})

        case "max_tokens":
            return "", fmt.Errorf("hit max_tokens at turn %d (response was truncated)", turn)

        default:
            return "", fmt.Errorf("unexpected stop_reason %q at turn %d", resp.StopReason, turn)
        }
    }
    return "", fmt.Errorf("loop exceeded MaxTurns=%d without end_turn", l.MaxTurns)
}
```

**4 个非显然之处**：

1. **assistant 的 tool_use 块要先入 history，才能 append tool_result** —— 协议要求：每个 `tool_use_id` 必须能在前一条 assistant 消息里找到对应的 `tool_use` 块。先 append assistant、再 append user(tool_result)，顺序不能颠倒。
2. **tool_result 是 user 角色，不是 system 也不是 tool** —— Anthropic 的 wire format 把工具反馈打包进 user 消息的 content blocks。OpenAI 把它单独做成 `role: "tool"`。我们采 Anthropic 形态，`OpenAIProvider` 在出站时做翻译。
3. **未知 tool name 不抛错，反馈给模型让它自我修正** —— `loop.go::runTools` 里看到 `byName[block.Name]` miss，会构造一个 "unknown tool: %q" 的 tool_result 喂回去。如果直接 return error，模型连"为什么失败"都看不见，就不可能学会避免。
4. **MaxTurns 退出时返回 error，不返回部分结果** —— 防止上层把"循环耗尽"误判成"agent 完成了任务"。失败必须显式。

## What Changed / 与上一节的变化

（无 — 这是第一节）

## Try It / 动手试一试

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s01-minimal-loop

# 最简单的 demo：让模型用 echo 工具复述一句话
go run . -v "use the echo tool to say 'banana'"

# 切到 DeepSeek（OpenAI-compat），看一样的循环用不同 backend 跑
export DEEPSEEK_API_KEY=...
go run . -provider deepseek -v "use the echo tool to say 'hello'"

# 跑全部测试
go test -v ./...
```

期望输出形态：

```
[s01-minimal-loop] provider=anthropic model=claude-sonnet-4-6 url=
[turn 0] assistant: I'll use the echo tool now.
[turn 0] -> echo map[message:banana]
[turn 0] <- banana
[turn 1] assistant: Here you go: banana.
Here you go: banana.
```

如果你看到 `[turn 0] -> echo` 但没有 `<- banana`，说明 tool 报错了；如果看到一连串 `[turn N]` 直到 MaxTurns，说明模型不肯发 end_turn——通常 prompt 太开放或 max_tokens 太小。

## Upstream Source Reading / 上游源码阅读

AutoGPT 经典版的同等机制在 `classic/original_autogpt/autogpt/app/main.py::run_interaction_loop`。它和我们 mini 版的差别主要是"装饰"：cycle 预算、信号处理、UI provider、连续模式、AgentFinished 路径——核心 think→act→observe 三步在两侧是一样的。

```upstream:classic/original_autogpt/autogpt/app/main.py
# Source: classic/original_autogpt/autogpt/app/main.py (run_interaction_loop)
# 简化：删除 logger、UI 渲染、AgentFinished 分支、TTS。保留 plan→execute 主干。

async def run_interaction_loop(agent, ui_provider=None):
    app_config = agent.app_config
    ai_profile = agent.state.ai_profile

    # cycle_budget = 用户允许 agent 自动跑多少轮，None 等价于 ∞
    cycle_budget = cycles_remaining = _get_cycle_budget(
        app_config.continuous_mode, app_config.continuous_limit
    )
    stop_reason = None

    # SIGINT 第一次按 → 把 cycles_remaining 砍到 1（让当前轮跑完干净退出）
    # 第二次按 → AgentTerminated（强退）
    def graceful_agent_interrupt(signum, frame):
        nonlocal cycles_remaining, stop_reason
        if stop_reason:
            sys.exit()
        if cycles_remaining in [0, 1]:
            stop_reason = AgentTerminated("Interrupt signal received")
        else:
            cycles_remaining = 1

    def handle_stop_signal():
        if stop_reason:
            raise stop_reason

    signal.signal(signal.SIGINT, graceful_agent_interrupt)

    consecutive_failures = 0

    while cycles_remaining > 0:
        ########
        # Plan #
        ########
        handle_stop_signal()
        # 只在没有进行中的 episode（或上一个 episode 已有 result）时才 propose
        if not (_ep := agent.event_history.current_episode) or _ep.result:
            async with ui_provider.show_spinner("Thinking..."):
                try:
                    action_proposal = await agent.propose_action()
                except InvalidAgentResponseError as e:
                    consecutive_failures += 1
                    if consecutive_failures >= 3:
                        raise AgentTerminated("...3 invalid thoughts in a row...")
                    continue
        else:
            action_proposal = _ep.action

        consecutive_failures = 0

        ###################
        # Execute Command #
        ###################
        if not action_proposal.use_tool:
            continue

        try:
            result = await agent.execute(action_proposal)
        except AgentFinished as e:
            # ... 交互式让用户输入下一个 task ...
            continue

        # 关键：用户中断（在 permission prompt 里选 deny）不扣 cycle
        # 这样 user feedback 不会让 budget 加速耗尽
        if result.status != "interrupted_by_human":
            cycles_remaining -= 1
```

```upstream:classic/original_autogpt/autogpt/agents/agent.py
# Source: classic/original_autogpt/autogpt/agents/agent.py
# (propose_action + complete_and_parse — 简化掉 directives 收集和异常分支)

async def propose_action(self) -> AnyActionProposal:
    self.reset_trace()

    # Pipeline 钩子：从所有 components 收集 directives / commands / messages
    self.commands = await self.run_pipeline(CommandProvider.get_commands)
    messages = await self.run_pipeline(MessageProvider.get_messages)

    prompt: ChatPrompt = self.prompt_strategy.build_prompt(
        messages=messages,
        task=self.state.task,
        ai_profile=self.state.ai_profile,
        ai_directives=self.state.directives,
        commands=function_specs_from_commands(self.commands),
    )

    output = await self.complete_and_parse(prompt)
    self.config.cycle_count += 1
    return output

async def complete_and_parse(self, prompt: ChatPrompt) -> AnyActionProposal:
    # ↓ 这一行是 LLM 调用现场。等价于我们 mini 版的 Provider.CreateMessage。
    response = await self.llm_provider.create_chat_completion(
        prompt.messages,
        model_name=self.llm.name,
        completion_parser=self.prompt_strategy.parse_response_content,
        functions=prompt.functions,
        prefill_response=prompt.prefill_response,
    )
    result = response.parsed_result

    # AfterParse 钩子：所有 components 在 LLM 出结果后都能改 / 评估它（s10 教这个）
    await self.run_pipeline(AfterParse.after_parse, result)

    return result
```

**对照阅读要点**：

- **`while cycles_remaining > 0` vs `for turn := 0; turn < MaxTurns`** —— 上游用"运行预算"模型（连续模式 = ∞，否则 = 用户给的次数），我们用"硬上限"。预算扣减条件（中断不扣）在 s09 的连续模式章节加回来。
- **`propose_action` vs `Provider.CreateMessage`** —— 上游 `propose_action` 把"收集 commands / messages → 构造 ChatPrompt → LLM 调用 → 解析"四步打包；我们 s01 把这四步退化成"build messages 内联在 Loop + LLM 调用 = Provider"。Strategy 抽象（s04）会把"build / parse"拆出来。
- **`run_pipeline(AfterParse.after_parse)` 我们没有** —— 上游每次 LLM 出结果都跑 AfterParse 钩子，可以做 Reflexion 二次评估。s10 会引入 `Pipeline` + `AfterParseHook`。
- **错误恢复**：上游有 `consecutive_failures >= 3 → AgentTerminated`、`InvalidAgentResponseError`、`AgentFinished`、`interrupted_by_human` 四条专用通道。我们 s01 只有"unknown tool 喂回 tool_result 让模型自救"一种——其它边界 case 故意不做，保持骨架可读。
- **故意保留的"正确但不完美"**：MaxTurns 退出直接返回 error，没有像上游一样保留中间 history 让外部检视。在 s05（episodic-history）和 s09（continuous-mode）我们会把 history 暴露出来，让 caller 在循环结束后还能看 thoughts。

**想读更多**：从 `app/main.py:run_interaction_loop` 入手（这一节的中心），跟着 `agent.propose_action` 进 `agents/agent.py:290-356`，再读 `agents/agent.py:358-387` 的 `complete_and_parse` 看到真正的 LLM 调用现场。这条线就是 s01 → s04（prompt strategy）→ s10（reflexion hooks）的真实代码地图。

---

**下一节预告**：s02 把 s01 里硬编码的 `[]Tool{NewEchoTool()}` 改成显式 `Registry`——上游用 `@command` 装饰器自动注册，Go 没有装饰器，所以我们要个能让任何文件 `Register(myTool)` 的注册中心。这也是 s07（permissions）和 s08（components）的前置依赖。
