---
title: "s06 · Sandboxed workspace storage"
chapter: 6
slug: s06-workspace
est_read_min: 12
---

# s06 · Sandboxed workspace storage

> What this teaches: give the agent file IO, but only inside a sandbox directory it can never escape. A `Workspace` interface (`Read` / `Write` / `List`) plus a `LocalWorkspace` implementation whose `filepath.Clean` + prefix-check rejects `../` traversal, absolute paths, and null bytes. We also ship the *first non-trivial tools* — `read_file` and `write_file` — both of which take a `Workspace` as a constructor argument. That dependency-injection pattern is the seam s07 (permissions) and s08 (FileManager component) build on.

---

## Problem

Through s05, every tool is a side-effect-free pure function: `echo` returns the input verbatim, `math` computes `a OP b`. An agent like that is at best an LLM-wrapped calculator — *it cannot leave any trace in the world*. Letting the agent write a file is the dividing line between "demo toy" and "tool that does something useful".

But just handing it `os.WriteFile` is a bad idea. One ordinary prompt injection or one hallucinated path is all it takes:

```go
// Agent meant to write "summary.md", but the model wandered:
os.WriteFile("/etc/passwd", "...", 0o644)
// or:
os.WriteFile("../../../home/user/.ssh/authorized_keys", "...", 0o600)
```

AutoGPT classic's solution lives in `classic/forge/forge/file_storage/`: a `FileStorage` ABC defines all file ops, a `LocalFileStorage` impl binds to a `root: Path` and `restrict_to_root: bool`, and most importantly a `_sanitize_path` method (in `base.py`) that rejects null bytes, rejects unauthorized absolute paths, then `Path.resolve()`s and checks `is_relative_to(root)`.

s06 brings this into Go:

1. The `Workspace` interface (three methods, minimum-viable).
2. A `LocalWorkspace` impl plus a `resolve()` path sanitizer.
3. `read_file` / `write_file` tools (two new tools that take a `Workspace` at construction).

The last point is what introduces the "constructor-injected dependency" pattern: from now on, any tool that needs an outside resource takes it via the constructor, never via a global. s07's `Permissions` will follow the same path.

## Solution

```go
type Workspace interface {
    Read(path string) (string, error)
    Write(path, content string) error
    List(prefix string) ([]string, error)
}

type LocalWorkspace struct {
    root string // absolute, cleaned, with trailing separator
}

func NewLocalWorkspace(root string) (*LocalWorkspace, error) {
    abs, _ := filepath.Abs(root)
    abs = filepath.Clean(abs)
    os.MkdirAll(abs, 0o755)         // mkdir if absent
    return &LocalWorkspace{root: abs + string(filepath.Separator)}, nil
}

// The path sanitizer — the load-bearing function of s06.
func (l *LocalWorkspace) resolve(p string) (string, error) {
    if p == "" { return "", fmt.Errorf("path is empty") }
    if strings.ContainsRune(p, 0) { return "", fmt.Errorf("path contains null byte") }
    if filepath.IsAbs(p) { return "", fmt.Errorf("absolute path not allowed: %q", p) }
    cleaned := filepath.Clean(filepath.Join(l.root, p))
    if !strings.HasPrefix(cleaned + string(filepath.Separator), l.root) &&
       cleaned + string(filepath.Separator) != l.root {
        return "", fmt.Errorf("path escapes root: %q -> %q", p, cleaned)
    }
    return cleaned, nil
}
```

Three methods on the interface, one struct + constructor for `LocalWorkspace`, one private `resolve` method holding all the safety checks.

```go
type ReadFileTool  struct{ ws Workspace }
type WriteFileTool struct{ ws Workspace }

func NewReadFileTool(ws Workspace) *ReadFileTool   // takes the interface, not a string
func NewWriteFileTool(ws Workspace) *WriteFileTool
```

Tools take a `Workspace` interface as their constructor argument. That gets us:

- Tests can mock cheaply (a `LocalWorkspace` rooted at `t.TempDir()` is enough — no mock framework).
- s07's permission-wrapper can decorate the same interface.
- s08's `FileManagerComponent` will emit these same two tools to the agent loop.

## How It Works

```ascii-anim frames=3
┌──────────────────────────────────────────────────────────────────┐
│  Agent: "write notes.md with content 'agent loop = think→act…'" │
│        │                                                          │
│        ▼                                                          │
│  WriteFileTool.Execute(input{path: "notes.md", content: "..."})  │
│        │                                                          │
│        ▼                                                          │
│  ws.Write("notes.md", "agent loop ...")                          │
│        │                                                          │
│        ▼                                                          │
│  LocalWorkspace.resolve("notes.md")                              │
│   ├─ p == ""              ?  no                                  │
│   ├─ ContainsRune(p, 0)   ?  no                                  │
│   ├─ filepath.IsAbs(p)    ?  no                                  │
│   ├─ Join(root, p)        →  /tmp/abc/workspace/notes.md         │
│   ├─ Clean                →  /tmp/abc/workspace/notes.md         │
│   ├─ HasPrefix(cleaned+/, /tmp/abc/workspace/)  ?  yes ✓         │
│   └─ return cleaned                                               │
│        │                                                          │
│        ▼                                                          │
│  os.MkdirAll(filepath.Dir(abs), 0o755)                           │
│  os.WriteFile(abs, []byte("agent loop ..."), 0o644)              │
│        │                                                          │
│        ▼                                                          │
│  return "wrote 35 bytes to notes.md"                             │
└──────────────────────────────────────────────────────────────────┘
```

### `resolve()` — three early-outs + one Clean+prefix

```go
func (l *LocalWorkspace) resolve(p string) (string, error) {
    if p == "" {
        return "", fmt.Errorf("path is empty")
    }
    if strings.ContainsRune(p, 0) {
        return "", fmt.Errorf("path contains null byte")
    }
    if filepath.IsAbs(p) {
        return "", fmt.Errorf("absolute path not allowed: %q", p)
    }
    cleaned := filepath.Clean(filepath.Join(l.root, p))
    if !strings.HasPrefix(cleaned+string(filepath.Separator), l.root) &&
        cleaned+string(filepath.Separator) != l.root {
        return "", fmt.Errorf("path escapes root: %q -> %q", p, cleaned)
    }
    return cleaned, nil
}
```

Four attack vectors, four defenses:

| Attack | Where it dies |
|---|---|
| `path = ""` | early return "path is empty" |
| `path = "x\x00/etc/passwd"` | `ContainsRune(p, 0)` → reject |
| `path = "/etc/passwd"` | `filepath.IsAbs` → reject |
| `path = "../../etc/passwd"` | After `Join + Clean` the literal `..` is gone but the resolved path is outside root, so `HasPrefix` fails |

### Three non-obvious points

1. **You MUST Clean before you prefix-check; literal `..` rejection alone is porous.**

   A naive "ban the substring `..`" fails on inputs like:

   ```
   path = "ok/sub/../../escape.txt"
   //  → naively: "../" appears AFTER "ok/sub/", so a literal-..-ban
   //    sees "..` only as part of `/../" preceded by something else
   //  → after filepath.Clean: "../escape.txt"
   //  → HasPrefix(/tmp/ws/../escape.txt, /tmp/ws/) → false → reject
   ```

   String scanning is not enough; you have to *resolve* the path then check the resolved location.

2. **The trailing separator on root is a detail that matters.**

   Imagine root is stored as `/tmp/ws` (no separator). If we just check the raw stored value:

   ```
   path = "../ws-evil/secret"
   cleaned = "/tmp/ws-evil/secret"
   HasPrefix("/tmp/ws-evil/secret", "/tmp/ws") → TRUE ✗
   ```

   We'd accept it! Storing the root as `/tmp/ws/` (with separator), and appending the separator to the path being checked (`cleaned+/`), kills this prefix-aliasing class of bugs. That's why our `LocalWorkspace.root` always has the trailing separator.

3. **`List` MUST return relative paths, never absolute.**

   `os.ReadDir` gives you `info.Name()` — relative, fine. But `filepath.Walk` gives you absolute. If we return those directly, the agent sees `/private/tmp/abc.../workspace/foo.md` — *which leaks the host's filesystem layout to the LLM*. Next turn it might try to write that absolute path (caught by `IsAbs`), but worse: it now knows roughly where your workspace lives. So `List` internally calls `filepath.Rel(l.root, p)` on every walked file before returning. The test asserts the host root never appears in any returned string.

## What Changed vs s05

```diff
 agents/s06-workspace/
 ├── provider.go              # verbatim from s05
 ├── provider_openai.go       # verbatim
 ├── provider_mock.go         # verbatim
 ├── provider_anthropic_test.go  # verbatim
 ├── provider_openai_test.go  # verbatim
 ├── provider_mock_test.go    # verbatim
 ├── tools.go / tools_test.go # verbatim
 ├── registry.go / registry_test.go  # verbatim
 ├── strategy.go / strategy_test.go  # verbatim
 ├── history.go / history_test.go    # verbatim
 ├── loop.go / loop_test.go   # verbatim
+├── workspace.go             # NEW: Workspace iface, LocalWorkspace, resolve()
+├── workspace_test.go        # NEW: 5 tests
+├── tools_file.go            # NEW: ReadFileTool, WriteFileTool (constructor takes Workspace)
+├── tools_file_test.go       # NEW: 4 tests
 └── main.go                  # MODIFIED: construct LocalWorkspace, register read_file + write_file
```

Type catalog additions:

```go
type Workspace interface {
    Read(path string) (string, error)
    Write(path, content string) error
    List(prefix string) ([]string, error)
}

type LocalWorkspace struct { root string }
```

**`Loop` is unchanged.** That's the design payoff of s06: adding a tool family doesn't touch the Loop — you only need `Registry.Register`. This is exactly what the s02 Registry abstraction earned for us: "explicit registration" makes "add a new tool" an import-then-Register concern, not a Loop-modification concern.

## Try It

```bash
cd agents/s06-workspace

# Anthropic native + oneshot (default); workspace defaults to ./workspace/
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "create notes.md with the sentence: agent loop = think → act → observe"

# DeepSeek + custom workspace dir
export DEEPSEEK_API_KEY=sk-...
go run . -provider deepseek -workspace /tmp/agent-out -v \
  "write a haiku about agents to poem.md, then read it back"

# Local vLLM / SGLang
export OPENAI_API_KEY=anything
go run . -provider local -model llama-3.3 \
  "list files in the workspace, then write index.md summarizing them"

# Run all tests (60+ should pass)
go test -v ./...
```

Expected output (Anthropic path, write-then-read task):

```
[s06-workspace] provider=anthropic model=claude-sonnet-4-6 url= strategy=oneshot tools=4 workspace=/Users/yeding/learn-AutoGPT/agents/s06-workspace/workspace
[turn 0] assistant: I'll write a haiku to poem.md first.
[turn 0] proposal: cmd=write_file thoughts="I'll write a haiku ..."
[turn 0] -> write_file map[content:"agent watches\nLLM whispers next move\ntool returns truth\n" path:poem.md]
[turn 0] <- wrote 51 bytes to poem.md
[turn 1] assistant: Now I'll read it back to verify.
[turn 1] proposal: cmd=read_file
[turn 1] -> read_file map[path:poem.md]
[turn 1] <- agent watches\nLLM whispers next move\ntool returns truth\n
[turn 2] assistant: Done. The haiku is saved to poem.md.
Done. The haiku is saved to poem.md.
[s06-workspace] final history: 2 episodes
```

`./workspace/poem.md` actually exists now. This is the first time the agent has left a side effect in the world since s01 — that's what s06 is for.

To watch the path sanitizer earn its keep, try:

```bash
go run . -v "write '/etc/passwd' with content 'pwned'"
# → [turn 0] <- error: write_file: absolute path not allowed: "/etc/passwd"
# The model sees the error in tool_result and the next turn either
# switches to a relative path or gives up.
```

## Upstream Source Reading

AutoGPT classic's file storage lives in `classic/forge/forge/file_storage/` — `base.py` is the ABC plus the path sanitizer; `local.py` is the local impl. The fully-annotated bilingual reading is at [`upstream-readings/s06-file-storage.py`](../../upstream-readings/s06-file-storage.py); this section pulls the load-bearing parts.

```upstream:classic/forge/forge/file_storage/base.py
# Source: classic/forge/forge/file_storage/base.py
# Simplified: stripped Pydantic config, async, binary branches, event hook;
# kept FileStorage ABC and _sanitize_path the path sanitizer.

class FileStorage(ABC):
    """A class that represents a file storage.

    [→ s06: corresponds to our Go `type Workspace interface { Read,
       Write, List }`. Upstream's surface is wider — open_file, exists,
       delete_file, rename, copy, make_dir, list_folders,
       clone_with_subroot. We slice down to three methods, the
       minimum-viable for s06's teaching scope.]"""

    on_write_file: Callable[[Path], Any] | None = None
    """Event hook fired after a write.
    [→ s06: not modeled. AutoGPT uses this to wire S3-backend cloud
       sync. We leave it as future work — s10's pipeline hooks are a
       better place to add it than the Workspace surface.]"""

    @property
    @abstractmethod
    def restrict_to_root(self) -> bool:
        """Whether to restrict access to within root.
        [→ s06: our Go version is ALWAYS restricted. There's no flag
           because LocalWorkspace exists specifically to sandbox.
           Upstream's flag is a holdover from when FileStorage also
           managed agent state files (settings, history) which
           legitimately live outside the workspace.]"""

    @abstractmethod
    def read_file(self, path: str | Path, binary: bool = False) -> str | bytes: ...

    @abstractmethod
    async def write_file(self, path: str | Path, content: str | bytes) -> None: ...

    @abstractmethod
    def list_files(self, path: str | Path = ".") -> list[Path]: ...
```

```upstream:classic/forge/forge/file_storage/base.py
# _sanitize_path — THE function s06 teaches.

def _sanitize_path(self, path: str | Path) -> Path:
    """Resolve the relative path within the given root if possible.

    Raises:
        ValueError: If absolute path escapes root, or post-resolution
                    path is outside root.
    """

    # Posix systems disallow null bytes in paths. Windows is agnostic.
    # [→ s06: same check via strings.ContainsRune(p, 0). Old C-string
    #    truncation defense — a path "safe.txt\\x00/etc/passwd" reads
    #    fine in Go or Python but lower-level libs (libc, sqlite) may
    #    truncate at the null byte. Reject up front.]
    if "\0" in str(path):
        raise ValueError("Embedded null byte")

    relative_path = Path(path)

    # Upstream allows absolute paths IF already inside root. Our Go
    # version flatly rejects every absolute path — semantically
    # cleaner, and the agent has no legitimate reason to construct an
    # absolute path when given a relative-rooted workspace.
    # [→ s06: filepath.IsAbs(p) → reject unconditionally.]
    if (
        relative_path.is_absolute()
        and self.restrict_to_root
        and not relative_path.is_relative_to(self.root)
    ):
        raise ValueError(...)

    # Join + resolve. resolve() canonicalizes (follows symlinks,
    # collapses `..` and `.`).
    # [→ s06: filepath.Clean(filepath.Join(l.root, p)). Clean does
    #    `..`/`.` collapse but does NOT follow symlinks — semantics
    #    difference #2; see upstream-readings file for the full
    #    discussion.]
    full_path = self.root / relative_path
    if self.is_local:
        full_path = full_path.resolve()
    else:
        full_path = Path(os.path.normpath(full_path))

    # The actual escape check. After resolution, the full path MUST
    # be inside self.root. If not, the input contained `..` (or a
    # symlink) that traversed out.
    # [→ s06: strings.HasPrefix(cleaned+sep, l.root). Trailing sep
    #    matters — semantics difference #3.]
    if self.restrict_to_root and not full_path.is_relative_to(self.root):
        raise ValueError(...)

    return full_path
```

### Side-by-side reading notes

- **Upstream's `Path.resolve()` vs our `filepath.Clean`.** `resolve()` follows symlinks; `Clean` does not. Trade-off: our Go version doesn't get confused by symlinks — *but* a symlink inside the workspace pointing outside it would let an attacker escape. s06's tests don't exercise symlinks; s07's permissions layer is a more natural place to add an `Lstat`-based check if you need it.

- **Upstream's root has no trailing separator + uses `is_relative_to`.** `Path.is_relative_to(root)` does the "treat root as a directory, the checked path must be inside it" check internally — equivalent to our trailing-separator-and-prefix string check. Python ships the high-level API; Go picks the lower-level `HasPrefix` route, but the semantics line up.

- **Upstream's `restrict_to_root` is optional.** Our Go version has no such flag. Reason: a `Workspace` is, by design, a sandbox in s06. An unrestricted "workspace" is just `os` calls — that's not a `Workspace`. Upstream needs the flag because `FileStorage` also manages agent state files (settings.yaml, history JSON) that legitimately exist outside the workspace; we don't conflate these two roles.

- **Upstream's `LocalFileStorage.write_file` is `async`.** Because the same ABC adapts `S3FileStorage` (HTTP PUTs must be async). The local impl inherits the contract but doesn't need it. Go has no color-mismatch problem (`ctx.Context` does the same job), so our Workspace is sync.

- **We don't implement `clone_with_subroot`.** Used to nest sub-workspaces (e.g., one per agent in a multi-agent setup). s06 doesn't need it; add it if your s09 multi-agent experiments require it.

**Read more**: [`upstream-readings/s06-file-storage.py`](../../upstream-readings/s06-file-storage.py) gives the fully-annotated bilingual excerpt of base.py + local.py with line-by-line Go cross-references. Then preview s07: workspace blocks the agent from writing *outside* root, but `rm -rf .` *inside* root would still wipe out 6 sessions of work. s07's `Permissions` layer adds allow/deny pattern matching on commands and arguments.

---

**Next chapter preview**: s07 introduces a `Permissions` struct with allow/deny patterns + glob matching, plus a `Check(cmd, args)` gate the Loop calls between strategy.Parse and tool.Execute. Decision is `Allow / Deny / Ask`. AutoGPT upstream's `CommandPermissionManager` is 4-level (`ONCE` / `AGENT` / `WORKSPACE` / `DENY`); we ship a 2-level subset, leaving the full 4-level scopes as Appendix B exercise #5.
