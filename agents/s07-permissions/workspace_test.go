package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestWorkspace_ReadAfterWrite — the smallest happy-path. Construct a
// workspace, Write, Read, assert round-trip equality. This proves the
// constructor mkdir's the root, that Write creates parents, and that
// Read returns the bytes verbatim.
func TestWorkspace_ReadAfterWrite(t *testing.T) {
	ws, err := NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	if err := ws.Write("notes/today.md", "# hello s06"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := ws.Read("notes/today.md")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != "# hello s06" {
		t.Errorf("Read content = %q, want %q", got, "# hello s06")
	}
}

// TestWorkspace_RejectsParentTraversal — `../../escape.txt` MUST fail
// even though no individual component is the literal `..` token before
// path-cleaning. AutoGPT's bug history shows this is the most common
// jailbreak vector — a single `..` ban without Clean+prefix-check is
// porous (Clean unwinds the literal sequence; you have to reject after
// resolving).
func TestWorkspace_RejectsParentTraversal(t *testing.T) {
	ws, err := NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	for _, badPath := range []string{
		"../escape.txt",
		"../../escape.txt",
		"sub/../../escape.txt",
		"a/b/../../../escape.txt",
	} {
		if err := ws.Write(badPath, "x"); err == nil {
			t.Errorf("Write(%q) should have rejected traversal, got nil", badPath)
		} else if !strings.Contains(err.Error(), "escapes root") {
			t.Errorf("Write(%q) error %q must mention 'escapes root'", badPath, err.Error())
		}
		if _, err := ws.Read(badPath); err == nil {
			t.Errorf("Read(%q) should have rejected traversal, got nil", badPath)
		}
	}
}

// TestWorkspace_RejectsAbsolutePath — a model that produces an absolute
// path must be refused outright. `/etc/passwd` is the canonical bait.
// Sanitizer error must NOT silently rewrite the path to a sibling under
// root — that would mask the model's mistake.
func TestWorkspace_RejectsAbsolutePath(t *testing.T) {
	ws, err := NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	for _, badPath := range []string{
		"/etc/passwd",
		"/tmp/anything",
		"/var/log/foo.log",
	} {
		if err := ws.Write(badPath, "pwned"); err == nil {
			t.Errorf("Write(%q) should have rejected absolute path, got nil", badPath)
		} else if !strings.Contains(err.Error(), "absolute") {
			t.Errorf("Write(%q) error %q must mention 'absolute'", badPath, err.Error())
		}
	}
}

// TestWorkspace_ListReturnsRelativePaths — the agent should never see
// the host's filesystem layout. List must return paths relative to the
// root, sorted alphabetically (so prompt rendering stays deterministic).
//
// We write three files in a nested layout, then List the whole tree
// and assert that (a) every entry is relative, (b) the order is sorted,
// and (c) no leaked absolute prefix is present anywhere.
func TestWorkspace_ListReturnsRelativePaths(t *testing.T) {
	root := t.TempDir()
	ws, err := NewLocalWorkspace(root)
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	files := map[string]string{
		"a.txt":          "one",
		"b/c.txt":        "two",
		"b/d/e.txt":      "three",
		"z/last.md":      "four",
	}
	for p, c := range files {
		if err := ws.Write(p, c); err != nil {
			t.Fatalf("Write %q: %v", p, err)
		}
	}
	got, err := ws.List("")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != len(files) {
		t.Fatalf("List len = %d (%v), want %d", len(got), got, len(files))
	}
	// Each entry must be relative — no leading slash, no leak of `root`.
	for _, p := range got {
		if filepath.IsAbs(p) {
			t.Errorf("List returned absolute path %q (must be relative to root)", p)
		}
		if strings.Contains(p, root) {
			t.Errorf("List entry %q leaks the host root path %q", p, root)
		}
	}
	// Order must be sorted.
	want := []string{"a.txt", "b/c.txt", "b/d/e.txt", "z/last.md"}
	// On Windows the separator would be \\; we run on POSIX in CI so
	// just check verbatim. (Add a filepath.ToSlash if we ever cross-test.)
	for i, w := range want {
		if got[i] != w {
			t.Errorf("List[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// TestWorkspace_RejectsNullByte — null bytes in paths are an old C-string
// truncation trick: a path "safe.txt\x00/etc/passwd" reads fine in Go but
// gets truncated by any underlying C library to "safe.txt". Reject up
// front so we never make the syscall.
func TestWorkspace_RejectsNullByte(t *testing.T) {
	ws, err := NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	if err := ws.Write("safe\x00.txt", "x"); err == nil {
		t.Error("Write with null byte should fail, got nil error")
	} else if !strings.Contains(err.Error(), "null byte") {
		t.Errorf("error %q must mention 'null byte'", err.Error())
	}
	if _, err := ws.Read("safe\x00.txt"); err == nil {
		t.Error("Read with null byte should fail, got nil error")
	}
}
