package main

import "testing"

// TestPermissions_PatternMatchBasic — the simplest contract: a pattern
// "read_file: *.md" with args {"path": "notes.md"} matches and
// returns Allow when on the AllowList. This test pins the happy-path
// dispatch through Check + patternMatches + globMatch in one shot.
func TestPermissions_PatternMatchBasic(t *testing.T) {
	p := NewPermissions()
	p.AddAllow("read_file: *.md")

	got := p.Check("read_file", map[string]interface{}{"path": "notes.md"})
	if got != Allow {
		t.Errorf("read_file notes.md → %v, want Allow", got)
	}

	// And a non-matching extension falls through to Ask.
	got = p.Check("read_file", map[string]interface{}{"path": "secret.txt"})
	if got != Ask {
		t.Errorf("read_file secret.txt with only an *.md allow → %v, want Ask", got)
	}

	// And a non-matching command name also falls to Ask.
	got = p.Check("write_file", map[string]interface{}{"path": "notes.md"})
	if got != Ask {
		t.Errorf("write_file with only a read_file allow → %v, want Ask", got)
	}
}

// TestPermissions_DenyOverridesAllow — when the same call matches BOTH
// an allow and a deny rule, Deny wins. This is the design's load-bearing
// invariant: it lets operators carve dangerous holes ("bash: rm -rf**")
// out of broad allow rules ("bash: **") without rewriting the broad
// rule.
//
// Note: the deny rule uses `**` (cross-segment) because shell args like
// "rm -rf /tmp/foo" contain `/`, which a single `*` won't span.
func TestPermissions_DenyOverridesAllow(t *testing.T) {
	p := NewPermissions()
	p.AddAllow("bash: **")     // broad: every bash invocation is allowed
	p.AddDeny("bash: rm -rf**") // narrow: dangerous form is denied (** crosses '/')

	// The narrow form: deny wins.
	got := p.Check("bash", map[string]interface{}{"command": "rm -rf /tmp/foo"})
	if got != Deny {
		t.Errorf("bash with rm -rf → %v, want Deny (deny must override allow)", got)
	}

	// A safe form: still allowed.
	got = p.Check("bash", map[string]interface{}{"command": "ls -la"})
	if got != Allow {
		t.Errorf("bash with ls -la → %v, want Allow", got)
	}
}

// TestPermissions_AskByDefault — a Permissions with NO rules returns
// Ask for everything. This is the safe default: no rules = every call
// must be approved.
func TestPermissions_AskByDefault(t *testing.T) {
	p := NewPermissions()

	for _, cmd := range []string{"echo", "read_file", "write_file", "bash"} {
		if got := p.Check(cmd, map[string]interface{}{"any": "thing"}); got != Ask {
			t.Errorf("Check(%q) on empty Permissions → %v, want Ask", cmd, got)
		}
	}
	// And empty args also asks (rather than silently allowing).
	if got := p.Check("echo", nil); got != Ask {
		t.Errorf("Check on empty Permissions with nil args → %v, want Ask", got)
	}
}

// TestPermissions_FirstDenyShortCircuits — when DenyList has multiple
// rules, the FIRST match returns Deny without evaluating the rest. We
// observe this by having the second deny rule match a different
// command — after the first match we return immediately, so the
// second never gets a chance to (incorrectly) report a non-match.
//
// The contract is "first-match-wins inside the deny list", which is
// what makes adding new deny rules cheap (you can prepend without
// auditing every later rule).
func TestPermissions_FirstDenyShortCircuits(t *testing.T) {
	p := NewPermissions()
	p.AddDeny("bash: rm*")    // matches first
	p.AddDeny("bash: ls*")    // would also match if we kept evaluating

	got := p.Check("bash", map[string]interface{}{"command": "rm dangerous"})
	if got != Deny {
		t.Fatalf("first deny → %v, want Deny", got)
	}

	// And the second deny still works on its own when the first doesn't match.
	got = p.Check("bash", map[string]interface{}{"command": "ls -la"})
	if got != Deny {
		t.Errorf("second deny → %v, want Deny", got)
	}

	// AllowList must NOT be consulted once any deny matches. Sanity-check
	// by adding a permissive allow rule; the deny still wins.
	p.AddAllow("bash: **")
	got = p.Check("bash", map[string]interface{}{"command": "rm dangerous"})
	if got != Deny {
		t.Errorf("with broad allow but matching deny → %v, want Deny", got)
	}
}

// TestPermissions_CmdOnlyPattern — a pattern "<cmd>: *" gates the
// command regardless of what args it takes. This is the form most
// rules will use in practice ("read_file: *" = "any read_file is fine").
func TestPermissions_CmdOnlyPattern(t *testing.T) {
	p := NewPermissions()
	p.AddAllow("read_file: *")

	// Matches with any string arg.
	if got := p.Check("read_file", map[string]interface{}{"path": "foo.md"}); got != Allow {
		t.Errorf("read_file with bare-* allow + any path → %v, want Allow", got)
	}
	// Matches with no args at all (empty map).
	if got := p.Check("read_file", map[string]interface{}{}); got != Allow {
		t.Errorf("read_file with bare-* allow + empty args → %v, want Allow", got)
	}
	// Matches with non-string args (numbers).
	if got := p.Check("read_file", map[string]interface{}{"limit": 100}); got != Allow {
		t.Errorf("read_file with bare-* allow + numeric arg → %v, want Allow", got)
	}
	// Different command falls through to Ask.
	if got := p.Check("write_file", map[string]interface{}{"path": "foo.md"}); got != Ask {
		t.Errorf("write_file with only a read_file rule → %v, want Ask", got)
	}
}

// TestPermissions_ArgGlobPattern — patterns can match against any
// string-valued arg (not just one named "path"). This is the "*: secret*"
// form: catch every command whose args mention something starting with
// "secret".
func TestPermissions_ArgGlobPattern(t *testing.T) {
	p := NewPermissions()
	p.AddDeny("*: secret*")

	// "*: secret*" should match any cmd with any string arg starting "secret".
	cases := []struct {
		name string
		cmd  string
		args map[string]interface{}
		want Decision
	}{
		{"read_file with secret-prefix path", "read_file",
			map[string]interface{}{"path": "secret-data.txt"}, Deny},
		{"web_fetch with secret-prefix value", "web_fetch",
			map[string]interface{}{"key": "secret-key-1"}, Deny},
		{"any command with non-secret arg", "read_file",
			map[string]interface{}{"path": "public.md"}, Ask},
		{"any command with non-string secret-shaped arg", "read_file",
			map[string]interface{}{"id": 12345}, Ask}, // numbers don't match string globs
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.Check(tc.cmd, tc.args); got != tc.want {
				t.Errorf("%s → %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestPermissions_DoubleStarMatchesAcrossSegments — `**` is the
// cross-segment wildcard; `*` is single-segment. So "read_file: **"
// matches "src/main.go" but "read_file: *" does NOT (a single `*` won't
// swallow the `/`). This pins the matcher's load-bearing distinction.
func TestPermissions_DoubleStarMatchesAcrossSegments(t *testing.T) {
	t.Run("** matches across slashes", func(t *testing.T) {
		p := NewPermissions()
		p.AddAllow("read_file: **")

		if got := p.Check("read_file", map[string]interface{}{"path": "src/main.go"}); got != Allow {
			t.Errorf("** + nested path → %v, want Allow", got)
		}
		if got := p.Check("read_file", map[string]interface{}{"path": "a/b/c/d/e.md"}); got != Allow {
			t.Errorf("** + deeply-nested path → %v, want Allow", got)
		}
	})

	t.Run("single * does NOT match across slashes", func(t *testing.T) {
		p := NewPermissions()
		p.AddAllow("read_file: *.md")

		// "notes.md" — no slashes, matches.
		if got := p.Check("read_file", map[string]interface{}{"path": "notes.md"}); got != Allow {
			t.Errorf("*.md + flat path → %v, want Allow", got)
		}
		// "src/notes.md" — has a slash, the single * can't span it.
		if got := p.Check("read_file", map[string]interface{}{"path": "src/notes.md"}); got != Ask {
			t.Errorf("*.md + nested path → %v, want Ask (single * must not cross /)", got)
		}
	})
}

// TestStubAsker_RecordsCalls — small sanity test on the test fixture
// itself: the Stub returns its canned reply and records (cmd, args) so
// loop_test can assert exactly what the Loop forwarded.
func TestStubAsker_RecordsCalls(t *testing.T) {
	s := NewStubAsker(Allow)

	got := s.Ask("read_file", map[string]interface{}{"path": "x.md"})
	if got != Allow {
		t.Errorf("StubAsker(Allow).Ask → %v, want Allow", got)
	}
	if len(s.Calls) != 1 {
		t.Fatalf("len(Calls) = %d, want 1", len(s.Calls))
	}
	if s.Calls[0].Cmd != "read_file" {
		t.Errorf("Calls[0].Cmd = %q, want %q", s.Calls[0].Cmd, "read_file")
	}
	if got, ok := s.Calls[0].Args["path"].(string); !ok || got != "x.md" {
		t.Errorf("Calls[0].Args[path] = %v, want \"x.md\"", s.Calls[0].Args["path"])
	}

	// And a Deny stub returns Deny.
	d := NewStubAsker(Deny)
	if got := d.Ask("any", nil); got != Deny {
		t.Errorf("StubAsker(Deny).Ask → %v, want Deny", got)
	}
}
