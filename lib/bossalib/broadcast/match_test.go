package broadcast

import (
	"reflect"
	"testing"
)

// fullTarget is a candidate with every dimension populated, so a test that
// constrains one dimension is not accidentally satisfied by another being
// zero on both sides.
var fullTarget = Target{
	ChatID:    "c1",
	SessionID: "s1",
	RepoID:    "r1",
	AgentName: "claude",
	AccountID: "a1",
	DaemonID:  "d1",
}

func TestMatches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		sel    Selector
		target Target
		want   bool
	}{
		// --- one dimension at a time: every field is actually consulted ---
		{
			name:   "chat dimension matches",
			sel:    Selector{Clauses: []Clause{{ChatIDs: []string{"c1"}}}},
			target: fullTarget,
			want:   true,
		},
		{
			name:   "chat dimension rejects a different chat",
			sel:    Selector{Clauses: []Clause{{ChatIDs: []string{"c2"}}}},
			target: fullTarget,
			want:   false,
		},
		{
			name:   "session dimension matches",
			sel:    Selector{Clauses: []Clause{{SessionIDs: []string{"s1"}}}},
			target: fullTarget,
			want:   true,
		},
		{
			name:   "session dimension rejects a different session",
			sel:    Selector{Clauses: []Clause{{SessionIDs: []string{"s2"}}}},
			target: fullTarget,
			want:   false,
		},
		{
			name:   "repo dimension matches",
			sel:    Selector{Clauses: []Clause{{RepoIDs: []string{"r1"}}}},
			target: fullTarget,
			want:   true,
		},
		{
			name:   "repo dimension rejects a different repo",
			sel:    Selector{Clauses: []Clause{{RepoIDs: []string{"r2"}}}},
			target: fullTarget,
			want:   false,
		},
		{
			name:   "agent dimension matches",
			sel:    Selector{Clauses: []Clause{{AgentNames: []string{"claude"}}}},
			target: fullTarget,
			want:   true,
		},
		{
			name:   "agent dimension rejects a different agent",
			sel:    Selector{Clauses: []Clause{{AgentNames: []string{"codex"}}}},
			target: fullTarget,
			want:   false,
		},
		{
			name:   "account dimension matches",
			sel:    Selector{Clauses: []Clause{{AccountIDs: []string{"a1"}}}},
			target: fullTarget,
			want:   true,
		},
		{
			name:   "account dimension rejects a different account",
			sel:    Selector{Clauses: []Clause{{AccountIDs: []string{"a2"}}}},
			target: fullTarget,
			want:   false,
		},
		{
			name:   "daemon dimension matches",
			sel:    Selector{Clauses: []Clause{{DaemonIDs: []string{"d1"}}}},
			target: fullTarget,
			want:   true,
		},
		{
			name:   "daemon dimension rejects a different daemon",
			sel:    Selector{Clauses: []Clause{{DaemonIDs: []string{"d2"}}}},
			target: fullTarget,
			want:   false,
		},

		// --- AND within a clause ---
		{
			name:   "AND within a clause: both dimensions match",
			sel:    Selector{Clauses: []Clause{{RepoIDs: []string{"r1"}, AgentNames: []string{"claude"}}}},
			target: fullTarget,
			want:   true,
		},
		{
			name:   "AND within a clause: first dimension misses",
			sel:    Selector{Clauses: []Clause{{RepoIDs: []string{"r2"}, AgentNames: []string{"claude"}}}},
			target: fullTarget,
			want:   false,
		},
		{
			name:   "AND within a clause: second dimension misses",
			sel:    Selector{Clauses: []Clause{{RepoIDs: []string{"r1"}, AgentNames: []string{"codex"}}}},
			target: fullTarget,
			want:   false,
		},
		{
			name: "AND within a clause: all six dimensions must match",
			sel: Selector{Clauses: []Clause{{
				ChatIDs:    []string{"c1"},
				SessionIDs: []string{"s1"},
				RepoIDs:    []string{"r1"},
				AgentNames: []string{"claude"},
				AccountIDs: []string{"a1"},
				DaemonIDs:  []string{"d1"},
			}}},
			target: fullTarget,
			want:   true,
		},
		{
			name: "AND within a clause: one of six dimensions misses",
			sel: Selector{Clauses: []Clause{{
				ChatIDs:    []string{"c1"},
				SessionIDs: []string{"s1"},
				RepoIDs:    []string{"r1"},
				AgentNames: []string{"claude"},
				AccountIDs: []string{"a1"},
				DaemonIDs:  []string{"d9"},
			}}},
			target: fullTarget,
			want:   false,
		},

		// --- an unset dimension does not constrain ---
		{
			name:   "an unset dimension does not constrain: repo-only clause ignores the agent",
			sel:    Selector{Clauses: []Clause{{RepoIDs: []string{"r1"}}}},
			target: Target{RepoID: "r1", AgentName: "codex", ChatID: "c9"},
			want:   true,
		},

		// --- OR across the values of one dimension ---
		{
			name:   "OR across values: first alternative",
			sel:    Selector{Clauses: []Clause{{AgentNames: []string{"claude", "codex"}}}},
			target: Target{AgentName: "claude"},
			want:   true,
		},
		{
			name:   "OR across values: second alternative",
			sel:    Selector{Clauses: []Clause{{AgentNames: []string{"claude", "codex"}}}},
			target: Target{AgentName: "codex"},
			want:   true,
		},
		{
			name:   "OR across values: neither alternative",
			sel:    Selector{Clauses: []Clause{{AgentNames: []string{"claude", "codex"}}}},
			target: Target{AgentName: "opencode"},
			want:   false,
		},

		// --- OR across clauses ---
		{
			name: "OR across clauses: first clause matches",
			sel: Selector{Clauses: []Clause{
				{RepoIDs: []string{"r1"}, AgentNames: []string{"claude"}},
				{AccountIDs: []string{"a9"}},
			}},
			target: fullTarget,
			want:   true,
		},
		{
			name: "OR across clauses: second clause matches",
			sel: Selector{Clauses: []Clause{
				{RepoIDs: []string{"r9"}},
				{AccountIDs: []string{"a1"}},
			}},
			target: fullTarget,
			want:   true,
		},
		{
			name: "OR across clauses: no clause matches",
			sel: Selector{Clauses: []Clause{
				{RepoIDs: []string{"r9"}},
				{AccountIDs: []string{"a9"}},
			}},
			target: fullTarget,
			want:   false,
		},

		// --- the empty-string dimensions: "" = system-default account 0 and
		// "" = the local daemon. Matches treats them as ordinary values, not
		// wildcards, so a clause naming a real id never sweeps them up. Such a
		// clause is NOT addressable through the grammar or Validate — it only
		// arrives wire-decoded — which
		// TestEmptyStringDimensionsAreNotAddressable pins.
		{
			name:   "empty account matches only when the clause lists it explicitly",
			sel:    Selector{Clauses: []Clause{{AccountIDs: []string{""}}}},
			target: Target{ChatID: "c1", AccountID: ""},
			want:   true,
		},
		{
			name:   "a clause listing the default account does not sweep up a real account",
			sel:    Selector{Clauses: []Clause{{AccountIDs: []string{""}}}},
			target: Target{ChatID: "c1", AccountID: "a1"},
			want:   false,
		},
		{
			name:   "a clause listing a real account does not sweep up the default account",
			sel:    Selector{Clauses: []Clause{{AccountIDs: []string{"a1"}}}},
			target: Target{ChatID: "c1", AccountID: ""},
			want:   false,
		},
		{
			name:   "empty daemon matches only when the clause lists it explicitly",
			sel:    Selector{Clauses: []Clause{{DaemonIDs: []string{""}}}},
			target: Target{ChatID: "c1", DaemonID: ""},
			want:   true,
		},
		{
			name:   "a clause listing the local daemon does not sweep up a remote daemon",
			sel:    Selector{Clauses: []Clause{{DaemonIDs: []string{""}}}},
			target: Target{ChatID: "c1", DaemonID: "d1"},
			want:   false,
		},
		{
			name:   "a clause listing a real daemon does not sweep up the local daemon",
			sel:    Selector{Clauses: []Clause{{DaemonIDs: []string{"d1"}}}},
			target: Target{ChatID: "c1", DaemonID: ""},
			want:   false,
		},
		{
			name:   "the default account and the local daemon AND together",
			sel:    Selector{Clauses: []Clause{{AccountIDs: []string{""}, DaemonIDs: []string{""}}}},
			target: Target{ChatID: "c1"},
			want:   true,
		},
		{
			name:   "an unconstrained account dimension still matches the default account",
			sel:    Selector{Clauses: []Clause{{ChatIDs: []string{"c1"}}}},
			target: Target{ChatID: "c1", AccountID: ""},
			want:   true,
		},

		// --- an empty selector is never "everyone" ---
		{
			name:   "the zero selector matches nothing",
			sel:    Selector{},
			target: fullTarget,
			want:   false,
		},
		{
			name:   "an explicitly empty clause list matches nothing",
			sel:    Selector{Clauses: []Clause{}},
			target: fullTarget,
			want:   false,
		},
		{
			// Decision: a clause that constrains no dimension matches NOTHING,
			// not everything. Validate already rejects such a selector, so this
			// only ever arises from an unvalidated hand-built or wire-decoded
			// value — exactly the case where "match everyone" would be a
			// daemon-wide message storm.
			name:   "an all-empty clause matches nothing rather than everything",
			sel:    Selector{Clauses: []Clause{{}}},
			target: fullTarget,
			want:   false,
		},
		{
			name:   "an all-empty clause matches nothing even for the zero target",
			sel:    Selector{Clauses: []Clause{{}}},
			target: Target{},
			want:   false,
		},
		{
			name: "an all-empty clause does not rescue a selector whose real clause misses",
			sel: Selector{Clauses: []Clause{
				{},
				{RepoIDs: []string{"r9"}},
			}},
			target: fullTarget,
			want:   false,
		},
		{
			name: "an all-empty clause does not suppress a real clause that matches",
			sel: Selector{Clauses: []Clause{
				{},
				{RepoIDs: []string{"r1"}},
			}},
			target: fullTarget,
			want:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.sel.Matches(tt.target); got != tt.want {
				t.Fatalf("Matches(%#v) = %v, want %v for selector %#v", tt.target, got, tt.want, tt.sel)
			}
		})
	}
}

// TestEmptyStringDimensionsAreNotAddressable pins the other half of the
// empty-string story the Matches table above tells: Matches handles a clause
// listing "" correctly because a wire-decoded selector can carry one, but no
// selector that reaches delivery ever can — the grammar rejects a blank value
// and Validate rejects a blank entry. Without this, the Matches cases read as
// though "account:" were a supported way to address the default account.
func TestEmptyStringDimensionsAreNotAddressable(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"account:", "daemon:", "account: ", "repo:r1,account:"} {
		if _, err := Parse(in); err == nil {
			t.Fatalf("Parse(%q) = nil error, want the grammar to reject a blank value", in)
		}
	}
	for _, sel := range []Selector{
		{Clauses: []Clause{{AccountIDs: []string{""}}}},
		{Clauses: []Clause{{DaemonIDs: []string{""}}}},
		{Clauses: []Clause{{AccountIDs: []string{""}, DaemonIDs: []string{""}}}},
		{Clauses: []Clause{{RepoIDs: []string{"r1"}, AccountIDs: []string{""}}}},
	} {
		if err := sel.Validate(); err == nil {
			t.Fatalf("Validate() = nil for %#v, want a blank entry to be rejected", sel)
		}
	}
}

// TestMatchesAgreesWithParsedSelectors pins that the matcher and the grammar
// describe the same audience: the textual form a user types resolves to the
// same verdict as the hand-built clause.
func TestMatchesAgreesWithParsedSelectors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in     string
		target Target
		want   bool
	}{
		{"repo:r1,agent:claude", fullTarget, true},
		{"repo:r1,agent:codex", fullTarget, false},
		{"agent:claude,agent:codex", Target{AgentName: "codex"}, true},
		{"repo:r9+account:a1", fullTarget, true},
		{"repo:r9+account:a9", fullTarget, false},
		{"chat:c1,chat:c2", Target{ChatID: "c2"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			sel, err := Parse(tt.in)
			if err != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", tt.in, err)
			}
			if got := sel.Matches(tt.target); got != tt.want {
				t.Fatalf("Parse(%q).Matches(%#v) = %v, want %v", tt.in, tt.target, got, tt.want)
			}
		})
	}
}

func TestFilterPreservesOrder(t *testing.T) {
	t.Parallel()
	sel := Selector{Clauses: []Clause{{RepoIDs: []string{"r1"}}}}
	in := []Target{
		{ChatID: "c1", RepoID: "r1"},
		{ChatID: "c2", RepoID: "r2"},
		{ChatID: "c3", RepoID: "r1"},
		{ChatID: "c4", RepoID: "r2"},
		{ChatID: "c5", RepoID: "r1"},
	}
	got := sel.Filter(in)
	want := []Target{
		{ChatID: "c1", RepoID: "r1"},
		{ChatID: "c3", RepoID: "r1"},
		{ChatID: "c5", RepoID: "r1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Filter() = %#v, want %#v", got, want)
	}
}

// TestFilterDoesNotMutateItsArgument guards the obvious in-place optimisation
// (`out := targets[:0]`), which would silently corrupt the caller's slice —
// bossd and bosso both hand Filter a slice they keep using afterwards.
func TestFilterDoesNotMutateItsArgument(t *testing.T) {
	t.Parallel()
	sel := Selector{Clauses: []Clause{{RepoIDs: []string{"r1"}}}}
	in := []Target{
		{ChatID: "c1", RepoID: "r2"},
		{ChatID: "c2", RepoID: "r1"},
		{ChatID: "c3", RepoID: "r2"},
	}
	want := []Target{
		{ChatID: "c1", RepoID: "r2"},
		{ChatID: "c2", RepoID: "r1"},
		{ChatID: "c3", RepoID: "r2"},
	}
	kept := sel.Filter(in)
	if !reflect.DeepEqual(in, want) {
		t.Fatalf("Filter mutated its argument: %#v, want %#v", in, want)
	}
	// The result must also be a distinct backing array, so mutating it cannot
	// reach back into the caller's slice.
	if len(kept) > 0 {
		kept[0] = Target{ChatID: "clobbered"}
		if !reflect.DeepEqual(in, want) {
			t.Fatalf("Filter aliased its argument: %#v, want %#v", in, want)
		}
	}
}

func TestFilterEdgeCases(t *testing.T) {
	t.Parallel()
	sel := Selector{Clauses: []Clause{{RepoIDs: []string{"r1"}}}}

	if got := sel.Filter(nil); len(got) != 0 {
		t.Fatalf("Filter(nil) = %#v, want empty", got)
	}
	if got := sel.Filter([]Target{}); len(got) != 0 {
		t.Fatalf("Filter(empty) = %#v, want empty", got)
	}
	if got := sel.Filter([]Target{{RepoID: "r2"}, {RepoID: "r3"}}); len(got) != 0 {
		t.Fatalf("Filter(no matches) = %#v, want empty", got)
	}

	// An empty selector filters everything out — it is never "everyone".
	all := []Target{fullTarget, {ChatID: "c2"}, {}}
	if got := (Selector{}).Filter(all); len(got) != 0 {
		t.Fatalf("zero Selector Filter = %#v, want empty", got)
	}
	if got := (Selector{Clauses: []Clause{{}}}).Filter(all); len(got) != 0 {
		t.Fatalf("all-empty-clause Filter = %#v, want empty", got)
	}
}

// TestMatchesIsAllocationFree pins the hot-path contract: bossd fans a
// selector out across every live chat, so Matches must not allocate.
func TestMatchesIsAllocationFree(t *testing.T) {
	sel := Selector{Clauses: []Clause{
		{RepoIDs: []string{"r1"}, AgentNames: []string{"claude", "codex"}},
		{AccountIDs: []string{"a9"}},
	}}
	if allocs := testing.AllocsPerRun(100, func() {
		if !sel.Matches(fullTarget) {
			t.Fatal("Matches() = false, want true")
		}
	}); allocs != 0 {
		t.Fatalf("Matches allocated %v times per run, want 0", allocs)
	}
}
