// tools.go — the Tool interface every session inherits, plus the s02
// builtins (EchoTool from s01 + MathTool, the second tool that lets the
// Registry's value be observable: you can't disambiguate one entry by
// name, you can disambiguate two).
//
// SECURITY NOTE: s02 still ships only side-effect-free tools. A real
// bash/exec tool is deferred to s06, where Workspace lands and we can
// sandbox shell access to a directory root.
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

// MathTool is the second builtin. It exists for one pedagogical reason:
// having two tools means the model — and the Registry — must
// *disambiguate by name*. With one tool, "look it up by name" and
// "always return the only tool we have" are observationally
// indistinguishable. Add MathTool and the registry's job becomes real.
//
// Operations are restricted to add/sub/mul/div over float64. We pin the
// set so an unknown operation surfaces a clear error the model can
// recover from, rather than evaluating arbitrary arithmetic strings.
type MathTool struct{}

func NewMathTool() *MathTool { return &MathTool{} }

func (m *MathTool) Schema() ToolSchema {
	return ToolSchema{
		Name:        "math",
		Description: "Evaluate a basic arithmetic operation (add | sub | mul | div) over two numbers. Returns the result as a string.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"operation": map[string]interface{}{
					"type":        "string",
					"description": "One of: add | sub | mul | div.",
					"enum":        []string{"add", "sub", "mul", "div"},
				},
				"a": map[string]interface{}{
					"type":        "number",
					"description": "Left operand.",
				},
				"b": map[string]interface{}{
					"type":        "number",
					"description": "Right operand.",
				},
			},
			"required": []string{"operation", "a", "b"},
		},
	}
}

func (m *MathTool) Execute(ctx context.Context, input map[string]interface{}) (string, error) {
	op, err := requireString(input, "operation")
	if err != nil {
		return "", fmt.Errorf("math: %w", err)
	}
	a, err := requireNumber(input, "a")
	if err != nil {
		return "", fmt.Errorf("math: %w", err)
	}
	b, err := requireNumber(input, "b")
	if err != nil {
		return "", fmt.Errorf("math: %w", err)
	}
	var out float64
	switch op {
	case "add":
		out = a + b
	case "sub":
		out = a - b
	case "mul":
		out = a * b
	case "div":
		// Divide-by-zero gets surfaced as a tool error so the model sees a
		// recoverable failure (and may choose different operands), instead
		// of a silent +Inf / NaN that would propagate downstream.
		if b == 0 {
			return "", fmt.Errorf("math: division by zero")
		}
		out = a / b
	default:
		return "", fmt.Errorf("math: unknown operation %q (want one of: add | sub | mul | div)", op)
	}
	// %g picks the shortest representation that round-trips — integers
	// render without a trailing `.0`, which is what models tend to expect.
	return fmt.Sprintf("%g", out), nil
}

// requireString / requireNumber centralize the "model produced wrong type"
// failure path. We accept JSON-decoded numbers (always float64) and ints
// (some marshalers). String parsing of numbers is intentionally NOT
// supported — if the model emits "2" instead of 2 we want to see the
// failure rather than mask it.
func requireString(input map[string]interface{}, key string) (string, error) {
	raw, ok := input[key]
	if !ok {
		return "", fmt.Errorf("missing required field %q", key)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("field %q must be string, got %T", key, raw)
	}
	return s, nil
}

func requireNumber(input map[string]interface{}, key string) (float64, error) {
	raw, ok := input[key]
	if !ok {
		return 0, fmt.Errorf("missing required field %q", key)
	}
	switch v := raw.(type) {
	case float64:
		return v, nil
	case float32:
		return float64(v), nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	}
	return 0, fmt.Errorf("field %q must be number, got %T", key, raw)
}
