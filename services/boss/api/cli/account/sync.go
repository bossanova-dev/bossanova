package account

import (
	"context"
	"errors"
	"time"

	"github.com/recurser/boss/internal/domain/display"
	"github.com/spf13/cobra"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// SyncedRegistryClient reads the hosted synced-account registry with the
// user's own login. `boss account ls --json` joins it to the daemon's accounts
// to report sync membership, which the daemon alone cannot answer.
type SyncedRegistryClient interface {
	ListSyncedAgentAccounts(context.Context) (*pb.ListSyncedAgentAccountsResponse, error)
}

// SyncedRegistryFactory builds the registry client for a command. It fails
// before any request is sent when the user is signed out.
type SyncedRegistryFactory func(*cobra.Command) (SyncedRegistryClient, error)

// ErrNotLoggedIn is the registry factory's failure for a caller with no usable
// cloud login. `boss account ls --json` reports sync_state unknown instead of
// failing.
var ErrNotLoggedIn = errors.New("account sync membership needs a Bossanova Cloud login: run 'boss login' first")

// syncedRegistryDeadline bounds the best-effort registry read, so an
// unreachable registry delays `ls --json` by at most this long once the login
// is usable. Refreshing an expired login is not bounded by it.
const syncedRegistryDeadline = 3 * time.Second

// loadSyncedRegistry reads the registry for cmd. known=false means no factory,
// a signed-out user or a failed read; the caller then reports unknown.
func loadSyncedRegistry(cmd *cobra.Command, factory SyncedRegistryFactory) (*pb.ListSyncedAgentAccountsResponse, bool) {
	if factory == nil {
		return nil, false
	}
	// The factory runs outside the deadline: it may refresh an expired login, and
	// cancelling that after the provider rotates the single-use refresh token
	// would leave a consumed token in the keychain and force `boss login`.
	client, err := factory(cmd)
	if err != nil || client == nil {
		return nil, false
	}
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, syncedRegistryDeadline)
	defer cancel()
	resp, err := client.ListSyncedAgentAccounts(ctx)
	if err != nil {
		return nil, false
	}
	return resp, true
}

// applyAccountSync fills out's sync fields from a's resolved membership.
func applyAccountSync(out *accountJSON, a *pb.AgentAccount, reg *pb.ListSyncedAgentAccountsResponse, known bool) {
	s := display.ResolveAgentAccountSync(a, reg, known)
	out.SyncedAgentAccountID = a.GetSyncedAgentAccountId()
	out.SyncState = s.State
	out.SyncedMemberCount = s.MemberCount
	out.LinkSuggestionIDs = make([]string, len(s.SuggestedPeers))
	for i, peer := range s.SuggestedPeers {
		out.LinkSuggestionIDs[i] = peer.ID
	}
}
