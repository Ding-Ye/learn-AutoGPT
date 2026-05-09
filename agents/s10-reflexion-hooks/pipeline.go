// pipeline.go — the AfterParse / AfterExecute hook registry.
//
// AutoGPT classic exposes pipeline hooks via abstract base classes in
// `forge/agent/protocols.py`:
//
//	class AfterParse(AgentComponent, ABC):
//	    @abstractmethod
//	    def after_parse(self, result: T) -> None: ...
//
//	class AfterExecute(AgentComponent, ABC):
//	    @abstractmethod
//	    def after_execute(self, result: ActionResult) -> None: ...
//
// `agent.py:282` then runs them via `await self.run_pipeline(AfterParse.after_parse, result)`,
// using attribute discovery on each registered component to find an
// `after_parse` method and invoke it. We translate this to a tiny Go
// type — `Pipeline` — that holds two slices of function-typed hooks and
// runs them in registration order.
//
// Why hooks deserve their own file (and not a method on Loop):
//
//  1. **Decoupling cross-cutting concerns.** Reflexion (the second-pass
//     evaluator) is one example, but logging, metrics, validation,
//     governance/audit, and rate limiting all want to observe the same
//     two seam points (post-parse and post-execute) without bloating
//     `runStep`. Pipeline gives each concern an injection point.
//
//  2. **Composition over inheritance.** The classic OO answer to
//     "different agents need different post-processing" is subclassing
//     `Agent` and overriding methods. AutoGPT's pipeline is the
//     non-OO answer: register a callback. Strategies stay focused on
//     prompt construction; hooks handle everything else. This is the
//     architectural punchline of the curriculum — see s_full for how
//     this generalizes.
//
//  3. **First-class mutation contract.** AfterParseHook receives a
//     *pointer* to the proposal so a hook can revise the action before
//     execution (Reflexion's reason for existing). AfterExecuteHook
//     similarly receives a *pointer* to the result so a hook can
//     post-process the output (e.g. truncate, redact, summarize) before
//     it lands in history.
package main

import (
	"context"
	"fmt"
)

// AfterParseHook fires AFTER strategy.ParseResponse but BEFORE the
// permission gate / tool dispatch. The hook receives a pointer to the
// just-parsed proposal so it can revise Command/Args/Thoughts in place
// before the Loop continues. Returning a non-nil error halts the
// pipeline and propagates the error to the Loop, which surfaces it as
// a step error.
//
// Reflexion uses this seam to ask the LLM "is this action sound?" and
// rewrite the proposal when the answer is no. Other natural callers:
//
//   - validation: reject malformed Args before they hit a tool
//   - audit: log every proposed action to a structured sink
//   - governance: short-circuit certain commands (kill switch)
type AfterParseHook func(ctx context.Context, proposal *ActionProposal) error

// AfterExecuteHook fires AFTER tool dispatch (whether the tool succeeded
// or returned an error result). The hook receives a pointer to the
// freshly-produced result so it can mutate Status/Output before history
// records it. Returning a non-nil error halts the pipeline and
// propagates to the Loop.
//
// Natural callers:
//
//   - truncation: cap large web_fetch outputs to N kilobytes
//   - redaction: scrub PII / API keys from tool stdout
//   - summarization: replace verbose JSON with a one-line summary
//   - metrics: increment a counter, record latency
type AfterExecuteHook func(ctx context.Context, result *ActionResult) error

// Pipeline holds the two ordered hook lists. It's small by design — no
// configuration, no priorities, no conditional dispatch. Order is
// strictly registration order.
//
// Concurrency: Pipeline is not thread-safe. Tests and main register
// hooks at startup before the Loop runs; the Loop reads but does not
// register during a step. If a future session needs concurrent
// registration (e.g. dynamic plugins), wrap the slices in a sync.Mutex.
type Pipeline struct {
	afterParse   []AfterParseHook
	afterExecute []AfterExecuteHook
}

// NewPipeline returns an empty pipeline. Register hooks via
// RegisterAfterParse / RegisterAfterExecute before passing the pipeline
// to the Loop.
func NewPipeline() *Pipeline {
	return &Pipeline{}
}

// RegisterAfterParse appends a hook to the AfterParse chain. Hooks run
// in the order they were registered.
func (p *Pipeline) RegisterAfterParse(h AfterParseHook) {
	p.afterParse = append(p.afterParse, h)
}

// RegisterAfterExecute appends a hook to the AfterExecute chain. Hooks
// run in the order they were registered.
func (p *Pipeline) RegisterAfterExecute(h AfterExecuteHook) {
	p.afterExecute = append(p.afterExecute, h)
}

// RunAfterParse fires every AfterParseHook in registration order. The
// first hook to return a non-nil error halts the chain and that error
// is returned wrapped with the hook index for debugability. A nil
// pipeline is a no-op (callers can pass *Pipeline=nil safely).
func (p *Pipeline) RunAfterParse(ctx context.Context, prop *ActionProposal) error {
	if p == nil {
		return nil
	}
	for i, h := range p.afterParse {
		if err := h(ctx, prop); err != nil {
			return fmt.Errorf("AfterParse hook %d: %w", i, err)
		}
	}
	return nil
}

// RunAfterExecute fires every AfterExecuteHook in registration order.
// Same halt-on-first-error contract as RunAfterParse. A nil pipeline
// is a no-op.
func (p *Pipeline) RunAfterExecute(ctx context.Context, res *ActionResult) error {
	if p == nil {
		return nil
	}
	for i, h := range p.afterExecute {
		if err := h(ctx, res); err != nil {
			return fmt.Errorf("AfterExecute hook %d: %w", i, err)
		}
	}
	return nil
}
