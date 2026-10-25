package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
)

func identityCredential(user string) []byte {
	claims := fmt.Sprintf(`{"https://api.openai.com/auth":{"chatgpt_account_id":"workspace","chatgpt_user_id":%q}}`, user)
	token := "header." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".signature"
	return []byte(fmt.Sprintf(`{"access":"access","refresh":"refresh","id_token":%q}`, token))
}

type failingIdentityStore struct{ db.AgentAccountStore }

func (f failingIdentityStore) SetIdentity(context.Context, string, domain.AgentAccountIdentity) error {
	return errors.New("store unavailable")
}

func TestAccountIdentityAddRefreshList(t *testing.T) {
	ctx := context.Background()
	srv, store := newAccountServer(t, &fakeCredStore{blobs: make(map[string][]byte)}, nil)
	added, err := srv.AddAgentAccount(ctx, connect.NewRequest(&pb.AddAgentAccountRequest{Provider: "codex", Label: "identity", Credential: identityCredential("first")}))
	if err != nil {
		t.Fatal(err)
	}
	first := added.Msg.Account.GetIdentity()
	if first.GetKind() != "codex_user" || len(first.GetDigest()) != 64 || !first.GetIsConfident() {
		t.Fatalf("add identity: %+v", first)
	}
	refreshed, err := srv.RefreshAgentAccount(ctx, connect.NewRequest(&pb.RefreshAgentAccountRequest{Id: added.Msg.Account.Id, Credential: identityCredential("second")}))
	if err != nil {
		t.Fatal(err)
	}
	second := refreshed.Msg.Account.GetIdentity()
	if second.GetKind() != "codex_user" || first.GetDigest() == second.GetDigest() {
		t.Fatalf("refresh identity did not change: %+v", second)
	}
	listed, err := srv.ListAgentAccounts(ctx, connect.NewRequest(&pb.ListAgentAccountsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if listed.Msg.Accounts[0].GetIdentity().GetDigest() != second.GetDigest() {
		t.Fatal("list lost identity")
	}
	stored, err := store.Get(ctx, added.Msg.Account.Id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Identity.Digest != second.GetDigest() {
		t.Fatal("identity not persisted")
	}
}

func TestAccountIdentityWriteFailureIsNonFatal(t *testing.T) {
	ctx := context.Background()
	srv, store := newAccountServer(t, &fakeCredStore{blobs: make(map[string][]byte)}, nil)
	srv.accounts = failingIdentityStore{store}
	added, err := srv.AddAgentAccount(ctx, connect.NewRequest(&pb.AddAgentAccountRequest{Provider: "codex", Label: "identity", Credential: identityCredential("first")}))
	if err != nil {
		t.Fatal(err)
	}
	if added.Msg.Account.GetIdentity().GetKind() != "" {
		t.Fatal("reported unpersisted identity")
	}
	_, err = srv.RefreshAgentAccount(ctx, connect.NewRequest(&pb.RefreshAgentAccountRequest{Id: added.Msg.Account.Id, Credential: identityCredential("second")}))
	if err != nil {
		t.Fatal(err)
	}
}

// accountStoreWithoutIdentity intentionally exposes only the legacy store API.
type accountStoreWithoutIdentity struct{ db.AgentAccountStore }

func TestAccountIdentityUnsupportedStoreIsNonFatal(t *testing.T) {
	srv, store := newAccountServer(t, &fakeCredStore{blobs: make(map[string][]byte)}, nil)
	srv.accounts = accountStoreWithoutIdentity{store}
	added, err := srv.AddAgentAccount(context.Background(), connect.NewRequest(&pb.AddAgentAccountRequest{Provider: "codex", Label: "identity", Credential: identityCredential("first")}))
	if err != nil {
		t.Fatal(err)
	}
	if added.Msg.Account.GetIdentity().GetKind() != "" {
		t.Fatal("reported identity without persistence capability")
	}
}
