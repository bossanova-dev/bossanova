package accountflow

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestRunAntigravityAddCapturesAndStoresToken(t *testing.T) {
	homeDir := t.TempDir()
	tokenJSON := []byte(`{"token":{"access_token":"fake-access","token_type":"Bearer","refresh_token":"fake-refresh"},"auth_method":"consumer","id_token_email":"user@example.com"}`)

	proc := newScriptedProc([]string{
		"Authentication required. Please visit the URL to log in:",
		"  https://accounts.google.com/o/oauth2/auth?client_id=test",
	}, nil)
	proc.waitHook = func() error {
		p := filepath.Join(homeDir, antigravityTokenRelPath)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		return os.WriteFile(p, tokenJSON, 0o600)
	}
	exec := &fakeExec{proc: proc}
	prompter := &fakePrompter{}
	client := &fakeAccountClient{
		addResult:  &pb.AgentAccount{Id: "acc-agy-1", Provider: "antigravity", Label: "work"},
		testResult: &pb.TestAgentAccountResponse{LiveSmokeRan: true, Account: &pb.AgentAccount{Id: "acc-agy-1", Provider: "antigravity", Label: "work", Status: "active", Health: "ok"}},
	}

	err := RunAntigravityAdd(context.Background(), AntigravityOptions{
		Exec:     exec,
		Prompter: prompter,
		Client:   client,
		HomeDir:  func() (string, error) { return homeDir, nil },
		Timeout:  5 * time.Second,
		Label:    "work",
		Priority: 2,
	})
	if err != nil {
		t.Fatalf("RunAntigravityAdd: %v", err)
	}
	if len(exec.args) == 0 || exec.args[0] != "-p" {
		t.Errorf("unexpected agy args: %v", exec.args)
	}
	hasHome, hasSSH := false, false
	for _, kv := range exec.extraEnv {
		if kv == "HOME="+homeDir {
			hasHome = true
		}
		if kv == "SSH_CLIENT=1" {
			hasSSH = true
		}
	}
	if !hasHome || !hasSSH {
		t.Errorf("env missing HOME or SSH_CLIENT=1: %v", exec.extraEnv)
	}
	if len(client.addReqs) != 1 {
		t.Fatalf("AddAgentAccount called %d times, want 1", len(client.addReqs))
	}
	req := client.addReqs[0]
	if req.GetProvider() != "antigravity" || req.GetLabel() != "work" {
		t.Errorf("AddAgentAccount = (%q, %q), want (antigravity, work)", req.GetProvider(), req.GetLabel())
	}
	if !bytes.Equal(req.GetCredential(), tokenJSON) {
		t.Errorf("stored credential mismatch")
	}
	// Temp HOME must be cleaned up on return.
	if _, err := os.Stat(homeDir); !os.IsNotExist(err) {
		t.Errorf("expected temp home %s to be deleted, err=%v", homeDir, err)
	}
}

func TestRunAntigravityReauthReplacesExistingAccount(t *testing.T) {
	homeDir := t.TempDir()
	tokenJSON := []byte(`{"token":{"access_token":"new-access","token_type":"Bearer","refresh_token":"new-refresh"},"auth_method":"consumer","id_token_email":"user@example.com"}`)

	proc := newScriptedProc(nil, nil)
	proc.waitHook = func() error {
		p := filepath.Join(homeDir, antigravityTokenRelPath)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		return os.WriteFile(p, tokenJSON, 0o600)
	}
	exec := &fakeExec{proc: proc}
	prompter := &fakePrompter{}
	client := &fakeAccountClient{
		listResult: []*pb.AgentAccount{{Id: "acc-agy-1", Provider: "antigravity", Label: "work"}},
		refreshResult: &pb.RefreshAgentAccountResponse{
			LiveSmokeRan: true,
			Account:      &pb.AgentAccount{Id: "acc-agy-1", Provider: "antigravity", Status: "active", Health: "ok"},
		},
	}

	err := RunAntigravityReauth(context.Background(), AntigravityOptions{
		Exec:     exec,
		Prompter: prompter,
		Client:   client,
		HomeDir:  func() (string, error) { return homeDir, nil },
		Timeout:  5 * time.Second,
	}, "acc-agy-1")
	if err != nil {
		t.Fatalf("RunAntigravityReauth: %v", err)
	}
	if len(client.refreshReqs) != 1 || client.refreshReqs[0].GetId() != "acc-agy-1" || !client.refreshReqs[0].GetTestAfterSave() {
		t.Fatalf("unexpected RefreshAgentAccount requests: %+v", client.refreshReqs)
	}
	if !strings.Contains(strings.Join(prompter.said, "\n"), reauthVerifiedMarker) {
		t.Errorf("expected verified line in output, got: %v", prompter.said)
	}
}
