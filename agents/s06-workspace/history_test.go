package main

import (
	"strings"
	"testing"
)

// TestHistory_AppendThenCurrent — the two simplest operations have to
// work in concert. Append, then Current returns the last appended
// pointer (identity, not equality). This is what the Loop relies on
// when it calls Current() between propose-and-execute.
func TestHistory_AppendThenCurrent(t *testing.T) {
	var h History
	if got := h.Current(); got != nil {
		t.Errorf("empty Current() = %v, want nil", got)
	}

	ep1 := &Episode{Actions: []ActionProposal{{Command: "echo", Args: map[string]interface{}{"message": "hi"}}}}
	ep2 := &Episode{Actions: []ActionProposal{{Command: "math"}}}

	h.Append(ep1)
	if got := h.Current(); got != ep1 {
		t.Errorf("Current after Append(ep1) = %p, want %p (identity)", got, ep1)
	}

	h.Append(ep2)
	if got := h.Current(); got != ep2 {
		t.Errorf("Current after Append(ep2) = %p, want %p", got, ep2)
	}

	// And the underlying slice grew to length 2.
	if len(h) != 2 {
		t.Errorf("len(history) = %d, want 2", len(h))
	}
}

// TestHistory_RenderMessages_PreservesChronologicalOrder — the contract
// the strategy depends on. RenderMessages walks the history first-to-
// last, emitting [assistant, user, assistant, user, ...] in episode
// order. Reversing the order would break the protocol — the model
// would see "tool_result before tool_use" and refuse to continue.
func TestHistory_RenderMessages_PreservesChronologicalOrder(t *testing.T) {
	var h History
	h.Append(&Episode{
		Actions: []ActionProposal{{Thoughts: "first thought", Command: "echo", Args: map[string]interface{}{"message": "one"}}},
		Results: []ActionResult{{Status: "ok", Output: "one"}},
	})
	h.Append(&Episode{
		Actions: []ActionProposal{{Thoughts: "second thought", Command: "echo", Args: map[string]interface{}{"message": "two"}}},
		Results: []ActionResult{{Status: "ok", Output: "two"}},
	})

	msgs := h.RenderMessages()
	// Two complete episodes → 2*2 = 4 messages.
	if len(msgs) != 4 {
		t.Fatalf("RenderMessages len = %d, want 4 (2 episodes × {assistant, user})", len(msgs))
	}
	wantRoles := []string{"assistant", "user", "assistant", "user"}
	for i, want := range wantRoles {
		if msgs[i].Role != want {
			t.Errorf("msgs[%d].Role = %q, want %q", i, msgs[i].Role, want)
		}
	}

	// First assistant message must reference echo("one"); second must
	// reference echo("two"). Reversing the loop would break this.
	first := msgs[0]
	if first.Content[0].Type != "text" || !strings.Contains(first.Content[0].Text, "first") {
		t.Errorf("msgs[0].Content[0] = %+v, want text 'first thought'", first.Content[0])
	}
	if first.Content[1].Type != "tool_use" || first.Content[1].Name != "echo" {
		t.Errorf("msgs[0].Content[1] = %+v, want tool_use echo", first.Content[1])
	}
	if msg, _ := first.Content[1].Input["message"].(string); msg != "one" {
		t.Errorf("first action arg = %v, want \"one\"", first.Content[1].Input["message"])
	}

	third := msgs[2]
	if msg, _ := third.Content[1].Input["message"].(string); msg != "two" {
		t.Errorf("third action arg = %v, want \"two\"", third.Content[1].Input["message"])
	}
}

// TestHistory_RenderMessages_EmptyReturnsEmptySlice — the explicit
// "return empty slice, not nil" contract. The strategy appends to this
// result, and a nil slice would still work (append handles nil), but
// the JSON-encoded provider request shape would be `messages: null`
// instead of `messages: []` — different on the wire.
func TestHistory_RenderMessages_EmptyReturnsEmptySlice(t *testing.T) {
	var h History
	msgs := h.RenderMessages()
	if msgs == nil {
		t.Fatal("RenderMessages on empty history returned nil; expected []Message{}")
	}
	if len(msgs) != 0 {
		t.Errorf("len(msgs) = %d on empty history, want 0", len(msgs))
	}
}

// TestHistory_RenderMessages_EpisodeWithoutResultRendersOnlyAction —
// the mid-turn state. After the Loop appends a proposal but before
// Execute lands, the Episode has Actions but no Results. RenderMessages
// must emit just the assistant message — emitting a tool_result for a
// not-yet-run tool would lie to the model.
func TestHistory_RenderMessages_EpisodeWithoutResultRendersOnlyAction(t *testing.T) {
	var h History
	h.Append(&Episode{
		Actions: []ActionProposal{{Thoughts: "thinking", Command: "math", Args: map[string]interface{}{"operation": "add", "a": 1.0, "b": 2.0}}},
	})

	msgs := h.RenderMessages()
	if len(msgs) != 1 {
		t.Fatalf("mid-turn render len = %d, want 1 (assistant only)", len(msgs))
	}
	if msgs[0].Role != "assistant" {
		t.Errorf("mid-turn msg.Role = %q, want \"assistant\"", msgs[0].Role)
	}

	// And: appending a result later flips the count to 2 with the same
	// underlying Episode pointer (proves the Loop's "append result to
	// Current() in place" pattern is observable).
	cur := h.Current()
	cur.Results = append(cur.Results, ActionResult{Status: "ok", Output: "3"})
	msgs = h.RenderMessages()
	if len(msgs) != 2 {
		t.Errorf("after-result render len = %d, want 2 (assistant + user tool_result)", len(msgs))
	}
	if msgs[1].Role != "user" || msgs[1].Content[0].Type != "tool_result" {
		t.Errorf("second msg = %+v, want user.tool_result", msgs[1])
	}
}

// TestHistory_TrimToLastN — the pedagogical compression seam in
// action. Build a 5-episode history, trim to last 2, assert we got the
// last 2 in order. Also check the edge cases: n=0 returns empty,
// n>=len returns the full history (a copy of the slice header).
func TestHistory_TrimToLastN(t *testing.T) {
	var h History
	for i := 0; i < 5; i++ {
		h.Append(&Episode{
			Actions: []ActionProposal{{Command: "echo", Args: map[string]interface{}{"message": "ep" + string(rune('0'+i))}}},
		})
	}

	got := h.TrimToLastN(2)
	if len(got) != 2 {
		t.Fatalf("TrimToLastN(2) len = %d, want 2", len(got))
	}
	if got[0] != h[3] || got[1] != h[4] {
		t.Errorf("TrimToLastN(2) returned wrong tail (pointer identity check failed)")
	}

	// n=0 → empty.
	if got := h.TrimToLastN(0); len(got) != 0 {
		t.Errorf("TrimToLastN(0) len = %d, want 0", len(got))
	}

	// n >= len → full copy.
	got = h.TrimToLastN(99)
	if len(got) != 5 {
		t.Errorf("TrimToLastN(99) len = %d, want 5", len(got))
	}

	// Mutating the returned slice header must NOT mutate the original
	// (the slice header is copied; the *Episode pointers are shared).
	got[0] = nil
	if h[0] == nil {
		t.Errorf("TrimToLastN aliased the slice; mutating result affected original")
	}
}
