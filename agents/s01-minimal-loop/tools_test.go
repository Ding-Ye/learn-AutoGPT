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

// Compile-time assertion that EchoTool satisfies Tool. If the interface
// signature ever drifts, this fails loudly at build time rather than at
// the first test that exercises it.
var _ Tool = (*EchoTool)(nil)
