---
title: "s07 · 分层权限管理"
chapter: 7
slug: s07-permissions
est_read_min: 13
---

# s07 · 分层权限管理

> 教什么：在 s06 沙箱化 Workspace 之上，再加一道闸门——每次工具派发前用 `Permissions.Check(cmd, args)` 决定 Allow / Deny / Ask。引入 `*` / `**` glob 匹配 + `Asker` 接口（生产 stdin、测试 stub）。Loop 在 `strategy.Parse` 与 `tool.Execute` 之间插入这道闸门——这是「跨切关注点放在 loop 接缝处」反模式的标准答案。

---

## Problem / 问题

s06 教 agent 在 workspace 沙箱里收手——任何尝试写到 `/etc/passwd` 或 `../../escape` 的请求都被 `LocalWorkspace.resolve()` 当场拦下。这把外向越权的洞补好了。

**但 root *内部* 的破坏力还没拦住**：

```
agent 的 tool_use:
{
  "name": "bash",
  "input": {"command": "rm -rf ."}
}
// 假设 bash 被 sandboxed 到 ./workspace/，仍然能 wipe 整个 6-session 工作目录
```

或者更隐蔽的：

```
{"name": "write_file", "input": {"path": ".ssh/authorized_keys", "content": "..."}}
// 路径相对 workspace，sanitizer 不会拦——但实际上 agent 在动 SSH 设施
```

AutoGPT 上游对这个问题的回答在 `classic/forge/forge/permissions.py`：一个 `CommandPermissionManager` 持有 4 个层级的 allow/deny 列表（`ONCE` / `AGENT` / `WORKSPACE` / `DENY`），用 glob 模式匹配 `(command_name, args_str)`，决策树是「拒绝列表先查、放行列表后查、都没命中就 prompt 用户」。

s07 把这套搬进 Go，并做 2-level 简化：

1. `type Decision int (Allow / Deny / Ask)` —— 三态枚举
2. `Permissions{AllowList, DenyList []Pattern}` —— 一对列表
3. `Check(cmd, args) Decision` —— 决策树（Deny 先于 Allow，无规则命中返回 Ask）
4. `Asker` 接口 —— 决定 Ask 时的实际行为（stdin 询问 / 测试 stub）

完整的 4-level scope（`ONCE` 一次性允许、`AGENT` 持久到 agent 设置、`WORKSPACE` 持久到 workspace 设置、`DENY` 拒绝）被砍到 2-level —— **完整的 4-level 留作附录 B 练习 #5**。

## Solution / 解决方案

```go
type Decision int
const (
    Allow Decision = iota
    Deny
    Ask
)

type Pattern struct {
    Glob string  // e.g. "read_file: *.md", "*: secret*", "bash: rm -rf**"
}

type Permissions struct {
    AllowList []Pattern
    DenyList  []Pattern
}

func (p *Permissions) Check(cmd string, args map[string]interface{}) Decision {
    for _, pat := range p.DenyList {
        if patternMatches(pat.Glob, cmd, args) { return Deny }
    }
    for _, pat := range p.AllowList {
        if patternMatches(pat.Glob, cmd, args) { return Allow }
    }
    return Ask
}

type Asker interface {
    Ask(cmd string, args map[string]interface{}) Decision  // returns Allow|Deny
}

type StdinAsker struct { ... }  // production: prompt y/N
type StubAsker struct { ... }   // test: return canned reply
```

模式格式 `"<command>: <arg-glob>"`：

| 模式 | 含义 |
|---|---|
| `read_file: *.md` | 匹配 read_file 当 args 任一字符串值 glob 匹配 `*.md` |
| `write_file: *` | 匹配 write_file 任意参数（bare-* 含义是「无视 args」） |
| `*: secret*` | 匹配任何命令，当任一字符串参数以 "secret" 开头 |
| `bash: rm -rf**` | 匹配 bash 当任一参数 glob 匹配 `rm -rf**`（** 跨 `/`） |

Glob 匹配器规则：
- `*` 匹配一段路径（不跨 `/`）
- `**` 跨段匹配（任意数量字符，含 `/`）
- `?` 单字符通配

Loop 在每次 tool_use 派发前调用 `Check`：Allow 走原路径；Deny 合成一个 `ActionResult{Status:"denied", Output:"permission denied: <cmd>"}` 喂给下一轮的模型；Ask 则交给 `Asker`。

## How It Works / 工作原理

```ascii-anim frames=4
┌────────────────────────────────────────────────────────────────────────┐
│ TURN N                                                                  │
│                                                                         │
│   provider response: tool_use {name=write_file, input={path=".ssh/x"}}  │
│        │                                                                │
│        ▼                                                                │
│   strategy.ParseResponse → ActionProposal{Command:"write_file", ...}    │
│        │                                                                │
│        ▼                                                                │
│   Permissions.Check("write_file", args)                                 │
│   ├─ DenyList: "write_file: **/.ssh/**"                                │
│   │     patternMatches → cmd="write_file" ✓ , glob ".ssh/x" matches → MATCH
│   │     return Deny                                                    │
│   └─ AllowList not consulted (deny short-circuits)                     │
│        │                                                                │
│        ▼                                                                │
│   Loop synthesizes ActionResult{Status:"denied",                       │
│                                  Output:"permission denied: write_file"}│
│        │                                                                │
│        ▼                                                                │
│   Episode.Results gets the denial                                      │
│                                                                         │
│ TURN N+1                                                                │
│   strategy.BuildPrompt sees the prior episode → renders a tool_result   │
│   block with content '{"status":"denied", "output":"..."}'              │
│        │                                                                │
│        ▼                                                                │
│   model adapts: "Sorry, I'll skip the .ssh write."                      │
└────────────────────────────────────────────────────────────────────────┘
```

### `Check` —— Deny 先于 Allow，否则 Ask

```go
func (p *Permissions) Check(cmd string, args map[string]interface{}) Decision {
    for _, pat := range p.DenyList {     // 1. Deny 先查
        if patternMatches(pat.Glob, cmd, args) {
            return Deny                   //    第一个 deny 命中即短路
        }
    }
    for _, pat := range p.AllowList {    // 2. Allow 后查
        if patternMatches(pat.Glob, cmd, args) {
            return Allow
        }
    }
    return Ask                            // 3. 都没命中就询问
}
```

为什么「Deny 先于 Allow」？因为操作员希望写一条**宽 allow** + 几条**窄 deny** 来挖洞：

```yaml
allow:
  - "bash: **"               # 宽：bash 默认全开
deny:
  - "bash: rm -rf**"         # 窄：但 rm -rf 永远拒
  - "bash: dd**"             # 还有 dd
```

如果 Allow 先查，宽规则会先返回 Allow，窄 Deny 永远不会被评估——所以必须 Deny 先。这是 AutoGPT 上游 `check_command` 决策树的同样选择。

### `patternMatches` —— 命令名 + 任意字符串参数

```go
func patternMatches(pattern, cmd string, args map[string]interface{}) bool {
    idx := strings.Index(pattern, ":")
    cmdGlob := strings.TrimSpace(pattern[:idx])
    argGlob := strings.TrimSpace(pattern[idx+1:])

    // 命令名：* 是通配，否则严格相等
    if cmdGlob != "*" && cmdGlob != cmd { return false }

    // bare-* 或 bare-** 的 arg-glob 直接通过（命令级规则）
    if argGlob == "*" || argGlob == "**" { return true }

    // 否则：测试 EVERY 字符串值，任一命中即匹配
    for _, v := range args {
        if s, ok := v.(string); ok && globMatch(argGlob, s) {
            return true
        }
    }
    return false
}
```

「ANY string-arg matches」这条逻辑是故意松：

- 防止 agent 通过把 `path` 改名为 `target` 来绕规则
- 同一规则覆盖多种工具的多个字段（`*: secret*` 同时拦下 `read_file{path:"secret-x"}` 和 `web_fetch{key:"secret-y"}`）
- 数字、布尔、嵌套对象不参与匹配——故意只查字符串

### `globMatch` —— 自己实现，不用 `path/filepath.Match`

```go
// * 匹配单段（不跨 /），** 跨段，? 单字符
func globMatch(pattern, input string) bool {
    pi, ii := 0, 0
    starPi, starII := -1, -1
    doubleStar := false
    // ... 递归扫描，遇 * 标记回溯点，** 允许跨 / ...
}
```

为什么不用 `path/filepath.Match`？

1. 它不支持 `**`（跨段）—— upstream 用 `**` 是核心需求
2. 它的转义规则跨平台不一致（Windows vs POSIX）
3. 我们的输入根本不是路径——可能是 URL、shell 命令、查询字符串。`filepath.Match` 文档明确说自己是 *path* 匹配器

详细实现见 `permissions.go::globMatch` —— 大约 30 行带注释，最坏 O(len(pattern)*len(input))，agent 实际输入都很小。

### 三个非显然之处

1. **「无规则命中」返回 `Ask` 而不是 `Deny`**

   你可能直觉「无规则就拒」更安全，但那破坏了「Asker 是策略的实际持有者」的设计。`Permissions.Check` 只表达**规则的判定**；当规则没说时，*真正的策略*（要不要询问、要不要拒、要不要 fail-closed）由 `Asker` 持有。这样：

   - 测试可以塞 `StubAsker(Deny)` —— 等同于「无规则即拒」
   - 生产可以塞 `StdinAsker` —— 询问用户
   - 未来 s09 可以塞一个 RichUI Asker —— 弹 GUI 确认框

   `Permissions` 与 `Asker` 的边界是「事实 vs 策略」的分界。

2. **Loop 在 `strategy.Parse` 和 `tool.Execute` 之间插入闸门，不在 Tool.Execute 内部**

   反直觉的写法是把权限检查写进每个 Tool：

   ```go
   // 反例
   func (w *WriteFileTool) Execute(ctx, input) (string, error) {
       if !w.perms.Allow(input) { return "", errors.New("denied") }
       // ...
   }
   ```

   问题：

   - N 个工具就要写 N 次同样的检查
   - 任何一个工具忘了写就漏了
   - 测试每个工具时都要 mock Permissions
   - 跨切关注点（权限 / 日志 / 审计 / 限流）会污染每个 Tool 的核心逻辑

   正确做法：把闸门放在 Loop 里——**一个地方、一次检查、所有工具都过这道闸**。dossier 的反模式 #2 正好说这个。

3. **denied 的 ActionResult 必须让模型看到**

   `Loop.runTools` 拒绝执行后并不是 silent skip——它合成一个 `ActionResult{Status:"denied", ...}` 加到 Episode。下一轮 `RenderMessages` 把它渲染成 tool_result，模型于是看到「我刚才那个动作被拒了」并能调整策略。`history.go::renderResult` 的 default 分支正好处理 `denied` 状态（JSON 编码 status+output）。

   如果 silent skip，模型会以为工具运行成功了，下一步可能基于错误假设继续行动。让模型看到拒绝是**让 LLM 自己处理 fallback**的关键。

## What Changed / 与 s06 的变化

```diff
 agents/s07-permissions/
 ├── provider.go              # 与 s06 一字不差
 ├── provider_openai.go       # 与 s06 一字不差
 ├── provider_mock.go         # 与 s06 一字不差
 ├── provider_*_test.go       # 与 s06 一字不差
 ├── tools.go / tools_test.go # 与 s06 一字不差
 ├── tools_file.go / _test.go # 与 s06 一字不差
 ├── workspace.go / _test.go  # 与 s06 一字不差
 ├── registry.go / _test.go   # 与 s06 一字不差
 ├── strategy.go / _test.go   # 与 s06 一字不差
 ├── history.go / _test.go    # 与 s06 一字不差
+├── permissions.go           # 新：Decision、Pattern、Permissions、Check、globMatch、Asker
+├── permissions_test.go      # 新：8 个测试
 ├── loop.go                  # 改：新增 Permissions / Asker 字段；runTools 加权限闸门
 ├── loop_test.go             # 改：s06 测试 + 2 个权限闸门测试
 └── main.go                  # 改：从 ./permissions.json 加载（缺省走默认规则）+ -ask 标志
```

类型 catalog 新增：

```go
type Decision int
type Pattern    struct{ Glob string }
type Permissions struct{ AllowList, DenyList []Pattern }
type Asker interface { Ask(cmd, args) Decision }
type StdinAsker struct{ ... }
type StubAsker  struct{ ... }
```

`Loop` 的字段净增 2 个：`Permissions *Permissions`、`Asker Asker`。两个都允许 nil——nil Permissions 退回 s06 行为（无闸门），nil Asker 配合 nil Permissions 也安全；只有 Permissions 非 nil 但 Asker nil 且某次 `Check` 返回 Ask 时才会报错。这种「向后兼容的扩展」是 s06 → s07 这种增量教学最干净的形态。

## Try It / 动手试一试

```bash
cd agents/s07-permissions

# 1. 默认规则（read/write/echo/math 全允许，无 deny）
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "create notes.md with a one-line summary of the agent loop"

# 2. 启用样例 permissions 文件（见 testdata/permissions.yaml，JSON 格式）
cp testdata/permissions.yaml ./permissions.json
go run . -v -ask stdin "read README.md, then write a paraphrase to notes.md"
# 任何匹配规则之外的工具都会触发 stdin 询问 [y/N]

# 3. fail-closed 模式：未知工具直接拒
go run . -v "do something I haven't whitelisted"
# 默认 -ask=deny，所以未匹配的调用走 Asker→Deny→记录 denied

# 4. 跑全部测试
go test -v ./...
```

期望输出（默认规则，写文件成功的任务）：

```
[s07-permissions] provider=anthropic model=claude-sonnet-4-6 ... permissions=defaults ask=deny
[turn 0] assistant: I'll write the summary to notes.md.
[turn 0] proposal: cmd=write_file thoughts="..."
[turn 0] permission check: write_file → Allow
[turn 0] -> write_file map[content:"agent loop = think → act → observe" path:notes.md]
[turn 0] <- wrote 36 bytes to notes.md
[turn 1] assistant: Done.
Done.
```

启用 `testdata/permissions.yaml` + 让 agent 尝试越权写 SSH key 的输出：

```
[turn 0] permission check: write_file → Deny
[turn 0]    (matched DenyList rule "write_file: **/.ssh/**")
[turn 1] assistant: Sorry, that path is restricted. I'll write to a normal location instead.
```

### 给 agent 一个「危险」工具试试

把 `bash` 工具临时塞进 `main.go` 的 registry（不要 commit），然后运行：

```bash
go run . -v -ask stdin "list files in ./workspace using bash"
# Loop 在 bash 派发前打印：
# [turn 0] permission check: bash → Ask
# permission required: bash(map[command:ls -la ./workspace]) [y/N]:
```

回 `n` 即拒绝。这就是 `Asker` 接口的工厂价值：在 LLM agent 真正动手之前，*人*在 loop 里。

## Upstream Source Reading / 上游源码阅读

AutoGPT classic 的权限系统在 `classic/forge/forge/permissions.py`——一个 `CommandPermissionManager` 类带 4-level scope。完整双语注解版见 [`upstream-readings/s07-permissions.py`](../../upstream-readings/s07-permissions.py)；本节只摘核心。

```upstream:classic/forge/forge/permissions.py
# Source: classic/forge/forge/permissions.py
# 简化：去掉 Pydantic 设置、workspace 持久化、session_denied 集合，
# 保留 4-level Scope 枚举、check_command 决策树、_pattern_matches 匹配器。

class ApprovalScope(str, Enum):
    """Scope of permission approval."""
    ONCE = "once"          # 一次性允许（不持久）
    AGENT = "agent"        # 该 agent 永久允许（写 agent 设置）
    WORKSPACE = "workspace" # 该 workspace 永久允许（写 ws 设置）
    DENY = "deny"          # 拒绝

    # [→ s07：我们把 4 个 scope 合并成 3 态 Decision (Allow/Deny/Ask)。
    #    持久化层级（ONCE 临时 vs AGENT 文件 vs WORKSPACE 文件）的区别
    #    是 upstream 4-level 的本质——s07 砍到 2-level，把任何 approved
    #    pattern 都放进 AllowList，由 caller 决定要不要持久化（我们的
    #    `permissions.json` 读取器就是一种持久化）。完整 4-level 是
    #    附录 B 练习 #5。]
```

```upstream:classic/forge/forge/permissions.py
class CommandPermissionManager:
    """Manages layered permissions for agent command execution.

    Check order (first match wins):
    1. Agent deny list → block
    2. Workspace deny list → block
    3. Agent allow list → allow
    4. Workspace allow list → allow
    5. No match → prompt user
    """

    def check_command(self, command_name, arguments) -> PermissionCheckResult:
        args_str = self._format_args(command_name, arguments)

        # 1. Agent deny
        if self._matches_patterns(command_name, args_str,
                                   self.agent_permissions.permissions.deny):
            return PermissionCheckResult(False, ApprovalScope.DENY)
        # 2. Workspace deny
        # 3. Agent allow
        # 4. Workspace allow → 上面 4 步, deny 先 allow 后

        # 5. 都没命中：prompt 用户
        if self.prompt_fn is None:
            return PermissionCheckResult(False, ApprovalScope.DENY)
        scope, feedback = self.prompt_fn(command_name, args_str, arguments)
        ...

    # [→ s07：我们的 Permissions.Check 同样的决策树，但只有一对列表
    #    （AllowList / DenyList），不是 4 个。`prompt_fn` 对应我们的
    #    `Asker.Ask(cmd, args) Decision` —— 简化签名（无 feedback 字符串
    #    返回，无 scope 持久化），具体见 upstream-readings 文件。]
```

```upstream:classic/forge/forge/permissions.py
def _pattern_matches(self, pattern, cmd, args) -> bool:
    """Check if a single pattern matches the command."""
    # Parse pattern: command_name(args_pattern)
    match = re.match(r"^(\w+)\((.+)\)$", pattern)
    if not match: return False
    pattern_cmd, args_pattern = match.groups()
    if pattern_cmd != cmd: return False

    # Convert glob → regex:
    #   ** matches any path (including /)
    #   * matches any characters except /
    args_pattern = re.escape(args_pattern)
    args_pattern = args_pattern.replace(r"\*\*", ".*")
    args_pattern = args_pattern.replace(r"\*", "[^/]*")
    return bool(re.match(f"^{args_pattern}$", args))

# [→ s07：我们的 patternMatches + globMatch。差异：
#    1) 模式语法 "cmd(arg)" → "cmd: arg"（更易在 YAML/JSON 写）
#    2) 我们直接写 globMatch，不绕 regex
#       (规模小 + 教学清晰 + 跨平台稳定)
#    3) 我们支持 cmdGlob == "*" 表示「任何命令」；upstream \w+ 不允许]
```

### 对照阅读要点

- **4-level → 2-level 简化**：上游 `ONCE` / `AGENT` / `WORKSPACE` / `DENY` 区分的是**持久化层级**（一次性、agent 级、workspace 级），不是不同的判定逻辑。我们 Go 版只关心「这次允不允许」，把「下次怎么记住」推给 caller（写 `permissions.json` 即可）。

- **arg 渲染策略不同**：上游对每个工具有专门的 `_format_args` 分支——`read_file` 拿到的是 *resolved 绝对路径*，`execute_shell` 拿到的是 `executable:rest`。我们 Go 版采用更松的策略：对**所有字符串参数**逐一测试，任一命中即匹配。这让规则不能被「重命名字段」绕过，但失去了 upstream 的路径标准化（`{"path":"./notes.md"}` 不会匹配规则 `read_file: notes.md`）。如果你需要标准化，建议在 Workspace 层做（s06 的 `resolve` 早就标准化过路径），permissions 层只做名字匹配。

- **`{workspace}` 占位符没做**：上游允许 `read_file({workspace}/data/*)` 这种引用 workspace 根的写法。我们 Go 版没做——一是模型本来就只能emit 相对路径（被 s06 强制），二是这增加了 Permissions 与 Workspace 的耦合。如果你确实需要，加一个 `(p *Permissions) WithWorkspace(ws Workspace)` 方法，在 `patternMatches` 入口替换 `{workspace}` 即可。

- **Asker 接口 vs prompt_fn 回调**：函数式回调 `Callable[[str,str,dict], tuple[scope,feedback]]` 与接口 `Asker` 在 Python 里效果一样。Go 选 interface 是 idiomatic 的接口注入；测试里给 `StubAsker`，生产给 `StdinAsker`，s09 可以给 `RichUIAsker`——seam 一致。

- **session_denied 集合没做**：上游用 `set[str]` 记录「这个 session 内已经被拒过的 perm_string」，避免重复 prompt。我们没做——因为模型在拒绝后会**看到** denied 的 tool_result（s07 的关键设计），通常会自己停止重试。如果你跑一个尾巴特别 stubborn 的开源模型，加一个 `LRU<string, time.Time>` 在 `Loop` 字段上、`Check` 之前预查即可。

**深入阅读**：[`upstream-readings/s07-permissions.py`](../../upstream-readings/s07-permissions.py) 含完整 4-level 决策树注释 + Go 翻译表 + Asker 桥接说明。然后预览 s08：到 s07 末尾，`Loop` 已经持有 `Provider, Tools, Strategy, History, Workspace, Permissions, Asker` 7 个字段——s08 把这些"能力捆绑"重构成 `Component`，每个 component 通过类型断言暴露 `Commands()` / `Messages()` / `Directives()`，Loop 收集所有 component 自动构建 Registry 与 system prompt。

---

**下一节预告**：s08 引入 `Component` 标记接口 + 三个可选子接口（`CommandProvider`、`MessageProvider`、`DirectiveProvider`）。`ComponentBus` 用 type-switch 聚合多个 component 的能力，`Loop` 改成只接 `*ComponentBus` —— 字段从 7 个回收到 2 个（Provider + Bus）。两个示例 component：`FileManagerComponent`（包 Workspace + 吐出 read_file/write_file + 加 directive）和 `WebFetchComponent`（吐出 web_fetch 工具）。
