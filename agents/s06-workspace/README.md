# s06 · Sandboxed workspace storage

> **zh** s05 的 agent 已经会"记得自己干了什么"，但还不能真正写盘——`echo` 和 `math` 都没有副作用。s06 引入 `Workspace` 接口与 `LocalWorkspace` 实现：所有路径相对 root 解析，`filepath.Clean` + 前缀检查拦截 `../` 越权，连 null byte 与绝对路径都先 reject。两个新工具 `read_file` / `write_file` 接受一个 `Workspace` 作为构造参数——这是教程里第一次出现"工具需要外部依赖"的范式，s07 / s08 会沿着这条线生长。
> **en** s05's agent could remember what it did but couldn't write to disk — `echo` and `math` have no side effects. s06 introduces the `Workspace` interface plus a `LocalWorkspace` impl: all paths are resolved relative to a root, `filepath.Clean` + prefix-check rejects `../` traversal, and null bytes plus absolute paths get rejected up front. Two new tools `read_file` / `write_file` take a `Workspace` as a constructor argument — the first time in this curriculum a tool needs an external dependency, the pattern s07 / s08 will build on.

## Files

| file | role |
|---|---|
| `provider.go` / `provider_openai.go` / `provider_mock.go` | three Provider impls — verbatim from s05 |
| `provider_anthropic_test.go` / `provider_openai_test.go` / `provider_mock_test.go` | provider tests — verbatim |
| `tools.go` / `tools_test.go` | `EchoTool` + `MathTool` — verbatim |
| `registry.go` / `registry_test.go` | `Registry` — verbatim |
| `strategy.go` / `strategy_test.go` | `OneShotStrategy` — verbatim |
| `history.go` / `history_test.go` | `Episode` / `History` — verbatim |
| `loop.go` / `loop_test.go` | `Loop` with `*History` — verbatim |
| `workspace.go` | **NEW** — `Workspace` iface + `LocalWorkspace` + `resolve()` sanitizer |
| `workspace_test.go` | **NEW** — 5 tests (read after write; reject `../`; reject `/etc/passwd`; List returns relative; reject null byte) |
| `tools_file.go` | **NEW** — `ReadFileTool` + `WriteFileTool` — first tools with constructor deps |
| `tools_file_test.go` | **NEW** — 4 tests (read happy; write happy; read missing; write outside root rejected) |
| `main.go` | constructs a `LocalWorkspace` rooted at `./workspace/` (auto-mkdir) and registers both file tools |
| `testdata/golden_response.json` | sample Anthropic-shape response with native `tool_use` |

## Run / 运行

```bash
cd agents/s06-workspace

# Anthropic native + oneshot (default); workspace defaults to ./workspace/
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "create notes.md with the sentence: agent loop = think → act → observe"

# DeepSeek + custom workspace dir
export DEEPSEEK_API_KEY=sk-...
go run . -provider deepseek -workspace /tmp/agent-out -v "write a haiku to poem.md, then read it back"

# Local vLLM / SGLang
export OPENAI_API_KEY=anything
go run . -provider local -model llama-3.3 "list files in the workspace, then write index.md summarizing them"
```

## Test / 测试

```bash
go test -v ./...
```

Expect ~46 tests across 11 files: tools (echo + math), registry, loop, three provider tests, strategy, history, plus the new **workspace (5)** and **file tools (4)**.

## Key teaching points / 学习要点

1. **`filepath.Clean` is the load-bearing primitive / `Clean` 是核心** — a single `..` ban is porous (`a/../../x` passes a literal `..` reject because `..` follows a non-`..` segment, but Cleans to `../x` outside root). Always Join + Clean + prefix-check. 单纯禁 `..` 是漏的，必须先 Clean 再前缀检查。
2. **Trailing separator on the stored root / root 后面带分隔符** — `HasPrefix("/tmp/ws-evil/x", "/tmp/ws")` is `true` without it. Append the separator so the comparison means "inside this directory". 防止前缀串混入相邻同前缀目录。
3. **Tools take a `Workspace`, not a `string`** — the interface is the seam s07 (permissions) and s08 (FileManager component) will hook into. 工具拿 interface，不拿原始路径——s07/s08 沿这条线生长。
4. **List returns RELATIVE paths** — never leak the host's filesystem layout to the model. `/tmp/abc123/workspace/foo.txt` becomes `foo.txt`. 把绝对路径泄漏给 LLM 永远是个错误。
5. **Reject null bytes up front** — old C-string truncation defense. `safe.txt\x00/etc/passwd` reads fine in Go but lower libs may truncate. We never make the syscall. Null byte 拦截是防 C-string 截断古老攻击。

## Read more / 深入阅读

- 中文：[`docs/zh/s06-workspace.md`](../../docs/zh/s06-workspace.md)
- English: [`docs/en/s06-workspace.md`](../../docs/en/s06-workspace.md)
- Upstream excerpt: [`upstream-readings/s06-file-storage.py`](../../upstream-readings/s06-file-storage.py)
