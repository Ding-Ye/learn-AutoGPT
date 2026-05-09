# s08 · Pluggable Component system

> **zh** s07 末尾，`Loop` 持有 `Provider, Tools, Strategy, History, Workspace, Permissions, Asker` 7 个字段。AutoGPT 用 component 把这种"能力捆绑"重组：每个 Component 实现 `Component` 标记接口（空），并选实现 `CommandProvider`/`DirectiveProvider`/`MessageProvider` 中的若干个。`ComponentBus` 通过类型断言聚合 Commands/Directives/Messages。Loop 的字段从 7 个收回 2.5 个（Provider + ComponentBus + 跨切关注点）。新示例 component：`FileManagerComponent`（包 Workspace + 吐 read_file/write_file + 2 条 directive）和 `WebFetchComponent`（吐 web_fetch，超时可配）。`PromptStrategy.BuildPrompt` 签名扩了一个 `directives []string` 参数，OneShot 把它渲染到系统 prompt 的 `## Directives` 段。
> **en** By s07's end, `Loop` carries 7 fields. AutoGPT consolidates these "capability bundles" into components: each Component satisfies an empty `Component` marker plus any subset of `CommandProvider`/`DirectiveProvider`/`MessageProvider`. A `ComponentBus` aggregates commands/directives/messages via type-assertion. `Loop`'s field count drops back to 2.5 (Provider + ComponentBus + cross-cutting concerns). Two example components: `FileManagerComponent` (wraps a Workspace, emits read_file/write_file + 2 directives) and `WebFetchComponent` (emits web_fetch with configurable timeout). `PromptStrategy.BuildPrompt` gains a `directives []string` parameter; OneShot renders it into a `## Directives` section in the system prompt.

## Files

| file | role |
|---|---|
| `provider.go` / `provider_openai.go` / `provider_mock.go` | three Provider impls — verbatim from s07 |
| `provider_anthropic_test.go` / `provider_openai_test.go` / `provider_mock_test.go` | provider tests — verbatim |
| `tools.go` / `tools_test.go` | `EchoTool` + `MathTool` — verbatim |
| `tools_file.go` / `tools_file_test.go` | `ReadFileTool` + `WriteFileTool` — verbatim |
| `workspace.go` / `workspace_test.go` | `Workspace` iface + `LocalWorkspace` — verbatim |
| `registry.go` / `registry_test.go` | `Registry` — verbatim (test adapted to use ComponentBus) |
| `history.go` / `history_test.go` | `Episode` / `History` — verbatim |
| `permissions.go` / `permissions_test.go` | permissions — verbatim |
| `strategy.go` | **MODIFIED** — `BuildPrompt` gains `directives` param; `BuildSystem` renders directives section |
| `strategy_test.go` | s07 tests + 1 new directives-rendering test |
| `component.go` | **NEW** — `Component` marker, 3 sub-protocols, `ComponentBus` |
| `component_test.go` | **NEW** — 5 tests (independent type-switch detection, registry/directives aggregation, marker-only, multi-component) |
| `component_filemgr.go` / `component_filemgr_test.go` | **NEW** — `FileManagerComponent` (CommandProvider + DirectiveProvider) |
| `component_web.go` / `component_web_test.go` | **NEW** — `WebFetchComponent` + `web_fetch` tool with timeout + truncation |
| `loop.go` | **MODIFIED** — `Tools *Registry` → `Components *ComponentBus`; derives registry/directives at Run start |
| `loop_test.go` | s07 tests adapted + 2 new directive-flow tests |
| `main.go` | constructs `[]Component{NewFileManagerComponent(ws), NewWebFetchComponent(30s)}` and a bus |
| `testdata/golden_response.json` | sample Anthropic-shape response |
| `testdata/permissions.yaml` | sample permissions config |

## Run / 运行

```bash
cd agents/s08-components

# Default: FileManager + WebFetch components, 30s web timeout
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "fetch https://example.com and write a one-line summary to notes.md"

# Custom web timeout
go run . -v -web-timeout 5s "fetch https://example.com"

# Same multi-provider story as s03–s07 — set the right env var per profile
export DEEPSEEK_API_KEY=sk-...
go run . -provider deepseek -v "fetch https://example.com"
```

## Test / 测试

```bash
go test -v ./...
```

Adds **component (5)** + **filemgr (2)** + **web_fetch (4 sub-tests across 2 funcs)** + **2 loop directive-flow tests** + **1 strategy directive-rendering test** on top of s07's inheritance.

## Key teaching points / 学习要点

1. **Empty marker + optional sub-interfaces** — `Component` is `interface{}`. Each capability protocol (Commands/Directives/Messages) is its own interface; a component implements the subset it needs. Go's structural typing makes this clean — no `extends ABC, Mixin1, Mixin2` ceremony, just methods. 标记接口 + 可选子接口：要哪个能力，实现哪个 method。
2. **ComponentBus aggregates via type-switch** — `for c := range components { if cp, ok := c.(CommandProvider); ok { ... } }`. The bus walks the slice once per stream; ordering is preserved so prompts stay deterministic. ComponentBus 用类型断言遍历，顺序稳定。
3. **Directives flow through the strategy seam** — bus → directives → `Strategy.BuildPrompt(directives)` → `OneShot.BuildSystem(tools, directives)` → system prompt's `## Directives` section. Adding a new directive means adding one line to a component's `Directives()`; the rest is automatic. Directive 一条线流到 system prompt：组件出 → bus 聚合 → strategy 渲染。
4. **Loop's field count collapses** — s07's 7 fields (Provider, Tools, Strategy, History, Workspace-via-tools, Permissions, Asker) become s08's: Provider + ComponentBus + Strategy + History + Permissions + Asker. Workspace moves INSIDE FileManagerComponent — a new file storage scheme means a new component, not new Loop fields. Loop 字段收回；新存储 = 新 component。
5. **WebFetch demonstrates network-capability components** — `WebFetchComponent` shows one tool with a `time.Duration` constructor argument; tests use `httptest.NewServer` for the network seam. Real network calls never happen in tests. WebFetch 演示有构造参数的网络型 component。

## Read more / 深入阅读

- 中文：[`docs/zh/s08-components.md`](../../docs/zh/s08-components.md)
- English: [`docs/en/s08-components.md`](../../docs/en/s08-components.md)
- Upstream excerpt: [`upstream-readings/s08-components.py`](../../upstream-readings/s08-components.py)
