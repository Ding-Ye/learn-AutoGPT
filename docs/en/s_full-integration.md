---
title: "s_full · End-to-end integration"
chapter: full
slug: s_full-integration
est_read_min: 18
---

# s_full · End-to-end integration

> No new code in this chapter. It stitches the parts from s01–s10 into a 16-step trace of one canonical upstream use case, lifting your understanding from "I get each mechanism" to "I get how an agent runs from a user prompt to a finished artifact."

---

## Problem

After s01–s10 you have a clean mental model of each individual mechanism — but the way the ten of them collaborate during one execution is still fuzzy. Concretely:

- Multi-step tool calls, the permission gate, Reflexion second-pass, history append, UI output — **what is the precise temporal order of these events?**
- The `messages` list on each LLM request is assembled from which components?
- When the agent is interrupted by SIGINT mid-execution, how does state roll back / persist?
- When upstream AutoGPT runs a real task ("research recent LLM evaluation papers, save the summary as markdown"), what are the 16 steps it goes through? Where does our mini line up, and where did we trim?

s_full's job is to pack all of these answers into **one diagram + one 16-step trace table**.

## Solution

Treat the agent as a 4-layer composition:

1. **Entry layer**: `main.go` + `RunInteractionLoop` (s09)
2. **Decision layer**: `Loop.runStep` orchestrating Provider + Strategy (possibly wrapped by Reflexion) + Pipeline (s10)
3. **Capability layer**: `ComponentBus` → `Registry` → `Tools` (s08, s06, s02)
4. **State layer**: `History`, `Workspace`, `Permissions`, `Asker` (s05, s06, s07)

Each step of execution is one traversal across these four layers — and each mechanism you learned corresponds to **one abstraction point in one of the layers**. Reflexion is an `AfterParseHook` registered (s10) by `ReflexionStrategy`; it fires in the middle of the decision layer; it lands in history and becomes part of the state layer; its prompt reuses the directives that components emit through the strategy. All those relationships show up in the 16-step trace below.

## How It Works

The runtime composition relationship across all parts:

```
                          ┌─ User CLI flags / env / -strategy / -cycles
                          │
                          v
   ┌──────────────────────────────────────────────────────────────────┐
   │ main.go                                                          │
   │   • providerProfiles[8] → Provider concrete (Anthropic|OpenAI…)  │
   │   • LocalWorkspace("./workspace/")                               │
   │   • Components = [FileMgr(ws), WebFetch()]                       │
   │   • Permissions ← permissions.json | defaults                    │
   │   • Pipeline = NewPipeline()                                     │
   │   • Strategy = OneShot OR Reflexion(OneShot, provider, pipeline) │
   │   • History = &History{}                                         │
   │   • Loop{Provider, Components, Strategy, History,                │
   │           Permissions, Asker(stdin), Pipeline,                   │
   │           MaxTurns, Verbose}                                     │
   │   • RunInteractionLoop(ctx, loop, ConsoleUI, LoopOpts)           │
   └────────────────────────────┬─────────────────────────────────────┘
                                │
                                v  per-step (cycle counter ↓ on success)
   ┌──────────────────────────────────────────────────────────────────┐
   │ Loop.runStep(ctx)                                                │
   │   1. registry  = Components.Registry()                           │
   │   2. directives = Components.Directives()                        │
   │   3. msgs = Strategy.BuildPrompt(History, registry.All(),        │
   │                                  directives, userTask)           │
   │   4. resp = Provider.CreateMessage(msgs)                         │
   │   5. proposal, err = Strategy.ParseResponse(resp.Content)        │
   │                                                                  │
   │   6. Pipeline.RunAfterParse(&proposal)        ◀─ Reflexion fires │
   │                                                                  │
   │   7. decision = Permissions.Check(proposal.Command, .Args)       │
   │       ├── Allow → continue                                       │
   │       ├── Deny  → result = {Status:"error", Output:"denied"}     │
   │       └── Ask   → Asker.Ask(...) → Allow|Deny                    │
   │                                                                  │
   │   8. tool, ok = registry.Lookup(proposal.Command)                │
   │   9. output, err = tool.Execute(ctx, proposal.Args)              │
   │  10. result = ActionResult{Status, Output}                       │
   │                                                                  │
   │  11. Pipeline.RunAfterExecute(&result)                           │
   │                                                                  │
   │  12. History.Append(Episode{Actions:[proposal], Results:[result]}│
   │  13. UI.RenderThought + UI.RenderResult                          │
   └──────────────────────────────────────────────────────────────────┘
```

## What Changed (vs. s10)

s_full ships no new code. It produces only **the trace table + omissions table** as documentation:

```diff
+ docs/zh/s_full-integration.md
+ docs/en/s_full-integration.md   ← you are here
```

If you scroll back to the README curriculum table, every row is now ✅.

## Try It

Walk through this 16-step trace — the canonical upstream use case "research recent LLM evaluation papers and write a markdown summary":

| Step | Who | Does what | Sessions / files involved |
|---|---|---|---|
| 1 | User | `go run . -strategy=reflexion -cycles 10 -v "research recent LLM eval papers, write to ./summary.md"` | main.go (inherits from s01–s10) |
| 2 | main | Parse `-provider` → Anthropic; read ANTHROPIC_API_KEY | main.go (s01) |
| 3 | main | Construct LocalWorkspace("./workspace/"); auto-mkdir | s06 workspace.go |
| 4 | main | Build [FileMgr(ws), WebFetch(30s)] → ComponentBus | s08 component.go + component_filemgr.go + component_web.go |
| 5 | main | Load ./permissions.json (allow web_fetch / read_file / write_file) | s07 permissions.go |
| 6 | main | Construct Pipeline + ReflexionStrategy(OneShot, provider, pipeline) | s10 pipeline.go + strategy_reflexion.go |
| 7 | RunInteractionLoop | for cycles_remaining { ... }; UI.Spinner("Thinking...") | s09 interaction_loop.go |
| 8 | Strategy.BuildPrompt | render system message: tool schemas (web_fetch, read_file, write_file) + directives ("read before write") + userTask | s04 strategy.go + s08 component.go (Directives) |
| 9 | Provider.CreateMessage | POST to Anthropic, Content-Type / x-api-key headers, await response | s01 provider.go |
| 10 | Strategy.ParseResponse | extract tool_use ContentBlock → ActionProposal{Command:"web_fetch", Args:{"url":"https://arxiv.org/list/cs.CL/recent"}} | s04 strategy.go |
| 11 | Pipeline.RunAfterParse | Reflexion hook fires: 2nd-pass LLM eval → "sound": true → no rewrite | s10 strategy_reflexion.go |
| 12 | Permissions.Check | pattern "web_fetch: *" matches the allow list → Allow | s07 permissions.go |
| 13 | Registry.Lookup("web_fetch").Execute | http.Get(url, 30s timeout); truncate to 8KB | s08 component_web.go |
| 14 | Pipeline.RunAfterExecute | (no AfterExecute hook registered in this example) → passes through | s10 pipeline.go |
| 15 | History.Append | Episode{Actions:[proposal], Results:[result]} archived | s05 history.go |
| 16 | UI | RenderThought + RenderResult written to stderr; cycle counter -- | s09 ui.go |

Subsequent cycles (cycle 2 onward) follow the same flow, except Strategy.BuildPrompt now receives a **populated** History — s05's `RenderMessages` translates last turn's web_fetch into a [user, assistant, user] structure for the LLM; the model decides the next step accordingly. This continues until the model emits `end_turn` (no more tool_use), at which point RunInteractionLoop exits and the final output flows to ConsoleUI.

## Upstream Source Reading

### Deliberate omissions

| Upstream has | We don't | Why |
|---|---|---|
| 8 Prompt strategies (one_shot / rewoo / reflexion / plan_execute / tree_of_thoughts / lats / multi_agent_debate / base) | Only ship one_shot; reflexion as a Strategy wrapper | Pedagogically "8 competing strategies" is overload; s10's "Strategy can be wrapped" demonstration is sufficient |
| Vector memory backends (chromadb / pinecone / weaviate / redis) | None | Learning-path priority; vector memory is an implementation detail of history compression — see Appendix B exercise #2 |
| Plugin loader (`original_autogpt/plugins/`) | None | Component system (s08) demonstrates the plugin idea; dynamic loading is Appendix B exercise #4 |
| Telemetry / analytics opt-in | None | A teaching repo doesn't need telemetry |
| Agent Protocol REST server (`serve` mode + FastAPI routes) | None | See Appendix B exercise #3 |
| `_execute_tools_parallel` parallel tool dispatch | Sequential | One tool_use per turn already demonstrates the protocol; parallel is a perf optimization |
| AppConfig + AgentSettings persistence (.autogpt/agents/{id}/state.json) | Single-process in-memory | Cross-process resume is an engineering problem, not a teaching point |
| AIProfile (persisted name/role/goals) | A single prompt string | Persisting persona doesn't change the loop's shape |
| 4-level permission scopes (ONCE/AGENT/WORKSPACE/DENY) | 2-level (Allow/Deny/Ask) | Simpler to read; full 4-level is Appendix B exercise #5 |
| ReflexionMemory cross-turn reflection persistence | Single-turn second-pass eval only | We demonstrate the hook mechanism; cross-turn reflection is the s10 + s05 advanced combo, in Appendix B exercise #1 |

**Reading notes**:

- **Whole-system mental picture**: upstream's `agent.py` is 542 lines that mash everything into one `Agent` class. We split it across 10 modules of ≈300 lines each. When you read upstream, treat `agent.py` as "the union of all sNN modules you've already read."
- **Entry point**: upstream's `app/main.py:run_interaction_loop` corresponds to our s09 `RunInteractionLoop`. The difference is that upstream uses Rich for animated UI; we use plain ANSI line output.
- **Reflexion is a standalone strategy in upstream**, but a strategy + hook registrar in ours — Go's composition expression is more explicit.
- **Wire-format shape**: upstream uses OpenAI function-calling shape on the wire; our internal types use Anthropic content-block shape; s03's OpenAIProvider does the wire translation at the provider boundary — this is **why our Loop code is ~30% shorter than upstream's**.

**Read further**: from the README curriculum, every ✅ link leads to a chapter doc with its own "Read further" pointer at the end; Appendix B gives the canonical full upstream-reading order.
