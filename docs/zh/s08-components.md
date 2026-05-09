---
title: "s08 · 可插拔 Component 系统"
chapter: 8
slug: s08-components
est_read_min: 14
---

# s08 · 可插拔 Component 系统

> 教什么：把 s07 那个有 7 个字段的 Loop 重构成 component 化结构。引入 `Component` 标记接口（空）和三个可选子接口：`CommandProvider` / `DirectiveProvider` / `MessageProvider`。`ComponentBus` 用类型断言聚合所有 component 的能力，Loop 只接 `*ComponentBus` 一个对象就拿到 Registry + 系统 prompt directive 列表 + 预注入消息。两个示例 component：`FileManagerComponent`（包 Workspace、吐 read/write 工具 + 2 条 directive）和 `WebFetchComponent`（吐 web_fetch、可配超时）。`PromptStrategy.BuildPrompt` 签名扩了 `directives []string` 参数。

---

## Problem / 问题

到 s07 末尾，`Loop` 长这样：

```go
type Loop struct {
    Provider    Provider
    Tools       *Registry
    Strategy    PromptStrategy
    History     *History
    Permissions *Permissions
    Asker       Asker
    MaxTurns    int
    Verbose     bool
}
```

七个字段（不算 MaxTurns/Verbose）。每个都是 agent 的一项「能力」：调 LLM、查工具、构造 prompt、记历史、过权限、问用户。再加一项能力（比如 web 抓取、向量搜索、记忆系统）就要加新字段；构造 Loop 的 main.go 会越来越长。

更糟的是：**能力之间有共享状态**。`ReadFileTool` 和 `WriteFileTool` 都需要同一个 `Workspace`。Loop 的字段里没有 `Workspace`——它藏在 `tools_file.go::ReadFileTool.ws` 里，由 main.go 在 `reg.Register(NewReadFileTool(ws))` 时注入。这种「能力捆绑」的概念散落在好几处。

AutoGPT 在 `forge/agent/protocols.py` 给的答案是 **component**：

```python
class CommandProvider(AgentComponent, ABC):
    @abstractmethod
    def get_commands(self) -> Iterator[Command]: ...

class DirectiveProvider(AgentComponent, ABC):
    @abstractmethod
    def get_constraints(self) -> Iterator[str]: ...

class MessageProvider(AgentComponent, ABC):
    @abstractmethod
    def get_messages(self) -> AsyncIterator[ChatMessage]: ...
```

每个 component 是一个「能力捆绑」，可以选实现这三个 ABC 中的若干个。`Agent` 持有一个 `[]Component`，遍历时用 `isinstance` 检测每个 component 实现了哪些协议，把对应方法的产物聚合起来：所有 `get_commands()` 的并集 = registry，所有 `get_constraints()` 的并集 = system prompt 的 directive 列表，等等。

s08 把这套搬到 Go：

1. `type Component interface{}`——空标记接口
2. 三个可选子接口（`CommandProvider`/`DirectiveProvider`/`MessageProvider`）
3. `ComponentBus` 用类型断言聚合
4. Loop 字段从 7 个回收到 ~5 个（Provider + Bus + Strategy + History + Permissions + Asker），其中 Bus 替换掉 Tools

## Solution / 解决方案

```go
// 标记接口：空。任何值都可以是 Component。
type Component interface{}

// 三个可选子协议：
type CommandProvider   interface { Commands() []Tool }
type DirectiveProvider interface { Directives() []string }
type MessageProvider   interface { Messages() []Message }

// 聚合器：
type ComponentBus struct {
    components []Component
}
func NewComponentBus(components ...Component) *ComponentBus
func (b *ComponentBus) Registry() *Registry      // 用类型断言找所有 CommandProvider
func (b *ComponentBus) Directives() []string     // 顺序聚合所有 DirectiveProvider
func (b *ComponentBus) Messages() []Message      // 顺序聚合所有 MessageProvider

// Loop 改造：
type Loop struct {
    Provider    Provider
    Components  *ComponentBus  // 替换 Tools *Registry
    Strategy    PromptStrategy
    // ...
}
```

两个示例 component：

```go
// 实现两个协议：CommandProvider + DirectiveProvider。包一个 Workspace。
type FileManagerComponent struct {
    ws Workspace
}
func (f *FileManagerComponent) Commands() []Tool {
    return []Tool{NewReadFileTool(f.ws), NewWriteFileTool(f.ws)}
}
func (f *FileManagerComponent) Directives() []string {
    return []string{
        "Always read a file before editing it.",
        "Use list_files to discover before reading.",
    }
}

// 只实现一个协议：CommandProvider。一个 web_fetch 工具，超时可配。
type WebFetchComponent struct {
    httpTimeout time.Duration
}
func (w *WebFetchComponent) Commands() []Tool {
    return []Tool{newWebFetchTool(w.httpTimeout)}
}
```

PromptStrategy 接口改了一处：`BuildPrompt` 加了 `directives []string`：

```go
type PromptStrategy interface {
    BuildPrompt(history []*Episode, tools []ToolSchema, directives []string, task string) []Message
    ParseResponse(content []ContentBlock) (ActionProposal, error)
}
```

`OneShotStrategy.BuildSystem(tools, directives)` 把 directives 渲染到 system prompt 的 `## Directives` 段，紧跟在 `## Best practices` 之后。

`main.go` 不再调 `reg.Register(...)`：

```go
components := []Component{
    NewFileManagerComponent(ws),
    NewWebFetchComponent(30 * time.Second),
}
bus := NewComponentBus(components...)
loop := &Loop{Provider: p, Components: bus, /* ... */}
```

要加一个新能力？写一个新 struct，实现至少一个子协议，append 到 components 切片。Loop 不动，Strategy 不动。

## How It Works / 工作原理

```ascii-anim frames=4
┌────────────────────────────────────────────────────────────────────────┐
│ STARTUP                                                                  │
│                                                                         │
│   main.go: components := []Component{                                  │
│              NewFileManagerComponent(ws),                              │
│              NewWebFetchComponent(30s),                                │
│            }                                                            │
│            bus := NewComponentBus(components...)                       │
│            loop := &Loop{Provider, Components: bus, ...}               │
│        │                                                                │
│        ▼                                                                │
│ LOOP.RUN                                                                │
│                                                                         │
│   l.Components.Registry() ─── 类型断言扫描所有 c                       │
│        │                       cp, ok := c.(CommandProvider)            │
│        │                       if ok { for tool := range cp.Commands(){ │
│        │                                  reg.Register(tool) }}        │
│        │                                                                │
│        ▼                                                                │
│   l.Components.Directives() ─── 同样扫描，顺序拼接                      │
│        │   FileManager → ["Always read...", "Use list_files..."]       │
│        │   WebFetch    → []                                             │
│        │   合并         → ["Always read...", "Use list_files..."]      │
│        │                                                                │
│        ▼                                                                │
│   strategy.BuildPrompt(history, schemas, directives, task)              │
│   strategy.BuildSystem(schemas, directives)                             │
│        │   system prompt 包含 "## Commands" + "## Best practices"      │
│        │   + "## Directives"（来自 component）                          │
│        │                                                                │
│        ▼                                                                │
│   provider.CreateMessage{System: ..., Tools: schemas, Messages: ...}    │
└────────────────────────────────────────────────────────────────────────┘
```

### `ComponentBus.Registry()` —— 类型断言 + 注册

```go
func (b *ComponentBus) Registry() *Registry {
    reg := NewRegistry()
    for _, c := range b.components {
        cp, ok := c.(CommandProvider)
        if !ok { continue }
        for _, tool := range cp.Commands() {
            if err := reg.Register(tool); err != nil {
                panic("ComponentBus.Registry: " + err.Error())
            }
        }
    }
    return reg
}
```

为什么 panic 而不是 return error？因为 `Registry()` 在 Loop 启动时调用一次；component 名字撞车是开发者在 `NewComponentBus(...)` 时拼错了，不是模型在运行时能恢复的情况。Panic 让 stack trace 直指错误的 component 注册调用。

### `ComponentBus.Directives()` —— 顺序聚合

```go
func (b *ComponentBus) Directives() []string {
    out := make([]string, 0)
    for _, c := range b.components {
        dp, ok := c.(DirectiveProvider)
        if !ok { continue }
        out = append(out, dp.Directives()...)
    }
    return out
}
```

返回 `make([]string, 0)`（不是 nil）——保证 strategy 拿到的总是非 nil 切片，渲染逻辑里不需要 nil-check。

### `OneShotStrategy.BuildSystem` —— directive 段

```go
func (s *OneShotStrategy) BuildSystem(tools []ToolSchema, directives []string) string {
    var b strings.Builder
    b.WriteString("You are a methodical autonomous agent. ...")
    // ## Commands
    // ## Best practices
    if len(directives) > 0 {
        b.WriteString("\n## Directives\n")
        b.WriteString("These are component-supplied policies you must follow:\n")
        for i, d := range directives {
            fmt.Fprintf(&b, "%d. %s\n", i+1, d)
        }
    }
    return strings.TrimRight(b.String(), "\n")
}
```

为什么 directive 在 best practices **之后**？因为 best practices 是 strategy 自己的静态指令（每个 OneShot 都一样），directive 是 component 提供的、随构造而变的。**最近的指令优先**——把 component-specific 的放最后，模型最容易记住。

### 三个非显然之处

1. **空标记接口比 ABC 继承更灵活**

   Python 用 `class FileManagerComponent(DirectiveProvider, CommandProvider)`——继承表达「实现哪些协议」。Go 用结构化类型：

   ```go
   type FileManagerComponent struct{ ws Workspace }
   func (f *FileManagerComponent) Commands() []Tool { ... }
   func (f *FileManagerComponent) Directives() []string { ... }
   ```

   这两个 method 让 `*FileManagerComponent` 自动满足 `CommandProvider` 和 `DirectiveProvider`——**没有显式的「我继承了哪个 ABC」声明**。第三方包写一个有 `Commands()` 方法的 struct，扔进 `NewComponentBus(...)` 就能用，不需要 `forge.register_component` 之类的调用。

   这就是 s08 的架构关键：component 之所以可插拔，是因为它**自描述**。

2. **MessageProvider 只用于第一轮，避免重复**

   `Loop.Run` 在 `turn == 0 && len(*l.History) == 0` 时把 `bus.Messages()` 前置到当前轮的 messages。之后所有轮次都靠 `History.RenderMessages()` 重建对话——如果每轮都前置 component messages，模型就会看到 N 份重复的 "preamble"。

   这是 component 与 history 的边界：`MessageProvider` 提供的是**会话起点**的固定信息（如 pinned 提醒、当前任务说明），不是每轮要重新注入的状态。

3. **PromptStrategy.BuildPrompt 签名变了——这是 s07 → s08 的破坏性变更**

   ```diff
   - BuildPrompt(history []*Episode, tools []ToolSchema, task string) []Message
   + BuildPrompt(history []*Episode, tools []ToolSchema, directives []string, task string) []Message
   ```

   为什么不在 Loop 里包一个 wrapper 把 directive 拼到 system message 上？因为 system message 是**由 strategy 自己构造**（在 `BuildSystem` 里）——绕过 strategy 直接拼是干扰职责边界。让 strategy 自己看到 directive，意味着它能选择**怎么**集成：OneShot 渲染成 `## Directives` 段；一个假想的 directive-as-user-prefix 策略可以拼到当前轮的 user message 前面。

   这是 strategy 的扩展性——把直接注入改成「告诉 strategy 有这些 directive」，让 strategy 选择渲染方式。

## What Changed / 与 s07 的变化

```diff
 agents/s08-components/
 ├── provider.go                      # 与 s07 一字不差
 ├── provider_openai.go               # 与 s07 一字不差
 ├── provider_mock.go                 # 与 s07 一字不差
 ├── provider_*_test.go               # 与 s07 一字不差
 ├── tools.go / tools_test.go         # 与 s07 一字不差
 ├── tools_file.go / _test.go         # 与 s07 一字不差
 ├── workspace.go / _test.go          # 与 s07 一字不差
 ├── registry.go / _test.go           # 与 s07 一字不差（test 适配 ComponentBus）
 ├── history.go / _test.go            # 与 s07 一字不差
 ├── permissions.go / _test.go        # 与 s07 一字不差
 ├── strategy.go                      # 改：BuildPrompt 加 directives 参数；BuildSystem 渲染 directive
 ├── strategy_test.go                 # s07 测试 + 1 个 directive 渲染测试
+├── component.go                     # 新：Component 标记 + 3 个子协议 + ComponentBus
+├── component_test.go                # 新：5 个测试
+├── component_filemgr.go             # 新：FileManagerComponent
+├── component_filemgr_test.go        # 新：2 个测试
+├── component_web.go                 # 新：WebFetchComponent + web_fetch 工具
+├── component_web_test.go            # 新：2 个测试（含 httptest）
 ├── loop.go                          # 改：Tools *Registry → Components *ComponentBus
 ├── loop_test.go                     # 改：s07 测试 + 2 个 directive-flow 测试
 └── main.go                          # 改：构造 []Component 并传给 ComponentBus
```

类型 catalog 新增：

```go
type Component interface{}
type CommandProvider   interface { Commands() []Tool }
type DirectiveProvider interface { Directives() []string }
type MessageProvider   interface { Messages() []Message }
type ComponentBus struct{ components []Component }

type FileManagerComponent struct{ ws Workspace }
type WebFetchComponent struct{ httpTimeout time.Duration }
```

`Loop` 的字段净变化：`Tools *Registry` 删除，`Components *ComponentBus` 新增。

`PromptStrategy.BuildPrompt` 签名变了——这是 s04 以来第一次破坏性变更，doc 在 strategy.go 文件头说明。所有用 PromptStrategy 的代码（s08 的 Loop）跟着改一行。

## Try It / 动手试一试

```bash
cd agents/s08-components

# 1. 默认配置：FileManager + WebFetch，30s web 超时
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "fetch https://example.com and write a one-line summary to notes.md"

# 2. 调短 web 超时（用于测试网络敏感场景）
go run . -v -web-timeout 5s "fetch https://example.com"

# 3. 走多 provider，s03–s07 同样的 8 profile
export DEEPSEEK_API_KEY=sk-...
go run . -provider deepseek -v "fetch https://example.com"

# 4. 跑全部测试
go test -v ./...
```

期望输出（详情模式）：

```
[s08-components] provider=anthropic model=claude-sonnet-4-6 ... components=2 tools=3 directives=2 workspace=./workspace permissions=defaults ask=deny web-timeout=30s
[turn 0] assistant: I'll fetch the URL and then summarize the body.
[turn 0] proposal: cmd=web_fetch thoughts="..."
[turn 0] permission check: web_fetch → Allow
[turn 0] -> web_fetch map[url:https://example.com]
[turn 0] <- <!doctype html>...
[turn 1] permission check: write_file → Allow
[turn 1] -> write_file map[content:Example.com is a documentation domain. path:notes.md]
[turn 1] <- wrote 36 bytes to notes.md
[turn 2] assistant: Done. Summary written to notes.md.
Done. Summary written to notes.md.
```

### 加一个自定义 component

把这个文件丢到 `agents/s08-components/component_clock.go`：

```go
package main

import "time"

type ClockComponent struct{}

func (c *ClockComponent) Commands() []Tool {
    return []Tool{&clockTool{}}
}

type clockTool struct{}

func (c *clockTool) Schema() ToolSchema {
    return ToolSchema{
        Name: "now",
        Description: "Return the current time as RFC3339.",
        InputSchema: map[string]interface{}{"type": "object"},
    }
}

func (c *clockTool) Execute(_ context.Context, _ map[string]interface{}) (string, error) {
    return time.Now().Format(time.RFC3339), nil
}
```

把 `&ClockComponent{}` 加到 main.go 的 components 切片，重新跑——agent 立即多了 `now` 工具。**Loop 不动，Strategy 不动**。

## Upstream Source Reading / 上游源码阅读

AutoGPT classic 的 component 系统在 `classic/forge/forge/agent/protocols.py`（三个 ABC）和 `classic/forge/forge/components/` 子目录（具体 component 示例）。完整双语注解版见 [`upstream-readings/s08-components.py`](../../upstream-readings/s08-components.py)；本节只摘要。

```upstream:classic/forge/forge/agent/protocols.py
class AgentComponent(ABC):
    """Base class for every component."""
    enabled: bool = True

class CommandProvider(AgentComponent, ABC):
    @abstractmethod
    def get_commands(self) -> Iterator[Command]: ...

class DirectiveProvider(AgentComponent, ABC):
    @abstractmethod
    def get_constraints(self) -> Iterator[str]: ...
    @abstractmethod
    def get_resources(self) -> Iterator[str]: ...
    @abstractmethod
    def get_best_practices(self) -> Iterator[str]: ...

class MessageProvider(AgentComponent, ABC):
    @abstractmethod
    def get_messages(self) -> AsyncIterator[ChatMessage]: ...

# [→ s08：我们的 Go 版本：
#    - AgentComponent → 空 interface{} 标记
#    - CommandProvider → Commands() []Tool
#    - DirectiveProvider → Directives() []string（合并 3 个 bucket 成一个）
#    - MessageProvider → Messages() []Message]
```

```upstream:classic/forge/forge/components/file_manager/__init__.py
class FileManagerComponent(DirectiveProvider, CommandProvider):
    def __init__(self, file_storage):
        self.workspace = file_storage

    def get_constraints(self) -> Iterator[str]:
        yield "Always read a file before editing it."
        yield "Use list_folder to discover before reading."

    def get_commands(self) -> Iterator[Command]:
        yield self.read_file
        yield self.write_file
        # ...

# [→ s08：我们的 FileManagerComponent 同样实现 Commands + Directives，
#    包一个 Workspace，吐 read_file + write_file。]
```

### 对照阅读要点

- **空标记 vs ABC**：upstream 用 `AgentComponent(ABC)` 做基类，Go 版用 `Component interface{}`。结构化类型让 component 自带「我满足哪些协议」的信息——不需要 `class extends`。

- **三个 directive bucket → 一个**：upstream 把 directive 拆成 `constraints`/`resources`/`best_practices` 三组方法，让 prompt 模板可以分别渲染。Go 版合成一个 `Directives() []string`——OneShotStrategy 渲染到一个 `## Directives` 段。如果你需要更细粒度的分组，扩 `DirectiveProvider` 接口为返回 `map[string][]string` 即可。

- **isinstance vs type-assertion**：Python 的 `isinstance(c, CommandProvider)` ↔ Go 的 `c.(CommandProvider)`。同样的语义、同样的运行时检测。

- **MessageProvider 只用第一轮**：上游每轮都重新查一遍 messages（component 可以决定何时返回什么）；我们简化为「只在第一轮注入一次」，避免重复。如果需要每轮重新查（如「显示当前时间」），改 `Loop.Run` 让 `bus.Messages()` 每轮调用即可。

- **WebFetch 的简化**：上游的 web component（`forge/components/web/`）做了 HTML 渲染、链接抽取、BeautifulSoup 解析；我们只做 GET + 8KiB 截断。原因是 Go 标准库没有 BS4 等价物（`golang.org/x/net/html` 太底层），而做 token 友好的 HTML→text 转换需要约 200 行额外代码。把它留作 Appendix B 练习。

**深入阅读**：[`upstream-readings/s08-components.py`](../../upstream-readings/s08-components.py) 含完整 ABC 定义 + Go 翻译表 + 「为什么类型断言替代了 isinstance」的解释。然后预览 s09：到 s08 末尾，agent 已经能自主跑一轮 think→act→observe 但只跑一次；s09 引入 `RunInteractionLoop` + cycle budget + signal handling，把 agent 变成「持续运行模式」——可中断、可暂停、可恢复。

---

**下一节预告**：s09 引入 `LoopOpts{Cycles, AskEachStep, OnInterrupt}` + `RunInteractionLoop` 包装器 + 通过 `os/signal.Notify` 的 SIGINT 处理 + `UIProvider` 接口（spinner / RenderThought / RenderResult）。把 s01–s08 的「单次 Run」升级成「N 轮自主 + Ctrl-C 优雅退出」，对标 AutoGPT classic 的 `app/main.py:run_interaction_loop`。
