---
title: "s02 · Explicit command registry"
chapter: 2
slug: s02-command-registry
est_read_min: 10
---

# s02 · Explicit command registry

> What this teaches: replace s01's hard-coded `[]Tool{NewEchoTool()}` with an explicit `Registry`. The mechanism is **command_registry** — upstream AutoGPT uses Python's `@command` decorator for auto-registration; Go has no decorator, so we build a registration center any file can call `Register(myTool)` against.

---

## Problem

s01 wrote the tool list as a literal `[]Tool{NewEchoTool()}`. It works, but three problems surface immediately. (1) Adding a tool means editing `main.go` — if multiple files want to expose tools, who owns that line? (2) `Loop.Run` rebuilds the `name → Tool` map from the slice on every call. (3) Anyone asking "which tools does the agent currently have?" has to grep slice literals.

AutoGPT classic solves this in `classic/forge/forge/command/decorator.py` with `@command(...)` — annotate a function and importing the module auto-registers it. Elegant, but **implicit**: the dependency lives in the import graph, and removing one `import` line silently removes the tool. Go has no decorators, and even if it did, this is the wrong path: the dossier's anti-pattern #4 is explicit — *"decorator-based registration is implicit; Go repo prefers explicit so deps are visible."* This chapter solves that: **make tool registration a grep-able source fact, not an import side effect.**

## Solution

Introduce a `Registry` type — a `name → Tool` index with three methods: `Register(t Tool) error`, `Lookup(name) (Tool, bool)`, `All() []ToolSchema`. Any package that wants to expose a tool writes one line of `reg.Register(...)`, and that line is the source-level evidence of the dependency.

Key design decisions:

1. **Lookup returns `(Tool, bool)`, not `(Tool, error)`** — "not found" is a routine condition the caller decides what to do with (Loop translates a miss into an "unknown tool" tool_result so the model can self-correct). Lookup itself didn't fail. Wrapping missing as an error would mislead callers into treating it like an IO/parse failure.
2. **`All()` must return insertion order** — Go map iteration order is randomized. If the tool list shown to the model jiggles each turn, prompt cache hit rate drops and golden tests can't pin anything. A parallel `[]string{names}` slice solves this, paying one extra field for determinism.
3. **Duplicate names error, never overwrite silently** — silent overwrites cause the worst kind of bug: you think you changed A, but B is still running. Refuse with `fmt.Errorf("tool %q already registered", name)` so conflicts are visible at startup.

One pedagogical add-on: alongside `EchoTool`, ship `MathTool` (add/sub/mul/div). With one tool, "look up by name" and "always return the only one" are observationally identical; two tools make the Registry's job real.

## How It Works

```ascii-anim frames=2
┌────────────────────────────────────────────────────────────────┐
│   main.go                                                      │
│      reg := NewRegistry()                                      │
│      reg.Register(NewEchoTool())   ──┐  explicit dep edges    │
│      reg.Register(NewMathTool())   ──┤  visible to grep        │
│      loop := &Loop{Tools: reg, …}  ──┘                         │
│                          │                                     │
│                          ▼                                     │
│   Loop.Run(prompt):                                            │
│      schemas := reg.All()           ── insertion order         │
│      send to Provider as Tools field                          │
│                                                                │
│   Provider returns tool_use{Name:"math", …}                    │
│      ↓                                                         │
│   Loop.runTools:                                               │
│      tool, ok := reg.Lookup(block.Name)                        │
│      ├── ok=true  → tool.Execute(input) → tool_result          │
│      └── ok=false → "unknown tool: ?" tool_result              │
│                     (model self-corrects; loop survives)       │
└────────────────────────────────────────────────────────────────┘
```

Core code (excerpt from [`agents/s02-command-registry/registry.go`](https://github.com/Ding-Ye/learn-AutoGPT/blob/main/agents/s02-command-registry/registry.go)):

```go
type Registry struct {
    commands map[string]Tool
    names    []string // parallel slice keeps insertion order stable
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

The Loop's dispatch path also changed — s01 built a map at the top of `Run`; s02 just calls `Lookup`:

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

**3 non-obvious points**:

1. **The `names []string` parallel slice is not redundant** — the `commands` map alone could store every tool, but Go map iteration order is randomized. We need "registered first, listed first" because (a) the prompt shown to the model must be stable for cache hits, (b) golden tests must be able to pin order, (c) the doc viewer should render the same order as the source. One extra field buys determinism.
2. **Lookup misses don't raise — they let the model self-correct** — models occasionally hallucinate tool names that don't exist (especially small open-weight ones). Panicking on miss kills the loop and the run. Translating the miss into a `tool_result: "unknown tool: %q"` and feeding it back means the model sees its own mistake on the next turn and picks a real tool. This continues s01's "loop survives, model recovers" principle into the Registry layer.
3. **Reject `Schema().Name == ""` too** — an inconspicuous corner case: a developer writing a new Tool forgets to fill in `Name`, so Schema returns the zero value. Without the check, the empty string registers fine the first time; a second tool with the same bug fires `tool "" already registered` — looks like a registry bug but is actually a typo. Rejecting empty names early makes the error message point at the real cause.

## What Changed (vs s01)

```diff
 type Loop struct {
     Provider Provider
-    Tools    []Tool       // s01: literal slice; map rebuilt every Run
+    Tools    *Registry    // s02: explicit registry; index built at Register
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
     // ... think→act→observe body unchanged ...
 }

 // The dispatch path inside runTools has the same semantics, new caller:
-tool, ok := byName[block.Name]
+tool, ok := l.Tools.Lookup(block.Name)
```

`main.go` also goes from `tools := []Tool{NewEchoTool()}` to three explicit lines:

```go
reg := NewRegistry()
if err := reg.Register(NewEchoTool()); err != nil { log.Fatalf("%v", err) }
if err := reg.Register(NewMathTool()); err != nil { log.Fatalf("%v", err) }
```

**Semantically**: the tool list moved from "literal" to "runtime-built index", but because Register happens once at startup and the registry is read-only thereafter, concurrency safety and hot-reload aren't problems s02 has to deal with — s08 (components) makes construction more interesting; we'll handle that then.

## Try It

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s02-command-registry

# demo 1: ask the model to use the math tool (watch Registry dispatch)
go run . -v "use the math tool to add 7 and 35"

# demo 2: same registry, different name → echo path
go run . -v "echo back the word 'hello'"

# demo 3: switch to an OpenAI-compat backend (DeepSeek) — see the Registry
# schema list translated into OpenAI tools format on the wire
export DEEPSEEK_API_KEY=...
go run . -provider deepseek -v "use math to multiply 6 and 7"

# run all tests (should be 36 passing)
go test -v ./...
```

Expected output shape:

```
[s02-command-registry] provider=anthropic model=claude-sonnet-4-6 url= tools=2
[turn 0] assistant: I'll use the math tool to add these.
[turn 0] -> math map[a:7 b:35 operation:add]
[turn 0] <- 42
[turn 1] assistant: 7 + 35 = 42.
7 + 35 = 42.
```

If you see `unknown tool: "calculator"` — the model hallucinated a name that isn't in the registry; the next turn it self-corrects. That's the "miss survives, model recovers" design at work.

## Upstream Source Reading

AutoGPT classic's `classic/forge/forge/command/` directory holds three files for the Command abstraction: `command.py` (the `Command` class), `decorator.py` (the `@command` decorator), and `parameter.py` (`CommandParameter`). Below is the decorator + Command core, side by side with how upstream auto-registers a Python function as an agent-callable command.

```upstream:classic/forge/forge/command/decorator.py
# Source: classic/forge/forge/command/decorator.py
# Simplified by removing generic typing; preserves the decorator's execution path.

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
    Wrap a function as a Command.
    - empty names      → use func.__name__
    - empty description → take the first paragraph of the docstring
    - parameters        → wrap each into a CommandParameter
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

        # Key: wrap func in a Command instance and return it. The return
        # value REPLACES the original function at module scope, so when
        # someone imports the module, my_func IS now a Command object.
        # CommandProvider protocol picks it up and feeds it into agent.commands.
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
# Simplified by stripping ParamSpec/Generic typing.

class Command:
    """A class representing a command.

    Attributes:
        names (list[str]): aliases (first one is canonical)
        description (str): brief description shown to the LLM
        method (Callable): the actual handler function
        parameters (list[CommandParameter]): parameter schema
    """

    def __init__(self, names, description, method, parameters):
        # Validate: parameter names declared by the decorator must match
        # the function signature. This is the one static check Python can
        # do at import time — mismatch raises ValueError on module load.
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
        # Descriptor protocol — when a Command attached to a class is
        # accessed via an instance, auto-bind self. This is why @command-
        # decorated methods can be called like normal methods.
        if instance is None:
            return self
        return Command(
            self.names, self.description,
            self.method.__get__(instance, owner),
            self.parameters,
        )
```

**Reading notes**:

- **Implicit registration vs explicit Register**: upstream chains "import module → decorator runs → function is replaced by a Command instance → CommandProvider protocol collects all Commands at agent startup". Not a single line of "explicit registration" anywhere — the dependency lives entirely in the import graph. Our Go version writes one `reg.Register(...)` line per tool in `main.go` (or, in s08, in component constructors). More typing, but grep-friendly.
- **Multi-name aliases vs single name**: upstream's `Command.names` is a `list[str]` (one command, multiple aliases). We simplify to a single `Schema().Name`. Multi-alias support is left as Appendix B exercise material.
- **Parameter schema validation timing**: upstream's `_parameters_match` checks "decorator-declared parameter names" vs "function signature" at `Command.__init__` — failing at import time. Go has no equivalent runtime-reflection convenience (and shouldn't: the `Tool` interface signature is fixed). We push type errors to `Tool.Execute` via `requireString/requireNumber`, which surfaces failures *after* the LLM call. Different timing, same visibility guarantee.
- **No `__get__` descriptor analog**: upstream uses Python's descriptor protocol so a `@command`-decorated method can be called from either class or instance — needed because `CommandProvider` treats the class itself as a registry. Our Go version writes `reg.Register(NewEchoTool())` directly in `main`; there's no "class-as-registry" idiom, so the descriptor protocol has no analog.
- **`disabled_commands` is deliberately omitted**: AutoGPT runs `_remove_disabled_commands` at startup (per the `AppConfig.disabled_commands` list) to strip certain commands. We skip it — s07 (permissions) covers the same ability with a more general `Allow/Deny pattern` system.

**Read further**: start at `classic/forge/forge/command/decorator.py::command`, follow `Command.__init__` into `command.py`, then read `forge/agent/protocols.py::CommandProvider` (covered in depth in s08) for how components inject commands into the agent. That trace is the real-source map for s02 → s08 (components) → s07 (permissions match by name against allow/deny patterns).

---

**Next**: s03 finishes the multi-backend Provider story — s01 already shipped Anthropic + OpenAI-compat implementations, but the chapter that consolidates the `-provider` flag logic, profile table, full httptest matrix, and the mock-provider reuse strategy across sessions is s03.
