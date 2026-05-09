package main

import (
	"context"
	"strings"
	"testing"
)

// newTestWorkspace returns a LocalWorkspace rooted at t.TempDir(). The
// host directory is automatically cleaned up after the test, so each
// test starts with a fresh empty workspace.
func newTestWorkspace(t *testing.T) *LocalWorkspace {
	t.Helper()
	ws, err := NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	return ws
}

// TestReadFileTool_HappyPath — write a file via the workspace directly,
// then read it via the tool. This exercises the Schema → Execute → Read
// flow and proves the tool propagates the workspace's value verbatim.
func TestReadFileTool_HappyPath(t *testing.T) {
	ws := newTestWorkspace(t)
	if err := ws.Write("notes.md", "# s06 workspace"); err != nil {
		t.Fatalf("seed Write: %v", err)
	}
	tool := NewReadFileTool(ws)

	out, err := tool.Execute(context.Background(), map[string]interface{}{"path": "notes.md"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out != "# s06 workspace" {
		t.Errorf("Execute returned %q, want %q", out, "# s06 workspace")
	}

	// Schema sanity — name + required field.
	s := tool.Schema()
	if s.Name != "read_file" {
		t.Errorf("Schema().Name = %q, want %q", s.Name, "read_file")
	}
	required, _ := s.InputSchema["required"].([]string)
	if len(required) != 1 || required[0] != "path" {
		t.Errorf("required = %v, want [\"path\"]", required)
	}
}

// TestWriteFileTool_HappyPath — invoke the tool with a path and content,
// assert it returns "wrote N bytes to <path>" and the file is actually
// readable via the workspace afterward.
func TestWriteFileTool_HappyPath(t *testing.T) {
	ws := newTestWorkspace(t)
	tool := NewWriteFileTool(ws)

	out, err := tool.Execute(context.Background(), map[string]interface{}{
		"path":    "out/result.txt",
		"content": "hello world",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Status must mention byte count and path.
	if !strings.Contains(out, "wrote 11 bytes") {
		t.Errorf("status %q must contain 'wrote 11 bytes'", out)
	}
	if !strings.Contains(out, "out/result.txt") {
		t.Errorf("status %q must mention the path", out)
	}

	// And the file actually landed where we said.
	got, err := ws.Read("out/result.txt")
	if err != nil {
		t.Fatalf("Read after Write: %v", err)
	}
	if got != "hello world" {
		t.Errorf("Read = %q, want %q", got, "hello world")
	}
}

// TestReadFileTool_MissingFile — sad path 1: read a path that doesn't
// exist. The tool must return an error mentioning the path so the agent
// sees the failure mode and can recover (e.g., by listing the dir first).
func TestReadFileTool_MissingFile(t *testing.T) {
	ws := newTestWorkspace(t)
	tool := NewReadFileTool(ws)

	_, err := tool.Execute(context.Background(), map[string]interface{}{"path": "does-not-exist.txt"})
	if err == nil {
		t.Fatal("Execute on missing file should error, got nil")
	}
	if !strings.Contains(err.Error(), "does-not-exist.txt") {
		t.Errorf("error %q should mention the missing path", err.Error())
	}
}

// TestWriteFileTool_RejectsOutsideRoot — sad path 2: a malicious or
// confused model emits a `..` traversal. The tool MUST refuse, and the
// error must be loud enough that the agent can recover (so we surface
// the sanitizer's "escapes root" message verbatim through the wrapper).
func TestWriteFileTool_RejectsOutsideRoot(t *testing.T) {
	ws := newTestWorkspace(t)
	tool := NewWriteFileTool(ws)

	_, err := tool.Execute(context.Background(), map[string]interface{}{
		"path":    "../escape.txt",
		"content": "pwned",
	})
	if err == nil {
		t.Fatal("Execute with traversal should error, got nil")
	}
	if !strings.Contains(err.Error(), "escapes root") {
		t.Errorf("error %q must mention 'escapes root'", err.Error())
	}

	// And absolute paths.
	_, err = tool.Execute(context.Background(), map[string]interface{}{
		"path":    "/etc/passwd",
		"content": "pwned",
	})
	if err == nil {
		t.Fatal("Execute with absolute path should error, got nil")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error %q must mention 'absolute'", err.Error())
	}
}
