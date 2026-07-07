package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/accountcred"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"
)

// fakeCredStore is an in-memory AccountCredentialStore for handler tests. It
// mirrors accountcred.Store's not-found semantics and can be forced to fail
// Load to simulate a locked/unreadable keyring.
type fakeCredStore struct {
	mu          sync.Mutex
	blobs       map[string][]byte
	loadErr     error // when non-nil, Load returns this (simulates locked keyring)
	deleteErr   error
	preSaveHook func(id string, blob []byte)
	saveHook    func(id string, blob []byte)
}

func newFakeCredStore() *fakeCredStore { return &fakeCredStore{blobs: map[string][]byte{}} }

func (f *fakeCredStore) Save(id string, blob []byte) error {
	if f.preSaveHook != nil {
		f.preSaveHook(id, append([]byte(nil), blob...))
	}
	f.mu.Lock()
	f.blobs[id] = append([]byte(nil), blob...)
	f.mu.Unlock()
	if f.saveHook != nil {
		f.saveHook(id, append([]byte(nil), blob...))
	}
	return nil
}

func (f *fakeCredStore) Load(id string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	f.mu.Lock()
	defer f.mu.Unlock()
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
	called    bool
	accountID string
	provider  string
	blob      []byte
	err       error
}

func (f *fakeSmoke) Smoke(_ context.Context, accountID, provider string, blob []byte) error {
	f.called = true
	f.accountID = accountID
	f.provider = provider
	f.blob = append([]byte(nil), blob...)
	return f.err
}

type fakeUsageProbe struct {
	ids []string
	err error
}

func (f *fakeUsageProbe) RecordUsageProbe(_ context.Context, accountID string) error {
	f.ids = append(f.ids, accountID)
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

func TestListAccountsRefreshUsesUsageProbeFailSoft(t *testing.T) {
	creds := newFakeCredStore()
	srv, store := newAccountServer(t, creds, nil)
	ctx := context.Background()

	first := mustAddClaude(t, srv, "first", []byte("token-a"))
	second := mustAddClaude(t, srv, "second", []byte("token-b"))
	refresh := true
	probe := &fakeUsageProbe{err: errors.New("probe down")}
	srv.usageProbe = probe

	listed, err := srv.ListAccounts(ctx, connect.NewRequest(&pb.ListAccountsRequest{Refresh: &refresh}))
	if err != nil {
		t.Fatalf("ListAccounts refresh: %v", err)
	}
	if len(listed.Msg.Accounts) != 2 {
		t.Fatalf("list len = %d, want 2", len(listed.Msg.Accounts))
	}
	if len(probe.ids) != 2 {
		t.Fatalf("probe calls = %v, want both accounts", probe.ids)
	}
	if probe.ids[0] != first.Id || probe.ids[1] != second.Id {
		t.Fatalf("probe ids = %v, want [%s %s]", probe.ids, first.Id, second.Id)
	}

	probe.ids = nil
	refresh = false
	if _, err := srv.ListAccounts(ctx, connect.NewRequest(&pb.ListAccountsRequest{Refresh: &refresh})); err != nil {
		t.Fatalf("ListAccounts no refresh: %v", err)
	}
	if len(probe.ids) != 0 {
		t.Fatalf("refresh=false probe calls = %v, want none", probe.ids)
	}

	if _, err := store.Get(ctx, first.Id); err != nil {
		t.Fatalf("store still readable after fail-soft probe: %v", err)
	}
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
		{"reserved label", &pb.AddAccountRequest{Provider: "claude", Label: "Unmanaged local credentials", Credential: []byte("c")}},
		{"reserved label case-insensitive", &pb.AddAccountRequest{Provider: "claude", Label: "  unmanaged LOCAL credentials  ", Credential: []byte("c")}},
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

func TestAddAccountDuplicateCredential(t *testing.T) {
	creds := newFakeCredStore()
	srv, _ := newAccountServer(t, creds, nil)

	first := mustAddClaude(t, srv, "work", []byte("same-token"))

	_, err := srv.AddAccount(context.Background(), connect.NewRequest(&pb.AddAccountRequest{
		Provider:   "claude",
		Label:      "personal",
		Credential: []byte("same-token"),
	}))
	if got := connect.CodeOf(err); got != connect.CodeAlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists (err=%v)", got, err)
	}
	if len(creds.blobs) != 1 {
		t.Errorf("cred store has %d blobs, want 1 (duplicate credential must not persist)", len(creds.blobs))
	}
	if got := string(creds.blobs[first.Id]); got != "same-token" {
		t.Errorf("original credential = %q, want %q", got, "same-token")
	}
}

func TestAddAccountDuplicateCodexCredentialNormalizesTokenShapes(t *testing.T) {
	creds := newFakeCredStore()
	srv, _ := newAccountServer(t, creds, nil)

	resp, err := srv.AddAccount(context.Background(), connect.NewRequest(&pb.AddAccountRequest{
		Provider:   "codex",
		Label:      "interactive",
		Credential: []byte(`{"access":"access-token","refresh":"refresh-token","id_token":"id-token"}`),
	}))
	if err != nil {
		t.Fatalf("AddAccount flat codex: %v", err)
	}

	_, err = srv.AddAccount(context.Background(), connect.NewRequest(&pb.AddAccountRequest{
		Provider: "codex",
		Label:    "nested-credential-file",
		Credential: []byte(`{"tokens":{` +
			`"access_token":"access-token",` +
			`"refresh_token":"refresh-token",` +
			`"id_token":"id-token"` +
			`}}`),
	}))
	if got := connect.CodeOf(err); got != connect.CodeAlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists (err=%v)", got, err)
	}
	_, err = srv.AddAccount(context.Background(), connect.NewRequest(&pb.AddAccountRequest{
		Provider:   "codex",
		Label:      "empty-nested-credential-file",
		Credential: []byte(`{"access":"access-token","refresh":"refresh-token","id_token":"id-token","tokens":{}}`),
	}))
	if got := connect.CodeOf(err); got != connect.CodeAlreadyExists {
		t.Fatalf("empty nested tokens code = %v, want AlreadyExists (err=%v)", got, err)
	}
	_, err = srv.AddAccount(context.Background(), connect.NewRequest(&pb.AddAccountRequest{
		Provider: "codex",
		Label:    "partial-nested-credential-file",
		Credential: []byte(`{"access":"access-token","refresh":"refresh-token","id_token":"id-token",` +
			`"tokens":{"access_token":"access-token"}}`),
	}))
	if got := connect.CodeOf(err); got != connect.CodeAlreadyExists {
		t.Fatalf("partial nested tokens code = %v, want AlreadyExists (err=%v)", got, err)
	}
	if len(creds.blobs) != 1 {
		t.Fatalf("cred store has %d blobs, want 1", len(creds.blobs))
	}
	if got, want := string(creds.blobs[resp.Msg.Account.GetId()]), `{"access":"access-token","refresh":"refresh-token","id_token":"id-token"}`; got != want {
		t.Fatalf("stored credential = %q, want %q", got, want)
	}
}

func TestAddAccountConcurrentDuplicateCredential(t *testing.T) {
	creds := newFakeCredStore()
	srv, _ := newAccountServer(t, creds, nil)
	ctx := context.Background()

	firstSaveReached := make(chan struct{})
	releaseFirstSave := make(chan struct{})
	var pauseFirstSave sync.Once
	creds.preSaveHook = func(_ string, _ []byte) {
		pauseFirstSave.Do(func() {
			close(firstSaveReached)
			<-releaseFirstSave
		})
	}

	errs := make(chan error, 2)
	go func() {
		_, err := srv.AddAccount(ctx, connect.NewRequest(&pb.AddAccountRequest{
			Provider:   "claude",
			Label:      "work",
			Credential: []byte("same-token"),
		}))
		errs <- err
	}()

	select {
	case <-firstSaveReached:
	case <-time.After(time.Second):
		t.Fatal("first AddAccount did not reach credential save")
	}

	go func() {
		_, err := srv.AddAccount(ctx, connect.NewRequest(&pb.AddAccountRequest{
			Provider:   "claude",
			Label:      "personal",
			Credential: []byte("same-token"),
		}))
		errs <- err
	}()
	close(releaseFirstSave)

	var success, alreadyExists int
	for i := 0; i < 2; i++ {
		err := <-errs
		if err == nil {
			success++
			continue
		}
		switch connect.CodeOf(err) {
		case connect.CodeAlreadyExists:
			alreadyExists++
		default:
			t.Fatalf("unexpected AddAccount error: %v", err)
		}
	}
	if success != 1 || alreadyExists != 1 {
		t.Fatalf("success/AlreadyExists = %d/%d, want 1/1", success, alreadyExists)
	}
	if len(creds.blobs) != 1 {
		t.Fatalf("cred store has %d blobs, want 1", len(creds.blobs))
	}
}

// TestUpdateAccountReservedLabelRejected proves renaming an account to the
// reserved system-default label (case-insensitively) is a client error, so no
// real account can ever take the "Unmanaged local credentials" label the
// apiversion V20260706 switch down-convert keys on.
func TestUpdateAccountReservedLabelRejected(t *testing.T) {
	srv, _ := newAccountServer(t, newFakeCredStore(), nil)
	acct := mustAddClaude(t, srv, "work", []byte("setup-token"))

	reserved := "Unmanaged Local Credentials"
	_, err := srv.UpdateAccount(context.Background(), connect.NewRequest(&pb.UpdateAccountRequest{
		Id:    acct.Id,
		Label: &reserved,
	}))
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err=%v)", got, err)
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

func TestRefreshAccountReplacesCredentialAndReturnsMetadataOnly(t *testing.T) {
	creds := newFakeCredStore()
	srv, _ := newAccountServer(t, creds, nil)
	secret := []byte("replacement-secret-token")
	acct := mustAddClaude(t, srv, "refresh", []byte("old-token"))

	resp, err := srv.RefreshAccount(context.Background(), connect.NewRequest(&pb.RefreshAccountRequest{
		Id:         acct.Id,
		Credential: secret,
	}))
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if got := string(creds.blobs[acct.Id]); got != string(secret) {
		t.Fatalf("stored credential = %q, want replacement", got)
	}
	if resp.Msg.Account.GetId() != acct.Id {
		t.Fatalf("account id = %q, want %q", resp.Msg.Account.GetId(), acct.Id)
	}
	if resp.Msg.GetLiveSmokeRan() {
		t.Fatalf("live_smoke_ran = true, want false without test_after_save")
	}
	if resp.Msg.GetDetail() == "" {
		t.Fatalf("detail empty, want success detail")
	}
	raw, err := proto.Marshal(resp.Msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(raw, secret) {
		t.Fatalf("RefreshAccount response wire form leaked the credential")
	}
}

func TestRefreshAccountRejectsDuplicateCredentialExceptSelf(t *testing.T) {
	creds := newFakeCredStore()
	srv, _ := newAccountServer(t, creds, nil)
	first := mustAddClaude(t, srv, "work", []byte("token-a"))
	second := mustAddClaude(t, srv, "personal", []byte("token-b"))

	_, err := srv.RefreshAccount(context.Background(), connect.NewRequest(&pb.RefreshAccountRequest{
		Id:         second.Id,
		Credential: []byte("token-a"),
	}))
	if got := connect.CodeOf(err); got != connect.CodeAlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists (err=%v)", got, err)
	}
	if got := string(creds.blobs[first.Id]); got != "token-a" {
		t.Fatalf("first credential = %q, want token-a", got)
	}
	if got := string(creds.blobs[second.Id]); got != "token-b" {
		t.Fatalf("second credential = %q, want token-b after rejected refresh", got)
	}

	if _, err := srv.RefreshAccount(context.Background(), connect.NewRequest(&pb.RefreshAccountRequest{
		Id:         first.Id,
		Credential: []byte("token-a"),
	})); err != nil {
		t.Fatalf("RefreshAccount self credential: %v", err)
	}
}

func TestRefreshAccountRestoresFailedHealth(t *testing.T) {
	creds := newFakeCredStore()
	srv, accts := newAccountServer(t, creds, nil)
	acct := mustAddClaude(t, srv, "refresh-health", []byte("old-token"))
	failed := models.AccountHealthFailed
	if _, err := accts.Update(context.Background(), acct.Id, db.UpdateAccountParams{Health: &failed}); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	resp, err := srv.RefreshAccount(context.Background(), connect.NewRequest(&pb.RefreshAccountRequest{
		Id:         acct.Id,
		Credential: []byte("new-token"),
	}))
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if got := resp.Msg.Account.GetHealth(); got != string(models.AccountHealthOK) {
		t.Fatalf("response health = %q, want %q", got, models.AccountHealthOK)
	}
	got, err := accts.Get(context.Background(), acct.Id)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if got.Health != models.AccountHealthOK {
		t.Fatalf("stored health = %q, want %q", got.Health, models.AccountHealthOK)
	}
}

func TestRefreshAccountWithFailedTestPreservesFailedHealth(t *testing.T) {
	creds := newFakeCredStore()
	smoke := &fakeSmoke{err: errors.New("provider rejected token")}
	srv, accts := newAccountServer(t, creds, smoke)
	acct := mustAddClaude(t, srv, "refresh-health-test-fails", []byte("old-token"))
	failed := models.AccountHealthFailed
	if _, err := accts.Update(context.Background(), acct.Id, db.UpdateAccountParams{Health: &failed}); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	resp, err := srv.RefreshAccount(context.Background(), connect.NewRequest(&pb.RefreshAccountRequest{
		Id:            acct.Id,
		Credential:    []byte("new-token"),
		TestAfterSave: true,
	}))
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if !resp.Msg.GetLiveSmokeRan() {
		t.Fatalf("live_smoke_ran = false, want true")
	}
	if got := resp.Msg.Account.GetHealth(); got != string(models.AccountHealthFailed) {
		t.Fatalf("response health = %q, want %q", got, models.AccountHealthFailed)
	}
	if resp.Msg.Account.GetLastTestError() == "" {
		t.Fatalf("last_test_error empty, want smoke failure detail")
	}
	got, err := accts.Get(context.Background(), acct.Id)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if got.Health != models.AccountHealthFailed {
		t.Fatalf("stored health = %q, want %q", got.Health, models.AccountHealthFailed)
	}
}

func TestRefreshAccountWithFailedTestFailsHealthyAccount(t *testing.T) {
	creds := newFakeCredStore()
	smoke := &fakeSmoke{err: errors.New("provider rejected token")}
	srv, accts := newAccountServer(t, creds, smoke)
	acct := mustAddClaude(t, srv, "refresh-healthy-test-fails", []byte("old-token"))

	resp, err := srv.RefreshAccount(context.Background(), connect.NewRequest(&pb.RefreshAccountRequest{
		Id:            acct.Id,
		Credential:    []byte("new-token"),
		TestAfterSave: true,
	}))
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if !resp.Msg.GetLiveSmokeRan() {
		t.Fatalf("live_smoke_ran = false, want true")
	}
	if got := resp.Msg.Account.GetHealth(); got != string(models.AccountHealthFailed) {
		t.Fatalf("response health = %q, want %q", got, models.AccountHealthFailed)
	}
	if resp.Msg.Account.GetLastTestError() == "" {
		t.Fatalf("last_test_error empty, want smoke failure detail")
	}
	got, err := accts.Get(context.Background(), acct.Id)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if got.Health != models.AccountHealthFailed {
		t.Fatalf("stored health = %q, want %q", got.Health, models.AccountHealthFailed)
	}
}

func TestRefreshAccountWithUnavailableSmokeDoesNotFailHealth(t *testing.T) {
	creds := newFakeCredStore()
	srv, accts := newAccountServer(t, creds, nil)
	acct := mustAddClaude(t, srv, "refresh-no-smoke", []byte("old-token"))

	resp, err := srv.RefreshAccount(context.Background(), connect.NewRequest(&pb.RefreshAccountRequest{
		Id:            acct.Id,
		Credential:    []byte("new-token"),
		TestAfterSave: true,
	}))
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if resp.Msg.GetLiveSmokeRan() {
		t.Fatalf("live_smoke_ran = true, want false")
	}
	if got := resp.Msg.GetDetail(); got != liveSmokeUnavailableDetail {
		t.Fatalf("detail = %q, want %q", got, liveSmokeUnavailableDetail)
	}
	if got := resp.Msg.Account.GetHealth(); got != string(models.AccountHealthOK) {
		t.Fatalf("response health = %q, want %q", got, models.AccountHealthOK)
	}
	if got := resp.Msg.Account.GetLastTestError(); got != liveSmokeUnavailableDetail {
		t.Fatalf("last_test_error = %q, want %q", got, liveSmokeUnavailableDetail)
	}
	got, err := accts.Get(context.Background(), acct.Id)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if got.Health != models.AccountHealthOK {
		t.Fatalf("stored health = %q, want %q", got.Health, models.AccountHealthOK)
	}
}

func TestRefreshAccountWithUnavailableSmokeRestoresFailedHealth(t *testing.T) {
	creds := newFakeCredStore()
	srv, accts := newAccountServer(t, creds, nil)
	acct := mustAddClaude(t, srv, "refresh-no-smoke-failed", []byte("old-token"))
	failed := models.AccountHealthFailed
	if _, err := accts.Update(context.Background(), acct.Id, db.UpdateAccountParams{Health: &failed}); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	resp, err := srv.RefreshAccount(context.Background(), connect.NewRequest(&pb.RefreshAccountRequest{
		Id:            acct.Id,
		Credential:    []byte("new-token"),
		TestAfterSave: true,
	}))
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if resp.Msg.GetLiveSmokeRan() {
		t.Fatalf("live_smoke_ran = true, want false")
	}
	if got := resp.Msg.GetDetail(); got != liveSmokeUnavailableDetail {
		t.Fatalf("detail = %q, want %q", got, liveSmokeUnavailableDetail)
	}
	if got := resp.Msg.Account.GetHealth(); got != string(models.AccountHealthOK) {
		t.Fatalf("response health = %q, want %q", got, models.AccountHealthOK)
	}
	if got := resp.Msg.Account.GetLastTestError(); got != liveSmokeUnavailableDetail {
		t.Fatalf("last_test_error = %q, want %q", got, liveSmokeUnavailableDetail)
	}
	got, err := accts.Get(context.Background(), acct.Id)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if got.Health != models.AccountHealthOK {
		t.Fatalf("stored health = %q, want %q", got.Health, models.AccountHealthOK)
	}
}

func TestRefreshAccountWithValidationFailureFailsHealthyAccount(t *testing.T) {
	creds := newFakeCredStore()
	smoke := &fakeSmoke{}
	srv, accts := newAccountServer(t, creds, smoke)
	resp, err := srv.AddAccount(context.Background(), connect.NewRequest(&pb.AddAccountRequest{
		Provider:   "codex",
		Label:      "refresh-invalid-codex",
		Credential: []byte(`{"access":"a","refresh":"r","id_token":"i"}`),
	}))
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	id := resp.Msg.Account.GetId()

	refreshResp, err := srv.RefreshAccount(context.Background(), connect.NewRequest(&pb.RefreshAccountRequest{
		Id:            id,
		Credential:    []byte("not-json"),
		TestAfterSave: true,
	}))
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if refreshResp.Msg.GetLiveSmokeRan() {
		t.Fatalf("live_smoke_ran = true, want false")
	}
	if smoke.called {
		t.Fatalf("smoke runner ran on invalid credential")
	}
	if got := refreshResp.Msg.Account.GetHealth(); got != string(models.AccountHealthFailed) {
		t.Fatalf("response health = %q, want %q", got, models.AccountHealthFailed)
	}
	if refreshResp.Msg.Account.GetLastTestError() == "" {
		t.Fatalf("last_test_error empty, want validation failure detail")
	}
	got, err := accts.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if got.Health != models.AccountHealthFailed {
		t.Fatalf("stored health = %q, want %q", got.Health, models.AccountHealthFailed)
	}
}

func TestRefreshAccountCodexAuthJSONWithTestAfterSaveNormalizesCredential(t *testing.T) {
	creds := newFakeCredStore()
	smoke := &fakeSmoke{}
	srv, _ := newAccountServer(t, creds, smoke)
	resp, err := srv.AddAccount(context.Background(), connect.NewRequest(&pb.AddAccountRequest{
		Provider:   "codex",
		Label:      "refresh-codex-auth-json",
		Credential: []byte(`{"access":"old-access","refresh":"old-refresh","id_token":"old-id"}`),
	}))
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	id := resp.Msg.Account.GetId()

	refreshResp, err := srv.RefreshAccount(context.Background(), connect.NewRequest(&pb.RefreshAccountRequest{
		Id: id,
		Credential: []byte(`{"tokens":{` +
			`"access_token":"new-access",` +
			`"refresh_token":"new-refresh",` +
			`"id_token":"new-id"` +
			`}}`),
		TestAfterSave: true,
	}))
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if got := refreshResp.Msg.Account.GetHealth(); got != string(models.AccountHealthOK) {
		t.Fatalf("response health = %q, want %q", got, models.AccountHealthOK)
	}
	if refreshResp.Msg.Account.GetLastTestError() != "" {
		t.Fatalf("last_test_error = %q, want empty", refreshResp.Msg.Account.GetLastTestError())
	}
	if !refreshResp.Msg.GetLiveSmokeRan() {
		t.Fatalf("live_smoke_ran = false, want true")
	}
	if !smoke.called || smoke.provider != "codex" || smoke.accountID != id {
		t.Fatalf("smoke called with account=%q provider=%q", smoke.accountID, smoke.provider)
	}

	assertCodexCredentialFields(t, creds.blobs[id])
	assertCodexCredentialFields(t, smoke.blob)
}

func assertCodexCredentialFields(t *testing.T, blob []byte) {
	t.Helper()
	var payload struct {
		Access  string `json:"access"`
		Refresh string `json:"refresh"`
		IDToken string `json:"id_token"`
		Tokens  struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(blob, &payload); err != nil {
		t.Fatalf("credential is not valid JSON: %v", err)
	}
	if payload.Access != "new-access" || payload.Refresh != "new-refresh" || payload.IDToken != "new-id" {
		t.Fatalf("top-level credential fields = (%q,%q,%q), want refreshed tokens",
			payload.Access, payload.Refresh, payload.IDToken)
	}
	if payload.Tokens.AccessToken != "new-access" ||
		payload.Tokens.RefreshToken != "new-refresh" ||
		payload.Tokens.IDToken != "new-id" {
		t.Fatalf("tokens fields = (%q,%q,%q), want refreshed tokens",
			payload.Tokens.AccessToken, payload.Tokens.RefreshToken, payload.Tokens.IDToken)
	}
}

func TestRefreshAccountMissingAccountNotFound(t *testing.T) {
	srv, _ := newAccountServer(t, newFakeCredStore(), nil)

	_, err := srv.RefreshAccount(context.Background(), connect.NewRequest(&pb.RefreshAccountRequest{
		Id:         "missing",
		Credential: []byte("new-token"),
	}))
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("code = %v, want NotFound (err=%v)", got, err)
	}
}

func TestRefreshAccountCleansUpCredentialWhenAccountDisappearsAfterSave(t *testing.T) {
	creds := newFakeCredStore()
	srv, accts := newAccountServer(t, creds, nil)
	acct := mustAddClaude(t, srv, "refresh-removed", []byte("old-token"))
	creds.saveHook = func(id string, _ []byte) {
		if err := accts.Delete(context.Background(), id); err != nil {
			t.Fatalf("delete account during refresh save: %v", err)
		}
	}

	_, err := srv.RefreshAccount(context.Background(), connect.NewRequest(&pb.RefreshAccountRequest{
		Id:         acct.Id,
		Credential: []byte("new-token"),
	}))
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("code = %v, want NotFound (err=%v)", got, err)
	}
	if _, ok := creds.blobs[acct.Id]; ok {
		t.Fatalf("credential left behind for removed account")
	}
}

func TestRefreshAccountWithTestAfterSaveCleansUpCredentialWhenAccountDisappearsAfterSave(t *testing.T) {
	creds := newFakeCredStore()
	smoke := &fakeSmoke{}
	srv, accts := newAccountServer(t, creds, smoke)
	acct := mustAddClaude(t, srv, "refresh-test-removed", []byte("old-token"))
	creds.saveHook = func(id string, _ []byte) {
		if err := accts.Delete(context.Background(), id); err != nil {
			t.Fatalf("delete account during refresh save: %v", err)
		}
	}

	_, err := srv.RefreshAccount(context.Background(), connect.NewRequest(&pb.RefreshAccountRequest{
		Id:            acct.Id,
		Credential:    []byte("new-token"),
		TestAfterSave: true,
	}))
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("code = %v, want NotFound (err=%v)", got, err)
	}
	if _, ok := creds.blobs[acct.Id]; ok {
		t.Fatalf("credential left behind for removed account")
	}
	if smoke.called {
		t.Fatalf("smoke ran after account disappeared")
	}
}

func TestRefreshAccountRejectsEmptyCredential(t *testing.T) {
	creds := newFakeCredStore()
	srv, _ := newAccountServer(t, creds, nil)
	acct := mustAddClaude(t, srv, "empty", []byte("old-token"))

	_, err := srv.RefreshAccount(context.Background(), connect.NewRequest(&pb.RefreshAccountRequest{
		Id: acct.Id,
	}))
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err=%v)", got, err)
	}
	if got := string(creds.blobs[acct.Id]); got != "old-token" {
		t.Fatalf("credential changed on invalid refresh: %q", got)
	}
}

func TestRefreshAccountWithTestAfterSaveRunsSmoke(t *testing.T) {
	creds := newFakeCredStore()
	smoke := &fakeSmoke{}
	srv, _ := newAccountServer(t, creds, smoke)
	acct := mustAddClaude(t, srv, "refresh-live", []byte("old-token"))

	resp, err := srv.RefreshAccount(context.Background(), connect.NewRequest(&pb.RefreshAccountRequest{
		Id:            acct.Id,
		Credential:    []byte("new-token"),
		TestAfterSave: true,
	}))
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if !resp.Msg.GetLiveSmokeRan() {
		t.Fatalf("live_smoke_ran = false, want true")
	}
	if !smoke.called || smoke.accountID != acct.Id || smoke.provider != "claude" {
		t.Fatalf("smoke called with account=%q provider=%q", smoke.accountID, smoke.provider)
	}
	if got := string(smoke.blob); got != "new-token" {
		t.Fatalf("smoke credential = %q, want refreshed credential", got)
	}
	if resp.Msg.GetDetail() != "credential test passed" {
		t.Fatalf("detail = %q, want credential test passed", resp.Msg.GetDetail())
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
		t.Errorf("last_test_ok_at set, want nil when provider verification is unavailable")
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
