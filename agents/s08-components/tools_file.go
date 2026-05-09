// tools_file.go — `read_file` and `write_file`, the first non-trivial tools.
//
// EchoTool (s01) and MathTool (s02) have NO side effects: they're pure
// functions. ReadFileTool and WriteFileTool are the first tools that
// actually *do* something with state — and they're also the first tools
// that take a constructor argument (a `Workspace`). That's the core
// pedagogical step in s06: tools that need access to a shared resource
// take it via the constructor, not via a global.
//
// Why pass a Workspace and not a `root string`? Because the same tool
// shape works for s07 (where a permissioned Workspace wraps the local
// one), s08 (where a FileManagerComponent emits these tools), and any
// future S3/cloud workspace. The interface is the seam.
package main

import (
	"context"
	"fmt"
)

// ReadFileTool exposes `Workspace.Read` to the LLM as a tool. Schema:
// `{ "path": "<relative path>" }`. Execute returns the file contents
// verbatim — large files are returned in full because truncation
// surprises model planning. Truncation should happen at a higher layer
// (e.g. an "open + skim head" tool added in s08) so the agent sees the
// boundary explicitly.
type ReadFileTool struct {
	ws Workspace
}

func NewReadFileTool(ws Workspace) *ReadFileTool {
	return &ReadFileTool{ws: ws}
}

func (r *ReadFileTool) Schema() ToolSchema {
	return ToolSchema{
		Name:        "read_file",
		Description: "Read a file from the workspace, by path RELATIVE to the workspace root. Returns the full file contents as a string.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Relative path within the workspace (e.g. \"notes.md\" or \"src/main.go\"). Absolute paths and `..` traversals are rejected.",
				},
			},
			"required": []string{"path"},
		},
	}
}

func (r *ReadFileTool) Execute(ctx context.Context, input map[string]interface{}) (string, error) {
	path, err := requireString(input, "path")
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	content, err := r.ws.Read(path)
	if err != nil {
		// The workspace already produces clean errors ("path escapes
		// root", "no such file"). Wrap to identify the tool but don't
		// re-decorate the cause.
		return "", fmt.Errorf("read_file: %w", err)
	}
	return content, nil
}

// WriteFileTool exposes `Workspace.Write`. Schema:
// `{ "path": "<relative path>", "content": "<file body>" }`. Returns a
// short status string `"wrote N bytes to <path>"` so the agent gets a
// concrete confirmation back through the tool_result channel.
//
// Why a status string and not just an empty success? Because Anthropic's
// tool_result content field is mandatory; an empty string still works,
// but a short concrete acknowledgment ("wrote 42 bytes to notes.md") is
// what the model uses to verify its own actions. AutoGPT upstream's
// `write_file` returns a similar status line.
type WriteFileTool struct {
	ws Workspace
}

func NewWriteFileTool(ws Workspace) *WriteFileTool {
	return &WriteFileTool{ws: ws}
}

func (w *WriteFileTool) Schema() ToolSchema {
	return ToolSchema{
		Name:        "write_file",
		Description: "Write a file into the workspace, by path RELATIVE to the workspace root. Parent directories are created automatically. Overwrites the file if it already exists.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Relative path within the workspace. Absolute paths and `..` traversals are rejected.",
				},
				"content": map[string]interface{}{
					"type":        "string",
					"description": "The file body to write.",
				},
			},
			"required": []string{"path", "content"},
		},
	}
}

func (w *WriteFileTool) Execute(ctx context.Context, input map[string]interface{}) (string, error) {
	path, err := requireString(input, "path")
	if err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}
	content, err := requireString(input, "content")
	if err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}
	if err := w.ws.Write(path, content); err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), path), nil
}

// Compile-time checks: both tools satisfy Tool. If the interface drifts,
// this fails at build rather than the first test that runs them.
var (
	_ Tool = (*ReadFileTool)(nil)
	_ Tool = (*WriteFileTool)(nil)
)
