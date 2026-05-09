// component.go — the pluggable Component system.
//
// By s07, the Loop has 7 fields: Provider, Tools, Strategy, History,
// Workspace (via tools), Permissions, Asker. Each is a separate
// "capability" the agent has. AutoGPT classic faces the same accretion
// in `forge/agent/` and answers it with **components**: each Component
// is a capability bundle that *opts into* one or more protocols
// (`CommandProvider`, `MessageProvider`, `DirectiveProvider`). The
// agent loop collects all components and asks each via type-assertion
// "give me your commands / messages / directives." The Loop's field
// list collapses back: instead of N capability fields, one
// `*ComponentBus`.
//
// AutoGPT upstream protocols (Python ABC) → Go translation:
//
//	class CommandProvider(AgentComponent, ABC):       → type CommandProvider interface
//	    @abstractmethod
//	    def get_commands(self) -> Iterator[Command]: ...    Commands() []Tool
//
//	class MessageProvider(AgentComponent, ABC):       → type MessageProvider interface
//	    @abstractmethod                                       Messages() []Message
//	    def get_messages(self) -> Iterator[ChatMessage]: ...
//
//	class DirectiveProvider(AgentComponent, ABC):     → type DirectiveProvider interface
//	    @abstractmethod                                       Directives() []string
//	    def get_constraints(self) -> Iterator[str]: ...
//
// Python uses ABCs + `isinstance` checks; Go uses structural typing and
// type assertions. Same outcome, less ceremony.
//
// The `Component` marker interface is empty (anything can be a
// component). The bus iterates the `[]Component` slice and uses type
// switches to discover which optional protocols each one implements.
// A component implementing zero protocols is legal but pointless — the
// bus will skip it for everything and the user has no leverage.
package main

// Component is the marker interface every capability bundle must
// satisfy. It's deliberately empty: any Go value can be a component.
// Capability is opted into via the three sub-interfaces below.
//
// Why an empty marker? Three reasons:
//
//  1. The bus stores `[]Component`, not `[]interface{}` — readers
//     immediately see "this slice is a list of components" without
//     guessing.
//  2. Future extensions can add base methods to Component (e.g. a
//     Name() string) without breaking existing callers — they just
//     wouldn't satisfy the new method until updated. We don't add such
//     methods in s08 to keep the seam minimal.
//  3. Authors can pass in any value — even a plain struct with no
//     methods — for documentation purposes (a "tag" component) without
//     having to satisfy a specific interface.
type Component interface{}

// CommandProvider is the optional protocol for components that emit
// tools. The bus aggregates Commands() across all components into a
// single *Registry. A component implementing CommandProvider behaves
// the same as a manual `registry.Register(tool)` in earlier sessions —
// the difference is who owns the registration.
//
// Returning a `[]Tool` (not `[]ToolSchema`) means the bus can also
// build a runtime registry where Lookup → Execute works, not just a
// schema-only listing.
type CommandProvider interface {
	Commands() []Tool
}

// DirectiveProvider is the optional protocol for components that
// contribute system-prompt policy lines. Each string returned by
// Directives() is a single instruction the agent should follow ("Always
// read a file before editing it.", "Use list_files to discover before
// reading."). The bus concatenates Directives() across components and
// the Loop passes the result into Strategy.BuildPrompt where the
// strategy renders it into the system message.
//
// Order matters: directives are rendered in component order, which is
// the order ComponentBus was constructed with. Ordering is
// deterministic so golden tests pin behavior.
type DirectiveProvider interface {
	Directives() []string
}

// MessageProvider is the optional protocol for components that
// contribute pre-injected messages. AutoGPT upstream uses this for
// things like "current task description", "pinned reminders", and
// per-step context that should appear at the top of every prompt.
// In s08 we expose the protocol but the Loop only reads it for the
// FIRST turn (so the messages don't get duplicated each round). The
// minimal-loop component test verifies the bus aggregates Messages()
// across providers; what the Loop does with them is a Loop-level
// integration concern.
//
// Anthropic only allows ONE system message per request, so MessageProvider
// is NOT a way to inject system content (that's DirectiveProvider's job).
// MessageProvider returns user/assistant messages — for example a
// pinned "you previously stored this state" reminder.
type MessageProvider interface {
	Messages() []Message
}

// ComponentBus aggregates a list of components into the three
// capability streams (commands, directives, messages). The bus is
// constructed once, in component order, and the order is preserved
// through every aggregation.
//
// Storing components by reference (interface, not value) means
// component state — like FileManagerComponent's wrapped Workspace —
// outlives the bus's construction call and is shared across calls to
// Registry()/Directives()/Messages().
type ComponentBus struct {
	components []Component
}

// NewComponentBus accepts components in the order the user wants them
// processed. A registry built from the bus will list tools in this
// order, and a directive list will be rendered in this order. Tests
// that pin "echo before math" rely on this contract.
func NewComponentBus(components ...Component) *ComponentBus {
	return &ComponentBus{components: components}
}

// Registry synthesizes a fresh *Registry from every component that
// implements CommandProvider. Tools are registered in component order.
// If two components emit a tool with the same name, the second
// Register call returns an error and the bus PANICS — duplicate names
// are a configuration bug, not a runtime condition the loop can
// gracefully handle.
//
// Why panic, not return error? Because Registry() is called once at
// Loop startup; a duplicate-name component pair is something the
// developer wired wrong, not something the model can recover from. A
// panic here surfaces the bug at startup with a stack trace pointing
// directly at the registration call.
func (b *ComponentBus) Registry() *Registry {
	reg := NewRegistry()
	for _, c := range b.components {
		cp, ok := c.(CommandProvider)
		if !ok {
			continue
		}
		for _, tool := range cp.Commands() {
			if err := reg.Register(tool); err != nil {
				panic("ComponentBus.Registry: " + err.Error())
			}
		}
	}
	return reg
}

// Directives concatenates Directives() from every component that
// implements DirectiveProvider, preserving component order.
//
// Returns an empty slice (not nil) when no component contributes —
// callers may `range` the result without nil checks, and a JSON
// encoding of the result stays stable.
func (b *ComponentBus) Directives() []string {
	out := make([]string, 0)
	for _, c := range b.components {
		dp, ok := c.(DirectiveProvider)
		if !ok {
			continue
		}
		out = append(out, dp.Directives()...)
	}
	return out
}

// Messages concatenates Messages() from every component that
// implements MessageProvider, preserving component order.
//
// Returns an empty slice (not nil) when no component contributes.
func (b *ComponentBus) Messages() []Message {
	out := make([]Message, 0)
	for _, c := range b.components {
		mp, ok := c.(MessageProvider)
		if !ok {
			continue
		}
		out = append(out, mp.Messages()...)
	}
	return out
}

// Components returns the underlying slice. Used by tests and verbose
// banners; not part of the Loop's hot path.
func (b *ComponentBus) Components() []Component {
	return b.components
}
