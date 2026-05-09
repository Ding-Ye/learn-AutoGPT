package main

import (
	"testing"
)

// TestFileManagerComponent_CommandsReturnsReadAndWrite — the core
// contract: FileManagerComponent emits exactly two tools, in the order
// "read_file" then "write_file". Order matters because directives below
// imply that ordering, and ComponentBus preserves component order when
// building the Registry.
func TestFileManagerComponent_CommandsReturnsReadAndWrite(t *testing.T) {
	ws, err := NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	c := NewFileManagerComponent(ws)

	cmds := c.Commands()
	if len(cmds) != 2 {
		t.Fatalf("Commands() len = %d, want 2", len(cmds))
	}
	if got := cmds[0].Schema().Name; got != "read_file" {
		t.Errorf("Commands()[0] name = %q, want \"read_file\"", got)
	}
	if got := cmds[1].Schema().Name; got != "write_file" {
		t.Errorf("Commands()[1] name = %q, want \"write_file\"", got)
	}

	// And both tools must share the same Workspace — verifiable by
	// writing through the write tool and reading through the read tool.
	// (We don't run them here — tools_file_test.go does that — we just
	// pin the wiring.)
	if cmds[0] == nil || cmds[1] == nil {
		t.Fatal("Commands returned a nil tool")
	}
}

// TestFileManagerComponent_DirectivesNonEmpty — Directives must produce
// at least one line, and each line must be non-empty. We pin the two
// shipped directives by stable substring so wording can evolve.
func TestFileManagerComponent_DirectivesNonEmpty(t *testing.T) {
	ws, err := NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	c := NewFileManagerComponent(ws)

	d := c.Directives()
	if len(d) < 2 {
		t.Fatalf("Directives len = %d, want >= 2", len(d))
	}
	for i, line := range d {
		if line == "" {
			t.Errorf("Directives[%d] is empty", i)
		}
	}

	// Two shipped directives — pin by substring.
	combined := ""
	for _, l := range d {
		combined += l + "\n"
	}
	for _, want := range []string{"read", "before"} {
		// "Always read a file before editing it." — substrings 'read' & 'before'
		if !contains(combined, want) {
			t.Errorf("Directives missing substring %q; got:\n%s", want, combined)
		}
	}
}

// contains is a tiny helper — strings.Contains avoiding the import in
// this test-only file. Cheap and clear.
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
