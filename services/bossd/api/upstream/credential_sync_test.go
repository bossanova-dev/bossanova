package upstream

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossd/internal/credsync"
)

// credentialAccountsClientSpy answers the daemon-level account listing and
// records any call to the user-level registry listing, which a daemon token
// cannot reach.
type credentialAccountsClientSpy struct {
	bossanovav1connect.OrchestratorServiceClient
	accounts      []*pb.CredentialAgentAccount
	err           error
	bearer        string
	calls         int
	userListCalls int
}

func (c *credentialAccountsClientSpy) ListCredentialAgentAccounts(_ context.Context, req *connect.Request[pb.ListCredentialAgentAccountsRequest]) (*connect.Response[pb.ListCredentialAgentAccountsResponse], error) {
	c.calls++
	c.bearer = req.Header().Get("Authorization")
	if c.err != nil {
		return nil, c.err
	}
	return connect.NewResponse(&pb.ListCredentialAgentAccountsResponse{Accounts: c.accounts}), nil
}

func (c *credentialAccountsClientSpy) ListSyncedAgentAccounts(context.Context, *connect.Request[pb.ListSyncedAgentAccountsRequest]) (*connect.Response[pb.ListSyncedAgentAccountsResponse], error) {
	c.userListCalls++
	return nil, connect.NewError(connect.CodePermissionDenied, errors.New("daemon credentials cannot call this procedure"))
}

func TestCredentialRemoteListsAccountsThroughDaemonProcedure_Accounts_AC223701(t *testing.T) {
	client := &credentialAccountsClientSpy{accounts: []*pb.CredentialAgentAccount{
		{SyncedAgentAccountId: "in", Provider: "claude", Label: "work", DesiredStatus: "active", Priority: 3, IsInScope: true},
		{SyncedAgentAccountId: "out", Provider: "codex", Label: "side", DesiredStatus: "paused", Priority: 1},
	}}
	remote := &CredentialRemote{Client: client, Token: func() string { return "daemon-session-token" }}

	got, err := remote.ListAgentAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if client.calls != 1 || client.userListCalls != 0 {
		t.Fatalf("ListCredentialAgentAccounts calls = %d, ListSyncedAgentAccounts calls = %d; want 1 and 0", client.calls, client.userListCalls)
	}
	if client.bearer != "Bearer daemon-session-token" {
		t.Fatalf("Authorization = %q, want the daemon session token", client.bearer)
	}
	want := []credsync.AgentAccountMetadata{
		{SyncedAgentAccountID: "in", Provider: "claude", Label: "work", DesiredStatus: "active", Priority: 3, IsInScope: true},
		{SyncedAgentAccountID: "out", Provider: "codex", Label: "side", DesiredStatus: "paused", Priority: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("accounts = %+v, want %+v", got, want)
	}
}

func TestCredentialRemoteAccountListingMapsErrors(t *testing.T) {
	client := &credentialAccountsClientSpy{err: connect.NewError(connect.CodeNotFound, errors.New("gone"))}
	remote := &CredentialRemote{Client: client, Token: func() string { return "daemon-session-token" }}
	if _, err := remote.ListAgentAccounts(context.Background()); !errors.Is(err, credsync.ErrNotFound) {
		t.Fatalf("err = %v, want credsync.ErrNotFound", err)
	}
}
