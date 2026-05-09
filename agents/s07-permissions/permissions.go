// permissions.go — the permission gate.
//
// s06 sandboxed the agent's filesystem to a workspace root. But that
// only stops escapes from the workspace; an agent with `bash` (or even
// just `write_file`) can still wipe out 6 sessions of work *inside* the
// root. AutoGPT classic answers this with `forge/permissions.py`'s
// `CommandPermissionManager`: a 4-level (`ONCE` / `AGENT` / `WORKSPACE`
// / `DENY`) permission engine that checks `(command_name, args)` against
// glob patterns before every dispatch.
//
// We pick a 2-level subset for s07: a Decision is `Allow / Deny / Ask`,
// rules live in `Permissions.AllowList` and `Permissions.DenyList`, and
// the matcher supports `*` (one path segment, no `/`) and `**` (any
// number of segments). The full 4-level scope hierarchy is left as
// Appendix B exercise #5.
//
// Pattern format: "<command-name>: <arg-glob>". Examples:
//
//	read_file: *.md     match read_file when args["path"] glob-matches *.md
//	write_file: *       match write_file with any path
//	*: secret*          match ANY command when any string arg starts with "secret"
//	bash: rm -rf*       match bash when args["command"] starts with "rm -rf"
//
// Decision tree (Check):
//
//  1. Walk DenyList; first match → Deny.
//  2. Walk AllowList; first match → Allow.
//  3. No match → Ask (caller's Asker decides).
//
// Loop integration is in loop.go: between `strategy.ParseResponse` and
// `tool.Execute` we ask Permissions.Check; on Deny we synthesize a
// "permission denied" tool_result so the model sees the rejection in
// the next turn; on Ask we delegate to an Asker. Putting the gate at
// parse time (not deep inside Execute) is the dossier's anti-pattern
// #2 prescription: keep cross-cutting concerns at the seams, not
// scattered across tool implementations.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Decision is the three-valued outcome of Permissions.Check. The values
// follow the same ordering as their constants so a switch on Decision
// reads naturally (Allow=0 first / Deny / Ask).
type Decision int

const (
	// Allow — the call is on an allow-list and has not been denied. Loop
	// proceeds straight to Tool.Execute.
	Allow Decision = iota
	// Deny — the call matched a deny pattern (deny wins over allow).
	// Loop synthesizes a "permission denied" tool_result.
	Deny
	// Ask — neither allow nor deny matched. Loop must delegate to an
	// Asker (interactive prompt in production; stubbed in tests).
	Ask
)

// String is purely for log/test readability.
func (d Decision) String() string {
	switch d {
	case Allow:
		return "Allow"
	case Deny:
		return "Deny"
	case Ask:
		return "Ask"
	}
	return fmt.Sprintf("Decision(%d)", int(d))
}

// Pattern is a single permission rule. Glob is the textual form
// "<cmd>: <arg-glob>" — we keep it as a string rather than parsing into
// a struct because the matcher (patternMatches) needs the original split
// anyway, and storing the source string makes round-trips through a
// config file lossless.
type Pattern struct {
	Glob string
}

// Permissions is the rule set the Loop consults each turn. AllowList
// and DenyList are slices (not sets) because order matters in two ways:
// first-match-wins inside a list, and DenyList is checked before
// AllowList (deny short-circuits).
type Permissions struct {
	AllowList []Pattern
	DenyList  []Pattern
}

// NewPermissions constructs an empty rule set. With no rules every
// Check returns Ask — the safe default.
func NewPermissions() *Permissions {
	return &Permissions{}
}

// AddAllow appends an allow rule. Multiple calls accumulate; rules are
// evaluated in registration order on Check.
func (p *Permissions) AddAllow(glob string) {
	p.AllowList = append(p.AllowList, Pattern{Glob: glob})
}

// AddDeny appends a deny rule. Same accumulation/ordering semantics as
// AddAllow, but DenyList is consulted FIRST in Check.
func (p *Permissions) AddDeny(glob string) {
	p.DenyList = append(p.DenyList, Pattern{Glob: glob})
}

// Check evaluates a (cmd, args) pair against the rules and returns a
// Decision. The decision tree:
//
//  1. For each pattern in DenyList: if patternMatches → Deny.
//  2. For each pattern in AllowList: if patternMatches → Allow.
//  3. Otherwise → Ask.
//
// Why deny before allow? A specific deny ("bash: rm -rf*") should win
// over a broad allow ("bash: **") so the operator can carve dangerous
// holes out of broad permissions without rewriting the broad rule.
//
// args is the same map[string]interface{} the model emitted for the
// tool's input; we treat every string-valued entry as a candidate for
// the arg-glob to match against. This is deliberately permissive: a
// pattern "*: secret*" can hit a "key" arg, a "path" arg, or a "url"
// arg — the agent shouldn't be able to dodge the rule by renaming a
// field.
func (p *Permissions) Check(cmd string, args map[string]interface{}) Decision {
	for _, pat := range p.DenyList {
		if patternMatches(pat.Glob, cmd, args) {
			return Deny
		}
	}
	for _, pat := range p.AllowList {
		if patternMatches(pat.Glob, cmd, args) {
			return Allow
		}
	}
	return Ask
}

// patternMatches splits a "<cmd>: <arg-glob>" pattern and tests it
// against (cmd, args). The split is on the FIRST `: ` so arg globs may
// themselves contain colons (e.g., "bash: rm -rf:tmp" — admittedly
// silly but legal).
//
// Cmd matching:
//
//	"*"       matches any command name
//	otherwise the literal command name (case-sensitive)
//
// Arg matching:
//
//	"*"       trivially matches any string arg (bare wildcard rule)
//	otherwise glob-matches against EACH string-valued entry in args;
//	if ANY entry matches, the pattern matches.
//
// We don't try to be clever about typed args (numbers, bools): the
// pattern format is string-shaped and we only test against strings.
// A model that emits {"count": 42} for a tool with no string args
// won't match any non-trivial pattern — which means rules can't
// accidentally veto a numeric-only call. If the rule writer wants to
// gate every call regardless of args, they use "<cmd>: *" which the
// match-any-string branch covers.
func patternMatches(pattern, cmd string, args map[string]interface{}) bool {
	idx := strings.Index(pattern, ":")
	if idx < 0 {
		return false
	}
	cmdGlob := strings.TrimSpace(pattern[:idx])
	argGlob := strings.TrimSpace(pattern[idx+1:])

	// Command name match: "*" is a free pass; otherwise exact compare.
	if cmdGlob != "*" && cmdGlob != cmd {
		return false
	}

	// Bare-wildcard arg-glob: any args (or no args) match. This is the
	// "<cmd>: *" rule that gates the command regardless of what it
	// emits.
	if argGlob == "*" || argGlob == "**" {
		return true
	}

	// Test the arg-glob against every string-valued arg. If ANY one
	// matches, the rule fires. This is the deliberate looseness from
	// the doc-comment above: prevents the agent from dodging a rule by
	// renaming `path` to `target`, etc.
	if len(args) == 0 {
		// No args at all — only the bare-wildcard arg-glob matches, and
		// we already handled that above.
		return false
	}
	for _, v := range args {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if globMatch(argGlob, s) {
			return true
		}
	}
	return false
}

// globMatch is a tiny custom glob matcher with two operators:
//
//	*   matches any number of characters EXCEPT '/' (one path segment)
//	**  matches any number of characters INCLUDING '/' (any segments)
//	?   matches exactly one character (any character)
//
// We deliberately don't use `path/filepath.Match` because:
//
//  1. it doesn't model `**`;
//  2. its escape rules differ across platforms (`\` is special on
//     non-Windows);
//  3. arg values are not paths — they may be URLs, shell commands,
//     arbitrary strings — and `filepath.Match` is documented as a path
//     matcher.
//
// Implementation: a recursive scan over the pattern, with `**` swallowing
// any prefix of the input (greedy + backtracking on mismatch). Inputs of
// realistic agent size (paths, shell strings, queries) are small enough
// that the worst-case O(len(pattern)*len(input)) is fine.
func globMatch(pattern, input string) bool {
	pi, ii := 0, 0
	// star: index in input that the most recent '*' matched up to so we
	// can backtrack if the rest of the pattern fails further on.
	starPi, starII := -1, -1
	doubleStar := false

	for ii < len(input) {
		if pi < len(pattern) {
			pc := pattern[pi]
			switch {
			case pc == '*':
				// Detect ** (double-star: cross-segment match).
				if pi+1 < len(pattern) && pattern[pi+1] == '*' {
					doubleStar = true
					pi += 2
				} else {
					doubleStar = false
					pi++
				}
				starPi = pi
				starII = ii
				continue
			case pc == '?':
				pi++
				ii++
				continue
			case pc == input[ii]:
				pi++
				ii++
				continue
			}
		}
		// Mismatch (or pattern exhausted with input remaining). Try to
		// backtrack to the most recent '*' and consume one more
		// character of input under it.
		if starPi >= 0 {
			// Single-star can't swallow '/'; if the next char in input
			// is '/', backtracking under a single-star is illegal.
			if !doubleStar && ii < len(input) && input[ii] == '/' {
				return false
			}
			ii = starII + 1
			starII = ii
			pi = starPi
			continue
		}
		return false
	}
	// Input exhausted; pattern must be all-star (or already fully consumed).
	for pi < len(pattern) {
		if pattern[pi] != '*' {
			return false
		}
		pi++
	}
	return true
}

// ──────────────────────────────────────────────────────────────────────
// Asker — the prompt callback for `Ask` decisions.
//
// AutoGPT upstream's permissions module takes a `prompt_fn` callback
// because it lives in a process where stdin is the obvious channel. We
// model it as an interface for two reasons (per dossier anti-pattern
// #1: avoid global stdin reader):
//
//  1. tests want a stub that returns canned answers without spinning up
//     a fake TTY;
//  2. s09 will introduce a fancier UI provider (spinner + Rich-style
//     prompts) and we want a clean seam to swap stdin for that.
// ──────────────────────────────────────────────────────────────────────

// Asker decides Allow vs Deny when Permissions.Check returns Ask. It
// MUST return only Allow or Deny — never Ask (otherwise the Loop loops
// forever on the same prompt).
type Asker interface {
	Ask(cmd string, args map[string]interface{}) Decision
}

// StdinAsker is the production implementation: print a one-line
// summary of the call to stderr, read y/n from stdin, return Allow on
// "y" / "yes" and Deny on anything else.
//
// Output goes to stderr (not stdout) so the agent's final-answer
// stream remains parseable when the binary is piped. Reads happen via
// a long-lived bufio.Reader so subsequent calls don't drop unread
// bytes. We construct the reader lazily on first Ask.
type StdinAsker struct {
	reader *bufio.Reader
}

// NewStdinAsker builds an asker bound to os.Stdin. For non-TTY use
// (CI, pipe), reads block until EOF; the caller should set a default
// rule or wrap with a different Asker.
func NewStdinAsker() *StdinAsker {
	return &StdinAsker{}
}

// Ask prints "permission required: <cmd>(<args>) [y/N]: " to stderr
// and reads a line from stdin. Anything starting with 'y' or 'Y'
// (case-insensitive) returns Allow; everything else (including empty
// line and EOF) returns Deny — fail-closed is the only safe default
// for an interactive permission prompt.
func (s *StdinAsker) Ask(cmd string, args map[string]interface{}) Decision {
	if s.reader == nil {
		s.reader = bufio.NewReader(os.Stdin)
	}
	fmt.Fprintf(os.Stderr, "permission required: %s(%v) [y/N]: ", cmd, args)
	line, err := s.reader.ReadString('\n')
	if err != nil {
		return Deny
	}
	line = strings.ToLower(strings.TrimSpace(line))
	if strings.HasPrefix(line, "y") {
		return Allow
	}
	return Deny
}

// StubAsker is for tests. It always returns the configured `reply`
// regardless of cmd/args, and records every call for assertions.
type StubAsker struct {
	reply Decision
	Calls []StubAskerCall
}

// StubAskerCall captures one Ask invocation for test assertions.
type StubAskerCall struct {
	Cmd  string
	Args map[string]interface{}
}

// NewStubAsker constructs a stub that returns `reply` from every Ask.
// `reply` MUST be Allow or Deny — using Ask here loops the Loop.
func NewStubAsker(reply Decision) *StubAsker {
	return &StubAsker{reply: reply}
}

// Ask records the call and returns the configured reply.
func (s *StubAsker) Ask(cmd string, args map[string]interface{}) Decision {
	s.Calls = append(s.Calls, StubAskerCall{Cmd: cmd, Args: args})
	return s.reply
}

// Compile-time checks.
var (
	_ Asker = (*StdinAsker)(nil)
	_ Asker = (*StubAsker)(nil)
)
