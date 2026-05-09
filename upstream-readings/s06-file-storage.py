# Source: classic/forge/forge/file_storage/base.py
#         classic/forge/forge/file_storage/local.py
# Upstream URLs:
#   https://github.com/Significant-Gravitas/AutoGPT/blob/master/classic/forge/forge/file_storage/base.py
#   https://github.com/Significant-Gravitas/AutoGPT/blob/master/classic/forge/forge/file_storage/local.py
# License: MIT (the classic/ subtree is MIT-licensed; only autogpt_platform/
# is under Polyform Shield 1.0).
#
# This file pulls upstream's `FileStorage` ABC + `_sanitize_path` (the
# load-bearing path validator) + a thin `LocalFileStorage` slice into one
# annotated reading for s06 of learn-AutoGPT. Lines marked [→ s06]
# indicate which Go construct in this repo's session 6 teaches the
# corresponding upstream concept. Pydantic / asyncio imports, S3 / GCS
# subclasses, on_write_file event hooks, and binary-vs-text branches are
# stripped where they distract from the path-sanitization story.


# ─────────────────────────────────────────────────────────────────────────
# file_storage/base.py · FileStorage abstract base
# ─────────────────────────────────────────────────────────────────────────

class FileStorage(ABC):
    """A class that represents a file storage.

    [→ s06: corresponds to our Go `type Workspace interface { Read,
       Write, List }`. Upstream's surface is wider — open_file, exists,
       delete_file, rename, copy, make_dir, list_folders, plus
       clone_with_subroot for nested workspaces. We slice down to the
       three methods every later session actually consumes. The
       abstract-class shape (with @property + @abstractmethod) is
       Python's "you can't instantiate this without implementing all
       methods"; Go gets the same effect for free via interface
       satisfaction.]
    """

    on_write_file: Callable[[Path], Any] | None = None
    """Event hook, executed after writing a file.
    [→ s06: not modeled in our Go version. AutoGPT uses this to wire
       cloud-sync triggers (S3 backend uploads after each write); we
       leave it as future work — s10's pipeline hooks are a better
       place to add it than the Workspace surface.]"""

    @property
    @abstractmethod
    def root(self) -> Path:
        """The root path of the file storage.
        [→ s06: our LocalWorkspace stores `root string` (absolute, with
           trailing separator). Exposed via `Root()` for tests.]"""

    @property
    @abstractmethod
    def restrict_to_root(self) -> bool:
        """Whether to restrict file access to within the storage's root.
        [→ s06: our Go version is ALWAYS restricted. There's no flag to
           turn it off because `LocalWorkspace` exists specifically to
           sandbox the agent — an unrestricted workspace would just be
           `os` directly. Upstream's flag is a holdover from when
           FileStorage was used to manage the agent's *own* state files
           (settings, history) which legitimately live outside the
           workspace.]"""

    @abstractmethod
    def read_file(self, path: str | Path, binary: bool = False) -> str | bytes:
        """Read a file in the storage.
        [→ s06: our `Workspace.Read(path string) (string, error)`. We
           skip the binary mode — text-only is sufficient for s06's
           teaching scope. Upstream's binary path is needed for
           image_gen / code_executor; we'd add an io.Reader return
           when those land in s08.]"""

    @abstractmethod
    async def write_file(self, path: str | Path, content: str | bytes) -> None:
        """Write to a file in the storage.
        [→ s06: our `Workspace.Write(path, content string) error`. Sync
           rather than async — Go's blocking-by-default + ctx.Context
           covers the same ground without color. Upstream is async
           because `S3FileStorage.write_file` does an HTTP PUT; the
           local backend doesn't need it but inherits the contract.]"""

    @abstractmethod
    def list_files(self, path: str | Path = ".") -> list[Path]:
        """List all files (recursively) in a directory in the storage.
        [→ s06: our `Workspace.List(prefix string) ([]string, error)`.
           Upstream returns `list[Path]` — Path objects with the host's
           absolute prefix preserved. We return relative strings so the
           agent's prompt never leaks the host's filesystem layout. See
           the test `TestWorkspace_ListReturnsRelativePaths` for the
           assertion.]"""


# ─────────────────────────────────────────────────────────────────────────
# file_storage/base.py · _sanitize_path (THE load-bearing function)
# ─────────────────────────────────────────────────────────────────────────

def get_path(self, relative_path: str | Path) -> Path:
    """Get the full path for an item in the storage."""
    return self._sanitize_path(relative_path)


def _sanitize_path(self, path: str | Path) -> Path:
    """Resolve the relative path within the given root if possible.

    Raises:
        ValueError: If the path is absolute and a root is provided.
        ValueError: If the path is outside the root and the root is
                    restricted.

    [→ s06: this is THE function s06 teaches. Our Go counterpart
       lives in `workspace.go` as `LocalWorkspace.resolve(p string)
       (string, error)`. Side-by-side comparison:

       upstream Python                   |  our Go translation
       ──────────────────────────────────┼──────────────────────
       if "\\0" in str(path):            |  if strings.ContainsRune(p, 0)
           raise ValueError              |       return err

       relative_path.is_absolute() and   |  if filepath.IsAbs(p)
       not is_relative_to(self.root)     |       return err
           raise ValueError              |

       full = self.root / relative       |  cleaned = filepath.Clean(
       full = full.resolve()             |       filepath.Join(l.root, p))

       if not full.is_relative_to(self.root) |  if !strings.HasPrefix(
           raise ValueError                  |       cleaned, l.root)
                                             |       return err

       return full                       |  return cleaned, nil

       Three semantics-preserving differences:

         1. Upstream allows absolute paths IF they're already inside
            root (`is_relative_to(root)`). Our Go version flatly
            rejects every absolute — pedagogically clearer, and the
            agent has no legitimate reason to construct an absolute
            path when given a relative-rooted workspace.

         2. Upstream uses `Path.resolve()` (which follows symlinks)
            for local; we use `filepath.Clean` (no symlink follow).
            The trade-off: our Go version doesn't trip over symlinks
            *but* a symlink inside the workspace pointing outside it
            would let an attacker escape. Pin tests for s06 don't
            exercise symlinks; s07's permissions layer is where you'd
            add a `Lstat`-based check if you need it.

         3. Upstream stores `self.root` as a `Path` (no trailing
            separator); we store ours WITH a trailing separator so
            `HasPrefix` can't false-positive on prefix-aliased
            siblings (`/tmp/ws-evil/x` does NOT prefix-match
            `/tmp/ws/`). Same effect achieved by upstream's
            `is_relative_to` (which does the right thing); the
            string-prefix variant is the lower-level idiom Go offers.]
    """

    # Posix systems disallow null bytes in paths. Windows is agnostic
    # about it. Do an explicit check here for all sorts of null byte
    # representations.
    #
    # [→ s06: identical check in our Go version
    #    (`strings.ContainsRune(p, 0)`). Old C-string truncation
    #    defense — a path "safe.txt\\x00/etc/passwd" reads fine in Go
    #    or Python but lower-level libraries (e.g. SQLite, libc) may
    #    truncate at the null byte. Reject up front so we never make
    #    the syscall with the truncated form.]
    if "\0" in str(path):
        raise ValueError("Embedded null byte")

    logger.debug(f"Resolving path '{path}' in storage '{self.root}'")

    relative_path = Path(path)

    # Allow absolute paths if they are contained in the storage.
    #
    # [→ s06: our Go version DOESN'T allow this — every absolute path
    #    is rejected outright. See semantics-difference #1 above.
    #    Upstream's leniency is to support `FileStorage` instances
    #    that legitimately span the host's filesystem (e.g. agent
    #    config files in /etc/autogpt); we don't have that concern
    #    because Workspace is purely the agent-output sandbox.]
    if (
        relative_path.is_absolute()
        and self.restrict_to_root
        and not relative_path.is_relative_to(self.root)
    ):
        raise ValueError(
            f"Attempted to access absolute path '{relative_path}' "
            f"in storage '{self.root}'"
        )

    # Join + resolve. The resolve() call canonicalizes (follows
    # symlinks, collapses `..` and `.`).
    #
    # [→ s06: our Go version uses `filepath.Clean(filepath.Join(...))`
    #   instead of `Path.resolve()`. Clean does the `.`/`..` collapse
    #   but does NOT follow symlinks — see semantics-difference #2.]
    full_path = self.root / relative_path
    if self.is_local:
        full_path = full_path.resolve()
    else:
        # Cloud backends (S3/GCS) can't symlink-resolve; fall back to
        # textual normalization.
        full_path = Path(os.path.normpath(full_path))

    logger.debug(f"Joined paths as '{full_path}'")

    # The actual escape check. After resolution, the full path MUST
    # be inside self.root. If not, the input contained `..` (or a
    # symlink) that traversed out.
    #
    # [→ s06: our Go version uses
    #    `strings.HasPrefix(cleaned+sep, l.root)` — the trailing
    #    separator (note the `+sep`) is what defends against
    #    prefix-aliased siblings. See semantics-difference #3.]
    if self.restrict_to_root and not full_path.is_relative_to(self.root):
        raise ValueError(
            f"Attempted to access path '{full_path}' "
            f"outside of storage '{self.root}'."
        )

    return full_path


# ─────────────────────────────────────────────────────────────────────────
# file_storage/local.py · LocalFileStorage thin wrapper
# ─────────────────────────────────────────────────────────────────────────

class LocalFileStorage(FileStorage):
    """A class that represents a file storage on the local filesystem.

    [→ s06: corresponds to our Go `type LocalWorkspace struct { root
       string }`. The split between FileStorage (validation) and
       LocalFileStorage (I/O) is conventional in Python; Go's interface
       lets us co-locate both in `LocalWorkspace` without losing the
       seam — `Workspace` interface stays clean for s07/s08.]
    """

    def __init__(
        self,
        root_initializer: Path,
        restrict_to_root: bool = True,
    ):
        self._root = root_initializer.resolve()
        self._restrict_to_root = restrict_to_root

    @property
    def root(self) -> Path:
        return self._root

    @property
    def restrict_to_root(self) -> bool:
        return self._restrict_to_root

    @property
    def is_local(self) -> bool:
        return True

    def initialize(self) -> None:
        """Create the root if it doesn't exist.
        [→ s06: our `NewLocalWorkspace` does this in the constructor
           via `os.MkdirAll(abs, 0o755)` so callers don't need a
           separate Initialize step.]"""
        self.root.mkdir(parents=True, exist_ok=True)

    def read_file(self, path: str | Path, binary: bool = False) -> str | bytes:
        """Read a file in the storage."""
        # get_path → _sanitize_path is implicit; both methods route
        # through it.
        with self._open_file(path, "rb" if binary else "r") as file:
            return file.read()

    async def write_file(self, path: str | Path, content: str | bytes) -> None:
        """Write to a file in the storage.

        [→ s06: our `LocalWorkspace.Write(path, content string) error`
           does:

               abs := l.resolve(path)              # the sanitizer
               os.MkdirAll(filepath.Dir(abs), …)   # parent dirs
               os.WriteFile(abs, content, 0o644)   # actual write

           Same shape — sanitize, mkdir parents, write. Upstream uses
           the @asynccontextmanager `_open_file` helper to do all
           three; we inline because Go has no with-statement and the
           three lines are clearer than a helper.]"""
        with self._open_file(path, "wb" if type(content) is bytes else "w") as file:
            file.write(content)
        if self.on_write_file:
            self.on_write_file(Path(path))

    def list_files(self, path: str | Path = ".") -> list[Path]:
        """List all files (recursively) in a directory in the storage.

        [→ s06: our `LocalWorkspace.List(prefix string) ([]string, error)`
           uses `filepath.Walk` and `filepath.Rel` to return relative
           strings. Upstream returns `list[Path]` — Path objects with
           the host's absolute prefix preserved. The relative-only
           shape in our Go version is deliberate: see the
           `TestWorkspace_ListReturnsRelativePaths` test, where we
           assert the host's filesystem layout never leaks into the
           output.]"""
        # Implementation walks self.root with rglob, filters dirs.
        ...
