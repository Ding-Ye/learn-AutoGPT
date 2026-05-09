---
title: "s02 · 显式命令注册表"
chapter: 2
slug: s02-command-registry
est_read_min: 10
---

# s02 · 显式命令注册表

> 教什么：把 s01 里硬编码的 `[]Tool{NewEchoTool()}` 升级成显式的 `Registry`。机制名是 **command_registry**——上游 AutoGPT 用 Python `@command` 装饰器自动注册，Go 没装饰器，所以我们写一个所有文件都能 `Register(myTool)` 的注册中心。

---

## Problem / 问题

s01 把 tool 列表写成 `[]Tool{NewEchoTool()}`——一个字面量切片。能跑，但有三个问题立刻浮出水面：(1) 加 tool 要改 `main.go`，多文件协作时谁拥有这一行？(2) Loop 每次 `Run` 都要把 `[]Tool` 重新折成 `map[string]Tool`，N 个 tool N 次重复工作。(3) 任何人想知道"agent 现在有哪些 tool" 只能 grep slice 字面量。

AutoGPT classic 在 `classic/forge/forge/command/decorator.py` 里用 `@command(...)` 装饰一个函数，import 那个 module 就自动注册。优雅，但是 **隐式**——dependency 藏在 import 顺序里，去掉一行 import tool 就消失了。Go 没有装饰器，即便有，也不应该这么做：dossier 反模式 #4 明确说"decorator-based registration is implicit; Go repo prefers explicit so deps are visible"。这一节就要解决这个：**让 tool 注册成为可 grep 的源代码事实，而不是 import 副作用**。

## Solution / 解决方案

引入 `Registry` 类型——一个 `name → Tool` 的索引，带三个方法：`Register(t Tool) error`、`Lookup(name) (Tool, bool)`、`All() []ToolSchema`。任何想暴露 tool 的包都得写一行 `reg.Register(...)`，这一行就是依赖关系的源代码证据。

关键决策点：

1. **Lookup 返回 `(Tool, bool)`，不是 `(Tool, error)`** —— "找不到"是常态条件，由 caller 决定如何处理（Loop 会把 miss 翻译成给模型的 "unknown tool" tool_result，让它自我修正）。Lookup 自身没失败。把 missing 包装成 error 会让 caller 误以为出了 IO/parse 这种"真"错。
2. **`All()` 必须按插入顺序返回** —— Go 的 map 迭代顺序随机；prompt 里展示给模型的 tool 列表如果每次抖动，模型 cache 命中率会掉，golden test 也没法 pin。一个并行的 `[]string{names}` 切片解决这个问题，多花一个字段换确定性。
3. **重名报错，不静默覆盖** —— 静默覆盖是最难调的 bug 类型：你以为改的是 A，运行的还是 B。Registry 拒绝重名时返回 `fmt.Errorf("tool %q already registered", name)`，让冲突在启动时立刻可见。

附带一个教学决定：除了 `EchoTool`，再加一个 `MathTool`（add/sub/mul/div）。一个 tool 时"按名字查找" 和"始终返回唯一那个" 在外部观察上不可区分；两个 tool 才让 Registry 的存在变得有可观测价值。

## How It Works / 工作原理

```ascii-anim frames=2
┌────────────────────────────────────────────────────────────────┐
│   main.go                                                      │
│      reg := NewRegistry()                                      │
│      reg.Register(NewEchoTool())   ──┐  显式依赖边             │
│      reg.Register(NewMathTool())   ──┤  grep 一眼可见           │
│      loop := &Loop{Tools: reg, …}  ──┘                         │
│                          │                                     │
│                          ▼                                     │
│   Loop.Run(prompt):                                            │
│      schemas := reg.All()           ── 插入顺序                │
│      send to Provider as Tools field                          │
│                                                                │
│   Provider returns tool_use{Name:"math", …}                    │
│      ↓                                                         │
│   Loop.runTools:                                               │
│      tool, ok := reg.Lookup(block.Name)                        │
│      ├── ok=true  → tool.Execute(input) → tool_result          │
│      └── ok=false → "unknown tool: ?" tool_result              │
│                     (模型自我修正，不崩)                        │
└────────────────────────────────────────────────────────────────┘
```

核心代码（节选自 [`agents/s02-command-registry/registry.go`](https://github.com/Ding-Ye/learn-AutoGPT/blob/main/agents/s02-command-registry/registry.go)）：

```go
type Registry struct {
    commands map[string]Tool
    names    []string // 平行切片：保留插入顺序
}

func NewRegistry() *Registry {
    return &Registry{commands: map[string]Tool{}}
}

func (r *Registry) Register(t Tool) error {
    name := t.Schema().Name
    if name == "" {
        return fmt.Errorf("registry: tool has empty name (Schema().Name must be set)")
    }
    if _, exists := r.commands[name]; exists {
        return fmt.Errorf("registry: tool %q already registered", name)
    }
    r.commands[name] = t
    r.names = append(r.names, name)
    return nil
}

func (r *Registry) Lookup(name string) (Tool, bool) {
    t, ok := r.commands[name]
    return t, ok
}

func (r *Registry) All() []ToolSchema {
    out := make([]ToolSchema, 0, len(r.names))
    for _, name := range r.names {
        out = append(out, r.commands[name].Schema())
    }
    return out
}
```

Loop 里的 dispatch 也变了——s01 在 `Run` 入口现搭一个 map，s02 直接调 `Lookup`：

```go
// loop.go (s02)
tool, ok := l.Tools.Lookup(block.Name)
if !ok {
    results = append(results, ContentBlock{
        Type:        "tool_result",
        ToolUseID:   block.ID,
        ToolContent: fmt.Sprintf("unknown tool: %q", block.Name),
    })
    continue
}
out, err := tool.Execute(ctx, block.Input)
```

**3 个非显然之处**：

1. **`names []string` 这个平行切片不是冗余** —— 单看 `commands` map 已经能存所有 tool，但 Go map 迭代顺序随机。我们需要"先 register 的先列出"，这是给模型看的 prompt 要稳定的硬要求；让 prompt cache 命中、让 golden test 能 pin、让 doc viewer 渲染顺序与代码一致。多一个字段换确定性。
2. **Lookup miss 不抛错，让模型自我修正** —— 模型偶尔会幻觉一个不存在的 tool 名字（特别是开源小模型）。如果 Lookup miss 就 panic，loop 死掉，这次 run 也死掉。我们把 miss 翻译成 `tool_result: "unknown tool: %q"` 喂回 user 消息，模型下一轮会看见自己的错并选一个真存在的 tool。这是 s01 就建立的"loop 不死，让模型恢复"原则在 Registry 层的延续。
3. **`Schema().Name == ""` 也要拒绝** —— 不显眼的 corner case：开发者写新 Tool 时忘了填 `Name` 字段，Schema 仍然返回零值。如果不查，零字符串就能注册，第二次再注册也还是零字符串——`already registered` 触发，但报错信息变成 `tool "" already registered`，看起来像 Bug 实际是 typo。提前在 Register 拒绝空名字，错误消息直击根因。

## What Changed / 与 s01 的变化

```diff
 type Loop struct {
     Provider Provider
-    Tools    []Tool       // s01: 字面量切片，每次 Run 现搭 map
+    Tools    *Registry    // s02: 显式注册中心，Register 时建索引
     MaxTurns int
     Verbose  bool
 }

 func (l *Loop) Run(ctx context.Context, userPrompt string) (string, error) {
-    toolByName := map[string]Tool{}
-    schemas := make([]ToolSchema, 0, len(l.Tools))
-    for _, t := range l.Tools {
-        s := t.Schema()
-        toolByName[s.Name] = t
-        schemas = append(schemas, s)
-    }
+    schemas := l.Tools.All()
     // ... 其余 think→act→observe 主体不变 ...
 }

 // runTools 的 dispatch 路径改名，但语义一样：
-tool, ok := byName[block.Name]
+tool, ok := l.Tools.Lookup(block.Name)
```

`main.go` 也从 `tools := []Tool{NewEchoTool()}` 变成显式三行：

```go
reg := NewRegistry()
if err := reg.Register(NewEchoTool()); err != nil { log.Fatalf("%v", err) }
if err := reg.Register(NewMathTool()); err != nil { log.Fatalf("%v", err) }
```

**语义层面**：tool 列表从"字面量"升级为"运行时构建的索引"，但因为是在 main 启动早期一次性 Register、之后只读，并发安全、热加载这种麻烦事 s02 还不沾——s08（components）会让构建过程更复杂，那时再讲。

## Try It / 动手试一试

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s02-command-registry

# demo 1：让模型用 math 工具算加法（观察 Registry dispatch）
go run . -v "use the math tool to add 7 and 35"

# demo 2：让模型用 echo 工具——同一个 Registry，不同 name 路由
go run . -v "echo back the word 'hello'"

# demo 3：切到 OpenAI-compat backend（DeepSeek），看 Registry 的 schema 列表如何被翻译成 OpenAI tools 格式
export DEEPSEEK_API_KEY=...
go run . -provider deepseek -v "use math to multiply 6 and 7"

# 跑全部测试（应该 36 通过）
go test -v ./...
```

期望输出形态：

```
[s02-command-registry] provider=anthropic model=claude-sonnet-4-6 url= tools=2
[turn 0] assistant: I'll use the math tool to add these.
[turn 0] -> math map[a:7 b:35 operation:add]
[turn 0] <- 42
[turn 1] assistant: 7 + 35 = 42.
7 + 35 = 42.
```

如果你看到 `unknown tool: "calculator"`——说明模型幻觉了一个 Registry 里没有的名字；下一轮它会自我修正。这正是"miss 不崩，让模型恢复"的设计目的。

## Upstream Source Reading / 上游源码阅读

AutoGPT classic 在 `classic/forge/forge/command/` 下有三个文件分管 Command 抽象：`command.py`（`Command` 类）、`decorator.py`（`@command` 装饰器）、`parameter.py`（`CommandParameter`）。下面是 decorator + Command 的核心，我们对照着看上游怎么把 Python 函数自动注册成 agent 可调用的命令。

```upstream:classic/forge/forge/command/decorator.py
# Source: classic/forge/forge/command/decorator.py
# 简化掉 generic typing；保留装饰器的执行链路。

import re
from typing import Callable, Optional

from forge.agent.protocols import CommandProvider
from forge.models.json_schema import JSONSchema

from .command import Command, CommandParameter


def command(
    names: list[str] = [],
    description: Optional[str] = None,
    parameters: dict[str, JSONSchema] = {},
):
    """
    把函数转成 Command。
    - names 为空 → 用函数名
    - description 为空 → 取 docstring 的第一段（双换行前）
    - parameters → 包装成 CommandParameter 列表
    """
    def decorator(func):
        doc = func.__doc__ or ""
        command_names = names or [func.__name__]
        if not (command_description := description):
            if not func.__doc__:
                raise ValueError("Description is required if function has no docstring")
            command_description = re.sub(r"\s+", " ", doc.split("\n\n")[0].strip())

        typed_parameters = [
            CommandParameter(name=param_name, spec=spec)
            for param_name, spec in parameters.items()
        ]

        # 关键：把 func 包成 Command 实例。返回值替换掉原函数本身，
        # 所以 import 这个 module 时，模块顶层的 my_func 就变成
        # 一个 Command 对象，CommandProvider 协议拿到它后注入到 agent.commands。
        return Command(
            names=command_names,
            description=command_description,
            method=func,
            parameters=typed_parameters,
        )

    return decorator
```

```upstream:classic/forge/forge/command/command.py
# Source: classic/forge/forge/command/command.py
# 简化：去掉 ParamSpec/Generic 泛型语法。

class Command:
    """A class representing a command.

    Attributes:
        names (list[str]): aliases for the command (first one is canonical)
        description (str): brief description shown to the LLM
        method (Callable): the actual handler function
        parameters (list[CommandParameter]): parameter schema
    """

    def __init__(self, names, description, method, parameters):
        # 校验：装饰器声明的 parameters 名字必须和函数签名匹配。
        # 这是 Python 唯一能"在 import 时静态检查"的点——签名不一致直接 ValueError。
        if not self._parameters_match(method, parameters):
            raise ValueError(
                f"Command {names[0]} has different parameters than provided schema"
            )
        self.names = names
        self.description = description
        self.method = method
        self.parameters = parameters

    def __call__(self, *args, **kwargs):
        return self.method(*args, **kwargs)

    def __get__(self, instance, owner):
        # descriptor 协议——挂在 class 上的 Command 在实例属性访问时
        # 自动绑定 self。这就是为什么 @command 修饰的方法能像普通方法那样调用。
        if instance is None:
            return self
        return Command(
            self.names, self.description,
            self.method.__get__(instance, owner),
            self.parameters,
        )
```

**对照阅读要点**：

- **隐式注册 vs 显式 Register**：上游靠"import module → 装饰器执行 → 函数被替换成 Command 实例 → CommandProvider 协议在 agent 启动时收集所有 Command"。整条链没有一行"显式注册"代码，依赖关系藏在 import graph 里。我们 Go 版每次新增 tool 都要在 `main.go`（或后续 s08 的 component 构造函数里）写一行 `reg.Register(...)`——多打字，但 grep 友好。
- **多个名字 (aliases) vs 单一 name**：上游 `Command.names` 是 `list[str]`（一个命令多个别名）。我们简化成单一 Schema().Name；多别名留作 Appendix B 扩展练习。
- **参数 schema 校验时机**：上游 `_parameters_match` 在 `Command.__init__` 时检查"装饰器声明的参数名"和"函数签名"是否一致——import 时即报错。Go 没有这种 runtime 反射的便利（也不该有：interface 的 Schema() 方法签名固定），我们靠 Tool.Execute 内部的 `requireString/requireNumber` 把 type 错误推到 LLM 调用之后，让模型从 tool error 里恢复。失败时机不同，但失败一定可见。
- **`__get__` descriptor 协议没有对应物**：上游用 Python descriptor 协议让 `@command` 装饰的方法既能从 class 调用也能从 instance 调用——本质上是为了支持 `CommandProvider` 这种"类即注册表"的模式。我们 Go 版直接在 main 里 `reg.Register(NewEchoTool())`，没有"class-as-registry"的概念，所以 descriptor 协议不需要对应。
- **故意没做的 disabled_commands**：AutoGPT 上游在 agent 启动时会跑 `_remove_disabled_commands`（基于 `AppConfig.disabled_commands` 列表）剥掉某些 command。我们暂时不处理——s07（permissions）会用更通用的 `Allow/Deny pattern` 系统覆盖这个能力。

**想读更多**：从 `classic/forge/forge/command/decorator.py::command` 入手，跟着 `Command.__init__` 进 `command.py`，再读 `forge/agent/protocols.py::CommandProvider`（s08 会重点讲）看它怎么从 component 收集 commands 注入到 agent。这条线就是 s02 → s08（components）→ s07（permissions 用 name 来匹配 allow/deny pattern）的真实代码地图。

---

**下一节预告**：s03 把 `Provider` 接口的多后端故事补完——s01 已经写好了 Anthropic 和 OpenAI-compat 的实现，但 main.go 切换 provider 的逻辑、profile 表、httptest 测试矩阵、以及 mock provider 在多个 session 间复用的策略都集中在 s03 讲清楚。
