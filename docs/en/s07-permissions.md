---
title: "s07 · Layered permission system"
chapter: 7
slug: s07-permissions
est_read_min: 13
---

# s07 · Layered permission system

> What this teaches: on top of s06's sandboxed workspace, we add a second gate — every tool dispatch first goes through `Permissions.Check(cmd, args)` which returns `Allow / Deny / Ask`. We introduce `*` / `**` glob matching and an `Asker` interface (stdin in production, stub in tests). The Loop calls `Check` between `strategy.Parse` and `tool.Execute` — the canonical answer to the "cross-cutting concerns belong at the Loop's seams" anti-pattern.

---

## Problem

s06 taught the agent to keep its hands inside the workspace sandbox — any attempt to write `/etc/passwd` or `../../escape` is rejected on the spot by `LocalWorkspace.resolve()`. That plugs the outward-escape leak.

**But destructive power *inside* root is still uncovered**:

```
agent's tool_use:
{
  "name": "bash",
  "input": {"command": "rm -rf ."}
}
// Even if bash is sandboxed to ./workspace/, this wipes 6 sessions of work.
```

Or, more subtly:

```
{"name": "write_file", "input": {"path": ".ssh/authorized_keys", "content": "..."}}
// Path is relative to workspace, sanitizer doesn't catch it — but the
// agent is now touching SSH infrastructure inside its sandbox.
```

AutoGPT classic answers this in `classic/forge/forge/permissions.py`: a `CommandPermissionManager` with 4 levels of allow/deny lists (`ONCE` / `AGENT` / `WORKSPACE` / `DENY`), glob-pattern matching on `(command_name, args_str)`, and a decision tree of "deny lists first, allow lists second, prompt the user if neither matched".

s07 brings this into Go with a 2-level simplification:

1. `type Decision int (Allow / Deny / Ask)` — three-valued enum.
2. `Permissions{AllowList, DenyList []Pattern}` — one allow list, one deny list.
3. `Check(cmd, args) Decision` — the decision tree (deny before allow; no rule → Ask).
4. `Asker` interface — what to do on `Ask` (stdin prompt in production; stub in tests).

The full 4-level scope hierarchy (`ONCE` once-only, `AGENT` persists to agent settings, `WORKSPACE` persists to workspace settings, `DENY` denies) is collapsed to 2-level — **the full 4-level is left as Appendix B exercise #5**.

## Solution

```go
type Decision int
const (
    Allow Decision = iota
    Deny
    Ask
)

type Pattern struct {
    Glob string  // e.g. "read_file: *.md", "*: secret*", "bash: rm -rf**"
}

type Permissions struct {
    AllowList []Pattern
    DenyList  []Pattern
}

func (p *Permissions) Check(cmd string, args map[string]interface{}) Decision {
    for _, pat := range p.DenyList {
        if patternMatches(pat.Glob, cmd, args) { return Deny }
    }
    for _, pat := range p.AllowList {
        if patternMatches(pat.Glob, cmd, args) { return Allow }
    }
    return Ask
}

type Asker interface {
    Ask(cmd string, args map[string]interface{}) Decision  // returns Allow|Deny
}

type StdinAsker struct { ... }  // production: prompt y/N
type StubAsker struct { ... }   // test: return canned reply
```

Pattern format `"<command>: <arg-glob>"`:

| Pattern | Meaning |
|---|---|
| `read_file: *.md` | matches read_file when any string arg glob-matches `*.md` |
| `write_file: *` | matches write_file with any args (bare-* means "ignore args") |
| `*: secret*` | matches ANY command when any string arg starts with "secret" |
| `bash: rm -rf**` | matches bash when any string arg matches `rm -rf**` (`**` crosses `/`) |

Glob matcher rules:
- `*` matches one path segment (no `/`)
- `**` matches any number of segments (including `/`)
- `?` matches one character

The Loop calls `Check` before every tool_use dispatch: `Allow` proceeds; `Deny` synthesizes an `ActionResult{Status:"denied", Output:"permission denied: <cmd>"}` that the next turn's model sees; `Ask` delegates to the configured `Asker`.

## How It Works

```ascii-anim frames=4
┌────────────────────────────────────────────────────────────────────────┐
│ TURN N                                                                  │
│                                                                         │
│   provider response: tool_use {name=write_file, input={path=".ssh/x"}}  │
│        │                                                                │
│        ▼                                                                │
│   strategy.ParseResponse → ActionProposal{Command:"write_file", ...}    │
│        │                                                                │
│        ▼                                                                │
│   Permissions.Check("write_file", args)                                 │
│   ├─ DenyList: "write_file: **/.ssh/**"                                │
│   │     patternMatches → cmd="write_file" ✓ , glob ".ssh/x" matches → MATCH
│   │     return Deny                                                    │
│   └─ AllowList not consulted (deny short-circuits)                     │
│        │                                                                │
│        ▼                                                                │
│   Loop synthesizes ActionResult{Status:"denied",                       │
│                                  Output:"permission denied: write_file"}│
│        │                                                                │
│        ▼                                                                │
│   Episode.Results gets the denial                                      │
│                                                                         │
│ TURN N+1                                                                │
│   strategy.BuildPrompt sees the prior episode → renders a tool_result   │
│   block with content '{"status":"denied", "output":"..."}'              │
│        │                                                                │
│        ▼                                                                │
│   model adapts: "Sorry, I'll skip the .ssh write."                      │
└────────────────────────────────────────────────────────────────────────┘
```

### `Check` — Deny before Allow, otherwise Ask

```go
func (p *Permissions) Check(cmd string, args map[string]interface{}) Decision {
    for _, pat := range p.DenyList {     // 1. Deny is consulted first
        if patternMatches(pat.Glob, cmd, args) {
            return Deny                   //    First deny match short-circuits
        }
    }
    for _, pat := range p.AllowList {    // 2. Allow second
        if patternMatches(pat.Glob, cmd, args) {
            return Allow
        }
    }
    return Ask                            // 3. No rule → Ask
}
```

Why "Deny before Allow"? Because the operator wants to write **one broad allow** + **a few narrow denies** to carve dangerous holes:

```yaml
allow:
  - "bash: **"               # broad: bash is always allowed
deny:
  - "bash: rm -rf**"         # narrow: but rm -rf is always denied
  - "bash: dd**"             # and dd
```

If Allow were consulted first, the broad rule would return Allow before the narrow Deny ran — you'd never get a chance to evaluate the narrow Deny. So Deny must come first. AutoGPT upstream's `check_command` makes the same choice.

### `patternMatches` — command name + any string arg

```go
func patternMatches(pattern, cmd string, args map[string]interface{}) bool {
    idx := strings.Index(pattern, ":")
    cmdGlob := strings.TrimSpace(pattern[:idx])
    argGlob := strings.TrimSpace(pattern[idx+1:])

    // Command name: "*" is a wildcard, otherwise exact compare.
    if cmdGlob != "*" && cmdGlob != cmd { return false }

    // Bare-* or bare-** arg-glob: match regardless of args (cmd-only rule).
    if argGlob == "*" || argGlob == "**" { return true }

    // Otherwise: test EVERY string arg; if ANY one matches, the rule fires.
    for _, v := range args {
        if s, ok := v.(string); ok && globMatch(argGlob, s) {
            return true
        }
    }
    return false
}
```

The "ANY string-arg matches" logic is deliberately loose:

- Prevents the agent from dodging a rule by renaming `path` to `target`.
- A single rule covers multiple fields across multiple tools (`*: secret*` catches both `read_file{path:"secret-x"}` and `web_fetch{key:"secret-y"}`).
- Numbers, booleans, nested objects don't participate — string-only is the deliberate scope.

### `globMatch` — custom matcher, not `path/filepath.Match`

```go
// * matches one segment (no /); ** crosses segments; ? matches one char.
func globMatch(pattern, input string) bool {
    pi, ii := 0, 0
    starPi, starII := -1, -1
    doubleStar := false
    // ... recursive scan, mark backtrack point on *, ** allows /
}
```

Why not `path/filepath.Match`?

1. It doesn't support `**` (cross-segment) — and `**` is the load-bearing distinction we need.
2. Its escape rules differ across platforms (Windows vs POSIX).
3. Our inputs are not necessarily paths — they may be URLs, shell commands, query strings. `filepath.Match` is documented as a *path* matcher.

Implementation in `permissions.go::globMatch` — about 30 lines with comments, worst-case O(len(pattern)*len(input)). Agent inputs are small.

### Three non-obvious points

1. **"No rule matched" returns `Ask`, not `Deny`.**

   You might think "no rule = deny" is safer, but that breaks the "Asker holds the actual policy" design. `Permissions.Check` only expresses **what the rules say**; when the rules are silent, *the actual policy* (prompt? deny? fail-closed?) is held by `Asker`. This means:

   - Tests can plug in `StubAsker(Deny)` — equivalent to "no rule = deny".
   - Production plugs in `StdinAsker` — prompts the user.
   - Future s09 can plug in a RichUI `Asker` — opens a GUI confirmation dialog.

   The `Permissions` ↔ `Asker` boundary is the "facts vs policy" line.

2. **Loop gates between `strategy.Parse` and `tool.Execute`, NOT inside `Tool.Execute`.**

   The naive write puts the check inside each Tool:

   ```go
   // anti-pattern
   func (w *WriteFileTool) Execute(ctx, input) (string, error) {
       if !w.perms.Allow(input) { return "", errors.New("denied") }
       // ...
   }
   ```

   Problems:

   - N tools = N copies of the same check.
   - Any tool that forgets the check is a hole.
   - Every tool's tests must mock `Permissions`.
   - Cross-cutting concerns (permission / logging / audit / rate-limit) pollute every Tool's core logic.

   Right answer: the gate lives in the Loop — **one place, one check, every tool goes through it**. This is what dossier anti-pattern #2 is about.

3. **The denied `ActionResult` MUST be visible to the model.**

   `Loop.runTools` does NOT silently skip on deny — it synthesizes an `ActionResult{Status:"denied", ...}` and appends it to the Episode. On the next turn, `RenderMessages` renders that into a tool_result block, so the model sees "my last action got denied" and can adapt. `history.go::renderResult`'s default branch handles the `denied` status (JSON-encoding status + output).

   If we silently skipped, the model would assume the tool succeeded and might continue based on a wrong assumption. Letting the model see denials is what makes **the LLM handle its own fallback**.

## What Changed vs s06

```diff
 agents/s07-permissions/
 ├── provider.go              # verbatim from s06
 ├── provider_openai.go       # verbatim
 ├── provider_mock.go         # verbatim
 ├── provider_*_test.go       # verbatim
 ├── tools.go / tools_test.go # verbatim
 ├── tools_file.go / _test.go # verbatim
 ├── workspace.go / _test.go  # verbatim
 ├── registry.go / _test.go   # verbatim
 ├── strategy.go / _test.go   # verbatim
 ├── history.go / _test.go    # verbatim
+├── permissions.go           # NEW: Decision, Pattern, Permissions, Check, globMatch, Asker
+├── permissions_test.go      # NEW: 8 tests
 ├── loop.go                  # MODIFIED: adds Permissions / Asker fields; runTools gates dispatch
 ├── loop_test.go             # MODIFIED: s06 tests + 2 permission-gate tests
 └── main.go                  # MODIFIED: load ./permissions.json (or defaults) + -ask flag
```

Type catalog additions:

```go
type Decision int
type Pattern    struct{ Glob string }
type Permissions struct{ AllowList, DenyList []Pattern }
type Asker interface { Ask(cmd, args) Decision }
type StdinAsker struct{ ... }
type StubAsker  struct{ ... }
```

Net new on `Loop`: 2 fields — `Permissions *Permissions` and `Asker Asker`. Both nil-safe — nil `Permissions` reverts to s06 behavior (no gate); nil Asker paired with nil Permissions is also fine; an error only triggers if Permissions is non-nil and a `Check` returns Ask but Asker is nil. This kind of "backward-compatible extension" is the cleanest shape for s06 → s07's incremental teaching.

## Try It

```bash
cd agents/s07-permissions

# 1. Default rules (read/write/echo/math allowed; no deny)
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "create notes.md with a one-line summary of the agent loop"

# 2. With sample permissions file (testdata/permissions.yaml — JSON shape)
cp testdata/permissions.yaml ./permissions.json
go run . -v -ask stdin "read README.md, then write a paraphrase to notes.md"
# Any tool not whitelisted triggers a stdin y/N prompt.

# 3. fail-closed mode: unknown tools auto-denied
go run . -v "do something I haven't whitelisted"
# Default -ask=deny so unmatched calls go Asker→Deny→record denied result.

# 4. Run all tests
go test -v ./...
```

Expected output (default rules, file write succeeds):

```
[s07-permissions] provider=anthropic model=claude-sonnet-4-6 ... permissions=defaults ask=deny
[turn 0] assistant: I'll write the summary to notes.md.
[turn 0] proposal: cmd=write_file thoughts="..."
[turn 0] permission check: write_file → Allow
[turn 0] -> write_file map[content:"agent loop = think → act → observe" path:notes.md]
[turn 0] <- wrote 36 bytes to notes.md
[turn 1] assistant: Done.
Done.
```

With `testdata/permissions.yaml` enabled, when the agent tries to write an SSH key:

```
[turn 0] permission check: write_file → Deny
[turn 0]    (matched DenyList rule "write_file: **/.ssh/**")
[turn 1] assistant: Sorry, that path is restricted. I'll write to a normal location instead.
```

### Try a "dangerous" tool

Add a `bash` tool to the registry in `main.go` (don't commit), then run:

```bash
go run . -v -ask stdin "list files in ./workspace using bash"
# Loop prints, before bash dispatches:
# [turn 0] permission check: bash → Ask
# permission required: bash(map[command:ls -la ./workspace]) [y/N]:
```

Type `n` to deny. That's the practical value of the `Asker` interface: the *human* is in the loop before the LLM agent actually runs anything.

## Upstream Source Reading

AutoGPT classic's permission system is in `classic/forge/forge/permissions.py` — a `CommandPermissionManager` class with 4-level scope. The fully-annotated bilingual reading lives at [`upstream-readings/s07-permissions.py`](../../upstream-readings/s07-permissions.py); this section pulls only the load-bearing parts.

```upstream:classic/forge/forge/permissions.py
# Source: classic/forge/forge/permissions.py
# Simplified: stripped Pydantic settings, workspace persistence,
# session_denied set; kept the 4-level Scope enum, check_command
# decision tree, and _pattern_matches matcher.

class ApprovalScope(str, Enum):
    """Scope of permission approval."""
    ONCE = "once"          # allow this one time only (not saved)
    AGENT = "agent"        # always allow for this agent (writes agent settings)
    WORKSPACE = "workspace" # always allow for ALL agents (writes ws settings)
    DENY = "deny"          # deny

    # [→ s07: we collapse 4 scopes into a 3-valued Decision (Allow/Deny/Ask).
    #    The persistence-tier distinction (ONCE = ephemeral, AGENT = file,
    #    WORKSPACE = file) is the essence of upstream's 4-level — s07
    #    drops persistence and lets the caller decide (we ship a
    #    `permissions.json` reader as one form of persistence). Full
    #    4-level is Appendix B exercise #5.]
```

```upstream:classic/forge/forge/permissions.py
class CommandPermissionManager:
    """Manages layered permissions for agent command execution.

    Check order (first match wins):
    1. Agent deny list → block
    2. Workspace deny list → block
    3. Agent allow list → allow
    4. Workspace allow list → allow
    5. No match → prompt user
    """

    def check_command(self, command_name, arguments) -> PermissionCheckResult:
        args_str = self._format_args(command_name, arguments)

        # 1. Agent deny
        if self._matches_patterns(command_name, args_str,
                                   self.agent_permissions.permissions.deny):
            return PermissionCheckResult(False, ApprovalScope.DENY)
        # 2. Workspace deny
        # 3. Agent allow
        # 4. Workspace allow → 4 passes; deny first, allow second

        # 5. No match → prompt user
        if self.prompt_fn is None:
            return PermissionCheckResult(False, ApprovalScope.DENY)
        scope, feedback = self.prompt_fn(command_name, args_str, arguments)
        ...

    # [→ s07: same decision tree in our Permissions.Check, but with one
    #    pair of lists (AllowList / DenyList) rather than four.
    #    `prompt_fn` ↔ our `Asker.Ask(cmd, args) Decision` — simplified
    #    signature (no feedback string return, no scope persistence).
    #    See upstream-readings file for the bridge details.]
```

```upstream:classic/forge/forge/permissions.py
def _pattern_matches(self, pattern, cmd, args) -> bool:
    """Check if a single pattern matches the command."""
    # Parse pattern: command_name(args_pattern)
    match = re.match(r"^(\w+)\((.+)\)$", pattern)
    if not match: return False
    pattern_cmd, args_pattern = match.groups()
    if pattern_cmd != cmd: return False

    # Convert glob → regex:
    #   ** matches any path (including /)
    #   * matches any characters except /
    args_pattern = re.escape(args_pattern)
    args_pattern = args_pattern.replace(r"\*\*", ".*")
    args_pattern = args_pattern.replace(r"\*", "[^/]*")
    return bool(re.match(f"^{args_pattern}$", args))

# [→ s07: our patternMatches + globMatch. Differences:
#    1) Pattern syntax "cmd(arg)" → "cmd: arg" (cleaner in YAML/JSON)
#    2) We implement globMatch directly, not via regex
#       (small inputs + clearer to step through + cross-platform stable)
#    3) We support cmdGlob == "*" for "any command";
#       upstream's \w+ regex disallows it.]
```

### Side-by-side reading notes

- **4-level → 2-level simplification.** Upstream's `ONCE` / `AGENT` / `WORKSPACE` / `DENY` distinguishes **persistence tiers** (one-time, agent-scoped, workspace-scoped), not different decision logic. Our Go version cares only about "is this allowed now"; "remember next time" is the caller's problem (write `permissions.json`).

- **Different arg-rendering strategies.** Upstream has per-command `_format_args`: `read_file` gets the *resolved absolute path*; `execute_shell` gets `executable:rest`. Our Go version takes the looser approach: test each string arg, ANY match wins. This prevents agents from dodging a rule by renaming a field, but loses upstream's path canonicalization (`{"path":"./notes.md"}` won't match a rule `read_file: notes.md` in our version). If you want canonicalization, do it in the Workspace layer (s06's `resolve` already canonicalizes paths); keep permissions layer name-only.

- **`{workspace}` placeholder not implemented.** Upstream lets you write `read_file({workspace}/data/*)` to reference the workspace root. We didn't: (1) the model emits relative paths anyway (s06 enforces this), (2) it would couple Permissions to Workspace. If you need it, add a `(p *Permissions) WithWorkspace(ws Workspace)` and substitute `{workspace}` in `patternMatches` entry.

- **Asker interface vs prompt_fn callback.** A function callback `Callable[[str,str,dict], tuple[scope,feedback]]` and an `Asker` interface are equivalent in Python. Go picks the interface — idiomatic dependency injection. Tests get a `StubAsker`, production gets `StdinAsker`, s09 might get a `RichUIAsker` — the seam is uniform.

- **session_denied set not implemented.** Upstream uses `set[str]` to remember "this perm_string was denied this session" so it doesn't re-prompt. We didn't, because the model **sees** the denied tool_result (s07's key design choice) and usually stops retrying. If you run a stubborn open-weight model that doesn't, add an `LRU<string, time.Time>` to `Loop` and pre-check before `Check`.

**Read more**: [`upstream-readings/s07-permissions.py`](../../upstream-readings/s07-permissions.py) has the full 4-level decision-tree commentary plus the Go translation table and Asker bridge notes. Then preview s08: by the end of s07, `Loop` has 7 fields (Provider, Tools, Strategy, History, Workspace, Permissions, Asker) — s08 refactors these "capability bundles" into `Component`s, where each component exposes `Commands()` / `Messages()` / `Directives()` via type-assertions. The Loop collects all components and auto-builds the Registry + system prompt.

---

**Next chapter preview**: s08 introduces a `Component` marker interface and three optional sub-interfaces (`CommandProvider`, `MessageProvider`, `DirectiveProvider`). A `ComponentBus` aggregates capability-bundles via type-switch; `Loop` takes only `*ComponentBus` — fields collapse from 7 back to 2 (Provider + Bus). Two example components: `FileManagerComponent` (wraps Workspace + emits read_file/write_file + adds a directive) and `WebFetchComponent` (emits a `web_fetch` tool).
