package accountwiring

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/credmaterialize"

	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/accountcred"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
)

// spyStore is an in-memory AccountStore that records Get/Update calls so tests
// can assert the last-used bump happens exactly once (and only for bound,
// rotation-capable accounts).
type spyStore struct {
	accounts      map[string]*models.Account
	getCalls      int
	updateCalls   int
	usageCalls    int
	usageSnap     models.UsageSnapshot
	usageErr      error
	lastUpdate    db.UpdateAccountParams
	suspendCalls  int
	suspendID     string
	suspendReason string

	injectCalls  int
	injectID     string
	injectReason string
	clearCalls   int
	clearID      string

	// getErr, when set, is returned from Get instead of consulting accounts.
	// It lets a test drive an infrastructure failure (a locked SQLite file, an
	// I/O error) as distinct from the sql.ErrNoRows absence below.
	getErr error
}

func (s *spyStore) RecordInjectionFailure(_ context.Context, id string, reason string) error {
	s.injectCalls++
	s.injectID = id
	s.injectReason = reason
	return nil
}

func (s *spyStore) ClearInjectionFailure(_ context.Context, id string) error {
	s.clearCalls++
	s.clearID = id
	return nil
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
	if s.getErr != nil {
		return nil, s.getErr
	}
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

func (s *spyStore) RecordUsageProbe(_ context.Context, _ string, snap models.UsageSnapshot) error {
	s.usageCalls++
	s.usageSnap = snap
	if s.usageErr != nil {
		return s.usageErr
	}
	return nil
}

func (s *spyStore) MarkAccountSuspended(_ context.Context, id string, reason string) error {
	s.suspendCalls++
	s.suspendID = id
	s.suspendReason = reason
	return nil
}

// fakeCreds is an in-memory CredentialLoader.
type fakeCreds struct {
	blob  []byte
	err   error
	calls int
	saves int
}

// probeLockedCreds models the production credential store's shared,
// per-account compound-operation lock and successful-mutation generation.
type probeLockedCreds struct {
	fakeCreds
	mu           sync.Mutex
	lockAttempts chan struct{}
	generationMu sync.Mutex
	generation   uint64
}

func (f *probeLockedCreds) WithCredentialLock(_ string, fn func() error) error {
	if f.lockAttempts != nil {
		f.lockAttempts <- struct{}{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return fn()
}

func (f *probeLockedCreds) CredentialGeneration(string) uint64 {
	f.generationMu.Lock()
	defer f.generationMu.Unlock()
	return f.generation
}

func (f *probeLockedCreds) Save(accountID string, blob []byte) error {
	if err := f.fakeCreds.Save(accountID, blob); err != nil {
		return err
	}
	f.generationMu.Lock()
	f.generation++
	f.generationMu.Unlock()
	return nil
}

func (f *fakeCreds) Load(string) ([]byte, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.blob, nil
}

func (f *fakeCreds) Save(_ string, blob []byte) error {
	f.saves++
	f.blob = append([]byte(nil), blob...)
	return nil
}

// fakeRotationClient embeds the AgentRunnerClient interface (nil) and overrides
// only the two RPCs the materializer uses; any other call would panic, proving
// the resolver touches nothing else.
type fakeRotationClient struct {
	agent.AgentRunnerClient
	supports     bool
	capErr       error
	env          map[string]string
	files        []*bossanovav1.MaterializedFile
	homeDirKey   string
	matErr       error
	probe        *bossanovav1.RateLimitStatus
	probeErr     error
	probeHook    func(map[string]string) error
	capCalls     int
	matCalls     int
	probeCalls   int
	lastBlob     []byte
	lastProbeEnv map[string]string
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
	return &bossanovav1.MaterializeAccountResponse{
		Env:           f.env,
		Files:         f.files,
		HomeDirEnvKey: f.homeDirKey,
	}, nil
}

func (f *fakeRotationClient) ProbeRateLimit(_ context.Context, req *bossanovav1.ProbeRateLimitRequest) (*bossanovav1.ProbeRateLimitResponse, error) {
	f.probeCalls++
	f.lastProbeEnv = req.GetCredentialEnv()
	if f.probeErr != nil {
		return nil, f.probeErr
	}
	if f.probeHook != nil {
		if err := f.probeHook(req.GetCredentialEnv()); err != nil {
			return nil, err
		}
	}
	return &bossanovav1.ProbeRateLimitResponse{Status: f.probe}, nil
}

// blockingSuspensionProbe holds a usage probe after materialization so a test
// can race it against an explicit credential replacement.
type blockingSuspensionProbe struct {
	agent.AgentRunnerClient
	started chan struct{}
	release chan struct{}
}

func (p *blockingSuspensionProbe) MaterializeAccount(_ context.Context, _ *bossanovav1.MaterializeAccountRequest) (*bossanovav1.MaterializeAccountResponse, error) {
	return &bossanovav1.MaterializeAccountResponse{Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "old-token"}}, nil
}

func (p *blockingSuspensionProbe) ProbeRateLimit(context.Context, *bossanovav1.ProbeRateLimitRequest) (*bossanovav1.ProbeRateLimitResponse, error) {
	p.started <- struct{}{}
	<-p.release
	return nil, grpcstatus.Error(codes.PermissionDenied, "old credential suspended")
}

func newClaudeAccount() *models.Account {
	return &models.Account{
		ID:       "a1",
		Provider: models.AccountProvider("claude"),
		Status:   models.AccountStatusActive,
		Priority: 1,
	}
}

func newCodexAccount() *models.Account {
	return &models.Account{
		ID:       "codex-1",
		Provider: models.AccountProvider("codex"),
		Status:   models.AccountStatusActive,
		Priority: 1,
	}
}

func TestToMetaPlumbsUsage(t *testing.T) {
	fetched := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	reset5h := fetched.Add(3 * time.Hour)
	reset7d := fetched.Add(48 * time.Hour)
	acct := &models.Account{
		ID:       "a1",
		Provider: models.AccountProvider("claude"),
		Status:   models.AccountStatusActive,
		Health:   models.AccountHealthOK,
		Priority: 2,
		Usage: &models.UsageSnapshot{
			Util5h:    0.4,
			Util7d:    0.9,
			Reset5h:   &reset5h,
			Reset7d:   &reset7d,
			FetchedAt: &fetched,
		},
	}
	m := toMeta(acct)
	if m.Util5h != 0.4 || m.Util7d != 0.9 {
		t.Errorf("util: got (%v,%v), want (0.4,0.9)", m.Util5h, m.Util7d)
	}
	if m.UsageFetchedAt == nil || !m.UsageFetchedAt.Equal(fetched) {
		t.Errorf("UsageFetchedAt: got %v, want %v", m.UsageFetchedAt, fetched)
	}
	if m.Reset5h == nil || !m.Reset5h.Equal(reset5h) {
		t.Errorf("Reset5h: got %v, want %v", m.Reset5h, reset5h)
	}
	if m.Reset7d == nil || !m.Reset7d.Equal(reset7d) {
		t.Errorf("Reset7d: got %v, want %v", m.Reset7d, reset7d)
	}
}

func TestToMetaNilUsageYieldsZeroFields(t *testing.T) {
	m := toMeta(newClaudeAccount()) // no Usage
	if m.Util5h != 0 || m.Util7d != 0 {
		t.Errorf("util: got (%v,%v), want (0,0)", m.Util5h, m.Util7d)
	}
	if m.UsageFetchedAt != nil || m.Reset5h != nil || m.Reset7d != nil {
		t.Errorf("nil Usage must yield nil pointers, got %v/%v/%v", m.UsageFetchedAt, m.Reset5h, m.Reset7d)
	}
}

func newResolver(store *spyStore, client *fakeRotationClient, creds CredentialStore) *account.Resolver {
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

	if env, _ := r.Resolve(context.Background(), &models.Session{AgentName: "claude"}); env != nil {
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
	store := &spyStore{accounts: map[string]*models.Account{"a1": newClaudeAccount()}}
	client := &fakeRotationClient{supports: false}
	r := NewSpawnEnvResolver(newResolver(store, client, &fakeCreds{blob: []byte("blob")}), zerolog.Nop())

	sess := &models.Session{AgentName: "claude", AccountID: strptr("a1")}
	if env, _ := r.Resolve(context.Background(), sess); env != nil {
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
	store := &spyStore{accounts: map[string]*models.Account{"a1": newClaudeAccount()}}
	client := &fakeRotationClient{supports: true, env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "x"}}
	creds := &fakeCreds{blob: []byte("secret-blob")}
	r := NewSpawnEnvResolver(newResolver(store, client, creds), zerolog.Nop())

	sess := &models.Session{AgentName: "claude", AccountID: strptr("a1")}
	env, _ := r.Resolve(context.Background(), sess)
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

func TestSpawnEnvResolver_CodexUsesManagedProjectedHome(t *testing.T) {
	baseHome := t.TempDir()
	appDataDir := t.TempDir()
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"app_data_dir":`+strconv.Quote(appDataDir)+`}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	t.Setenv("CODEX_HOME", baseHome)

	for path, content := range map[string]string{
		"config.toml":                       "model = \"gpt-profiled\"\n",
		"sessions/2026/07/29/rollout.jsonl": `{"type":"session_meta"}` + "\n",
	} {
		fullPath := filepath.Join(baseHome, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
			t.Fatalf("mkdir projected state: %v", err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
			t.Fatalf("write projected state: %v", err)
		}
	}

	const credential = `{"tokens":{"access_token":"fixture-access","refresh_token":"fixture-refresh"}}`
	store := &spyStore{accounts: map[string]*models.Account{"codex-1": newCodexAccount()}}
	client := &fakeRotationClient{supports: true}
	creds := &fakeCreds{blob: []byte(credential)}
	var logs bytes.Buffer
	materializer := NewMaterializer(
		map[string]agent.AgentRunnerClient{"codex": client},
		store,
		creds,
		zerolog.New(&logs),
	)
	resolver := NewSpawnEnvResolver(
		account.NewResolver(NewRegistry(store), materializer, zerolog.Nop()),
		zerolog.Nop(),
	)

	env, _ := resolver.Resolve(context.Background(), &models.Session{
		AgentName: "codex",
		AccountID: strptr("codex-1"),
	})
	managedHome := env["CODEX_HOME"]
	if managedHome == "" || managedHome == baseHome {
		t.Fatalf("CODEX_HOME = %q, want distinct managed home", managedHome)
	}
	if want := filepath.Join(appDataDir, "accounts", "codex", "codex-1"); managedHome != want {
		t.Fatalf("CODEX_HOME = %q, want %q", managedHome, want)
	}
	for _, path := range []string{
		"config.toml",
		"sessions/2026/07/29/rollout.jsonl",
	} {
		if _, err := os.Stat(filepath.Join(managedHome, filepath.FromSlash(path))); err != nil {
			t.Fatalf("projected state %q unavailable: %v", path, err)
		}
	}

	authPath := filepath.Join(managedHome, "auth.json")
	info, err := os.Lstat(authPath)
	if err != nil {
		t.Fatalf("lstat managed auth: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("managed auth.json must be account-local, not a symlink")
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("managed auth.json mode = %#o, want 0600", got)
	}
	if got, err := os.ReadFile(authPath); err != nil {
		t.Fatalf("read managed auth: %v", err)
	} else if !bytes.Equal(got, []byte(credential)) {
		t.Fatal("managed auth.json does not contain the stored credential")
	}
	if client.matCalls != 0 {
		t.Fatalf("plugin MaterializeAccount calls = %d, want 0 for Codex", client.matCalls)
	}
	if logs.Bytes() != nil && bytes.Contains(logs.Bytes(), []byte("fixture-access")) {
		t.Fatal("credential bytes leaked to logs")
	}
}

// SupportsRotation degrades to (false, nil) for the two answers that are
// authoritative: a client that was never registered, and an Unimplemented
// plugin. Neither is a guess — the first means the plugin was not loaded at
// daemon start, the second is the runner saying "no rotation" in as many words.
func TestMaterializer_SupportsRotationDegrades(t *testing.T) {
	store := &spyStore{accounts: map[string]*models.Account{}}

	// Missing client for provider.
	m := NewMaterializer(map[string]agent.AgentRunnerClient{}, store, &fakeCreds{}, zerolog.Nop())
	if ok, err := m.SupportsRotation(context.Background(), "claude"); ok || err != nil {
		t.Errorf("missing client: got (%v,%v), want (false,nil)", ok, err)
	}

	client := &fakeRotationClient{capErr: grpcstatus.Error(codes.Unimplemented, "no rotation")}
	m = NewMaterializer(map[string]agent.AgentRunnerClient{"claude": client}, store, &fakeCreds{}, zerolog.Nop())
	if ok, err := m.SupportsRotation(context.Background(), "claude"); ok || err != nil {
		t.Errorf("unimplemented: got (%v,%v), want (false,nil)", ok, err)
	}
}

// Any other probe failure is undetermined and must propagate. codes.Unavailable
// is named explicitly because host_agent_proxy resolves a momentarily-absent
// plugin to exactly that: collapsed to (false, nil) it would reach the
// resolver's "the answer IS known" branch and tell the operator to rebind an
// account whose runner was merely restarting.
func TestMaterializer_SupportsRotationPropagatesUndeterminedProbe(t *testing.T) {
	store := &spyStore{accounts: map[string]*models.Account{}}

	for name, capErr := range map[string]error{
		"unavailable": grpcstatus.Error(codes.Unavailable, "plugin restarting"),
		"opaque":      errors.New("boom"),
	} {
		client := &fakeRotationClient{capErr: capErr}
		m := NewMaterializer(map[string]agent.AgentRunnerClient{"claude": client}, store, &fakeCreds{}, zerolog.Nop())
		ok, err := m.SupportsRotation(context.Background(), "claude")
		if ok {
			t.Errorf("%s: supports = true, want false", name)
		}
		if err == nil {
			t.Fatalf("%s: err = nil, want the probe failure to propagate", name)
		}
		if !errors.Is(err, capErr) {
			t.Errorf("%s: err = %v, want it to wrap the probe failure", name, err)
		}
	}
}

func TestMaterializer_RecordUsageProbeStoresMetadataOnly(t *testing.T) {
	reset5h := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Millisecond)
	reset7d := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Millisecond)
	store := &spyStore{accounts: map[string]*models.Account{"a1": newClaudeAccount()}}
	client := &fakeRotationClient{
		env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "token"},
		probe: &bossanovav1.RateLimitStatus{
			Status:   bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_WARNING,
			Util_5H:  0.4,
			Reset_5H: timestamppb.New(reset5h),
			Util_7D:  0.8,
			Reset_7D: timestamppb.New(reset7d),
			PlanTier: "max",
		},
	}
	creds := &fakeCreds{blob: []byte("secret-blob")}
	m := NewMaterializer(map[string]agent.AgentRunnerClient{"claude": client}, store, creds, zerolog.Nop())
	probeCache := m.(interface {
		RecordUsageProbe(context.Context, string) error
	})

	if err := probeCache.RecordUsageProbe(context.Background(), "a1"); err != nil {
		t.Fatalf("RecordUsageProbe: %v", err)
	}
	if client.matCalls != 1 || client.probeCalls != 1 || creds.calls != 1 {
		t.Fatalf("calls mat=%d probe=%d creds=%d, want 1/1/1", client.matCalls, client.probeCalls, creds.calls)
	}
	if client.lastProbeEnv["CLAUDE_CODE_OAUTH_TOKEN"] != "token" {
		t.Fatalf("probe env = %v, want materialized token env", client.lastProbeEnv)
	}
	if store.usageCalls != 1 {
		t.Fatalf("RecordUsageProbe store calls = %d, want 1", store.usageCalls)
	}
	got := store.usageSnap
	if got.Util5h != 0.4 || got.Util7d != 0.8 {
		t.Errorf("utils = %v/%v, want 0.4/0.8", got.Util5h, got.Util7d)
	}
	if got.Status != "RATE_LIMIT_PLAN_STATUS_WARNING" || got.PlanTier != "max" {
		t.Errorf("status/plan = %q/%q, want warning/max", got.Status, got.PlanTier)
	}
	if got.Reset5h == nil || !got.Reset5h.Equal(reset5h) {
		t.Errorf("reset5h = %v, want %v", got.Reset5h, reset5h)
	}
	if got.Reset7d == nil || !got.Reset7d.Equal(reset7d) {
		t.Errorf("reset7d = %v, want %v", got.Reset7d, reset7d)
	}
	if got.FetchedAt == nil {
		t.Fatal("FetchedAt = nil, want probe time")
	}
}

func TestMaterializer_ProbeUsageSnapshotDoesNotWriteCache(t *testing.T) {
	reset5h := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Millisecond)
	store := &spyStore{accounts: map[string]*models.Account{"a1": newClaudeAccount()}}
	client := &fakeRotationClient{
		env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "token"},
		probe: &bossanovav1.RateLimitStatus{
			Status:   bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_RATE_LIMITED,
			Util_5H:  1,
			Reset_5H: timestamppb.New(reset5h),
			PlanTier: "max",
		},
	}
	m := NewMaterializer(
		map[string]agent.AgentRunnerClient{"claude": client},
		store,
		&fakeCreds{blob: []byte("secret-blob")},
		zerolog.Nop(),
	)
	prober := m.(interface {
		ProbeUsageSnapshot(context.Context, string) (models.UsageSnapshot, error)
	})

	snap, err := prober.ProbeUsageSnapshot(context.Background(), "a1")
	if err != nil {
		t.Fatalf("ProbeUsageSnapshot: %v", err)
	}
	if client.matCalls != 1 || client.probeCalls != 1 {
		t.Fatalf("calls mat=%d probe=%d, want 1/1", client.matCalls, client.probeCalls)
	}
	if store.usageCalls != 0 {
		t.Fatalf("ProbeUsageSnapshot store calls = %d, want 0", store.usageCalls)
	}
	if snap.Status != "RATE_LIMIT_PLAN_STATUS_RATE_LIMITED" || snap.Util5h != 1 || snap.PlanTier != "max" {
		t.Fatalf("snapshot = %+v, want rate-limited max usage", snap)
	}
	if snap.Reset5h == nil || !snap.Reset5h.Equal(reset5h) {
		t.Fatalf("reset5h = %v, want %v", snap.Reset5h, reset5h)
	}
	if snap.FetchedAt == nil {
		t.Fatal("FetchedAt = nil, want probe time")
	}
}

func TestMaterializer_ProbeUsageSnapshotMaterializesHomeDirFiles(t *testing.T) {
	store := &spyStore{accounts: map[string]*models.Account{"codex-1": newCodexAccount()}}
	baseCodexHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseCodexHome, "session_index.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write seed session_index: %v", err)
	}
	seedRollout := filepath.Join(baseCodexHome, "sessions", "2026", "07", "07", "rollout-seed.jsonl")
	if err := os.MkdirAll(filepath.Dir(seedRollout), 0o700); err != nil {
		t.Fatalf("mkdir seed sessions: %v", err)
	}
	if err := os.WriteFile(seedRollout, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write seed rollout: %v", err)
	}
	t.Setenv("CODEX_HOME", baseCodexHome)

	client := &fakeRotationClient{
		files: []*bossanovav1.MaterializedFile{{
			RelativePath: "auth.json",
			Content:      []byte(`{"tokens":{"id_token":"secret"}}`),
			Mode:         0o600,
		}},
		homeDirKey: "CODEX_HOME",
		probe: &bossanovav1.RateLimitStatus{
			Status:  bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_UNSUPPORTED,
			Util_5H: 0,
		},
		probeHook: func(env map[string]string) error {
			home := env["CODEX_HOME"]
			if home == "" {
				t.Fatal("CODEX_HOME missing from probe env")
			}
			info, err := os.Stat(home)
			if err != nil {
				t.Fatalf("stat CODEX_HOME: %v", err)
			}
			if !info.IsDir() {
				t.Fatalf("CODEX_HOME %q is not a directory", home)
			}
			got, err := os.ReadFile(filepath.Join(home, "auth.json"))
			if err != nil {
				t.Fatalf("read materialized auth.json: %v", err)
			}
			if string(got) != `{"tokens":{"id_token":"secret"}}` {
				t.Fatalf("auth.json = %q, want materialized credential", string(got))
			}
			if _, err := os.Stat(filepath.Join(home, "session_index.jsonl")); err != nil {
				t.Fatalf("seeded session_index.jsonl missing: %v", err)
			}
			if _, err := os.Stat(filepath.Join(home, "sessions", "2026", "07", "07", "rollout-seed.jsonl")); err != nil {
				t.Fatalf("seeded rollout missing: %v", err)
			}
			return nil
		},
	}
	m := NewMaterializer(
		map[string]agent.AgentRunnerClient{"codex": client},
		store,
		&fakeCreds{blob: []byte("secret-blob")},
		zerolog.Nop(),
	)
	prober := m.(interface {
		ProbeUsageSnapshot(context.Context, string) (models.UsageSnapshot, error)
	})

	if _, err := prober.ProbeUsageSnapshot(context.Background(), "codex-1"); err != nil {
		t.Fatalf("ProbeUsageSnapshot: %v", err)
	}
	if client.matCalls != 1 || client.probeCalls != 1 {
		t.Fatalf("calls mat=%d probe=%d, want 1/1", client.matCalls, client.probeCalls)
	}
}

func TestMaterializer_RecordUsageProbeReturnsStoreError(t *testing.T) {
	store := &spyStore{
		accounts: map[string]*models.Account{"a1": newClaudeAccount()},
		usageErr: errors.New("store down"),
	}
	client := &fakeRotationClient{env: map[string]string{}, probe: &bossanovav1.RateLimitStatus{}}
	m := NewMaterializer(map[string]agent.AgentRunnerClient{"claude": client}, store, &fakeCreds{blob: []byte("secret")}, zerolog.Nop())
	probeCache := m.(interface {
		RecordUsageProbe(context.Context, string) error
	})

	if err := probeCache.RecordUsageProbe(context.Background(), "a1"); err == nil {
		t.Fatal("RecordUsageProbe error = nil, want store error for caller to log and ignore")
	}
}

func TestMaterializer_RecordUsageProbeSuspensionFailsHealth(t *testing.T) {
	const reason = "account suspended: organization disabled Claude subscription access"
	store := &spyStore{accounts: map[string]*models.Account{"a1": newClaudeAccount()}}
	client := &fakeRotationClient{
		env:      map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "token"},
		probeErr: grpcstatus.Error(codes.PermissionDenied, reason),
	}
	m := NewMaterializer(map[string]agent.AgentRunnerClient{"claude": client}, store, &fakeCreds{blob: []byte("secret")}, zerolog.Nop())
	probeCache := m.(interface {
		RecordUsageProbe(context.Context, string) error
	})

	// A confirmed suspension is handled (health failed), not surfaced as an error
	// for the caller to merely log — so the account is proactively sidelined.
	if err := probeCache.RecordUsageProbe(context.Background(), "a1"); err != nil {
		t.Fatalf("RecordUsageProbe err = %v, want nil (suspension handled)", err)
	}
	if store.suspendCalls != 1 || store.suspendID != "a1" {
		t.Fatalf("MarkAccountSuspended calls=%d id=%q, want 1/a1", store.suspendCalls, store.suspendID)
	}
	if store.suspendReason != reason {
		t.Errorf("suspend reason = %q, want %q", store.suspendReason, reason)
	}
	if store.usageCalls != 0 {
		t.Errorf("usageCalls = %d, want 0 (no snapshot cached on suspension)", store.usageCalls)
	}
}

func TestMaterializer_RecordUsageProbeDiscardsStaleSuspensionWithoutBlockingCredentialRefresh(t *testing.T) {
	store := &spyStore{accounts: map[string]*models.Account{"a1": newClaudeAccount()}}
	creds := &probeLockedCreds{
		fakeCreds:    fakeCreds{blob: []byte("old-token")},
		lockAttempts: make(chan struct{}, 3),
	}
	probe := &blockingSuspensionProbe{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	m := NewMaterializer(map[string]agent.AgentRunnerClient{"claude": probe}, store, creds, zerolog.Nop())
	probeCache := m.(interface {
		RecordUsageProbe(context.Context, string) error
	})

	probeDone := make(chan error, 1)
	go func() { probeDone <- probeCache.RecordUsageProbe(context.Background(), "a1") }()
	select {
	case <-probe.started:
	case <-time.After(5 * time.Second):
		t.Fatal("usage probe did not start")
	}
	select {
	case <-creds.lockAttempts:
	case <-time.After(5 * time.Second):
		t.Fatal("usage probe did not capture the credential generation")
	}

	refreshDone := make(chan error, 1)
	go func() {
		refreshDone <- creds.WithCredentialLock("a1", func() error {
			return creds.Save("a1", []byte("new-token"))
		})
	}()
	select {
	case <-creds.lockAttempts:
	case <-time.After(5 * time.Second):
		t.Fatal("credential refresh did not attempt the shared credential lock")
	}
	select {
	case err := <-refreshDone:
		if err != nil {
			t.Fatalf("refresh credential: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("credential refresh waited for the slow usage probe")
	}

	close(probe.release)
	select {
	case err := <-probeDone:
		if err != nil {
			t.Fatalf("RecordUsageProbe: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("usage probe did not finish")
	}
	if store.suspendCalls != 0 {
		t.Fatalf("suspension writes = %d, want 0 for the replaced credential", store.suspendCalls)
	}
	if got := string(creds.blob); got != "new-token" {
		t.Errorf("stored credential = %q, want refreshed credential", got)
	}
}

func TestMaterializer_ProbeUsageSnapshotPreservesGRPCStatusVerbatim(t *testing.T) {
	const reason = "account suspended: organization disabled Claude subscription access"
	store := &spyStore{accounts: map[string]*models.Account{"a1": newClaudeAccount()}}
	client := &fakeRotationClient{
		env:      map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "token"},
		probeErr: grpcstatus.Error(codes.PermissionDenied, reason),
	}
	m := NewMaterializer(map[string]agent.AgentRunnerClient{"claude": client}, store, &fakeCreds{blob: []byte("secret")}, zerolog.Nop())
	prober := m.(interface {
		ProbeUsageSnapshot(context.Context, string) (models.UsageSnapshot, error)
	})

	// The plugin's gRPC-status error must surface verbatim so main.go's rotation
	// refresh can classify it by code — NOT re-wrapped by fmt.Errorf("plugin
	// ProbeRateLimit: %w", err), which would still unwrap the code but pollute the
	// operator-facing reason message with a wrapper prefix.
	_, err := prober.ProbeUsageSnapshot(context.Background(), "a1")
	if err == nil {
		t.Fatal("ProbeUsageSnapshot err = nil, want a PermissionDenied status error")
	}
	if got := grpcstatus.Code(err); got != codes.PermissionDenied {
		t.Fatalf("grpcstatus.Code(err) = %v, want PermissionDenied", got)
	}
	if got := grpcstatus.Convert(err).Message(); got != reason {
		t.Fatalf("status message = %q, want %q (verbatim, not wrapped)", got, reason)
	}
}

// materializationRemover is the removal capability the daemon type-asserts out
// of the account.Materializer returned by NewMaterializer. It is deliberately
// NOT part of account.Materializer (removal is not a spawn concern), so the
// tests assert it the same way cmd/main.go does: an optional type assertion.
type materializationRemover interface {
	RemoveMaterialization(ctx context.Context, provider, accountID string) error
}

// newMaterializedCodexAdapter builds a materializer whose codex leg writes under
// a temp app-data dir (never the real one, never the real keyring), materializes
// codex-1, and returns the materializer plus the on-disk account dir.
func newMaterializedCodexAdapter(t *testing.T) (account.Materializer, string) {
	t.Helper()
	appDataDir := t.TempDir()
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"app_data_dir":`+strconv.Quote(appDataDir)+`}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	t.Setenv("CODEX_HOME", t.TempDir())

	acct := newCodexAccount()
	store := &spyStore{accounts: map[string]*models.Account{acct.ID: acct}}
	m := NewMaterializer(
		map[string]agent.AgentRunnerClient{},
		store,
		&fakeCreds{blob: []byte(`{"tokens":{"access_token":"fixture-access"}}`)},
		zerolog.Nop(),
	)
	if _, err := m.MaterializeAccount(context.Background(), acct.ID); err != nil {
		t.Fatalf("MaterializeAccount: %v", err)
	}
	dir := filepath.Join(appDataDir, "accounts", "codex", acct.ID)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("materialized codex dir missing before removal: %v", err)
	}
	return m, dir
}

func removerFor(t *testing.T, m account.Materializer) materializationRemover {
	t.Helper()
	remover, ok := m.(materializationRemover)
	if !ok {
		t.Fatalf("materializer %T does not expose RemoveMaterialization", m)
	}
	return remover
}

// RemoveMaterialization delegates to the codex materializer, so the on-disk
// account dir (and the plaintext auth.json in it) is actually gone afterwards.
func TestMaterializer_RemoveMaterializationPurgesCodexDir(t *testing.T) {
	m, dir := newMaterializedCodexAdapter(t)

	if err := removerFor(t, m).RemoveMaterialization(context.Background(), "codex", newCodexAccount().ID); err != nil {
		t.Fatalf("RemoveMaterialization: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("codex account dir still present after removal: %v", err)
	}
}

// Any non-codex provider is a no-op: no error, and nothing on disk is touched.
// An unknown provider must not reach credmaterialize.RemoveAccount, which
// rejects it with "unknown provider".
func TestMaterializer_RemoveMaterializationNoOpsForOtherProviders(t *testing.T) {
	for _, provider := range []string{"claude", "opencode-not-a-provider"} {
		t.Run(provider, func(t *testing.T) {
			m, dir := newMaterializedCodexAdapter(t)

			if err := removerFor(t, m).RemoveMaterialization(context.Background(), provider, newCodexAccount().ID); err != nil {
				t.Fatalf("RemoveMaterialization(%q) = %v, want nil (no-op)", provider, err)
			}
			if _, err := os.Stat(dir); err != nil {
				t.Fatalf("provider %q purged the codex dir; want an untouched no-op: %v", provider, err)
			}
		})
	}
}

// Without a credential store there is no codex materializer at all, so removal
// degrades to a nil-error no-op rather than panicking.
func TestMaterializer_RemoveMaterializationNoOpsWithoutCodexLeg(t *testing.T) {
	store := &spyStore{accounts: map[string]*models.Account{"codex-1": newCodexAccount()}}
	m := NewMaterializer(map[string]agent.AgentRunnerClient{}, store, nil, zerolog.Nop())

	if err := removerFor(t, m).RemoveMaterialization(context.Background(), "codex", "codex-1"); err != nil {
		t.Fatalf("RemoveMaterialization with nil codex leg = %v, want nil (no-op)", err)
	}
}

func TestLifecycleMaterializer_MaterializesAccount(t *testing.T) {
	store := &spyStore{accounts: map[string]*models.Account{"a1": newClaudeAccount()}}
	client := &fakeRotationClient{supports: true, env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "x"}}
	creds := &fakeCreds{blob: []byte("secret-blob")}
	m := NewLifecycleMaterializer(NewMaterializer(
		map[string]agent.AgentRunnerClient{"claude": client},
		store,
		creds,
		zerolog.Nop(),
	))

	env, err := m.Materialize(context.Background(), newClaudeAccount())
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if env["CLAUDE_CODE_OAUTH_TOKEN"] != "x" {
		t.Fatalf("Materialize env = %v, want token", env)
	}
	if client.matCalls != 1 || creds.calls != 1 {
		t.Fatalf("calls mat=%d creds=%d, want 1/1", client.matCalls, creds.calls)
	}
}

func TestRotationBindingResolver_CurrentBinding(t *testing.T) {
	tests := []struct {
		name     string
		sess     *models.Session
		store    *spyStore
		client   *fakeRotationClient
		wantBind bool
		want     session.RotationBinding
	}{
		{
			name:     "unbound account zero",
			sess:     &models.Session{AgentName: "claude"},
			store:    &spyStore{accounts: map[string]*models.Account{"a1": newClaudeAccount()}},
			client:   &fakeRotationClient{supports: true},
			wantBind: false,
		},
		{
			name:     "missing account degrades unbound",
			sess:     &models.Session{AgentName: "claude", AccountID: strptr("missing")},
			store:    &spyStore{accounts: map[string]*models.Account{}},
			client:   &fakeRotationClient{supports: true},
			wantBind: false,
		},
		{
			name:     "bound rotation capable",
			sess:     &models.Session{AgentName: "claude", AccountID: strptr("a1")},
			store:    &spyStore{accounts: map[string]*models.Account{"a1": newClaudeAccount()}},
			client:   &fakeRotationClient{supports: true},
			wantBind: true,
			want:     session.RotationBinding{CappedAccountID: "a1", Provider: "claude", RotationCapable: true},
		},
		{
			name:     "bound status only",
			sess:     &models.Session{AgentName: "claude", AccountID: strptr("a1")},
			store:    &spyStore{accounts: map[string]*models.Account{"a1": newClaudeAccount()}},
			client:   &fakeRotationClient{supports: false},
			wantBind: true,
			want:     session.RotationBinding{CappedAccountID: "a1", Provider: "claude", RotationCapable: false},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRotationBindingResolver(
				NewRegistry(tc.store),
				NewMaterializer(map[string]agent.AgentRunnerClient{"claude": tc.client}, tc.store, &fakeCreds{}, zerolog.Nop()),
			)
			got, bound, err := r.CurrentBinding(context.Background(), tc.sess)
			if err != nil {
				t.Fatalf("CurrentBinding: %v", err)
			}
			if bound != tc.wantBind {
				t.Fatalf("bound = %v, want %v", bound, tc.wantBind)
			}
			if got != tc.want {
				t.Fatalf("binding = %+v, want %+v", got, tc.want)
			}
		})
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

// throttleErr builds the exact error shape the claude plugin returns for a
// throttled usage endpoint, so these tests pin the real plugin→daemon contract
// rather than a convenient stand-in.
func throttleErr(t *testing.T, retryAfter time.Duration) error {
	t.Helper()
	st := grpcstatus.New(codes.ResourceExhausted, "usage_probe_throttled")
	if retryAfter <= 0 {
		return st.Err()
	}
	withDetails, err := st.WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(retryAfter)})
	if err != nil {
		t.Fatalf("attach RetryInfo: %v", err)
	}
	return withDetails.Err()
}

// TestIsProbeThrottledAndRetryAfter pins the read-only throttle classifiers.
// Note there is deliberately NO store-writing MarkThrottled... counterpart to
// MarkSuspendedIfConfirmed: a polling throttle says nothing about the account's
// quota, so keeping this surface read-only is what stops a future caller
// reaching for a cooldown and re-creating BOS-584.
func TestIsProbeThrottledAndRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name          string
		err           error
		wantThrottled bool
		wantDelay     time.Duration
		wantOK        bool
	}{
		{
			name:          "throttle with retry info",
			err:           throttleErr(t, 2*time.Minute),
			wantThrottled: true,
			wantDelay:     2 * time.Minute,
			wantOK:        true,
		},
		{
			name:          "throttle without retry info",
			err:           throttleErr(t, 0),
			wantThrottled: true,
		},
		{
			name: "suspension is not a throttle",
			err:  grpcstatus.Error(codes.PermissionDenied, "account suspended"),
		},
		{
			name: "auth invalidation is not a throttle",
			err:  grpcstatus.Error(codes.Unauthenticated, "auth_invalidated"),
		},
		{
			name: "plain non-grpc error",
			err:  errors.New("dial tcp: connection refused"),
		},
		{
			name: "nil",
			err:  nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsProbeThrottled(tc.err); got != tc.wantThrottled {
				t.Fatalf("IsProbeThrottled = %v, want %v", got, tc.wantThrottled)
			}
			delay, ok := ProbeRetryAfter(tc.err)
			if ok != tc.wantOK {
				t.Fatalf("ProbeRetryAfter ok = %v, want %v", ok, tc.wantOK)
			}
			if delay != tc.wantDelay {
				t.Fatalf("ProbeRetryAfter delay = %v, want %v", delay, tc.wantDelay)
			}
		})
	}
}

// TestProbeThrottleAndSuspensionAreDisjoint pins that the two classifiers can
// never both fire. They drive opposite reactions — a throttle is transient and
// must leave health untouched, a suspension is permanent and fails health — so
// an overlap would let a polling throttle bench a healthy account.
func TestProbeThrottleAndSuspensionAreDisjoint(t *testing.T) {
	throttle := throttleErr(t, time.Minute)
	if !IsProbeThrottled(throttle) || IsSuspension(throttle) {
		t.Fatalf("throttle: IsProbeThrottled=%v IsSuspension=%v, want true/false",
			IsProbeThrottled(throttle), IsSuspension(throttle))
	}
	suspension := grpcstatus.Error(codes.PermissionDenied, "account suspended")
	if IsProbeThrottled(suspension) || !IsSuspension(suspension) {
		t.Fatalf("suspension: IsProbeThrottled=%v IsSuspension=%v, want false/true",
			IsProbeThrottled(suspension), IsSuspension(suspension))
	}
}

// TestMarkSuspendedIfConfirmedIgnoresThrottle is the BOS-584 guard from the
// other direction: the one store-writing reaction point must not fire on a
// throttle, so a rate-limited poller can never fail a healthy account's health.
func TestMarkSuspendedIfConfirmedIgnoresThrottle(t *testing.T) {
	store := &spySuspender{}
	handled, _, err := MarkSuspendedIfConfirmed(context.Background(), store, "acct", throttleErr(t, time.Minute))
	if err != nil {
		t.Fatalf("MarkSuspendedIfConfirmed err = %v, want nil", err)
	}
	if handled {
		t.Fatal("handled = true, want false: a throttle is not a suspension")
	}
	if store.calls != 0 {
		t.Fatalf("MarkAccountSuspended calls = %d, want 0", store.calls)
	}
}

type spySuspender struct{ calls int }

func (s *spySuspender) MarkAccountSuspended(context.Context, string, string) error {
	s.calls++
	return nil
}

// TestRegistryAdapterInjectionHealthPassThrough proves the wiring the resolver's
// optional seam depends on: the adapter must actually satisfy
// account.injectionHealthRecorder and forward to the store, or the resolver's
// type assertion silently fails and BOS-973's whole observability path is dead
// code in production while the unit tests still pass on their fake.
func TestRegistryAdapterInjectionHealthPassThrough(t *testing.T) {
	store := &spyStore{accounts: map[string]*models.Account{
		"a1": {ID: "a1", Provider: models.AccountProviderCodex, Health: models.AccountHealthOK},
	}}
	reg := NewRegistry(store)

	rec, ok := reg.(interface {
		RecordInjectionFailure(ctx context.Context, id string, reason string) error
		ClearInjectionFailure(ctx context.Context, id string) error
	})
	if !ok {
		t.Fatal("registry adapter does not satisfy the injection-health seam the resolver asserts")
	}

	if err := rec.RecordInjectionFailure(context.Background(), "a1", "project codex base home: boom"); err != nil {
		t.Fatalf("RecordInjectionFailure: %v", err)
	}
	if store.injectCalls != 1 || store.injectID != "a1" || store.injectReason != "project codex base home: boom" {
		t.Fatalf("store record = (%d, %q, %q), want (1, \"a1\", the materialize error)",
			store.injectCalls, store.injectID, store.injectReason)
	}

	if err := rec.ClearInjectionFailure(context.Background(), "a1"); err != nil {
		t.Fatalf("ClearInjectionFailure: %v", err)
	}
	if store.clearCalls != 1 || store.clearID != "a1" {
		t.Fatalf("store clear = (%d, %q), want (1, \"a1\")", store.clearCalls, store.clearID)
	}
}

// A nil store must stay a no-op, matching every other registryAdapter method.
func TestRegistryAdapterInjectionHealthNilStore(t *testing.T) {
	reg := NewRegistry(nil)
	rec, ok := reg.(interface {
		RecordInjectionFailure(ctx context.Context, id string, reason string) error
		ClearInjectionFailure(ctx context.Context, id string) error
	})
	if !ok {
		t.Fatal("registry adapter does not satisfy the injection-health seam")
	}
	if err := rec.RecordInjectionFailure(context.Background(), "a1", "boom"); err != nil {
		t.Errorf("RecordInjectionFailure on nil store: %v", err)
	}
	if err := rec.ClearInjectionFailure(context.Background(), "a1"); err != nil {
		t.Errorf("ClearInjectionFailure on nil store: %v", err)
	}
}

// TestSpawnEnvResolver_MaterializeFailureIsLoudAndRecorded is the BOS-973
// end-to-end guard, run through the REAL registry adapter and resolver rather
// than a stub seam. It pins all three halves of the fix at the one site whose
// WRN line hid a month of ambient-login spawns:
//
//  1. the env still degrades to nil (the spawn policy is deliberately unchanged);
//  2. the log line is ERROR — not WARN — and names the account id and provider,
//     so the downgrade is legible in the daemon log; and
//  3. the failure is recorded durably on the account row, which is what the TUI
//     Accounts list, `boss account ls`, and rotation eligibility all read.
//
// (3) is the part a stubbed resolver test cannot prove: it only works if the
// registry adapter actually satisfies the resolver's optional type assertion.
func TestSpawnEnvResolver_MaterializeFailureIsLoudAndRecorded(t *testing.T) {
	var logs bytes.Buffer
	store := &spyStore{accounts: map[string]*models.Account{"a1": newClaudeAccount()}}
	client := &fakeRotationClient{supports: true, matErr: errors.New("project codex base home: boom")}
	r := NewSpawnEnvResolver(newResolver(store, client, &fakeCreds{blob: []byte("blob")}), zerolog.New(&logs))

	sess := &models.Session{AgentName: "claude", AccountID: strptr("a1")}
	if env, _ := r.Resolve(context.Background(), sess); env != nil {
		t.Fatalf("env = %v, want nil (the degrade policy is unchanged)", env)
	}

	out := logs.String()
	if !strings.Contains(out, `"level":"error"`) {
		t.Fatalf("degrade must log at ERROR, not WARN:\n%s", out)
	}
	if !strings.Contains(out, `"account_id":"a1"`) {
		t.Fatalf("degrade log must name the account id:\n%s", out)
	}
	if !strings.Contains(out, `"provider":"claude"`) {
		t.Fatalf("degrade log must name the provider:\n%s", out)
	}

	if store.injectCalls != 1 {
		t.Fatalf("store recorded %d injection failures, want exactly 1", store.injectCalls)
	}
	if store.injectID != "a1" {
		t.Errorf("recorded against %q, want %q", store.injectID, "a1")
	}
	if !strings.Contains(store.injectReason, "project codex base home: boom") {
		t.Errorf("recorded reason = %q, want the materialize error", store.injectReason)
	}
}

// The mirror case: a successful spawn withdraws any recorded injection failure,
// again through the real adapter chain, so a transient failure self-heals.
func TestSpawnEnvResolver_SuccessClearsInjectionFailure(t *testing.T) {
	store := &spyStore{accounts: map[string]*models.Account{"a1": newClaudeAccount()}}
	client := &fakeRotationClient{supports: true, env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "x"}}
	r := NewSpawnEnvResolver(newResolver(store, client, &fakeCreds{blob: []byte("blob")}), zerolog.Nop())

	sess := &models.Session{AgentName: "claude", AccountID: strptr("a1")}
	if env, _ := r.Resolve(context.Background(), sess); env["CLAUDE_CODE_OAUTH_TOKEN"] != "x" {
		t.Fatalf("env = %v, want the materialized overlay", env)
	}
	if store.clearCalls != 1 {
		t.Fatalf("store clear calls = %d, want exactly 1", store.clearCalls)
	}
	if store.injectCalls != 0 {
		t.Errorf("store recorded %d injection failures on success, want 0", store.injectCalls)
	}
}

// BOS-1142: the unwired-resolver guard must read the binding before it decides.
// An UNBOUND session keeps degrading to account 0 (the plan's degrade-site table
// requires it), but a session BOUND to a managed account must not silently spawn
// on the ambient CLI login just because this daemon was assembled without a
// spawn-env resolver. The refusal is classified undetermined — wiring absent is
// "could not evaluate", never "credential is bad".
func TestSpawnEnvResolver_UnwiredResolverFailsClosedOnlyWhenBound(t *testing.T) {
	r := NewSpawnEnvResolver(nil, zerolog.Nop())

	t.Run("bound", func(t *testing.T) {
		id := "a1"
		env, err := r.Resolve(context.Background(), &models.Session{AgentName: "codex", AccountID: &id})
		if err == nil {
			t.Fatalf("bound session with unwired resolver: err = nil, want refusal")
		}
		if env != nil {
			t.Errorf("env = %v, want nil", env)
		}
		if !account.IsInjectionUndetermined(err) {
			t.Fatalf("outcome = %q, want %q", account.InjectionOutcomeOf(err), account.InjectionOutcomeUndetermined)
		}
		if account.IsInjectionInvalid(err) {
			t.Error("unwired wiring must not be reported as an invalid credential")
		}
		ie, ok := account.AsInjectionError(err)
		if !ok {
			t.Fatal("refusal is not a typed *account.InjectionError")
		}
		if ie.AccountID != id {
			t.Errorf("AccountID = %q, want %q", ie.AccountID, id)
		}
	})

	t.Run("unbound", func(t *testing.T) {
		env, err := r.Resolve(context.Background(), &models.Session{AgentName: "codex"})
		if err != nil {
			t.Fatalf("unbound session with unwired resolver: err = %v, want nil", err)
		}
		if env != nil {
			t.Errorf("env = %v, want nil", env)
		}
	})

	t.Run("explicit account 0", func(t *testing.T) {
		id := account.SystemDefaultAccountID
		env, err := r.Resolve(context.Background(), &models.Session{AgentName: "codex", AccountID: &id})
		if err != nil {
			t.Fatalf("account-0 session with unwired resolver: err = %v, want nil", err)
		}
		if env != nil {
			t.Errorf("env = %v, want nil", env)
		}
	})
}

// This is the half of the BOS-1142 classification that internal/account cannot
// test: deciding a materialize failure never reached a verdict means reading a
// gRPC status code, and that package is grpc-free by design. So the wiring layer
// marks such causes with account.ErrInjectionUndetermined and the resolver reads
// the sentinel back (see TestResolveSpawnEnvUndeterminedMaterializeFailure).
//
// codes.Unavailable is the case that motivates it: agent runners resolve
// per-call through host_agent_proxy, so a plugin restarting between two RPCs
// surfaces exactly that code. Reporting it as "invalid" tells an operator to
// re-authenticate a credential no plugin ever looked at.
func TestMaterializeAccountMarksTransportFailuresUndetermined(t *testing.T) {
	tests := []struct {
		name             string
		matErr           error
		wantUndetermined bool
	}{
		{
			name:             "plugin restarting between calls",
			matErr:           grpcstatus.Error(codes.Unavailable, `agent "claude" is not currently loaded (plugin restarting?)`),
			wantUndetermined: true,
		},
		{
			name:             "rpc deadline expired",
			matErr:           grpcstatus.Error(codes.DeadlineExceeded, "context deadline exceeded"),
			wantUndetermined: true,
		},
		{
			name:             "context cancelled beneath the rpc",
			matErr:           context.Canceled,
			wantUndetermined: true,
		},
		{
			// The same cancellation in its OTHER shape. A gRPC client reports a
			// cancelled call as a status error that does not unwrap to the
			// context.Canceled sentinel, so the errors.Is arm above cannot see
			// it and the status arm has to carry the code.
			name:             "cancellation arriving as a grpc status",
			matErr:           grpcstatus.Error(codes.Canceled, "context canceled"),
			wantUndetermined: true,
		},
		{
			// Negative controls. A plugin that answered and rejected the
			// credential HAS reached a verdict, and must keep prompting the
			// operator to re-authenticate; marking these undetermined would
			// reopen the opposite half of BOS-1142.
			name:             "provider rejected the credential",
			matErr:           grpcstatus.Error(codes.PermissionDenied, "stored credential is no longer valid"),
			wantUndetermined: false,
		},
		{
			name:             "plugin returned a plain error",
			matErr:           errors.New("malformed credential blob"),
			wantUndetermined: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &spyStore{accounts: map[string]*models.Account{"a1": newClaudeAccount()}}
			client := &fakeRotationClient{matErr: tc.matErr}
			m := NewMaterializer(map[string]agent.AgentRunnerClient{"claude": client}, store, &fakeCreds{blob: []byte("secret")}, zerolog.Nop())

			env, err := m.MaterializeAccount(context.Background(), "a1")
			if err == nil {
				t.Fatal("MaterializeAccount: want an error, got nil")
			}
			if env != nil {
				t.Fatalf("MaterializeAccount returned env %v alongside an error; must be nil", env)
			}
			if got := errors.Is(err, account.ErrInjectionUndetermined); got != tc.wantUndetermined {
				t.Errorf("errors.Is(err, account.ErrInjectionUndetermined) = %v, want %v (err: %v)", got, tc.wantUndetermined, err)
			}
			// The original cause must stay reachable: callers match on
			// context cancellation and on gRPC codes through this error.
			if !errors.Is(err, tc.matErr) && grpcstatus.Code(err) != grpcstatus.Code(tc.matErr) {
				t.Errorf("the underlying cause did not survive wrapping (err: %v)", err)
			}
			// The credential blob must never reach an error string.
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("error text leaked the credential blob: %v", err)
			}
		})
	}
}

// TestMaterializeAccountClassifiesLookupAndKeyringFailures pins the two legs
// that run BEFORE any provider is consulted: reading the account row and
// loading the credential blob from the keyring.
//
// Neither of these ever asks the provider anything, so only a genuine ABSENCE
// is a verdict about the credential: no account row (sql.ErrNoRows) or no
// stored blob (accountcred.ErrCredentialNotFound) both mean re-binding or
// re-authenticating is the right operator action, and both stay invalid. A
// locked SQLite file or a keyring that cannot be opened is infrastructure
// noise; labelling it invalid tells the operator to re-authenticate a
// credential nothing ever read, which is the BOS-881 collapse.
func TestMaterializeAccountClassifiesLookupAndKeyringFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		storeErr         error
		credsErr         error
		wantUndetermined bool
	}{
		{
			// Definite absence. SQLiteAccountStore.Get returns sql.ErrNoRows
			// verbatim from its row scan for a row that is not there.
			name:             "account row is genuinely absent",
			storeErr:         sql.ErrNoRows,
			wantUndetermined: false,
		},
		{
			// Same verdict once a caller has added context around it.
			name:             "absent account row wrapped in context",
			storeErr:         fmt.Errorf("scan account %q: %w", "a1", sql.ErrNoRows),
			wantUndetermined: false,
		},
		{
			name:             "account store is locked",
			storeErr:         errors.New("database is locked"),
			wantUndetermined: true,
		},
		{
			name:             "account store i/o failure",
			storeErr:         fmt.Errorf("query account: %w", io.ErrUnexpectedEOF),
			wantUndetermined: true,
		},
		{
			name:             "lookup cancelled underneath us",
			storeErr:         context.Canceled,
			wantUndetermined: true,
		},
		{
			// Definite absence on the credential side: the keyring store maps
			// keyring.ErrKeyNotFound to this sentinel and nothing else.
			name:             "credential is genuinely absent",
			credsErr:         accountcred.ErrCredentialNotFound,
			wantUndetermined: false,
		},
		{
			name:             "absent credential wrapped in context",
			credsErr:         fmt.Errorf("load account credential: %w", accountcred.ErrCredentialNotFound),
			wantUndetermined: false,
		},
		{
			// The store's own "open keyring: %w" shape: the keyring could not
			// be opened or unlocked, so the blob inside was never read.
			name:             "keyring cannot be opened",
			credsErr:         fmt.Errorf("open keyring: %w", errors.New("dial unix /run/dbus: connection refused")),
			wantUndetermined: true,
		},
		{
			name:             "keyring read failed",
			credsErr:         errors.New("load account credential: keyring is locked"),
			wantUndetermined: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := &spyStore{
				accounts: map[string]*models.Account{"a1": newClaudeAccount()},
				getErr:   tc.storeErr,
			}
			creds := &fakeCreds{blob: []byte("secret"), err: tc.credsErr}
			// A healthy, loaded plugin, so nothing else can account for the
			// refusal: the only failure in play is the one under test.
			client := &fakeRotationClient{supports: true, env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "x"}}
			m := NewMaterializer(map[string]agent.AgentRunnerClient{"claude": client}, store, creds, zerolog.Nop())

			env, err := m.MaterializeAccount(context.Background(), "a1")
			if err == nil {
				t.Fatalf("MaterializeAccount must refuse, got env %v and a nil error", env)
			}
			if env != nil {
				t.Errorf("MaterializeAccount returned env %v alongside an error; must be nil", env)
			}
			if got := errors.Is(err, account.ErrInjectionUndetermined); got != tc.wantUndetermined {
				t.Errorf("errors.Is(err, account.ErrInjectionUndetermined) = %v, want %v (err: %v)", got, tc.wantUndetermined, err)
			}
			// The original cause must stay reachable through the wrap.
			cause := tc.storeErr
			if cause == nil {
				cause = tc.credsErr
			}
			if !errors.Is(err, cause) {
				t.Errorf("the underlying cause did not survive wrapping (err: %v, want cause: %v)", err, cause)
			}
			// A refusal on the credential leg must never have reached the
			// plugin, and no refusal may leak the blob.
			if tc.credsErr == nil && tc.storeErr != nil && client.matCalls != 0 {
				t.Errorf("a failed account lookup still called the plugin %d time(s)", client.matCalls)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("error text leaked the credential blob: %v", err)
			}
		})
	}

	// Negative control for the fail-safe direction taken above: enumerating
	// absence and defaulting everything else to undetermined must not turn a
	// rendered provider verdict undetermined too.
	t.Run("loaded plugin rejecting the credential stays invalid", func(t *testing.T) {
		t.Parallel()

		store := &spyStore{accounts: map[string]*models.Account{"a1": newClaudeAccount()}}
		client := &fakeRotationClient{
			supports: true,
			matErr:   grpcstatus.Error(codes.PermissionDenied, "stored credential is no longer valid"),
		}
		m := NewMaterializer(map[string]agent.AgentRunnerClient{"claude": client}, store, &fakeCreds{blob: []byte("secret")}, zerolog.Nop())

		env, err := m.MaterializeAccount(context.Background(), "a1")
		if err == nil {
			t.Fatalf("MaterializeAccount must refuse, got env %v", env)
		}
		if errors.Is(err, account.ErrInjectionUndetermined) {
			t.Errorf("a rendered provider verdict must stay invalid: %v", err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("error text leaked the credential blob: %v", err)
		}
	})
}

// TestIsUndeterminedMaterializeErrorCancellation pins the predicate itself on
// the two shapes a cancellation can take. errors.Is(err, context.Canceled)
// covers only the sentinel; a gRPC client hands back codes.Canceled as a status
// error, which does not unwrap to that sentinel, so without the status case a
// merely-cancelled request would be reported to the operator as an invalid
// credential.
func TestIsUndeterminedMaterializeErrorCancellation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil is not undetermined", err: nil, want: false},
		{name: "context.Canceled sentinel", err: context.Canceled, want: true},
		{name: "cancellation as a grpc status", err: grpcstatus.Error(codes.Canceled, "context canceled"), want: true},
		{
			name: "cancellation status wrapped in context",
			err:  fmt.Errorf("plugin MaterializeAccount: %w", grpcstatus.Error(codes.Canceled, "context canceled")),
			want: true,
		},
		{name: "deadline as a grpc status", err: grpcstatus.Error(codes.DeadlineExceeded, "deadline"), want: true},
		{name: "plugin restarting", err: grpcstatus.Error(codes.Unavailable, "not loaded"), want: true},
		// Negative controls: a rendered verdict is not a cancellation.
		{name: "provider rejected the credential", err: grpcstatus.Error(codes.PermissionDenied, "invalid"), want: false},
		{name: "plain error", err: errors.New("malformed credential blob"), want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isUndeterminedMaterializeError(tc.err); got != tc.want {
				t.Errorf("isUndeterminedMaterializeError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsUndeterminedLocalMaterializeError pins the codex leg's classification:
// the store having NO credential is the one verdict, and every other way
// MaterializeCodex can fail degrades to undetermined.
//
// The filesystem cases are driven from REAL os-produced errors rather than
// hand-built ones, because they are the shapes MaterializeCodex's own dir work
// actually returns and a fabricated value would test the fabrication. The plain
// errors alongside them are the ones a structural *fs.PathError / *os.LinkError
// test used to miss entirely.
func TestIsUndeterminedLocalMaterializeError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missing := filepath.Join(dir, "no-such-file")

	// ENOENT on open -> *fs.PathError.
	_, openErr := os.Open(missing)
	if openErr == nil {
		t.Fatal("expected opening a missing path to fail")
	}

	// A regular file used as a directory component -> ENOTDIR *fs.PathError.
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed regular file: %v", err)
	}
	mkdirErr := os.MkdirAll(filepath.Join(regular, "accounts", "codex"), 0o700)
	if mkdirErr == nil {
		t.Fatal("expected MkdirAll under a regular file to fail")
	}

	// Hard-linking a missing source -> *os.LinkError.
	linkErr := os.Link(missing, filepath.Join(dir, "link"))
	if linkErr == nil {
		t.Fatal("expected linking a missing source to fail")
	}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil is not undetermined", err: nil, want: false},
		{name: "ENOENT path error", err: openErr, want: true},
		{name: "ENOTDIR from the mkdir chain", err: mkdirErr, want: true},
		{name: "link error", err: linkErr, want: true},
		{
			// MaterializeCodex wraps every filesystem fault in context before it
			// reaches us, so the predicate must see through fmt.Errorf.
			name: "wrapped exactly as MaterializeCodex wraps it",
			err:  fmt.Errorf("create codex account dir for %q: %w", "acct-1", mkdirErr),
			want: true,
		},
		{
			name: "wrapped twice, as the adapter would re-wrap it",
			err:  fmt.Errorf("materialize codex account: %w", fmt.Errorf("enforce 0700 on codex dir %q: %w", dir, openErr)),
			want: true,
		},
		{name: "bare permission error", err: fs.ErrPermission, want: true},
		{name: "context cancelled", err: context.Canceled, want: true},
		{name: "wrapped deadline exceeded", err: fmt.Errorf("materialize codex account: %w", context.DeadlineExceeded), want: true},

		// The plain-error failures. None of these carries a filesystem shape, so
		// a structural test left every one of them classified as a credential
		// verdict and sent the operator to re-authenticate over it.
		{
			// The one that bites in practice: a locked or unavailable system
			// keyring. accountcred.Store never opened it, so nothing read the
			// credential, let alone judged it.
			name: "keyring cannot be opened",
			err: fmt.Errorf("load codex credential for %q: %w", "acct-1",
				fmt.Errorf("open keyring: %w", errors.New("the collection is locked"))),
			want: true,
		},
		{
			// The other half of accountcred.Store.Load: a keyring that opened but
			// failed the read for any reason other than a missing key.
			name: "keyring read failed for a reason other than absence",
			err: fmt.Errorf("load codex credential for %q: %w", "acct-1",
				fmt.Errorf("load account credential: %w", errors.New("dbus: connection closed"))),
			want: true,
		},
		{
			// assertNoSymlinkChain refuses to WRITE into a tampered-looking tree.
			// That is a statement about the tree, not about the credential.
			name: "symlinked accounts tree",
			err:  errors.New(`"/data/accounts/codex" is a symlink`),
			want: true,
		},
		{
			// validateAccountID rejects the id before any credential is reached.
			name: "invalid account id",
			err:  fmt.Errorf("invalid account id %q", ".."),
			want: true,
		},
		{
			// The fail-safe direction: an unrecognised failure added below this
			// layer later must report "could not be checked", never accuse a
			// credential nothing looked at.
			name: "an unclassified error is undetermined",
			err:  errors.New("boom"),
			want: true,
		},

		// Negative controls. Absence IS a verdict about the credential, and
		// re-authenticating is its remedy, so it keeps the invalid arm. The
		// sentinel is what carries it — a look-alike message does not, which is
		// why the predicate matches the value and never the text.
		{
			name: "credential absent from the store stays invalid",
			err:  fmt.Errorf("load codex credential for %q: %w", "acct-1", accountcred.ErrCredentialNotFound),
			want: false,
		},
		{
			name: "absence wrapped twice, as the adapter would re-wrap it",
			err: fmt.Errorf("materialize codex account: %w",
				fmt.Errorf("load codex credential for %q: %w", "acct-1", accountcred.ErrCredentialNotFound)),
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isUndeterminedLocalMaterializeError(tc.err); got != tc.want {
				t.Errorf("isUndeterminedLocalMaterializeError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestMaterializeAccountWrapsLocalCodexFailuresUndetermined drives the codex leg
// end to end through a REAL credmaterialize.Materializer pointed at a temp dir.
//
// NewMaterializer builds that materializer against the default base dir, so the
// adapter is constructed directly here — same package — to aim it somewhere
// hermetic. CODEX_HOME is redirected to an empty dir so base-home projection has
// nothing to copy and the outcome does not depend on the developer's own ~/.codex.
func TestMaterializeAccountWrapsLocalCodexFailuresUndetermined(t *testing.T) {
	// No t.Parallel: t.Setenv below is incompatible with it.
	t.Setenv("CODEX_HOME", t.TempDir())

	tests := []struct {
		name             string
		credErr          error
		breakAccountsDir bool
		wantUndetermined bool
	}{
		{
			// A regular file where accounts/ must be makes the dir chain
			// unusable. Nothing reads the credential, so nothing can have
			// judged it.
			name:             "the account home cannot be prepared",
			breakAccountsDir: true,
			wantUndetermined: true,
		},
		{
			// The store answered, and its answer is about the credential.
			// Re-authenticating is the remedy, so this keeps the invalid arm.
			name:             "the credential is absent from the store",
			credErr:          accountcred.ErrCredentialNotFound,
			wantUndetermined: false,
		},
		{
			// The store could not answer at all: accountcred.Store reports a
			// keyring it cannot open as "open keyring: %w", a plain error with no
			// filesystem shape. Nothing read the stored bytes, so telling the
			// operator to re-authenticate them is an accusation with no evidence
			// behind it.
			name:             "the keyring is locked or unavailable",
			credErr:          fmt.Errorf("open keyring: %w", errors.New("the collection is locked")),
			wantUndetermined: true,
		},
		{
			// The same class one layer in: the keyring opened, the read failed
			// for a reason that is not "no such key".
			name:             "the keyring read failed for a reason other than absence",
			credErr:          fmt.Errorf("load account credential: %w", errors.New("dbus: connection closed")),
			wantUndetermined: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			baseDir := t.TempDir()
			if tc.breakAccountsDir {
				if err := os.WriteFile(filepath.Join(baseDir, "accounts"), []byte("x"), 0o600); err != nil {
					t.Fatalf("seed a regular file at accounts/: %v", err)
				}
			}

			creds := &fakeCreds{blob: []byte("secret"), err: tc.credErr}
			codex, err := credmaterialize.New(contextCredentialStore{store: creds}, zerolog.Nop(), credmaterialize.WithBaseDir(baseDir))
			if err != nil {
				t.Fatalf("build codex materializer: %v", err)
			}
			m := &materializerAdapter{
				store: &spyStore{accounts: map[string]*models.Account{"codex-1": newCodexAccount()}},
				creds: creds,
				codex: codex,
				log:   zerolog.Nop(),
			}

			env, err := m.MaterializeAccount(context.Background(), "codex-1")
			if err == nil {
				t.Fatal("MaterializeAccount: want an error, got nil")
			}
			if env != nil {
				t.Fatalf("MaterializeAccount returned env %v alongside an error; must be nil", env)
			}
			if got := errors.Is(err, account.ErrInjectionUndetermined); got != tc.wantUndetermined {
				t.Errorf("errors.Is(err, account.ErrInjectionUndetermined) = %v, want %v (err: %v)", got, tc.wantUndetermined, err)
			}
			// The credential blob must never reach an error string.
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("error text leaked the credential blob: %v", err)
			}
		})
	}
}

// TestMaterializeAccountNeverReturnsSuccessWithNoEnv covers the three arms that
// used to return (nil, nil) — a shape the resolver could only read as
// "materialization succeeded and this account needs no environment", which
// cleared the injection failure, bumped the LRU timestamp, and spawned on the
// agent CLI's ambient login while every surface still reported the account as
// bound and in force. That is the BOS-973 silent degrade reached without any
// error ever being returned.
//
// Every arm here is a MISSING DEPENDENCY, so every arm must be undetermined:
// nothing in these paths ever asked the credential anything, and telling the
// operator to re-authenticate over an unwired daemon or an unloaded plugin is
// the BOS-881 collapse the outcome split exists to prevent. The negative
// control below pins the other half — a plugin that answered and rejected the
// credential must stay invalid.
func TestMaterializeAccountNeverReturnsSuccessWithNoEnv(t *testing.T) {
	t.Parallel()

	claudeStore := func() *spyStore {
		return &spyStore{accounts: map[string]*models.Account{"a1": newClaudeAccount()}}
	}
	codexStore := func() *spyStore {
		return &spyStore{accounts: map[string]*models.Account{"codex-1": newCodexAccount()}}
	}

	tests := []struct {
		name      string
		adapter   func() *materializerAdapter
		accountID string
		wantIn    string
	}{
		{
			// Built through the real constructor: a daemon wired with no
			// account store and no credential store cannot reach the
			// credential at all.
			name: "daemon wired without a store",
			adapter: func() *materializerAdapter {
				return NewMaterializer(nil, nil, nil, zerolog.Nop()).(*materializerAdapter)
			},
			accountID: "a1",
			wantIn:    "wired without an account or credential store",
		},
		{
			// credmaterialize.New failed at startup, so the codex leg has no
			// materializer. The struct is built by hand because the
			// constructor only reaches this state through that failure.
			name: "codex account with no codex materializer",
			adapter: func() *materializerAdapter {
				return &materializerAdapter{
					clients: map[string]agent.AgentRunnerClient{},
					store:   codexStore(),
					creds:   &fakeCreds{blob: []byte("secret")},
					codex:   nil,
					log:     zerolog.Nop(),
				}
			},
			accountID: "codex-1",
			wantIn:    "no codex materializer is wired",
		},
		{
			// No plugin is loaded for the account's provider — it may simply
			// be restarting. Contrast the resolver's !SupportsRotation arm,
			// which IS invalid: there a loaded runner gave a definite answer.
			name: "no runner plugin loaded for the provider",
			adapter: func() *materializerAdapter {
				return NewMaterializer(
					map[string]agent.AgentRunnerClient{},
					claudeStore(),
					&fakeCreds{blob: []byte("secret")},
					zerolog.Nop(),
				).(*materializerAdapter)
			},
			accountID: "a1",
			wantIn:    "no runner plugin is loaded",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env, err := tc.adapter().MaterializeAccount(context.Background(), tc.accountID)
			if err == nil {
				t.Fatalf("MaterializeAccount must refuse, got env %v and a nil error", env)
			}
			if env != nil {
				t.Errorf("MaterializeAccount returned env %v alongside an error; must be nil", env)
			}
			if !errors.Is(err, account.ErrInjectionUndetermined) {
				t.Errorf("a missing dependency must be undetermined, not invalid: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not name the missing dependency (want it to contain %q)", err, tc.wantIn)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("error text leaked the credential blob: %v", err)
			}
		})
	}

	// Negative control: a loaded plugin that answered and rejected the
	// credential still classifies invalid, so the arms above did not widen
	// undetermined into "every failure".
	t.Run("loaded plugin rejecting the credential stays invalid", func(t *testing.T) {
		t.Parallel()

		m := NewMaterializer(
			map[string]agent.AgentRunnerClient{"claude": &fakeRotationClient{
				matErr: grpcstatus.Error(codes.PermissionDenied, "stored credential is no longer valid"),
			}},
			claudeStore(),
			&fakeCreds{blob: []byte("secret")},
			zerolog.Nop(),
		)
		env, err := m.MaterializeAccount(context.Background(), "a1")
		if err == nil {
			t.Fatalf("MaterializeAccount must refuse, got env %v", env)
		}
		if errors.Is(err, account.ErrInjectionUndetermined) {
			t.Errorf("a rendered provider verdict must stay invalid: %v", err)
		}
	})
}
