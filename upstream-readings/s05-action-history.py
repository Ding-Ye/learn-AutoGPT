# Source: classic/forge/forge/components/action_history/model.py
#         classic/forge/forge/components/action_history/action_history.py
# Upstream URLs:
#   https://github.com/Significant-Gravitas/AutoGPT/blob/master/classic/forge/forge/components/action_history/model.py
#   https://github.com/Significant-Gravitas/AutoGPT/blob/master/classic/forge/forge/components/action_history/action_history.py
# License: MIT (the classic/ subtree is MIT-licensed; only autogpt_platform/
# is under Polyform Shield 1.0).
#
# This file pulls upstream's Episode + EpisodicActionHistory data shapes
# and the ActionHistoryComponent that wires them into the agent loop
# into one annotated reading for s05 of learn-AutoGPT. Lines marked
# [→ s05] indicate which Go construct in this repo's session 5 teaches
# the corresponding upstream concept. Pydantic config, asyncio.Lock,
# message-shape branches, and OpenAI/Anthropic content-array packing
# are stripped where they distract from the structural story.


# ─────────────────────────────────────────────────────────────────────────
# action_history/model.py · Episode + EpisodicActionHistory
# ─────────────────────────────────────────────────────────────────────────

@dataclass
class Episode(Generic[T]):
    """One think-act-observe round.

    Stores:
      - action: the proposal the model emitted (an ActionProposal subclass)
      - result: what happened when we executed that proposal (or None
        when the loop is mid-turn — proposal landed but tool hasn't run)
      - summary: filled in by handle_compression() once this episode is
        old enough to be condensed into a one-line summary

    [→ s05: corresponds to our Go `type Episode struct { Actions
       []ActionProposal; Results []ActionResult }`. We use slices
       because parallel tool calls (a single assistant turn that emits
       multiple tool_use blocks) belong in the same Episode.
       Upstream's `result: Optional[...]` is our `len(Results) == 0`
       mid-turn state.]
    """
    action: T
    result: Optional[ActionResult]
    summary: Optional[str] = None

    def format(self) -> str:
        """Human-readable rendering used inside prepare_messages when
        no summary exists yet. Looks roughly like:

            ## Step N
            * REASONING: <action.thoughts.reasoning>
            * STATUS: success
            * RESULT: <result.output>

        [→ s05: our `History.RenderMessages()` does the same job but
           instead of producing a markdown blob, it produces a list of
           protocol-shape Messages — assistant tool_use + user
           tool_result. The shape difference is because upstream's
           prompt strategy renders this string into a system-side
           "## Progress" section; our OneShotStrategy uses native
           tool_use blocks for replay, no markdown wrapper needed.]"""
        ...


class EpisodicActionHistory(Generic[T]):
    """The agent's event log, sliced into discrete episodes.

    Provides:
      - sequential storage (`episodes: list[Episode[T]]`)
      - a cursor for partial rollback (rewind)
      - a queue for user feedback to inject between turns
      - thread-safe compression via asyncio.Lock

    [→ s05: corresponds to our `type History []*Episode`. Upstream
       wraps the slice in a class with cursor + lock + summarizer. Our
       Go version is a slice with methods; the cursor/feedback queue
       arrives in s09 (continuous mode + interrupt), and compression
       is left as the (advanced) comment seam in history.go.]
    """

    full_message_count: int = 4   # most recent N episodes stay verbatim;
                                  # everything older may be summarized

    def __init__(self):
        self.episodes: list[Episode[T]] = []
        self.cursor: int = 0
        self._user_feedback_queue: list[str] = []
        self._compression_lock = asyncio.Lock()

    def __len__(self) -> int:
        return len(self.episodes)

    def __getitem__(self, idx: int) -> Episode[T]:
        return self.episodes[idx]

    @property
    def current_episode(self) -> Optional[Episode[T]]:
        """The most recent episode (the one the loop is filling in).
        [→ s05: our `History.Current()` returns the same thing —
           the last *Episode pointer, or nil if empty.]"""
        return self.episodes[-1] if self.episodes else None

    def register_action(self, action: T) -> None:
        """Start a new episode by appending a fresh Episode whose
        result is None. Called by AfterParse hook after the strategy
        parses a proposal.

        [→ s05: our Loop does this in two steps for clarity:
              ep := &Episode{}
              l.History.Append(ep)
              ep.Actions = append(ep.Actions, proposal)
           Upstream collapses the create + register; we keep them
           separate so the empty-Episode-then-fill pattern is visible
           in the test (and so RenderMessages can render the mid-turn
           state without surprises).]"""
        self.episodes.append(Episode(action=action, result=None))
        self.cursor = len(self.episodes)

    def register_result(self, result: ActionResult) -> None:
        """Attach a result to the current (in-flight) episode.
        Called by AfterExecute hook after the tool runs.

        [→ s05: our Loop does
              ep.Results = append(ep.Results, results...)
           — passing a slice because parallel tool dispatch produces
           multiple results per Episode. Upstream registers one
           result; we record N to keep parallel execution natural.]"""
        if self.current_episode is None:
            raise RuntimeError("register_result with no current episode")
        self.current_episode.result = result

    async def handle_compression(
        self, llm_provider, model_name: str,
    ) -> None:
        """Summarize older episodes (those past full_message_count) by
        asking an LLM to produce a one-paragraph summary, then setting
        ep.summary = "<summary text>" so prepare_messages can render
        the summary instead of action+result.

        Lock-guarded so two concurrent prepare_messages calls don't
        double-summarize the same episode.

        [→ s05 LEAVES THIS UNIMPLEMENTED. The Go implementation reads:

            // (advanced) when context overflows, summarize old
            // episodes here.
            func (h *History) RenderMessages() []Message { ... }

           That comment is the seam. When you wire compression in,
           the body of RenderMessages — before the for-loop walks the
           old slice — is where the lazy-compress check + LLM call
           lives.]
        """
        async with self._compression_lock:
            # Walk all but the last full_message_count episodes, asking
            # the LLM to summarize each that doesn't already have a
            # summary attached.
            for ep in self.episodes[: -self.full_message_count]:
                if ep.summary is not None:
                    continue
                ep.summary = await self._summarize(ep, llm_provider, model_name)

    async def _summarize(self, ep: Episode[T], llm_provider, model_name: str) -> str:
        """Single-episode summarization helper. Builds a small
        compress-this-episode prompt, calls the LLM, returns the
        summary string.

        [→ s05: corresponds to "the LLM call you'd add in
           RenderMessages's compression branch". Note: this is a SECOND
           LLM round-trip per old episode — compression is expensive,
           don't do it eagerly. AutoGPT only triggers it when the
           rendered messages would exceed the budget. Same plan
           applies to our (future) Go implementation.]"""
        ...

    def rewind(self, steps: int = 1) -> None:
        """Roll back the cursor by `steps`, removing partial records.
        Used for human-in-the-loop interrupt: the user hits Ctrl-C, we
        rewind one action so the next prompt re-asks the model.

        [→ s05 doesn't model rewind; we'd need s09's signal handler
           and cursor management first. The Loop's MaxTurns is the
           only stop condition in s05.]"""
        ...

    def fmt_paragraph(self) -> str:
        """Render every episode (using format() or its summary) as a
        sequence of paragraphs separated by blank lines.

        [→ s05: corresponds (loosely) to our RenderMessages(). The
           shape is different — paragraphs vs Messages — but the role
           is the same: walk the episode list, emit per-episode
           rendering, concatenate.]"""
        ...


# ─────────────────────────────────────────────────────────────────────────
# action_history/action_history.py · ActionHistoryComponent
# ─────────────────────────────────────────────────────────────────────────

class ActionHistoryComponent(MessageProvider, AfterParse, AfterExecute,
                              Generic[T]):
    """Glue between EpisodicActionHistory and the agent loop. Implements
    THREE optional protocols:

      - MessageProvider.get_messages: yields chat messages representing
        the rendered history (full for recent episodes, summary for
        compressed old ones, capped by max_tokens).
      - AfterParse.after_parse: hook fired after strategy.parse_response,
        registers the parsed action on the in-flight episode.
      - AfterExecute.after_execute: hook fired after tool dispatch,
        registers the result on the in-flight episode.

    [→ s05 inlines all three roles in the Loop body:
          ep := &Episode{}
          l.History.Append(ep)            ← MessageProvider's add-to-history
          ep.Actions = append(...)         ← AfterParse equivalent
          results := l.runTools(...)
          ep.Results = append(...)         ← AfterExecute equivalent
       The Component-style separation arrives in s08 along with
       protocols; for s05 the inline form is enough to teach the
       Episode + History data flow without dragging in protocol
       dispatching.]
    """

    def __init__(
        self,
        event_history: EpisodicActionHistory[T],
        llm_provider,
        model_name: str,
        max_tokens: int = 4096,
        full_message_count: int = 4,
        enable_compression: bool = True,
    ):
        self.event_history = event_history
        self.llm_provider = llm_provider
        self.model_name = model_name
        self.max_tokens = max_tokens
        self.full_message_count = full_message_count
        self.enable_compression = enable_compression

    async def prepare_messages(self, messages: list[ChatMessage]) -> None:
        """LAZY COMPRESSION ENTRY POINT. Only invokes compression when
        the rendered history would push the request over budget.

        Pseudocode:

            if self.enable_compression and self._needs_compression():
                await self.event_history.handle_compression(
                    self.llm_provider, self.model_name,
                )
            for episode in self.event_history.episodes:
                messages.append(self._render_episode(episode))

        [→ s05: our `History.RenderMessages()` corresponds to the
           per-episode `messages.append(...)` loop. The `if
           enable_compression` branch is the seam our `// (advanced)`
           comment marks — fill it in when context overflows.]
        """
        if self.enable_compression and self._needs_compression():
            await self.event_history.handle_compression(
                self.llm_provider, self.model_name,
            )
        # Iterate self.event_history.episodes, append rendered chat
        # messages onto the `messages` list (full-format for last
        # full_message_count, summary for older).
        ...

    def after_parse(self, proposal: T) -> None:
        """[→ s05: Loop's
              ep.Actions = append(ep.Actions, proposal)
           after creating the Episode with Append(ep). The hook form
           arrives in s10 along with Pipeline + AfterParse[T] type;
           for s05 the inline append in Loop.Run is enough to teach
           the data flow.]"""
        self.event_history.register_action(proposal)

    def after_execute(self, result: ActionResult) -> None:
        """[→ s05: Loop's
              ep.Results = append(ep.Results, ...)
           after tool dispatch. Same hook caveat as after_parse.]"""
        self.event_history.register_result(result)

    def _needs_compression(self) -> bool:
        """Estimate whether rendering the current history (un-summarized)
        would exceed self.max_tokens. The check uses tiktoken or a
        per-provider counter; we don't render-then-measure (too
        expensive) — we estimate from episode count and average
        message size.

        [→ s05: omitted. When you implement compression in
           RenderMessages, this is the budget check that gates whether
           to compress at all.]"""
        ...
