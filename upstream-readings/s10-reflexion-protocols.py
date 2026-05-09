# Source: classic/forge/forge/agent/protocols.py (top half)
#       + classic/original_autogpt/autogpt/agents/prompt_strategies/reflexion.py (header)
# Upstream URLs:
#   https://github.com/Significant-Gravitas/AutoGPT/blob/master/classic/forge/forge/agent/protocols.py
#   https://github.com/Significant-Gravitas/AutoGPT/blob/master/classic/original_autogpt/autogpt/agents/prompt_strategies/reflexion.py
#   https://github.com/Significant-Gravitas/AutoGPT/blob/master/classic/original_autogpt/autogpt/agents/agent.py
# License: MIT (the classic/ subtree is MIT-licensed; only autogpt_platform/ uses Polyform Shield)
#
# This file is the upstream backing for the s10 chapter. AutoGPT classic
# defines its hook system as Python abstract base classes that components
# inherit from. The agent dispatcher (agent.py:282) uses `getattr`-based
# reflection to discover and run them. Reflexion is one user of this
# system. Annotated for the s10 chapter of learn-AutoGPT; lines marked
# [→ sNN] indicate which Go session in this repo teaches that concept.

# ==========================================================================
# Part A: forge/agent/protocols.py  — the hook ABCs
# ==========================================================================

from abc import abstractmethod
from typing import TYPE_CHECKING, Awaitable, Generic, Iterator

from forge.models.action import ActionResult, AnyProposal

from .components import AgentComponent

if TYPE_CHECKING:
    from forge.command.command import Command
    from forge.llm.providers import ChatMessage


# DirectiveProvider — components implementing this interface contribute
# strings to the system prompt. [→ s08] The Go version is a structural
# `DirectiveProvider { Directives() []string }` interface satisfied
# without explicit `implements` clause.
class DirectiveProvider(AgentComponent):
    def get_constraints(self) -> Iterator[str]:
        return iter([])

    def get_resources(self) -> Iterator[str]:
        return iter([])

    def get_best_practices(self) -> Iterator[str]:
        return iter([])


# CommandProvider — components contribute Command objects to the
# agent's tool registry. [→ s08] The Go translation is
# `CommandProvider { Commands() []Tool }`; ComponentBus aggregates them.
class CommandProvider(AgentComponent):
    @abstractmethod
    def get_commands(self) -> Iterator["Command"]: ...


# MessageProvider — components contribute pre-injected ChatMessage
# instances. [→ s08] We implemented this surface but the s10 main.go
# doesn't use any MessageProvider components — kept the interface for
# parity with upstream.
class MessageProvider(AgentComponent):
    @abstractmethod
    def get_messages(self) -> Iterator["ChatMessage"]: ...


# === The two abstract base classes that anchor s10 =======================

# AfterParse — the post-parse hook. agent.py:282 calls
# `await self.run_pipeline(AfterParse.after_parse, result)` after
# strategy.parse_response_content has produced a proposal but before
# the action is executed. [→ s10] Our Go version promotes the
# callback itself to a type: `type AfterParseHook func(ctx, *prop) error`.
# A single Pipeline holds the slice of hooks; no reflection needed.
class AfterParse(AgentComponent, Generic[AnyProposal]):
    @abstractmethod
    def after_parse(self, result: AnyProposal) -> None | Awaitable[None]: ...


# ExecutionFailure — the exception-path hook (called when the agent's
# Python code raises during execute()). [→ s10 NOT IMPLEMENTED]
# Reason: our Go ActionResult uses Status: "error" as a string field
# rather than raising panics, so AfterExecuteHook already covers
# failure cases. ExecutionFailure is a Python-exception-model artifact
# we deliberately omit. (Documented in s10 README.)
class ExecutionFailure(AgentComponent):
    @abstractmethod
    def execution_failure(self, error: Exception) -> None | Awaitable[None]: ...


# AfterExecute — the post-execute hook. agent.py runs this immediately
# after action dispatch (success or failure as long as no exception).
# [→ s10] Go: `type AfterExecuteHook func(ctx, *result) error`.
# Same pipeline mechanism as AfterParse; symmetric API.
class AfterExecute(AgentComponent):
    @abstractmethod
    def after_execute(self, result: "ActionResult") -> None | Awaitable[None]: ...


# ==========================================================================
# Part B: original_autogpt/autogpt/agents/prompt_strategies/reflexion.py
#         (module docstring + key types — full file is 600+ lines)
# ==========================================================================

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

# [→ s10] We implement step 1 (GENERATE) via the base OneShotStrategy
# (s04) and step 3 (REFLECT) via the second-pass LLM call inside the
# AfterParseHook. Steps 2 (EXECUTE) and 4 (RETRY) are already in the
# Loop from s01 + s09. The piece we deliberately omit is the
# **persistence of reflections into episodic memory across turns** —
# that's the Reflexion paper's distinctive contribution and is left
# as Appendix B exercise #1.


from enum import Enum
from typing import Optional

from pydantic import Field

from forge.models.utils import ModelWithSummary


# [→ s10 REFLECTION-PHASE STATE MACHINE — NOT IMPLEMENTED]
# Upstream Reflexion is a multi-step strategy: PROPOSING (build prompt
# + parse) and REFLECTING (run the second-pass + write to memory). Our
# Go ReflexionStrategy collapses both into a single AfterParseHook
# call — the "REFLECT" step happens inside the hook synchronously,
# without a separate phase. The state-machine version is a natural
# evolution and would also live as Appendix B exercise material.
class ReflexionPhase(str, Enum):
    PROPOSING = "proposing"
    REFLECTING = "reflecting"


# [→ s10 EvaluationResult ≈ reflexionVerdict]
# Our reflexionVerdict (in strategy_reflexion.go) carries the same
# essence: a `sound: bool` plus an optional revision. We dropped the
# `score: float` field — the binary sound/unsound decision is enough
# to gate "do we rewrite the proposal." The `feedback: str` we
# implicitly carry as the verdict's `reason` field.
class EvaluationResult(ModelWithSummary):
    """Result from the Evaluator component."""

    success: bool = Field(description="Whether the action was successful")
    score: Optional[float] = Field(
        default=None, ge=0.0, le=1.0, description="Score from 0-1 if available"
    )
    feedback: str = Field(default="", description="Feedback about the result")


# Reading map:
# - AfterParse / AfterExecute ABCs       → s10 (Pipeline + hook types)
# - run_pipeline (agent.py:282)          → s10 (Pipeline.RunAfterParse / RunAfterExecute)
# - ReflexionPromptStrategy              → s10 (ReflexionStrategy wraps OneShotStrategy)
# - EvaluationResult                     → s10 (reflexionVerdict — minus score)
# - ReflexionMemory / Reflection.persist → Appendix B exercise #1 (semantic memory)
# - DirectiveProvider/CommandProvider/   → s08 (component protocols)
#   MessageProvider
# - ExecutionFailure                     → DELIBERATELY OMITTED (Python exception artifact)
