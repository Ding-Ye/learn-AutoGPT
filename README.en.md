# learn-AutoGPT

> Build an AutoGPT-classic-style autonomous agent from scratch in Go, session by session — each chapter ends with the upstream Python source.

[中文版 / Chinese version](README.md)

---

## What is this

`Significant-Gravitas/AutoGPT` is the 2023 milestone autonomous-agent repo — wrapping GPT-4 in a *think → act → observe* loop where the model itself plans, calls tools, reads/writes files, and iterates to completion. At roughly 21M LOC across a Python backend, a TypeScript frontend, and several sub-projects, reading the whole thing in one sitting is not realistic.

**`learn-AutoGPT` decomposes the upstream `classic/` subtree (the original Python agent, MIT-licensed) into 10 progressively-built Go teaching sessions.** Each session adds one mechanism, each is a self-contained runnable `package main`, and each ends with a side-by-side reading of the Go mini and the AutoGPT upstream source.

We deliberately leave `autogpt_platform/` alone (Polyform Shield license, outside our derivative scope).

## Curriculum

| # | Chapter | Upstream mechanism | Status |
|---|---|---|---|
| s01 | [Minimal think→act→observe loop](docs/en/s01-minimal-loop.md) | `app/main.py:run_interaction_loop` + `agents/agent.py:propose_action` | ✅ |
| s02 | [Explicit command registry](docs/en/s02-command-registry.md) | `forge/command/decorator.py` (@command) | ✅ |
| s03 | [LLM provider with multiple backends](docs/en/s03-llm-provider.md) | `forge/llm/providers/multi.py` | ✅ |
| s04 | [Prompt strategies & response parsing](docs/en/s04-prompt-strategy.md) | `agents/prompt_strategies/one_shot.py` | ✅ |
| s05 | [Episodic action history](docs/en/s05-episodic-history.md) | `forge/components/action_history/` | ✅ |
| s06 | [Sandboxed workspace storage](docs/en/s06-workspace.md) | `forge/file_storage/local.py` | ✅ |
| s07 | [Layered permission system](docs/en/s07-permissions.md) | `forge/permissions.py` | ✅ |
| s08 | [Pluggable component system](docs/en/s08-components.md) | `forge/agent/protocols.py` + `forge/components/` | ✅ |
| s09 | [Continuous mode & UI feedback](docs/en/s09-continuous-mode.md) | `app/main.py:655-768` (cycle budget + signal) | ✅ |
| s10 | [Reflexion & AfterParse pipeline](docs/en/s10-reflexion-hooks.md) | `agents/prompt_strategies/reflexion.py` + `forge/agent/protocols.py` (AfterParse) | ✅ |
| s_full | [End-to-end integration](docs/en/s_full-integration.md) | (16-step trace) | ✅ |
| App. A | [Classic vs Modern agent architectures](docs/en/appendix-a-classic-vs-modern.md) | (mental model) | ✅ |
| App. B | [Upstream source-reading map](docs/en/appendix-b-upstream-map.md) | (reference) | ✅ |
| **M** | [**Multi-model guide** (DeepSeek / Qwen / Moonshot / self-hosted)](docs/en/multi-model.md) | (8 LLM profiles, 1-flag swap) | ✅ |

## Quickstart

```bash
# Go ≥ 1.22 + any LLM API key
git clone https://github.com/Ding-Ye/learn-AutoGPT.git
cd learn-AutoGPT/agents/s01-minimal-loop

# 1) Anthropic (default profile)
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "say hi via the echo tool"

# 2) Any OpenAI-compatible provider: OpenAI / DeepSeek / Qwen / Moonshot / Groq / OpenRouter / local vLLM
export DEEPSEEK_API_KEY=sk-...
go run . -provider deepseek -v "say hi via the echo tool"

# 3) Run tests
go test -v ./...
```

## Doc viewer

```bash
cd web
npm install
npm run dev    # http://localhost:3000
```

Bilingual (Chinese + English) side-by-side. Chapter sidebar. Upstream Python excerpt embedded at the end of each chapter.

## Pedagogy

Borrowed from [shareAI-lab/learn-claude-code](https://github.com/shareAI-lab/learn-claude-code), each chapter follows a six-section spine:

1. **Problem** — the gap this chapter opens
2. **Solution** — mental model (before any code)
3. **How It Works** — ASCII diagram + 30-60 lines of core code + non-obvious points
4. **What Changed** — diff from the previous chapter
5. **Try It** — copy-pasteable commands + expected output shape
6. **Upstream Source Reading** — real upstream excerpt, annotated

Each session is a self-contained Go module with **no cross-session imports** — you can read s01 in isolation as a 250-line "minimum viable agent", then diff against s02 to see exactly what was added.

## Acknowledgments

- Upstream: [Significant-Gravitas/AutoGPT](https://github.com/Significant-Gravitas/AutoGPT) (classic/ subtree, MIT)
- Pedagogy: [shareAI-lab/learn-claude-code](https://github.com/shareAI-lab/learn-claude-code)
- Generator: this repo was bootstrapped by the [learn-repo-generator skill](https://github.com/anthropics/claude-code)

## License

MIT — see [LICENSE](LICENSE). This learning repo is a Go re-implementation; no code is copied verbatim from upstream. Short excerpts under `upstream-readings/` are educational citations preserved from the upstream MIT source.
