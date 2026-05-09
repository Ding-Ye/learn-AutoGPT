package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestPipeline_RunsHooksInRegistrationOrder — the core invariant: hooks
// fire in the order RegisterAfterParse / RegisterAfterExecute received
// them, and every hook sees the same proposal/result pointer.
func TestPipeline_RunsHooksInRegistrationOrder(t *testing.T) {
	pipe := NewPipeline()
	var order []string

	pipe.RegisterAfterParse(func(_ context.Context, p *ActionProposal) error {
		order = append(order, "parse-A")
		return nil
	})
	pipe.RegisterAfterParse(func(_ context.Context, p *ActionProposal) error {
		order = append(order, "parse-B")
		return nil
	})
	pipe.RegisterAfterParse(func(_ context.Context, p *ActionProposal) error {
		order = append(order, "parse-C")
		return nil
	})

	pipe.RegisterAfterExecute(func(_ context.Context, r *ActionResult) error {
		order = append(order, "exec-A")
		return nil
	})
	pipe.RegisterAfterExecute(func(_ context.Context, r *ActionResult) error {
		order = append(order, "exec-B")
		return nil
	})

	prop := &ActionProposal{Command: "echo"}
	if err := pipe.RunAfterParse(context.Background(), prop); err != nil {
		t.Fatalf("RunAfterParse: %v", err)
	}
	res := &ActionResult{Status: "ok"}
	if err := pipe.RunAfterExecute(context.Background(), res); err != nil {
		t.Fatalf("RunAfterExecute: %v", err)
	}

	want := []string{"parse-A", "parse-B", "parse-C", "exec-A", "exec-B"}
	if len(order) != len(want) {
		t.Fatalf("order len = %d, want %d (got %v)", len(order), len(want), order)
	}
	for i, s := range want {
		if order[i] != s {
			t.Errorf("order[%d] = %q, want %q (full: %v)", i, order[i], s, order)
		}
	}
}

// TestPipeline_AfterParseHookCanMutateProposal — the load-bearing
// reflexion contract: a hook's *ActionProposal pointer means edits
// propagate back to the caller. Reflexion relies on this to rewrite
// the action before the Loop hits the permission gate.
func TestPipeline_AfterParseHookCanMutateProposal(t *testing.T) {
	pipe := NewPipeline()
	pipe.RegisterAfterParse(func(_ context.Context, p *ActionProposal) error {
		p.Command = "rewritten"
		p.Args = map[string]interface{}{"safe": true}
		p.Thoughts = "(revised by hook)"
		return nil
	})

	prop := &ActionProposal{
		Command: "original",
		Args:    map[string]interface{}{"danger": true},
	}
	if err := pipe.RunAfterParse(context.Background(), prop); err != nil {
		t.Fatalf("RunAfterParse: %v", err)
	}
	if prop.Command != "rewritten" {
		t.Errorf("Command = %q, want %q (mutation did not propagate)", prop.Command, "rewritten")
	}
	if prop.Args["safe"] != true {
		t.Errorf("Args = %v, want safe=true", prop.Args)
	}
	if _, leftover := prop.Args["danger"]; leftover {
		t.Errorf("danger key still present in Args = %v", prop.Args)
	}
	if prop.Thoughts != "(revised by hook)" {
		t.Errorf("Thoughts = %q, want revision", prop.Thoughts)
	}
}

// TestPipeline_HaltsAndSurfacesErrorOnHookError — the first hook to
// return a non-nil error stops subsequent hooks and surfaces the error
// (wrapped with the hook index) to the caller. This is what lets a
// "kill switch" hook abort the rest of the pipeline cleanly.
func TestPipeline_HaltsAndSurfacesErrorOnHookError(t *testing.T) {
	pipe := NewPipeline()
	var ran []string
	sentinel := errors.New("kill-switch-engaged")

	pipe.RegisterAfterParse(func(_ context.Context, p *ActionProposal) error {
		ran = append(ran, "first")
		return nil
	})
	pipe.RegisterAfterParse(func(_ context.Context, p *ActionProposal) error {
		ran = append(ran, "second")
		return sentinel
	})
	pipe.RegisterAfterParse(func(_ context.Context, p *ActionProposal) error {
		ran = append(ran, "third (should not run)")
		return nil
	})

	err := pipe.RunAfterParse(context.Background(), &ActionProposal{Command: "x"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error %v should wrap sentinel %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "AfterParse hook 1") {
		t.Errorf("error %q should mention which hook (index 1)", err.Error())
	}
	if len(ran) != 2 {
		t.Errorf("ran = %v, want 2 hooks (first + second; third must not run)", ran)
	}
	if ran[0] != "first" || ran[1] != "second" {
		t.Errorf("ran = %v, want [first, second]", ran)
	}

	// Same contract on the AfterExecute side.
	pipe2 := NewPipeline()
	pipe2.RegisterAfterExecute(func(_ context.Context, r *ActionResult) error {
		return sentinel
	})
	pipe2.RegisterAfterExecute(func(_ context.Context, r *ActionResult) error {
		t.Error("second AfterExecute hook ran despite first returning error")
		return nil
	})
	if err := pipe2.RunAfterExecute(context.Background(), &ActionResult{}); err == nil {
		t.Fatal("expected error from RunAfterExecute, got nil")
	}
}

// TestPipeline_NilPipelineIsNoOp — passing *Pipeline=nil is a documented
// safe default so the Loop's runStep doesn't need a "if l.Pipeline !=
// nil" guard around every Run* call. Both Run methods on a nil
// receiver must return nil error and do nothing.
func TestPipeline_NilPipelineIsNoOp(t *testing.T) {
	var pipe *Pipeline // nil

	prop := &ActionProposal{Command: "x"}
	if err := pipe.RunAfterParse(context.Background(), prop); err != nil {
		t.Errorf("nil pipeline RunAfterParse should be no-op, got %v", err)
	}
	if prop.Command != "x" {
		t.Errorf("nil pipeline mutated proposal: Command = %q", prop.Command)
	}

	res := &ActionResult{Status: "ok"}
	if err := pipe.RunAfterExecute(context.Background(), res); err != nil {
		t.Errorf("nil pipeline RunAfterExecute should be no-op, got %v", err)
	}
	if res.Status != "ok" {
		t.Errorf("nil pipeline mutated result: Status = %q", res.Status)
	}
}
