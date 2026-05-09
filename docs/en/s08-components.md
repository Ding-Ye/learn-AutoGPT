---
title: "s08 · Pluggable component system"
chapter: 8
slug: s08-components
est_read_min: 14
---

# s08 · Pluggable component system

> What this teaches: refactor s07's 7-field Loop into a component-based shape. Introduce a `Component` marker interface (empty) plus three optional sub-interfaces — `CommandProvider` / `DirectiveProvider` / `MessageProvider`. A `ComponentBus` aggregates capabilities across components via type-assertion; the Loop holds a single `*ComponentBus` from which it derives the Registry, the system-prompt directive list, and any pre-injected messages. Two example components: `FileManagerComponent` (wraps a Workspace, emits read/write tools + 2 directives) and `WebFetchComponent` (emits a `web_fetch` tool with a configurable timeout). `PromptStrategy.BuildPrompt` gains a `directives []string` parameter.

---

## Problem

By the end of s07, `Loop` looks like this:

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

Seven fields (counting MaxTurns/Verbose as scalars). Each one is a "capability" the agent has: call the LLM, look up tools, build the prompt, remember the history, gate by permissions, ask the user. Add another capability — web fetch, vector search, memory store — and you grow the field count again. main.go gets longer.

Worse: **capabilities share state**. `ReadFileTool` and `WriteFileTool` both need the same `Workspace`. The Loop's field list doesn't have `Workspace` — it lives inside `tools_file.go::ReadFileTool.ws`, injected by main.go via `reg.Register(NewReadFileTool(ws))`. The "capability bundle" concept is scattered across multiple places.

AutoGPT's answer in `forge/agent/protocols.py` is the **component**:

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

Each component is a "capability bundle" implementing any subset of these three ABCs. The `Agent` holds a `[]Component` and uses `isinstance` checks to discover which protocols each component implements; method outputs aggregate: union of `get_commands()` = registry, union of `get_constraints()` = directive list, etc.

s08 brings this into Go:

1. `type Component interface{}` — empty marker
2. Three optional sub-interfaces (`CommandProvider` / `DirectiveProvider` / `MessageProvider`)
3. `ComponentBus` aggregates via type assertion
4. Loop's field count drops back to ~5 (Provider + Bus + Strategy + History + Permissions + Asker), with the Bus replacing `Tools`

## Solution

```go
// Marker interface: empty. Any value can be a Component.
type Component interface{}

// Three optional sub-protocols:
type CommandProvider   interface { Commands() []Tool }
type DirectiveProvider interface { Directives() []string }
type MessageProvider   interface { Messages() []Message }

// Aggregator:
type ComponentBus struct {
    components []Component
}
func NewComponentBus(components ...Component) *ComponentBus
func (b *ComponentBus) Registry() *Registry      // type-assertions across all CommandProviders
func (b *ComponentBus) Directives() []string     // ordered concat of all DirectiveProviders
func (b *ComponentBus) Messages() []Message      // ordered concat of all MessageProviders

// Loop change:
type Loop struct {
    Provider    Provider
    Components  *ComponentBus  // replaces Tools *Registry
    Strategy    PromptStrategy
    // ...
}
```

Two example components:

```go
// Implements TWO protocols: CommandProvider + DirectiveProvider. Wraps a Workspace.
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

// Implements ONE protocol: CommandProvider. A single web_fetch tool with a configurable timeout.
type WebFetchComponent struct {
    httpTimeout time.Duration
}
func (w *WebFetchComponent) Commands() []Tool {
    return []Tool{newWebFetchTool(w.httpTimeout)}
}
```

The PromptStrategy interface gains one parameter — `BuildPrompt` now takes `directives []string`:

```go
type PromptStrategy interface {
    BuildPrompt(history []*Episode, tools []ToolSchema, directives []string, task string) []Message
    ParseResponse(content []ContentBlock) (ActionProposal, error)
}
```

`OneShotStrategy.BuildSystem(tools, directives)` renders directives into a `## Directives` section, after the `## Best practices` block in the system prompt.

`main.go` no longer calls `reg.Register(...)`:

```go
components := []Component{
    NewFileManagerComponent(ws),
    NewWebFetchComponent(30 * time.Second),
}
bus := NewComponentBus(components...)
loop := &Loop{Provider: p, Components: bus, /* ... */}
```

Want to add a new capability? Write a new struct that implements at least one sub-protocol, and append it to the components slice. Loop unchanged. Strategy unchanged.

## How It Works

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
│   l.Components.Registry() ─── type-assert across components            │
│        │                       cp, ok := c.(CommandProvider)            │
│        │                       if ok { for tool := range cp.Commands(){ │
│        │                                  reg.Register(tool) }}        │
│        │                                                                │
│        ▼                                                                │
│   l.Components.Directives() ─── same scan, ordered concat               │
│        │   FileManager → ["Always read...", "Use list_files..."]       │
│        │   WebFetch    → []                                             │
│        │   merged      → ["Always read...", "Use list_files..."]       │
│        │                                                                │
│        ▼                                                                │
│   strategy.BuildPrompt(history, schemas, directives, task)              │
│   strategy.BuildSystem(schemas, directives)                             │
│        │   system prompt has "## Commands" + "## Best practices"        │
│        │   + "## Directives" (from components)                          │
│        │                                                                │
│        ▼                                                                │
│   provider.CreateMessage{System: ..., Tools: schemas, Messages: ...}    │
└────────────────────────────────────────────────────────────────────────┘
```

### `ComponentBus.Registry()` — type-assertion + register

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

Why panic instead of returning an error? Because `Registry()` is called once at Loop startup; a duplicate-name component pair is a developer mistake at `NewComponentBus(...)` time, not something the model can recover from at runtime. The panic produces a stack trace pointing straight at the misregistration.

### `ComponentBus.Directives()` — ordered aggregation

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

Returns `make([]string, 0)` (not nil) — guarantees the strategy receives a non-nil slice and doesn't need a nil check in render logic.

### `OneShotStrategy.BuildSystem` — the directive section

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

Why do directives come **after** best practices? Because best practices are static instructions owned by the strategy itself (every OneShot has the same five lines); directives are component-supplied and vary by construction. **Most recent instructions get the highest salience** — putting component-specific lines last leaves them freshest in the model's mind.

### Three non-obvious points

1. **Empty marker beats ABC inheritance for flexibility.**

   Python uses `class FileManagerComponent(DirectiveProvider, CommandProvider)` — inheritance expresses "which protocols I satisfy." Go uses structural typing:

   ```go
   type FileManagerComponent struct{ ws Workspace }
   func (f *FileManagerComponent) Commands() []Tool { ... }
   func (f *FileManagerComponent) Directives() []string { ... }
   ```

   Those two methods automatically satisfy `CommandProvider` and `DirectiveProvider` — **no explicit "I extend X" declaration**. A third-party package can write a struct with a `Commands()` method and pass it into `NewComponentBus(...)` without any `forge.register_component` ceremony.

   This is s08's architectural punchline: components are pluggable precisely because they're **self-describing**.

2. **MessageProvider runs only on turn 0 to avoid duplication.**

   `Loop.Run` prepends `bus.Messages()` only when `turn == 0 && len(*l.History) == 0`. Subsequent turns rebuild the conversation from `History.RenderMessages()` — if we prepended every turn, the model would see N copies of the same "preamble."

   This is the boundary between component and history: `MessageProvider` provides **session-start** fixed content (pinned reminders, current task description), not state to be re-injected each turn.

3. **PromptStrategy.BuildPrompt's signature is the s07→s08 breaking change.**

   ```diff
   - BuildPrompt(history []*Episode, tools []ToolSchema, task string) []Message
   + BuildPrompt(history []*Episode, tools []ToolSchema, directives []string, task string) []Message
   ```

   Why not have the Loop wrap a directive-prepender around the system message? Because the system message is **constructed by the strategy itself** (in `BuildSystem`) — bypassing the strategy is a violation of separation of concerns. Letting the strategy see directives means it can choose **how** to integrate them: OneShot renders into a `## Directives` section; a hypothetical "directive-as-user-prefix" strategy could splice them into the user message.

   This is strategy extensibility — instead of injecting at a fixed seam, we tell the strategy "here are some directives" and let the strategy pick the rendering.

## What Changed vs s07

```diff
 agents/s08-components/
 ├── provider.go                      # verbatim from s07
 ├── provider_openai.go               # verbatim
 ├── provider_mock.go                 # verbatim
 ├── provider_*_test.go               # verbatim
 ├── tools.go / tools_test.go         # verbatim
 ├── tools_file.go / _test.go         # verbatim
 ├── workspace.go / _test.go          # verbatim
 ├── registry.go / _test.go           # verbatim (test adapted to use ComponentBus)
 ├── history.go / _test.go            # verbatim
 ├── permissions.go / _test.go        # verbatim
 ├── strategy.go                      # MODIFIED: BuildPrompt gets directives param; BuildSystem renders them
 ├── strategy_test.go                 # s07 tests + 1 directive-rendering test
+├── component.go                     # NEW: Component marker + 3 sub-protocols + ComponentBus
+├── component_test.go                # NEW: 5 tests
+├── component_filemgr.go             # NEW: FileManagerComponent
+├── component_filemgr_test.go        # NEW: 2 tests
+├── component_web.go                 # NEW: WebFetchComponent + web_fetch tool
+├── component_web_test.go            # NEW: 2 tests (with httptest)
 ├── loop.go                          # MODIFIED: Tools *Registry → Components *ComponentBus
 ├── loop_test.go                     # MODIFIED: s07 tests + 2 directive-flow tests
 └── main.go                          # MODIFIED: builds []Component and ComponentBus
```

Type catalog additions:

```go
type Component interface{}
type CommandProvider   interface { Commands() []Tool }
type DirectiveProvider interface { Directives() []string }
type MessageProvider   interface { Messages() []Message }
type ComponentBus struct{ components []Component }

type FileManagerComponent struct{ ws Workspace }
type WebFetchComponent struct{ httpTimeout time.Duration }
```

`Loop`'s net change: `Tools *Registry` removed, `Components *ComponentBus` added.

`PromptStrategy.BuildPrompt`'s signature changed — the first breaking change since s04, documented in the strategy.go header. Every caller of PromptStrategy (only s08's Loop) updates one line.

## Try It

```bash
cd agents/s08-components

# 1. Default config: FileManager + WebFetch, 30s web timeout
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "fetch https://example.com and write a one-line summary to notes.md"

# 2. Shorter web timeout (for testing network-sensitive scenarios)
go run . -v -web-timeout 5s "fetch https://example.com"

# 3. Multi-provider — same 8 profiles as s03–s07
export DEEPSEEK_API_KEY=sk-...
go run . -provider deepseek -v "fetch https://example.com"

# 4. Run all tests
go test -v ./...
```

Expected verbose output:

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

### Add a custom component

Drop this file into `agents/s08-components/component_clock.go`:

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

Add `&ClockComponent{}` to main.go's components slice, rerun — the agent immediately gains a `now` tool. **Loop unchanged. Strategy unchanged.**

## Upstream Source Reading

AutoGPT classic's component system lives in `classic/forge/forge/agent/protocols.py` (the three ABCs) and `classic/forge/forge/components/` (concrete component examples). The fully annotated bilingual reading is at [`upstream-readings/s08-components.py`](../../upstream-readings/s08-components.py); this section pulls the core only.

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

# [→ s08: our Go translation:
#    - AgentComponent → empty interface{} marker
#    - CommandProvider → Commands() []Tool
#    - DirectiveProvider → Directives() []string (collapses 3 buckets into 1)
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

# [→ s08: our FileManagerComponent likewise implements Commands + Directives,
#    wraps a Workspace, emits read_file + write_file.]
```

### Side-by-side reading notes

- **Empty marker vs ABC.** Upstream uses `AgentComponent(ABC)` as a base class; our Go version uses `Component interface{}`. Structural typing means a component carries "which protocols I satisfy" in its method set — no `class extends` declaration needed.

- **Three directive buckets → one.** Upstream splits directives into `constraints`/`resources`/`best_practices` separate methods so the prompt template can render each section distinctly. The Go version collapses these into one `Directives() []string` — OneShotStrategy renders them all under a single `## Directives` header. If you want finer-grained grouping, expand the `DirectiveProvider` interface to return `map[string][]string`.

- **isinstance vs type-assertion.** Python's `isinstance(c, CommandProvider)` ↔ Go's `c.(CommandProvider)`. Same semantics, same runtime check.

- **MessageProvider is first-turn-only.** Upstream re-queries `messages` every turn (the component decides what/when). We simplify to "inject once on turn 0 only" to avoid duplication. If you need per-turn re-query (e.g., a "current time" message), modify `Loop.Run` to call `bus.Messages()` every iteration.

- **WebFetch simplification.** Upstream's web component (`forge/components/web/`) does HTML rendering, link extraction, BeautifulSoup parsing; we ship just GET + 8 KiB truncation. The reason: Go's standard library has no BS4 equivalent (`golang.org/x/net/html` is too low-level), and a token-friendly HTML→text conversion would add ~200 lines. We leave that as Appendix B exercise.

**Read more**: [`upstream-readings/s08-components.py`](../../upstream-readings/s08-components.py) has the full ABC definitions plus the Go translation table and "why type-assertion replaced isinstance" explainer. Then preview s09: by the end of s08, the agent runs one `Loop.Run` to completion; s09 introduces `RunInteractionLoop` + cycle budget + signal handling, turning the agent into a "continuous mode" runner — interruptible, pauseable, resumable.

---

**Next chapter preview**: s09 introduces `LoopOpts{Cycles, AskEachStep, OnInterrupt}` + a `RunInteractionLoop` wrapper + SIGINT handling via `os/signal.Notify` + a `UIProvider` interface (spinner / RenderThought / RenderResult). It promotes s01–s08's "single Run" into "N-cycle autonomy with graceful Ctrl-C", matching AutoGPT classic's `app/main.py:run_interaction_loop`.
