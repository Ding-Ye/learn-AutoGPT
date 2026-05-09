# s07 · Layered permission system

> **zh** s06 的沙箱拦住了"agent 写到 root 外"，但 *root 内的* `rm -rf .` 仍能毁掉 6 节工作。s07 引入 `Permissions` 结构 + `Allow`/`Deny`/`Ask` 三态 Decision，glob 匹配的 `*`（单段）/`**`（跨段）模式，loop 在 `strategy.Parse` 与 `tool.Execute` 之间插入 `Check(cmd, args)` 闸门。新接口 `Asker`（生产用 `StdinAsker` 走 stdin 交互；测试用 `StubAsker` 返预设答案）。AutoGPT 上游的 `CommandPermissionManager` 是 4-level（`ONCE`/`AGENT`/`WORKSPACE`/`DENY`）；我们做 2-level 子集，把完整 4-level 留作附录 B 练习 #5。
> **en** s06's sandbox stops the agent from writing *outside* root, but `rm -rf .` *inside* root still wipes out 6 sessions of work. s07 introduces a `Permissions` struct with three-valued `Allow`/`Deny`/`Ask` `Decision`, glob matching with `*` (one segment) and `**` (any segments), and a `Check(cmd, args)` gate the Loop calls between `strategy.Parse` and `tool.Execute`. New `Asker` interface (`StdinAsker` for production interactive y/N; `StubAsker` for tests). AutoGPT upstream's `CommandPermissionManager` is 4-level (`ONCE`/`AGENT`/`WORKSPACE`/`DENY`); we ship the 2-level subset and leave the full 4-level as Appendix B exercise #5.

## Files

| file | role |
|---|---|
| `provider.go` / `provider_openai.go` / `provider_mock.go` | three Provider impls — verbatim from s06 |
| `provider_anthropic_test.go` / `provider_openai_test.go` / `provider_mock_test.go` | provider tests — verbatim |
| `tools.go` / `tools_test.go` | `EchoTool` + `MathTool` — verbatim |
| `tools_file.go` / `tools_file_test.go` | `ReadFileTool` + `WriteFileTool` — verbatim |
| `workspace.go` / `workspace_test.go` | `Workspace` iface + `LocalWorkspace` — verbatim |
| `registry.go` / `registry_test.go` | `Registry` — verbatim |
| `strategy.go` / `strategy_test.go` | `OneShotStrategy` — verbatim |
| `history.go` / `history_test.go` | `Episode` / `History` — verbatim |
| `permissions.go` | **NEW** — `Decision`, `Pattern`, `Permissions`, `Check`, custom glob matcher, `Asker` iface, `StdinAsker`, `StubAsker` |
| `permissions_test.go` | **NEW** — 8 tests (basic match, deny>allow, default Ask, first-deny short-circuit, cmd-only pattern, arg-glob pattern, ** vs *, stub asker) |
| `loop.go` | **MODIFIED** — adds `Permissions *Permissions` + `Asker Asker` fields; gates dispatch via `Check` between Parse and Execute |
| `loop_test.go` | s06 tests + 2 new permission-gate tests |
| `main.go` | constructs `Permissions` from `./permissions.json` (or built-in defaults), `Asker` from `-ask` flag |
| `testdata/permissions.yaml` | sample config — JSON-shape (parses as JSON; YAML extension is per the plan) |
| `testdata/golden_response.json` | sample Anthropic-shape response |

## Run / 运行

```bash
cd agents/s07-permissions

# Default: load ./permissions.json if present, else built-in defaults
# (allow read_file, write_file, echo, math). -ask=deny means any pattern
# that hits Ask is auto-denied — fail-closed safe default.
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "create notes.md with a one-line summary of the agent loop"

# Interactive permission prompt — agent pauses; you say y/N for each tool
go run . -v -ask stdin "read README.md, then write a paraphrase to notes.md"

# Custom permissions file — drop a JSON file with {"allow":[...], "deny":[...]}
cp testdata/permissions.yaml ./permissions.json
go run . -v "try to write somewhere dangerous"
```

## Test / 测试

```bash
go test -v ./...
```

Expect ~50 tests passing — adds **permissions (8)** and **2 loop permission-gate tests** on top of the s06 inheritance.

## Key teaching points / 学习要点

1. **Gate at Loop seams, not in Tool.Execute** — cross-cutting concerns (permission, logging, audit) belong at the agent loop's seams. If the gate were inside each tool, you'd duplicate it N times and miss any tool that forgot to call it. 把跨切关注点放在 loop 的接缝处，不要洒进每个工具的 Execute。
2. **Deny before Allow** — DenyList is consulted first so a narrow deny ("bash: rm -rf**") can carve a hole out of a broad allow ("bash: **"). 拒绝优先于放行——窄 deny 能在宽 allow 上挖洞，无需重写宽规则。
3. **Default to Ask, not Deny** — when no rule matches, return `Ask` (not `Deny`). The Asker decides whether to pause for human approval (`StdinAsker`) or fail-closed (`StubAsker(Deny)`). 默认是询问，不是拒绝——询问是策略；具体动作交给 Asker。
4. **`*` is single-segment, `**` is cross-segment** — `read_file: *.md` does NOT match `src/notes.md` because the `*` won't span `/`. Use `read_file: **.md` (or `read_file: **/*.md`) for cross-directory matches. This matches AutoGPT upstream's `_pattern_matches`. 单星不跨 `/`，双星跨。
5. **Asker is an interface, not a global stdin** — per dossier anti-pattern #1: tests get a `StubAsker`; production gets `StdinAsker`; s09 will swap in a Rich-style UI. `Asker` 是接口而非全局 stdin。

## Read more / 深入阅读

- 中文：[`docs/zh/s07-permissions.md`](../../docs/zh/s07-permissions.md)
- English: [`docs/en/s07-permissions.md`](../../docs/en/s07-permissions.md)
- Upstream excerpt: [`upstream-readings/s07-permissions.py`](../../upstream-readings/s07-permissions.py)
