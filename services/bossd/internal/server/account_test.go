package server

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/accountcred"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"
)

// fakeCredStore is an in-memory AccountCredentialStore for handler tests. It
// mirrors accountcred.Store's not-found semantics and can be forced to fail
// Load to simulate a locked/unreadable keyring.
type fakeCredStore struct {
	blobs     map[string][]byte
	loadErr   error // when non-nil, Load returns this (simulates locked keyring)
	deleteErr error
}

func newFakeCredStore() *fakeCredStore { return &fakeCredStore{blobs: map[string][]byte{}} }

func (f *fakeCredStore) Save(id string, blob []byte) error {
	f.blobs[id] = append([]byte(nil), blob...)
	return nil
}

func (f *fakeCredStore) Load(id string) ([]byte, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	b, ok := f.blobs[id]
	if !ok {
		return nil, accountcred.ErrCredentialNotFound
	}
	return append([]byte(nil), b...), nil
}

func (f *fakeCredStore) Delete(id string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.blobs[id]; !ok {
		return accountcred.ErrCredentialNotFound
	}
	delete(f.blobs, id)
	return nil
}

// fakeSmoke records whether Smoke ran and with what provider, returning a
// configurable error.
type fakeSmoke struct {
	called   bool
	provider string
	err      error
}

func (f *fakeSmoke) Smoke(_ context.Context, provider string, _ []byte) error {
	f.called = true
	f.provider = provider
	return f.err
}

func newAccountServer(t *testing.T, creds AccountCredentialStore, smoke AccountSmokeRunner) (*Server, db.AccountStore) {
	t.Helper()
	sqlDB := setupServerTestDB(t)
	accts := db.NewAccountStore(sqlDB)
	return &Server{
		accounts:     accts,
		accountCreds: creds,
		accountSmoke: smoke,
		logger:       zerolog.Nop(),
	}, accts
}

func mustAddClaude(t *testing.T, srv *Server, label string, cred []byte) *pb.Account {
	t.Helper()
	resp, err := srv.AddAccount(context.Background(), connect.NewRequest(&pb.AddAccountRequest{
		Provider:   "claude",
		Label:      label,
		Email:      "dev@example.com",
		Priority:   3,
		Credential: cred,
	}))
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	return resp.Msg.Account
}

func TestAccountCRUDHappyPath(t *testing.T) {
	creds := newFakeCredStore()
	srv, _ := newAccountServer(t, creds, nil)
	ctx := context.Background()

	acct := mustAddClaude(t, srv, "work", []byte("setup-token-xyz"))
	if acct.Id == "" || acct.Provider != "claude" || acct.Label != "work" {
		t.Fatalf("unexpected account: %+v", acct)
	}
	if _, ok := creds.blobs[acct.Id]; !ok {
		t.Fatalf("credential not stored for %s", acct.Id)
	}

	listed, err := srv.ListAccounts(ctx, connect.NewRequest(&pb.ListAccountsRequest{}))
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(listed.Msg.Accounts) != 1 {
		t.Fatalf("list len = %d, want 1", len(listed.Msg.Accounts))
	}

	// Update label, priority, status.
	newLabel := "renamed"
	newPriority := int32(9)
	newStatus := "disabled"
	upd, err := srv.UpdateAccount(ctx, connect.NewRequest(&pb.UpdateAccountRequest{
		Id:            acct.Id,
		Label:         &newLabel,
		Priority:      &newPriority,
		Status:        &newStatus,
		AllowedModels: []string{"claude-opus-4-8"},
	}))
	if err != nil {
		t.Fatalf("UpdateAccount: %v", err)
	}
	if upd.Msg.Account.Label != "renamed" || upd.Msg.Account.Priority != 9 || upd.Msg.Account.Status != "disabled" {
		t.Fatalf("update not applied: %+v", upd.Msg.Account)
	}
	if len(upd.Msg.Account.AllowedModels) != 1 || upd.Msg.Account.AllowedModels[0] != "claude-opus-4-8" {
		t.Fatalf("allowed_models = %v", upd.Msg.Account.AllowedModels)
	}

	// Remove purges both the row and the keyring credential.
	if _, err := srv.RemoveAccount(ctx, connect.NewRequest(&pb.RemoveAccountRequest{Id: acct.Id})); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	if _, ok := creds.blobs[acct.Id]; ok {
		t.Errorf("credential still present after remove")
	}
	listed2, _ := srv.ListAccounts(ctx, connect.NewRequest(&pb.ListAccountsRequest{}))
	if len(listed2.Msg.Accounts) != 0 {
		t.Errorf("list after remove = %d, want 0", len(listed2.Msg.Accounts))
	}
}

func TestAddAccountValidation(t *testing.T) {
	srv, _ := newAccountServer(t, newFakeCredStore(), nil)
	ctx := context.Background()

	cases := []struct {
		name string
		req  *pb.AddAccountRequest
	}{
		{"empty provider", &pb.AddAccountRequest{Label: "l", Credential: []byte("c")}},
		{"unknown provider", &pb.AddAccountRequest{Provider: "gemini", Label: "l", Credential: []byte("c")}},
		{"empty label", &pb.AddAccountRequest{Provider: "claude", Credential: []byte("c")}},
		{"empty credential", &pb.AddAccountRequest{Provider: "claude", Label: "l"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := srv.AddAccount(ctx, connect.NewRequest(tc.req))
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("code = %v, want InvalidArgument (err=%v)", connect.CodeOf(err), err)
			}
		})
	}
}

// TestAddAccountDuplicateLabel proves a re-add of an existing provider/label is
// surfaced as a client-facing AlreadyExists conflict, not an internal error, and
// that no orphaned credential is left behind for the rejected create.
func TestAddAccountDuplicateLabel(t *testing.T) {
	creds := newFakeCredStore()
	srv, _ := newAccountServer(t, creds, nil)

	first := mustAddClaude(t, srv, "work", []byte("token-a"))

	_, err := srv.AddAccount(context.Background(), connect.NewRequest(&pb.AddAccountRequest{
		Provider:   "claude",
		Label:      "work",
		Credential: []byte("token-b"),
	}))
	if got := connect.CodeOf(err); got != connect.CodeAlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists (err=%v)", got, err)
	}
	// The rejected create must not have stored a second credential, and the
	// original account's credential must be untouched.
	if len(creds.blobs) != 1 {
		t.Errorf("cred store has %d blobs, want 1 (duplicate must not persist a credential)", len(creds.blobs))
	}
	if got := string(creds.blobs[first.Id]); got != "token-a" {
		t.Errorf("original credential = %q, want %q", got, "token-a")
	}
}

// TestAddAccountResponseHasNoCredential proves the credential bytes never appear
// in the AddAccount response wire form.
func TestAddAccountResponseHasNoCredential(t *testing.T) {
	srv, _ := newAccountServer(t, newFakeCredStore(), nil)
	secret := []byte("super-secret-token-DO-NOT-LEAK")
	resp, err := srv.AddAccount(context.Background(), connect.NewRequest(&pb.AddAccountRequest{
		Provider:   "claude",
		Label:      "leaky",
		Credential: secret,
	}))
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	raw, err := proto.Marshal(resp.Msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(raw, secret) {
		t.Errorf("AddAccount response wire form leaked the credential")
	}
}

func TestAccountNotFound(t *testing.T) {
	srv, _ := newAccountServer(t, newFakeCredStore(), nil)
	ctx := context.Background()

	label := "x"
	if _, err := srv.UpdateAccount(ctx, connect.NewRequest(&pb.UpdateAccountRequest{Id: "nope", Label: &label})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("update: code = %v, want NotFound", connect.CodeOf(err))
	}
	if _, err := srv.RemoveAccount(ctx, connect.NewRequest(&pb.RemoveAccountRequest{Id: "nope"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("remove: code = %v, want NotFound", connect.CodeOf(err))
	}
	if _, err := srv.TestAccount(ctx, connect.NewRequest(&pb.TestAccountRequest{Id: "nope"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("test: code = %v, want NotFound", connect.CodeOf(err))
	}
}

func TestTestAccountLiveRunnerSuccess(t *testing.T) {
	creds := newFakeCredStore()
	smoke := &fakeSmoke{}
	srv, _ := newAccountServer(t, creds, smoke)
	acct := mustAddClaude(t, srv, "live", []byte("setup-token"))

	resp, err := srv.TestAccount(context.Background(), connect.NewRequest(&pb.TestAccountRequest{Id: acct.Id}))
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if !resp.Msg.LiveSmokeRan {
		t.Errorf("live_smoke_ran = false, want true")
	}
	if !smoke.called || smoke.provider != "claude" {
		t.Errorf("smoke called=%v provider=%q, want true/claude", smoke.called, smoke.provider)
	}
	if resp.Msg.Account.LastTestOkAt == nil {
		t.Errorf("last_test_ok_at not set on success")
	}
	if resp.Msg.Account.LastTestError != "" {
		t.Errorf("last_test_error = %q, want empty on success", resp.Msg.Account.LastTestError)
	}
}

func TestTestAccountLiveRunnerFailure(t *testing.T) {
	creds := newFakeCredStore()
	smoke := &fakeSmoke{err: errors.New("provider rejected token")}
	srv, _ := newAccountServer(t, creds, smoke)
	acct := mustAddClaude(t, srv, "livefail", []byte("setup-token"))

	resp, err := srv.TestAccount(context.Background(), connect.NewRequest(&pb.TestAccountRequest{Id: acct.Id}))
	if err != nil {
		t.Fatalf("TestAccount should not error on smoke failure: %v", err)
	}
	if !resp.Msg.LiveSmokeRan {
		t.Errorf("live_smoke_ran = false, want true (runner executed)")
	}
	if resp.Msg.Account.LastTestOkAt != nil {
		t.Errorf("last_test_ok_at set despite smoke failure")
	}
	if resp.Msg.Account.LastTestError == "" {
		t.Errorf("last_test_error empty, want the smoke failure detail")
	}
}

func TestTestAccountNilRunnerDegrades(t *testing.T) {
	creds := newFakeCredStore()
	srv, _ := newAccountServer(t, creds, nil) // no smoke runner
	acct := mustAddClaude(t, srv, "nilrunner", []byte("setup-token"))

	resp, err := srv.TestAccount(context.Background(), connect.NewRequest(&pb.TestAccountRequest{Id: acct.Id}))
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if resp.Msg.LiveSmokeRan {
		t.Errorf("live_smoke_ran = true, want false with nil runner")
	}
	if resp.Msg.Detail != liveSmokeUnavailableDetail {
		t.Errorf("detail = %q, want %q", resp.Msg.Detail, liveSmokeUnavailableDetail)
	}
	if resp.Msg.Account.LastTestError != liveSmokeUnavailableDetail {
		t.Errorf("last_test_error = %q, want %q", resp.Msg.Account.LastTestError, liveSmokeUnavailableDetail)
	}
	if resp.Msg.Account.LastTestOkAt != nil {
		t.Errorf("last_test_ok_at set, want nil when live smoke unavailable")
	}
}

func TestTestAccountMalformedBlobRecordsError(t *testing.T) {
	creds := newFakeCredStore()
	smoke := &fakeSmoke{}
	srv, _ := newAccountServer(t, creds, smoke)
	// Register a codex account whose stored blob is not valid JSON.
	resp, err := srv.AddAccount(context.Background(), connect.NewRequest(&pb.AddAccountRequest{
		Provider:   "codex",
		Label:      "bad",
		Credential: []byte("this is not json"),
	}))
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	id := resp.Msg.Account.Id

	tr, err := srv.TestAccount(context.Background(), connect.NewRequest(&pb.TestAccountRequest{Id: id}))
	if err != nil {
		t.Fatalf("TestAccount should return OK result for malformed blob, got err: %v", err)
	}
	if tr.Msg.LiveSmokeRan {
		t.Errorf("live_smoke_ran = true, want false for malformed blob")
	}
	if smoke.called {
		t.Errorf("smoke runner ran on a malformed blob")
	}
	if tr.Msg.Account.LastTestError == "" {
		t.Errorf("last_test_error empty, want a validation failure detail")
	}
	if tr.Msg.Account.LastTestOkAt != nil {
		t.Errorf("last_test_ok_at set despite malformed blob")
	}
}

func TestTestAccountLockedKeyringIsInternal(t *testing.T) {
	creds := newFakeCredStore()
	srv, _ := newAccountServer(t, creds, nil)
	acct := mustAddClaude(t, srv, "locked", []byte("tok"))
	creds.loadErr = errors.New("keyring is locked")

	_, err := srv.TestAccount(context.Background(), connect.NewRequest(&pb.TestAccountRequest{Id: acct.Id}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("code = %v, want Internal for locked keyring", connect.CodeOf(err))
	}
}

func TestRemoveAccountToleratesMissingCredential(t *testing.T) {
	creds := newFakeCredStore()
	srv, accts := newAccountServer(t, creds, nil)
	ctx := context.Background()
	// Create a row directly without a stored credential (D9-style).
	acct, err := accts.Create(ctx, db.CreateAccountParams{Provider: "claude", Label: "nocred"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := srv.RemoveAccount(ctx, connect.NewRequest(&pb.RemoveAccountRequest{Id: acct.ID})); err != nil {
		t.Fatalf("RemoveAccount should tolerate missing credential: %v", err)
	}
}
