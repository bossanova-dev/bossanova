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

// daemonOrganizations resolves tokens to orgs and records whether the RPC's
// account lock was free during the lookup.
type daemonOrganizations struct {
	srv        *Server
	orgs       map[string]string
	lockedSeen bool
	calls      int
}

func (o *daemonOrganizations) OrganizationID(_ context.Context, token string) (string, error) {
	o.calls++
	if o.srv.addAccountMu.TryLock() {
		o.srv.addAccountMu.Unlock()
	} else {
		o.lockedSeen = true
	}
	org, ok := o.orgs[token]
	if !ok {
		return "", errors.New("lookup failed")
	}
	return org, nil
}

func TestAccountIdentityClaudeOrganizationLabel_Accounts_AC225501(t *testing.T) {
	ctx := context.Background()
	srv, store := newAccountServer(t, &fakeCredStore{blobs: make(map[string][]byte)}, nil)
	orgs := &daemonOrganizations{srv: srv, orgs: map[string]string{"token-a": "org-1", "token-b": "org-1"}}
	srv.claudeOrganizations = orgs
	added, err := srv.AddAgentAccount(ctx, connect.NewRequest(&pb.AddAgentAccountRequest{Provider: "claude", Label: "Work", Credential: []byte("token-a")}))
	if err != nil {
		t.Fatal(err)
	}
	identity := added.Msg.Account.GetIdentity()
	if identity.GetKind() != "claude_org_label" || len(identity.GetDigest()) != 64 || !identity.GetIsConfident() {
		t.Fatalf("add identity: %+v", identity)
	}
	stored, err := store.Get(ctx, added.Msg.Account.Id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Identity.Digest != identity.GetDigest() {
		t.Fatal("identity not persisted")
	}
	refreshed, err := srv.RefreshAgentAccount(ctx, connect.NewRequest(&pb.RefreshAgentAccountRequest{Id: added.Msg.Account.Id, Credential: []byte("token-b")}))
	if err != nil {
		t.Fatal(err)
	}
	if got := refreshed.Msg.Account.GetIdentity(); got.GetDigest() != identity.GetDigest() {
		t.Fatalf("same org and label changed identity on refresh: %+v", got)
	}
	if orgs.lockedSeen {
		t.Fatal("organization lookup ran under addAccountMu")
	}
}

func TestAccountIdentityClaudeLookupFailureIsNonFatal_Accounts_AC225502(t *testing.T) {
	ctx := context.Background()
	srv, _ := newAccountServer(t, &fakeCredStore{blobs: make(map[string][]byte)}, nil)
	srv.claudeOrganizations = &daemonOrganizations{srv: srv}
	added, err := srv.AddAgentAccount(ctx, connect.NewRequest(&pb.AddAgentAccountRequest{Provider: "claude", Label: "work", Credential: []byte("token-a")}))
	if err != nil {
		t.Fatal(err)
	}
	if got := added.Msg.Account.GetIdentity(); got.GetKind() != "none" || got.GetIsConfident() {
		t.Fatalf("failed lookup identity: %+v", got)
	}
}

func renameAccount(t *testing.T, srv *Server, id, label string) *pb.AgentAccountIdentity {
	t.Helper()
	updated, err := srv.UpdateAgentAccount(context.Background(), connect.NewRequest(&pb.UpdateAgentAccountRequest{Id: id, Label: &label}))
	if err != nil {
		t.Fatal(err)
	}
	return updated.Msg.Account.GetIdentity()
}

// A rename must not leave the digest on the old label, or a different seat that
// later takes that label would link into this account's row.
func TestAccountIdentityClaudeRenameRederives_Accounts_AC225502(t *testing.T) {
	ctx := context.Background()
	srv, store := newAccountServer(t, &fakeCredStore{blobs: make(map[string][]byte)}, nil)
	orgs := &daemonOrganizations{srv: srv, orgs: map[string]string{"token-a": "org-1", "token-b": "org-1"}}
	srv.claudeOrganizations = orgs
	work, err := srv.AddAgentAccount(ctx, connect.NewRequest(&pb.AddAgentAccountRequest{Provider: "claude", Label: "work", Credential: []byte("token-a")}))
	if err != nil {
		t.Fatal(err)
	}
	oldDigest := work.Msg.Account.GetIdentity().GetDigest()

	renamed := renameAccount(t, srv, work.Msg.Account.Id, "alice")
	if !renamed.GetIsConfident() || renamed.GetDigest() == oldDigest {
		t.Fatalf("rename kept the old-label identity: %+v", renamed)
	}
	alice, err := srv.AddAgentAccount(ctx, connect.NewRequest(&pb.AddAgentAccountRequest{Provider: "claude", Label: "Alice ", Credential: []byte("token-b")}))
	if err != nil {
		t.Fatal(err)
	}
	if alice.Msg.Account.GetIdentity().GetDigest() != renamed.GetDigest() {
		t.Fatal("renamed account does not carry the identity of its new label")
	}
	stored, err := store.Get(ctx, work.Msg.Account.Id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Identity.Digest != renamed.GetDigest() {
		t.Fatal("re-derived identity not persisted")
	}

	calls := orgs.calls
	if got := renameAccount(t, srv, work.Msg.Account.Id, "  ALICE"); got.GetDigest() != renamed.GetDigest() || orgs.calls != calls {
		t.Fatalf("a rename that normalizes alike re-derived the identity: %+v (lookups %d -> %d)", got, calls, orgs.calls)
	}
}

func TestAccountIdentityClaudeRenameLookupFailureClearsDigest_Accounts_AC225502(t *testing.T) {
	ctx := context.Background()
	srv, _ := newAccountServer(t, &fakeCredStore{blobs: make(map[string][]byte)}, nil)
	orgs := &daemonOrganizations{srv: srv, orgs: map[string]string{"token-a": "org-1"}}
	srv.claudeOrganizations = orgs
	work, err := srv.AddAgentAccount(ctx, connect.NewRequest(&pb.AddAgentAccountRequest{Provider: "claude", Label: "work", Credential: []byte("token-a")}))
	if err != nil {
		t.Fatal(err)
	}
	if !work.Msg.Account.GetIdentity().GetIsConfident() {
		t.Fatal("setup: expected a confident identity")
	}
	delete(orgs.orgs, "token-a")
	if got := renameAccount(t, srv, work.Msg.Account.Id, "alice"); got.GetKind() != "none" || got.GetIsConfident() {
		t.Fatalf("failed lookup on rename kept a stale identity: %+v", got)
	}
}
