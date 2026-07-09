package accountflow

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

	"github.com/recurser/bossalib/agentcred"
)

// scriptedProcHook returns a proc that replays lines, closes the channel, then
// runs hook when Wait is called (used to model codex writing auth.json on exit).
func scriptedProcHook(lines []string, waitErr error, hook func() error) *fakeProc {
	ch := make(chan string, len(lines)+1)
	for _, l := range lines {
		ch <- l
	}
	close(ch)
	return &fakeProc{lines: ch, waitErr: waitErr, waitHook: hook}
}

func codexIDToken(t *testing.T, email string) string {
	t.Helper()
	claims, err := json.Marshal(map[string]string{"em" + "ail": email})
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString(claims)
	return "hdr." + payload + ".sig"
}

func codexAuthBytes(t *testing.T, tokens map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"tokens": tokens, "last_refresh": "2026-07-04T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func tempCodexHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "codex-home-*")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunCodexAdd(t *testing.T) {
	t.Run("happy_path", func(t *testing.T) {
		dir := tempCodexHome(t)
		auth := codexAuthBytes(t, map[string]any{
			"access_token": "at", "refresh_token": "rt",
			"id_token": codexIDToken(t, "codexuser@example.com"), "account_id": "acc",
		})
		hook := func() error { return os.WriteFile(filepath.Join(dir, "auth.json"), auth, 0o600) }
		proc := scriptedProcHook([]string{
			"To sign in visit https://auth.openai.com/codex/device",
			"and enter code ABCD-EFGHI",
		}, nil, hook)
		ex := &fakeExec{proc: proc}
		pr := &fakePrompter{}
		cl := &fakeAccountClient{}
		err := RunCodexAdd(context.Background(), CodexOptions{
			Exec: ex, Prompter: pr, Client: cl, Priority: 3,
			HomeDir: func() (string, error) { return dir, nil },
		})
		if err != nil {
			t.Fatalf("RunCodexAdd: %v", err)
		}
		tr := pr.transcript()
		urlIdx := strings.Index(tr, "https://auth.openai.com/codex/device")
		codeIdx := strings.Index(tr, "ABCD-EFGHI")
		okIdx := strings.Index(tr, "registered and verified")
		if urlIdx < 0 || codeIdx < 0 {
			t.Fatalf("device URL/code not surfaced:\n%s", tr)
		}
		if okIdx >= 0 && urlIdx > okIdx {
			t.Fatalf("URL surfaced AFTER completion; want before:\n%s", tr)
		}
		if len(cl.addReqs) != 1 {
			t.Fatalf("want 1 AddAccount, got %d", len(cl.addReqs))
		}
		req := cl.addReqs[0]
		if req.GetProvider() != "codex" {
			t.Fatalf("provider = %q", req.GetProvider())
		}
		// The stored blob must be the canonical account-store shape
		// ({access,refresh,id_token}) the daemon validates, NOT the raw auth.json
		// (nested tokens.*_token); otherwise TestAccount rejects every interactive
		// codex registration as a missing-field credential.
		var stored map[string]string
		if err := json.Unmarshal(req.GetCredential(), &stored); err != nil {
			t.Fatalf("stored credential is not a JSON object: %v", err)
		}
		if stored["access"] != "at" || stored["refresh"] != "rt" {
			t.Fatalf("stored credential = %v, want flat access/refresh from auth.json tokens", stored)
		}
		if stored["id_token"] == "" {
			t.Fatalf("stored credential dropped id_token: %v", stored)
		}
		// Unmarshaling into map[string]string above would fail on the raw
		// {"tokens":{...}} shape (object value, not string), so success already
		// proves the nested shape was flattened.
		if req.GetPriority() != 3 {
			t.Fatalf("priority = %d, want 3 (--priority must reach AddAccount)", req.GetPriority())
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("CODEX_HOME not removed: %v", err)
		}
	})

	t.Run("timeout_abandoned", func(t *testing.T) {
		dir := tempCodexHome(t)
		proc := newBlockingProc([]string{
			"https://auth.openai.com/codex/device",
			"code ABCD-EFGHI",
		})
		ex := &fakeExec{proc: proc}
		cl := &fakeAccountClient{}
		err := RunCodexAdd(context.Background(), CodexOptions{
			Exec: ex, Prompter: &fakePrompter{}, Client: cl,
			HomeDir: func() (string, error) { return dir, nil },
			Timeout: 50 * time.Millisecond,
		})
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("err = %v, want timed out", err)
		}
		if !proc.wasKilled() {
			t.Fatalf("proc was not killed on timeout")
		}
		if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
			t.Fatalf("CODEX_HOME not removed on timeout: %v", statErr)
		}
		if len(cl.addReqs) != 0 {
			t.Fatalf("AddAccount must not be called")
		}
	})

	t.Run("nonzero_exit", func(t *testing.T) {
		dir := tempCodexHome(t)
		proc := scriptedProcHook([]string{"connecting", "429 too many requests"}, errors.New("exit status 1"), nil)
		ex := &fakeExec{proc: proc}
		cl := &fakeAccountClient{}
		err := RunCodexAdd(context.Background(), CodexOptions{
			Exec: ex, Prompter: &fakePrompter{}, Client: cl,
			HomeDir: func() (string, error) { return dir, nil },
		})
		if err == nil || !strings.Contains(err.Error(), "429 too many requests") {
			t.Fatalf("err = %v, want 429 line", err)
		}
		if len(cl.addReqs) != 0 {
			t.Fatalf("AddAccount must not be called")
		}
	})

	t.Run("disabled_setting", func(t *testing.T) {
		dir := tempCodexHome(t)
		proc := scriptedProcHook([]string{
			"device code login is not enabled for this Codex server. Use the browser login or verify the server URL.",
		}, errors.New("exit status 1"), nil)
		ex := &fakeExec{proc: proc}
		pr := &fakePrompter{}
		cl := &fakeAccountClient{}
		err := RunCodexAdd(context.Background(), CodexOptions{
			Exec: ex, Prompter: pr, Client: cl,
			HomeDir: func() (string, error) { return dir, nil },
		})
		if err == nil {
			t.Fatalf("RunCodexAdd returned nil, want disabled-device-auth error")
		}
		tr := pr.transcript()
		for _, want := range []string{
			"chatgpt.com/#settings",
			"device code authorization",
			"Security",
			"boss account add codex",
		} {
			if !strings.Contains(tr, want) {
				t.Fatalf("transcript missing %q:\n%s", want, tr)
			}
		}
		if strings.Contains(tr, "ABCD-EFGHI") {
			t.Fatalf("device code leaked into remediation transcript:\n%s", tr)
		}
		if len(cl.addReqs) != 0 {
			t.Fatalf("AddAccount must not be called")
		}
		if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
			t.Fatalf("CODEX_HOME not removed on disabled setting: %v", statErr)
		}
	})

	t.Run("disabled_setting_wins_over_timeout", func(t *testing.T) {
		dir := tempCodexHome(t)
		proc := newBlockingProc([]string{
			"device code login is not enabled for this Codex server. Use the browser login or verify the server URL.",
		})
		ex := &fakeExec{proc: proc}
		pr := &fakePrompter{}
		cl := &fakeAccountClient{}
		err := RunCodexAdd(context.Background(), CodexOptions{
			Exec: ex, Prompter: pr, Client: cl,
			HomeDir: func() (string, error) { return dir, nil },
			Timeout: 50 * time.Millisecond,
		})
		if err == nil || strings.Contains(err.Error(), "timed out") {
			t.Fatalf("err = %v, want disabled-device-auth error before timeout", err)
		}
		if !strings.Contains(pr.transcript(), "Enable device code authorization for Codex") {
			t.Fatalf("missing remediation transcript:\n%s", pr.transcript())
		}
		if !proc.wasKilled() {
			t.Fatalf("proc was not killed after disabled-device-auth signal")
		}
		if len(cl.addReqs) != 0 {
			t.Fatalf("AddAccount must not be called")
		}
		if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
			t.Fatalf("CODEX_HOME not removed on disabled setting: %v", statErr)
		}
	})

	t.Run("no_auth_json", func(t *testing.T) {
		dir := tempCodexHome(t)
		proc := scriptedProcHook([]string{"logging in"}, nil, nil)
		ex := &fakeExec{proc: proc}
		cl := &fakeAccountClient{}
		err := RunCodexAdd(context.Background(), CodexOptions{
			Exec: ex, Prompter: &fakePrompter{}, Client: cl,
			HomeDir: func() (string, error) { return dir, nil },
		})
		if err == nil || !strings.Contains(err.Error(), "no auth.json") {
			t.Fatalf("err = %v, want 'no auth.json'", err)
		}
		if len(cl.addReqs) != 0 {
			t.Fatalf("AddAccount must not be called")
		}
	})

	t.Run("missing_id_token", func(t *testing.T) {
		dir := tempCodexHome(t)
		auth := codexAuthBytes(t, map[string]any{"access_token": "at", "refresh_token": "rt"})
		hook := func() error { return os.WriteFile(filepath.Join(dir, "auth.json"), auth, 0o600) }
		proc := scriptedProcHook([]string{"done"}, nil, hook)
		ex := &fakeExec{proc: proc}
		cl := &fakeAccountClient{}
		err := RunCodexAdd(context.Background(), CodexOptions{
			Exec: ex, Prompter: &fakePrompter{}, Client: cl,
			HomeDir: func() (string, error) { return dir, nil },
		})
		if !errors.Is(err, agentcred.ErrCodexAuthMissingField) {
			t.Fatalf("err = %v, want ErrCodexAuthMissingField", err)
		}
		if !strings.Contains(err.Error(), "id_token") {
			t.Fatalf("err = %v, want mention of id_token", err)
		}
		if len(cl.addReqs) != 0 {
			t.Fatalf("AddAccount must not be called")
		}
	})

	t.Run("prompt_never_parsed", func(t *testing.T) {
		dir := tempCodexHome(t)
		auth := codexAuthBytes(t, map[string]any{
			"access_token": "at", "refresh_token": "rt",
			"id_token": codexIDToken(t, "bg@example.com"),
		})
		hook := func() error { return os.WriteFile(filepath.Join(dir, "auth.json"), auth, 0o600) }
		proc := scriptedProcHook([]string{"authenticated in background"}, nil, hook)
		ex := &fakeExec{proc: proc}
		cl := &fakeAccountClient{}
		if err := RunCodexAdd(context.Background(), CodexOptions{
			Exec: ex, Prompter: &fakePrompter{}, Client: cl,
			HomeDir: func() (string, error) { return dir, nil },
		}); err != nil {
			t.Fatalf("background-auth path should still succeed: %v", err)
		}
		if len(cl.addReqs) != 1 {
			t.Fatalf("want 1 AddAccount, got %d", len(cl.addReqs))
		}
	})

	t.Run("no_secret_leak", func(t *testing.T) {
		dir := tempCodexHome(t)
		auth := codexAuthBytes(t, map[string]any{
			"access_token": "super-secret-access", "refresh_token": "super-secret-refresh",
			"id_token": codexIDToken(t, "leak@example.com"),
		})
		hook := func() error { return os.WriteFile(filepath.Join(dir, "auth.json"), auth, 0o600) }
		proc := scriptedProcHook([]string{
			"https://auth.openai.com/codex/device", "code ABCD-EFGHI",
		}, nil, hook)
		ex := &fakeExec{proc: proc}
		pr := &fakePrompter{}
		cl := &fakeAccountClient{}
		if err := RunCodexAdd(context.Background(), CodexOptions{
			Exec: ex, Prompter: pr, Client: cl,
			HomeDir: func() (string, error) { return dir, nil },
		}); err != nil {
			t.Fatalf("RunCodexAdd: %v", err)
		}
		tr := pr.transcript()
		for _, secret := range []string{"super-secret-access", "super-secret-refresh", string(auth)} {
			if strings.Contains(tr, secret) {
				t.Fatalf("secret leaked into transcript:\n%s", tr)
			}
		}
	})
}
