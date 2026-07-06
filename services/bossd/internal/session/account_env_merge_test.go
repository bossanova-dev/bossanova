package session

import (
	"context"
	"reflect"
	"testing"

	"github.com/recurser/bossalib/models"
)

// fakeAccountEnvResolver is an accountEnvResolver returning a fixed overlay,
// mirroring fakeProofEnvResolver in tmux_chat_test.go. It records the session
// it was asked to resolve so tests can assert the binding is threaded through.
type fakeAccountEnvResolver struct {
	env     map[string]string
	gotSess *models.Session
	calls   int
}

func (f *fakeAccountEnvResolver) Resolve(_ context.Context, sess *models.Session) map[string]string {
	f.calls++
	f.gotSess = sess
	return f.env
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
	tmuxEnv := mergeSessionEnv(ManagedSessionEnv(sess, "agent-7", sess.AgentName), l.resolveAccountEnv(ctx, sess), l.resolveProofEnv())
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
	headlessEnv := mergeEnv(l.resolveAccountEnv(ctx, sess), l.resolveProofEnv())
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
	if got := l.resolveAccountEnv(context.Background(), &models.Session{}); got != nil {
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
	if got := l.resolveAccountEnv(context.Background(), sess); !reflect.DeepEqual(got, want) {
		t.Errorf("resolveAccountEnv() = %v, want %v", got, want)
	}
	if fake.calls != 1 {
		t.Errorf("resolver calls = %d, want 1", fake.calls)
	}
	if fake.gotSess != sess {
		t.Errorf("resolver got session %v, want %v", fake.gotSess, sess)
	}
}
