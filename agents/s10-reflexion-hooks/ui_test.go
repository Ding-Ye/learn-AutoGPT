package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestConsoleUI_RenderThought_WritesPrefixedLine verifies the
// "💭 <text>" line shape the docs promise.
func TestConsoleUI_RenderThought_WritesPrefixedLine(t *testing.T) {
	var buf bytes.Buffer
	ui := NewConsoleUI(&buf)

	ui.RenderThought("I should read the file first")
	got := buf.String()

	if !strings.Contains(got, "💭") {
		t.Errorf("RenderThought output %q must contain the thought emoji", got)
	}
	if !strings.Contains(got, "I should read the file first") {
		t.Errorf("RenderThought output %q must contain the input text", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("RenderThought output %q must end with newline", got)
	}

	// Empty input should be silent — no spurious "💭" line.
	buf.Reset()
	ui.RenderThought("")
	if buf.Len() != 0 {
		t.Errorf("RenderThought(\"\") wrote %q, want empty", buf.String())
	}
	buf.Reset()
	ui.RenderThought("   ")
	if buf.Len() != 0 {
		t.Errorf("RenderThought of whitespace-only wrote %q, want empty", buf.String())
	}
}

// TestConsoleUI_RenderResult_OkAndError exercises both branches: ok
// renders "✓ ..." and any non-ok status renders "✗ ...".
func TestConsoleUI_RenderResult_OkAndError(t *testing.T) {
	var buf bytes.Buffer
	ui := NewConsoleUI(&buf)

	ui.RenderResult(ActionResult{Status: "ok", Output: "hello"})
	if got := buf.String(); !strings.Contains(got, "✓") || !strings.Contains(got, "hello") {
		t.Errorf("ok render = %q, want contains ✓ and \"hello\"", got)
	}

	buf.Reset()
	ui.RenderResult(ActionResult{Status: "error", Output: "boom"})
	got := buf.String()
	if !strings.Contains(got, "✗") {
		t.Errorf("error render = %q, want contains ✗", got)
	}
	if !strings.Contains(got, "error") {
		t.Errorf("error render = %q, want contains status \"error\"", got)
	}
	if !strings.Contains(got, "boom") {
		t.Errorf("error render = %q, want contains \"boom\"", got)
	}

	// "denied" is also a non-ok status; should render with ✗ prefix.
	buf.Reset()
	ui.RenderResult(ActionResult{Status: "denied", Output: "permission denied: bash"})
	if got := buf.String(); !strings.Contains(got, "✗") || !strings.Contains(got, "denied") {
		t.Errorf("denied render = %q, want contains ✗ and \"denied\"", got)
	}
}

// TestConsoleUI_Spinner_StopFnIsCallable verifies the spinner writes a
// busy line and the stop fn clears it. Idempotency: calling stop twice
// must not double-write.
func TestConsoleUI_Spinner_StopFnIsCallable(t *testing.T) {
	var buf bytes.Buffer
	ui := NewConsoleUI(&buf)

	stop := ui.Spinner("Thinking")
	if got := buf.String(); !strings.Contains(got, "[busy]") || !strings.Contains(got, "Thinking") {
		t.Errorf("spinner start = %q, want contains \"[busy] Thinking\"", got)
	}

	// Stop fn must run without panic and emit a CR-clear sequence.
	stop()
	out := buf.String()
	if !strings.Contains(out, "\r") {
		t.Errorf("spinner stop = %q, want contains carriage return for clear", out)
	}

	// Second stop is a no-op (idempotent).
	prevLen := buf.Len()
	stop()
	if buf.Len() != prevLen {
		t.Errorf("second stop wrote %d more bytes; want idempotent", buf.Len()-prevLen)
	}
}

// TestNoopUI_RecordsCallsInOrder is a sanity check on the test helper
// itself: NoopUI.Calls must show events in the order they happened so
// the interaction-loop tests can assert thought-before-result ordering.
func TestNoopUI_RecordsCallsInOrder(t *testing.T) {
	ui := NewNoopUI()
	stop := ui.Spinner("step")
	stop()
	ui.RenderThought("plan")
	ui.RenderResult(ActionResult{Status: "ok", Output: "did it"})

	want := []string{"spin:step", "stop", "thought:plan", "result:ok:did it"}
	if len(ui.Calls) != len(want) {
		t.Fatalf("Calls len = %d, want %d (got %v)", len(ui.Calls), len(want), ui.Calls)
	}
	for i := range want {
		if ui.Calls[i] != want[i] {
			t.Errorf("Calls[%d] = %q, want %q", i, ui.Calls[i], want[i])
		}
	}
}
