package accountwiring

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
)

// spyStore is an in-memory AccountStore that records Get/Update calls so tests
// can assert the last-used bump happens exactly once (and only for bound,
// rotation-capable accounts).
type spyStore struct {
	accounts    map[string]*models.Account
	getCalls    int
	updateCalls int
	lastUpdate  db.UpdateAccountParams
}

func (s *spyStore) List(_ context.Context) ([]*models.Account, error) {
	out := make([]*models.Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, a)
	}
	return out, nil
}

func (s *spyStore) Get(_ context.Context, id string) (*models.Account, error) {
	s.getCalls++
	a, ok := s.accounts[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return a, nil
}

func (s *spyStore) Update(_ context.Context, id string, params db.UpdateAccountParams) (*models.Account, error) {
	s.updateCalls++
	s.lastUpdate = params
	return s.accounts[id], nil
}

// fakeCreds is an in-memory CredentialLoader.
type fakeCreds struct {
	blob  []byte
	err   error
	calls int
}

func (f *fakeCreds) Load(string) ([]byte, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.blob, nil
}

// fakeRotationClient embeds the AgentRunnerClient interface (nil) and overrides
// only the two RPCs the materializer uses; any other call would panic, proving
// the resolver touches nothing else.
type fakeRotationClient struct {
	agent.AgentRunnerClient
	supports bool
	capErr   error
	env      map[string]string
	matErr   error
	capCalls int
	matCalls int
	lastBlob []byte
}

func (f *fakeRotationClient) RotationCapability(context.Context, *bossanovav1.RotationCapabilityRequest) (*bossanovav1.RotationCapabilityResponse, error) {
	f.capCalls++
	if f.capErr != nil {
		return nil, f.capErr
	}
	return &bossanovav1.RotationCapabilityResponse{SupportsRotation: f.supports}, nil
}

func (f *fakeRotationClient) MaterializeAccount(_ context.Context, req *bossanovav1.MaterializeAccountRequest) (*bossanovav1.MaterializeAccountResponse, error) {
	f.matCalls++
	f.lastBlob = req.GetCredentialBlob()
	if f.matErr != nil {
		return nil, f.matErr
	}
	return &bossanovav1.MaterializeAccountResponse{Env: f.env}, nil
}

func newAccount(id, provider string) *models.Account {
	return &models.Account{
		ID:       id,
		Provider: models.AccountProvider(provider),
		Status:   models.AccountStatusActive,
		Priority: 1,
	}
}

func newResolver(store *spyStore, client *fakeRotationClient, creds CredentialLoader) *account.Resolver {
	clients := map[string]agent.AgentRunnerClient{}
	if client != nil {
		clients["claude"] = client
	}
	return account.NewResolver(
		NewRegistry(store),
		NewMaterializer(clients, store, creds, zerolog.Nop()),
		zerolog.Nop(),
	)
}

// (a) An unbound session ("" AccountID) must resolve to account 0 with NO
// registry or plugin RPCs at all.
func TestSpawnEnvResolver_UnboundNoRPC(t *testing.T) {
	store := &spyStore{accounts: map[string]*models.Account{}}
	client := &fakeRotationClient{}
	r := NewSpawnEnvResolver(newResolver(store, client, &fakeCreds{}), zerolog.Nop())

	if env := r.Resolve(context.Background(), &models.Session{AgentName: "claude"}); env != nil {
		t.Fatalf("unbound session env = %v, want nil", env)
	}
	if store.getCalls != 0 || store.updateCalls != 0 {
		t.Errorf("store calls get=%d update=%d, want 0/0", store.getCalls, store.updateCalls)
	}
	if client.capCalls != 0 || client.matCalls != 0 {
		t.Errorf("client calls cap=%d mat=%d, want 0/0", client.capCalls, client.matCalls)
	}
}

// (c) A bound session whose provider plugin lacks rotation degrades to nil env
// (status-only binding): RotationCapability is consulted once, but no
// MaterializeAccount and no last-used bump happen. The session still spawns.
func TestSpawnEnvResolver_NoRotationDegrades(t *testing.T) {
	store := &spyStore{accounts: map[string]*models.Account{"a1": newAccount("a1", "claude")}}
	client := &fakeRotationClient{supports: false}
	r := NewSpawnEnvResolver(newResolver(store, client, &fakeCreds{blob: []byte("blob")}), zerolog.Nop())

	sess := &models.Session{AgentName: "claude", AccountID: strptr("a1")}
	if env := r.Resolve(context.Background(), sess); env != nil {
		t.Fatalf("no-rotation env = %v, want nil (degrade)", env)
	}
	if client.capCalls != 1 {
		t.Errorf("RotationCapability calls = %d, want 1", client.capCalls)
	}
	if client.matCalls != 0 {
		t.Errorf("MaterializeAccount calls = %d, want 0", client.matCalls)
	}
	if store.updateCalls != 0 {
		t.Errorf("last-used bump calls = %d, want 0 (no touch on degrade)", store.updateCalls)
	}
}

// (b)+(d) A bound, rotation-capable account materializes its env, forwards the
// credential blob, and bumps last-used exactly once.
func TestSpawnEnvResolver_BoundRotationInjectsEnvAndTouchesOnce(t *testing.T) {
	store := &spyStore{accounts: map[string]*models.Account{"a1": newAccount("a1", "claude")}}
	client := &fakeRotationClient{supports: true, env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "x"}}
	creds := &fakeCreds{blob: []byte("secret-blob")}
	r := NewSpawnEnvResolver(newResolver(store, client, creds), zerolog.Nop())

	sess := &models.Session{AgentName: "claude", AccountID: strptr("a1")}
	env := r.Resolve(context.Background(), sess)
	if env["CLAUDE_CODE_OAUTH_TOKEN"] != "x" {
		t.Fatalf("materialized env = %v, want CLAUDE_CODE_OAUTH_TOKEN=x", env)
	}
	if client.matCalls != 1 {
		t.Errorf("MaterializeAccount calls = %d, want 1", client.matCalls)
	}
	if string(client.lastBlob) != "secret-blob" {
		t.Errorf("credential blob forwarded = %q, want secret-blob", client.lastBlob)
	}
	if creds.calls != 1 {
		t.Errorf("credential Load calls = %d, want 1", creds.calls)
	}
	if store.updateCalls != 1 {
		t.Errorf("last-used bump calls = %d, want exactly 1", store.updateCalls)
	}
	if store.lastUpdate.LastUsedAt == nil || *store.lastUpdate.LastUsedAt == nil {
		t.Errorf("last-used bump did not set LastUsedAt: %+v", store.lastUpdate)
	}
}

// SupportsRotation degrades (false, nil) for a missing client, an Unimplemented
// plugin, and any other RPC error — an unreachable plugin never fails a spawn.
func TestMaterializer_SupportsRotationDegrades(t *testing.T) {
	store := &spyStore{accounts: map[string]*models.Account{}}

	// Missing client for provider.
	m := NewMaterializer(map[string]agent.AgentRunnerClient{}, store, &fakeCreds{}, zerolog.Nop())
	if ok, err := m.SupportsRotation(context.Background(), "claude"); ok || err != nil {
		t.Errorf("missing client: got (%v,%v), want (false,nil)", ok, err)
	}

	for name, capErr := range map[string]error{
		"unimplemented": grpcstatus.Error(codes.Unimplemented, "no rotation"),
		"other":         errors.New("boom"),
	} {
		client := &fakeRotationClient{capErr: capErr}
		m := NewMaterializer(map[string]agent.AgentRunnerClient{"claude": client}, store, &fakeCreds{}, zerolog.Nop())
		if ok, err := m.SupportsRotation(context.Background(), "claude"); ok || err != nil {
			t.Errorf("%s: got (%v,%v), want (false,nil)", name, ok, err)
		}
	}
}

// The registry adapter maps model fields and TouchLastUsed writes the
// **time.Time last-used column.
func TestRegistryAdapter_MapAndTouch(t *testing.T) {
	cool := time.Now().Add(time.Hour)
	acct := &models.Account{
		ID: "a1", Provider: "codex", Label: "work", Status: models.AccountStatusActive,
		Priority: 7, CooldownUntil: &cool,
	}
	store := &spyStore{accounts: map[string]*models.Account{"a1": acct}}
	reg := NewRegistry(store)

	got, ok, err := reg.Get(context.Background(), "a1")
	if err != nil || !ok {
		t.Fatalf("Get = (_,%v,%v), want (_,true,nil)", ok, err)
	}
	if got.Provider != "codex" || got.Label != "work" || got.Status != "active" || got.Priority != 7 || got.CoolingUntil == nil {
		t.Errorf("mapped meta = %+v", got)
	}

	// Unknown id is (zero,false,nil) not an error.
	if _, ok, err := reg.Get(context.Background(), "missing"); ok || err != nil {
		t.Errorf("Get(missing) = (_,%v,%v), want (_,false,nil)", ok, err)
	}

	at := time.Now()
	if err := reg.TouchLastUsed(context.Background(), "a1", at); err != nil {
		t.Fatalf("TouchLastUsed: %v", err)
	}
	if store.lastUpdate.LastUsedAt == nil || *store.lastUpdate.LastUsedAt == nil || !(*store.lastUpdate.LastUsedAt).Equal(at) {
		t.Errorf("TouchLastUsed wrote %+v, want LastUsedAt=%v", store.lastUpdate, at)
	}
}

func strptr(s string) *string { return &s }
