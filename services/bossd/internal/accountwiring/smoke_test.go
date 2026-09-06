package accountwiring

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/credmaterialize"
	"github.com/rs/zerolog"
)

type smokeCreds struct {
	blobs map[string][]byte
}

func (s *smokeCreds) Load(accountID string) ([]byte, error) {
	return append([]byte(nil), s.blobs[accountID]...), nil
}

func (s *smokeCreds) Save(accountID string, blob []byte) error {
	s.blobs[accountID] = append([]byte(nil), blob...)
	return nil
}

type lockingSmokeCreds struct {
	smokeCreds
	lockAccount string
	lockCalls   int
}

func (s *lockingSmokeCreds) WithCredentialLock(accountID string, fn func() error) error {
	s.lockAccount = accountID
	s.lockCalls++
	return fn()
}

type smokeClient struct {
	agent.AgentRunnerClient
	startReqs []*bossanovav1.StartAgentRunRequest
	stopReqs  []*bossanovav1.StopAgentRunRequest
	exitError string
	logText   string
}

func (c *smokeClient) StartRun(_ context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	c.startReqs = append(c.startReqs, req)
	if c.logText != "" {
		if err := os.WriteFile(req.GetLogPath(), []byte(c.logText), 0o600); err != nil {
			return nil, err
		}
	}
	return &bossanovav1.StartAgentRunResponse{SessionId: req.GetSessionId()}, nil
}

func (c *smokeClient) ExitStatus(context.Context, *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error) {
	return &bossanovav1.AgentExitStatusResponse{IsComplete: true, ExitError: c.exitError}, nil
}

func (c *smokeClient) StopRun(_ context.Context, req *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error) {
	c.stopReqs = append(c.stopReqs, req)
	return &bossanovav1.StopAgentRunResponse{}, nil
}

func TestCredentialStoreAdapterForwardsCredentialLock(t *testing.T) {
	store := &lockingSmokeCreds{}
	adapter := credentialStoreAdapter{store: store}

	called := false
	if err := adapter.WithCredentialLock("acct-codex", func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("WithCredentialLock: %v", err)
	}

	if !called {
		t.Fatal("lock callback was not called")
	}
	if store.lockCalls != 1 || store.lockAccount != "acct-codex" {
		t.Fatalf("lock calls/account = %d/%q, want 1/%q", store.lockCalls, store.lockAccount, "acct-codex")
	}
}

func TestSmokeRunnerClaudeStartsLiveRunWithAccountEnv(t *testing.T) {
	client := &smokeClient{}
	runner, err := NewSmokeRunner(map[string]agent.AgentRunnerClient{"claude": client}, &smokeCreds{
		blobs: map[string][]byte{"acct-claude": []byte("sk-ant-oat01-secret")},
	}, zerolog.Nop(), WithSmokeBaseDir(t.TempDir()))
	if err != nil {
		t.Fatalf("NewSmokeRunner: %v", err)
	}

	if err := runner.Smoke(context.Background(), "acct-claude", "claude", []byte("sk-ant-oat01-secret")); err != nil {
		t.Fatalf("Smoke: %v", err)
	}

	if len(client.startReqs) != 1 {
		t.Fatalf("StartRun calls = %d, want 1", len(client.startReqs))
	}
	req := client.startReqs[0]
	if got := req.GetExtraEnv()["CLAUDE_CODE_OAUTH_TOKEN"]; got != "sk-ant-oat01-secret" {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN = %q, want stored token", got)
	}
	if req.GetWorkDir() == "" || req.GetLogPath() == "" || req.GetPlan() == "" {
		t.Fatalf("smoke StartRun missing required fields: %+v", req)
	}
	if _, err := uuid.Parse(req.GetSessionId()); err != nil {
		t.Fatalf("smoke session id = %q, want UUID: %v", req.GetSessionId(), err)
	}
}

func TestSmokeRunnerCodexWritesAuthJSONAndSetsCodeXHome(t *testing.T) {
	client := &smokeClient{}
	blob := []byte(`{"access":"access-token","refresh":"refresh-token","id_token":"id-token"}`)
	runner, err := NewSmokeRunner(map[string]agent.AgentRunnerClient{"codex": client}, &smokeCreds{
		blobs: map[string][]byte{"acct-codex": blob},
	}, zerolog.Nop(), WithSmokeBaseDir(t.TempDir()))
	if err != nil {
		t.Fatalf("NewSmokeRunner: %v", err)
	}

	if err := runner.Smoke(context.Background(), "acct-codex", "codex", blob); err != nil {
		t.Fatalf("Smoke: %v", err)
	}

	if len(client.startReqs) != 1 {
		t.Fatalf("StartRun calls = %d, want 1", len(client.startReqs))
	}
	home := client.startReqs[0].GetExtraEnv()["CODEX_HOME"]
	if home == "" {
		t.Fatal("CODEX_HOME not set")
	}
	authPath := filepath.Join(home, "auth.json")
	auth, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read materialized auth.json: %v", err)
	}
	if !strings.Contains(string(auth), `"tokens"`) {
		t.Fatalf("auth.json was not normalized for codex: %s", auth)
	}
}

func TestSmokeRunnerReturnsExitError(t *testing.T) {
	client := &smokeClient{exitError: "401 unauthorized"}
	runner, err := NewSmokeRunner(map[string]agent.AgentRunnerClient{"claude": client}, &smokeCreds{
		blobs: map[string][]byte{"acct-claude": []byte("sk-ant-oat01-secret")},
	}, zerolog.Nop(), WithSmokeBaseDir(t.TempDir()))
	if err != nil {
		t.Fatalf("NewSmokeRunner: %v", err)
	}

	err = runner.Smoke(context.Background(), "acct-claude", "claude", []byte("sk-ant-oat01-secret"))
	if err == nil || !strings.Contains(err.Error(), "401 unauthorized") {
		t.Fatalf("Smoke err = %v, want exit error", err)
	}
}

// TestSmokeRunnerNonExecutionNamesTheBinaryNotTheCredential is the operator's
// half of BOS-1172. The leading sentence is what an operator reads first, and
// "credential verification failed" for a PATH fault sent one down the auth
// path for an hour. The message must name the binary that could not be run --
// without claiming it is missing, since exit 127 also covers a login shell
// that failed to start.
func TestSmokeRunnerNonExecutionNamesTheBinaryNotTheCredential(t *testing.T) {
	client := &smokeClient{
		exitError: codexRunnerUnavailableSentinel,
		logText:   "The `codex' command exists in these Node versions: 22.22.2\n",
	}
	runner, err := NewSmokeRunner(map[string]agent.AgentRunnerClient{"claude": client}, &smokeCreds{
		blobs: map[string][]byte{"acct-claude": []byte("sk-ant-oat01-secret")},
	}, zerolog.Nop(), WithSmokeBaseDir(t.TempDir()))
	if err != nil {
		t.Fatalf("NewSmokeRunner: %v", err)
	}

	err = runner.Smoke(context.Background(), "acct-claude", "claude", []byte("sk-ant-oat01-secret"))
	if err == nil {
		t.Fatal("Smoke err = nil, want failure")
	}
	got := err.Error()
	if strings.Contains(got, "credential verification failed") {
		t.Fatalf("Smoke err = %q, must not blame the credential for a non-execution failure", got)
	}
	if !strings.Contains(got, "codex binary") {
		t.Fatalf("Smoke err = %q, want the binary that could not be run named", got)
	}
	// The diagnostic tail already carried the real cause; keep it.
	if !strings.Contains(got, "diagnostic:") {
		t.Fatalf("Smoke err = %q, want the diagnostic tail preserved", got)
	}
	// The classification must survive the rewording: the message the operator
	// reads is also the signal the maintainer classifies.
	outcome, class := classifyVerification(err, SmokeResult{}, time.Now())
	if outcome != models.AuthCheckOutcomeUnavailable || class != authFailureRunnerUnavailable {
		t.Fatalf("classify(%q) = %q/%q, want %q/%q", got, outcome, class,
			models.AuthCheckOutcomeUnavailable, authFailureRunnerUnavailable)
	}
}

func TestSmokeRunnerReturnsRedactedLogDiagnostic(t *testing.T) {
	client := &smokeClient{
		exitError: "exit status 1",
		logText:   `{"text":"authentication failed for agent.yuki@kamik.ai with token sk-ant-oat01-secretsecret"}` + "\n",
	}
	runner, err := NewSmokeRunner(map[string]agent.AgentRunnerClient{"claude": client}, &smokeCreds{
		blobs: map[string][]byte{"acct-claude": []byte("sk-ant-oat01-secretsecret")},
	}, zerolog.Nop(), WithSmokeBaseDir(t.TempDir()))
	if err != nil {
		t.Fatalf("NewSmokeRunner: %v", err)
	}

	err = runner.Smoke(context.Background(), "acct-claude", "claude", []byte("sk-ant-oat01-secretsecret"))
	if err == nil {
		t.Fatal("Smoke err = nil, want failure")
	}
	got := err.Error()
	for _, want := range []string{"credential verification failed: exit status 1", "diagnostic:", "authentication failed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Smoke err = %q, want %q", got, want)
		}
	}
	for _, secret := range []string{"sk-ant-oat01-secretsecret", "agent.yuki@kamik.ai"} {
		if strings.Contains(got, secret) {
			t.Fatalf("Smoke err leaked %q: %s", secret, got)
		}
	}
	if len(client.stopReqs) != 1 {
		t.Fatalf("StopRun calls = %d, want 1", len(client.stopReqs))
	}
}

func TestSmokeDiagnosticRedactsBeforeTailingLongLine(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "smoke.log")
	secretTail := strings.Repeat("b", smokeDiagnosticTailBytes+1024)
	if err := os.WriteFile(logPath, []byte("provider rejected access_token="+secretTail), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	got := smokeDiagnostic(logPath)
	if strings.Contains(got, strings.Repeat("b", 64)) {
		t.Fatalf("smokeDiagnostic leaked token suffix: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("smokeDiagnostic = %q, want redacted sentinel", got)
	}
}

// refreshingSmokeClient simulates a Codex agent that refreshes auth.json in
// place during the run, which is exactly the case PersistBack exists for.
type refreshingSmokeClient struct {
	smokeClient
	refreshed []byte
}

func (c *refreshingSmokeClient) StartRun(ctx context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	resp, err := c.smokeClient.StartRun(ctx, req)
	if err != nil {
		return nil, err
	}
	home := req.GetExtraEnv()["CODEX_HOME"]
	if home == "" {
		return nil, os.ErrNotExist
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), c.refreshed, 0o600); err != nil {
		return nil, err
	}
	return resp, nil
}

// TestSmokeRunnerVerifyPersistsRefreshedCodexAuth is AC-3: a clean Codex smoke
// persists refreshed auth.json content through the existing PersistBack
// closure, and reports that it did so.
func TestSmokeRunnerVerifyPersistsRefreshedCodexAuth(t *testing.T) {
	blob := []byte(`{"tokens":{"access_token":"old-access","refresh_token":"old-refresh","id_token":"old-id"}}`)
	refreshed := []byte(`{"tokens":{"access_token":"new-access","refresh_token":"new-refresh","id_token":"new-id"}}`)
	client := &refreshingSmokeClient{refreshed: refreshed}
	creds := &smokeCreds{blobs: map[string][]byte{"acct-codex": blob}}
	runner, err := NewSmokeRunner(map[string]agent.AgentRunnerClient{"codex": client}, creds,
		zerolog.Nop(), WithSmokeBaseDir(t.TempDir()))
	if err != nil {
		t.Fatalf("NewSmokeRunner: %v", err)
	}

	res, err := runner.Verify(context.Background(), "acct-codex", "codex", blob)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.SelfCredentialWrites == 0 {
		t.Fatal("SelfCredentialWrites = 0; a clean codex run that refreshed auth.json must report its own write")
	}
	stored := string(creds.blobs["acct-codex"])
	if !strings.Contains(stored, "new-access") {
		t.Fatalf("refreshed access token was not persisted back into the store")
	}
}

// TestSmokeRunnerVerifyReportsNoPersistForClaude pins the other side: the
// Claude path has no PersistBack closure, so nothing is claimed.
func TestSmokeRunnerVerifyReportsNoPersistForClaude(t *testing.T) {
	client := &smokeClient{}
	runner, err := NewSmokeRunner(map[string]agent.AgentRunnerClient{"claude": client}, &smokeCreds{
		blobs: map[string][]byte{"acct-claude": []byte("sk-ant-oat01-secret")},
	}, zerolog.Nop(), WithSmokeBaseDir(t.TempDir()))
	if err != nil {
		t.Fatalf("NewSmokeRunner: %v", err)
	}
	res, err := runner.Verify(context.Background(), "acct-claude", "claude", []byte("sk-ant-oat01-secret"))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.SelfCredentialWrites != 0 {
		t.Fatalf("SelfCredentialWrites = %d for claude; that path never writes the credential store", res.SelfCredentialWrites)
	}
}

// generationSmokeCreds is a credential store with the two optional surfaces the
// production keyring store has: a per-account compound lock and a
// monotonically increasing generation bumped on every committed write.
type generationSmokeCreds struct {
	smokeCreds
	generations map[string]uint64
}

func (s *generationSmokeCreds) Save(accountID string, blob []byte) error {
	if err := s.smokeCreds.Save(accountID, blob); err != nil {
		return err
	}
	s.generations[accountID]++
	return nil
}

func (s *generationSmokeCreds) WithCredentialLock(_ string, fn func() error) error { return fn() }

func (s *generationSmokeCreds) CredentialGeneration(accountID string) uint64 {
	return s.generations[accountID]
}

// replacingSmokeClient refreshes auth.json like a real Codex agent and, in the
// same window, has someone else replace the stored credential — an operator
// refresh or a peer session landing while this run is in flight.
type replacingSmokeClient struct {
	refreshingSmokeClient
	store       *generationSmokeCreds
	accountID   string
	replacement []byte
}

func (c *replacingSmokeClient) StartRun(ctx context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	resp, err := c.refreshingSmokeClient.StartRun(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := c.store.Save(c.accountID, c.replacement); err != nil {
		return nil, err
	}
	return resp, nil
}

// TestSmokeRunnerVerifyRefusesPersistOverReplacedCredential pins the data-loss
// half of mid-run credential replacement. credcheck's generation compare
// discards a verdict computed against a superseded credential, but it runs
// after the fact: persist-back reloads the store as its merge baseline and lets
// the on-disk tokens win, so without a guard at the write seam the run's own
// stale tokens overwrite the replacement in the keyring and nothing rolls it
// back.
func TestSmokeRunnerVerifyRefusesPersistOverReplacedCredential(t *testing.T) {
	original := []byte(`{"tokens":{"access_token":"old-access","refresh_token":"old-refresh","id_token":"old-id"}}`)
	agentRefreshed := []byte(`{"tokens":{"access_token":"agent-access","refresh_token":"agent-refresh","id_token":"agent-id"}}`)
	operator := []byte(`{"tokens":{"access_token":"operator-access","refresh_token":"operator-refresh","id_token":"operator-id"}}`)

	creds := &generationSmokeCreds{
		smokeCreds:  smokeCreds{blobs: map[string][]byte{"acct-codex": original}},
		generations: map[string]uint64{},
	}
	client := &replacingSmokeClient{
		refreshingSmokeClient: refreshingSmokeClient{refreshed: agentRefreshed},
		store:                 creds,
		accountID:             "acct-codex",
		replacement:           operator,
	}
	runner, err := NewSmokeRunner(map[string]agent.AgentRunnerClient{"codex": client}, creds,
		zerolog.Nop(), WithSmokeBaseDir(t.TempDir()))
	if err != nil {
		t.Fatalf("NewSmokeRunner: %v", err)
	}

	res, err := runner.Verify(context.Background(), "acct-codex", "codex", original)
	if !errors.Is(err, errCredentialReplacedMidRun) {
		t.Fatalf("Verify err = %v, want errCredentialReplacedMidRun", err)
	}
	stored := string(creds.blobs["acct-codex"])
	if !strings.Contains(stored, "operator-access") {
		t.Fatalf("the replacement credential was overwritten by the run's own stale tokens")
	}
	if strings.Contains(stored, "agent-access") {
		t.Fatalf("the run folded its materialized tokens over a credential it no longer described")
	}
	if res.SelfCredentialWrites != 0 {
		t.Fatalf("SelfCredentialWrites = %d after a refused save; a refused write mutates nothing and must not be claimed", res.SelfCredentialWrites)
	}
}

// TestSmokeRunnerVerifyPersistsWhenOnlyItsOwnWritesMoved is the other side of
// the guard: a generation that moved by exactly this run's own writes is not a
// replacement, and the persist must still land.
func TestSmokeRunnerVerifyPersistsWhenOnlyItsOwnWritesMoved(t *testing.T) {
	original := []byte(`{"tokens":{"access_token":"old-access","refresh_token":"old-refresh","id_token":"old-id"}}`)
	refreshed := []byte(`{"tokens":{"access_token":"new-access","refresh_token":"new-refresh","id_token":"new-id"}}`)

	creds := &generationSmokeCreds{
		smokeCreds:  smokeCreds{blobs: map[string][]byte{"acct-codex": original}},
		generations: map[string]uint64{},
	}
	client := &refreshingSmokeClient{refreshed: refreshed}
	runner, err := NewSmokeRunner(map[string]agent.AgentRunnerClient{"codex": client}, creds,
		zerolog.Nop(), WithSmokeBaseDir(t.TempDir()))
	if err != nil {
		t.Fatalf("NewSmokeRunner: %v", err)
	}

	res, err := runner.Verify(context.Background(), "acct-codex", "codex", original)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.SelfCredentialWrites == 0 {
		t.Fatal("SelfCredentialWrites = 0; the guard must not suppress a run's own legitimate persist")
	}
	if stored := string(creds.blobs["acct-codex"]); !strings.Contains(stored, "new-access") {
		t.Fatalf("refreshed access token was not persisted back into the store")
	}
}

// TestSmokeRunnerVerifyCarriesTheRefreshAssertion closes the second half of the
// only untested link in the BOS-1174 chain. TestMaterializeCodexCarriesTheRefreshAssertion
// (credmaterialize) proves the verdict reaches Materialized; this proves
// SmokeRunner.Verify forwards it onto SmokeResult, where classifyCleanVerification
// reads it. Delete either line and the whole suite stayed green while the feature
// degraded back to a permanent Unknown — the original bug, with passing CI, a
// fixture row and a proof scenario still showing it working.
//
// It needs no injected clock. The threshold is a fraction of the token's OWN
// observed lifetime, not a wall-clock duration, so a token issued 9 days ago and
// expiring in 1 day reads as 90% through a 10-day life under any clock this test
// can run on. The not-due case is asserted in the same shape so the test cannot
// pass against a constant.
func TestSmokeRunnerVerifyCarriesTheRefreshAssertion(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name    string
		issued  time.Time
		expires time.Time
		want    credmaterialize.RefreshAssertion
	}{
		{
			name:    "overdue",
			issued:  now.Add(-9 * 24 * time.Hour),
			expires: now.Add(24 * time.Hour),
			want:    credmaterialize.RefreshAssertionOverdue,
		},
		{
			name:    "not due",
			issued:  now.Add(-24 * time.Hour),
			expires: now.Add(9 * 24 * time.Hour),
			want:    credmaterialize.RefreshAssertionNotDue,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blob := codexExpiryBlob(t, tc.issued, tc.expires)
			runner, err := NewSmokeRunner(map[string]agent.AgentRunnerClient{"codex": &smokeClient{}}, &smokeCreds{
				blobs: map[string][]byte{"acct-codex": blob},
			}, zerolog.Nop(), WithSmokeBaseDir(t.TempDir()))
			if err != nil {
				t.Fatalf("NewSmokeRunner: %v", err)
			}

			res, err := runner.Verify(context.Background(), "acct-codex", "codex", blob)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if res.RefreshAssertion != tc.want {
				t.Fatalf("SmokeResult.RefreshAssertion = %v, want %v — the verdict stopped at the materialize seam",
					res.RefreshAssertion, tc.want)
			}
		})
	}
}

// TestSmokeRunnerVerifyReportsUnknownBeforeMaterialization pins the safe default
// on the paths that return before anything has read the token. "Cannot evaluate"
// must never be reported as evidence of a dead refresh chain.
func TestSmokeRunnerVerifyReportsUnknownBeforeMaterialization(t *testing.T) {
	runner, err := NewSmokeRunner(map[string]agent.AgentRunnerClient{"codex": &smokeClient{}}, &smokeCreds{
		blobs: map[string][]byte{"acct-codex": []byte("{}")},
	}, zerolog.Nop(), WithSmokeBaseDir(t.TempDir()))
	if err != nil {
		t.Fatalf("NewSmokeRunner: %v", err)
	}
	res, err := runner.Verify(context.Background(), "acct-codex", "nosuchprovider", nil)
	if err == nil {
		t.Fatal("Verify for an unknown provider returned nil error")
	}
	if res.RefreshAssertion != credmaterialize.RefreshAssertionUnknown {
		t.Fatalf("RefreshAssertion = %v, want Unknown for a run that never materialized", res.RefreshAssertion)
	}
}

// codexExpiryBlob builds the codex auth.json shape around a synthetic unsigned
// JWT carrying only the two NumericDate claims the assertion reads. Nothing here
// is a real credential: the signature segment is a literal, and the claims are
// the timestamps under test.
func codexExpiryBlob(t *testing.T, issued, expires time.Time) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"iat": issued.Unix(), "exp": expires.Unix()})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	token := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".signature-not-verified"
	blob, err := json.Marshal(map[string]any{
		"tokens": map[string]any{
			"access_token":  token,
			"refresh_token": "synthetic-refresh-token",
		},
	})
	if err != nil {
		t.Fatalf("marshal codex blob: %v", err)
	}
	return blob
}
