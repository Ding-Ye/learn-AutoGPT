---
title: "Appendix A · Classic vs Modern agent architectures"
chapter: appendix-a
slug: appendix-a-classic-vs-modern
est_read_min: 14
---

# Appendix A · Classic vs Modern agent architectures

> A mental-model essay, not a code chapter. Read this after s01–s_full and you'll understand what AutoGPT classic was in 2023, why Significant-Gravitas pivoted to Platform, and how modern frameworks (LangGraph, OpenAI Agents SDK, Anthropic Agents SDK) answer the same questions differently.

---

## 1. The 2023 "autonomous-agent moment"

In April 2023, AutoGPT racked up 100k+ stars on GitHub in a few days. It wasn't because the underlying algorithm was novel — the `ReAct` paper (reasoning + acting) had landed in October 2022 — but because it packaged **a complete think→act→observe loop + a custom command system** as a Python program any developer could run.

Why that moment? Three things collided:

1. **GPT-4** released, the first time an LLM was reliable enough at multi-step planning without fine-tuning.
2. **OpenAI function-calling** turned structured tool invocation from a prompt-engineering hack into a first-class API.
3. **OSS Python**: early AutoGPT was under 2k LoC; an undergrad could fork it on a weekend.

But the 2023 optimism — "let GPT-4 run autonomously for 24 hours and solve any problem" — hit walls fast:

- multi-step planning hallucinates; intermediate state gets lost
- context windows grow linearly with history; overflow within a few turns
- shell command execution without a sandbox is extremely dangerous
- success rate on long-horizon tasks dropped under 50% (AGBenchmark data)

These are **the problems every later agent framework has been solving**. AutoGPT classic was the first draft of that problem list, which is why every mechanism has a sharp "why this exists" — exactly what makes it a great teaching subject.

## 2. AutoGPT classic's signature bets

Treat classic as the 2023 "agent design encyclopedia." It made several distinctive bets:

**a. Eight prompt strategies coexisting**: `one_shot / rewoo / plan_execute / reflexion / tree_of_thoughts / lats / multi_agent_debate / base`. Each is a `PromptStrategy` subclass — the same agent code can hot-swap strategies.

The bet was "**no single best strategy; different tasks want different reasoning shapes**." In hindsight, this bet didn't pan out — from 2024 onward virtually every production framework converged on "one strong baseline + tool/data augmentation," because maintaining 8 strategies costs much more than 1 strategy + a properly abstracted tool layer.

**b. Plugin-style abilities**: the `@command` decorator turns any Python function into an agent tool; the `plugins/` directory supports runtime loading of third-party packages.

The bet was "**ecosystem breadth > kernel strength**." In hindsight, this bet was right, but the form changed — MCP (Model Context Protocol, 2024+) standardized "plugin-style abilities" across processes more cleanly than AutoGPT's plugin loader.

**c. EpisodicActionHistory + automatic summarization**: each turn's action+result is wrapped as an Episode; `prepare_messages()` is a hook that calls the LLM to summarize old episodes when the context overflows.

The bet was "**memory is a first-class concern for agents**." In hindsight, this bet was right, but the implementation went toward vector memory + retrieval (chromadb / pinecone) rather than "summarize via LLM every time."

**d. Eight LLM providers via litellm**: `MultiProvider` keeps OpenAI, Anthropic, Groq, Llamafile etc. in a single dict.

The bet was "**multi-LLM is the steady state**." In hindsight, this bet was completely correct — which is why the learn-AutoGPT s01 ships the 8 profile picker straight from the bootstrap (Phase G's contribution).

## 3. Why Significant-Gravitas pivoted to Platform

In 2024, the team moved core effort to `autogpt_platform/` (under Polyform Shield). Platform is a graph executor + visual builder: users drag nodes in a web UI; each node is an LLM / tool / decision; the runtime is a concurrent graph engine instead of a single-process loop.

The pivot wasn't because classic was failing — it was because classic had hit **architectural ceilings**:

- **Visual builders are growth leverage**. The TAM for CLI agents is far smaller than "drag-and-drop no-code workflows."
- **Enterprise deployments need multi-tenancy + audit + scheduling**. Classic's single-process Python loop model doesn't naturally support any of those.
- **Graph topology** makes parallel sub-agents / conditional branches / cycle-back loops first-class — prompt strategies can't get there.
- **License pivot**: Polyform Shield restricts commercial redistribution, leaving room for monetization. The MIT classic doesn't allow such restrictions.

The learning repo **leaves Platform alone** — the license is one reason; the other is that the teaching value of a graph executor lies in workflow engines, not agent design, and crossing that boundary makes the curriculum incoherent.

## 4. How modern alternatives are designed

Four representative modern agent frameworks:

**LangGraph** (LangChain offshoot, MIT)

- Model: **explicit state machine**. The developer defines nodes, edges, state fields; the framework executes the graph.
- Pros: visualizable flow (mermaid diagrams); natural state persistence; parallel/conditional paths first-class.
- Cons: graph definition has high boilerplate; dynamic behavior (LLM picks the next node) requires defining nodes as routers.
- vs. classic: classic gives the LLM full decision power on every "pick a tool" step; LangGraph splits decision power between graph structure and LLM — the LLM only chooses the next node at router nodes.

**OpenAI Agents SDK** (released 2024-12, MIT)

- Model: **handoff + tool**. An Agent is `instructions + tools + handoff_to_agents`; at runtime the LLM chooses between tool calls and handoffs.
- Pros: minimal; deeply integrated with the OpenAI Responses API; guardrails / tracing built in.
- Cons: handoff is an implicit graph — debugging is harder than with an explicit graph.
- vs. classic: classic's tool list is fully static; Agents SDK's handoff is an "agent-level tool," naturally supporting multi-agent collaboration.

**Anthropic Agents SDK / Claude Agent SDK** (2024-12+, MIT/Apache)

- Model: **the same as Claude Code**. An Agent is a conversation + tool-use loop + optional sub-agents; deeply interoperable with MCP servers.
- Pros: the tool protocol (content-block tagged union) and prompt caching are first-class; sub-agent context-window isolation is natural.
- Cons: heavily Anthropic-shaped; multi-model support is less clean than OpenAI Agents SDK.
- vs. classic: classic uses OpenAI function-calling shape, with AnthropicProvider added later; Agents SDK reverses this — content-blocks are native, OpenAI-compat is an adapter. **learn-AutoGPT's type catalog borrows this shape.**

**LlamaIndex Agents / HypothesisWorkflow**

- Model: **event-driven + DAG**. Events trigger steps; steps pass typed events between each other.
- Pros: long-running tasks (hours-to-days) recover naturally; steps decoupled; visualization good.
- Cons: steep learning curve; over-engineered for short-task agents.
- vs. classic: classic is a synchronous loop; Workflow is an async event bus.

## 5. When you'd still pick classic-style today

Not every scenario needs a graph engine. Classic style (single-process loop + tool registry + prompt strategy) is still the right pick when:

- **Teaching**: classic's design choices have clear "why this way" stories; modern frameworks tend to be a mix of problem-driven and engineering-driven decisions, pedagogically harder to unpack. **This is exactly why learn-AutoGPT exists.**
- **Quick prototypes**: write a small agent that solves problem X, without paying for a graph engine.
- **CLI / local dev tools**: Claude Code / aider / cursor are all classic-style loops at their core (each with extensive optimization).
- **Borrowing the "agent pattern" in other domains**: when you see think-act-observe in RL / planning / control systems, you'll recognize it as the universal idiom classic agents popularized.

---

If reading this section gives you the urge to "I want to implement a modern framework from scratch too," exercises #3 and #4 in Appendix B give two concrete paths to extend learn-AutoGPT into an Agent Protocol REST server + dynamic plugin loader.
