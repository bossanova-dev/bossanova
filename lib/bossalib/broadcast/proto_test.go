package broadcast

import (
	"fmt"
	"reflect"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// protoRoundTripCorpus covers every dimension, multi-value fields, multi-clause
// selectors, and the empty-string account/daemon values that only a hand-built
// clause can express.
var protoRoundTripCorpus = []struct {
	name string
	sel  Selector
}{
	{
		name: "single dimension",
		sel:  Selector{Clauses: []Clause{{RepoIDs: []string{"r1"}}}},
	},
	{
		name: "every dimension populated",
		sel: Selector{Clauses: []Clause{{
			ChatIDs:    []string{"c1"},
			SessionIDs: []string{"s1"},
			RepoIDs:    []string{"r1"},
			AgentNames: []string{"claude"},
			AccountIDs: []string{"a1"},
			DaemonIDs:  []string{"d1"},
		}}},
	},
	{
		name: "multiple values per dimension",
		sel:  Selector{Clauses: []Clause{{AgentNames: []string{"claude", "codex"}}}},
	},
	{
		name: "multiple clauses keep their order",
		sel: Selector{Clauses: []Clause{
			{RepoIDs: []string{"r2"}},
			{RepoIDs: []string{"r1"}},
			{AccountIDs: []string{"a1"}},
		}},
	},
	{
		// "" = system-default account 0 / the local daemon: not addressable —
		// the grammar rejects a blank value and Validate rejects a blank entry
		// (see TestEmptyStringDimensionsAreNotAddressable) — so this shape only
		// ever arrives wire-decoded. The conversion must still carry it through
		// losslessly rather than dropping it, so the delivery boundary judges
		// the selector exactly as it was sent instead of a silently narrowed
		// one.
		name: "empty-string account and daemon survive the round trip",
		sel:  Selector{Clauses: []Clause{{AccountIDs: []string{""}, DaemonIDs: []string{""}}}},
	},
	{
		name: "an all-empty clause survives rather than being silently dropped",
		sel:  Selector{Clauses: []Clause{{RepoIDs: []string{"r1"}}, {}}},
	},
}

func TestSelectorProtoRoundTripIsIdentity(t *testing.T) {
	t.Parallel()
	for _, tt := range protoRoundTripCorpus {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := SelectorFromProto(SelectorToProto(tt.sel))
			if !reflect.DeepEqual(got, tt.sel) {
				t.Fatalf("round trip = %#v, want %#v", got, tt.sel)
			}
		})
	}
}

func TestSelectorProtoRoundTripFromParsedSelectors(t *testing.T) {
	t.Parallel()
	for _, in := range roundTripCorpus {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			want, err := Parse(in)
			if err != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", in, err)
			}
			got := SelectorFromProto(SelectorToProto(want))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip of %q = %#v, want %#v", in, got, want)
			}
			// The canonical text must survive the wire trip too, since later
			// children persist and compare that string.
			if got.String() != want.String() {
				t.Fatalf("round trip of %q changed the canonical form: %q, want %q", in, got.String(), want.String())
			}
		})
	}
}

// TestSelectorToProtoMapsEachDimensionToItsOwnField guards against a
// copy-paste in the conversion wiring two dimensions to one proto field, which
// a symmetric round-trip test alone would not catch.
func TestSelectorToProtoMapsEachDimensionToItsOwnField(t *testing.T) {
	t.Parallel()
	sel := Selector{Clauses: []Clause{{
		ChatIDs:    []string{"c1"},
		SessionIDs: []string{"s1"},
		RepoIDs:    []string{"r1"},
		AgentNames: []string{"claude"},
		AccountIDs: []string{"a1"},
		DaemonIDs:  []string{"d1"},
	}}}
	msg := SelectorToProto(sel)
	if len(msg.GetClauses()) != 1 {
		t.Fatalf("SelectorToProto produced %d clauses, want 1", len(msg.GetClauses()))
	}
	clause := msg.GetClauses()[0]
	checks := []struct {
		field string
		got   []string
		want  []string
	}{
		{"chat_ids", clause.GetChatIds(), []string{"c1"}},
		{"session_ids", clause.GetSessionIds(), []string{"s1"}},
		{"repo_ids", clause.GetRepoIds(), []string{"r1"}},
		{"agent_names", clause.GetAgentNames(), []string{"claude"}},
		{"account_ids", clause.GetAccountIds(), []string{"a1"}},
		{"daemon_ids", clause.GetDaemonIds(), []string{"d1"}},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Fatalf("SelectorToProto %s = %#v, want %#v", c.field, c.got, c.want)
		}
	}
}

// TestProtoRoundTripCoversEveryDimension builds its clause through the shared
// fields table rather than by hand, so adding a seventh dimension without
// teaching the conversion about it turns this test red instead of silently
// dropping the new dimension on the wire.
func TestProtoRoundTripCoversEveryDimension(t *testing.T) {
	t.Parallel()
	var clause Clause
	for i, f := range fields {
		*f.get(&clause) = []string{fmt.Sprintf("%s-v%d", f.key, i)}
	}
	sel := Selector{Clauses: []Clause{clause}}
	got := SelectorFromProto(SelectorToProto(sel))
	if !reflect.DeepEqual(got, sel) {
		t.Fatalf("round trip dropped a dimension: %#v, want %#v", got, sel)
	}
}

// protoFields mirrors the package's fields table on the wire side, in the same
// canonical order, so the aliasing guards below cover all six dimensions in
// both directions rather than the one that happened to be spot-checked —
// dropping slices.Clone on any single dimension is otherwise invisible.
var protoFields = []struct {
	key string
	get func(*pb.BroadcastSelectorClause) []string
	set func(*pb.BroadcastSelectorClause, []string)
}{
	{"chat", func(c *pb.BroadcastSelectorClause) []string { return c.GetChatIds() }, func(c *pb.BroadcastSelectorClause, v []string) { c.ChatIds = v }},
	{"session", func(c *pb.BroadcastSelectorClause) []string { return c.GetSessionIds() }, func(c *pb.BroadcastSelectorClause, v []string) { c.SessionIds = v }},
	{"repo", func(c *pb.BroadcastSelectorClause) []string { return c.GetRepoIds() }, func(c *pb.BroadcastSelectorClause, v []string) { c.RepoIds = v }},
	{"agent", func(c *pb.BroadcastSelectorClause) []string { return c.GetAgentNames() }, func(c *pb.BroadcastSelectorClause, v []string) { c.AgentNames = v }},
	{"account", func(c *pb.BroadcastSelectorClause) []string { return c.GetAccountIds() }, func(c *pb.BroadcastSelectorClause, v []string) { c.AccountIds = v }},
	{"daemon", func(c *pb.BroadcastSelectorClause) []string { return c.GetDaemonIds() }, func(c *pb.BroadcastSelectorClause, v []string) { c.DaemonIds = v }},
}

// requireFieldTablesAgree fails if protoFields has drifted from fields, so a
// seventh dimension (or a reorder) cannot silently leave a dimension unguarded.
func requireFieldTablesAgree(t *testing.T) {
	t.Helper()
	if len(protoFields) != len(fields) {
		t.Fatalf("protoFields has %d entries, fields has %d", len(protoFields), len(fields))
	}
	for i, pf := range protoFields {
		if pf.key != fields[i].key {
			t.Fatalf("protoFields[%d] is %q, fields[%d] is %q", i, pf.key, i, fields[i].key)
		}
	}
}

func TestSelectorToProtoDoesNotAliasTheSelector(t *testing.T) {
	t.Parallel()
	requireFieldTablesAgree(t)
	for i, pf := range protoFields {
		t.Run(pf.key, func(t *testing.T) {
			t.Parallel()
			want := []string{"v0", "v1"}
			var clause Clause
			*fields[i].get(&clause) = []string{"v0", "v1"}
			sel := Selector{Clauses: []Clause{clause}}

			msg := SelectorToProto(sel)
			pf.get(msg.GetClauses()[0])[0] = "clobbered"

			if got := *fields[i].get(&sel.Clauses[0]); !reflect.DeepEqual(got, want) {
				t.Fatalf("SelectorToProto aliased %q: %#v, want %#v", pf.key, got, want)
			}
		})
	}
}

func TestSelectorFromProtoDoesNotAliasTheMessage(t *testing.T) {
	t.Parallel()
	requireFieldTablesAgree(t)
	for i, pf := range protoFields {
		t.Run(pf.key, func(t *testing.T) {
			t.Parallel()
			want := []string{"v0", "v1"}
			clause := &pb.BroadcastSelectorClause{}
			pf.set(clause, []string{"v0", "v1"})
			msg := &pb.BroadcastSelector{Clauses: []*pb.BroadcastSelectorClause{clause}}

			sel := SelectorFromProto(msg)
			(*fields[i].get(&sel.Clauses[0]))[0] = "clobbered"

			if got := pf.get(msg.GetClauses()[0]); !reflect.DeepEqual(got, want) {
				t.Fatalf("SelectorFromProto aliased %q: %#v, want %#v", pf.key, got, want)
			}
		})
	}
}

// TestSelectorProtoEmptyCases pins the degenerate inputs: a nil or empty
// message decodes to a selector that Validate rejects and Matches refuses, so
// a missing selector on the wire can never become "everyone".
func TestSelectorProtoEmptyCases(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		msg  *pb.BroadcastSelector
	}{
		{"nil message", nil},
		{"empty message", &pb.BroadcastSelector{}},
		{"message with no clauses", &pb.BroadcastSelector{Clauses: []*pb.BroadcastSelectorClause{}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sel := SelectorFromProto(tt.msg)
			if len(sel.Clauses) != 0 {
				t.Fatalf("SelectorFromProto(%s) = %#v, want no clauses", tt.name, sel)
			}
			if err := sel.Validate(); err == nil {
				t.Fatalf("SelectorFromProto(%s).Validate() = nil, want error", tt.name)
			}
			if sel.Matches(fullTarget) {
				t.Fatalf("SelectorFromProto(%s).Matches() = true, want false", tt.name)
			}
		})
	}

	// A nil clause entry on the wire must decode to an all-empty clause, not
	// panic — and an all-empty clause matches nothing.
	sel := SelectorFromProto(&pb.BroadcastSelector{Clauses: []*pb.BroadcastSelectorClause{nil}})
	if len(sel.Clauses) != 1 {
		t.Fatalf("SelectorFromProto(nil clause) = %#v, want one clause", sel)
	}
	if sel.Matches(fullTarget) {
		t.Fatal("a nil clause decoded to something that matches; want no match")
	}
	if err := sel.Validate(); err == nil {
		t.Fatal("SelectorFromProto(nil clause).Validate() = nil, want error")
	}

	// The zero Selector encodes to a message with no clauses, which decodes
	// back to the zero Selector.
	if msg := SelectorToProto(Selector{}); len(msg.GetClauses()) != 0 {
		t.Fatalf("SelectorToProto(zero) = %#v, want no clauses", msg)
	}
}
