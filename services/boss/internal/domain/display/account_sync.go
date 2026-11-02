package display

import pb "github.com/recurser/bossalib/gen/bossanova/v1"

// Sync states of a local agent account, shared by the TUI and
// `boss account ls --json` so both surfaces classify an account the same way.
const (
	// AgentAccountSyncLocalOnly means a change to the account stays on this daemon:
	// it has no synced id, its registry row has one member, or the registry
	// does not contain its id.
	AgentAccountSyncLocalOnly = "local_only"
	// AgentAccountSyncSynced means the account's registry row has at least two
	// distinct in-scope member daemons, so a change reaches the others.
	AgentAccountSyncSynced = "synced"
	// AgentAccountSyncUnknown means the account has a synced id but the hosted
	// registry could not be read. It is never shown as synced.
	AgentAccountSyncUnknown = "unknown"
)

// SyncPeer is another registry row a pending link suggestion pairs with this
// account's row.
type SyncPeer struct {
	ID          string
	Label       string
	MemberCount int
}

// AgentAccountSync is an account's resolved sync membership. MemberCount is the
// number of distinct daemons a change to the account's registry row reaches
// (see SyncedDaemonCount), and 0 when the row is not known.
type AgentAccountSync struct {
	State          string
	MemberCount    int
	SuggestedPeers []SyncPeer
}

// ResolveAgentAccountSync joins a local account to the hosted synced registry by its
// synced_agent_account_id. known=false means the registry could not be read,
// so an account with a synced id resolves to AgentAccountSyncUnknown. A suggestion
// is reported only when both of its rows are in the registry, as the web
// Settings → Accounts page does.
func ResolveAgentAccountSync(a *pb.AgentAccount, reg *pb.ListSyncedAgentAccountsResponse, known bool) AgentAccountSync {
	id := a.GetSyncedAgentAccountId()
	if id == "" {
		return AgentAccountSync{State: AgentAccountSyncLocalOnly}
	}
	if !known {
		return AgentAccountSync{State: AgentAccountSyncUnknown}
	}
	rows := make(map[string]*pb.SyncedAgentAccount, len(reg.GetAccounts()))
	for _, row := range reg.GetAccounts() {
		rows[row.GetId()] = row
	}
	own, ok := rows[id]
	if !ok {
		return AgentAccountSync{State: AgentAccountSyncLocalOnly}
	}
	out := AgentAccountSync{State: AgentAccountSyncLocalOnly, MemberCount: SyncedDaemonCount(own)}
	if out.MemberCount >= 2 && !ownMemberOutOfScope(own, a.GetId()) {
		out.State = AgentAccountSyncSynced
	}
	out.SuggestedPeers = suggestedPeers(id, reg.GetSuggestions(), rows)
	return out
}

// suggestedPeers returns the other row of every suggestion naming id, once per
// row, in suggestion order.
func suggestedPeers(id string, suggestions []*pb.SyncedAgentAccountSuggestion, rows map[string]*pb.SyncedAgentAccount) []SyncPeer {
	var peers []SyncPeer
	seen := map[string]bool{}
	for _, s := range suggestions {
		other := ""
		switch id {
		case s.GetFirstId():
			other = s.GetSecondId()
		case s.GetSecondId():
			other = s.GetFirstId()
		}
		row, ok := rows[other]
		if other == "" || other == id || !ok || seen[other] {
			continue
		}
		seen[other] = true
		peers = append(peers, SyncPeer{ID: other, Label: row.GetLabel(), MemberCount: SyncedDaemonCount(row)})
	}
	return peers
}

// Registry sync scopes, as bosso stores them on a synced row.
const (
	syncScopeAllDaemons      = "all_daemons"
	syncScopeSelectedDaemons = "selected_daemons"
)

// SyncedDaemonCount is the number of distinct daemons a change to row reaches.
// It mirrors bosso's agentaccountsync.InScope: every member of an all_daemons
// row, and only the selected members of a selected_daemons row. Members are
// keyed by daemon and local account, so a daemon with two local accounts in one
// row is counted once.
func SyncedDaemonCount(row *pb.SyncedAgentAccount) int {
	daemons := map[string]bool{}
	for _, m := range row.GetMembers() {
		if memberInScope(row, m) {
			daemons[m.GetDaemonId()] = true
		}
	}
	return len(daemons)
}

// memberInScope mirrors bosso's agentaccountsync.InScope.
func memberInScope(row *pb.SyncedAgentAccount, m *pb.SyncedAgentAccountMember) bool {
	switch row.GetSyncScope() {
	case syncScopeAllDaemons:
		return true
	case syncScopeSelectedDaemons:
		return m.GetIsSelected()
	default:
		return false
	}
}

// ownMemberOutOfScope reports whether row lists the local account localID as a
// member outside the row's scope. bosso neither accepts that member's edits nor
// sends it the row's, so the account is local only however many other daemons
// the row reaches.
func ownMemberOutOfScope(row *pb.SyncedAgentAccount, localID string) bool {
	if localID == "" {
		return false
	}
	for _, m := range row.GetMembers() {
		if m.GetLocalAgentAccountId() == localID && !memberInScope(row, m) {
			return true
		}
	}
	return false
}
