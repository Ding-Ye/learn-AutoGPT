# Source: classic/forge/forge/permissions.py
# Upstream URL:
#   https://github.com/Significant-Gravitas/AutoGPT/blob/master/classic/forge/forge/permissions.py
# License: MIT (the classic/ subtree is MIT-licensed; only autogpt_platform/
# is under Polyform Shield 1.0).
#
# This file pulls upstream's `CommandPermissionManager` — the 4-level
# permission engine that gates every tool dispatch — into one annotated
# reading for s07 of learn-AutoGPT. Lines marked [→ s07] indicate which
# Go construct in this repo's session 7 teaches the corresponding
# upstream concept. Pydantic settings, workspace persistence, and the
# session_denied set are stripped where they distract from the
# pattern-matching + decision tree story.


from __future__ import annotations

import re
from enum import Enum
from pathlib import Path
from typing import Any, Callable

from forge.config.workspace_settings import AgentPermissions, WorkspaceSettings


# ─────────────────────────────────────────────────────────────────────────
# permissions.py · ApprovalScope (4-level decision)
# ─────────────────────────────────────────────────────────────────────────

class ApprovalScope(str, Enum):
    """Scope of permission approval.

    [→ s07: we collapse 4 scopes into a 3-valued `type Decision int
       (Allow / Deny / Ask)`. The simplification:

       upstream Python                |  our Go translation
       ───────────────────────────────┼──────────────────────────
       ONCE   (allow once)            |  Allow (returned by Asker)
       AGENT  (always for agent)      |  Allow (recorded in AllowList)
       WORKSPACE (always for ws)      |  Allow (recorded in AllowList)
       DENY   (deny)                  |  Deny

       The persistence-tier distinction (ONCE = ephemeral, AGENT =
       per-agent file, WORKSPACE = per-workspace file) is what makes
       upstream 4-level. Our s07 collapses them: any approved pattern
       lives in `Permissions.AllowList`, which the caller can persist
       to disk however they like (we ship `permissions.json` reader).
       The richer scope hierarchy is left as Appendix B exercise #5.]
    """

    ONCE = "once"          # Allow this one time only (not saved)
    AGENT = "agent"        # Always allow for this agent
    WORKSPACE = "workspace" # Always allow for all agents
    DENY = "deny"          # Deny this command


# ─────────────────────────────────────────────────────────────────────────
# permissions.py · CommandPermissionManager.check_command (THE entry point)
# ─────────────────────────────────────────────────────────────────────────

class CommandPermissionManager:
    """Manages layered permissions for agent command execution.

    Check order (first match wins):
    1. Agent deny list → block
    2. Workspace deny list → block
    3. Agent allow list → allow
    4. Workspace allow list → allow
    5. No match → prompt user

    [→ s07: corresponds to our Go `type Permissions struct { AllowList,
       DenyList []Pattern }` plus `Check(cmd, args) Decision`. We have
       ONE allow list and ONE deny list rather than {agent, workspace}
       splits — the simplification mirrors the scope collapse above.
       Same first-match-wins semantics; same deny-before-allow ordering.

       The `prompt_fn` callback parameter maps onto our `Asker`
       interface — see the bottom of this file for the bridge.]
    """

    def check_command(
        self, command_name: str, arguments: dict[str, Any]
    ) -> PermissionCheckResult:
        """Check if command execution is allowed. Prompts if needed.

        [→ s07: our `Permissions.Check(cmd string, args
           map[string]interface{}) Decision`. The decision tree below
           is the load-bearing logic — the Go version is structurally
           identical but with two lists instead of four.]
        """
        args_str = self._format_args(command_name, arguments)
        perm_string = f"{command_name}({args_str})"

        # 1. Check agent deny list
        # [→ s07: we have ONE deny list; no agent-vs-workspace split.
        #    `for _, pat := range p.DenyList { if patternMatches(...) }`]
        if self._matches_patterns(
            command_name, args_str, self.agent_permissions.permissions.deny
        ):
            return PermissionCheckResult(False, ApprovalScope.DENY)

        # 2. Check workspace deny list
        # [→ s07: collapsed into the same DenyList walk above.]
        if self._matches_patterns(
            command_name, args_str, self.workspace_settings.permissions.deny
        ):
            return PermissionCheckResult(False, ApprovalScope.DENY)

        # 3. Check agent allow list
        if self._matches_patterns(
            command_name, args_str, self.agent_permissions.permissions.allow
        ):
            return PermissionCheckResult(True, ApprovalScope.AGENT)

        # 4. Check workspace allow list
        # [→ s07: collapsed into one AllowList walk.]
        if self._matches_patterns(
            command_name, args_str, self.workspace_settings.permissions.allow
        ):
            return PermissionCheckResult(True, ApprovalScope.WORKSPACE)

        # 5. Check session denials (ephemeral DENY for this run)
        # [→ s07: not modeled. Our Loop emits a "denied" tool_result
        #    on any Deny decision; the agent observes the denial and
        #    typically stops asking. We don't carry a separate
        #    session-denied set because the model already sees the
        #    rejection in its history.]
        if perm_string in self._session_denied:
            return PermissionCheckResult(False, ApprovalScope.DENY)

        # 6. Prompt user (the Ask decision)
        # [→ s07: our `Decision == Ask` branch in Permissions.Check.
        #    The Loop then consults `Asker.Ask(cmd, args)`. If no
        #    Asker is configured, Loop returns an error rather than
        #    failing silently.]
        if self.prompt_fn is None:
            return PermissionCheckResult(False, ApprovalScope.DENY)

        scope, feedback = self.prompt_fn(command_name, args_str, arguments)
        # The prompt_fn returns a scope which the manager persists if
        # the user chose AGENT or WORKSPACE. Our s07 doesn't auto-
        # persist; the operator is expected to edit permissions.json
        # by hand. Persistence is left as Appendix B exercise #5.
        ...


# ─────────────────────────────────────────────────────────────────────────
# permissions.py · _pattern_matches (THE matcher)
# ─────────────────────────────────────────────────────────────────────────

    def _pattern_matches(self, pattern: str, cmd: str, args: str) -> bool:
        """Check if a single pattern matches the command.

        Args:
            pattern: Permission pattern like "command_name(glob_pattern)".
            cmd: Command name.
            args: Formatted arguments string.

        Returns:
            True if pattern matches.

        [→ s07: this is THE function s07 teaches. Our Go counterpart
           lives in `permissions.go` as `patternMatches(pattern, cmd,
           args)` plus `globMatch(pattern, input)`. Side-by-side:

           upstream Python                  |  our Go translation
           ─────────────────────────────────┼──────────────────────
           re.match(r"^(\\w+)\\((.+)\\)$",  |  idx := strings.Index(
                pattern)                    |       pattern, ":")
                                            |  cmdGlob := pattern[:idx]
                                            |  argGlob := pattern[idx+1:]

           Pattern syntax difference:

           Upstream uses `command_name(arg_pattern)` — parens. Our
           Go version uses `command_name: arg_pattern` — colon. Both
           are conventional; our `:` is easier to read in YAML/JSON
           config files where parens would conflict with array
           syntax.]
        """

        # Parse pattern: command_name(args_pattern)
        match = re.match(r"^(\w+)\((.+)\)$", pattern)
        if not match:
            return False

        pattern_cmd, args_pattern = match.groups()

        # Command name must match
        # [→ s07: same `if cmdGlob != "*" && cmdGlob != cmd { return false }`.
        #    We add the `cmdGlob == "*"` wildcard form for "any command";
        #    upstream's pattern grammar requires \w+ which forbids `*`.
        #    The wildcard form lets us write rules like "*: secret*" to
        #    catch every command whose args mention "secret".]
        if pattern_cmd != cmd:
            return False

        # Expand {workspace} placeholder
        # [→ s07: not modeled. Upstream lets you write
        #    "read_file({workspace}/data/*)" and the manager substitutes
        #    the absolute workspace root before matching. We don't do
        #    this in s07 — the model emits relative paths anyway, and
        #    the workspace sandbox in s06 already restricts paths.]
        args_pattern = args_pattern.replace("{workspace}", str(self.workspace))

        # Convert glob pattern to regex
        # ** matches any path (including /)
        # * matches any characters except /
        #
        # [→ s07: our Go version implements glob matching directly via
        #    a tiny custom matcher (`globMatch` in permissions.go),
        #    rather than translating to regex. Two reasons:
        #
        #      1. The Go regex package supports the same constructs
        #         but adding a regex compilation step per match is
        #         overkill for the small inputs we see (paths,
        #         shell strings, queries — all under 1KB).
        #
        #      2. A direct matcher is easier to read and debug —
        #         students can step through the recursive scan and
        #         see exactly which character the * is greedily
        #         matching at any point.
        #
        #    The semantics are identical: `*` won't span `/`, `**` will.]
        regex_pattern = args_pattern
        regex_pattern = re.escape(regex_pattern)
        regex_pattern = regex_pattern.replace(r"\*\*", ".*")
        regex_pattern = regex_pattern.replace(r"\*", "[^/]*")
        regex_pattern = f"^{regex_pattern}$"

        try:
            return bool(re.match(regex_pattern, args))
        except re.error:
            return False


# ─────────────────────────────────────────────────────────────────────────
# permissions.py · _format_args (the per-command arg renderer)
# ─────────────────────────────────────────────────────────────────────────

    def _format_args(self, command_name: str, arguments: dict[str, Any]) -> str:
        """Format command arguments for pattern matching.

        [→ s07: NOT modeled. Upstream has command-specific arg
           formatting:

             read_file/write_file/etc → resolved absolute path
             execute_shell/execute_python → "executable:rest"
             web_search → query string
             read_webpage → URL
             generic → ":".join(values)

           Our Go version is simpler: we treat EVERY string-valued
           entry in args as a candidate, and ANY match counts. This
           is more permissive (rules can't dodge by renaming a field)
           AND simpler (no per-command formatter table to keep in
           sync). The trade-off: we lose upstream's path canonicalization
           for file ops — a rule "read_file: notes.md" won't match a
           call with arg {"path": "./notes.md"} in our Go version,
           though it would in upstream after Path.resolve().

           Per the s06 doc: workspace-resolution lives in the
           Workspace.resolve() sanitizer, not in the permissions
           layer. So a future enhancement would be to call
           ws.Resolve(path) on string args before glob matching —
           but for s07 we keep the permissions module independent
           of workspace.]
        """
        # File ops: resolve and canonicalize the path
        if command_name in (
            "read_file", "write_file", "write_to_file", "create_file", "list_folder",
        ):
            path = arguments.get("filename") or arguments.get("path") or ""
            if path:
                p = Path(path)
                if not p.is_absolute():
                    p = self.workspace / p
                return str(p.resolve())
            return ""

        # Shell ops: format as "executable:rest"
        if command_name in ("execute_shell", "execute_python"):
            cmd = arguments.get("command_line") or arguments.get("code") or ""
            parts = str(cmd).split(maxsplit=1)
            if len(parts) == 2:
                return f"{parts[0]}:{parts[1]}"
            return f"{parts[0]}:"

        # Web ops, generic, etc. — see upstream for full list.
        ...


# ─────────────────────────────────────────────────────────────────────────
# Bridge: prompt_fn (upstream) ↔ Asker (s07 Go)
# ─────────────────────────────────────────────────────────────────────────

# Upstream signature:
#
#   prompt_fn: Callable[[str, str, dict], tuple[ApprovalScope, str | None]]
#
# Returns (scope, feedback). The feedback string is fed BACK to the
# agent via a `do_not_execute()` path when the user denies — i.e. the
# user can leave a one-shot note like "use 'mv' instead of 'rm'" that
# becomes part of the next prompt.
#
# [→ s07: our `Asker.Ask(cmd, args) Decision` is simpler. Returns just
#    Allow or Deny — no feedback channel. Justification:
#
#      1. The model already sees a structured "denied" tool_result on
#         deny, so it can adapt without an extra feedback string.
#
#      2. Adding a feedback string requires plumbing it through Loop +
#         Episode + RenderMessages — we leave that as a 1-line
#         extension exercise (add a `feedback string` return; route it
#         into ActionResult.Output).
#
#      3. The 4-level scope persistence (AGENT vs WORKSPACE) is the
#         other thing prompt_fn returns; we collapsed those above.
#
#    `StdinAsker` in our Go version reads y/N from stdin and returns
#    Allow/Deny accordingly. `StubAsker` for tests returns a canned
#    answer. Both implement the `Asker` interface.]
