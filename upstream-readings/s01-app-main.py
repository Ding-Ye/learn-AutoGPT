# Source: classic/original_autogpt/autogpt/app/main.py
# Upstream URL: https://github.com/Significant-Gravitas/AutoGPT/blob/master/classic/original_autogpt/autogpt/app/main.py
# License: MIT (the classic/ subtree is MIT-licensed; only autogpt_platform/
# is under Polyform Shield 1.0).
#
# This file shows the upstream `run_interaction_loop` — the heart of the
# autonomous-agent control flow — alongside `agent.propose_action` and
# `agent.complete_and_parse` (from agents/agent.py) to expose the LLM call
# site. Annotated for the s01 chapter of learn-AutoGPT.
#
# Lines marked [→ sNN] indicate which Go session in this repo teaches that
# specific upstream concept. Imports, logging, and try/except boilerplate
# stripped where they distract from the control flow.


# ─────────────────────────────────────────────────────────────────────────
# app/main.py · run_interaction_loop  (the agent's main control loop)
# ─────────────────────────────────────────────────────────────────────────

async def run_interaction_loop(
    agent: "Agent",
    ui_provider: Optional["UIProvider"] = None,
) -> None:
    """Run the main interaction loop for the agent."""
    app_config = agent.app_config
    ai_profile = agent.state.ai_profile

    if ui_provider is None:
        ui_provider = create_ui_provider(
            plain_output=app_config.logging.plain_console_output,
        )

    # cycle_budget = how many autonomous turns the user permits.
    # None ≡ infinity (continuous mode). [→ s09: cycle budget]
    cycle_budget = cycles_remaining = _get_cycle_budget(
        app_config.continuous_mode, app_config.continuous_limit
    )
    spinner = Spinner("Thinking...", plain_output=app_config.logging.plain_console_output)
    stop_reason = None

    # SIGINT handling: first press clamps cycles_remaining to 1 (let the
    # current cycle finish cleanly); second press hard-exits.
    # [→ s09: signal handling — Go uses signal.Notify channel pattern]
    def graceful_agent_interrupt(signum, frame):
        nonlocal cycles_remaining, stop_reason
        if stop_reason:
            sys.exit()
        if cycles_remaining in [0, 1]:
            stop_reason = AgentTerminated("Interrupt signal received")
        else:
            cycles_remaining = 1

    def handle_stop_signal():
        if stop_reason:
            raise stop_reason

    signal.signal(signal.SIGINT, graceful_agent_interrupt)

    consecutive_failures = 0

    # ─── The actual loop. This is what s01 reproduces in Go. ───
    while cycles_remaining > 0:                        # [→ s01: for turn := 0; turn < MaxTurns]
        handle_stop_signal()

        ########
        # Plan #  (think)
        ########
        # Only propose a new action if we don't have an in-flight episode.
        if not (_ep := agent.event_history.current_episode) or _ep.result:
            async with ui_provider.show_spinner("Thinking..."):
                try:
                    action_proposal = await agent.propose_action()  # [→ s01: Provider.CreateMessage]
                except InvalidAgentResponseError as e:
                    # 3 strikes and we abort — protects against runaway parse failures.
                    consecutive_failures += 1
                    if consecutive_failures >= 3:
                        raise AgentTerminated("...3 invalid thoughts in a row...")
                    continue
        else:
            action_proposal = _ep.action

        consecutive_failures = 0

        # Show the assistant's thoughts and proposed command to the user.
        await ui_provider.display_thoughts(
            ai_name=ai_profile.ai_name,
            thoughts=action_proposal.thoughts,
            speak_mode=app_config.tts_config.speak_mode,
        )

        handle_stop_signal()

        ###################
        # Execute Command #   (act + observe)
        ###################
        if not action_proposal.use_tool:
            continue

        handle_stop_signal()

        try:
            # Permission manager prompts user inside execute() if needed.
            # [→ s07: permission manager — gates between parse and execute]
            result = await agent.execute(action_proposal)
        except AgentFinished as e:
            # Agent voluntarily finished. Interactive: prompt for next task.
            # Non-interactive: exit (preserves benchmark behavior).
            if app_config.noninteractive_mode:
                return
            next_task = await ui_provider.prompt_finish_continuation(
                summary=e.message, suggested_next_task=e.suggested_next_task,
            )
            if not next_task.strip():
                return
            agent.state.task = next_task
            agent.event_history.episodes.clear()
            agent.event_history.cursor = 0
            cycles_remaining = _get_cycle_budget(
                app_config.continuous_mode, app_config.continuous_limit
            )
            continue

        # KEY: human deny doesn't burn a cycle. So feedback cannot accelerate
        # the budget's exhaustion.  [→ s09: cycles_remaining decrement guard]
        if result.status != "interrupted_by_human":
            cycles_remaining -= 1


# ─────────────────────────────────────────────────────────────────────────
# agents/agent.py · propose_action  (the "think" half of the loop)
# ─────────────────────────────────────────────────────────────────────────

async def propose_action(self) -> "AnyActionProposal":
    """Decide what to do next, by asking the LLM."""
    self.reset_trace()

    # Pipeline hooks: gather directives / commands / messages from every
    # component. This is the seam Components use to inject capability.
    # [→ s08: components, CommandProvider/MessageProvider/DirectiveProvider]
    resources = await self.run_pipeline(DirectiveProvider.get_resources)
    constraints = await self.run_pipeline(DirectiveProvider.get_constraints)
    best_practices = await self.run_pipeline(DirectiveProvider.get_best_practices)
    directives = self.state.directives.model_copy(deep=True)
    directives.resources += resources
    directives.constraints += constraints
    directives.best_practices += best_practices

    self.commands = await self.run_pipeline(CommandProvider.get_commands)  # [→ s02: Registry, s08: components]
    self._remove_disabled_commands()

    if hasattr(self, "history"):
        await self.history.prepare_messages()  # lazy-compress old episodes [→ s05]

    messages = await self.run_pipeline(MessageProvider.get_messages)

    # Strategy chooses how to render the system prompt + tool list.
    # [→ s04: PromptStrategy.BuildPrompt]
    prompt: ChatPrompt = self.prompt_strategy.build_prompt(
        messages=messages,
        task=self.state.task,
        ai_profile=self.state.ai_profile,
        ai_directives=directives,
        commands=function_specs_from_commands(self.commands),
    )

    output = await self.complete_and_parse(prompt)
    self.config.cycle_count += 1

    return output


async def complete_and_parse(self, prompt: "ChatPrompt") -> "AnyActionProposal":
    """The actual LLM call. Equivalent to our Go Provider.CreateMessage."""
    # [→ s01: Provider interface, AnthropicProvider, OpenAIProvider]
    # [→ s03: multi-provider system, MockProvider for tests]
    response = await self.llm_provider.create_chat_completion(
        prompt.messages,
        model_name=self.llm.name,
        completion_parser=self.prompt_strategy.parse_response_content,  # [→ s04: Strategy.ParseResponse]
        functions=prompt.functions,
        prefill_response=prompt.prefill_response,
    )
    result = response.parsed_result

    # AfterParse hook: every component gets a chance to mutate / critique
    # the proposal before it's executed. Reflexion does its second-pass
    # critique here.  [→ s10: AfterParseHook, ReflexionStrategy]
    await self.run_pipeline(AfterParse.after_parse, result)

    return result


# ─────────────────────────────────────────────────────────────────────────
# Reading map — which session of learn-AutoGPT teaches each upstream symbol
# ─────────────────────────────────────────────────────────────────────────
#
# run_interaction_loop          → s01 (minimal loop), s09 (continuous mode + UI)
# graceful_agent_interrupt      → s09 (signal handling — Go pattern is cleaner)
# agent.propose_action          → s01 (LLM call site), s04 (prompt strategy),
#                                 s10 (AfterParse hook chain)
# agent.complete_and_parse      → s01 (Provider.CreateMessage), s03 (multi-provider)
# agent.execute                 → s01 (tool dispatch via toolByName map),
#                                 s07 (permission gate before execute)
# CommandProvider.get_commands  → s02 (Registry), s08 (Component as tool source)
# MessageProvider.get_messages  → s05 (Episode + History.RenderMessages),
#                                 s08 (Component as message source)
# DirectiveProvider.*           → s08 (Component as directive source)
# AfterParse.after_parse        → s10 (Pipeline + AfterParseHook)
# event_history / Episode       → s05 (episodic action history)
# permission manager            → s07 (Permissions + Decision + Asker)
# cycles_remaining decrement    → s09 (LoopOpts.Cycles + interrupt guard)
