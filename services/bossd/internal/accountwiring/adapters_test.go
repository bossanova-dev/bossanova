package accountwiring

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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

	"github.com/recurser/bossd/internal/account"
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
	store := &spyStore{accounts: map[string]*models.Account{"a1": newClaudeAccount()}}
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
	store := &spyStore{accounts: map[string]*models.Account{"a1": newClaudeAccount()}}
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

	env := resolver.Resolve(context.Background(), &models.Session{
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
