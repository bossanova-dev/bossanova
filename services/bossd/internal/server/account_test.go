package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/accountcred"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"
)

// eventRecorder is a shared, ordered log used by the refresh-ordering test to
// prove Save → ForgetAllBearers → TestAccount(smoke) happen in that sequence.
type eventRecorder struct {
	mu  sync.Mutex
	log []string
}

func (e *eventRecorder) record(step string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, step)
}

func (e *eventRecorder) steps() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.log...)
}

// recordingProxyRegistrar implements session.proxyTokenRegistrar and logs the
// ForgetAllBearers call into a shared eventRecorder.
type recordingProxyRegistrar struct{ rec *eventRecorder }

func (r recordingProxyRegistrar) TokenForSession(string) string                    { return "" }
func (r recordingProxyRegistrar) TokenForChat(string, string, string) string       { return "" }
func (r recordingProxyRegistrar) ForgetBearer(string)                              {}
func (r recordingProxyRegistrar) ForgetAllBearers()                                { r.rec.record("forget") }
func (r recordingProxyRegistrar) AdoptToken(string, string)                        {}
func (r recordingProxyRegistrar) AdoptTokenForChat(string, string, string, string) {}

// recordingSmoke logs the TestAccount live-smoke into a shared eventRecorder.
type recordingSmoke struct{ rec *eventRecorder }

func (s recordingSmoke) Smoke(context.Context, string, string, []byte) error {
	s.rec.record("test")
	return nil
}

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
	deleteHook  func(id string)
}

// lockedFakeCredStore exercises the optional production lock without forcing
// every AccountCredentialStore test double to implement it.
type lockedFakeCredStore struct {
	*fakeCredStore
	lockMu    sync.Mutex
	lockCalls int
}

func (f *lockedFakeCredStore) WithCredentialLock(_ string, fn func() error) error {
	f.lockMu.Lock()
	defer f.lockMu.Unlock()
	f.lockCalls++
	return fn()
}

func (f *lockedFakeCredStore) credentialLockCalls() int {
	f.lockMu.Lock()
	defer f.lockMu.Unlock()
	return f.lockCalls
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
	if f.deleteHook != nil {
		f.deleteHook(id)
	}
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

// materializationPurge is one recorded RemoveMaterialization argument pair.
type materializationPurge struct {
	provider  string
	accountID string
}

// fakeAccountMaterializations is an in-memory AccountMaterializations capability.
// It records every purge (provider + id) and, when rec is set, logs the call into
// a shared eventRecorder so RemoveAccount's ordering is assertable. It never
// touches disk.
type fakeAccountMaterializations struct {
	rec    *eventRecorder
	purges []materializationPurge
	err    error
}

func (f *fakeAccountMaterializations) RemoveMaterialization(_ context.Context, provider, accountID string) error {
	f.purges = append(f.purges, materializationPurge{provider: provider, accountID: accountID})
	if f.rec != nil {
		f.rec.record("purge")
	}
	return f.err
}

// recordingAccountStore logs the metadata-row delete into a shared eventRecorder
// so the purge → keyring → row sequence can be pinned.
type recordingAccountStore struct {
	db.AccountStore
	rec *eventRecorder
}

func (r recordingAccountStore) Delete(ctx context.Context, id string) error {
	r.rec.record("row-delete")
	return r.AccountStore.Delete(ctx, id)
}

// fakeUsageProbe records the accounts ListAccounts probed. ListAccounts runs
// its probes concurrently (usageProbeConcurrency), so the recording is guarded
// and the recorded order is not meaningful — assert on the SET of ids.
type fakeUsageProbe struct {
	mu  sync.Mutex
	ids []string
	err error
}

func (f *fakeUsageProbe) RecordUsageProbe(_ context.Context, accountID string) error {
	f.mu.Lock()
	f.ids = append(f.ids, accountID)
	f.mu.Unlock()
	return f.err
}

func (f *fakeUsageProbe) probed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.ids...)
	sort.Strings(out)
	return out
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
	want := []string{first.Id, second.Id}
	sort.Strings(want)
	if got := probe.probed(); !slices.Equal(got, want) {
		t.Fatalf("probe ids = %v, want %v", got, want)
	}

	probe.mu.Lock()
	probe.ids = nil
	probe.mu.Unlock()
	refresh = false
	if _, err := srv.ListAccounts(ctx, connect.NewRequest(&pb.ListAccountsRequest{Refresh: &refresh})); err != nil {
		t.Fatalf("ListAccounts no refresh: %v", err)
	}
	if got := probe.probed(); len(got) != 0 {
		t.Fatalf("refresh=false probe calls = %v, want none", got)
	}

	if _, err := store.Get(ctx, first.Id); err != nil {
		t.Fatalf("store still readable after fail-soft probe: %v", err)
	}
}

// barrierUsageProbe parks every probe until release is closed, announcing each
// arrival first. A sequential ListAccounts can therefore only ever announce
// ONE arrival, which is what makes the concurrency assertion falsifiable.
type barrierUsageProbe struct {
	arrived chan string
	release chan struct{}
}

func (b *barrierUsageProbe) RecordUsageProbe(ctx context.Context, accountID string) error {
	b.arrived <- accountID
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestListAccountsRefreshProbesConcurrently asserts a refresh=true list probes
// accounts in parallel rather than one at a time (BOS-655). Sequential probing
// made the refresh cost the SUM of every probe — ~20s each for a worst-case
// Claude account — so it outgrew bosso's bounded refresh deadline as accounts
// were added, and a timeout there makes that daemon's rows vanish from the web
// account list instead of updating. Three accounts all park inside the probe at
// once here; under the old loop the second arrival never happens and the read
// below times out.
func TestListAccountsRefreshProbesConcurrently(t *testing.T) {
	srv, _ := newAccountServer(t, newFakeCredStore(), nil)
	const accountCount = 3
	for i := range accountCount {
		// Distinct credentials: AddAccount rejects a duplicate blob.
		mustAddClaude(t, srv, fmt.Sprintf("acct-%d", i), fmt.Appendf(nil, "token-%d", i))
	}
	probe := &barrierUsageProbe{
		arrived: make(chan string, accountCount),
		release: make(chan struct{}),
	}
	srv.usageProbe = probe

	refresh := true
	listed := make(chan error, 1)
	go func() {
		_, err := srv.ListAccounts(context.Background(), connect.NewRequest(&pb.ListAccountsRequest{Refresh: &refresh}))
		listed <- err
	}()

	for i := range accountCount {
		select {
		case <-probe.arrived:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d probes started concurrently", i, accountCount)
		}
	}
	close(probe.release)

	select {
	case err := <-listed:
		if err != nil {
			t.Fatalf("ListAccounts refresh: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListAccounts did not return after the probes were released")
	}
}

// TestRefreshUsageSnapshotsSharesConcurrencyLimitAcrossRequests proves that
// independent ListAccounts commands share the daemon-wide cap. The reverse
// stream dispatches each command in its own goroutine, so without a Server
// semaphore two refreshes could each start usageProbeConcurrency probes.
func TestRefreshUsageSnapshotsSharesConcurrencyLimitAcrossRequests(t *testing.T) {
	srv, _ := newAccountServer(t, newFakeCredStore(), nil)
	accounts := make([]*models.Account, usageProbeConcurrency)
	for i := range accounts {
		accounts[i] = &models.Account{ID: fmt.Sprintf("acct-%d", i)}
	}
	probe := &barrierUsageProbe{
		arrived: make(chan string, 2*usageProbeConcurrency),
		release: make(chan struct{}),
	}
	srv.usageProbe = probe

	done := make(chan struct{}, 2)
	for range 2 {
		go func() {
			srv.refreshUsageSnapshots(context.Background(), accounts)
			done <- struct{}{}
		}()
	}

	for i := 0; i < usageProbeConcurrency; i++ {
		select {
		case <-probe.arrived:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d shared probes started", i, usageProbeConcurrency)
		}
	}
	select {
	case accountID := <-probe.arrived:
		t.Fatalf("started %q above the daemon-wide limit of %d", accountID, usageProbeConcurrency)
	default:
	}

	close(probe.release)
	for range 2 {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("refresh did not return after the probes were released")
		}
	}
}

// TestRefreshUsageSnapshotsStopsSchedulingWhenCancelled ensures a refresh
// deadline cancels a wait for a saturated probe slot instead of allowing the
// next account to start as an earlier probe exits.
func TestRefreshUsageSnapshotsStopsSchedulingWhenCancelled(t *testing.T) {
	srv, _ := newAccountServer(t, newFakeCredStore(), nil)
	accounts := make([]*models.Account, usageProbeConcurrency+1)
	for i := range accounts {
		accounts[i] = &models.Account{ID: fmt.Sprintf("acct-%d", i)}
	}
	probe := &barrierUsageProbe{
		arrived: make(chan string, len(accounts)),
		release: make(chan struct{}),
	}
	srv.usageProbe = probe

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		srv.refreshUsageSnapshots(ctx, accounts)
		close(done)
	}()

	for i := 0; i < usageProbeConcurrency; i++ {
		select {
		case <-probe.arrived:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d initial probes started", i, usageProbeConcurrency)
		}
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not stop after cancellation")
	}
	select {
	case accountID := <-probe.arrived:
		t.Fatalf("scheduled %q after cancellation", accountID)
	default:
	}
}

func mustAddClaude(t *testing.T, srv *Server, label string, cred []byte) *pb.Account {
	t.Helper()
	resp, err := srv.AddAccount(context.Background(), connect.NewRequest(&pb.AddAccountRequest{
		Provider:   "claude",
		Label:      label,
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

func TestRefreshAccountUsesOptionalCredentialStoreLock(t *testing.T) {
	creds := &lockedFakeCredStore{fakeCredStore: newFakeCredStore()}
	srv, _ := newAccountServer(t, creds, nil)
	acct := mustAddClaude(t, srv, "refresh-lock", []byte("old-token"))

	if _, err := srv.RefreshAccount(context.Background(), connect.NewRequest(&pb.RefreshAccountRequest{
		Id:         acct.Id,
		Credential: []byte("new-token"),
	})); err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if got := creds.credentialLockCalls(); got != 1 {
		t.Fatalf("credential lock calls = %d, want 1", got)
	}
}

// TestRefreshAccountForgetsProxyBearersAfterSaveBeforeTest proves the BOS-484
// wiring: RefreshAccount drops every sticky failover-proxy bearer AFTER the new
// credential is saved and BEFORE the optional TestAccount live-smoke, so the
// smoke (and every later request) authenticates against the freshly-saved
// secret rather than a stale swapped bearer.
func TestRefreshAccountForgetsProxyBearersAfterSaveBeforeTest(t *testing.T) {
	rec := &eventRecorder{}
	creds := newFakeCredStore()
	creds.saveHook = func(string, []byte) { rec.record("save") }
	srv, _ := newAccountServer(t, creds, recordingSmoke{rec: rec})
	lc := &session.Lifecycle{}
	lc.SetProxyRegistrar(recordingProxyRegistrar{rec: rec})
	srv.lifecycle = lc
	acct := mustAddClaude(t, srv, "refresh-forget", []byte("old-token"))
	// The add above also saved; reset the log to isolate the refresh sequence.
	rec.log = nil

	if _, err := srv.RefreshAccount(context.Background(), connect.NewRequest(&pb.RefreshAccountRequest{
		Id:            acct.Id,
		Credential:    []byte("new-token"),
		TestAfterSave: true,
	})); err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}

	got := rec.steps()
	want := []string{"save", "forget", "test"}
	if len(got) != len(want) {
		t.Fatalf("event order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event order = %v, want %v", got, want)
		}
	}
}

// TestRefreshAccountWithoutLifecycleDoesNotPanic proves the dual-nil-safe
// wrapper: a Server with no Lifecycle wired (s.lifecycle == nil) refreshes a
// credential without panicking on the ForgetAllProxyBearers call.
func TestRefreshAccountWithoutLifecycleDoesNotPanic(t *testing.T) {
	creds := newFakeCredStore()
	srv, _ := newAccountServer(t, creds, nil) // no lifecycle wired
	acct := mustAddClaude(t, srv, "refresh-nolc", []byte("old-token"))

	if _, err := srv.RefreshAccount(context.Background(), connect.NewRequest(&pb.RefreshAccountRequest{
		Id:         acct.Id,
		Credential: []byte("new-token"),
	})); err != nil {
		t.Fatalf("RefreshAccount without lifecycle: %v", err)
	}
	if got := string(creds.blobs[acct.Id]); got != "new-token" {
		t.Fatalf("stored credential = %q, want new-token", got)
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

func TestTestAccountNoRunnerLoadedDegradesToUnavailable(t *testing.T) {
	creds := newFakeCredStore()
	// The smoke runner reports that no agent plugin is loaded to run verification.
	smoke := &fakeSmoke{err: fmt.Errorf("credential verification unavailable: %w", agent.ErrAgentRunnerNotLoaded)}
	srv, _ := newAccountServer(t, creds, smoke)
	acct := mustAddClaude(t, srv, "norunner", []byte("setup-token"))

	resp, err := srv.TestAccount(context.Background(), connect.NewRequest(&pb.TestAccountRequest{Id: acct.Id}))
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if resp.Msg.GetLiveSmokeRan() {
		t.Fatal("no-runner case must not report live_smoke_ran=true")
	}
	if resp.Msg.GetDetail() != liveSmokeUnavailableDetail {
		t.Fatalf("detail=%q, want the unavailable sentinel", resp.Msg.GetDetail())
	}
	if resp.Msg.Account.GetLastTestError() != liveSmokeUnavailableDetail {
		t.Fatalf("last_test_error=%q, want the unavailable sentinel", resp.Msg.Account.GetLastTestError())
	}
	if resp.Msg.Account.GetLastTestOkAt() != nil {
		t.Fatal("last_test_ok_at set, want nil when verification could not run")
	}
}

// TestLiveSmokeUnavailableDetailContract pins the exact sentinel bytes. The boss
// CLI (services/boss/internal/accountflow/claude.go) duplicates this literal (by
// module-boundary convention) and routes on an exact string match to keep an
// account silently when verification could not run. If you change this string
// you MUST change the CLI copy (and its matching contract test) in lockstep, or
// the CLI silently reverts to the keep/remove prompt for the unavailable case.
func TestLiveSmokeUnavailableDetailContract(t *testing.T) {
	const want = "provider verification unavailable"
	if liveSmokeUnavailableDetail != want {
		t.Fatalf("liveSmokeUnavailableDetail = %q, want %q (must match the boss CLI sentinel)", liveSmokeUnavailableDetail, want)
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

// codexAuthBlob is a well-formed codex credential, unique per label so the
// duplicate-credential guard does not reject a second account.
func codexAuthBlob(label string) []byte {
	return []byte(`{"access":"a-` + label + `","refresh":"r-` + label + `","id_token":"i-` + label + `"}`)
}

func TestRemoveAccountPurgesMaterializationBeforeCredentialAndRow(t *testing.T) {
	rec := &eventRecorder{}
	creds := newFakeCredStore()
	creds.deleteHook = func(string) { rec.record("cred-delete") }
	srv, accts := newAccountServer(t, creds, nil)
	srv.accounts = recordingAccountStore{AccountStore: accts, rec: rec}
	purger := &fakeAccountMaterializations{rec: rec}
	srv.accountMaterializations = purger
	ctx := context.Background()

	acct := mustAddCodex(t, srv, "materialized", codexAuthBlob("materialized"))
	if _, err := srv.RemoveAccount(ctx, connect.NewRequest(&pb.RemoveAccountRequest{Id: acct.Id})); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}

	want := []materializationPurge{{provider: "codex", accountID: acct.Id}}
	if len(purger.purges) != 1 || purger.purges[0] != want[0] {
		t.Fatalf("purges = %+v, want %+v", purger.purges, want)
	}
	if got, wantSteps := rec.steps(), []string{"purge", "cred-delete", "row-delete"}; !equalSteps(got, wantSteps) {
		t.Fatalf("order = %v, want %v", got, wantSteps)
	}
}

func TestRemoveAccountPurgeFailureIsInternalAndLeavesCredentialAndRow(t *testing.T) {
	rec := &eventRecorder{}
	creds := newFakeCredStore()
	creds.deleteHook = func(string) { rec.record("cred-delete") }
	srv, accts := newAccountServer(t, creds, nil)
	srv.accounts = recordingAccountStore{AccountStore: accts, rec: rec}
	srv.accountMaterializations = &fakeAccountMaterializations{rec: rec, err: errors.New("permission denied")}
	ctx := context.Background()

	acct := mustAddCodex(t, srv, "stuck", codexAuthBlob("stuck"))
	_, err := srv.RemoveAccount(ctx, connect.NewRequest(&pb.RemoveAccountRequest{Id: acct.Id}))
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("code = %v, want Internal (err=%v)", got, err)
	}
	// Neither the keyring credential nor the metadata row may be touched, so the
	// whole removal stays retryable.
	if got, wantSteps := rec.steps(), []string{"purge"}; !equalSteps(got, wantSteps) {
		t.Fatalf("order = %v, want %v (nothing may be deleted after a purge failure)", got, wantSteps)
	}
	if _, ok := creds.blobs[acct.Id]; !ok {
		t.Errorf("credential gone after purge failure, want it to survive")
	}
	if _, err := accts.Get(ctx, acct.Id); err != nil {
		t.Errorf("account row gone after purge failure: %v", err)
	}
}

func TestRemoveAccountWithoutMaterializationsCapabilityStillRemoves(t *testing.T) {
	creds := newFakeCredStore()
	srv, accts := newAccountServer(t, creds, nil)
	ctx := context.Background()

	acct := mustAddCodex(t, srv, "unwired", codexAuthBlob("unwired"))
	if srv.accountMaterializations != nil {
		t.Fatalf("capability should default to nil")
	}
	if _, err := srv.RemoveAccount(ctx, connect.NewRequest(&pb.RemoveAccountRequest{Id: acct.Id})); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	if _, ok := creds.blobs[acct.Id]; ok {
		t.Errorf("credential still present after remove")
	}
	if _, err := accts.Get(ctx, acct.Id); err == nil {
		t.Errorf("account row still present after remove")
	}
}

func TestRemoveAccountUnknownIDIsNotFoundWithoutPurging(t *testing.T) {
	purger := &fakeAccountMaterializations{}
	srv, _ := newAccountServer(t, newFakeCredStore(), nil)
	srv.accountMaterializations = purger

	_, err := srv.RemoveAccount(context.Background(), connect.NewRequest(&pb.RemoveAccountRequest{Id: "nope"}))
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("code = %v, want NotFound (err=%v)", got, err)
	}
	if len(purger.purges) != 0 {
		t.Errorf("purges = %+v, want none for an unknown id", purger.purges)
	}
}

func TestRemoveClaudeAccountStillInvokesPurge(t *testing.T) {
	creds := newFakeCredStore()
	srv, _ := newAccountServer(t, creds, nil)
	purger := &fakeAccountMaterializations{}
	srv.accountMaterializations = purger

	acct := mustAddClaude(t, srv, "claude-acct", []byte("setup-token"))
	if _, err := srv.RemoveAccount(context.Background(), connect.NewRequest(&pb.RemoveAccountRequest{Id: acct.Id})); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	// The RPC always calls the capability; the adapter's claude no-op is what
	// makes it harmless, so the server never branches on provider.
	want := materializationPurge{provider: "claude", accountID: acct.Id}
	if len(purger.purges) != 1 || purger.purges[0] != want {
		t.Fatalf("purges = %+v, want [%+v]", purger.purges, want)
	}
	if _, ok := creds.blobs[acct.Id]; ok {
		t.Errorf("credential still present after remove")
	}
}

func equalSteps(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
