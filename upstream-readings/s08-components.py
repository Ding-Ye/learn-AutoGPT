# Source: classic/forge/forge/agent/protocols.py
#         + classic/forge/forge/components/file_manager/__init__.py
# Upstream URLs:
#   https://github.com/Significant-Gravitas/AutoGPT/blob/master/classic/forge/forge/agent/protocols.py
#   https://github.com/Significant-Gravitas/AutoGPT/blob/master/classic/forge/forge/components/file_manager/__init__.py
# License: MIT (the classic/ subtree is MIT-licensed; only autogpt_platform/
# is under Polyform Shield 1.0).
#
# This file pulls upstream's component protocols (CommandProvider,
# DirectiveProvider, MessageProvider) and the canonical example
# (FileManagerComponent) into one annotated reading for s08 of
# learn-AutoGPT. Lines marked [→ s08] indicate which Go construct in
# this repo's session 8 teaches the corresponding upstream concept.
# Pydantic, async iterators, and dependency-injection plumbing are
# stripped where they distract from the protocol → bus → loop story.


from __future__ import annotations

from abc import ABC, abstractmethod
from typing import AsyncIterator, Iterator, TYPE_CHECKING

if TYPE_CHECKING:
    from forge.command import Command
    from forge.llm.providers import ChatMessage


# ─────────────────────────────────────────────────────────────────────────
# protocols.py · the three optional protocols
# ─────────────────────────────────────────────────────────────────────────

class AgentComponent(ABC):
    """Base class for every component.

    [→ s08: our Go counterpart is `type Component interface{}` — an
       EMPTY marker. Python uses an ABC to give shared lifecycle hooks
       (`enabled`, `_run_after`) and dependency-injection plumbing; we
       strip all that. A Go component is just any value the user
       chooses to call a Component.

       Why empty? Because:
         1. Capability is opted into via the sub-protocols below, not
            via inheritance — Go's structural typing handles this for
            free.
         2. A future need (e.g. naming, ordering hints) can grow the
            marker without breaking existing components.
         3. The empty marker still makes `[]Component` self-documenting
            — readers see "this is a list of components" not "this is
            interface{} with extra steps".]
    """

    enabled: bool = True


class CommandProvider(AgentComponent, ABC):
    """A component that contributes commands the agent can call.

    [→ s08: our Go counterpart:

         type CommandProvider interface {
             Commands() []Tool
         }

       Python uses `Iterator[Command]` for streaming; Go returns a
       slice because tool counts per component are tiny (1–10) and a
       slice is easier to test against. The `Tool` type is s01's
       interface (Schema + Execute) — the bus consumes Tools so the
       Registry it builds is fully runnable, not just schema-only.]
    """

    @abstractmethod
    def get_commands(self) -> Iterator[Command]:
        """Return commands this component provides."""
        ...


class DirectiveProvider(AgentComponent, ABC):
    """A component that contributes system-prompt directives.

    [→ s08: our Go counterpart:

         type DirectiveProvider interface {
             Directives() []string
         }

       Upstream splits directives into THREE methods (`get_constraints`,
       `get_resources`, `get_best_practices`) for finer control over
       prompt sectioning. We collapse those into one `Directives()`
       returning `[]string` — the OneShotStrategy renders them under a
       single `## Directives` header. A future strategy can split them
       again by accepting a richer return type without changing the
       protocol's call site.

       Pedagogically: separate sub-buckets for constraints/resources/
       best-practices is mostly prompt-engineering hygiene, and our
       agent is small enough that one bucket reads fine.]
    """

    @abstractmethod
    def get_constraints(self) -> Iterator[str]:
        """Lines like 'No user assistance' or 'Read before edit'."""
        ...

    @abstractmethod
    def get_resources(self) -> Iterator[str]:
        """Lines like 'Internet access for searches'."""
        ...

    @abstractmethod
    def get_best_practices(self) -> Iterator[str]:
        """Lines like 'Continuously refine actions'."""
        ...


class MessageProvider(AgentComponent, ABC):
    """A component that contributes pre-injected chat messages.

    [→ s08: our Go counterpart:

         type MessageProvider interface {
             Messages() []Message
         }

       Used in upstream for things like "current task description" and
       "pinned reminders" — content that should appear at the top of
       every prompt. We ship the protocol but use it only for the FIRST
       turn — subsequent turns rely on history rendering to avoid
       duplication. (See loop.go: the `turn == 0 && history is empty`
       guard.)]
    """

    @abstractmethod
    def get_messages(self) -> AsyncIterator[ChatMessage]:
        """Pre-injected messages."""
        ...


# ─────────────────────────────────────────────────────────────────────────
# Component discovery: how upstream Agent finds protocols
# ─────────────────────────────────────────────────────────────────────────

# Upstream Agent stores components as a list and iterates it for each
# protocol. The detection is `isinstance` checks:
#
#     for component in self.components:
#         if isinstance(component, CommandProvider):
#             yield from component.get_commands()
#
# [→ s08: our Go counterpart in component.go (ComponentBus):
#
#      func (b *ComponentBus) Registry() *Registry {
#          reg := NewRegistry()
#          for _, c := range b.components {
#              cp, ok := c.(CommandProvider)
#              if !ok { continue }
#              for _, tool := range cp.Commands() {
#                  reg.Register(tool)
#              }
#          }
#          return reg
#      }
#
#    Type assertion replaces isinstance. Same outcome; less ceremony.
#    The type-switch idiom (`if cp, ok := c.(X); ok`) is Go's
#    structural-typing answer to Python's runtime introspection.]


# ─────────────────────────────────────────────────────────────────────────
# components/file_manager/__init__.py · canonical multi-protocol example
# ─────────────────────────────────────────────────────────────────────────

class FileManagerComponent(DirectiveProvider, CommandProvider):
    """File management component — wraps a FileStorage (workspace).

    [→ s08: our Go counterpart `FileManagerComponent` in
       component_filemgr.go. Same shape: implements TWO protocols
       (Commands + Directives), wraps a Workspace at construction.

       Differences:
         1. Upstream's CommandProvider yields more tools (open_file,
            write_to_file, list_folder, write_to_existing_file) —
            we ship only read_file + write_file, which covers the
            agent's basic needs. List_folder is mentioned in our
            directive list as a forward-compat hook.

         2. Upstream's directives are longer (~5 lines) and cover
            multiple buckets (constraints + best_practices). We ship
            two short lines because the OneShotStrategy renders them
            into a numbered list and short bullets read better.

         3. Upstream stores per-agent workspace settings; we just
            capture a Workspace and let the caller decide where it
            points.]
    """

    def __init__(self, file_storage):
        self.workspace = file_storage

    def get_constraints(self) -> Iterator[str]:
        yield "Always read a file before editing it."
        yield "Use list_folder to discover before reading."

    def get_commands(self) -> Iterator[Command]:
        # In real upstream, these are decorated functions. Our Go
        # version returns []Tool from `Commands()` — same idea, no
        # decorator.
        yield self.read_file
        yield self.write_file
        yield self.list_folder
        yield self.write_to_existing_file

    @command(...)
    def read_file(self, file_path: str) -> str:
        ...

    @command(...)
    def write_file(self, file_path: str, content: str) -> None:
        ...


# ─────────────────────────────────────────────────────────────────────────
# Bridge: directives → system prompt
# ─────────────────────────────────────────────────────────────────────────

# Upstream's `OneShotPrompt.build_prompt` does:
#
#     constraints = list(self.agent.directives_constraints)
#     resources = list(self.agent.directives_resources)
#     best_practices = list(self.agent.directives_best_practices)
#     prompt = render_template(
#         "one_shot_system.j2",
#         constraints=constraints,
#         resources=resources,
#         best_practices=best_practices,
#         commands=commands,
#     )
#
# [→ s08: our Go counterpart in strategy.go (OneShotStrategy.BuildSystem):
#
#      func (s *OneShotStrategy) BuildSystem(tools []ToolSchema,
#                                            directives []string) string {
#          // ... role line + Commands section + Best practices section ...
#          if len(directives) > 0 {
#              b.WriteString("\n## Directives\n")
#              for i, d := range directives {
#                  fmt.Fprintf(&b, "%d. %s\n", i+1, d)
#              }
#          }
#      }
#
#    Differences:
#      1. We use Go string-builders + Sprintf, not Jinja2.
#      2. We collapse upstream's 3 directive buckets (constraints +
#         resources + best_practices) into ONE flat list in the
#         "## Directives" section. Our OneShotStrategy still has its
#         own static "## Best practices" section (5 default lines)
#         that's separate from component-supplied directives — those
#         live in "## Directives". This split makes it visible to the
#         operator which lines come from where.]


# ─────────────────────────────────────────────────────────────────────────
# Why type-assertion (Go) replaces ABC inheritance (Python)
# ─────────────────────────────────────────────────────────────────────────

# Python's protocol detection requires explicit `class Foo(CommandProvider)`
# inheritance. Go's structural typing means a type satisfies an interface
# just by having the right methods. So:
#
#   Python:  class FileManagerComponent(DirectiveProvider, CommandProvider): ...
#   Go:      type FileManagerComponent struct { ws Workspace }
#            func (f *FileManagerComponent) Commands() []Tool { ... }
#            func (f *FileManagerComponent) Directives() []string { ... }
#
# Go's structural typing means a third-party Component (built outside
# this package) can ALSO satisfy CommandProvider just by having a
# `Commands() []Tool` method. No registration step, no plugin file —
# the interface is the contract, the type assertion is the discovery.
#
# This is the architectural punchline of s08: components are pluggable
# precisely because they're self-describing. Add a new component
# struct, give it a `Commands()` method, drop it into
# NewComponentBus(...), and the agent gains the capability — no
# changes to Loop, no changes to Strategy, no changes to anything else.
