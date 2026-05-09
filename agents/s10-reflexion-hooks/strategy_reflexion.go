// strategy_reflexion.go — the Reflexion strategy wrapper.
//
// AutoGPT classic's `prompt_strategies/reflexion.py` adds a SECOND LLM
// pass to evaluate each proposed action before the agent commits to
// executing it. The pattern:
//
//  1. Base strategy (one_shot) builds prompt → LLM → parses proposal.
//  2. Reflexion sends a second prompt: "Here is the proposal. Is it
//     sound? Reply with JSON {sound: bool, revised: ActionProposal?}."
//  3. If sound=false and revised is provided, swap the proposal for the
//     revision before the agent dispatches the tool.
//
// In Python this is woven into `propose_action`. In our Go translation
// the natural seam is **the AfterParseHook** (pipeline.go): Reflexion
// registers a hook on construction that does the second-pass call and
// mutates the proposal in place. The Loop doesn't know reflexion exists —
// it just runs the pipeline like normal — and that's exactly the
// architectural payoff. Hooks decouple cross-cutting concerns from the
// strategy class itself.
//
// ReflexionStrategy delegates BuildPrompt / ParseResponse to the base
// strategy unchanged: prompt construction and primary parsing are not
// where reflexion intervenes. The hook registration happens in the
// constructor so the Pipeline already has the AfterParseHook in place
// before the Loop starts iterating.
//
// The reflexion prompt is intentionally minimal — a single user message
// asking for {sound, revised} JSON. A real implementation might add a
// rubric, few-shot examples, or chain-of-thought; we keep it tight so
// the test can use a MockProvider with a canned JSON response.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ReflexionStrategy wraps a base PromptStrategy with a second-pass LLM
// evaluator. On construction, it registers an AfterParseHook on the
// supplied pipeline that issues a separate Provider.CreateMessage call
// asking the model to verify (and optionally revise) the just-parsed
// proposal.
//
// Composition note: Reflexion is BOTH a Strategy (so the Loop can swap
// it in via -strategy=reflexion) AND a hook registrar. This dual role is
// deliberate — it's the cleanest way to teach "a Strategy variant can
// reuse cross-cutting plumbing instead of duplicating logic." See
// docs/{en,zh}/s10-reflexion-hooks.md for the longer discussion.
type ReflexionStrategy struct {
	base     PromptStrategy
	provider Provider
	pipeline *Pipeline
}

// reflexionVerdict is the JSON shape the second-pass LLM is asked to
// emit. The "revised" field is optional — when sound=true, it is
// expected to be absent or null.
type reflexionVerdict struct {
	Sound   bool                  `json:"sound"`
	Reason  string                `json:"reason,omitempty"`
	Revised *reflexionRevisedSpec `json:"revised,omitempty"`
}

type reflexionRevisedSpec struct {
	Command  string                 `json:"command"`
	Args     map[string]interface{} `json:"args,omitempty"`
	Thoughts string                 `json:"thoughts,omitempty"`
}

// NewReflexionStrategy constructs the wrapper and registers its
// AfterParseHook on the supplied pipeline. Both base and pipeline are
// required; provider is required only when the hook actually fires (a
// caller that wants to inspect the wrapper without calling the LLM can
// pass a NoopProvider, but the natural setup is a real provider).
//
// The hook captures `provider` by closure so each call uses the same
// upstream model. Production code might choose a cheaper "fast_llm" for
// reflexion to control cost — that's a one-line change in main.go.
func NewReflexionStrategy(base PromptStrategy, provider Provider, pipeline *Pipeline) *ReflexionStrategy {
	r := &ReflexionStrategy{
		base:     base,
		provider: provider,
		pipeline: pipeline,
	}
	if pipeline != nil {
		pipeline.RegisterAfterParse(r.afterParseHook)
	}
	return r
}

// BuildPrompt delegates to the base strategy unchanged. Reflexion does
// not alter the primary prompt — the second pass uses its own freshly
// constructed prompt (see afterParseHook).
func (r *ReflexionStrategy) BuildPrompt(history []*Episode, tools []ToolSchema, directives []string, task string) []Message {
	return r.base.BuildPrompt(history, tools, directives, task)
}

// ParseResponse delegates to the base strategy unchanged. The hook runs
// AFTER ParseResponse (per Pipeline.RunAfterParse contract), so the
// second-pass evaluation sees a fully-parsed proposal.
func (r *ReflexionStrategy) ParseResponse(content []ContentBlock) (ActionProposal, error) {
	return r.base.ParseResponse(content)
}

// afterParseHook is the AfterParseHook this strategy registers. On each
// invocation it:
//
//  1. Skips if the proposal carries no command (nothing to evaluate).
//  2. Builds a single-user-message reflexion prompt asking for
//     {sound: bool, revised?: ActionProposal} JSON.
//  3. Calls Provider.CreateMessage with that prompt. Provider failures
//     halt the pipeline (per RunAfterParse contract).
//  4. Parses the response. Garbled JSON is treated as "sound=true" —
//     we don't want a second-pass parse failure to block real actions.
//  5. If sound=false and revised is non-nil, swaps the in-place
//     proposal for the revision.
func (r *ReflexionStrategy) afterParseHook(ctx context.Context, prop *ActionProposal) error {
	if prop == nil || prop.Command == "" {
		return nil
	}
	if r.provider == nil {
		// No provider configured → reflexion is a no-op. This is the
		// "construct-and-inspect" path; production setups always supply
		// a provider.
		return nil
	}

	question := r.buildReflexionPrompt(prop)
	resp, err := r.provider.CreateMessage(ctx, CreateMessageRequest{
		Messages: []Message{{
			Role:    "user",
			Content: []ContentBlock{{Type: "text", Text: question}},
		}},
	})
	if err != nil {
		return fmt.Errorf("reflexion second-pass: %w", err)
	}

	verdict, parseErr := parseReflexionVerdict(resp.Content)
	if parseErr != nil {
		// A garbled second-pass response shouldn't block the agent. We
		// log the issue (best-effort) and pass the original proposal
		// through. Tests pin this behavior: a bad JSON verdict leaves
		// the original Command intact.
		_ = parseErr
		return nil
	}

	if !verdict.Sound && verdict.Revised != nil {
		prop.Command = verdict.Revised.Command
		if verdict.Revised.Args != nil {
			prop.Args = verdict.Revised.Args
		}
		if verdict.Revised.Thoughts != "" {
			prop.Thoughts = verdict.Revised.Thoughts
		}
	}
	return nil
}

// buildReflexionPrompt formats the second-pass question for the LLM.
// Format choices:
//
//   - Single user message (no system prompt): the second pass is a
//     standalone "is this sound?" judgment, not a continuing conversation.
//   - JSON-only response demand: easier to parse than free text and
//     matches AutoGPT upstream's structured-output preference.
//   - Args rendered via json.Marshal: faithful representation, no
//     ambiguity from Go's default fmt.
func (r *ReflexionStrategy) buildReflexionPrompt(prop *ActionProposal) string {
	argsJSON, err := json.Marshal(prop.Args)
	if err != nil {
		argsJSON = []byte("{}")
	}
	var b strings.Builder
	b.WriteString("Evaluate this proposed action and respond with JSON only.\n\n")
	b.WriteString("Proposed action:\n")
	fmt.Fprintf(&b, "  command: %s\n", prop.Command)
	fmt.Fprintf(&b, "  args: %s\n", string(argsJSON))
	if prop.Thoughts != "" {
		fmt.Fprintf(&b, "  thoughts: %s\n", prop.Thoughts)
	}
	b.WriteString("\nReply with this exact JSON shape:\n")
	b.WriteString(`{"sound": <bool>, "reason": "<short>", "revised": {"command": "...", "args": {...}, "thoughts": "..."}}`)
	b.WriteString("\n")
	b.WriteString("If sound=true, omit the \"revised\" field.\n")
	b.WriteString("If sound=false, include a corrected revised proposal.\n")
	return b.String()
}

// parseReflexionVerdict pulls a reflexionVerdict out of the response
// content. It accepts either a raw text block containing JSON or a
// fenced ```json ... ``` block (so the same parser works against
// chatty models that wrap their output).
func parseReflexionVerdict(content []ContentBlock) (reflexionVerdict, error) {
	var combined strings.Builder
	for _, b := range content {
		if b.Type == "text" {
			combined.WriteString(b.Text)
		}
	}
	raw := strings.TrimSpace(combined.String())
	if raw == "" {
		return reflexionVerdict{}, fmt.Errorf("empty reflexion response")
	}

	// Try direct JSON parse first.
	var v reflexionVerdict
	if err := json.Unmarshal([]byte(raw), &v); err == nil {
		return v, nil
	}

	// Fallback: extract from a fenced code block.
	if match := fenceRegex.FindStringSubmatch(raw); len(match) > 1 {
		payload := strings.TrimSpace(match[1])
		if err := json.Unmarshal([]byte(payload), &v); err == nil {
			return v, nil
		}
	}

	return reflexionVerdict{}, fmt.Errorf("reflexion verdict not parseable as JSON: %q", raw)
}

// Compile-time check that ReflexionStrategy satisfies PromptStrategy.
var _ PromptStrategy = (*ReflexionStrategy)(nil)
