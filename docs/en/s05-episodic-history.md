---
title: "s05 · Episodic action history"
chapter: 5
slug: s05-episodic-history
est_read_min: 12
---

# s05 · Episodic action history

> What this teaches: lift the Loop's implicit `[]Message` accumulator into an explicit `History []*Episode`. Every tool turn now appends an Episode that records `Actions []ActionProposal` + `Results []ActionResult`; the strategy's `BuildPrompt` rebuilds the conversation each turn via `RenderMessages()`. This is the minimum-viable take on AutoGPT upstream's `EpisodicActionHistory.prepare_messages`. Compression — where the LLM summarizes old episodes to fit a context window — is left as a single `// (advanced)` comment marking the seam.

---

## Problem

s04's Loop accumulated messages in a flat slice. Every turn appended one Message; rolled-over context never got shorter. That works for 5 turns, breaks at 50.

```go
// s04 Loop, simplified:
messages := strategy.BuildPrompt(nil, schemas, userPrompt)  // ← history is nil
for turn := 0; turn < l.MaxTurns; turn++ {
    resp, _ := l.Provider.CreateMessage(ctx, ...)
    messages = append(messages, Message{Role: "assistant", Content: resp.Content})
    // ... if tool_use, run tools and append a user-role tool_result message
    // ... if end_turn, return
}
```

Two specific things break as the agent runs longer:

1. **No compression seam.** When the conversation outgrows the model's context window, AutoGPT classic's `EpisodicActionHistory.handle_compression()` walks the older episodes (those past `full_message_count`) and asks the LLM to summarize each into a one-paragraph blob, replacing the verbose action+result pair. s04's flat slice has no granular boundary at which to "summarize this old chunk into a sentence" — every Message looks the same. We need *episodes* (action + result pairs) so summarization has a coherent unit to operate on.

2. **No structured history for strategies that consume it.** s04's `PromptStrategy.BuildPrompt(history []*Episode, ...)` already takes a `[]*Episode` parameter — but s04 always passed `nil`. The signature was a forward-compat seam. s05 actually populates it. Strategies that want to fold prior actions/results back into the prompt (every strategy except the trivial single-shot case) need this slice.

s05 fixes both with one move: introduce `Episode` and `History` types, give the Loop a `*History` field, and let the strategy call `History.RenderMessages()` to rebuild the conversation each turn.

## Solution

```go
type ActionResult struct {
    Status string // "ok" | "error" | "interrupted_by_human"
    Output string
}

type Episode struct {
    Actions []ActionProposal // 1+ proposals (parallel tool calls = same episode)
    Results []ActionResult   // result[i] pairs with action[i]
}

type History []*Episode

func (h *History) Append(ep *Episode)         // mutates in place
func (h *History) Current() *Episode          // last episode or nil
func (h *History) RenderMessages() []Message  // ← the compression seam
func (h History)  TrimToLastN(n int) History  // pedagogical helper, not used by Loop
```

Three exported types, four methods. Two of the methods (`Append`, `Current`) are slice operations dressed up as methods so the Loop reads naturally; `RenderMessages` is the load-bearing one — it converts an Episode list back into the alternating user/assistant flow the protocol expects.

`TrimToLastN` is a deliberate pedagogical inclusion: it shows where compression *would* fit, but the Loop never calls it. The plan calls this "the seam"; the test exercises it; you fill in the LLM-summarize-old-episodes call when your context overflows. Until then, the comment at the top of `history.go` is your only documentation:

```go
// (advanced) when context overflows, summarize old episodes here.
```

## How It Works

```ascii-anim frames=3
┌─────────────────────────────────────────────────────────────────────┐
│   Loop.Run(ctx, "compute 2+3, then echo the result")                │
│         │                                                           │
│         ▼                                                           │
│   if l.History == nil: l.History = &History{}                       │
│   schemas := l.Tools.All()                                          │
│   system  := strategy.BuildSystem(schemas)                          │
│         │                                                           │
│         ▼                                                           │
│   for turn = 0; turn < MaxTurns; turn++:                            │
│       msgs := strategy.BuildPrompt(*l.History, schemas, userPrompt) │
│              │                                                      │
│              ├─ history.RenderMessages()  ← rebuild prior turns     │
│              ├─ + Message{user, current task}                       │
│              ▼                                                      │
│       resp := provider.CreateMessage(req{System, msgs, Tools})      │
│              │                                                      │
│       case "tool_use":                                              │
│           ep := &Episode{}                                          │
│           history.Append(ep)             ← s05 new: per-turn append │
│           ep.Actions = append(ep.Actions, parsed proposal)          │
│           results := runTools(resp.Content)                         │
│           ep.Results = append(ep.Results, results...)               │
│       case "end_turn":                                              │
│           return text                                               │
└─────────────────────────────────────────────────────────────────────┘
```

### `RenderMessages` — the seam

```go
// (advanced) when context overflows, summarize old episodes here.
func (h *History) RenderMessages() []Message {
    out := make([]Message, 0, len(*h)*2)
    for _, ep := range *h {
        if ep == nil {
            continue
        }
        // assistant turn: free-text Thoughts + tool_use blocks
        assistantContent := make([]ContentBlock, 0, 2*len(ep.Actions))
        for i, a := range ep.Actions {
            if a.Thoughts != "" {
                assistantContent = append(assistantContent, ContentBlock{Type: "text", Text: a.Thoughts})
            }
            if a.Command != "" {
                assistantContent = append(assistantContent, ContentBlock{
                    Type:  "tool_use",
                    ID:    episodeActionID(ep, len(out), i),
                    Name:  a.Command,
                    Input: a.Args,
                })
            }
        }
        if len(assistantContent) > 0 {
            out = append(out, Message{Role: "assistant", Content: assistantContent})
        }
        // user turn: one tool_result block per Result; skip if mid-turn (no results yet)
        if len(ep.Results) == 0 {
            continue
        }
        userContent := make([]ContentBlock, 0, len(ep.Results))
        for i, r := range ep.Results {
            id := episodeActionID(ep, len(out)-1, i)
            userContent = append(userContent, ContentBlock{
                Type:        "tool_result",
                ToolUseID:   id,
                ToolContent: renderResult(r),
            })
        }
        out = append(out, Message{Role: "user", Content: userContent})
    }
    return out
}
```

If you ever want compression, the body of this method is where it goes. Walk `*h`, decide which episodes to compress (e.g. all except the last `full_message_count`), call `provider.CreateMessage` on each old episode asking for a summary, replace the rendered tool_use+tool_result pair with one summary text block. The Loop above doesn't change.

### Three non-obvious points

1. **`Append` is a pointer-receiver method, `Current` is too** — both must mutate or read the underlying slice header through the pointer. Otherwise `var h History; h.Append(ep)` would silently grow a *copy*. The s04 code used `messages = append(messages, ...)` everywhere; pulling that into a method gives you the same in-place semantics but lets the caller name the operation.

2. **The synthesized `tool_use_id`** — when the Loop appended a proposal to `ep.Actions`, it didn't carry the original `block.ID` from the wire response. RenderMessages fabricates a stable id like `"ep<msgIndex>_act<i>"` so the matching tool_result block uses the same value. This is *deliberate*: the original wire id is an implementation detail of the assistant turn that was. Reconstructing the conversation from logical actions is the whole point of episodic history.

3. **Empty history returns `[]Message{}`, not `nil`** — the strategy appends to this slice, and a nil slice would still compile (Go's `append` handles nil), but the JSON-encoded provider request shape would emit `messages: null` instead of `messages: []`. Different on the wire. Two of the OpenAI-compat backends (vLLM, llama.cpp's server) reject `null` outright. So we explicitly `make([]Message, 0, ...)`.

## What Changed (vs s04)

```diff
 agents/s05-episodic-history/
 ├── provider.go              # byte-identical to s04
 ├── provider_openai.go       # byte-identical to s04
 ├── provider_mock.go         # byte-identical to s04
 ├── provider_anthropic_test.go  # byte-identical to s04
 ├── provider_openai_test.go  # byte-identical to s04
 ├── provider_mock_test.go    # byte-identical to s04
 ├── tools.go / tools_test.go # byte-identical to s04
 ├── registry.go              # byte-identical to s04
 ├── registry_test.go         # one assertion adapted for the new render shape
 ├── strategy.go              # MODIFIED: BuildPrompt now folds history.RenderMessages()
 ├── strategy_test.go         # byte-identical to s04
+├── history.go               # NEW: Episode, ActionResult, History + 4 methods
+├── history_test.go          # NEW: 5 tests
 ├── loop.go                  # MODIFIED: Loop.History field; per-turn Append + record
 ├── loop_test.go             # s04 tests adapted + 1 new TestLoop_HistoryGrowsAfterEachTurn
 └── main.go                  # constructs &History{} and threads it into Loop
```

Type catalog gains:

```go
type ActionResult struct { Status, Output string }
type Episode      struct { Actions []ActionProposal; Results []ActionResult }
type History      []*Episode
```

`Loop` gains a `History *History` field. Nil is safe — Run allocates `&History{}` if missing — so s04-style construction (no History field) keeps compiling.

## Try It

```bash
cd agents/s05-episodic-history

# Anthropic native + oneshot (default)
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "add 7 and 35, then echo the result"

# DeepSeek (OpenAI-compat); the rendered history will travel through the
# OpenAI translation layer just fine — provider.go didn't change.
export DEEPSEEK_API_KEY=sk-...
go run . -provider deepseek -v "compute 6 * 7, then echo it"

# Local vLLM / SGLang
export OPENAI_API_KEY=anything
go run . -provider local -model llama-3.3 "echo hi, then echo bye"

# Run all tests (53 should pass)
go test -v ./...
```

Expected output (Anthropic path, two-tool-call task):

```
[s05-episodic-history] provider=anthropic model=claude-sonnet-4-6 url= strategy=oneshot tools=2
[turn 0] assistant: I'll compute 7 + 35 first.
[turn 0] proposal: cmd=math thoughts="I'll compute 7 + 35 first."
[turn 0] -> math map[a:7 b:35 operation:add]
[turn 0] <- 42
[turn 1] assistant: Now I'll echo the result.
[turn 1] proposal: cmd=echo thoughts="Now I'll echo the result."
[turn 1] -> echo map[message:42]
[turn 1] <- 42
[turn 2] assistant: 7 + 35 = 42 (echoed back).
7 + 35 = 42 (echoed back).
[s05-episodic-history] final history: 2 episodes
```

Each tool_use turn produced one Episode; the final `final history: 2 episodes` line confirms the History is observable from outside. By turn 2 the model received a `messages` payload that included BOTH prior episodes — assistant tool_use → user tool_result → assistant tool_use → user tool_result → trailing user task — proving `RenderMessages` rebuilt the conversation correctly.

## Upstream Source Reading

AutoGPT classic's episodic history lives in `classic/forge/forge/components/action_history/` — two files, `model.py` for the data shape and `action_history.py` for the component that exposes it to the agent loop.

```upstream:classic/forge/forge/components/action_history/model.py
# Source: classic/forge/forge/components/action_history/model.py
# Simplified: type vars, asyncio.Lock, Pydantic config, OpenAI/Anthropic
# message-shape branches stripped where they distract from the core
# Episode + EpisodicActionHistory[T] structure.

@dataclass
class Episode(Generic[T]):
    """One think-act-observe round: a proposal plus its result, optionally
    summarized for compression."""
    action: T                        # the proposal
    result: Optional[ActionResult]   # what happened
    summary: Optional[str] = None    # filled in by handle_compression()

    def format(self) -> str:
        """Human-readable rendering used in prepare_messages when no
        summary exists yet."""
        ...


class EpisodicActionHistory(Generic[T]):
    """The agent's event log, sliced into episodes.

    [→ s05: corresponds to our `History []*Episode`. Upstream wraps this
       in a class with cursor + lock + summarizer. Our Go version is a
       slice with methods; compression is left as the (advanced)
       comment seam.]
    """

    def __init__(self, full_message_count: int = 4):
        self.episodes: list[Episode[T]] = []
        self.cursor: int = 0
        self.full_message_count = full_message_count   # how many recent
                                                       # episodes stay
                                                       # un-summarized
        self._lock = asyncio.Lock()

    def register_action(self, action: T) -> None:
        """[→ s05: Loop's `history.Current().Actions = append(..., proposal)`
           after creating the Episode with Append(ep).]"""
        ...

    def register_result(self, result: ActionResult) -> None:
        """[→ s05: Loop's `ep.Results = append(ep.Results, ...)` after Execute.]"""
        ...

    async def handle_compression(self, llm_provider, model_name) -> None:
        """Summarize older episodes (those past full_message_count) by
        asking an LLM to produce a one-paragraph summary, then replacing
        the verbose action+result with the summary on each Episode.

        [→ s05 LEAVES THIS UNIMPLEMENTED. Our `// (advanced)` comment in
           history.go points to where this method's logic would land —
           inside RenderMessages, before walking the older slice.]
        """
        async with self._lock:
            for ep in self.episodes[: -self.full_message_count]:
                if ep.summary is not None:
                    continue
                ep.summary = await self._summarize(ep, llm_provider, model_name)

    def rewind(self, steps: int = 1) -> None:
        """Roll back the cursor by `steps` actions, removing partial
        records. Used by AutoGPT's user-feedback / interrupt path; s05
        doesn't model it (we'd need s09's signal handler first).]"""
        ...


# action_history.py (the Component that wires History into the agent)

class ActionHistoryComponent(MessageProvider, AfterParse, AfterExecute,
                              Generic[T]):
    """Implements three protocols at once:
       - MessageProvider: contributes to the prompt by turning History
         into [user, assistant, ...] messages.
       - AfterParse: hooked after the LLM proposal lands, registers
         the action on the current episode.
       - AfterExecute: hooked after the tool runs, registers the result.
    """

    def __init__(self, event_history: EpisodicActionHistory[T], llm_provider,
                 model_name: str, max_tokens: int = 4096):
        self.event_history = event_history
        self.llm_provider = llm_provider
        self.model_name = model_name
        self.max_tokens = max_tokens

    async def prepare_messages(self, messages: list[ChatMessage]) -> None:
        """LAZY COMPRESSION: only compress when we need the history.
        Calls handle_compression() if the rendered history would exceed
        budget; otherwise leaves it alone.

        [→ s05: our `RenderMessages()` returns the rendered slice with
           NO compression. The `// (advanced)` comment marks where this
           lazy-compress check would slot in.]"""
        if self._needs_compression():
            await self.event_history.handle_compression(
                self.llm_provider, self.model_name,
            )
        # ... append rendered messages onto `messages` ...

    def after_parse(self, proposal: T) -> None:
        """[→ s05: Loop's `ep.Actions = append(ep.Actions, proposal)`.]"""
        self.event_history.register_action(proposal)

    def after_execute(self, result: ActionResult) -> None:
        """[→ s05: Loop's `ep.Results = append(ep.Results, ...)`.]"""
        self.event_history.register_result(result)
```

### Reading notes

- **Compression is lazy in upstream too** — `prepare_messages` only invokes `handle_compression` when the budget check trips. AutoGPT doesn't summarize eagerly; the LLM call is expensive and most short conversations never need it. The `// (advanced)` comment in our `history.go` points to exactly this: don't compress until you have to.

- **Upstream's `Episode` is generic over the proposal type**; we hard-code `ActionProposal`. AutoGPT uses `Episode[OneShotAgentActionProposal]` vs `Episode[ReflexionAgentActionProposal]` to capture per-strategy fields (Reflexion's proposal has an extra `evaluation` field). Our Go version sticks with one ActionProposal struct because we ship one strategy; if s10 adds Reflexion, we'd add fields to ActionProposal directly rather than parameterize.

- **`AfterParse`/`AfterExecute` are protocols, not direct calls** — upstream's `ActionHistoryComponent` implements three optional protocols and the agent loop's pipeline runs all registered hooks. Our Go version inlines the bookkeeping in the Loop directly. s10 will introduce the pipeline + hooks abstraction, at which point ActionHistory becomes a Component (s08 territory) that implements those hooks. The boundary work is paid in s08 + s10; s05 just has the ingredients.

- **`rewind` is for human interrupt** — AutoGPT lets the user hit Ctrl-C, edit the proposed action, and resume. The history's cursor lets `rewind(1)` discard the in-flight action and re-prompt the model with a "the human said no, try again" signal. s09's continuous-mode chapter introduces this; s05 doesn't have a way for the user to interrupt yet (the Loop's `MaxTurns` is the only stop condition).

- **`full_message_count` defaults to 4** in upstream. That means the last 4 episodes always render verbatim; everything older gets summarized. We don't need this constant in s05 because we don't compress, but if you wire compression in, that number is the knob. Smaller = more aggressive compression (= more LLM calls = pricier) = longer effective history. Bigger = closer to verbatim = closer to the s05 baseline.

**Read further**: the upstream-readings excerpt is in [`upstream-readings/s05-action-history.py`](../../upstream-readings/s05-action-history.py) — keep it open beside the Go code. Then peek at s06's preview: episodic history travels well between turns, but `Workspace` is what lets the agent's actions actually *do* something to disk.

---

**Next**: s06 introduces the `Workspace` interface — sandboxed file-storage abstraction, with `LocalWorkspace` enforcing root-restriction via `filepath.Clean` + prefix check. The first non-trivial side-effect tools (`read_file`, `write_file`) land then. After s06 the agent can finally write to disk; s05 just made sure it could remember what it did.
