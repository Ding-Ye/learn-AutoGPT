package main

import (
	"context"
	"reflect"
	"testing"
)

// commandsOnlyComponent implements ONLY CommandProvider — used to prove
// the bus's type-switch detects each protocol independently.
type commandsOnlyComponent struct {
	tools []Tool
}

func (c *commandsOnlyComponent) Commands() []Tool { return c.tools }

// directivesOnlyComponent implements ONLY DirectiveProvider.
type directivesOnlyComponent struct {
	lines []string
}

func (d *directivesOnlyComponent) Directives() []string { return d.lines }

// messagesOnlyComponent implements ONLY MessageProvider.
type messagesOnlyComponent struct {
	msgs []Message
}

func (m *messagesOnlyComponent) Messages() []Message { return m.msgs }

// fullCapabilityComponent implements all three protocols.
type fullCapabilityComponent struct {
	tools []Tool
	lines []string
	msgs  []Message
}

func (f *fullCapabilityComponent) Commands() []Tool       { return f.tools }
func (f *fullCapabilityComponent) Directives() []string   { return f.lines }
func (f *fullCapabilityComponent) Messages() []Message    { return f.msgs }

// markerOnlyComponent implements NO sub-protocol — the bus must skip it
// for everything (no tools, no directives, no messages). This proves a
// component that's literally `struct{}` isn't a bug — it just contributes
// nothing.
type markerOnlyComponent struct{}

// dummyToolForBus is a no-op Tool used so component-level tests don't
// depend on EchoTool/MathTool or any specific tool family.
type dummyToolForBus struct {
	name string
}

func (d *dummyToolForBus) Schema() ToolSchema {
	return ToolSchema{
		Name:        d.name,
		Description: "test tool for ComponentBus",
		InputSchema: map[string]interface{}{"type": "object"},
	}
}

func (d *dummyToolForBus) Execute(ctx context.Context, input map[string]interface{}) (string, error) {
	return d.name + ":executed", nil
}

// TestComponentBus_TypeSwitchDetectsProtocolsIndependently — the marker
// interface lets a value implement any subset of the three protocols.
// The bus's type assertions must detect each independently: a
// CommandProvider that doesn't also implement DirectiveProvider must
// not be asked for directives.
func TestComponentBus_TypeSwitchDetectsProtocolsIndependently(t *testing.T) {
	bus := NewComponentBus(
		&commandsOnlyComponent{tools: []Tool{&dummyToolForBus{name: "alpha"}}},
		&directivesOnlyComponent{lines: []string{"only directive"}},
		&messagesOnlyComponent{msgs: []Message{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: "preamble"}}},
		}},
	)

	// Each component contributes ONLY to its own stream.
	reg := bus.Registry()
	if got := len(reg.All()); got != 1 {
		t.Errorf("Registry size = %d, want 1 (only commandsOnlyComponent contributes)", got)
	}
	if reg.All()[0].Name != "alpha" {
		t.Errorf("Registry[0].Name = %q, want \"alpha\"", reg.All()[0].Name)
	}

	directives := bus.Directives()
	if got := len(directives); got != 1 {
		t.Errorf("Directives len = %d, want 1", got)
	}
	if got := directives[0]; got != "only directive" {
		t.Errorf("Directives[0] = %q, want \"only directive\"", got)
	}

	msgs := bus.Messages()
	if got := len(msgs); got != 1 {
		t.Errorf("Messages len = %d, want 1", got)
	}
	if got := msgs[0].Content[0].Text; got != "preamble" {
		t.Errorf("Messages[0] text = %q, want \"preamble\"", got)
	}
}

// TestComponentBus_RegistryAggregatesMultipleProviders — two components
// each emit one tool. The Registry returned by the bus contains both,
// in component order.
func TestComponentBus_RegistryAggregatesMultipleProviders(t *testing.T) {
	bus := NewComponentBus(
		&commandsOnlyComponent{tools: []Tool{&dummyToolForBus{name: "alpha"}}},
		&commandsOnlyComponent{tools: []Tool{&dummyToolForBus{name: "beta"}}},
	)

	reg := bus.Registry()
	all := reg.All()
	if len(all) != 2 {
		t.Fatalf("Registry size = %d, want 2", len(all))
	}
	if all[0].Name != "alpha" || all[1].Name != "beta" {
		t.Errorf("Registry order = [%s, %s], want [alpha, beta]", all[0].Name, all[1].Name)
	}

	// And lookup of each must work.
	if _, ok := reg.Lookup("alpha"); !ok {
		t.Errorf("Lookup(alpha) failed after bus aggregation")
	}
	if _, ok := reg.Lookup("beta"); !ok {
		t.Errorf("Lookup(beta) failed after bus aggregation")
	}
}

// TestComponentBus_DirectivesPreserveComponentOrder — directives are
// concatenated in the order components were passed to NewComponentBus.
// This is the contract the strategy depends on for stable system prompts.
func TestComponentBus_DirectivesPreserveComponentOrder(t *testing.T) {
	bus := NewComponentBus(
		&directivesOnlyComponent{lines: []string{"alpha-1", "alpha-2"}},
		&directivesOnlyComponent{lines: []string{"beta-1"}},
		&directivesOnlyComponent{lines: []string{"gamma-1", "gamma-2", "gamma-3"}},
	)

	got := bus.Directives()
	want := []string{"alpha-1", "alpha-2", "beta-1", "gamma-1", "gamma-2", "gamma-3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Directives order = %v, want %v", got, want)
	}
}

// TestComponentBus_MarkerOnlyComponentContributesNothing — a value
// satisfying only Component (the empty interface) must produce zero
// tools, zero directives, zero messages. No panic, no error.
func TestComponentBus_MarkerOnlyComponentContributesNothing(t *testing.T) {
	bus := NewComponentBus(&markerOnlyComponent{})

	if got := len(bus.Registry().All()); got != 0 {
		t.Errorf("Registry size = %d, want 0 for marker-only component", got)
	}
	if got := len(bus.Directives()); got != 0 {
		t.Errorf("Directives len = %d, want 0", got)
	}
	if got := len(bus.Messages()); got != 0 {
		t.Errorf("Messages len = %d, want 0", got)
	}

	// Empty bus also returns empty (not nil) slices — pin the contract.
	empty := NewComponentBus()
	if d := empty.Directives(); d == nil {
		t.Error("Directives() on empty bus returned nil; want []string{}")
	}
	if m := empty.Messages(); m == nil {
		t.Error("Messages() on empty bus returned nil; want []Message{}")
	}
}

// TestComponentBus_MultipleComponentsAddToSameRegistry — fullCapability
// component plus commandsOnly component both contribute tools; the
// resulting Registry has both, plus directives only from the
// fullCapability provider.
func TestComponentBus_MultipleComponentsAddToSameRegistry(t *testing.T) {
	full := &fullCapabilityComponent{
		tools: []Tool{&dummyToolForBus{name: "f1"}, &dummyToolForBus{name: "f2"}},
		lines: []string{"full directive"},
		msgs:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "fmsg"}}}},
	}
	cmd := &commandsOnlyComponent{tools: []Tool{&dummyToolForBus{name: "c1"}}}

	bus := NewComponentBus(full, cmd)

	reg := bus.Registry()
	all := reg.All()
	if len(all) != 3 {
		t.Fatalf("Registry size = %d, want 3 (f1, f2, c1)", len(all))
	}
	wantNames := []string{"f1", "f2", "c1"}
	for i, n := range wantNames {
		if all[i].Name != n {
			t.Errorf("all[%d].Name = %q, want %q", i, all[i].Name, n)
		}
	}

	// Only `full` contributes a directive.
	if got := bus.Directives(); len(got) != 1 || got[0] != "full directive" {
		t.Errorf("Directives = %v, want [\"full directive\"]", got)
	}

	// Only `full` contributes a message.
	if got := bus.Messages(); len(got) != 1 || got[0].Content[0].Text != "fmsg" {
		t.Errorf("Messages = %+v, want [{user fmsg}]", got)
	}
}
