---
title: "s10 · Reflexion & AfterParse hooks"
chapter: 10
slug: s10-reflexion-hooks
est_read_min: 16
---

# s10 · Reflexion & AfterParse hooks

> What this teaches: extract cross-cutting concerns from `Loop` into a `Pipeline` of `AfterParse` / `AfterExecute` hooks. Reflexion is the first user — a second-pass LLM evaluation that judges every proposal and rewrites it before tool dispatch. This is the most architecturally significant chapter in the curriculum.

---

## Problem

s09 lets the agent run autonomously across multiple turns, but the Loop still hard-codes a straight line: `strategy.Parse → permissions.Check → tool.Execute → history.Append`. If you want to insert "let the LLM second-guess every proposal before execution" — the canonical Reflexion / Self-Refine pattern — you have only two ugly options:

1. Stuff the second-pass LLM call inside `OneShotStrategy.ParseResponse`, conflating prompt parsing with action validation.
2. Write a parallel `ReflexionLoop` that copies all of s09 and adds a section, doubling the maintenance cost across sessions.

AutoGPT upstream walked past both. Its answer lives in `forge/agent/protocols.py`: define abstract base classes `AfterParse` and `AfterExecute`; let any component implement `after_parse(result)` / `after_execute(result)`; `agent.py:282` runs `await self.run_pipeline(AfterParse.after_parse, result)` to dispatch all of them. Reflexion thus becomes "an `AfterParse` hook," not "a special Loop." The Go version needs to reinvent that seam.

## Solution

Introduce `Pipeline` — an **ordered registry of callbacks** with two mount points: `AfterParseHook` fires after `strategy.Parse` and before the permission gate; `AfterExecuteHook` fires after `tool.Execute` and before `history.Append`. Both hook types receive **pointers to proposal/result**, so a hook can **mutate in place**. The first hook to return a non-nil error halts the chain and bubbles up to Loop.

`ReflexionStrategy` is the first client of this seam. Its `BuildPrompt` / `ParseResponse` simply delegate to a base strategy (default OneShot) — Reflexion does not change prompt construction. On **construction**, it registers an `AfterParseHook` on the Pipeline: each invocation issues a separate `Provider.CreateMessage` asking the model to reply with `{"sound": bool, "revised"?: ActionProposal}`, and rewrites Command/Args in place when `sound=false`. Loop has no idea Reflexion exists — it just runs the pipeline by contract — and that's the architectural payoff.

Three key design decisions:

1. **Hooks receive pointers; they don't return new values.** The contract is "**mutate in place** or return error," not "return a new struct." This lets multiple hooks chain — a validation hook checks first, a metrics hook records length, a Reflexion hook rewrites the command — all on the same `ActionProposal` instance.
2. **Pipeline is an optional Loop field.** When `Loop.Pipeline == nil` the whole subsystem is a no-op; every test from s01–s09 still passes inside the s10 module — zero-intrusion upgrade.
3. **Reflexion is both a Strategy and a hook registrar.** This dual role is intentional pedagogy: it shows that "a Strategy variant can reuse cross-cutting plumbing instead of cramming everything into the Strategy class." It's the Go-flavored expression of **composition over inheritance**.

## How It Works

```
                   ┌────────────────────────────────────────────────┐
                   │ Loop.runStep(ctx)                              │
                   │                                                │
   ┌───────────────│  msgs = strategy.BuildPrompt(history,...)      │
   │               │  resp = provider.CreateMessage(msgs)           │
   │               │  prop = strategy.ParseResponse(resp.Content)   │
   │               │                                                │
   │               │  pipeline.RunAfterParse(&prop)  ←── hook #1    │
   │               │       │                                        │
   │               │       ├── ReflexionHook: 2nd LLM, rewrite prop │
   │               │       ├── ValidationHook: reject malformed args│
   │               │       └── AuditHook: structured log            │
   │               │                                                │
   │               │  permissions.Check(prop) → Allow/Deny/Ask      │
   │               │  result = registry.Lookup(prop.Command).Execute│
   │               │                                                │
   │               │  pipeline.RunAfterExecute(&result) ── hook #2  │
   │               │       │                                        │
   │               │       ├── TruncateHook: cap web_fetch output   │
   │               │       ├── RedactHook: scrub API keys / PII     │
   │               │       └── MetricsHook: counter + latency       │
   │               │                                                │
   │               │  history.Append(Episode{prop, result})         │
   └───────────────└────────────────────────────────────────────────┘
```

Core 30-60 lines (excerpt from [`agents/s10-reflexion-hooks/pipeline.go`](https://github.com/Ding-Ye/learn-AutoGPT/blob/main/agents/s10-reflexion-hooks/pipeline.go)):

```go
type AfterParseHook func(ctx context.Context, proposal *ActionProposal) error
type AfterExecuteHook func(ctx context.Context, result *ActionResult) error

type Pipeline struct {
    afterParse   []AfterParseHook
    afterExecute []AfterExecuteHook
}

func (p *Pipeline) RegisterAfterParse(h AfterParseHook) {
    p.afterParse = append(p.afterParse, h)
}

func (p *Pipeline) RunAfterParse(ctx context.Context, prop *ActionProposal) error {
    if p == nil {
        return nil  // nil pipeline = pure no-op, by design
    }
    for i, h := range p.afterParse {
        if err := h(ctx, prop); err != nil {
            return fmt.Errorf("AfterParse hook %d: %w", i, err)
        }
    }
    return nil
}
```

ReflexionStrategy's hook registration (excerpt from [`strategy_reflexion.go`](https://github.com/Ding-Ye/learn-AutoGPT/blob/main/agents/s10-reflexion-hooks/strategy_reflexion.go)):

```go
func NewReflexionStrategy(base PromptStrategy, provider Provider, pipeline *Pipeline) *ReflexionStrategy {
    r := &ReflexionStrategy{base: base, provider: provider, pipeline: pipeline}
    if pipeline != nil {
        pipeline.RegisterAfterParse(r.afterParseHook)  // self-injection
    }
    return r
}

func (r *ReflexionStrategy) afterParseHook(ctx context.Context, prop *ActionProposal) error {
    if prop == nil || prop.Command == "" { return nil }
    question := r.buildReflexionPrompt(prop)
    resp, err := r.provider.CreateMessage(ctx, CreateMessageRequest{
        Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: question}}}},
    })
    if err != nil { return fmt.Errorf("reflexion second-pass: %w", err) }
    verdict, parseErr := parseReflexionVerdict(resp.Content)
    if parseErr != nil { return nil }  // garbled verdict shouldn't block the agent
    if !verdict.Sound && verdict.Revised != nil {
        prop.Command = verdict.Revised.Command       // mutate in place
        if verdict.Revised.Args != nil { prop.Args = verdict.Revised.Args }
        if verdict.Revised.Thoughts != "" { prop.Thoughts = verdict.Revised.Thoughts }
    }
    return nil
}
```

**4 non-obvious points**:

1. **Empty Pipeline is a legal no-op** — `(*Pipeline)(nil).RunAfterParse(...)` explicitly returns nil. The Loop never has to nil-check; every test from s09 passes unmodified inside s10.
2. **Verdict-parse failures don't block the main path** — when `parseReflexionVerdict` errors, the hook swallows the error and lets the original proposal through. Reason: an evaluator LLM occasionally emitting bad JSON should not crash the agent's whole turn. The test `TestReflexionStrategy_GarbledVerdictPassesThrough` pins this behavior.
3. **Reflexion is both a Strategy and a hook registrar** — the dual role is the design goal. Pass `-strategy=reflexion` and you get strategy substitution AND hook injection in one constructor.
4. **Pointer receivers compose naturally** — a validation hook can mutate args, then a Reflexion hook can rewrite the command, then an audit hook logs the final form — all stacking on the same `*ActionProposal` in registration order. This is Go's structural answer to Python's abstract base classes.

## What Changed (vs. s09)

```diff
 // loop.go
 type Loop struct {
     Provider    Provider
     Components  *ComponentBus
     Strategy    PromptStrategy
     History     *History
     Permissions *Permissions
     Asker       Asker
+    Pipeline    *Pipeline   // s10: optional cross-cutting hooks
     MaxTurns    int
     Verbose     bool
 }

 func (l *Loop) runStep(ctx context.Context, ...) (...) {
     // ... strategy.BuildPrompt + provider + strategy.ParseResponse ...
     proposal, err := strategy.ParseResponse(resp.Content)
     if err != nil { return ..., err }

+    if err := l.Pipeline.RunAfterParse(ctx, &proposal); err != nil {
+        return ..., err
+    }

     decision := l.Permissions.Check(proposal.Command, proposal.Args)
     // ... permission handling + tool.Execute ...

+    if err := l.Pipeline.RunAfterExecute(ctx, &result); err != nil {
+        return ..., err
+    }

     l.History.Current().Results = append(l.History.Current().Results, result)
 }
```

New files: `pipeline.go`, `pipeline_test.go`, `strategy_reflexion.go`, `strategy_reflexion_test.go`. `main.go` gains a `-strategy={oneshot|reflexion}` flag. All other infra files (provider/tools/registry/strategy/history/workspace/permissions/component/ui/interaction_loop) are byte-for-byte copies from s09.

Semantically: from this chapter on, **any logic that wants to observe or modify the propose↔execute boundary** (logging, metrics, redact, governance, reflexion) no longer has to touch Loop — it just registers an `AfterParseHook` / `AfterExecuteHook`. That decoupling is Pipeline's entire value proposition.

## Try It

```bash
export PATH="$HOME/sdk/go-1.26.3/bin:$PATH"
cd agents/s10-reflexion-hooks

# 1) Default oneshot strategy (Pipeline empty; behavior identical to s09)
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "echo hello"

# 2) Switch to reflexion — every proposal gets a 2nd-pass LLM evaluation
go run . -v -strategy=reflexion "Use the math tool to compute 7 * 13"

# 3) Tests
go test -v ./...
```

Expected output shape:

```
[s10-reflexion-hooks] provider=anthropic strategy=reflexion ...
💭 I'll use the math tool to compute 7 * 13.
[reflexion] second-pass: sound=true
✓ 91
```

When reflexion judges a proposal unsound, you'll see a rewrite:

```
💭 I'll execute `bash` with `rm -rf /tmp`.
[reflexion] second-pass: sound=false → revised to {command: echo, ...}
✓ (revised proposal output)
```

Tests cover 4 hook contracts + 5 reflexion behaviors: registration order, mutability, error propagation, nil-pipeline, empty Command skip, garbled-JSON tolerance, sound=true passthrough, sound=false-with-revised rewrite, provider-failure propagation.

## Upstream Source Reading

AutoGPT upstream's pipeline abstraction lives in two files: `forge/agent/protocols.py` (the hook ABC types) and `original_autogpt/autogpt/agents/agent.py:282` (the actual `run_pipeline` dispatch). Reflexion's implementation is in `original_autogpt/autogpt/agents/prompt_strategies/reflexion.py`, a 600+ line multi-phase strategy; we distill only its "second-pass evaluation with in-place rewrite" core, leaving "reflections persisted into episodic memory across turns" for the appendix exercise.

```upstream:classic/forge/forge/agent/protocols.py#L1-L46
from abc import abstractmethod
from typing import TYPE_CHECKING, Awaitable, Generic, Iterator

from forge.models.action import ActionResult, AnyProposal
from .components import AgentComponent

if TYPE_CHECKING:
    from forge.command.command import Command
    from forge.llm.providers import ChatMessage


class DirectiveProvider(AgentComponent):
    def get_constraints(self) -> Iterator[str]: return iter([])
    def get_resources(self) -> Iterator[str]: return iter([])
    def get_best_practices(self) -> Iterator[str]: return iter([])

class CommandProvider(AgentComponent):
    @abstractmethod
    def get_commands(self) -> Iterator["Command"]: ...

class MessageProvider(AgentComponent):
    @abstractmethod
    def get_messages(self) -> Iterator["ChatMessage"]: ...

# === The two ABCs that anchor this chapter ===
class AfterParse(AgentComponent, Generic[AnyProposal]):
    @abstractmethod
    def after_parse(self, result: AnyProposal) -> None | Awaitable[None]: ...

class ExecutionFailure(AgentComponent):
    @abstractmethod
    def execution_failure(self, error: Exception) -> None | Awaitable[None]: ...

class AfterExecute(AgentComponent):
    @abstractmethod
    def after_execute(self, result: "ActionResult") -> None | Awaitable[None]: ...
```

```upstream:classic/original_autogpt/autogpt/agents/prompt_strategies/reflexion.py#L1-L60
"""Reflexion Prompt Strategy.

This strategy implements the Reflexion pattern from research including:
- Reflexion: Verbal Reinforcement Learning (arxiv.org/abs/2303.11366)
- Self-Refine: Iterative Self-Feedback (arxiv.org/abs/2303.17651)
- Self-Reflection in LLM Agents (arxiv.org/abs/2405.06682)

Key benefits:
- 91% pass@1 on HumanEval (vs GPT-4's 80%)
- No training required - same LLM generates, critiques, refines
- Agents store reflections in episodic memory for better future decisions
- Supports 8 types of self-reflection that improve problem-solving

Pattern:
1. GENERATE: Propose action
2. EXECUTE: Run action
3. REFLECT: Critique result, extract lessons
4. RETRY: Use reflection to improve next attempt
"""

class ReflexionPhase(str, Enum):
    PROPOSING = "proposing"
    REFLECTING = "reflecting"

class EvaluationResult(ModelWithSummary):
    success: bool
    score: Optional[float]   # 0..1
    feedback: str
```

**Reading notes**:

- **Python uses ABCs; Go uses function types**: upstream's `AfterParse` is an abstract base class; components inherit and implement `after_parse`; `run_pipeline` uses `getattr` to find them. Go has no reflection-based registration sugar, so we promote the callback itself to a type — `type AfterParseHook func(...)`. One less layer of abstraction, more explicit.
- **Upstream weaves reflexion into `propose_action`**: Reflection / EvaluationResult / ReflexionMemory / ReflexionPhase add up to a 600+ line multi-phase strategy (including cross-turn memory of reflections). We only do "second-pass evaluation + in-place rewrite" — the thinnest viable implementation. Extending it to "reflections live in History across turns" is exercise #1 in Appendix B.
- **Upstream also has an `ExecutionFailure` hook** (the exception path). We don't — Go's `ActionResult.Status: "error"` string already routes failures through `AfterExecuteHook`; a separate `ExecutionFailure` is a Python-exception artifact.
- **`async/await` vs Go ctx**: upstream hooks return `None | Awaitable[None]` (sync or async). Go threads `context.Context` through every call instead, with a uniform sync-with-cancel signature. More predictable.
- **Where JSON-tolerance lives**: upstream's `extract_dict_from_json` is a global utility shared across strategies. We keep the lenient parser inside `parseReflexionVerdict` because reflexion is the only caller — pulling it out is fair game once a second user appears, KISS until then.

**Read further**: start at `forge/agent/protocols.py` for the ABC definitions, follow `agent.py:282`'s `run_pipeline` call into `forge/agent/components.py` for the `getattr` reflection registration, then read `prompt_strategies/reflexion.py` to see how `ReflexionMemory` and `EvaluationResult` persist reflections into episodic memory. That trace is the s10 → s_full → Appendix B exercise #1 source map.

---

**Next**: s_full doesn't write new code — it stitches all ten chapters' parts into a 16-step end-to-end execution trace, showing how a real upstream task runs through our mini.
