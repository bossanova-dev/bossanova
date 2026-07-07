package accountwiring

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/agent"
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
