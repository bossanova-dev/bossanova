package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/session"
)

func TestListAgentAccountsCredentialSyncBlockReason_Accounts_AC224701(t *testing.T) {
	ctx := context.Background()
	srv, store := newAccountServer(t, newFakeCredStore(), nil)
	synced := mustAddCodex(t, srv, "synced", []byte(`{"access":"a","refresh":"r"}`))
	local := mustAddCodex(t, srv, "local", []byte(`{"access":"b","refresh":"s"}`))
	claude := mustAddClaude(t, srv, "claude", []byte("token"))
	syncer := store.(interface {
		SetSync(context.Context, string, string, int64) error
	})
	for _, id := range []string{synced.Id, claude.Id} {
		if err := syncer.SetSync(ctx, id, "cloud-"+id, 1); err != nil {
			t.Fatal(err)
		}
	}
	f := newReadinessFixture(t)
	f.setDevices(&pb.AgentCredentialDevice{DaemonId: "self", Status: "enrolled", ApprovedByDaemonId: "boot", DevicePublicKey: f.ownKey})
	srv.credentialDevices = f.service
	f.service.RefreshReadiness(ctx)
	check := func(want string) {
		t.Helper()
		response, err := srv.ListAgentAccounts(ctx, connect.NewRequest(&pb.ListAgentAccountsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Msg.Accounts) != 3 {
			t.Fatalf("accounts = %d, want 3", len(response.Msg.Accounts))
		}
		for _, a := range response.Msg.Accounts {
			w := ""
			if a.Id == synced.Id {
				w = want
			}
			if a.GetCredentialSyncBlockReason() != w {
				t.Errorf("account %s (%s): block = %q, want %q", a.Id, a.Provider, a.GetCredentialSyncBlockReason(), w)
			}
		}
	}
	check("bootstrap_unpinned")
	if err := f.pins.Pin("boot", f.bootPub); err != nil {
		t.Fatal(err)
	}
	f.service.RefreshReadiness(ctx)
	check("")
	if id, err := srv.resolveSessionAccount(ctx, &local.Id, "codex", ""); err != nil || id != local.Id {
		t.Fatalf("local binding = %q, %v", id, err)
	}
}

func TestExplicitBindingRefusesCredentialSyncBeforeSwitch_Accounts_AC224701(t *testing.T) {
	ctx := context.Background()
	srv, store := newAccountServer(t, newFakeCredStore(), nil)
	acct := mustAddCodex(t, srv, "blocked", []byte(`{"access":"a","refresh":"r"}`))
	if err := store.(interface {
		SetSync(context.Context, string, string, int64) error
	}).SetSync(ctx, acct.Id, "cloud", 1); err != nil {
		t.Fatal(err)
	}
	f := newReadinessFixture(t)
	f.setDevices(&pb.AgentCredentialDevice{DaemonId: "self", Status: "enrolled", ApprovedByDaemonId: "boot", DevicePublicKey: f.ownKey})
	f.service.RefreshReadiness(ctx)
	srv.credentialDevices = f.service
	live := "live"
	srv.agentChats = &switchChatStoreFake{chats: []*domain.AgentChat{{AgentSessionID: "chat", AgentName: "codex", TmuxSessionName: &live}}}
	switches := 0
	srv.switchAccountFn = func(context.Context, session.SwitchAgentAccountParams) (session.SwitchAgentAccountResult, error) {
		switches++
		return session.SwitchAgentAccountResult{}, nil
	}
	for _, requested := range []string{acct.Id, acct.Label} {
		if _, err := srv.resolveSessionAccount(ctx, &requested, "codex", ""); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "bootstrap_unpinned") {
			t.Fatalf("create binding %q: %v", requested, err)
		}
		_, err := srv.SwitchSessionAgentAccount(ctx, connect.NewRequest(&pb.SwitchSessionAgentAccountRequest{SessionId: "session", AccountId: requested}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "bootstrap_unpinned") {
			t.Fatalf("switch binding %q: %v", requested, err)
		}
	}
	if switches != 0 {
		t.Fatalf("blocked binding mutated session %d times", switches)
	}
}
