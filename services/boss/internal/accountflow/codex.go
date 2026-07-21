package accountflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/recurser/bossalib/agentcred"
)

var errCodexDeviceAuthDisabled = errors.New("codex device-code login disabled")

// CodexOptions configures the `boss account add codex` registration flow.
type CodexOptions struct {
	Exec     Exec
	Prompter Prompter
	Client   AccountClient
	CodexBin string                 // default "codex"
	HomeDir  func() (string, error) // default: 0700 os.MkdirTemp CODEX_HOME
	Timeout  time.Duration          // device-flow deadline, default 10m
	Label    string
	Priority int32 // account ordering, from --priority (default 0)
}

// RunCodexAdd registers an additional Codex account by driving
// `codex login --device-auth` under a FRESH temp CODEX_HOME, capturing the
// auth.json it writes, then storing and verifying the raw blob.
func RunCodexAdd(ctx context.Context, o CodexOptions) error {
	if o.CodexBin == "" {
		o.CodexBin = "codex"
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultFlowTimeout
	}
	if o.HomeDir == nil {
		o.HomeDir = func() (string, error) { return tempDir("boss-account-codex-*") }
	}

	dir, err := o.HomeDir()
	if err != nil {
		return err
	}
	// The dir MUST pre-exist and be private before spawn (auth.json holds live
	// tokens). Remove it on EVERY exit path — a leaked temp dir is a leak.
	// dir is a directory: 0700 is least-privilege and owner-execute is required
	// to traverse the CODEX_HOME dir, so a stricter 0600 would make it unusable.
	// #nosec G302 -- Chmod(dir,0o700) on the private CODEX_HOME cred dir; 0700 is least-privilege
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("could not secure temp CODEX_HOME %s: %w", dir, err)
	}
	defer func() {
		if rerr := os.RemoveAll(dir); rerr != nil {
			o.Prompter.Say("warning: could not remove temp CODEX_HOME %s (delete it manually — it may hold credentials): %v", dir, rerr)
		}
	}()

	blob, err := codexCapture(ctx, o, dir)
	if err != nil {
		if errors.Is(err, errCodexDeviceAuthDisabled) {
			o.Prompter.Say("Codex device-code login is disabled for this ChatGPT account.")
			o.Prompter.Say("Enable it, then re-run this command:")
			o.Prompter.Say("  1. Open https://chatgpt.com/#settings and go to the Security section.")
			o.Prompter.Say("  2. Turn on \"Enable device code authorization for Codex\".")
			o.Prompter.Say("  3. Re-run: boss account add codex")
		}
		return err
	}

	auth, err := agentcred.ValidateCodexAuthJSON(blob)
	if err != nil {
		if errors.Is(err, agentcred.ErrCodexAuthMissingField) {
			return fmt.Errorf("codex login did not produce a complete credential (id_token is required for token refresh); nothing stored: %w", err)
		}
		return fmt.Errorf("codex auth.json is invalid; nothing stored: %w", err)
	}

	// Codex registration always runs against an interactive TTY (the device flow
	// has no --token-stdin path), so identity prompting stays enabled.
	label, err := promptIdentity(ctx, o.Prompter, o.Client, "codex", o.Label, false)
	if err != nil {
		return err
	}
	// Store the canonical account-store shape ({access,refresh,id_token}), not the
	// raw auth.json: storeAndTest verifies immediately via TestAccount, which
	// validates codex credentials in that flat shape. The 1.5 materializer owns
	// merge semantics from there.
	stored, err := agentcred.CodexAccountStoreJSON(auth)
	if err != nil {
		return fmt.Errorf("could not encode codex credential for storage; nothing stored: %w", err)
	}
	return storeAndTest(ctx, o.Prompter, o.Client, "codex", label, o.Priority, stored, "")
}

type codexResult struct {
	err      error
	last     []string
	disabled bool
}

// codexCapture spawns the device-auth login, surfaces the URL+code the first
// time both parse, waits for exit under the deadline, then reads auth.json.
func codexCapture(ctx context.Context, o CodexOptions, dir string) ([]byte, error) {
	proc, err := o.Exec.Start(ctx, o.CodexBin, []string{"login", "--device-auth"}, []string{"CODEX_HOME=" + dir})
	if err != nil {
		return nil, err
	}

	tctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	done := make(chan codexResult, 1)
	disabledSeen := make(chan struct{})
	go func() {
		var buf strings.Builder
		var all []string
		surfaced := false
		disabled := false
		for line := range proc.Lines() {
			buf.WriteString(line)
			buf.WriteByte('\n')
			all = append(all, line)
			if !disabled && agentcred.ParseCodexDeviceAuthDisabled(buf.String()) {
				disabled = true
				close(disabledSeen)
			}
			if !surfaced {
				if prompt, ok := agentcred.ParseCodexDeviceAuthPrompt(buf.String()); ok {
					o.Prompter.Say("Open %s in your browser and enter code %s, then finish signing in… (waiting)", prompt.URL, prompt.Code)
					surfaced = true
				}
			}
		}
		done <- codexResult{err: proc.Wait(), last: all, disabled: disabled}
	}()

	select {
	case res := <-done:
		if res.disabled {
			return nil, errCodexDeviceAuthDisabled
		}
		if res.err != nil {
			return nil, fmt.Errorf("codex login exited with error (%v); last output: %s", res.err, strings.Join(lastN(res.last, 3), " | "))
		}
		// #nosec G304 -- reads the codex auth.json the login flow just wrote; const component on an internal temp HOME; secret-adjacent
		// owner=@recurser review-by=2027-01-18 issue=BOS-28
		data, rerr := os.ReadFile(filepath.Join(dir, "auth.json"))
		if rerr != nil {
			return nil, fmt.Errorf("codex exited cleanly but wrote no auth.json to %s: %w", dir, rerr)
		}
		return data, nil
	case <-disabledSeen:
		_ = proc.Kill()
		return nil, errCodexDeviceAuthDisabled
	case <-tctx.Done():
		select {
		case <-disabledSeen:
			_ = proc.Kill()
			return nil, errCodexDeviceAuthDisabled
		default:
		}
		_ = proc.Kill()
		return nil, errors.New("codex device flow timed out (abandoned?)")
	}
}
