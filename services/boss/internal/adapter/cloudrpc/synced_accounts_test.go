package cloudrpc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/domain/rpcerr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

// fakeSyncedAccountsOrchestrator serves the synced registry read. Unimplemented
// RPCs inherit CodeUnimplemented from the embedded base.
type fakeSyncedAccountsOrchestrator struct {
	bossanovav1connect.UnimplementedOrchestratorServiceHandler

	resp *pb.ListSyncedAgentAccountsResponse
	err  error
}

func (f *fakeSyncedAccountsOrchestrator) ListSyncedAgentAccounts(context.Context, *connect.Request[pb.ListSyncedAgentAccountsRequest]) (*connect.Response[pb.ListSyncedAgentAccountsResponse], error) {
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(f.resp), nil
}

func newSyncedAccountsRemote(t *testing.T, fake *fakeSyncedAccountsOrchestrator) *RemoteClient {
	t.Helper()
	path, handler := bossanovav1connect.NewOrchestratorServiceHandler(fake)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewRemote(srv.URL, "tok")
}

func TestRemoteClient_ListSyncedAgentAccounts(t *testing.T) {
	t.Parallel()
	want := &pb.ListSyncedAgentAccountsResponse{
		Accounts: []*pb.SyncedAgentAccount{{
			Id:    "row-1",
			Label: "work",
			Members: []*pb.SyncedAgentAccountMember{
				{DaemonId: "d1", LocalAgentAccountId: "a1"},
				{DaemonId: "d2", LocalAgentAccountId: "a2"},
			},
		}},
		Suggestions: []*pb.SyncedAgentAccountSuggestion{{FirstId: "row-1", SecondId: "row-2", Reason: "matching_label"}},
	}
	c := newSyncedAccountsRemote(t, &fakeSyncedAccountsOrchestrator{resp: want})

	got, err := c.ListSyncedAgentAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListSyncedAgentAccounts: %v", err)
	}
	if !proto.Equal(got, want) {
		t.Fatalf("response = %v, want %v", got, want)
	}
}

func TestRemoteClient_ListSyncedAgentAccountsClassifiesFailure(t *testing.T) {
	t.Parallel()
	c := newSyncedAccountsRemote(t, &fakeSyncedAccountsOrchestrator{
		err: connect.NewError(connect.CodeUnauthenticated, errors.New("login expired")),
	})

	_, err := c.ListSyncedAgentAccounts(context.Background())
	if got := rpcerr.KindOf(err); got != rpcerr.Unauthenticated {
		t.Fatalf("kind = %q (%v), want unauthenticated", got, err)
	}
}
