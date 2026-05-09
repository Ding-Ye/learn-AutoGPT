---
title: "s04 · Prompt 策略与解析"
chapter: 4
slug: s04-prompt-strategy
est_read_min: 14
---

# s04 · Prompt 策略与解析

> 教什么：把 prompt 构建从 Loop 里抽出来，藏在 `PromptStrategy` 接口后面。strategy 决定 system prompt（角色 + 工具列表 + 5 条 best-practices）和初始 messages，并把模型响应解析成 `ActionProposal`——支持 native `tool_use` 与 ```json 围栏两条路径。这是 AutoGPT classic 8 个策略的入口；我们只实现 `OneShotStrategy`，Reflexion 留给 s10。

---

## Problem / 问题

s01 / s02 / s03 的 Loop 是这样开局的：

```go
messages := []Message{{
    Role: "user",
    Content: []ContentBlock{{Type: "text", Text: userPrompt}},
}}
```

一句 user prompt，没有 system prompt，没有角色描述，没有"读文件之前先想清楚"这类直接写死的 best-practice，没有 tool 列表的人类可读说明（虽然 `req.Tools` 走 schema 通道，但 system 提示里没有"你 *只能* 用这些命令"这类显式约束）。

这种最小 prompt 在 s01/02/03 跑得通——因为我们只测两件事：(a) 模型能不能调起工具；(b) 工具能不能 round-trip。但真正用起来缺三样：

1. **缺角色定义** — 模型不知道自己是个 autonomous agent，不知道任务完成后该说什么、不该说什么、什么时候该 stop。
2. **缺最佳实践注入** — AutoGPT classic 在 `OneShotAgentPromptConfiguration.DEFAULT_BODY_TEMPLATE` 里放了 7 条 efficiency guidelines（"UNDERSTAND BEFORE ACTING"、"PARALLEL EXECUTION"、"FIX ROOT CAUSE"等）。这些是经验编码——少了它们模型会乱来（写半截代码、不读文件就改、把 test 当 bug 改）。
3. **缺响应解析的回退路径** — 大模型（claude-sonnet、gpt-4o、deepseek-chat）原生 tool_use 很可靠；但小模型（older llama、某些 vLLM 自部署版本、一些 Mistral 派生模型）的 tool-calling head 不稳，更倾向于在文本里塞个 ```json `{"command": "...", "args": {...}}` ```围栏。s01-s03 的 Loop 看到 stop_reason="end_turn" 就直接终止了，模型本意要调工具的信号被丢了。

s04 用一个抽象解决这三件：`PromptStrategy` 接口。

## Solution / 解决方案

**接口长这样**：

```go
type ActionProposal struct {
    Thoughts string                 // 文本推理（可空）
    Command  string                 // 工具名（空表示无动作）
    Args     map[string]interface{} // 工具入参
}

type PromptStrategy interface {
    BuildPrompt(history []*Episode, tools []ToolSchema, task string) []Message
    ParseResponse(content []ContentBlock) (ActionProposal, error)
}
```

两个方法，分别管两端：

- **BuildPrompt** — 决定打开对话的形态。s04 的 `OneShotStrategy.BuildPrompt` 只返回一条 user message（task 原文），但 system prompt 通过单独的 `BuildSystem(tools)` 返回——因为 Anthropic 把 system 当顶层字段，不是 message。Loop 会把 BuildSystem 的输出塞到 `CreateMessageRequest.System`。
- **ParseResponse** — 把 assistant 响应的 `[]ContentBlock` 翻译成 `ActionProposal`。两条路径，按优先级：
  1. 任意 block.Type == "tool_use"：原生 tool 调用，直接读 Name/Input。
  2. 没有 tool_use，但 text block 里有 ```json ... ``` 围栏，且 JSON 是 `{"command": "...", "args": {...}}` 形态：fallback 解析。
  3. 都没有：返回 error。

**Episode 是占位**。s04 里 `BuildPrompt(history []*Episode, ...)` 永远收到 nil。s05 才会把 Episode struct 填上 Actions/Results 字段、把历史折叠进 system 后的 messages。这是个**前向兼容缝**——s04 已经把 history 参数留好，s05 只动 strategy 实现，不动接口。

**为什么是 5 条而不是 7 条 best-practice**：上游 7 条里第 6 条（CODE STYLE）和第 7 条（SECURITY: never log secrets）只在 s06 引入 Workspace、s08 引入 web/file 组件之后才有真实意义——agent 在 s04 阶段还只能 echo+math，谈不上 mimic 代码风格也碰不到 secrets。提前注入这两条等于让 prompt 撒谎，所以删掉。

## How It Works / 工作原理

```ascii-anim frames=2
┌────────────────────────────────────────────────────────────────────┐
│   Loop.Run(ctx, "compute 2+3")                                     │
│         │                                                          │
│         ▼                                                          │
│   strategy := l.Strategy                  ← s04 新字段             │
│   if strategy == nil:                                              │
│       strategy = NewOneShotStrategy()    ← 默认                    │
│         │                                                          │
│         ▼                                                          │
│   schemas := l.Tools.All()                                         │
│   messages := strategy.BuildPrompt(nil, schemas, task)            │
│   system   := strategy.BuildSystem(schemas)                       │
│         │                                                          │
│         ▼                                                          │
│   for turn < MaxTurns:                                            │
│       resp := provider.CreateMessage(req{System, Messages, Tools})│
│       proposal := strategy.ParseResponse(resp.Content) ← 新缝     │
│       switch resp.StopReason:                                      │
│           "tool_use":                                              │
│               results := runTools(resp.Content)  ← 协议原生分支    │
│               messages.append(user-role tool_results)              │
│           "end_turn":                                              │
│               return extractText(resp.Content)                     │
│           default:  ← s04 新增 JSON-fence fallback 分支            │
│               if proposal.Command != "":                          │
│                   results := runFallbackTool(proposal)            │
│                   messages.append(synthesized tool_result)         │
└────────────────────────────────────────────────────────────────────┘
```

### System prompt 渲染（节选自 `strategy.go`）

`OneShotStrategy.BuildSystem` 拼三段：role intro、`## Commands`、`## Best practices`：

```go
func (s *OneShotStrategy) BuildSystem(tools []ToolSchema) string {
    var b strings.Builder
    b.WriteString("You are a methodical autonomous agent. ")
    b.WriteString("Decide one or more tool calls per turn, observe the result, then continue. ")
    b.WriteString("When the task is complete, reply with plain text and no tool call.")

    b.WriteString("\n\n## Commands\n")
    if len(tools) == 0 {
        b.WriteString("(no tools available; respond with plain text)\n")
    } else {
        b.WriteString("These are the ONLY commands you can use. Any action you perform must be possible through one of these:\n")
        for i, t := range tools {
            schemaJSON, _ := json.Marshal(t.InputSchema)
            fmt.Fprintf(&b, "%d. **%s** — %s\n   input_schema: %s\n",
                i+1, t.Name, t.Description, string(schemaJSON))
        }
    }

    b.WriteString("\n## Best practices\n")
    for i, line := range s.BestPractices {
        fmt.Fprintf(&b, "%d. %s\n", i+1, line)
    }

    return strings.TrimRight(b.String(), "\n")
}
```

跑出来给 echo+math 两个 tool 注册的样子（节选）：

```text
You are a methodical autonomous agent. Decide one or more tool calls
per turn, observe the result, then continue. When the task is complete,
reply with plain text and no tool call.

  ## Commands
  These are the ONLY commands you can use. Any action you perform must be
  possible through one of these:
  1. **echo** — Echo back the input message verbatim. Useful for testing
     the tool-use round-trip without side effects.
     input_schema: {"properties":{"message":...},"required":["message"]}
  2. **math** — Evaluate a basic arithmetic operation (add | sub | mul | div)
     over two numbers. Returns the result as a string.
     input_schema: {"properties":{"operation":..., "a":..., "b":...},
                    "required":["operation","a","b"]}

  ## Best practices
  1. UNDERSTAND BEFORE ACTING: read all relevant files / inputs before
     making changes; never guess at interfaces.
  2. PARALLEL EXECUTION: when independent operations can run concurrently,
     request them in one turn rather than serializing.
  3. WRITE COMPLETE CODE: produce full working implementations — no stubs,
     TODOs, or placeholders.
  4. VERIFY AFTER CHANGES: after modifying state, verify the change took
     (re-read a file, re-run a check).
  5. FIX ROOT CAUSE: when something breaks, fix the underlying cause, not
     the symptom; if a test fails, the bug is in your code, not the test.
```

### ParseResponse 的两条路径

```go
func (s *OneShotStrategy) ParseResponse(content []ContentBlock) (ActionProposal, error) {
    var thoughts []string
    var toolUseBlock *ContentBlock
    var allText []string

    for i := range content {
        b := &content[i]
        switch b.Type {
        case "tool_use":
            if toolUseBlock == nil { toolUseBlock = b }   // 第一个 tool_use 胜出
        case "text":
            allText = append(allText, b.Text)
            thoughts = append(thoughts, b.Text)
        }
    }

    // 路径 1：原生 tool_use
    if toolUseBlock != nil {
        return ActionProposal{
            Thoughts: strings.TrimSpace(strings.Join(thoughts, "\n")),
            Command:  toolUseBlock.Name,
            Args:     toolUseBlock.Input,
        }, nil
    }

    // 路径 2：```json ... ``` fence 回退
    combined := strings.Join(allText, "\n")
    if match := fenceRegex.FindStringSubmatch(combined); len(match) > 1 {
        payload := strings.TrimSpace(match[1])
        var parsed struct {
            Command string                 `json:"command"`
            Args    map[string]interface{} `json:"args"`
        }
        if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
            return ActionProposal{}, fmt.Errorf("parse JSON fallback: %w (payload=%q)", err, payload)
        }
        if parsed.Command == "" {
            return ActionProposal{}, fmt.Errorf("parse JSON fallback: missing required field %q", "command")
        }
        thoughtsText := strings.TrimSpace(fenceRegex.ReplaceAllString(combined, ""))
        return ActionProposal{Thoughts: thoughtsText, Command: parsed.Command, Args: parsed.Args}, nil
    }

    // 路径 3：都没有 → 报错
    return ActionProposal{}, fmt.Errorf("ParseResponse: response has neither tool_use block nor JSON-fenced action (content blocks: %d)", len(content))
}
```

### 三个非显然之处

1. **System prompt 不是 Message** — Anthropic 的 `messages` API 把 system 设计成顶层字段（不是 role:"system" 的 message）。所以 `BuildPrompt` 返回 []Message 不放 system；BuildSystem 是单独的 string。Loop 通过类型断言把 OneShotStrategy 的 BuildSystem 拿出来塞进 `CreateMessageRequest.System`。换 Strategy 实现时如果需要不同 system，可以在 strategy 上加自己的 BuildSystem 方法（不进 PromptStrategy 接口，避免每个 strategy 强制实现）。
2. **Native 优先于 Fence** — 如果模型同时给了 native tool_use 和文本里的 ```json fence（不该发生但偶现），native 路径胜出。否则 ParseResponse 会执行两次同一个动作，对外可观测就是 LLM "double-clicked" 自己。测试用例 `TestOneShotStrategy_ParseResponse_NativeWinsOverFence` 锁这个契约。
3. **Loop 里的 default 分支** — s03 的 switch 默认情况是直接报错。s04 改成"先尝试 ParseResponse 一遍"——因为 stop_reason=end_turn 但实际模型想调工具的情况，正是 fence 回退路径要救的场景。`runFallbackTool` 合成一个 `tool_use_id` 让 tool_result 回路对 Provider 看起来一致。

## What Changed / 与 s03 的变化

```diff
 agents/s04-prompt-strategy/
 ├── provider.go              # 与 s03 一字不差
 ├── provider_openai.go       # 与 s03 一字不差
 ├── provider_mock.go         # 与 s03 一字不差
 ├── provider_anthropic_test.go  # 与 s03 一字不差
 ├── provider_openai_test.go  # 与 s03 一字不差
 ├── provider_mock_test.go    # 与 s03 一字不差
 ├── tools.go / tools_test.go # 与 s03 一字不差
 ├── registry.go / registry_test.go  # 与 s03 一字不差
+├── strategy.go              # 新增：PromptStrategy + OneShotStrategy + ActionProposal + Episode 占位
+├── strategy_test.go         # 新增：8 个测试覆盖 BuildSystem/BuildPrompt/ParseResponse 两条路径
 ├── loop.go                  # 改：Loop 加 Strategy 字段；Run 调 strategy.BuildPrompt + ParseResponse；新增 fallback 分支
 ├── loop_test.go             # s03 测试 + 新增 stubStrategy 测试验证 strategy 被正确调用
 └── main.go                  # 加 -strategy oneshot flag（s04 唯一选项；s10 引入 reflexion）
```

类型 catalog 新增：

```go
type ActionProposal struct { Thoughts, Command string; Args map[string]interface{} }
type Episode      struct { /* s05 fills in */ }
type PromptStrategy interface { BuildPrompt(history []*Episode, tools []ToolSchema, task string) []Message; ParseResponse([]ContentBlock) (ActionProposal, error) }
type OneShotStrategy struct { BestPractices []string }
```

`Loop` 字段新增 `Strategy PromptStrategy`；零值兜底：Run 第一行检测 nil 就构造默认 `NewOneShotStrategy()`，所以 s03 风格的 `&Loop{Provider: p, Tools: reg, MaxTurns: 5}` 仍然合法。

## Try It / 动手试一试

```bash
cd agents/s04-prompt-strategy

# Anthropic native + oneshot（默认 strategy）
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "use the math tool to add 7 and 35"

# DeepSeek（OpenAI-compat），看 Loop 把 system prompt 翻译成 OpenAI role:"system" message
export DEEPSEEK_API_KEY=sk-...
go run . -provider deepseek -v "echo back hi"

# 本地 vLLM / SGLang，常见的 fence-fallback 触发场景
export OPENAI_API_KEY=anything
go run . -provider local -model llama-3.3 "compute 6 * 7"

# 跑全部测试（应该 47 通过）
go test -v ./...
```

期望输出形态（Anthropic 路径）：

```
[s04-prompt-strategy] provider=anthropic model=claude-sonnet-4-6 url= strategy=oneshot tools=2
[turn 0] assistant: I'll use the math tool to add 7 and 35.
[turn 0] proposal: cmd=math thoughts="I'll use the math tool to add 7 and 35."
[turn 0] -> math map[a:7 b:35 operation:add]
[turn 0] <- 42
[turn 1] assistant: 7 + 35 = 42.
7 + 35 = 42.
```

如果是 fence-fallback 路径（模拟一个不输出 native tool_use 的小模型）：

```
[turn 0] assistant: I'll use the echo tool. ```json
{"command": "echo", "args": {"message": "hi"}}
```
[turn 0] JSON-fallback proposal: cmd=echo
[turn 1] assistant: hi
hi
```

测试覆盖了两条路径——`TestOneShotStrategy_ParseResponse_NativeToolUse` 与 `TestOneShotStrategy_ParseResponse_JSONFenceFallback` 直接锁住断言。

## Upstream Source Reading / 上游源码阅读

AutoGPT classic 的 prompt strategy 在 `classic/original_autogpt/autogpt/agents/prompt_strategies/`，8 个文件——`base.py` 是 ABC，`one_shot.py` 是基线（其余 reflexion/rewoo/plan_execute/lats/tree_of_thoughts/multi_agent_debate 是变体）。我们对照 `one_shot.py` 的核心 3 个方法：

```upstream:classic/original_autogpt/autogpt/agents/prompt_strategies/one_shot.py
# Source: classic/original_autogpt/autogpt/agents/prompt_strategies/one_shot.py
# 简化：去掉 Pydantic 校验、prefill 钩子；保留三段式（system / task / response_format）。

class OneShotAgentPromptConfiguration(SystemConfiguration):
    DEFAULT_BODY_TEMPLATE: str = (
        "## Constraints\n{constraints}\n\n"
        "## Resources\n{resources}\n\n"
        "## Commands\n"
        "These are the ONLY commands you can use."
        " Any action you perform must be possible through one of these commands:\n"
        "{commands}\n\n"
        "## Best practices\n{best_practices}\n\n"
        "## Efficiency Guidelines\n"
        "1. UNDERSTAND BEFORE ACTING: Read ALL relevant files before making changes...\n"
        "2. PARALLEL EXECUTION: When multiple operations don't depend on each other, "
        "execute them simultaneously...\n"
        "3. WRITE COMPLETE CODE: Write complete, working implementations...\n"
        "4. VERIFY AFTER CHANGES: After modifying code, verify it works...\n"
        "5. FIX ROOT CAUSE: When debugging, fix the underlying issue, not symptoms...\n"
        "6. CODE STYLE: Mimic existing code conventions...\n"
        "7. SECURITY: Never expose, log, or commit secrets, API keys, or credentials."
    )
    body_template: str = UserConfigurable(default=DEFAULT_BODY_TEMPLATE)
    use_prefill: bool = True


class OneShotAgentPromptStrategy(PromptStrategy):
    def build_prompt(
        self, *, messages, task, ai_profile, ai_directives,
        commands, include_os_info, **extras,
    ) -> ChatPrompt:
        system_prompt, response_prefill = self.build_system_prompt(
            ai_profile=ai_profile, ai_directives=ai_directives,
            commands=commands, include_os_info=include_os_info,
        )
        final_instruction_msg = ChatMessage.user(self.config.choose_action_instruction)
        return ChatPrompt(
            messages=[
                ChatMessage.system(system_prompt),
                ChatMessage.user(f'"""{task}"""'),
                *messages,                 # ← 历史，对应我们 history []*Episode
                final_instruction_msg,
            ],
            prefill_response=response_prefill if self.config.use_prefill else "",
            functions=commands,
        )

    def parse_response_content(
        self, response: AssistantChatMessage,
    ) -> OneShotAgentActionProposal:
        if not response.content:
            # 模型只给了 tool_calls 没给 text（GPT-5 偶现）
            if response.tool_calls:
                assistant_reply_dict = {"thoughts": {
                    "observations": "", "text": "", "reasoning": "",
                    "self_criticism": "", "plan": [], "speak": "",
                }}
            else:
                raise InvalidAgentResponseError("Assistant response has no text content")
        else:
            assistant_reply_dict = extract_dict_from_json(response.content)

        if not response.tool_calls:
            raise InvalidAgentResponseError("Assistant did not use a tool")
        assistant_reply_dict["use_tool"] = response.tool_calls[0].function
        if len(response.tool_calls) > 1:
            assistant_reply_dict["use_tools"] = [
                tc.function for tc in response.tool_calls
            ]
        parsed_response = OneShotAgentActionProposal.model_validate(assistant_reply_dict)
        parsed_response.raw_message = response.model_copy()
        return parsed_response


class AssistantThoughts(ModelWithSummary):
    observations: str = Field(description="Relevant observations from your last action")
    reasoning: str    = Field(description="Reasoning behind choosing this action")
    self_criticism: str = Field(description="Constructive self-criticism")
    plan: list[str]   = Field(description="Short list that conveys the long-term plan")


class OneShotAgentActionProposal(ActionProposal):
    thoughts: AssistantThoughts  # type: ignore
```

### 对照阅读要点

- **System prompt 三段式 vs 我们的两段式**：上游 `body_template` 有 4 节（Constraints / Resources / Commands / Best practices）+ 后接 Efficiency Guidelines + Task + Response Format，整套大约 1500 字符。我们只放 Commands + Best practices + 一句 role intro，约 400 字符。原因：`Constraints` 与 `Resources` 在上游靠 `AIDirectives` 注入（每个 component 都可以贡献几条），而我们 s04 还没引入 component（s08 才到），先不做空架子。
- **Prefill 我们没做** — 上游 `use_prefill: bool = True` 让 LLM 的回复总是从 `{\n    "thoughts":` 开头，这样 Pydantic 解析能强制 JSON shape。这只对 Anthropic native 有意义（Anthropic 支持 prefill response），且只在 OneShotAgentActionProposal 这种"模型必须输出结构化 JSON" 的设计下才需要。我们直接吃 native tool_use（参数已经是 JSON 对象），不需要 prefill。Reflexion 在 s10 引入"模型自评"时也不打算用 prefill——更直接的办法是再发一次 LLM 调用问"is this proposal sound?"。
- **`AssistantThoughts` 我们没建模** — 上游用 Pydantic 让响应必须包含 observations/reasoning/self_criticism/plan 四段思考。我们 `ActionProposal.Thoughts string` 是单一 free-text 字段，由 ParseResponse 把所有 text block 拼成一行。原因：(a) `AssistantThoughts` 的字段在 OneShot 路径下没有任何下游消费者使用（这是上游一个长期 dead code 风险）；(b) 单字段更适合 streaming/部分输出的情况；(c) Reflexion 真正用到 thoughts 时，再加结构化字段也来得及。
- **`extract_dict_from_json` 是 fence 解析的对应物** — 上游 `forge.json.parsing.extract_dict_from_json` 是个容错 JSON 解析器，从一段文本里"找到第一个看起来像 dict 的 JSON 子串"。我们的 `fenceRegex` 是同一思路的轻量版：只找 ```json``` 围栏，不做更激进的"在任意位置找 dict 起止"。这避免了 false positive（模型在散文里写了个 `{ key: value }` 例子被错当成 action）。
- **`use_tool`/`use_tools` 双字段** — 上游为并发执行预留：单工具放 `use_tool`，多工具放 `use_tools[]`。我们在 s04 只取第一个 tool_use block；并发执行是上游 `_execute_tools_parallel` 的事，s08 component 系统再讨论是否要做。

**想读更多**：从 `classic/original_autogpt/autogpt/agents/prompt_strategies/base.py::PromptStrategy` 读起，对照 `one_shot.py::OneShotAgentPromptStrategy.build_prompt`，再去 `reflexion.py::ReflexionAgentPromptStrategy.build_prompt`——你会发现 Reflexion 几乎是 OneShot 的 wrapper，只在 build_prompt 末尾追加一段"先评估自己上一步是否合理再行动"的 instruction。这正是 s10 要在 Go 里复刻的形态。

---

**下一节预告**：s05 把 Loop 现在的隐式 messages 累积器改成显式的 `History`，由 `Episode{Actions, Results}` 组成。s04 的 `BuildPrompt(history []*Episode, ...)` 已经预留了 history 参数；s05 只动 strategy 实现把 history 折叠回 messages，不动接口。这是 AutoGPT 上游 `EpisodicActionHistory.prepare_messages` 的最小可教学版本。
