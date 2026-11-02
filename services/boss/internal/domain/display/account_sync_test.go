package display

import (
	"fmt"
	"slices"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func syncedRow(id, label string, members int) *pb.SyncedAgentAccount {
	row := &pb.SyncedAgentAccount{Id: id, Label: label, SyncScope: "all_daemons"}
	for i := range members {
		row.Members = append(row.Members, &pb.SyncedAgentAccountMember{DaemonId: fmt.Sprintf("daemon-%d", i)})
	}
	return row
}

func TestResolveAgentAccountSync(t *testing.T) {
	t.Parallel()
	registry := &pb.ListSyncedAgentAccountsResponse{
		Accounts: []*pb.SyncedAgentAccount{
			syncedRow("solo", "work", 1),
			syncedRow("pair", "home", 2),
			syncedRow("peer-a", "work", 1),
			syncedRow("peer-b", "work", 3),
		},
		Suggestions: []*pb.SyncedAgentAccountSuggestion{
			{FirstId: "solo", SecondId: "peer-a", Reason: "matching_label"},
			{FirstId: "peer-b", SecondId: "solo", Reason: "matching_label"},
			{FirstId: "pair", SecondId: "gone", Reason: "matching_label"},
		},
	}
	cases := []struct {
		name      string
		syncedID  string
		registry  *pb.ListSyncedAgentAccountsResponse
		known     bool
		state     string
		members   int
		peerIDs   []string
		peerSizes []int
	}{
		{name: "no synced id", syncedID: "", registry: registry, known: true, state: AgentAccountSyncLocalOnly},
		{name: "no synced id with registry unknown", syncedID: "", known: false, state: AgentAccountSyncLocalOnly},
		{name: "one-member row", syncedID: "peer-a", registry: registry, known: true, state: AgentAccountSyncLocalOnly, members: 1, peerIDs: []string{"solo"}, peerSizes: []int{1}},
		{name: "two-member row", syncedID: "pair", registry: registry, known: true, state: AgentAccountSyncSynced, members: 2},
		{name: "id missing from the registry", syncedID: "stale", registry: registry, known: true, state: AgentAccountSyncLocalOnly},
		{name: "registry unknown", syncedID: "pair", registry: registry, known: false, state: AgentAccountSyncUnknown},
		{name: "known but empty response", syncedID: "pair", registry: nil, known: true, state: AgentAccountSyncLocalOnly},
		{name: "suggestions on first_id and second_id", syncedID: "solo", registry: registry, known: true, state: AgentAccountSyncLocalOnly, members: 1, peerIDs: []string{"peer-a", "peer-b"}, peerSizes: []int{1, 3}},
		{name: "suggestion whose peer row is missing", syncedID: "pair", registry: registry, known: true, state: AgentAccountSyncSynced, members: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ResolveAgentAccountSync(&pb.AgentAccount{Id: "local", SyncedAgentAccountId: tc.syncedID}, tc.registry, tc.known)
			if got.State != tc.state || got.MemberCount != tc.members {
				t.Fatalf("got state %q members %d, want %q %d", got.State, got.MemberCount, tc.state, tc.members)
			}
			var ids []string
			var sizes []int
			for _, peer := range got.SuggestedPeers {
				ids = append(ids, peer.ID)
				sizes = append(sizes, peer.MemberCount)
			}
			if !slices.Equal(ids, tc.peerIDs) || !slices.Equal(sizes, tc.peerSizes) {
				t.Fatalf("peers %v sizes %v, want %v %v", ids, sizes, tc.peerIDs, tc.peerSizes)
			}
		})
	}
}

func TestResolveAgentAccountSyncDeduplicatesPeers(t *testing.T) {
	t.Parallel()
	registry := &pb.ListSyncedAgentAccountsResponse{
		Accounts: []*pb.SyncedAgentAccount{syncedRow("a", "x", 1), syncedRow("b", "x", 1)},
		Suggestions: []*pb.SyncedAgentAccountSuggestion{
			{FirstId: "a", SecondId: "b"},
			{FirstId: "b", SecondId: "a"},
		},
	}
	got := ResolveAgentAccountSync(&pb.AgentAccount{SyncedAgentAccountId: "a"}, registry, true)
	if len(got.SuggestedPeers) != 1 || got.SuggestedPeers[0].Label != "x" {
		t.Fatalf("peers = %+v, want one peer b", got.SuggestedPeers)
	}
}

func TestSyncedDaemonCountFollowsScope(t *testing.T) {
	t.Parallel()
	member := func(daemon string, selected bool) *pb.SyncedAgentAccountMember {
		return &pb.SyncedAgentAccountMember{DaemonId: daemon, IsSelected: selected}
	}
	cases := []struct {
		name string
		row  *pb.SyncedAgentAccount
		want int
	}{
		{name: "all daemons counts every daemon", row: &pb.SyncedAgentAccount{SyncScope: "all_daemons", Members: []*pb.SyncedAgentAccountMember{member("a", false), member("b", false)}}, want: 2},
		{name: "selected daemons skips unselected members", row: &pb.SyncedAgentAccount{SyncScope: "selected_daemons", Members: []*pb.SyncedAgentAccountMember{member("a", true), member("b", false)}}, want: 1},
		{name: "same daemon twice counts once", row: &pb.SyncedAgentAccount{SyncScope: "all_daemons", Members: []*pb.SyncedAgentAccountMember{member("a", false), member("a", false)}}, want: 1},
		{name: "unrecognised scope reaches no daemon", row: &pb.SyncedAgentAccount{SyncScope: "", Members: []*pb.SyncedAgentAccountMember{member("a", true), member("b", true)}}, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := SyncedDaemonCount(tc.row); got != tc.want {
				t.Fatalf("SyncedDaemonCount = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestResolveAgentAccountSyncDeselectedPeerIsLocalOnly(t *testing.T) {
	t.Parallel()
	registry := &pb.ListSyncedAgentAccountsResponse{Accounts: []*pb.SyncedAgentAccount{{
		Id:        "row",
		SyncScope: "selected_daemons",
		Members: []*pb.SyncedAgentAccountMember{
			{DaemonId: "here", IsSelected: true},
			{DaemonId: "there", IsSelected: false},
		},
	}}}
	got := ResolveAgentAccountSync(&pb.AgentAccount{SyncedAgentAccountId: "row"}, registry, true)
	if got.State != AgentAccountSyncLocalOnly || got.MemberCount != 1 {
		t.Fatalf("got state %q members %d, want local_only 1", got.State, got.MemberCount)
	}
}

func TestResolveAgentAccountSyncOwnMemberOutOfScopeIsLocalOnly(t *testing.T) {
	t.Parallel()
	registry := &pb.ListSyncedAgentAccountsResponse{Accounts: []*pb.SyncedAgentAccount{{
		Id:        "row",
		SyncScope: "selected_daemons",
		Members: []*pb.SyncedAgentAccountMember{
			{DaemonId: "here", LocalAgentAccountId: "local", IsSelected: false},
			{DaemonId: "a", LocalAgentAccountId: "x", IsSelected: true},
			{DaemonId: "b", LocalAgentAccountId: "y", IsSelected: true},
		},
	}}}
	got := ResolveAgentAccountSync(&pb.AgentAccount{Id: "local", SyncedAgentAccountId: "row"}, registry, true)
	if got.State != AgentAccountSyncLocalOnly {
		t.Fatalf("state = %q, want local_only for a deselected local member", got.State)
	}
	other := ResolveAgentAccountSync(&pb.AgentAccount{Id: "x", SyncedAgentAccountId: "row"}, registry, true)
	if other.State != AgentAccountSyncSynced || other.MemberCount != 2 {
		t.Fatalf("selected member = %q %d, want synced 2", other.State, other.MemberCount)
	}
}
