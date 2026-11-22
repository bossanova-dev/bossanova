package accountflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const antigravityTokenRelPath = ".gemini/antigravity-cli/antigravity-oauth-token"

// AntigravityOptions configures the `boss account add antigravity` and
// `boss account reauth <id>` registration flows.
type AntigravityOptions struct {
	Exec     Exec
	Prompter Prompter
	Client   AgentAccountClient
	AgyBin   string                 // default "agy"
	BinEnv   []string               // extra environment required by the resolved binary
	HomeDir  func() (string, error) // default: 0700 os.MkdirTemp HOME
	Timeout  time.Duration          // OAuth login deadline, default 10m
	Label    string
	Priority int32
}

// RunAntigravityAdd registers an Antigravity account by driving `agy -p /usage`
// under a fresh isolated temp HOME with SSH_CLIENT=1 so agy bypasses the OS
// keyring, completes Google OAuth, and writes its file-based token store, then
// stores and verifies the raw JSON blob.
func RunAntigravityAdd(ctx context.Context, o AntigravityOptions) error {
	stored, err := captureAntigravityCredential(ctx, &o)
	if err != nil {
		return err
	}
	label, err := promptIdentity(ctx, o.Prompter, o.Client, "antigravity", o.Label, false)
	if err != nil {
		return err
	}
	return storeAndTest(ctx, o.Prompter, o.Client, "antigravity", label, o.Priority, stored, "")
}

// RunAntigravityReauth replaces an existing Antigravity account's stored
// credential in place via RefreshAgentAccount with TestAfterSave.
func RunAntigravityReauth(ctx context.Context, o AntigravityOptions, accountID string) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return errors.New("account id is required to reauthenticate")
	}
	if err := requireAntigravityAccount(ctx, o.Client, accountID); err != nil {
		return err
	}
	stored, err := captureAntigravityCredential(ctx, &o)
	if err != nil {
		return err
	}
	resp, refreshErr := o.Client.RefreshAgentAccount(ctx, &pb.RefreshAgentAccountRequest{
		Id:            accountID,
		Credential:    stored,
		TestAfterSave: true,
	})
	outcome, reason := classifyRefresh(resp, refreshErr)
	switch outcome {
	case testVerified:
		o.Prompter.Say("%s", ReauthVerifiedLine(accountID, resp.GetAccount(), time.Now()))
		return nil
	case testUnavailable:
		o.Prompter.Say("%s", ReauthUnverifiedLine(accountID, reason))
		return nil
	default:
		if refreshErr != nil {
			return fmt.Errorf("reauthenticate account %s: %w", accountID, refreshErr)
		}
		return fmt.Errorf("reauthenticate account %s: the new credential was stored but verification failed (%s); the account stays unavailable until a reauthentication verifies", accountID, reason)
	}
}

func requireAntigravityAccount(ctx context.Context, c AgentAccountClient, accountID string) error {
	accounts, err := c.ListAgentAccounts(ctx, "", false)
	if err != nil {
		return fmt.Errorf("look up account %s: %w", accountID, err)
	}
	for _, a := range accounts {
		if a.GetId() != accountID {
			continue
		}
		if provider := strings.ToLower(a.GetProvider()); provider != "antigravity" {
			return fmt.Errorf("account %s is a %s account; this flow re-runs the Antigravity OAuth login. Use `boss account refresh %s` with a replacement credential instead", accountID, provider, accountID)
		}
		return nil
	}
	return fmt.Errorf("no account with id %s (run `boss account ls` to see the ids)", accountID)
}

func captureAntigravityCredential(ctx context.Context, o *AntigravityOptions) ([]byte, error) {
	if o.AgyBin == "" {
		o.AgyBin = "agy"
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultFlowTimeout
	}
	if o.HomeDir == nil {
		o.HomeDir = func() (string, error) { return tempDir("boss-account-antigravity-*") }
	}
	dir, cleanup, err := isolatedTempHome(o.HomeDir, o.Prompter, "HOME")
	if err != nil {
		return nil, err
	}
	defer cleanup()

	blob, err := antigravityCapture(ctx, *o, dir)
	if err != nil {
		return nil, err
	}
	if err := validateAntigravityTokenJSON(blob); err != nil {
		return nil, fmt.Errorf("antigravity login did not produce a valid credential; nothing stored: %w", err)
	}
	return blob, nil
}

func antigravityCapture(ctx context.Context, o AntigravityOptions, dir string) ([]byte, error) {
	env := append([]string{"HOME=" + dir, "SSH_CLIENT=1"}, o.BinEnv...)
	proc, err := o.Exec.Start(ctx, o.AgyBin, []string{"-p", "/usage", "--output-format", "json"}, env)
	if err != nil {
		return nil, err
	}

	tctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	type result struct {
		err  error
		last []string
	}
	done := make(chan result, 1)
	go func() {
		var all []string
		for line := range proc.Lines() {
			all = append(all, line)
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "https://accounts.google.com/") {
				o.Prompter.Say("Open %s in your browser, sign in, and paste the authorization code when prompted…", trimmed)
			}
		}
		done <- result{err: proc.Wait(), last: all}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			return nil, fmt.Errorf("agy login exited with error (%w); last output: %s", res.err, strings.Join(lastN(res.last, 3), " | "))
		}
		// #nosec G304 -- reads the antigravity-oauth-token file the login flow just wrote inside our temp HOME
		data, rerr := os.ReadFile(filepath.Join(dir, antigravityTokenRelPath))
		if rerr != nil {
			return nil, fmt.Errorf("agy exited cleanly but wrote no %s under %s: %w", antigravityTokenRelPath, dir, rerr)
		}
		return data, nil
	case <-tctx.Done():
		_ = proc.Kill()
		return nil, errors.New("antigravity login timed out (abandoned?)")
	}
}

func validateAntigravityTokenJSON(blob []byte) error {
	var payload struct {
		Token struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"token"`
	}
	if err := json.Unmarshal(blob, &payload); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if strings.TrimSpace(payload.Token.RefreshToken) == "" && strings.TrimSpace(payload.Token.AccessToken) == "" {
		return errors.New("missing token.refresh_token or token.access_token")
	}
	return nil
}
