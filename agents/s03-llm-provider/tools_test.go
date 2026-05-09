package main

import (
	"context"
	"strings"
	"testing"
)

func TestEchoTool_Execute_HappyPath(t *testing.T) {
	tool := NewEchoTool()
	out, err := tool.Execute(context.Background(), map[string]interface{}{"message": "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "hello" {
		t.Fatalf("got %q, want %q", out, "hello")
	}
}

func TestEchoTool_Execute_TypeError(t *testing.T) {
	tool := NewEchoTool()
	_, err := tool.Execute(context.Background(), map[string]interface{}{"message": 42})
	if err == nil {
		t.Fatal("expected error for non-string message, got nil")
	}
	// The error must explicitly mention the offending type so debugging
	// a misbehaving model is straightforward.
	if !strings.Contains(err.Error(), "string") || !strings.Contains(err.Error(), "int") {
		t.Fatalf("error %q must mention 'string' and the offending type 'int'", err.Error())
	}
}

func TestEchoTool_Execute_MissingField(t *testing.T) {
	tool := NewEchoTool()
	_, err := tool.Execute(context.Background(), map[string]interface{}{})
	if err == nil {
		t.Fatal("expected error for missing field, got nil")
	}
	if !strings.Contains(err.Error(), "message") {
		t.Fatalf("error %q must mention 'message'", err.Error())
	}
}

func TestEchoTool_Execute_EmptyString(t *testing.T) {
	tool := NewEchoTool()
	out, err := tool.Execute(context.Background(), map[string]interface{}{"message": ""})
	if err != nil {
		t.Fatalf("empty string should be valid, got error: %v", err)
	}
	if out != "" {
		t.Fatalf("got %q, want empty", out)
	}
}

func TestEchoTool_Schema(t *testing.T) {
	tool := NewEchoTool()
	s := tool.Schema()
	if s.Name != "echo" {
		t.Errorf("Name = %q, want %q", s.Name, "echo")
	}
	if !strings.Contains(s.Description, "Echo") {
		t.Errorf("Description %q does not mention 'Echo'", s.Description)
	}
	required, _ := s.InputSchema["required"].([]string)
	if len(required) != 1 || required[0] != "message" {
		t.Errorf("required fields = %v, want [\"message\"]", required)
	}
	props, _ := s.InputSchema["properties"].(map[string]interface{})
	if _, ok := props["message"]; !ok {
		t.Errorf("properties does not contain 'message': %v", props)
	}
}

// ============================================================================
// MathTool — the second builtin in s02 that makes the Registry observable.
// ============================================================================

func TestMathTool_Execute_Add(t *testing.T) {
	tool := NewMathTool()
	out, err := tool.Execute(context.Background(), map[string]interface{}{
		"operation": "add",
		"a":         2.0,
		"b":         3.0,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "5" {
		t.Fatalf("got %q, want %q", out, "5")
	}
}

func TestMathTool_Execute_Sub(t *testing.T) {
	tool := NewMathTool()
	out, err := tool.Execute(context.Background(), map[string]interface{}{
		"operation": "sub",
		"a":         10.0,
		"b":         4.0,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "6" {
		t.Fatalf("got %q, want %q", out, "6")
	}
}

func TestMathTool_Execute_Mul(t *testing.T) {
	tool := NewMathTool()
	out, err := tool.Execute(context.Background(), map[string]interface{}{
		"operation": "mul",
		"a":         6.0,
		"b":         7.0,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "42" {
		t.Fatalf("got %q, want %q", out, "42")
	}
}

func TestMathTool_Execute_Div(t *testing.T) {
	tool := NewMathTool()
	out, err := tool.Execute(context.Background(), map[string]interface{}{
		"operation": "div",
		"a":         9.0,
		"b":         2.0,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 9/2 = 4.5 — checks float formatting too.
	if out != "4.5" {
		t.Fatalf("got %q, want %q", out, "4.5")
	}
}

func TestMathTool_Execute_MissingArg(t *testing.T) {
	tool := NewMathTool()
	// Missing 'b' — error must call out the offending field.
	_, err := tool.Execute(context.Background(), map[string]interface{}{
		"operation": "add",
		"a":         1.0,
	})
	if err == nil {
		t.Fatal("expected error for missing 'b', got nil")
	}
	if !strings.Contains(err.Error(), "b") {
		t.Fatalf("error %q must mention missing 'b'", err.Error())
	}
}

func TestMathTool_Schema(t *testing.T) {
	tool := NewMathTool()
	s := tool.Schema()
	if s.Name != "math" {
		t.Errorf("Name = %q, want %q", s.Name, "math")
	}
	required, _ := s.InputSchema["required"].([]string)
	// add | sub | mul | div needs all three of operation/a/b.
	if len(required) != 3 {
		t.Errorf("required fields = %v, want 3", required)
	}
}

// Compile-time assertions — both tools must satisfy Tool. If the interface
// drifts, this fails at build time rather than the first test that runs it.
var (
	_ Tool = (*EchoTool)(nil)
	_ Tool = (*MathTool)(nil)
)
