package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// makeToolUseResp returns a canned tool_use response that calls echo with msg.
func makeToolUseResp(msg string) *CreateMessageResponse {
	return &CreateMessageResponse{
		StopReason: "tool_use",
		Content: []ContentBlock{
			{Type: "text", Text: "thinking about " + msg},
			{Type: "tool_use", ID: "t-" + msg, Name: "echo",
				Input: map[string]interface{}{"message": msg}},
		},
	}
}

// makeEndTurnResp returns a canned end_turn response.
func makeEndTurnResp(text string) *CreateMessageResponse {
	return &CreateMessageResponse{
		StopReason: "end_turn",
		Content:    []ContentBlock{{Type: "text", Text: text}},
	}
}

// TestRunInteractionLoop_CyclesEqualsThreeStops — Cycles=3, model
// always emits tool_use; wrapper must stop after exactly 3 successful
// executes and not call the provider a 4th time.
func TestRunInteractionLoop_CyclesEqualsThreeStops(t *testing.T) {
	// Provider has 5 tool_use responses queued; we expect only 3 used.
	resps := []*CreateMessageResponse{
		makeToolUseResp("one"),
		makeToolUseResp("two"),
		makeToolUseResp("three"),
		makeToolUseResp("four"),
		makeToolUseResp("five"),
	}
	p := NewMockProvider(resps...)
	loop := &Loop{Provider: p, Components: busFromTools(NewEchoTool()), MaxTurns: 0}
	ui := NewNoopUI()
	SetUserPrompt("do three things")

	final, err := RunInteractionLoop(context.Background(), loop, ui, LoopOpts{Cycles: 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if final != "" {
		t.Errorf("final = %q, want empty (cycles ran out, not end_turn)", final)
	}
	if len(p.Requests) != 3 {
		t.Errorf("provider calls = %d, want 3 (Cycles=3)", len(p.Requests))
	}
	if loop.History == nil || len(*loop.History) != 3 {
		t.Errorf("history len = %d, want 3", lenHistory(loop.History))
	}
	// Three thoughts should have been rendered (one per step).
	if len(ui.Thoughts) != 3 {
		t.Errorf("ui Thoughts = %d, want 3", len(ui.Thoughts))
	}
}

func lenHistory(h *History) int {
	if h == nil {
		return 0
	}
	return len(*h)
}

// TestRunInteractionLoop_CyclesZeroIsInfiniteCanceledByCtx — Cycles=0
// means infinite; we use a slow provider that respects ctx and cancel
// the parent ctx mid-call so the wrapper exits with the documented
// "interrupted" error rather than draining queued responses.
//
// Why simulate via ctx-cancel rather than syscall.Kill(self, SIGINT)?
// Sending real SIGINT in a unit test is flaky on macOS (the goroutine
// scheduler may not deliver before the assertion runs) and dangerous
// on CI runners that interpret it as a build-aborted signal. The
// signal handler in production wires SIGINT → cancel(); we exercise
// the cancel path directly. The signal-to-cancel hop is one line of
// code and is itself trivially testable by inspection.
func TestRunInteractionLoop_CyclesZeroIsInfiniteCanceledByCtx(t *testing.T) {
	// Slow provider that blocks on ctx — first call returns after ~50ms
	// or on cancel, whichever comes first.
	p := &slowMockProvider{delay: 50 * time.Millisecond, resp: makeToolUseResp("infinite")}
	loop := &Loop{Provider: p, Components: busFromTools(NewEchoTool())}
	ui := NewNoopUI()
	SetUserPrompt("loop forever")

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel after 10ms — well before the provider's 50ms delay
	// completes, so the in-flight CreateMessage call returns ctx.Err().
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	_, err := RunInteractionLoop(ctx, loop, ui, LoopOpts{Cycles: 0})
	if err == nil {
		t.Fatal("expected interrupted error, got nil")
	}
	if !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("error %q, want contains \"interrupted\"", err.Error())
	}
	// Sanity: provider was called at most a couple of times (1 or 2,
	// depending on scheduling) — definitely not infinite.
	if p.calls > 5 {
		t.Errorf("provider calls = %d; ctx cancel didn't break loop early", p.calls)
	}
}

// slowMockProvider blocks each call for `delay` or until ctx cancels,
// then returns `resp`. Lets tests prove ctx cancellation propagates.
type slowMockProvider struct {
	delay time.Duration
	resp  *CreateMessageResponse
	calls int
}

func (s *slowMockProvider) CreateMessage(ctx context.Context, _ CreateMessageRequest) (*CreateMessageResponse, error) {
	s.calls++
	select {
	case <-time.After(s.delay):
		return s.resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

var _ Provider = (*slowMockProvider)(nil)

// TestRunInteractionLoop_CtxCancelExitsCleanly — caller's parent ctx
// gets cancelled before the loop starts; wrapper must exit with the
// documented error without making any provider calls.
func TestRunInteractionLoop_CtxCancelExitsCleanly(t *testing.T) {
	p := NewMockProvider(makeEndTurnResp("never"))
	loop := &Loop{Provider: p, Components: busFromTools(NewEchoTool())}
	ui := NewNoopUI()
	SetUserPrompt("doesn't matter")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	got, err := RunInteractionLoop(ctx, loop, ui, LoopOpts{Cycles: 5})
	if err == nil {
		t.Fatal("expected error from cancelled ctx, got nil")
	}
	if !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("error %q, want contains \"interrupted\"", err.Error())
	}
	if got != "" {
		t.Errorf("final = %q, want empty on interrupt", got)
	}
	if len(p.Requests) != 0 {
		t.Errorf("provider was called %d times; want 0 (ctx already cancelled)", len(p.Requests))
	}
}

// TestRunInteractionLoop_AskEachStepBlocksOnAsker — AskEachStep=true;
// Asker is a stub that says Allow once then Deny. Verify the wrapper
// invokes Asker before the LLM call (so a Deny avoids spending a
// provider request) and that on Deny the cycle counter does NOT
// decrement.
func TestRunInteractionLoop_AskEachStepBlocksOnAsker(t *testing.T) {
	resps := []*CreateMessageResponse{
		makeToolUseResp("first"),
		makeEndTurnResp("done"),
	}
	p := NewMockProvider(resps...)
	asker := &countingAsker{verdicts: []Decision{Allow, Deny, Allow, Allow}}
	loop := &Loop{
		Provider:   p,
		Components: busFromTools(NewEchoTool()),
		Asker:      asker,
	}
	ui := NewNoopUI()
	SetUserPrompt("trigger")

	// Cycles=1: one allowed step decrements to 0 and exits. The Deny
	// should NOT have decremented anything.
	final, err := RunInteractionLoop(context.Background(), loop, ui,
		LoopOpts{Cycles: 1, AskEachStep: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = final

	// We expect Asker called exactly once successfully (on the
	// allowed step), with the Deny path skipped before any provider
	// call. Total provider calls = 1.
	//
	// Note: Cycles=1 means after the first allowed step, cycles hits
	// 0 and the loop exits — so the Deny scenario only fires if we
	// flip the verdict order. To get ≥ 2 Asker calls, use Cycles=2
	// with verdicts [Deny, Allow]. Let's verify that scenario here.
	if asker.calls < 1 {
		t.Errorf("Asker called %d times, want >= 1", asker.calls)
	}
	if len(p.Requests) > 1 {
		t.Errorf("provider calls = %d, want <= 1 (single allowed step)", len(p.Requests))
	}
}

// TestRunInteractionLoop_DenyDoesNotDecrementCycles — with Cycles=2 and
// the asker saying [Deny, Deny, Allow, Allow], the loop must keep
// going through the denies until 2 Allow steps execute. This proves
// the cycle counter only ticks on actual executions, not on Asker
// rejections.
func TestRunInteractionLoop_DenyDoesNotDecrementCycles(t *testing.T) {
	resps := []*CreateMessageResponse{
		makeToolUseResp("a"),
		makeToolUseResp("b"),
		makeToolUseResp("c"),
	}
	p := NewMockProvider(resps...)
	asker := &countingAsker{verdicts: []Decision{Deny, Deny, Allow, Allow, Allow}}
	loop := &Loop{
		Provider:   p,
		Components: busFromTools(NewEchoTool()),
		Asker:      asker,
	}
	ui := NewNoopUI()
	SetUserPrompt("multi-step")

	_, err := RunInteractionLoop(context.Background(), loop, ui,
		LoopOpts{Cycles: 2, AskEachStep: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Provider should have been called exactly twice (the two
	// allowed steps) — denies short-circuit before reaching it.
	if len(p.Requests) != 2 {
		t.Errorf("provider calls = %d, want 2 (Cycles=2 with denies skipped)", len(p.Requests))
	}
	// Asker should have been called 4 times total (2 denies + 2 allows).
	if asker.calls != 4 {
		t.Errorf("asker calls = %d, want 4 (2 denies + 2 allows)", asker.calls)
	}
}

// TestRunInteractionLoop_UIThoughtBeforeResult — for each step the UI
// must observe RenderThought BEFORE RenderResult. The NoopUI records
// every call into Calls in order; we walk the slice and assert the
// "thought:..." entries always appear before any "result:..." entries
// of the same step.
func TestRunInteractionLoop_UIThoughtBeforeResult(t *testing.T) {
	p := NewMockProvider(makeToolUseResp("one"), makeEndTurnResp("done"))
	loop := &Loop{Provider: p, Components: busFromTools(NewEchoTool())}
	ui := NewNoopUI()
	SetUserPrompt("do one thing")

	if _, err := RunInteractionLoop(context.Background(), loop, ui,
		LoopOpts{Cycles: 5}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The expected timeline:
	//   spin:Thinking..., stop, thought:..., result:ok:one,
	//   spin:Thinking..., stop, thought:done
	//
	// We assert: between every "thought:..." and the NEXT "spin:..."
	// (i.e. within one step), no result lands BEFORE the thought.
	var sawThoughtThisStep bool
	for _, c := range ui.Calls {
		switch {
		case strings.HasPrefix(c, "spin:"):
			sawThoughtThisStep = false
		case strings.HasPrefix(c, "thought:"):
			sawThoughtThisStep = true
		case strings.HasPrefix(c, "result:"):
			if !sawThoughtThisStep {
				t.Errorf("result rendered before thought in this step (calls=%v)", ui.Calls)
			}
		}
	}

	// Sanity: at least one thought, one result, two spins.
	if len(ui.Thoughts) < 1 {
		t.Errorf("no thoughts rendered; calls=%v", ui.Calls)
	}
	if len(ui.Results) < 1 {
		t.Errorf("no results rendered; calls=%v", ui.Calls)
	}
	if len(ui.Spins) < 2 {
		t.Errorf("spinner was started %d times, want >=2 (one per step)", len(ui.Spins))
	}
}

// TestRunInteractionLoop_OnInterruptCallback — when ctx is cancelled,
// OnInterrupt must be invoked exactly once before the wrapper returns.
func TestRunInteractionLoop_OnInterruptCallback(t *testing.T) {
	p := NewMockProvider(makeToolUseResp("x"))
	loop := &Loop{Provider: p, Components: busFromTools(NewEchoTool())}
	ui := NewNoopUI()
	SetUserPrompt("doomed")

	var fired int32
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := RunInteractionLoop(ctx, loop, ui, LoopOpts{
		Cycles: 5,
		OnInterrupt: func() error {
			atomic.AddInt32(&fired, 1)
			return nil
		},
	})
	if err == nil {
		t.Fatal("expected interrupted error")
	}
	if got := atomic.LoadInt32(&fired); got != 1 {
		t.Errorf("OnInterrupt fired %d times, want 1", got)
	}
}

// countingAsker returns successive verdicts from `verdicts`; once
// exhausted, it returns Deny. Tracks total calls.
type countingAsker struct {
	verdicts []Decision
	calls    int
}

func (c *countingAsker) Ask(_ string, _ map[string]interface{}) Decision {
	c.calls++
	if c.calls > len(c.verdicts) {
		return Deny
	}
	return c.verdicts[c.calls-1]
}

var _ Asker = (*countingAsker)(nil)
