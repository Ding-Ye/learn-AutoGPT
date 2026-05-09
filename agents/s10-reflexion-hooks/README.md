# s10 — Reflexion 与 AfterParse hooks · Reflexion & AfterParse pipeline

> 教什么 / What this teaches: `Pipeline` registry of `AfterParseHook` / `AfterExecuteHook` callbacks; `ReflexionStrategy` as a strategy wrapper that registers a 2nd-pass LLM evaluator on construction. The architectural punchline of the whole curriculum — composition over inheritance, hooks decouple cross-cutting concerns from Strategy.

## 文件 / Files

```
agents/s10-reflexion-hooks/
├── go.mod
├── pipeline.go                 ← NEW: AfterParseHook + AfterExecuteHook + Pipeline
├── pipeline_test.go            ← NEW: order, mutation, halt-on-error, nil-safety
├── strategy_reflexion.go       ← NEW: ReflexionStrategy wraps a base Strategy
├── strategy_reflexion_test.go  ← NEW: delegation, sound passthrough, revised mutation
├── loop.go                     ← MODIFIED: integrates Pipeline.RunAfterParse / RunAfterExecute
├── loop_test.go                ← inherited from s09 (still passes — Pipeline is optional)
├── main.go                     ← `-strategy=oneshot|reflexion` flag added
├── README.md                   ← this file
├── strategy.go / strategy_test.go ── verbatim from s09 (BuildPrompt with directives)
├── provider.go / provider_openai.go / provider_mock.go / *_test.go
│                              ── verbatim from s09 (Anthropic native + OpenAI-compat + mock)
├── tools.go / tools_file.go / tools_test.go / tools_file_test.go
│                              ── verbatim from s09 (Echo, Math, Read/Write file)
├── registry.go / registry_test.go ── verbatim from s09
├── history.go / history_test.go   ── verbatim from s09
├── workspace.go / workspace_test.go ── verbatim from s09
├── permissions.go / permissions_test.go ── verbatim from s09
├── component.go / component_filemgr.go / component_web.go / *_test.go
│                              ── verbatim from s09
├── ui.go / ui_test.go              ── verbatim from s09
├── interaction_loop.go / _test.go  ── verbatim from s09 (RunInteractionLoop wrapper)
└── testdata/
    ├── golden_response.json
    ├── golden_response_json_fallback.json
    └── permissions.yaml
```

## 跑起来 / Run

```bash
export PATH="$HOME/sdk/go-1.26.3/bin:$PATH"
cd agents/s10-reflexion-hooks

# 1) Default: oneshot strategy, empty Pipeline → behaves like s09
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "echo hello"

# 2) Reflexion: every proposal gets a 2nd-pass LLM evaluation
go run . -v -strategy=reflexion "Use the math tool to compute 7 * 13"

# 3) Different provider profile (8 supported via Phase G addendum)
export DEEPSEEK_API_KEY=sk-...
go run . -v -strategy=reflexion -provider deepseek "say hi"

# Tests
go test -v ./...
```

## 关键教学点 / Key teaching points

1. **Hooks accept *pointers*, mutate in place** — multiple hooks chain on the same `*ActionProposal` instance (validation → reflexion → audit). The contract is "mutate or return error," not "return a new struct."
2. **`*Pipeline = nil` is a legal no-op** — Loop never has to nil-check; tests inherited from s01–s09 pass unchanged.
3. **Reflexion is both a Strategy and a hook registrar** — `NewReflexionStrategy(base, provider, pipeline)` self-injects an `AfterParseHook` on construction. Loop has no idea Reflexion exists.
4. **Garbled second-pass JSON ≠ blocking** — when the evaluator LLM emits malformed JSON, the hook silently passes the original proposal through. Tests pin this behavior (`TestReflexionStrategy_GarbledVerdictPassesThrough`).
5. **What's deliberately omitted** — upstream's `ReflexionMemory` (cross-turn reflection persistence) and `ReflexionPhase` state machine. Both are listed as Appendix B exercise #1.

## 文档 / Docs

- 中文: [`docs/zh/s10-reflexion-hooks.md`](../../docs/zh/s10-reflexion-hooks.md)
- English: [`docs/en/s10-reflexion-hooks.md`](../../docs/en/s10-reflexion-hooks.md)
- Upstream excerpt: [`upstream-readings/s10-reflexion-protocols.py`](../../upstream-readings/s10-reflexion-protocols.py)

This is the last session in the curriculum. Continue to [s_full integration](../../docs/zh/s_full-integration.md) for the 16-step end-to-end trace.
