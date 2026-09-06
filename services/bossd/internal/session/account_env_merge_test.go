package session

import (
	"context"
	"reflect"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/models"
)

// fakeAccountEnvResolver is an accountEnvResolver returning a fixed overlay,
// mirroring fakeProofEnvResolver in tmux_chat_test.go. It records the session
// it was asked to resolve so tests can assert the binding is threaded through.
type fakeAccountEnvResolver struct {
	env     map[string]string
	err     error
	gotSess *models.Session
	calls   int
}

func (f *fakeAccountEnvResolver) Resolve(_ context.Context, sess *models.Session) (map[string]string, error) {
	f.calls++
	f.gotSess = sess
	if f.err != nil {
		return nil, f.err
	}
	return f.env, nil
}

// newTestLifecycleWithProxy builds a bare Lifecycle wired for proxyBaseURL
// tests: the given rotation config, a live proxy port, and a registrar
// returning a fixed token — mirroring the newLC() setup in
// TestResolveAccountEnv_InjectsProxyBaseURL.
func newTestLifecycleWithProxy(t *testing.T, cfg config.ManagedAccountsConfig) *Lifecycle {
	t.Helper()
	lc := &Lifecycle{logger: zerolog.Nop()}
	lc.SetRotationConfig(cfg)
	lc.SetProxyPort(5555)
	lc.SetProxyRegistrar(stubRegistrar{token: "path-tok"})
	return lc
}

// TestProxyBaseURL_RequiresManagedAccountsEnabled proves proxyBaseURL is
// gated on BOTH flags: managed_accounts.enabled AND failover_proxy. Task 1
// wired FailoverProxyEnabled() alone; this proves the conjunction, matching
// the config.go doc comment that injection "also requires
// ManagedAccountsEnabled() (gated by the caller)".
func TestProxyBaseURL_RequiresManagedAccountsEnabled(t *testing.T) {
	no, yes := false, true
	// managed disabled, failover enabled → no injection.
	l := newTestLifecycleWithProxy(t, config.ManagedAccountsConfig{
		Enabled:       &no,
		FailoverProxy: &yes,
	})
	acctID := "acct-1"
	sess := &models.Session{ID: "s1", AgentName: "claude", AccountID: &acctID}
	if got := l.proxyBaseURL(sess); got != "" {
		t.Errorf("proxyBaseURL = %q, want \"\" when managed_accounts disabled", got)
	}
	// both enabled → injection.
	l2 := newTestLifecycleWithProxy(t, config.ManagedAccountsConfig{
		Enabled:       &yes,
		FailoverProxy: &yes,
	})
	if got := l2.proxyBaseURL(sess); got == "" {
		t.Error("proxyBaseURL = \"\", want a URL when both flags on")
	}
}

// TestResolveAccountEnv_InjectsProxyBaseURL proves the S7 failover proxy env
// wiring: when the opt-in flag is on and the proxy is live, resolveAccountEnv
// folds ANTHROPIC_BASE_URL into the account overlay for a Claude session (and
// leaves the account creds intact); with the flag off it injects nothing.
func TestResolveAccountEnv_InjectsProxyBaseURL(t *testing.T) {
	newLC := func() *Lifecycle {
		accEnv := &fakeAccountEnvResolver{env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "acct-bearer"}}
		lc := &Lifecycle{logger: zerolog.Nop()}
		lc.SetAccountEnvResolver(accEnv)
		lc.SetProxyPort(5555)
		lc.SetProxyRegistrar(stubRegistrar{token: "path-tok"})
		return lc
	}
	acctID := "acct-1"
	claude := &models.Session{ID: "s1", AgentName: "claude", AccountID: &acctID}

	t.Run("flag on + live: injects loopback base URL, drops subprocess OAuth token", func(t *testing.T) {
		lc := newLC()
		enableFailoverProxy(lc)
		got, _ := lc.resolveAccountEnv(context.Background(), claude)
		if got["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:5555/s/path-tok" {
			t.Fatalf("ANTHROPIC_BASE_URL = %q, want the loopback proxy URL", got["ANTHROPIC_BASE_URL"])
		}
		// BOS-326: with the proxy live the sentinel is planted and the account
		// bearer is resolved server-side (CurrentBearer), so the subprocess must
		// NOT also carry CLAUDE_CODE_OAUTH_TOKEN — that combination trips Claude
		// Code's "both set" warning and mislabels billing.
		if _, ok := got["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
			t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN must be stripped when proxied: %v", got)
		}
	})

	t.Run("flag off: no ANTHROPIC_BASE_URL, creds unchanged", func(t *testing.T) {
		lc := newLC()
		off := false
		lc.SetRotationConfig(config.ManagedAccountsConfig{FailoverProxy: &off})
		got, _ := lc.resolveAccountEnv(context.Background(), claude)
		if _, ok := got["ANTHROPIC_BASE_URL"]; ok {
			t.Fatalf("ANTHROPIC_BASE_URL must not be injected when the flag is off: %v", got)
		}
		if got["CLAUDE_CODE_OAUTH_TOKEN"] != "acct-bearer" {
			t.Fatalf("account creds changed with proxy off: %v", got)
		}
	})
}

// TestResolveAccountEnv_InjectsSentinelWithBaseURL proves the BOS-326 sentinel:
// when the proxy overlay is active, resolveAccountEnv folds in
// ANTHROPIC_API_KEY=SentinelAPIKey exactly alongside ANTHROPIC_BASE_URL (the
// interactive REPL honours the api key, not the injected OAuth token), and never
// injects it when the proxy is off.
func TestResolveAccountEnv_InjectsSentinelWithBaseURL(t *testing.T) {
	yes := true
	l := newTestLifecycleWithProxy(t, config.ManagedAccountsConfig{Enabled: &yes, FailoverProxy: &yes})
	l.SetAccountEnvResolver(&fakeAccountEnvResolver{env: map[string]string{claudeOAuthTokenEnvKey: "acct-bearer"}})
	acctID := "acct-1"
	sess := &models.Session{ID: "s1", AgentName: "claude", AccountID: &acctID}
	env, _ := l.resolveAccountEnv(context.Background(), sess)
	if env["ANTHROPIC_BASE_URL"] == "" {
		t.Fatal("precondition: ANTHROPIC_BASE_URL should be injected")
	}
	if env["ANTHROPIC_API_KEY"] != SentinelAPIKey {
		t.Errorf("ANTHROPIC_API_KEY = %q, want sentinel %q", env["ANTHROPIC_API_KEY"], SentinelAPIKey)
	}
}

func TestResolveAccountEnv_NoProxyOrSentinelForUnmanagedAccount(t *testing.T) {
	yes := true
	l := newTestLifecycleWithProxy(t, config.ManagedAccountsConfig{Enabled: &yes, FailoverProxy: &yes})
	emptyAccountID := ""

	for name, sess := range map[string]*models.Session{
		"nil-account":   {ID: "s1", AgentName: "claude"},
		"empty-account": {ID: "s2", AgentName: "claude", AccountID: &emptyAccountID},
	} {
		t.Run(name, func(t *testing.T) {
			env, _ := l.resolveAccountEnv(context.Background(), sess)
			if _, ok := env["ANTHROPIC_BASE_URL"]; ok {
				t.Fatalf("ANTHROPIC_BASE_URL must not be injected for unmanaged account: %v", env)
			}
			if _, ok := env["ANTHROPIC_API_KEY"]; ok {
				t.Fatalf("ANTHROPIC_API_KEY must not be injected for unmanaged account: %v", env)
			}
		})
	}
}

func TestResolveAccountEnv_NoProxyOrSentinelWithoutMaterializedBearer(t *testing.T) {
	yes := true
	l := newTestLifecycleWithProxy(t, config.ManagedAccountsConfig{Enabled: &yes, FailoverProxy: &yes})
	l.SetAccountEnvResolver(&fakeAccountEnvResolver{env: map[string]string{"CODEX_HOME": "/synthetic/home"}})
	acctID := "acct-1"
	sess := &models.Session{ID: "s1", AgentName: "claude", AccountID: &acctID}

	env, _ := l.resolveAccountEnv(context.Background(), sess)
	if env["CODEX_HOME"] != "/synthetic/home" {
		t.Fatalf("resolver overlay changed: %v", env)
	}
	if _, ok := env["ANTHROPIC_BASE_URL"]; ok {
		t.Fatalf("ANTHROPIC_BASE_URL must not be injected without materialized bearer: %v", env)
	}
	if _, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Fatalf("ANTHROPIC_API_KEY must not be injected without materialized bearer: %v", env)
	}
}

func TestResolveAccountEnv_NoSentinelWhenProxyOff(t *testing.T) {
	no := false
	l := newTestLifecycleWithProxy(t, config.ManagedAccountsConfig{Enabled: &no})
	sess := &models.Session{ID: "s1", AgentName: "claude"}
	resolved, _ := l.resolveAccountEnv(context.Background(), sess)
	if _, ok := resolved["ANTHROPIC_API_KEY"]; ok {
		t.Error("sentinel ANTHROPIC_API_KEY must not be injected when the proxy is off")
	}
}

// TestSentinelApprovalSuffix pins the approval suffix to the sentinel's last 20
// chars — the exact value seeded into ~/.claude.json's approved list.
func TestSentinelApprovalSuffix(t *testing.T) {
	suffix := SentinelApprovalSuffix()
	if len(suffix) != 20 {
		t.Fatalf("approval suffix len = %d, want 20", len(suffix))
	}
	if suffix != SentinelAPIKey[len(SentinelAPIKey)-20:] {
		t.Errorf("suffix %q is not the last 20 chars of the sentinel key", suffix)
	}
}

// TestMergeSessionEnv_Precedence proves managed > account > proof: managed
// keys always win, account beats proof, and proof-only keys survive.
func TestMergeSessionEnv_Precedence(t *testing.T) {
	managed := map[string]string{"BOSS_SESSION_ID": "m", "K": "managed"}
	account := map[string]string{"K": "account", "A": "acct"}
	proof := map[string]string{"K": "proof", "A": "proofA", "P": "proof"}

	got := mergeSessionEnv(managed, account, proof)

	if got["K"] != "managed" {
		t.Errorf("K = %q, want %q (managed must win)", got["K"], "managed")
	}
	if got["A"] != "acct" {
		t.Errorf("A = %q, want %q (account must beat proof)", got["A"], "acct")
	}
	if got["P"] != "proof" {
		t.Errorf("P = %q, want %q (proof-only key must survive)", got["P"], "proof")
	}
	if got["BOSS_SESSION_ID"] != "m" {
		t.Errorf("BOSS_SESSION_ID = %q, want %q", got["BOSS_SESSION_ID"], "m")
	}
}

// TestMergeSessionEnv_ManagedNeverShadowed proves a managed BOSS_* key
// survives even when both the account and proof overlays try to override it.
func TestMergeSessionEnv_ManagedNeverShadowed(t *testing.T) {
	managed := map[string]string{"BOSS_SESSION_ID": "managed-wins"}
	account := map[string]string{"BOSS_SESSION_ID": "ACCOUNT-SHOULD-NOT-WIN"}
	proof := map[string]string{"BOSS_SESSION_ID": "PROOF-SHOULD-NOT-WIN"}

	got := mergeSessionEnv(managed, account, proof)

	if got["BOSS_SESSION_ID"] != "managed-wins" {
		t.Errorf("BOSS_SESSION_ID = %q, want %q (managed must never be shadowed)", got["BOSS_SESSION_ID"], "managed-wins")
	}
}

// TestMergeSessionEnv_NilOrEmptyAccountIsNoOp proves the account-rotation
// no-op path (D9): a nil or empty account map must yield a result
// byte-identical to the prior two-way mergeEnv(managed, proof) — production
// behaviour is unchanged until BOS-170 wires a real resolver.
func TestMergeSessionEnv_NilOrEmptyAccountIsNoOp(t *testing.T) {
	managed := map[string]string{
		"BOSS_SESSION_ID": "s1",
		"BOSS_AGENT":      "claude",
		"BOSS_CRON":       "true",
	}
	proof := map[string]string{
		"PROOF_ANTHROPIC_API_KEY": "secret-anthropic",
		"BOSS_PROOF_R2_BUCKET":    "bossanova-proof-production",
	}

	want := mergeEnv(managed, proof)

	if got := mergeSessionEnv(managed, nil, proof); !reflect.DeepEqual(got, want) {
		t.Errorf("mergeSessionEnv with nil account = %v, want byte-identical %v", got, want)
	}
	if got := mergeSessionEnv(managed, map[string]string{}, proof); !reflect.DeepEqual(got, want) {
		t.Errorf("mergeSessionEnv with empty account map = %v, want byte-identical %v", got, want)
	}
}

// TestAccountEnvReachesSpawnPaths proves a bound account's env overlay reaches
// the exact merge expression used at each of the three spawn sites this ticket
// touches, and never shadows managed BOSS_* keys.
//
//   - interactive tmux: mergeSessionEnv(managed, account, proof)
//   - headless / resume: mergeEnv(account, proof)
func TestAccountEnvReachesSpawnPaths(t *testing.T) {
	l := &Lifecycle{}
	l.SetAccountEnvResolver(&fakeAccountEnvResolver{env: map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": "x",
		"BOSS_SESSION_ID":         "ACCOUNT-MUST-NOT-WIN",
	}})
	l.SetProofEnvResolver(fakeProofEnvResolver{env: map[string]string{"PROOF_KEY": "p"}})

	ctx := context.Background()
	acctID := "a1"
	sess := &models.Session{ID: "s1", AgentName: "claude", AccountID: &acctID, WorktreePath: "/wt"}

	// Interactive tmux call-site expression.
	tmuxAccountEnv, _ := l.resolveAccountEnv(ctx, sess)
	tmuxEnv := mergeSessionEnv(ManagedSessionEnv(sess, "agent-7", sess.AgentName), tmuxAccountEnv, l.resolveProofEnv())
	if tmuxEnv["CLAUDE_CODE_OAUTH_TOKEN"] != "x" {
		t.Errorf("tmux: account token missing: %v", tmuxEnv)
	}
	if tmuxEnv["PROOF_KEY"] != "p" {
		t.Errorf("tmux: proof key missing: %v", tmuxEnv)
	}
	if tmuxEnv["BOSS_SESSION_ID"] != "s1" {
		t.Errorf("tmux: managed BOSS_SESSION_ID overwritten by account overlay: %q", tmuxEnv["BOSS_SESSION_ID"])
	}

	// Headless / resume call-site expression (no managed layer).
	headlessAccountEnv, _ := l.resolveAccountEnv(ctx, sess)
	headlessEnv := mergeEnv(headlessAccountEnv, l.resolveProofEnv())
	if headlessEnv["CLAUDE_CODE_OAUTH_TOKEN"] != "x" {
		t.Errorf("headless: account token missing: %v", headlessEnv)
	}
	if headlessEnv["PROOF_KEY"] != "p" {
		t.Errorf("headless: proof key missing: %v", headlessEnv)
	}
}

// TestLifecycleResolveAccountEnvNilResolver confirms a lifecycle with no
// account resolver wired (the default in this ticket) degrades to nil rather
// than panicking, mirroring TestLifecycleResolveProofEnvNilResolver.
func TestLifecycleResolveAccountEnvNilResolver(t *testing.T) {
	l := &Lifecycle{}
	if got, _ := l.resolveAccountEnv(context.Background(), &models.Session{}); got != nil {
		t.Errorf("expected nil overlay with no resolver, got %v", got)
	}
}

// TestLifecycleResolveAccountEnvInjectedResolver confirms
// SetAccountEnvResolver wires a fake resolver whose synthetic map is
// returned verbatim by resolveAccountEnv, and that the session is threaded
// through to the resolver.
func TestLifecycleResolveAccountEnvInjectedResolver(t *testing.T) {
	l := &Lifecycle{}
	want := map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "synthetic-token", "CODEX_HOME": "/synthetic/home"}
	fake := &fakeAccountEnvResolver{env: want}
	l.SetAccountEnvResolver(fake)

	sess := &models.Session{ID: "s1", AgentName: "claude"}
	if got, _ := l.resolveAccountEnv(context.Background(), sess); !reflect.DeepEqual(got, want) {
		t.Errorf("resolveAccountEnv() = %v, want %v", got, want)
	}
	if fake.calls != 1 {
		t.Errorf("resolver calls = %d, want 1", fake.calls)
	}
	if fake.gotSess != sess {
		t.Errorf("resolver got session %v, want %v", fake.gotSess, sess)
	}
}
