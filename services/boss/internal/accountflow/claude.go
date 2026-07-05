package accountflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/recurser/bossalib/agentcred"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// defaultFlowTimeout bounds an interactive registration walkthrough.
const defaultFlowTimeout = 10 * time.Minute

// ClaudeOptions configures the `boss account add claude` registration flow.
type ClaudeOptions struct {
	Exec       Exec
	Prompter   Prompter
	Client     AccountClient
	ClaudeBin  string                 // default "claude"
	ScratchDir func() (string, error) // default: 0700 os.MkdirTemp
	Timeout    time.Duration          // whole-walkthrough deadline, default 10m
	PasteMode  bool                   // skip the CLI walkthrough, paste a token
	Label      string                 // preseeded from --label; prompted when empty
	Email      string                 // preseeded from --email; prompted when empty
	Priority   int32                  // account ordering, from --priority (default 0)
}

// RunClaudeAdd registers an additional Claude account via `claude setup-token`
// (or a pasted token), then stores and live-tests it through the daemon.
func RunClaudeAdd(ctx context.Context, o ClaudeOptions) error {
	if o.ClaudeBin == "" {
		o.ClaudeBin = "claude"
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultFlowTimeout
	}
	if o.ScratchDir == nil {
		o.ScratchDir = func() (string, error) { return tempDir("boss-account-claude-*") }
	}

	token, err := claudeCaptureToken(ctx, o)
	if err != nil {
		return err
	}
	if err := agentcred.ValidateClaudeToken(token); err != nil {
		return err
	}

	// PasteMode is the --token-stdin escape hatch: stdin is consumed by the token
	// and there is no interactive terminal, so identity prompts must resolve to
	// their defaults instead of reading a closed stdin.
	label, email, err := promptIdentity(ctx, o.Prompter, o.Client, "claude", o.Label, o.Email, "", o.PasteMode)
	if err != nil {
		return err
	}
	return storeAndTest(ctx, o.Prompter, o.Client, "claude", label, email, o.Priority, []byte(token), agentcred.MaskToken(token))
}

// claudeCaptureToken obtains a setup token, either by pasting or by driving the
// `claude setup-token` CLI in a scratch CLAUDE_CONFIG_DIR (D9: the default
// login is never touched).
func claudeCaptureToken(ctx context.Context, o ClaudeOptions) (string, error) {
	if o.PasteMode {
		return pasteClaudeToken(o.Prompter)
	}
	walk, err := o.Prompter.Confirm("Run the claude setup-token walkthrough now? (No = paste an existing token)", true)
	if err != nil {
		return "", err
	}
	if !walk {
		return pasteClaudeToken(o.Prompter)
	}
	return claudeWalkthrough(ctx, o)
}

// pasteClaudeToken asks for a token and validates it, allowing exactly one
// re-prompt before failing.
func pasteClaudeToken(p Prompter) (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := p.AskSecret("Paste your Claude setup token")
		if err != nil {
			return "", err
		}
		if agentcred.ValidateClaudeToken(tok) == nil {
			return tok, nil
		}
		if attempt == 0 {
			p.Say("That does not look like a Claude setup token (expected sk-ant-…). Please try again.")
		}
	}
	return "", agentcred.ErrInvalidClaudeToken
}

type claudeResult struct {
	err  error
	buf  string
	last []string
}

// claudeWalkthrough runs `claude setup-token`, tees masked output to the user,
// and parses the token from the accumulated output after exit.
func claudeWalkthrough(ctx context.Context, o ClaudeOptions) (string, error) {
	scratch, err := o.ScratchDir()
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	proc, err := o.Exec.Start(ctx, o.ClaudeBin, []string{"setup-token"}, []string{"CLAUDE_CONFIG_DIR=" + scratch})
	if err != nil {
		return "", err
	}

	tctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	done := make(chan claudeResult, 1)
	go func() {
		var buf strings.Builder
		var all []string
		for line := range proc.Lines() {
			buf.WriteString(line)
			buf.WriteByte('\n')
			all = append(all, line)
			o.Prompter.Say("%s", maskLine(line))
		}
		done <- claudeResult{err: proc.Wait(), buf: buf.String(), last: all}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			return "", fmt.Errorf("claude setup-token failed: %s", maskLine(strings.Join(lastN(res.last, 3), " | ")))
		}
		tok, ok := agentcred.ParseClaudeSetupTokenOutput(res.buf)
		if !ok {
			return "", errors.New("claude setup-token completed but no setup token found in its output")
		}
		return tok, nil
	case <-tctx.Done():
		_ = proc.Kill()
		return "", errors.New("claude setup-token timed out")
	}
}

// --- shared identity + store phases (reused by the codex flow) -------------

// promptIdentity resolves the label + email for a new account. A non-empty
// flagLabel/flagEmail (supplied via --label/--email) is used verbatim without
// prompting, so pre-seeded invocations (e.g. `--token-stdin --label x --email y`)
// stay fully non-interactive. Empty values are prompted, defaulting the email
// to defaultEmail (e.g. a codex id_token claim) and the label to
// "<provider>-<n+1>". A duplicate email always forces a distinct label.
//
// When nonInteractive is set (the --token-stdin claude path), empty values
// resolve to their defaults WITHOUT prompting: stdin has already been consumed
// by the piped token, so an intentionally empty email must not trigger an Ask
// that would fail on EOF. A duplicate email that --label does not already
// disambiguate cannot be resolved without a prompt and is a hard error.
func promptIdentity(ctx context.Context, p Prompter, c AccountClient, provider, flagLabel, flagEmail, defaultEmail string, nonInteractive bool) (string, string, error) {
	existing, err := c.ListAccounts(ctx, provider)
	if err != nil {
		return "", "", fmt.Errorf("could not list existing %s accounts: %w", provider, err)
	}

	email := flagEmail
	if email == "" {
		if nonInteractive {
			email = defaultEmail
		} else if email, err = p.Ask(fmt.Sprintf("Email for this %s account (optional)", provider), defaultEmail); err != nil {
			return "", "", err
		}
	}

	defaultLabel := fmt.Sprintf("%s-%d", provider, len(existing)+1)
	collision := findEmailCollision(existing, email)
	if collision == nil {
		return resolveLabel(p, flagLabel, defaultLabel, email, nonInteractive)
	}

	p.Say("Email %s is already registered to account %q; choose a distinct label.", email, collision.GetLabel())
	if flagLabel != "" && flagLabel != collision.GetLabel() {
		return flagLabel, email, nil
	}
	if nonInteractive {
		return "", "", fmt.Errorf("email %s is already registered to account %q; pass a distinct --label to register another %s account with --token-stdin", email, collision.GetLabel(), provider)
	}
	for {
		label, aerr := p.Ask("Label for this account", defaultLabel)
		if aerr != nil {
			return "", "", aerr
		}
		if label != "" && label != collision.GetLabel() {
			return label, email, nil
		}
		p.Say("Label %q is already in use; enter a different label.", label)
	}
}

// resolveLabel uses flagLabel verbatim when supplied, otherwise prompts with
// defaultLabel. It is the no-collision path of promptIdentity. When
// nonInteractive is set, an empty flagLabel resolves to defaultLabel without
// prompting a stdin that the piped token already consumed.
func resolveLabel(p Prompter, flagLabel, defaultLabel, email string, nonInteractive bool) (string, string, error) {
	if flagLabel != "" {
		return flagLabel, email, nil
	}
	if nonInteractive {
		return defaultLabel, email, nil
	}
	label, err := p.Ask("Label for this account", defaultLabel)
	if err != nil {
		return "", "", err
	}
	return label, email, nil
}

// storeAndTest adds the credential through the daemon then live-tests it,
// offering keep-or-remove when the live test fails.
func storeAndTest(ctx context.Context, p Prompter, c AccountClient, provider, label, email string, priority int32, blob []byte, display string) error {
	acct, err := c.AddAccount(ctx, &pb.AddAccountRequest{
		Provider:   provider,
		Label:      label,
		Email:      email,
		Priority:   priority,
		Credential: blob,
	})
	if err != nil {
		if mentionsKeyring(err) {
			return fmt.Errorf("%w\nhint: bossd could not open the system keyring; see BOSS_KEYRING_BACKEND / --allow-insecure-keyring in the daemon docs", err)
		}
		return err
	}

	id := acct.GetId()
	resp, testErr := c.TestAccount(ctx, id)
	outcome, reason := classifyTest(resp, testErr)
	switch outcome {
	case testVerified:
		if display != "" {
			p.Say("Account %q registered and verified (%s).", label, display)
		} else {
			p.Say("Account %q registered and verified.", label)
		}
		return nil
	case testDeferred:
		// The daemon accepted the credential but no live smoke runner executed
		// (credential materialization is still pending — 1.5), so the live test
		// is deferred, not failed. Keep the just-registered valid account and
		// let rotation retry the live test later instead of prompting to remove.
		if display != "" {
			p.Say("Account %q registered (%s); live verification deferred: %s", label, display, reason)
		} else {
			p.Say("Account %q registered; live verification deferred: %s", label, reason)
		}
		p.Say("Rotation will run the live test once credential materialization is available.")
		return nil
	case testFailed:
		return keepOrRemove(ctx, p, c, id, label, reason, testErr)
	}
	return nil
}

// keepOrRemove handles a failed live test: a transport error or a live smoke
// that actually ran and reported an error. It offers keep-or-remove for the
// just-registered account and removes it when the operator declines.
func keepOrRemove(ctx context.Context, p Prompter, c AccountClient, id, label, reason string, testErr error) error {
	keep, cerr := p.Confirm(fmt.Sprintf("Live test failed (%s). Keep the account anyway?", reason), false)
	if cerr != nil {
		return cerr
	}
	if keep {
		p.Say("Account %q stored unverified; rotation will retry the live test later.", label)
		return nil
	}
	if rerr := c.RemoveAccount(ctx, id); rerr != nil {
		p.Say("warning: could not remove unverified account %s: %v", id, rerr)
	}
	if testErr != nil {
		return testErr
	}
	return fmt.Errorf("live test failed: %s", reason)
}

// --- small helpers ---------------------------------------------------------

func findEmailCollision(accounts []*pb.Account, email string) *pb.Account {
	if email == "" {
		return nil
	}
	for _, a := range accounts {
		if a.GetEmail() == email {
			return a
		}
	}
	return nil
}

// testOutcome classifies a TestAccount result.
type testOutcome int

const (
	// testVerified: the credential is stored and the live smoke passed (or
	// there is simply nothing to report).
	testVerified testOutcome = iota
	// testDeferred: the daemon accepted the credential but no live smoke runner
	// executed, so the live test was not actually run (e.g. credential
	// materialization is still pending). Not a failure — the account is valid.
	testDeferred
	// testFailed: a transport error, or a live smoke that ran and reported an
	// error.
	testFailed
)

// liveSmokeUnavailableMarker is a stable substring of the daemon's
// last_test_error when TestAccount degraded because no live smoke runner was
// wired (credential materialization pending — 1.5). It mirrors
// liveSmokeUnavailableDetail in services/bossd/internal/server/account.go;
// duplicated here because the plugin/CLI boundary must not import daemon
// internals. ONLY this deferred case is treated as a non-failure — the daemon
// also returns live_smoke_ran=false for genuine credential-validation failures
// (malformed/missing blob), which must still offer keep-or-remove.
const liveSmokeUnavailableMarker = "live smoke unavailable"

// classifyTest maps a TestAccount response (and any transport error) to an
// outcome plus a display reason. A transport error is always a hard failure. A
// non-empty last_test_error is deferred (not a failure) ONLY when the live
// smoke never ran AND the detail is the known live-smoke-unavailable degrade;
// any other last_test_error — including credential-validation failures that
// also report live_smoke_ran=false — is a hard failure so the bad credential is
// not silently kept (see credential materialization 1.5).
func classifyTest(resp *pb.TestAccountResponse, err error) (testOutcome, string) {
	if err != nil {
		return testFailed, err.Error()
	}
	detail := resp.GetAccount().GetLastTestError()
	if detail == "" {
		return testVerified, ""
	}
	if !resp.GetLiveSmokeRan() && strings.Contains(detail, liveSmokeUnavailableMarker) {
		return testDeferred, detail
	}
	return testFailed, detail
}

func mentionsKeyring(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "keyring")
}

// maskLine replaces any Claude setup token in s with its masked display form so
// teed CLI output and error text never surface a live token.
func maskLine(s string) string {
	if tok, ok := agentcred.ParseClaudeSetupTokenOutput(s); ok {
		return strings.ReplaceAll(s, tok, agentcred.MaskToken(tok))
	}
	return s
}

func lastN(s []string, n int) []string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// tempDir creates a 0700 temp directory matching pattern.
func tempDir(pattern string) (string, error) {
	dir, err := os.MkdirTemp("", pattern)
	if err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}
