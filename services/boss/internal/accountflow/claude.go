package accountflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
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
// (or a pasted token), then stores and verifies it through the daemon.
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

type claudeRelayFilter struct {
	lastEmitted string
	seen        map[string]bool
}

var claudeANSIEscapeRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

var claudeRelayMilestones = []struct {
	key       string
	prefix    string
	milestone string
}{
	{key: "welcome", prefix: "welcome to claude code", milestone: "Starting Claude sign-in..."},
	{key: "opening-browser", prefix: "opening browser to sign in", milestone: "Opening browser for Claude sign-in..."},
}

func (f *claudeRelayFilter) push(line string) (string, bool) {
	normalized := claudeANSIEscapeRE.ReplaceAllString(line, "")
	normalized = strings.ReplaceAll(normalized, "\r", "")
	normalized = strings.TrimSpace(normalized)
	if normalized == "" {
		return "", false
	}

	lower := strings.ToLower(normalized)
	for _, m := range claudeRelayMilestones {
		if !strings.HasPrefix(lower, m.prefix) {
			continue
		}
		if f.seen == nil {
			f.seen = make(map[string]bool)
		}
		if f.seen[m.key] {
			return "", false
		}
		f.seen[m.key] = true
		if m.milestone == f.lastEmitted {
			return "", false
		}
		f.lastEmitted = m.milestone
		return m.milestone, true
	}

	masked := maskLine(normalized)
	if masked == f.lastEmitted {
		return "", false
	}
	f.lastEmitted = masked
	return masked, true
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
		var filter claudeRelayFilter
		for line := range proc.Lines() {
			buf.WriteString(line)
			buf.WriteByte('\n')
			all = append(all, line)
			if msg, ok := filter.push(line); ok {
				o.Prompter.Say("%s", msg)
			}
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

// promptIdentity resolves the label + optional email metadata for a new
// account. A non-empty flagLabel/flagEmail (supplied via --label/--email) is
// used verbatim without prompting. Empty label defaults to "<provider>-<n+1>" in
// non-interactive mode or prompts in interactive mode. Email is Boss metadata
// only: never prompted, never auto-derived, and never used for collisions.
func promptIdentity(ctx context.Context, p Prompter, c AccountClient, provider, flagLabel, flagEmail, _ string, nonInteractive bool) (string, string, error) {
	existing, err := c.ListAccounts(ctx, provider, false)
	if err != nil {
		return "", "", fmt.Errorf("could not list existing %s accounts: %w", provider, err)
	}

	defaultLabel := nextDefaultLabel(provider, existing)
	label, err := resolveLabel(p, flagLabel, defaultLabel, nonInteractive)
	return label, flagEmail, err
}

func nextDefaultLabel(provider string, existing []*pb.Account) string {
	used := make(map[string]struct{}, len(existing))
	for _, acct := range existing {
		used[acct.GetLabel()] = struct{}{}
	}
	for i := 1; ; i++ {
		label := fmt.Sprintf("%s-%d", provider, i)
		if _, ok := used[label]; !ok {
			return label
		}
	}
}

// resolveLabel uses flagLabel verbatim when supplied, otherwise prompts with
// defaultLabel. It is the no-collision path of promptIdentity. When
// nonInteractive is set, an empty flagLabel resolves to defaultLabel without
// prompting a stdin that the piped token already consumed.
func resolveLabel(p Prompter, flagLabel, defaultLabel string, nonInteractive bool) (string, error) {
	if flagLabel != "" {
		return flagLabel, nil
	}
	if nonInteractive {
		return defaultLabel, nil
	}
	label, err := p.Ask("Label for this account", defaultLabel)
	if err != nil {
		return "", err
	}
	return label, nil
}

// storeAndTest adds the credential through the daemon then verifies it,
// offering keep-or-remove when verification fails.
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
	case testFailed:
		return keepOrRemove(ctx, p, c, id, label, reason, testErr)
	}
	return nil
}

// keepOrRemove handles failed account verification. It offers keep-or-remove
// for the just-registered account and removes it when the operator declines.
func keepOrRemove(ctx context.Context, p Prompter, c AccountClient, id, label, reason string, testErr error) error {
	keep, cerr := p.Confirm(fmt.Sprintf("Account verification failed (%s). Keep the account anyway?", reason), false)
	if cerr != nil {
		return cerr
	}
	if keep {
		p.Say("Account %q stored without verification; rotation will retry verification later.", label)
		return nil
	}
	if rerr := c.RemoveAccount(ctx, id); rerr != nil {
		p.Say("warning: could not remove unverified account %s: %v", id, rerr)
	}
	if testErr != nil {
		return testErr
	}
	return fmt.Errorf("account verification failed: %s", reason)
}

// --- small helpers ---------------------------------------------------------

// testOutcome classifies a TestAccount result.
type testOutcome int

const (
	// testVerified: the credential is stored and verification passed (or
	// there is simply nothing to report).
	testVerified testOutcome = iota
	// testFailed: a transport error, or credential verification reported an error.
	testFailed
)

// classifyTest maps a TestAccount response (and any transport error) to an
// outcome plus a display reason. A transport error is always a hard failure. A
// non-empty last_test_error is a hard failure so a bad or unverified credential
// is not silently kept.
func classifyTest(resp *pb.TestAccountResponse, err error) (testOutcome, string) {
	if err != nil {
		return testFailed, err.Error()
	}
	detail := resp.GetAccount().GetLastTestError()
	if detail == "" {
		return testVerified, ""
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
