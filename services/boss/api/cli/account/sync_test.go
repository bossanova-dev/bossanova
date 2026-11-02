package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/spf13/cobra"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// lsSyncedRegistryStub serves a canned registry read and counts calls.
type lsSyncedRegistryStub struct {
	resp  *pb.ListSyncedAgentAccountsResponse
	err   error
	calls int
}

func (s *lsSyncedRegistryStub) ListSyncedAgentAccounts(context.Context) (*pb.ListSyncedAgentAccountsResponse, error) {
	s.calls++
	return s.resp, s.err
}

func lsSyncAccounts() *accountLSStub {
	return &accountLSStub{accounts: []*pb.AgentAccount{
		{Id: "shared", Provider: "claude", Label: "shared", SyncedAgentAccountId: "row-shared"},
		{Id: "solo", Provider: "claude", Label: "solo", SyncedAgentAccountId: "row-solo"},
		{Id: "plain", Provider: "codex", Label: "plain"},
	}}
}

func lsSyncRegistry() *pb.ListSyncedAgentAccountsResponse {
	member := func(daemon string) *pb.SyncedAgentAccountMember {
		return &pb.SyncedAgentAccountMember{DaemonId: daemon}
	}
	return &pb.ListSyncedAgentAccountsResponse{
		Accounts: []*pb.SyncedAgentAccount{
			{Id: "row-shared", SyncScope: "all_daemons", Members: []*pb.SyncedAgentAccountMember{member("d1"), member("d2")}},
			{Id: "row-solo", SyncScope: "all_daemons", Members: []*pb.SyncedAgentAccountMember{member("d1")}},
			{Id: "row-peer", SyncScope: "all_daemons", Members: []*pb.SyncedAgentAccountMember{member("d2")}},
		},
		Suggestions: []*pb.SyncedAgentAccountSuggestion{{FirstId: "row-solo", SecondId: "row-peer", Reason: "matching_label"}},
	}
}

type lsSyncRow struct {
	ID                string    `json:"id"`
	SyncedID          *string   `json:"synced_agent_account_id"`
	SyncState         *string   `json:"sync_state"`
	MemberCount       *int      `json:"synced_member_count"`
	LinkSuggestionIDs *[]string `json:"link_suggestion_ids"`
}

func runSyncLS(t *testing.T, args []string, registry func(*cobra.Command) (SyncedRegistryClient, error)) []lsSyncRow {
	t.Helper()
	ls := findLSSubcommand(t)
	var out bytes.Buffer
	ls.SetOut(&out)
	if err := ls.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	if err := accountLS(ls, lsSyncAccounts(), registry); err != nil {
		t.Fatalf("accountLS: %v", err)
	}
	var rows []lsSyncRow
	if slices.Contains(args, "--json") {
		if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
			t.Fatalf("decode: %v\n%s", err, out.String())
		}
		for _, row := range rows {
			if row.SyncedID == nil || row.SyncState == nil || row.MemberCount == nil || row.LinkSuggestionIDs == nil || *row.LinkSuggestionIDs == nil {
				t.Fatalf("row %s lacks a sync field or emits null suggestions:\n%s", row.ID, out.String())
			}
		}
	}
	return rows
}

func registryFactory(c SyncedRegistryClient, err error) func(*cobra.Command) (SyncedRegistryClient, error) {
	return func(*cobra.Command) (SyncedRegistryClient, error) { return c, err }
}

func TestAccountLSJSONReportsSyncMembership_Accounts_AC225602(t *testing.T) {
	reg := &lsSyncedRegistryStub{resp: lsSyncRegistry()}
	rows := runSyncLS(t, []string{"--json"}, registryFactory(reg, nil))
	if reg.calls != 1 {
		t.Fatalf("registry reads = %d, want 1", reg.calls)
	}
	want := map[string]lsSyncRow{}
	for _, row := range rows {
		want[row.ID] = row
	}
	check := func(id, syncedID, state string, members int, links []string) {
		t.Helper()
		row := want[id]
		if *row.SyncedID != syncedID || *row.SyncState != state || *row.MemberCount != members || !slices.Equal(*row.LinkSuggestionIDs, links) {
			t.Errorf("%s = {%q %q %d %v}, want {%q %q %d %v}", id, *row.SyncedID, *row.SyncState, *row.MemberCount, *row.LinkSuggestionIDs, syncedID, state, members, links)
		}
	}
	check("shared", "row-shared", "synced", 2, []string{})
	check("solo", "row-solo", "local_only", 1, []string{"row-peer"})
	check("plain", "", "local_only", 0, []string{})
}

func TestAccountLSJSONSucceedsWithoutRegistry_Accounts_AC225602(t *testing.T) {
	cases := map[string]func(*cobra.Command) (SyncedRegistryClient, error){
		"no factory":  nil,
		"signed out":  registryFactory(nil, ErrNotLoggedIn),
		"read failed": registryFactory(&lsSyncedRegistryStub{err: errors.New("bosso down")}, nil),
		"nil client":  registryFactory(nil, nil),
	}
	for name, factory := range cases {
		t.Run(name, func(t *testing.T) {
			rows := runSyncLS(t, []string{"--json"}, factory)
			for _, row := range rows {
				wantState := "unknown"
				if row.ID == "plain" {
					wantState = "local_only"
				}
				if *row.SyncState != wantState || *row.MemberCount != 0 || len(*row.LinkSuggestionIDs) != 0 {
					t.Errorf("%s = {%q %d %v}, want {%q 0 []}", row.ID, *row.SyncState, *row.MemberCount, *row.LinkSuggestionIDs, wantState)
				}
			}
		})
	}
}

func TestAccountLSTableNeverReadsRegistry_Accounts_AC225602(t *testing.T) {
	reg := &lsSyncedRegistryStub{resp: lsSyncRegistry()}
	runSyncLS(t, nil, registryFactory(reg, nil))
	if reg.calls != 0 {
		t.Fatalf("table output read the registry %d times", reg.calls)
	}
}
