// tools.go — the Tool interface every session inherits and the s01 builtins.
//
// SECURITY NOTE: s01 ships ONLY the EchoTool. A real bash/exec tool is
// deferred to s06, where Workspace lands and we can sandbox shell access
// to a directory root. Wiring an unsandboxed BashTool here would teach
// readers a habit that ranges from "bad idea" to "rm -rf /". Don't.
package main

import (
	"context"
	"fmt"
)

// Tool is the contract between Loop and any executable capability.
// Schema describes the tool to the LLM (name, prose, JSON-schema input);
// Execute receives whatever the model produced for `input` and returns a
// string (since tool_result content is stringly-typed on the wire).
//
// Locked across all sessions — later chapters add more tools, never
// rename the methods.
type Tool interface {
	Schema() ToolSchema
	Execute(ctx context.Context, input map[string]interface{}) (string, error)
}

// EchoTool is the smallest tool that exercises the full round-trip:
// Loop → tool_use → Execute → tool_result → Loop. With no side effects,
// it makes the *protocol* observable in tests without any real I/O.
type EchoTool struct{}

func NewEchoTool() *EchoTool { return &EchoTool{} }

func (e *EchoTool) Schema() ToolSchema {
	return ToolSchema{
		Name:        "echo",
		Description: "Echo back the input message verbatim. Useful for testing the tool-use round-trip without side effects.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"message": map[string]interface{}{
					"type":        "string",
					"description": "The message to echo back verbatim.",
				},
			},
			"required": []string{"message"},
		},
	}
}

func (e *EchoTool) Execute(ctx context.Context, input map[string]interface{}) (string, error) {
	raw, ok := input["message"]
	if !ok {
		return "", fmt.Errorf("echo: missing required field %q", "message")
	}
	// The model occasionally produces non-string scalars where a string is
	// expected (numbers, booleans). Reject explicitly so the failure mode
	// is loud rather than a silent fmt.Sprint coercion.
	msg, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("echo: field %q must be string, got %T", "message", raw)
	}
	return msg, nil
}
