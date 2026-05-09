// registry.go — the explicit command registry.
//
// AutoGPT classic uses a Python `@command` decorator (see
// `classic/forge/forge/command/decorator.py`) so any module can drop a
// function in and have it auto-register. Go has no decorator, and even
// if it did, decorator-based registration is implicit: dependencies are
// invisible until runtime, ordering is whatever import-time happens to
// produce. We pick the explicit alternative — every package that wants
// to expose a tool calls `Register(tool)` itself, and the dependency is
// visible in source.
package main

import "fmt"

// Registry is a name → Tool index. The `names` slice carries insertion
// order so `All()` can return schemas deterministically — a map alone
// would yield an arbitrary order each iteration, and downstream
// consumers (the prompt that lists tools to the model, golden tests,
// the doc viewer) all benefit from a stable order.
type Registry struct {
	commands map[string]Tool
	names    []string
}

func NewRegistry() *Registry {
	return &Registry{commands: map[string]Tool{}}
}

// Register adds a tool under its schema name. Duplicate names are an
// error rather than a silent overwrite — silent overwrites cause the
// "I added foo, why is bar still running?" debugging mystery.
func (r *Registry) Register(t Tool) error {
	name := t.Schema().Name
	if name == "" {
		return fmt.Errorf("registry: tool has empty name (Schema().Name must be set)")
	}
	if _, exists := r.commands[name]; exists {
		return fmt.Errorf("registry: tool %q already registered", name)
	}
	r.commands[name] = t
	r.names = append(r.names, name)
	return nil
}

// Lookup returns (tool, true) for a known name and (nil, false) for an
// unknown name. The boolean exists flag — not an error — is the right
// shape because "not found" is not a failure of Lookup itself; the
// caller decides what missing means (the Loop turns it into a friendly
// tool_result so the model can recover).
func (r *Registry) Lookup(name string) (Tool, bool) {
	t, ok := r.commands[name]
	return t, ok
}

// All returns every registered tool's schema in the order it was
// registered. The fixed order is what makes prompt rendering and tests
// reproducible.
func (r *Registry) All() []ToolSchema {
	out := make([]ToolSchema, 0, len(r.names))
	for _, name := range r.names {
		out = append(out, r.commands[name].Schema())
	}
	return out
}
