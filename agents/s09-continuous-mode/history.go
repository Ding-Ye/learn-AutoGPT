// history.go — episodic action history.
//
// (advanced) when context overflows, summarize old episodes here.
//
// ──────────────────────────────────────────────────────────────────────
// That comment is the seam. Upstream AutoGPT's
// `EpisodicActionHistory.handle_compression` (in
// classic/forge/forge/components/action_history/model.py) walks the
// older episodes — those past `full_message_count` — and asks the LLM
// for a short summary, replacing each Episode.format() with the
// summary. The compressed messages then take the place of the verbose
// originals in `prepare_messages()`. We DO NOT implement compression
// here; the seam is `History.RenderMessages()` below — when the day
// comes that you exceed the context window, you swap the body of that
// method with a "summarize old, render new" branch.
//
// What s04 left as a placeholder:
//
//	type Episode struct {  /* s05 fills in */  }
//
// What s05 introduces here:
//
//	type ActionResult struct { Status, Output string }
//	type Episode struct { Actions []ActionProposal; Results []ActionResult }
//	type History []*Episode
//	    .Append(*Episode)
//	    .Current() *Episode
//	    .RenderMessages() []Message
//	    .TrimToLastN(n) History
//
// And the Loop now keeps a `*History` field, calling `Append` on a
// fresh Episode at the start of each turn and feeding the strategy the
// populated history at BuildPrompt time. The strategy renders messages
// back out of history via RenderMessages, so s04's "history is always
// nil" stops being true.
package main

import (
	"encoding/json"
	"fmt"
)

// ActionResult is the post-execute shape of one tool dispatch. Status
// captures the high-level outcome the Loop wants to remember; Output is
// what the tool produced (or the error text when it failed). Three
// statuses today:
//
//   - "ok"                  — Execute returned no error.
//   - "error"               — Execute returned an error; Output carries
//                             the error string.
//   - "interrupted_by_human" — reserved for s09; not currently produced
//                             by s05's Loop, but the Status string is
//                             the same one upstream uses so we don't
//                             have to rename anything later. Upstream:
//                             classic/forge/forge/components/action_history/
//                             model.py uses this status to skip
//                             cycle-budget decrement.
type ActionResult struct {
	Status string
	Output string
}

// Episode is one think→act→observe round. AutoGPT's upstream Episode
// (in `model.py`) is parameterized over the proposal type and stores
// an action + result + optional summary; our slimmer version stores a
// slice of actions and results so a single Episode can hold a parallel
// tool batch (one assistant turn that emits multiple tool_use blocks).
//
// In s05 the Loop only ever appends ONE proposal and ONE result per
// Episode, but the slice fields are deliberate — when s08's component
// system arrives and parallel tool execution becomes natural,
// Episode's shape doesn't need to change.
//
// Field invariant: len(Actions) == len(Results) AFTER the turn ends.
// While the model is still emitting (between proposal-append and
// result-append), len(Actions) == len(Results) + 1. Tests assert this.
type Episode struct {
	Actions []ActionProposal
	Results []ActionResult
}

// History is an ordered list of episodes — first is oldest. Pointer
// elements (rather than values) so `Current()` can return a pointer
// the caller mutates in place when the result lands.
//
// The type is a slice, not a struct, because *all* the operations we
// care about are slice-shaped (append, last-element, range). Wrapping
// in a struct would add ceremony with no payoff. Methods take a
// pointer receiver where they need to mutate the underlying slice
// (Append) and a value receiver where they don't (RenderMessages,
// Current, TrimToLastN).
type History []*Episode

// Append adds an episode to the history. Pointer receiver because we
// need to grow the underlying slice in place — `*h = append(*h, ep)`
// is the canonical Go idiom for "method that mutates the slice". The
// Loop calls this at the start of each turn (with an empty Episode)
// and the proposal/result get attached via direct pointer mutation
// thereafter.
func (h *History) Append(ep *Episode) {
	*h = append(*h, ep)
}

// Current returns the most recently appended Episode (the one the Loop
// is currently filling in). Returns nil when history is empty —
// callers MUST nil-check, even though in practice Loop only calls this
// after Append.
func (h *History) Current() *Episode {
	if len(*h) == 0 {
		return nil
	}
	return (*h)[len(*h)-1]
}

// RenderMessages reconstructs the [user-task, assistant, user, assistant, ...]
// flow that the Provider protocol expects, episode by episode. This is
// the COMPRESSION SEAM — see the file-level comment.
//
// For each episode:
//
//   - emit one assistant message containing the proposal's content
//     blocks (text Thoughts + tool_use Command/Args);
//   - emit one user message containing tool_result blocks for each
//     action that has a paired result.
//
// Episodes with no result yet (mid-turn state, where the model has
// proposed but the tool hasn't run) emit ONLY the assistant message —
// this is what makes RenderMessages safe to call from inside the Loop
// while a turn is in flight.
//
// The empty case returns []Message{} (a zero-length slice), NOT nil —
// callers (the strategy's BuildPrompt) `append` to the result, and an
// explicit empty slice keeps the JSON-encoded request shape stable
// (`messages: []` rather than `messages: null`).
func (h *History) RenderMessages() []Message {
	out := make([]Message, 0, len(*h)*2)
	for _, ep := range *h {
		if ep == nil {
			continue
		}
		// Assistant turn: assemble a Content slice from the actions.
		// Thoughts (free text) come first, then one tool_use block per
		// action — exactly the shape the original assistant emitted.
		assistantContent := make([]ContentBlock, 0, 2*len(ep.Actions))
		for i, a := range ep.Actions {
			if a.Thoughts != "" {
				assistantContent = append(assistantContent, ContentBlock{
					Type: "text",
					Text: a.Thoughts,
				})
			}
			if a.Command != "" {
				// Synthesize a stable tool_use_id from the position. The
				// model's original ID isn't retained on ActionProposal
				// (deliberate: the protocol-native ID is an implementation
				// detail of the assistant turn, not a logical attribute of
				// the proposal). We reconstruct an ID like
				// "ep<N>_act<I>" so the matching tool_result block below
				// can reference the same value.
				assistantContent = append(assistantContent, ContentBlock{
					Type:  "tool_use",
					ID:    episodeActionID(ep, len(out), i),
					Name:  a.Command,
					Input: a.Args,
				})
			}
		}
		if len(assistantContent) > 0 {
			out = append(out, Message{Role: "assistant", Content: assistantContent})
		}
		// User turn: tool_result blocks, one per Result. If the episode
		// hasn't seen its result yet (Loop mid-turn), skip — the
		// assistant message above is enough on its own.
		if len(ep.Results) == 0 {
			continue
		}
		userContent := make([]ContentBlock, 0, len(ep.Results))
		for i, r := range ep.Results {
			// Pair result[i] with action[i]; if Actions is shorter
			// (shouldn't happen, but be defensive), synthesize an ID
			// anyway so the JSON shape stays consistent.
			id := episodeActionID(ep, len(out)-1, i)
			userContent = append(userContent, ContentBlock{
				Type:        "tool_result",
				ToolUseID:   id,
				ToolContent: renderResult(r),
			})
		}
		if len(userContent) > 0 {
			out = append(out, Message{Role: "user", Content: userContent})
		}
	}
	return out
}

// TrimToLastN returns the most recent n episodes (or the full history
// if n >= len). Pedagogical helper exposed so the test can show "this
// is the shape compression *would* take if we wired it in". The Loop
// itself never calls TrimToLastN — compression is left as the s05 →
// s10 / s_full exercise.
//
// Returns a fresh History (slice header) so the caller can't mutate
// the original by writing into the returned slice's backing array.
// Note: the *Episode pointers are still shared, so a caller mutating
// an Episode in the trimmed view will also mutate the original — this
// matches Go's "share the pointed-to value, copy the slice header"
// convention and is what the Loop actually wants when it later wants
// to add summaries to old episodes in place.
func (h History) TrimToLastN(n int) History {
	if n <= 0 {
		return History{}
	}
	if n >= len(h) {
		out := make(History, len(h))
		copy(out, h)
		return out
	}
	out := make(History, n)
	copy(out, h[len(h)-n:])
	return out
}

// episodeActionID synthesizes a stable id like "ep<msgIndex>_act<i>"
// for tool_use/tool_result pairing. The msgIndex comes from the
// already-emitted message count so different episodes get different
// prefixes; act<i> separates parallel actions within one episode.
//
// _ = ep (kept for future: when Episode gets a unique id field, swap
// this body out for `ep.ID + "_act" + ...`).
func episodeActionID(ep *Episode, msgIndex int, actionIndex int) string {
	_ = ep
	return fmt.Sprintf("ep%d_act%d", msgIndex, actionIndex)
}

// renderResult turns an ActionResult into the string body that goes
// into a tool_result block. Status is prefixed when not "ok" so the
// model sees the failure mode (just "ok" outputs are passed verbatim
// without a prefix to match s04's natural shape).
func renderResult(r ActionResult) string {
	switch r.Status {
	case "", "ok":
		return r.Output
	case "error":
		if r.Output == "" {
			return "tool error"
		}
		return "tool error: " + r.Output
	default:
		// Pre-emptively cover s09's "interrupted_by_human" and any future
		// statuses so the model sees a clear status tag.
		payload, _ := json.Marshal(map[string]string{
			"status": r.Status,
			"output": r.Output,
		})
		return string(payload)
	}
}
