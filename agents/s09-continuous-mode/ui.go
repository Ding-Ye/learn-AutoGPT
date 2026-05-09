// ui.go — the UIProvider seam for continuous-mode feedback.
//
// AutoGPT classic uses Rich (a Python TUI library) to draw spinners and
// color-coded panels around each `propose_action → execute_command`
// step (`app/main.py:run_interaction_loop`). Rich's "Spinner with
// transient=True" pattern shows a "Thinking..." line while the LLM
// thinks, then erases it once the response arrives.
//
// Go has no Rich. Rather than pull in a TUI library (charm/log,
// charmbracelet/lipgloss, etc.) and lose pedagogy in styling code, we
// ship a tiny `ConsoleUI` that prints two-line summaries and a
// minimal-ANSI spinner — just enough to teach the seam, without making
// the file half-way to a TUI implementation.
//
// The interface lives here, the production console impl, and a test
// `NoopUI` that records calls for assertions. The interaction loop in
// interaction_loop.go consumes UIProvider; main.go constructs
// `ConsoleUI(os.Stderr)` so the agent's final stdout answer stays
// clean for piping.
package main

import (
	"fmt"
	"io"
	"strings"
	"sync"
)

// UIProvider is the seam between RunInteractionLoop and the user-facing
// terminal. RunInteractionLoop calls these in order around each step:
//
//  1. stop := ui.Spinner("Thinking...")
//  2. ... runStep ...
//  3. stop()
//  4. ui.RenderThought(proposal.Thoughts)
//  5. ui.RenderResult(result)  (for each result in the step)
//
// All three methods MUST be safe to call from a single goroutine (no
// internal locking required); the production ConsoleUI uses a mutex
// only because the spinner write and the stop write race in pathological
// timings, not because UIProvider is a concurrent contract.
type UIProvider interface {
	// RenderThought is called once per step with the model's free-text
	// reasoning (may be empty, in which case the impl should no-op).
	RenderThought(text string)
	// RenderResult is called for every ActionResult of a step.
	RenderResult(r ActionResult)
	// Spinner starts a "in-progress" indicator labeled `label` and
	// returns a stop function. Calling the stop function MUST be
	// idempotent (callers may call it twice on error paths).
	Spinner(label string) func()
}

// ConsoleUI writes terminal-friendly lines to `out`. It's not a TUI —
// no cursor movement beyond a single carriage-return for the spinner,
// no color codes (so output stays grep-friendly when piped), no
// progress bars. Pedagogy over polish.
//
// Per the s09 spec: the spinner draws ONE line "[busy] <label>..."
// and the stop fn writes "\r" + spaces to clear it. We don't animate
// frames — a single static "busy" line is enough to demonstrate the
// "stop fn after step" seam without a goroutine + ticker dance.
type ConsoleUI struct {
	out io.Writer
	mu  sync.Mutex
}

// NewConsoleUI binds a ConsoleUI to an io.Writer. Production calls
// pass `os.Stderr` so the spinner doesn't pollute the agent's
// `os.Stdout` final-answer line. Tests pass a `bytes.Buffer` so they
// can assert on rendered text.
func NewConsoleUI(out io.Writer) *ConsoleUI {
	return &ConsoleUI{out: out}
}

// RenderThought prints "💭 <text>" + newline. Empty input no-ops so
// the impl matches the s10 hook surface (RenderThought may be invoked
// even when proposal.Thoughts is empty, which AutoGPT classic also
// silently elides).
//
// Why "💭"? AutoGPT classic uses a Rich-emoji prefix for thought
// panels. We pick the same glyph for visual familiarity. If your
// terminal can't render multibyte chars, redirect stderr to /dev/null
// — the final stdout answer is still ASCII.
func (c *ConsoleUI) RenderThought(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.out, "💭 %s\n", text)
}

// RenderResult prints "✓ <output>" on success or "✗ <error>" on
// failure. Other statuses (e.g. "denied") render with "✗" and the
// status appears in the message. Per s05, ActionResult.Output is
// already a stringified result; we don't re-encode.
func (c *ConsoleUI) RenderResult(r ActionResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch r.Status {
	case "ok":
		fmt.Fprintf(c.out, "✓ %s\n", r.Output)
	default:
		// "error", "denied", or anything else — fail-loud.
		fmt.Fprintf(c.out, "✗ [%s] %s\n", r.Status, r.Output)
	}
}

// Spinner writes a static "[busy] <label>..." line and returns a
// stop function that emits a CR + blanks to clear it before the next
// log line lands. The stop fn is idempotent (a sync.Once guards the
// write) so error paths in the wrapper can defer it without worrying
// about double-call.
//
// Pedagogically minimal: no ticker, no frame animation. AutoGPT
// classic's Rich spinner cycles braille glyphs every 100ms; we omit
// that because the value here is "show the seam," and a real-frame
// animation needs a goroutine + cancel that obscures the lesson.
func (c *ConsoleUI) Spinner(label string) func() {
	c.mu.Lock()
	fmt.Fprintf(c.out, "[busy] %s...", label)
	c.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			// Clear the line: CR, then enough spaces to overwrite
			// "[busy] <label>..." (length ~= 9 + len(label)), then CR
			// again so the next print starts at column 0.
			pad := strings.Repeat(" ", 9+len(label))
			fmt.Fprintf(c.out, "\r%s\r", pad)
		})
	}
}

// NoopUI is the test-only impl that records every call for
// assertions. Tests that want to verify "RenderThought ran before
// RenderResult" use NoopUI.Calls for the ordering check.
type NoopUI struct {
	mu       sync.Mutex
	Calls    []string         // ordered list: "spin:<label>", "stop", "thought:<text>", "result:<status>:<output>"
	Thoughts []string         // every text passed to RenderThought
	Results  []ActionResult   // every ActionResult passed to RenderResult
	Spins    []string         // every label passed to Spinner
}

// NewNoopUI constructs an empty NoopUI.
func NewNoopUI() *NoopUI { return &NoopUI{} }

// RenderThought records the text and appends to Calls.
func (n *NoopUI) RenderThought(text string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Thoughts = append(n.Thoughts, text)
	n.Calls = append(n.Calls, "thought:"+text)
}

// RenderResult records the result and appends to Calls.
func (n *NoopUI) RenderResult(r ActionResult) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Results = append(n.Results, r)
	n.Calls = append(n.Calls, fmt.Sprintf("result:%s:%s", r.Status, r.Output))
}

// Spinner records the label and returns a stop fn that records "stop".
func (n *NoopUI) Spinner(label string) func() {
	n.mu.Lock()
	n.Spins = append(n.Spins, label)
	n.Calls = append(n.Calls, "spin:"+label)
	n.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			n.mu.Lock()
			defer n.mu.Unlock()
			n.Calls = append(n.Calls, "stop")
		})
	}
}

// Compile-time checks: both impls satisfy the interface.
var (
	_ UIProvider = (*ConsoleUI)(nil)
	_ UIProvider = (*NoopUI)(nil)
)
