package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestAccountChangesPublishInventory(t *testing.T) {
	srv, _ := newAccountServer(t, newFakeCredStore(), nil)
	changes := 0
	srv.agentAccountsChanged = func() { changes++ }
	acct := mustAddClaude(t, srv, "inventory", []byte("first-token"))
	if changes != 1 {
		t.Fatalf("add notifications = %d", changes)
	}
	label := "renamed"
	if _, err := srv.UpdateAgentAccount(context.Background(), connect.NewRequest(&pb.UpdateAgentAccountRequest{Id: acct.Id, Label: &label})); err != nil {
		t.Fatal(err)
	}
	if changes != 2 {
		t.Fatalf("update notifications = %d", changes)
	}
	if _, err := srv.TestAgentAccount(context.Background(), connect.NewRequest(&pb.TestAgentAccountRequest{Id: acct.Id})); err != nil {
		t.Fatal(err)
	}
	if changes != 3 {
		t.Fatalf("test notifications = %d", changes)
	}
	if _, err := srv.RefreshAgentAccount(context.Background(), connect.NewRequest(&pb.RefreshAgentAccountRequest{Id: acct.Id, Credential: []byte("second-token")})); err != nil {
		t.Fatal(err)
	}
	if changes != 4 {
		t.Fatalf("refresh notifications = %d", changes)
	}
	if _, err := srv.RemoveAgentAccount(context.Background(), connect.NewRequest(&pb.RemoveAgentAccountRequest{Id: acct.Id})); err != nil {
		t.Fatal(err)
	}
	if changes != 5 {
		t.Fatalf("remove notifications = %d", changes)
	}
	if _, err := srv.UpdateAgentAccount(context.Background(), connect.NewRequest(&pb.UpdateAgentAccountRequest{Id: acct.Id, Label: &label})); err == nil {
		t.Fatal("expected missing account")
	}
	if changes != 5 {
		t.Fatalf("failed update published: %d", changes)
	}
}
