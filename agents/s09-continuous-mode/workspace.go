// workspace.go — sandboxed file storage.
//
// AutoGPT classic restricts file ops to a workspace root via
// `forge/file_storage/local.py::LocalFileStorage._sanitize_path`. Three
// rejections:
//
//   - null bytes (defends against C-string truncation in lower libs);
//   - absolute paths (so a model can't write `/etc/passwd`);
//   - parent-traversal (so `../escape.txt` can't break out).
//
// The single-rejection isn't enough on its own: a string like `foo/../../x`
// passes a naive `..` ban because `..` follows a non-`..` segment, but
// after `filepath.Clean` resolves to `../x` which still escapes. So we
// compose: clean-then-prefix-check is the load-bearing step; the literal
// rejections in `resolve()` are belt-and-suspenders.
//
// This file introduces the `Workspace` interface and one implementation.
// The interface is what s07 (permissions over file paths), s08 (a
// FileManager component that wraps a Workspace), and s10 (Reflexion's
// AfterParse hook may inspect the path before write) all consume.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Workspace is the contract every file-touching tool depends on. Three
// methods:
//
//   - Read returns the contents of `path`, relative to the workspace root.
//   - Write puts `content` at `path`, creating parent directories as needed.
//   - List returns all files under `prefix` (a relative directory), with
//     paths returned ALSO relative to the root — never absolute, never
//     containing the host's actual workspace location.
//
// All three methods MUST reject paths that escape the root. The contract
// for an out-of-root path is "return an error", not "silently clamp" —
// silent clamping would mask a buggy or adversarial input.
type Workspace interface {
	Read(path string) (string, error)
	Write(path, content string) error
	List(prefix string) ([]string, error)
}

// LocalWorkspace stores files under a single host directory. The `root`
// field is always absolute, cleaned (no trailing slash from filepath.Clean
// on most platforms), and the constructor mkdir's it if absent.
//
// We append a trailing separator to the stored root so the prefix check
// in `resolve` can't false-positive on directories whose names share a
// prefix — e.g. root `/tmp/ws` would otherwise accept `/tmp/ws-evil/...`
// because `HasPrefix("/tmp/ws-evil/x", "/tmp/ws")` is true. Adding the
// separator turns the comparison into "must be inside this directory".
type LocalWorkspace struct {
	root string // absolute, cleaned, with trailing separator
}

// NewLocalWorkspace builds a workspace rooted at `root`, creating the
// directory if it doesn't exist. Returns an error only if the path
// can't be made absolute or the directory can't be created.
//
// We don't reject relative `root` arguments — `filepath.Abs` resolves
// them against the current working directory. This matches AutoGPT
// upstream's behavior where `LocalFileStorage(Path("./workspace"))` is
// the common construction.
func NewLocalWorkspace(root string) (*LocalWorkspace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root %q: %w", root, err)
	}
	abs = filepath.Clean(abs)
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir workspace root %q: %w", abs, err)
	}
	// Append the platform separator so prefix checks cleanly mean
	// "inside this directory" (see LocalWorkspace doc above).
	return &LocalWorkspace{root: abs + string(filepath.Separator)}, nil
}

// Root returns the absolute path of the workspace root (without the
// trailing separator) — handy for tests and for surfacing the path in
// CLI banners.
func (l *LocalWorkspace) Root() string {
	return strings.TrimRight(l.root, string(filepath.Separator))
}

// resolve is the load-bearing path sanitizer. Mirrors AutoGPT's
// `_sanitize_path` (`forge/file_storage/local.py`) with one tweak: the
// upstream version raises on `..` *literal* tokens, then runs Clean,
// then checks restriction. We collapse the literal-`..` ban into the
// post-Clean prefix check: `Join(root, p)` followed by `Clean` and a
// prefix-on-root-with-separator check is sufficient on its own AND
// is harder to typo-bypass than maintaining a separate `..` reject.
//
// Three early-out rejections keep error messages clear:
//
//	empty string             → "path is empty"
//	contains \x00 (null)     → "path contains null byte"
//	absolute (begins with /) → "absolute path not allowed"
//
// Then Join+Clean+HasPrefix decides whether the *resolved* path stays
// inside root. A `../../etc/passwd` would Clean to something starting
// with `/etc/` (above root); HasPrefix fails; we error.
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
	if !strings.HasPrefix(cleaned+string(filepath.Separator), l.root) && cleaned+string(filepath.Separator) != l.root {
		return "", fmt.Errorf("path escapes root: %q -> %q", p, cleaned)
	}
	return cleaned, nil
}

// Read returns the contents of the file at `path` relative to the root.
// Errors fall into three buckets: sanitizer rejection (escapes root /
// null byte / absolute), filesystem error (file missing, permission),
// or successful read with the bytes returned as a string.
func (l *LocalWorkspace) Read(path string) (string, error) {
	abs, err := l.resolve(path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("read %q: %w", path, err)
	}
	return string(data), nil
}

// Write puts `content` at the resolved path, creating parent directories
// as needed. Files get mode 0o644; directories 0o755 — same defaults as
// AutoGPT upstream's local.py. We pin 0o644 rather than respecting the
// process umask so the agent's writes are predictable across hosts.
func (l *LocalWorkspace) Write(path, content string) error {
	abs, err := l.resolve(path)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(abs); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir for %q: %w", path, err)
		}
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	return nil
}

// List walks all files under `prefix` (resolved against root) and returns
// their paths RELATIVE to the workspace root. We return relative paths
// (not absolute) because the agent shouldn't see — much less learn — the
// host's filesystem layout. A workspace at `/tmp/abc123/workspace` is an
// implementation detail; the agent only knows `notes.md`, `data/foo.txt`.
//
// An empty `prefix` lists everything under root. The result is sorted
// alphabetically so tests and the agent's prompt rendering are
// deterministic.
func (l *LocalWorkspace) List(prefix string) ([]string, error) {
	// Resolve prefix the same way we resolve any path; an empty prefix
	// becomes the root itself (treated as ".").
	target := l.root
	if prefix != "" && prefix != "." {
		abs, err := l.resolve(prefix)
		if err != nil {
			return nil, err
		}
		target = abs
	}
	var out []string
	err := filepath.Walk(target, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		// Trim the root prefix and any leading separator so the result
		// is a clean relative path. filepath.Rel handles platform-
		// specific separator differences for us.
		rel, err := filepath.Rel(strings.TrimRight(l.root, string(filepath.Separator)), p)
		if err != nil {
			return err
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list %q: %w", prefix, err)
	}
	sort.Strings(out)
	return out, nil
}

// Compile-time check.
var _ Workspace = (*LocalWorkspace)(nil)
