---
title: "Appendix B · Upstream source-reading map"
chapter: appendix-b
slug: appendix-b-upstream-map
est_read_min: 12
---

# Appendix B · Upstream source-reading map

> A guide to reading the upstream AutoGPT classic source. You've reimplemented the key mechanisms in Go; now move that understanding into the Python upstream — then push the mini toward production-ready with 5 exercises.

---

## 1. The big map of upstream `classic/`

```
classic/
├── original_autogpt/                    [the core agent application]
│   ├── autogpt/
│   │   ├── app/
│   │   │   ├── main.py            ← run_interaction_loop, CLI entry
│   │   │   ├── cli.py             ← Click command router
│   │   │   ├── config.py          ← AppConfig (smart_llm/fast_llm/...)
│   │   │   └── ui/                ← Rich-based terminal rendering
│   │   ├── agents/
│   │   │   ├── agent.py           ← Agent class + propose_action + execute
│   │   │   └── prompt_strategies/
│   │   │       ├── base.py        ← PromptStrategy ABC
│   │   │       ├── one_shot.py    ← default strategy
│   │   │       ├── reflexion.py   ← reflection strategy (600+ lines)
│   │   │       ├── rewoo.py
│   │   │       ├── plan_execute.py
│   │   │       ├── tree_of_thoughts.py
│   │   │       ├── lats.py
│   │   │       └── multi_agent_debate.py
│   │   └── plugins/               ← third-party plugin loader
│   ├── tests/                     ← integration/ + unit/
│   └── .env.template              ← config template
│
├── forge/                               [agent framework reused by original_autogpt]
│   ├── forge/
│   │   ├── agent/
│   │   │   ├── base.py            ← BaseAgent
│   │   │   ├── components.py      ← AgentComponent + run_pipeline (reflection registration)
│   │   │   └── protocols.py       ← AfterParse / AfterExecute / Command/Directive/MessageProvider
│   │   ├── command/
│   │   │   ├── command.py         ← Command class
│   │   │   ├── decorator.py       ← @command decorator
│   │   │   └── parameter.py       ← CommandParameter
│   │   ├── llm/providers/
│   │   │   ├── multi.py           ← MultiProvider + lazy provider cache
│   │   │   ├── openai.py
│   │   │   ├── anthropic.py
│   │   │   └── groq.py
│   │   ├── components/
│   │   │   ├── action_history/    ← Episode + EpisodicActionHistory
│   │   │   ├── file_manager/
│   │   │   ├── code_executor/
│   │   │   ├── web/
│   │   │   ├── system/
│   │   │   └── ... (19 subdirs total)
│   │   ├── file_storage/
│   │   │   ├── base.py            ← FileStorage ABC + restrict_to_root
│   │   │   ├── local.py           ← LocalFileStorage + _sanitize_path
│   │   │   ├── s3.py
│   │   │   └── gcs.py
│   │   └── permissions.py         ← CommandPermissionManager + ApprovalScope
│   └── tests/
│
├── direct_benchmark/                    [benchmark harness]
└── README.md / SECURITY.md / TROUBLESHOOTING.md
```

## 2. Suggested reading order (12 files)

Read upstream in the same order as the learn-AutoGPT chapters, with one pointer per file:

| # | Upstream file | Maps to | What to read |
|---|---|---|---|
| 1 | `classic/README.md` | (orientation) | project pitch + the deprecation note |
| 2 | `original_autogpt/autogpt/app/main.py:655-833` | s01, s09 | `run_interaction_loop` + signal handler + cycle decrement |
| 3 | `original_autogpt/autogpt/agents/agent.py:1-100` | s01 | the Agent class fields and imports — every component lands here |
| 4 | `original_autogpt/autogpt/agents/agent.py:270-300` | s01, s10 | the `create_chat_completion` call inside `propose_action` + `run_pipeline(AfterParse.after_parse, result)` |
| 5 | `forge/forge/command/command.py` + `decorator.py` | s02 | `@command` decorator implementation + Command class + parameter validation |
| 6 | `forge/forge/llm/providers/multi.py` | s03 | `MultiProvider.CHAT_MODELS` + lazy `_provider_instances` cache + `get_model_provider` |
| 7 | `original_autogpt/autogpt/agents/prompt_strategies/one_shot.py` | s04 | `OneShotAgentPromptStrategy.build_prompt` + `parse_response_content` |
| 8 | `forge/forge/components/action_history/action_history.py` + `model.py` | s05 | `Episode` dataclass + `EpisodicActionHistory.prepare_messages` (with the LLM-summarization hook) |
| 9 | `forge/forge/file_storage/base.py` + `local.py` | s06 | `FileStorage` ABC + `LocalFileStorage._sanitize_path` |
| 10 | `forge/forge/permissions.py` | s07 | `CommandPermissionManager.check_command` + `_pattern_matches` + the 4-level `ApprovalScope` enum |
| 11 | `forge/forge/agent/protocols.py` + `forge/components/file_manager/__init__.py` | s08 | the three protocol ABCs + one concrete component implementation |
| 12 | `original_autogpt/autogpt/agents/prompt_strategies/reflexion.py` | s10 | the full 600+ line `ReflexionPromptStrategy` — the Reflexion paper implementation |

After files 1-3 you have a holistic feel for the upstream agent; by file 7 you can run it end-to-end mentally; after file 12 you've completed the mini → upstream side-by-side reading.

## 3. Five advanced exercises (push the mini toward production)

Each exercise starts from a learn-AutoGPT Go session, reuses ~80% of the code, and adds 20% new content.

### Exercise #1: cross-turn Reflexion with episodic memory

**Goal**: extend s10's single-turn reflection into a version where reflections persist into history and are retrieved across turns.

**Starting point**: copy `agents/s10-reflexion-hooks/` to `agents/sX-reflexion-memory/`.

**Changes**:
- Add `Reflections []string` to `Episode`.
- ReflexionStrategy.afterParseHook archives `verdict.Reason` as a reflection.
- `Strategy.BuildPrompt` renders all reflections in history as a `<past_lessons>` block in the system message.
- Test: run the same buggy tool 3 times; on the 3rd turn, the proposal should be rewritten thanks to the previous two reflections.

**Upstream reference**: `prompt_strategies/reflexion.py`'s `ReflexionMemory` and `Reflection.persist()`.

### Exercise #2: vector memory backend

**Goal**: hook s05's `History.RenderMessages` to a local vector store, switching from chronological-rendering to semantic retrieval.

**Starting point**: copy `agents/s05-episodic-history/`; pull in `github.com/coder/hnsw` (Go's HNSW library).

**Changes**:
- `History.Append` asynchronously embeds the episode text and inserts into HNSW.
- Change `RenderMessages` signature to `RenderMessages(query string)`; do KNN top-k by query.
- The strategy passes the current task as the query.
- Test: build 100 episodes, verify retrieval recall > 0.8.

**Upstream reference**: `forge/components/memory/`'s ChromaDB / Pinecone adapters (which are mostly stubs in forge — read the docs).

### Exercise #3: Agent Protocol REST server

**Goal**: wrap an HTTP service so external clients submit tasks and poll status via REST, implementing [Agent Protocol](https://github.com/AI-Engineer-Foundation/agent-protocol).

**Starting point**: copy `agents/s09-continuous-mode/`; pull in `net/http` + `github.com/go-chi/chi/v5`.

**Changes**:
- POST `/ap/v1/agent/tasks` creates a task (body: `{input: string}`), starts a `RunInteractionLoop` goroutine.
- GET `/ap/v1/agent/tasks/{id}` returns status + history.
- POST `/ap/v1/agent/tasks/{id}/steps` advances one step explicitly (for manual mode).
- Test: use a subset of the official Agent Protocol test suite.

**Upstream reference**: `original_autogpt/autogpt/app/serve.py` + the FastAPI routes.

### Exercise #4: dynamic plugin loader

**Goal**: replace s08's hard-coded component list with a runtime mechanism that scans `./plugins/` for `.so` plugins.

**Starting point**: copy `agents/s08-components/`; use Go's `plugin` package (note: Linux/Mac only).

**Changes**:
- Each plugin is a `.so` exposing `New() Component`.
- At startup `main.go` does `filepath.Walk("./plugins/")` and for each `.so` calls `plugin.Open` + `Lookup("New")`.
- The returned Component is registered in the ComponentBus.
- Write a sample plugin (separate module) that implements `CommandProvider` and emits a `weather` tool.
- Test: plugin load order; broken plugin doesn't crash startup.

**Upstream reference**: `original_autogpt/plugins/`'s Python plugin loader (using `importlib` + entry_points).

### Exercise #5: full 4-level permission scope

**Goal**: upgrade s07's 2-level (Allow/Deny/Ask) to upstream's full 4-level (`ONCE` / `AGENT` / `WORKSPACE` / `DENY`).

**Starting point**: copy `agents/s07-permissions/`.

**Changes**:
- Add four `Decision` values.
- Add `Permissions.Save(scope Scope)` to persist ask answers to `~/.autogpt/agents/{id}/permissions.yaml` (agent scope) or `./permissions.yaml` (workspace scope).
- StdinAsker presents 4 options (One time / This agent / This workspace / Deny forever).
- Test: with scope=AGENT, a second `Check` doesn't re-ask; with scope=ONCE, it does.

**Upstream reference**: `forge/forge/permissions.py`'s `ApprovalScope` enum and `_save_permission` method.

---

## 4. Further reading

- AutoGPT Platform docs (out of our derivative scope, but readable): https://docs.agpt.co/
- LangGraph tutorials: [langchain-ai/langgraph](https://github.com/langchain-ai/langgraph)
- Anthropic Agents SDK / Claude Agent SDK docs: https://docs.claude.com/en/docs/claude-code
- Reflexion paper: https://arxiv.org/abs/2303.11366
- ReAct paper: https://arxiv.org/abs/2210.03629
