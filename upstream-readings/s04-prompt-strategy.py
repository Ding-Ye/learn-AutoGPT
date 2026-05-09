# Source: classic/original_autogpt/autogpt/agents/prompt_strategies/one_shot.py
#         classic/original_autogpt/autogpt/agents/prompt_strategies/base.py
# Upstream URLs:
#   https://github.com/Significant-Gravitas/AutoGPT/blob/master/classic/original_autogpt/autogpt/agents/prompt_strategies/one_shot.py
#   https://github.com/Significant-Gravitas/AutoGPT/blob/master/classic/original_autogpt/autogpt/agents/prompt_strategies/base.py
# License: MIT (the classic/ subtree is MIT-licensed; only autogpt_platform/
# is under Polyform Shield 1.0).
#
# This file pulls upstream's PromptStrategy abstraction + concrete
# OneShotAgentPromptStrategy into one annotated reading for s04 of
# learn-AutoGPT. Lines marked [→ sNN] indicate which Go session in this
# repo teaches the corresponding upstream concept. Pydantic settings,
# logger boilerplate, and generic typing are stripped where they distract
# from the strategy structure.


# ─────────────────────────────────────────────────────────────────────────
# prompt_strategies/base.py · the abstract base — what every strategy must do
# ─────────────────────────────────────────────────────────────────────────

class BaseMultiStepPromptStrategy(ABC):
    """Base class for multi-step prompt strategies.

    Provides common utilities for strategies that involve multiple phases
    like planning, execution, synthesis, or reflection. Also provides
    sub-agent spawning capabilities when enabled via config.

    [→ s04: our Go version trims this to two abstract methods —
       BuildPrompt + ParseResponse. Sub-agent spawning is out-of-scope;
       multi-phase strategies (rewoo, plan_execute, lats) are deferred to
       optional appendix-B exercises since shipping all 8 strategies in
       one teaching repo is the upstream anti-pattern we explicitly
       reject (see research-notes anti-patterns #6).]
    """

    def __init__(self, configuration, logger):
        self.config = configuration
        self.logger = logger
        self._execution_context = None

    @property
    @abstractmethod
    def llm_classification(self) -> LanguageModelClassification:
        """Declare whether this strategy needs a fast or smart model.
        [→ Go has no analog — model name lives on the Provider, not the
           Strategy. The fast_llm/smart_llm two-tier setup is upstream
           AppConfig (mentioned in research-notes glossary).]"""
        ...

    @abstractmethod
    def build_prompt(self, *_, **kwargs) -> ChatPrompt:
        """Build the prompt for the current phase.
        [→ s04: PromptStrategy.BuildPrompt(history, tools, task) []Message
           — far fewer parameters because we don't carry AIProfile, OS info,
           or a phase concept yet (multi-phase is not a s04 concern).]"""
        ...

    @abstractmethod
    def parse_response_content(self, response: AssistantChatMessage) -> ActionProposal:
        """Parse the LLM response into an action proposal.
        [→ s04: PromptStrategy.ParseResponse([]ContentBlock) ActionProposal
           — receives raw content blocks (Anthropic shape) instead of a
           Pydantic AssistantChatMessage; tries native tool_use first, then
           falls back to ```json fenced-code parsing for smaller models.]"""
        ...


# ─────────────────────────────────────────────────────────────────────────
# prompt_strategies/one_shot.py · the baseline strategy
# ─────────────────────────────────────────────────────────────────────────

class OneShotAgentPromptConfiguration(SystemConfiguration):
    """Holds the body template + the seven Efficiency Guidelines.

    [→ s04: our DefaultBestPractices in strategy.go ships only the first
       five guidelines verbatim. Items 6 (CODE STYLE) and 7 (SECURITY)
       require Workspace + Components to have ground truth — we add them
       back in s06/s08 once those exist. Injecting them now would be the
       prompt LYING about what the agent can actually do.]
    """

    DEFAULT_BODY_TEMPLATE: str = (
        "## Constraints\n"
        "You operate within the following constraints:\n"
        "{constraints}\n\n"
        "## Resources\n"
        "You can leverage access to the following resources:\n"
        "{resources}\n\n"
        "## Commands\n"
        "These are the ONLY commands you can use."
        " Any action you perform must be possible through one of these commands:\n"
        "{commands}\n\n"
        "## Best practices\n"
        "{best_practices}\n\n"
        "## Efficiency Guidelines\n"
        "You have LIMITED steps. Be efficient:\n\n"
        "1. UNDERSTAND BEFORE ACTING: Read ALL relevant files before making "
        "changes. Understand requirements, interfaces, and existing code "
        "patterns first.\n\n"
        "2. PARALLEL EXECUTION: When multiple operations don't depend on "
        "each other, execute them simultaneously (e.g., read multiple files "
        "at once).\n\n"
        "3. WRITE COMPLETE CODE: Write complete, working implementations. "
        "No stubs, TODOs, or placeholders.\n\n"
        "4. VERIFY AFTER CHANGES: After modifying code, verify it works. "
        "Run available linters/formatters/tests if available.\n\n"
        "5. FIX ROOT CAUSE: When debugging, fix the underlying issue, not "
        "symptoms. If a test fails, the bug is in your code, NOT in the "
        "test.\n\n"
        "6. CODE STYLE: Mimic existing code conventions. Don't add comments "
        "unless the logic is genuinely complex.\n\n"
        "7. SECURITY: Never expose, log, or commit secrets, API keys, or "
        "credentials."
    )
    body_template: str = UserConfigurable(default=DEFAULT_BODY_TEMPLATE)
    use_prefill: bool = True


class AssistantThoughts(ModelWithSummary):
    """The structured-reasoning shape upstream forces the model to emit.

    [→ s04: we collapse this to ActionProposal.Thoughts string — a single
       free-text field. Reasons: (a) AssistantThoughts subfields have no
       downstream consumers in OneShot, an upstream dead-code risk; (b) a
       single string streams more gracefully; (c) when s10's Reflexion
       actually CONSUMES thoughts, we'll add structured fields then.]"""
    observations: str = Field(
        description="Relevant observations from your last action (if any)"
    )
    reasoning: str = Field(description="Reasoning behind choosing this action")
    self_criticism: str = Field(description="Constructive self-criticism")
    plan: list[str] = Field(description="Short list that conveys the long-term plan")


class OneShotAgentActionProposal(ActionProposal):
    """[→ s04: corresponds to our flat ActionProposal struct. We don't
       inherit through Pydantic — Args is map[string]interface{} and
       Tools handle their own input validation in Execute.]"""
    thoughts: AssistantThoughts  # type: ignore


class OneShotAgentPromptStrategy(PromptStrategy):
    """The baseline one-shot strategy.

    [→ s04: matches our OneShotStrategy 1:1, modulo prefill. AutoGPT
       upstream ships 8 strategies (one_shot, rewoo, reflexion,
       plan_execute, lats, tree_of_thoughts, multi_agent_debate, base);
       we ship one. Reflexion is the most architecturally distinctive
       and gets its own session (s10).]"""

    def build_prompt(
        self,
        *,
        messages: list[ChatMessage],     # ← history; corresponds to []*Episode
        task: str,
        ai_profile: AIProfile,           # ← upstream-only; agent persona
        ai_directives: AIDirectives,     # ← upstream-only; component-injected
        commands: list[CompletionModelFunction],
        include_os_info: bool,
        **extras,
    ) -> ChatPrompt:
        """Constructs and returns a prompt with three layers:
            1. system prompt (role + constraints + commands + practices)
            2. user message with the task in triple quotes
            3. message history + final 'choose_action' instruction

        [→ s04: our BuildPrompt returns []Message{user-msg with task}.
           BuildSystem(tools) is a separate string method because Anthropic
           carries `system` as a top-level request field (not a Message).
           No final instruction — Anthropic native tool_use makes the
           "now please choose an action" reminder unnecessary.]"""
        system_prompt, response_prefill = self.build_system_prompt(
            ai_profile=ai_profile,
            ai_directives=ai_directives,
            commands=commands,
            include_os_info=include_os_info,
        )
        final_instruction_msg = ChatMessage.user(self.config.choose_action_instruction)
        return ChatPrompt(
            messages=[
                ChatMessage.system(system_prompt),
                ChatMessage.user(f'"""{task}"""'),
                *messages,
                final_instruction_msg,
            ],
            prefill_response=response_prefill if self.config.use_prefill else "",
            functions=commands,
        )

    def parse_response_content(
        self, response: AssistantChatMessage,
    ) -> OneShotAgentActionProposal:
        """Parse LLM response into an action proposal.

        [→ s04: our ParseResponse has the same dual-path structure but
           the FALLBACK is different: upstream uses extract_dict_from_json
           (search-anywhere for a dict-shaped substring), we use a strict
           ```json``` fenced-code regex. The fence regex is more
           conservative — avoids false positives where the model writes
           `{ example: 1 }` in prose. The trade-off: we miss some
           recoverable malformed responses upstream catches.]"""
        if not response.content:
            # Some models (e.g. GPT-5) return tool_calls without text content.
            # Use a minimal thoughts dict so we can still proceed.
            if response.tool_calls:
                assistant_reply_dict: dict = {
                    "thoughts": {
                        "observations": "", "text": "", "reasoning": "",
                        "self_criticism": "", "plan": [], "speak": "",
                    }
                }
            else:
                raise InvalidAgentResponseError(
                    "Assistant response has no text content"
                )
        else:
            # extract_dict_from_json is a forgiving JSON parser:
            # finds the first dict-like substring in the text.
            # [→ s04: our fenceRegex is more conservative — only matches
            #    ```json``` fences. Less false-positive risk on prose;
            #    less recovery on malformed responses.]
            assistant_reply_dict = extract_dict_from_json(response.content)

        # Always expect tool calls — native tool calling is always enabled.
        # [→ s04: same contract — if neither tool_use nor JSON-fence,
        #    ParseResponse returns an error and the Loop reports a
        #    recoverable failure to the caller.]
        if not response.tool_calls:
            raise InvalidAgentResponseError("Assistant did not use a tool")
        assistant_reply_dict["use_tool"] = response.tool_calls[0].function

        # Capture all tool calls for parallel execution.
        # [→ s04: we only consume the FIRST tool_use block; parallel
        #    execution lives in upstream _execute_tools_parallel and is
        #    deferred to s08 components.]
        if len(response.tool_calls) > 1:
            assistant_reply_dict["use_tools"] = [
                tc.function for tc in response.tool_calls
            ]

        parsed_response = OneShotAgentActionProposal.model_validate(
            assistant_reply_dict
        )
        parsed_response.raw_message = response.model_copy()
        return parsed_response


# ─────────────────────────────────────────────────────────────────────────
# Reading map — which session of learn-AutoGPT teaches each upstream symbol
# ─────────────────────────────────────────────────────────────────────────
#
# BaseMultiStepPromptStrategy ABC          → s04 (our PromptStrategy iface;
#                                                  the multi-step pipeline
#                                                  is left for s10's hooks)
# OneShotAgentPromptStrategy.build_prompt  → s04 (our OneShotStrategy.BuildPrompt
#                                                  + BuildSystem; system extracted
#                                                  to top-level Anthropic field)
# OneShotAgentPromptStrategy.parse_*       → s04 (our OneShotStrategy.ParseResponse;
#                                                  fenceRegex replaces upstream's
#                                                  extract_dict_from_json)
# OneShotAgentActionProposal               → s04 (flat ActionProposal struct;
#                                                  no Pydantic wrapper)
# AssistantThoughts                        → s04 (collapsed to single string;
#                                                  s10 may revisit if Reflexion
#                                                  needs structured fields)
# OneShotAgentPromptConfiguration          → s04 (5 best-practices vs upstream 7;
#                                                  6 & 7 added in s06/s08 when
#                                                  Workspace+Components arrive)
# use_prefill / response_prefill           → s04 (skipped — Anthropic native
#                                                  tool_use makes prefill obsolete)
# extract_dict_from_json                   → s04 (replaced by fenceRegex —
#                                                  conservative ```json``` only)
# AIProfile / AIDirectives                 → (deferred — s08 component system
#                                                  + future ai_settings.yaml)
# ChatPrompt / messages list with history  → s05 (Episode + History.RenderMessages
#                                                  fold history into messages)
# ReflexionAgentPromptStrategy             → s10 (separate session; wraps OneShot
#                                                  via AfterParse pipeline hook)
# _execute_tools_parallel                  → s08 (component system addresses
#                                                  parallel-tool execution)
