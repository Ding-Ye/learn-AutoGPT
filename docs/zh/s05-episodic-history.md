---
title: "s05 · 情节式动作历史"
chapter: 5
slug: s05-episodic-history
est_read_min: 12
---

# s05 · 情节式动作历史

> 教什么：把 Loop 隐式的 `[]Message` 累积器换成显式的 `History []*Episode`。每个 tool 轮 Append 一个 Episode，记录 `Actions []ActionProposal` + `Results []ActionResult`，下一轮的 BuildPrompt 通过 `RenderMessages()` 重建对话。这是 AutoGPT 上游 `EpisodicActionHistory.prepare_messages` 的最小可教学版本。压缩——LLM 总结老 episode 以适配 context window——只留一行 `// (advanced)` 注释作为钩子。

---

## Problem / 问题

s04 的 Loop 在一个扁平 slice 里堆 messages。每轮 append 一条；越堆越长，永远不会变短。5 轮没问题，50 轮就崩。

```go
// s04 Loop 简化版：
messages := strategy.BuildPrompt(nil, schemas, userPrompt)  // ← history 永远是 nil
for turn := 0; turn < l.MaxTurns; turn++ {
    resp, _ := l.Provider.CreateMessage(ctx, ...)
    messages = append(messages, Message{Role: "assistant", Content: resp.Content})
    // ... 如果是 tool_use，跑 tool 然后 append 一条 user-role tool_result message
    // ... 如果是 end_turn，return
}
```

agent 跑得久了具体坏在两件事上：

1. **没有压缩钩子**。当对话长到超过模型 context window，AutoGPT classic 的 `EpisodicActionHistory.handle_compression()` 会扫过老 episodes（超过 `full_message_count` 那一截），让 LLM 把每个总结成一段话，替换原本啰嗦的 action+result。s04 的扁平 slice 没有"在这里压缩老段"的颗粒度——每条 Message 都长得一样。我们需要 *episode*（一对 action + result）这种粒度，summarization 才有连贯单元可操作。

2. **没有结构化历史给 strategy 用**。s04 的 `PromptStrategy.BuildPrompt(history []*Episode, ...)` 已经在签名里留了 `[]*Episode` 参数——但 s04 永远传 `nil`。这个签名是前向兼容缝。s05 真正把它填上。需要把过往 actions/results 折回 prompt 的 strategy（除了最简单的单轮 case 都需要）依赖这个 slice。

s05 一招解决两件事：引入 `Episode` 与 `History` 类型，给 Loop 加 `*History` 字段，让 strategy 调 `History.RenderMessages()` 在每轮重建对话。

## Solution / 解决方案

```go
type ActionResult struct {
    Status string // "ok" | "error" | "interrupted_by_human"
    Output string
}

type Episode struct {
    Actions []ActionProposal // 1+ 个提案（并发 tool 调用 = 同一 episode）
    Results []ActionResult   // result[i] 与 action[i] 配对
}

type History []*Episode

func (h *History) Append(ep *Episode)         // in-place
func (h *History) Current() *Episode          // 最后一个或 nil
func (h *History) RenderMessages() []Message  // ← 压缩钩子位
func (h History)  TrimToLastN(n int) History  // 教学辅助，Loop 不用
```

3 个导出类型，4 个方法。其中 2 个（`Append`、`Current`）就是包装在 method 里的 slice 操作，让 Loop 读起来更自然；`RenderMessages` 是真正干活的——把 Episode 列表转回协议要求的 user/assistant 交替流。

`TrimToLastN` 是刻意保留的教学元素：它演示了压缩 *该放在哪*，但 Loop 永远不调它。Plan 把它叫做"the seam"；测试会跑它；当你 context 撑爆时，再去填 LLM-summarize-old-episodes 的调用。在那之前，`history.go` 文件顶部那一行注释就是你唯一的文档：

```go
// (advanced) when context overflows, summarize old episodes here.
```

## How It Works / 工作原理

```ascii-anim frames=3
┌─────────────────────────────────────────────────────────────────────┐
│   Loop.Run(ctx, "compute 2+3, then echo the result")                │
│         │                                                           │
│         ▼                                                           │
│   if l.History == nil: l.History = &History{}                       │
│   schemas := l.Tools.All()                                          │
│   system  := strategy.BuildSystem(schemas)                          │
│         │                                                           │
│         ▼                                                           │
│   for turn = 0; turn < MaxTurns; turn++:                            │
│       msgs := strategy.BuildPrompt(*l.History, schemas, userPrompt) │
│              │                                                      │
│              ├─ history.RenderMessages()  ← 重建过往轮               │
│              ├─ + Message{user, current task}                       │
│              ▼                                                      │
│       resp := provider.CreateMessage(req{System, msgs, Tools})      │
│              │                                                      │
│       case "tool_use":                                              │
│           ep := &Episode{}                                          │
│           history.Append(ep)             ← s05 新：每轮 append      │
│           ep.Actions = append(ep.Actions, parsed proposal)          │
│           results := runTools(resp.Content)                         │
│           ep.Results = append(ep.Results, results...)               │
│       case "end_turn":                                              │
│           return text                                               │
└─────────────────────────────────────────────────────────────────────┘
```

### `RenderMessages` —— 钩子的位置

```go
// (advanced) when context overflows, summarize old episodes here.
func (h *History) RenderMessages() []Message {
    out := make([]Message, 0, len(*h)*2)
    for _, ep := range *h {
        if ep == nil {
            continue
        }
        // assistant 轮：自由文本 Thoughts + tool_use blocks
        assistantContent := make([]ContentBlock, 0, 2*len(ep.Actions))
        for i, a := range ep.Actions {
            if a.Thoughts != "" {
                assistantContent = append(assistantContent, ContentBlock{Type: "text", Text: a.Thoughts})
            }
            if a.Command != "" {
                assistantContent = append(assistantContent, ContentBlock{
                    Type:  "tool_use",
                    ID:    episodeActionID(ep, len(out), i),
                    Name:  a.Command,
                    Input: a.Args,
                })
            }
        }
        if len(assistantContent) > 0 {
            out = append(out, Message{Role: "assistant", Content: assistantContent})
        }
        // user 轮：每个 Result 对应一个 tool_result block；mid-turn（还没 result）跳过
        if len(ep.Results) == 0 {
            continue
        }
        userContent := make([]ContentBlock, 0, len(ep.Results))
        for i, r := range ep.Results {
            id := episodeActionID(ep, len(out)-1, i)
            userContent = append(userContent, ContentBlock{
                Type:        "tool_result",
                ToolUseID:   id,
                ToolContent: renderResult(r),
            })
        }
        out = append(out, Message{Role: "user", Content: userContent})
    }
    return out
}
```

什么时候要做压缩？这个方法的方法体就是位置。遍历 `*h`，决定哪些 episode 需要压缩（比如除了最后 `full_message_count` 个），对每个老 episode 调 `provider.CreateMessage` 让它总结，把渲染出来的 tool_use+tool_result 替换成一个总结 text block。Loop 不用动。

### 三个非显然之处

1. **`Append` 是指针接收器，`Current` 也是** —— 都必须通过指针访问底层 slice header。否则 `var h History; h.Append(ep)` 会悄悄长一份*副本*。s04 代码到处都是 `messages = append(messages, ...)`；把它包成 method，调用方既能拿到同样的 in-place 语义，也能给操作起个名字。

2. **合成的 `tool_use_id`** —— 当 Loop 把 proposal append 到 `ep.Actions` 时，它没有把响应里原始的 `block.ID` 带过来。RenderMessages 用 `"ep<msgIndex>_act<i>"` 这种形式合成一个稳定 id，让对应的 tool_result block 用相同的值。这是*故意*：原始 wire id 是 assistant 那一轮的实现细节。从逻辑动作重建对话才是 episodic history 的本意。

3. **空 history 返回 `[]Message{}`，不返回 `nil`** —— strategy 会 append 这个 slice，nil slice 也能编译过（Go 的 `append` 处理 nil），但 JSON 编码出去的 provider 请求形态会是 `messages: null` 而不是 `messages: []`。线上不一样。两个 OpenAI-compat 后端（vLLM、llama.cpp 的 server）会直接拒绝 `null`。所以我们显式 `make([]Message, 0, ...)`。

## What Changed / 与 s04 的变化

```diff
 agents/s05-episodic-history/
 ├── provider.go              # 与 s04 一字不差
 ├── provider_openai.go       # 与 s04 一字不差
 ├── provider_mock.go         # 与 s04 一字不差
 ├── provider_anthropic_test.go  # 与 s04 一字不差
 ├── provider_openai_test.go  # 与 s04 一字不差
 ├── provider_mock_test.go    # 与 s04 一字不差
 ├── tools.go / tools_test.go # 与 s04 一字不差
 ├── registry.go              # 与 s04 一字不差
 ├── registry_test.go         # 一处断言改成新的 render 形态
 ├── strategy.go              # 改：BuildPrompt 现在折叠 history.RenderMessages()
 ├── strategy_test.go         # 与 s04 一字不差
+├── history.go               # 新：Episode、ActionResult、History + 4 个方法
+├── history_test.go          # 新：5 个测试
 ├── loop.go                  # 改：Loop.History 字段；每轮 Append + record
 ├── loop_test.go             # s04 测试调整 + 新增 TestLoop_HistoryGrowsAfterEachTurn
 └── main.go                  # 构造 &History{} 串到 Loop
```

类型 catalog 新增：

```go
type ActionResult struct { Status, Output string }
type Episode      struct { Actions []ActionProposal; Results []ActionResult }
type History      []*Episode
```

`Loop` 多了 `History *History` 字段。零值兜底——Run 里 `nil` 会自动分配 `&History{}`——所以 s04 风格的构造（不写 History 字段）继续编译通过。

## Try It / 动手试一试

```bash
cd agents/s05-episodic-history

# Anthropic native + oneshot（默认）
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "add 7 and 35, then echo the result"

# DeepSeek（OpenAI-compat）；rendered history 会经过 OpenAI 翻译层正确转换 ——
# provider.go 没动。
export DEEPSEEK_API_KEY=sk-...
go run . -provider deepseek -v "compute 6 * 7, then echo it"

# 本地 vLLM / SGLang
export OPENAI_API_KEY=anything
go run . -provider local -model llama-3.3 "echo hi, then echo bye"

# 跑全部测试（53 个应该通过）
go test -v ./...
```

期望输出（Anthropic 路径，两个 tool 调用的任务）：

```
[s05-episodic-history] provider=anthropic model=claude-sonnet-4-6 url= strategy=oneshot tools=2
[turn 0] assistant: I'll compute 7 + 35 first.
[turn 0] proposal: cmd=math thoughts="I'll compute 7 + 35 first."
[turn 0] -> math map[a:7 b:35 operation:add]
[turn 0] <- 42
[turn 1] assistant: Now I'll echo the result.
[turn 1] proposal: cmd=echo thoughts="Now I'll echo the result."
[turn 1] -> echo map[message:42]
[turn 1] <- 42
[turn 2] assistant: 7 + 35 = 42 (echoed back).
7 + 35 = 42 (echoed back).
[s05-episodic-history] final history: 2 episodes
```

每个 tool_use 轮都产生一个 Episode；末尾的 `final history: 2 episodes` 印证了 History 从外面可观测。到 turn 2 时模型收到的 messages 包含了 *两个* 过往 episode —— assistant tool_use → user tool_result → assistant tool_use → user tool_result → 末尾的 user task，证明 `RenderMessages` 把对话按时序重建出来了。

## Upstream Source Reading / 上游源码阅读

AutoGPT classic 的 episodic history 在 `classic/forge/forge/components/action_history/`——两个文件，`model.py` 是数据形态，`action_history.py` 是把它暴露给 agent loop 的 component。

```upstream:classic/forge/forge/components/action_history/model.py
# Source: classic/forge/forge/components/action_history/model.py
# 简化：去掉 type vars、asyncio.Lock、Pydantic 配置、OpenAI/Anthropic
# 消息形态分支，保留 Episode + EpisodicActionHistory[T] 核心结构。

@dataclass
class Episode(Generic[T]):
    """一轮 think-act-observe：一个提案 + 结果 + 可选总结。"""
    action: T                        # 提案
    result: Optional[ActionResult]   # 实际发生
    summary: Optional[str] = None    # handle_compression() 填入

    def format(self) -> str:
        """没有 summary 时给 prepare_messages 用的人类可读渲染。"""
        ...


class EpisodicActionHistory(Generic[T]):
    """agent 的事件日志，按 episode 切片。

    [→ s05：对应我们的 `History []*Episode`。上游用类包了 cursor +
       lock + summarizer。我们的 Go 版是带方法的 slice；压缩留作
       (advanced) 注释钩子。]
    """

    def __init__(self, full_message_count: int = 4):
        self.episodes: list[Episode[T]] = []
        self.cursor: int = 0
        self.full_message_count = full_message_count   # 最近多少 episode
                                                       # 不被总结
        self._lock = asyncio.Lock()

    def register_action(self, action: T) -> None:
        """[→ s05：Loop 在 Append(ep) 后做
           `history.Current().Actions = append(..., proposal)`。]"""
        ...

    def register_result(self, result: ActionResult) -> None:
        """[→ s05：Loop 在 Execute 后做
           `ep.Results = append(ep.Results, ...)`。]"""
        ...

    async def handle_compression(self, llm_provider, model_name) -> None:
        """总结老 episode（超过 full_message_count 那一截），让 LLM
        生成一段话总结，把详细 action+result 替换成 summary。

        [→ s05 不实现这个。我们 history.go 里的 `// (advanced)` 注释
           标记了这个方法该落在哪——RenderMessages 里走老 slice 之前。]
        """
        async with self._lock:
            for ep in self.episodes[: -self.full_message_count]:
                if ep.summary is not None:
                    continue
                ep.summary = await self._summarize(ep, llm_provider, model_name)

    def rewind(self, steps: int = 1) -> None:
        """把 cursor 回退 `steps`，删掉部分记录。给 AutoGPT 的
        用户反馈/打断路径用；s05 不建模（要 s09 的 signal handler 才有）。]"""
        ...


# action_history.py（把 History 接进 agent 的 Component）

class ActionHistoryComponent(MessageProvider, AfterParse, AfterExecute,
                              Generic[T]):
    """同时实现三个协议：
       - MessageProvider：把 History 转成 [user, assistant, ...] messages
         注入 prompt。
       - AfterParse：LLM 提案落地后的钩子，把 action 注册到当前 episode。
       - AfterExecute：tool 跑完之后的钩子，把 result 注册上去。
    """

    def __init__(self, event_history: EpisodicActionHistory[T], llm_provider,
                 model_name: str, max_tokens: int = 4096):
        self.event_history = event_history
        self.llm_provider = llm_provider
        self.model_name = model_name
        self.max_tokens = max_tokens

    async def prepare_messages(self, messages: list[ChatMessage]) -> None:
        """LAZY COMPRESSION：只在需要时压缩。如果 rendered history 超出
        预算，调 handle_compression()；否则原样保留。

        [→ s05：我们的 `RenderMessages()` 不做压缩。`// (advanced)` 注释
           标记了 lazy-compress 检查该插在哪。]"""
        if self._needs_compression():
            await self.event_history.handle_compression(
                self.llm_provider, self.model_name,
            )
        # ... 把 rendered messages append 到 `messages` ...

    def after_parse(self, proposal: T) -> None:
        """[→ s05：Loop 的 `ep.Actions = append(ep.Actions, proposal)`。]"""
        self.event_history.register_action(proposal)

    def after_execute(self, result: ActionResult) -> None:
        """[→ s05：Loop 的 `ep.Results = append(ep.Results, ...)`。]"""
        self.event_history.register_result(result)
```

### 对照阅读要点

- **上游的压缩也是 lazy 的** —— `prepare_messages` 只在 budget check 触发时调 `handle_compression`。AutoGPT 不主动压缩；LLM call 贵，大多数短对话压根不需要。我们 `history.go` 里的 `// (advanced)` 就是这个：context 撑爆之前别压缩。

- **上游 `Episode` 对 proposal 类型是 generic 的**；我们写死 `ActionProposal`。AutoGPT 用 `Episode[OneShotAgentActionProposal]` vs `Episode[ReflexionAgentActionProposal]` 来捕捉每个 strategy 自己的字段（Reflexion 的提案多一个 `evaluation` 字段）。我们 Go 版只有一个 ActionProposal struct，因为只 ship 一个 strategy；s10 加 Reflexion 时直接给 ActionProposal 加字段，不参数化。

- **`AfterParse`/`AfterExecute` 是 protocol，不是直接调用** —— 上游 `ActionHistoryComponent` 实现三个可选 protocol，agent loop 的 pipeline 跑所有注册的 hook。我们 Go 版直接在 Loop 里 inline 这部分簿记。s10 才引入 pipeline + hooks 抽象，那时 ActionHistory 才会成为 Component（s08 范围）来实现这些 hook。s08 + s10 一起付出代价；s05 只准备好原料。

- **`rewind` 是给人工打断用的** —— AutoGPT 允许用户按 Ctrl-C，编辑提案，然后恢复。history 的 cursor 让 `rewind(1)` 丢弃 in-flight 的 action，重新 prompt 模型并加上"人说不行，再试一次"的信号。s09 的 continuous-mode 章节会引入这个；s05 还没办法让用户打断（Loop 的 `MaxTurns` 是唯一停止条件）。

- **上游 `full_message_count` 默认 4**。意思是最后 4 个 episode 永远 verbatim 渲染；之外的全部总结。s05 不需要这个常量因为我们不压缩，但当你接入压缩时，这就是旋钮。小 = 压缩更激进（= 更多 LLM 调用 = 更贵）= 实效历史更长。大 = 接近 verbatim = 接近 s05 baseline。

**想读更多**：upstream-readings 摘录在 [`upstream-readings/s05-action-history.py`](../../upstream-readings/s05-action-history.py)——和 Go 代码并排打开。然后预览 s06：episodic history 在轮间传递良好，但 `Workspace` 才是让 agent 的动作真正*落到*磁盘的东西。

---

**下一节预告**：s06 引入 `Workspace` 接口——沙箱化文件存储抽象，`LocalWorkspace` 通过 `filepath.Clean` + 前缀检查强制 root 限制。第一个真正非平凡的副作用工具（`read_file`、`write_file`）那时才登场。s06 之后 agent 终于能写盘；s05 只是先确保它能记住自己干了什么。
