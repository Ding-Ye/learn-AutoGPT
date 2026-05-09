---
title: "s06 · 沙箱化 Workspace"
chapter: 6
slug: s06-workspace
est_read_min: 12
---

# s06 · 沙箱化 Workspace

> 教什么：给 agent 引入文件 IO，且只允许它在一个沙箱目录内动手脚。`Workspace` 接口（`Read` / `Write` / `List`）+ `LocalWorkspace` 实现，靠 `filepath.Clean` + 前缀检查阻止 `../` 越权、绝对路径与 null 字节注入。同时上线 *第一对真正非平凡的工具*——`read_file` 与 `write_file`——它们都把 `Workspace` 当构造参数传入，这是后续 s07 权限/s08 component 都会复用的范式。

---

## Problem / 问题

s05 之前我们的工具都是无副作用的纯函数：`echo` 把字符串吐回来，`math` 算两数加减乘除。这种 agent 顶多算一个有 LLM 包装的计算器——*它不能在世界里留下任何东西*。让 agent 写文件是它从「演示玩具」变成「能做点事的工具」的分水岭。

但是直接给它 `os.WriteFile` 是糟糕主意。一个普通 prompt 注入或一个 hallucinated 路径就足够：

```go
// agent 想写 "summary.md"，结果模型走神写成了：
os.WriteFile("/etc/passwd", "...", 0o644)
// 或者：
os.WriteFile("../../../home/user/.ssh/authorized_keys", "...", 0o600)
```

AutoGPT classic 上游对这个问题的解法在 `classic/forge/forge/file_storage/`：一个 `FileStorage` ABC 定义所有文件操作，一个 `LocalFileStorage` 实现绑定到一个 `root: Path` 和 `restrict_to_root: bool`，最关键的是 `_sanitize_path` 方法——一个会拒绝 null 字节、拒绝越权绝对路径、`Path.resolve()` 后再做 `is_relative_to(root)` 检查的小函数（详见 `base.py` 上的源码）。

s06 把这套搬进 Go：

1. `Workspace` 接口（三个方法，最小够用）
2. `LocalWorkspace` 实现 + `resolve()` 路径净化器
3. `read_file` / `write_file` 工具（两个新工具，构造时拿 `Workspace`）

后两者就是上面提到的「构造参数传依赖」范式：从今往后凡是需要外部资源的工具都走构造函数注入，而不是全局 var——s07 的 `Permissions` 也走同一条路。

## Solution / 解决方案

```go
type Workspace interface {
    Read(path string) (string, error)
    Write(path, content string) error
    List(prefix string) ([]string, error)
}

type LocalWorkspace struct {
    root string // 绝对路径 + filepath.Clean + 末尾分隔符
}

func NewLocalWorkspace(root string) (*LocalWorkspace, error) {
    abs, _ := filepath.Abs(root)
    abs = filepath.Clean(abs)
    os.MkdirAll(abs, 0o755)         // 不存在就建
    return &LocalWorkspace{root: abs + string(filepath.Separator)}, nil
}

// 路径净化器 —— 整个 s06 的承重梁
func (l *LocalWorkspace) resolve(p string) (string, error) {
    if p == "" { return "", fmt.Errorf("path is empty") }
    if strings.ContainsRune(p, 0) { return "", fmt.Errorf("path contains null byte") }
    if filepath.IsAbs(p) { return "", fmt.Errorf("absolute path not allowed: %q", p) }
    cleaned := filepath.Clean(filepath.Join(l.root, p))
    if !strings.HasPrefix(cleaned + string(filepath.Separator), l.root) &&
       cleaned + string(filepath.Separator) != l.root {
        return "", fmt.Errorf("path escapes root: %q -> %q", p, cleaned)
    }
    return cleaned, nil
}
```

接口三个方法、`LocalWorkspace` 一个 struct + 构造、一个 `resolve` 私有方法承担所有安全检查。

```go
type ReadFileTool  struct{ ws Workspace }
type WriteFileTool struct{ ws Workspace }

func NewReadFileTool(ws Workspace) *ReadFileTool   // 拿 interface，不拿 string
func NewWriteFileTool(ws Workspace) *WriteFileTool
```

工具拿 `Workspace` interface 作为构造参数。这样：

- 测试可以 mock（用 t.TempDir 起 LocalWorkspace 即可，无需 mock 框架）
- s07 的 permission-wrapper 可以包一层 decorator
- s08 的 FileManagerComponent 把同样这俩工具吐出来给 agent loop

## How It Works / 工作原理

```ascii-anim frames=3
┌──────────────────────────────────────────────────────────────────┐
│  agent 说："写 notes.md，内容是『agent loop = think→act→observe』"│
│        │                                                          │
│        ▼                                                          │
│  WriteFileTool.Execute(input{path: "notes.md", content: "..."})  │
│        │                                                          │
│        ▼                                                          │
│  ws.Write("notes.md", "agent loop ...")                          │
│        │                                                          │
│        ▼                                                          │
│  LocalWorkspace.resolve("notes.md")                              │
│   ├─ p == ""              ?  no                                  │
│   ├─ ContainsRune(p, 0)   ?  no                                  │
│   ├─ filepath.IsAbs(p)    ?  no                                  │
│   ├─ Join(root, p)        →  /tmp/abc/workspace/notes.md         │
│   ├─ Clean                →  /tmp/abc/workspace/notes.md         │
│   ├─ HasPrefix(cleaned+/, /tmp/abc/workspace/)  ?  yes ✓         │
│   └─ return cleaned                                               │
│        │                                                          │
│        ▼                                                          │
│  os.MkdirAll(filepath.Dir(abs), 0o755)                           │
│  os.WriteFile(abs, []byte("agent loop ..."), 0o644)              │
│        │                                                          │
│        ▼                                                          │
│  return "wrote 35 bytes to notes.md"                             │
└──────────────────────────────────────────────────────────────────┘
```

### `resolve()` —— 三道闸 + 一个 Clean+前缀

```go
func (l *LocalWorkspace) resolve(p string) (string, error) {
    if p == "" {
        return "", fmt.Errorf("path is empty")
    }
    if strings.ContainsRune(p, 0) {
        return "", fmt.Errorf("path contains null byte")
    }
    if filepath.IsAbs(p) {
        return "", fmt.Errorf("absolute path not allowed: %q", p)
    }
    cleaned := filepath.Clean(filepath.Join(l.root, p))
    if !strings.HasPrefix(cleaned+string(filepath.Separator), l.root) &&
        cleaned+string(filepath.Separator) != l.root {
        return "", fmt.Errorf("path escapes root: %q -> %q", p, cleaned)
    }
    return cleaned, nil
}
```

四个攻击向量、四道防线：

| 攻击 | 哪一道拦的 |
|---|---|
| `path = ""` | early return「path is empty」 |
| `path = "x\x00/etc/passwd"` | `ContainsRune(p, 0)` 直接 reject |
| `path = "/etc/passwd"` | `filepath.IsAbs` 直接 reject |
| `path = "../../etc/passwd"` | `Join + Clean` 后 cleaned 里没了 `..`，但绝对路径会跑到 root 之外，`HasPrefix` 失败 |

### 三个非显然之处

1. **必须先 Clean 再前缀检查，不能光禁 `..`**

   一个朴素的「字符串里禁 `..`」会被这种串绕开：

   ```
   path = "ok/sub/../../escape.txt"
   //  → 初步看："../" 出现在 "ok/sub/" 之后，禁字面 ../ 没用
   //  → filepath.Clean 后："../escape.txt"
   //  → HasPrefix(/tmp/ws/../escape.txt, /tmp/ws/) → false → 拦下
   ```

   单纯字符串扫描不够，必须先解析路径再检查解析后的位置。

2. **末尾分隔符是细节但要紧**

   想象 root 是 `/tmp/ws`（没分隔符）。如果存的就是 `/tmp/ws`：

   ```
   path = "../ws-evil/secret"
   cleaned = "/tmp/ws-evil/secret"
   HasPrefix("/tmp/ws-evil/secret", "/tmp/ws") → TRUE ✗
   ```

   错误地接受了！把 root 存成 `/tmp/ws/`（带分隔符），把要检查的路径也补上分隔符（`cleaned+/`），这种"前缀别名"就被卡死。这就是为什么我们的 `LocalWorkspace.root` 字段总是带末尾 separator。

3. **`List` 必须返回相对路径，不能返回绝对路径**

   `os.ReadDir` 给你的是 `info.Name()`——相对没问题。但 `filepath.Walk` 给的是绝对路径。如果直接 return 出去，agent 会看到 `/private/tmp/abc.../workspace/foo.md`——*这泄漏了主机文件系统结构给 LLM*。下一次它可能就用这个绝对路径试着写盘（被 `IsAbs` 拦下）但更糟的是它现在知道你的 workspace 大致位置在哪。所以 `List` 内部用 `filepath.Rel(l.root, p)` 把每个走到的文件转成相对路径再返回。测试用 `strings.Contains(p, host_root)` 显式断言「绝对前缀不能出现」。

## What Changed / 与 s05 的变化

```diff
 agents/s06-workspace/
 ├── provider.go              # 与 s05 一字不差
 ├── provider_openai.go       # 与 s05 一字不差
 ├── provider_mock.go         # 与 s05 一字不差
 ├── provider_anthropic_test.go  # 与 s05 一字不差
 ├── provider_openai_test.go  # 与 s05 一字不差
 ├── provider_mock_test.go    # 与 s05 一字不差
 ├── tools.go / tools_test.go # 与 s05 一字不差
 ├── registry.go / registry_test.go  # 与 s05 一字不差
 ├── strategy.go / strategy_test.go  # 与 s05 一字不差
 ├── history.go / history_test.go    # 与 s05 一字不差
 ├── loop.go / loop_test.go   # 与 s05 一字不差
+├── workspace.go             # 新：Workspace 接口、LocalWorkspace、resolve()
+├── workspace_test.go        # 新：5 个测试
+├── tools_file.go            # 新：ReadFileTool / WriteFileTool（构造时拿 Workspace）
+├── tools_file_test.go       # 新：4 个测试
 └── main.go                  # 改：构造 LocalWorkspace，注册 read_file + write_file
```

类型 catalog 新增：

```go
type Workspace interface {
    Read(path string) (string, error)
    Write(path, content string) error
    List(prefix string) ([]string, error)
}

type LocalWorkspace struct { root string }
```

**`Loop` 没变**。这是 s06 的设计美德：增加一个工具家族不需要触碰 Loop——只需要 Registry.Register 就行了。这正是 s02 的 Registry 抽象在 s06 兑现红利的地方：「显式注册」让"加新工具"是个 import-then-Register 的事，不是修改 Loop 的事。

## Try It / 动手试一试

```bash
cd agents/s06-workspace

# Anthropic native + oneshot（默认）；workspace 默认 ./workspace/
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "create notes.md with the sentence: agent loop = think → act → observe"

# DeepSeek + 自定义 workspace 目录
export DEEPSEEK_API_KEY=sk-...
go run . -provider deepseek -workspace /tmp/agent-out -v \
  "write a haiku about agents to poem.md, then read it back"

# 本地 vLLM / SGLang
export OPENAI_API_KEY=anything
go run . -provider local -model llama-3.3 \
  "list files in the workspace, then write index.md summarizing them"

# 跑全部测试（应该 60+ 个全过）
go test -v ./...
```

期望输出（Anthropic 路径，先 write 再 read 的任务）：

```
[s06-workspace] provider=anthropic model=claude-sonnet-4-6 url= strategy=oneshot tools=4 workspace=/Users/yeding/learn-AutoGPT/agents/s06-workspace/workspace
[turn 0] assistant: I'll write a haiku to poem.md first.
[turn 0] proposal: cmd=write_file thoughts="I'll write a haiku ..."
[turn 0] -> write_file map[content:"agent watches\nLLM whispers next move\ntool returns truth\n" path:poem.md]
[turn 0] <- wrote 51 bytes to poem.md
[turn 1] assistant: Now I'll read it back to verify.
[turn 1] proposal: cmd=read_file
[turn 1] -> read_file map[path:poem.md]
[turn 1] <- agent watches\nLLM whispers next move\ntool returns truth\n
[turn 2] assistant: Done. The haiku is saved to poem.md.
Done. The haiku is saved to poem.md.
[s06-workspace] final history: 2 episodes
```

`./workspace/poem.md` 现在真的存在。这是从 s01 起 6 节代码以来 agent 第一次在世界上留下副作用——这就是 s06 的意义。

如果想看路径净化器干活，试试这个：

```bash
go run . -v "write '/etc/passwd' with content 'pwned'"
# → [turn 0] <- error: write_file: absolute path not allowed: "/etc/passwd"
# 模型会看到这个 error，下一轮就会改用相对路径或者放弃
```

## Upstream Source Reading / 上游源码阅读

AutoGPT classic 的 file storage 在 `classic/forge/forge/file_storage/`——`base.py` 是 ABC + 路径净化器，`local.py` 是本地实现。完整双语注解版见 [`upstream-readings/s06-file-storage.py`](../../upstream-readings/s06-file-storage.py)；本节只摘核心。

```upstream:classic/forge/forge/file_storage/base.py
# Source: classic/forge/forge/file_storage/base.py
# 简化：去掉 Pydantic config、async、binary 分支、event hook，
# 保留 FileStorage ABC 与 _sanitize_path 路径净化器。

class FileStorage(ABC):
    """文件存储抽象基类。

    [→ s06：对应我们的 Go `type Workspace interface { Read, Write, List }`。
       上游接口面更宽（open_file、exists、delete_file、rename、copy、
       make_dir、list_folders、clone_with_subroot），s06 砍到三个方法
       够用就行。]"""

    on_write_file: Callable[[Path], Any] | None = None
    """写文件后触发的钩子。
    [→ s06 不建模这个。AutoGPT 拿来给 S3 backend 写完后做云同步；
       我们留作 s10 pipeline hooks 的位置。]"""

    @property
    @abstractmethod
    def restrict_to_root(self) -> bool:
        """是否限制访问只能在 root 内。
        [→ s06 没这个开关——LocalWorkspace 永远沙箱。上游有这个
           flag 是因为 FileStorage 也用来管 agent 自己的 state 文件
           （settings、history），那些合理地在 workspace 之外；
           我们不背这个负担。]"""

    @abstractmethod
    def read_file(self, path: str | Path, binary: bool = False) -> str | bytes: ...

    @abstractmethod
    async def write_file(self, path: str | Path, content: str | bytes) -> None: ...

    @abstractmethod
    def list_files(self, path: str | Path = ".") -> list[Path]: ...
```

```upstream:classic/forge/forge/file_storage/base.py
# _sanitize_path —— s06 教学的核心函数。

def _sanitize_path(self, path: str | Path) -> Path:
    """把相对路径解析到 root 内。

    Raises:
        ValueError: 绝对路径越权 / 越出 root。
    """
    # POSIX 不允许路径带 null 字节。先做这一道。
    # [→ s06：对应 strings.ContainsRune(p, 0)。]
    if "\0" in str(path):
        raise ValueError("Embedded null byte")

    relative_path = Path(path)

    # 上游允许*已经在 root 内*的绝对路径。我们 Go 版直接 reject 所有绝对路径——
    # 教学上更干净，agent 拿到相对 root 就没理由构造绝对。
    # [→ s06：我们直接 if filepath.IsAbs(p) → return err。]
    if (
        relative_path.is_absolute()
        and self.restrict_to_root
        and not relative_path.is_relative_to(self.root)
    ):
        raise ValueError(...)

    # 关键：Join 然后 resolve（跟 symlink、collapse `..`、`.`）。
    # [→ s06：filepath.Clean(filepath.Join(l.root, p))。
    #    Clean 会做 `..`/`.` collapse 但*不跟 symlink*——这是
    #    semantics 差别 #2，详见 upstream-readings 文件。]
    full_path = self.root / relative_path
    if self.is_local:
        full_path = full_path.resolve()
    else:
        full_path = Path(os.path.normpath(full_path))

    # 真正的越权检查：解析完之后必须仍在 root 内。
    # [→ s06：strings.HasPrefix(cleaned+sep, l.root)。]
    if self.restrict_to_root and not full_path.is_relative_to(self.root):
        raise ValueError(...)

    return full_path
```

### 对照阅读要点

- **上游 `Path.resolve()` vs 我们 `filepath.Clean`**：`resolve()` 跟 symlink，`Clean` 不跟。trade-off：我们 Go 版不会被 symlink 绕晕*但是* workspace 里如果有指向外面的 symlink，攻击者就能逃脱。s06 测试不覆盖 symlink 情况；s07 的 permissions 层是更合适的位置加 `Lstat` 检查。

- **上游 root 不带末尾分隔符 + 用 `is_relative_to`**：`Path.is_relative_to(root)` 内部做的就是「root 当目录看，被检查路径必须在它下面」——等价于我们带末尾 sep 的字符串前缀检查。Python 提供了高层 API；Go 选择更底层的 `HasPrefix` 方案，但语义一致。

- **上游 `restrict_to_root` 是可选的**：我们 Go 版没有这个开关。原因：`Workspace` 在 s06 的设计意图就是沙箱。一个无沙箱的 workspace 等于 `os` 直接调用——根本不是 `Workspace`。上游有 flag 是因为 `FileStorage` 也用来管 agent 自己的 state 文件（settings.yaml、history JSON），那些合理地超出 workspace；我们不混淆这两件事。

- **上游 `LocalFileStorage` 写文件是 async**：因为同一个 ABC 也适配 `S3FileStorage`（HTTP PUT 必须 async）。本地实现继承了协议但不需要 async。Go 没有 color-mismatch 问题（`ctx.Context` 干同样的活），所以我们的 Workspace 是 sync。

- **上游 `clone_with_subroot` 我们没做**：用来在 workspace 内开嵌套 sub-workspace（agent 之间互相隔离时用）。s06 不需要；如果你做 s09 的多 agent 实验时需要再加。

**深入阅读**：[`upstream-readings/s06-file-storage.py`](../../upstream-readings/s06-file-storage.py) 把 base.py + local.py 完整摘录注解，逐行对照 Go 版。然后预览 s07：workspace 防住了「agent 写到 root 外」，但是 *root 里的`rm -rf .`* 也能毁掉 6 节工作。s07 的 `Permissions` 层在 path 与 cmd 层加 allow/deny 模式匹配。

---

**下一节预告**：s07 引入 `Permissions` 结构——allow/deny patterns + glob 匹配，loop 在 strategy.Parse 与 tool.Execute 之间插入 `Check(cmd, args)` 闸门，对 `Allow / Deny / Ask` 做策略决策。AutoGPT 上游的 `CommandPermissionManager` 是 4-level（`ONCE` / `AGENT` / `WORKSPACE` / `DENY`）；我们做 2-level 简化版，保留 4-level 作为附录 B 练习。
