package accountflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/recurser/bossalib/agentcred"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// defaultFlowTimeout bounds an interactive registration walkthrough.
const defaultFlowTimeout = 10 * time.Minute

// liveSmokeUnavailableDetail mirrors the bossd sentinel (services/bossd/internal/server,
// account.go) returned when provider verification could not run. Different Go
// module, so the string is duplicated rather than imported (module-boundary convention).
const liveSmokeUnavailableDetail = "provider verification unavailable"

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
	Priority   int32                  // account ordering, from --priority (default 0)

	// StdinUnavailable declares that there is no interactive input left to read,
	// so identity prompts must resolve to their defaults instead of asking. It is
	// the --token-stdin condition: the piped token has already consumed stdin, and
	// a further Ask would hit io.EOF.
	//
	// It is deliberately NOT the same flag as PasteMode. PasteMode says only
	// "obtain the token by pasting rather than by running the CLI", which is also
	// true of a fully interactive caller — the TUI answering "No" to the
	// walkthrough confirm, and the --host TUI flow that cannot spawn a local
	// claude at all. Those callers have a working prompter and must still be asked
	// for a label. Folding the two together made the same screen drop its label
	// prompt based on a flag about a different machine (BOS-847).
	StdinUnavailable bool
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

	// From here on a LIVE credential exists that boss has not stored yet. Every
	// failure below used to return a bare error, so the operator was told the
	// registration failed and never told that a real token had been minted in
	// their account — it was simply dropped (BOS-848). preservedToken carries it
	// across those error boundaries in a form that cannot be formatted back into
	// the raw value, so the recovery advice can name the token without leaking it.
	minted := preservedToken(token)

	// StdinUnavailable — not PasteMode — is what suppresses the label prompt: a
	// pasted token can come from an interactive prompter that is perfectly able to
	// answer one more question.
	label, err := promptIdentity(ctx, o.Prompter, o.Client, "claude", o.Label, o.StdinUnavailable)
	if err != nil {
		// The label step is the reported failure window: the token is already
		// minted and on screen, and this is where the flow used to end silently.
		return minted.recoveryError(err, "while choosing a label for the account")
	}

	// reveal() is the single sanctioned unmasking and feeds only the daemon
	// credential blob; the display argument comes from masked(), so the value
	// that reaches a screen cannot accidentally be the raw one.
	if err := storeAndTest(ctx, o.Prompter, o.Client, "claude", label, o.Priority, []byte(minted.reveal()), minted.masked()); err != nil {
		// Only the paths that left the daemon WITHOUT the credential get the
		// recovery advice. A verification failure the operator chose to keep
		// stored the account, and telling them to re-mint would be wrong.
		if isCredentialNotStored(err) {
			return minted.recoveryError(err, "while storing the account")
		}
		return err
	}
	return nil
}

// preservedToken is a live Claude setup token held across an error boundary.
//
// It is masked BY CONSTRUCTION: Format intercepts every fmt verb — %v, %s, %q
// and %#v alike, because fmt consults Formatter before Stringer, GoStringer or
// its own reflection — so no error path can render the raw value even by
// accident. That matters because the whole point of the type is to travel
// through fmt.Errorf chains that are then logged and rendered on screen.
//
// It is a defined string type rather than a struct so it cannot be copied into
// a wider interface and re-formatted by some other route; obtaining the raw
// value requires the explicit reveal() conversion below, which has exactly one
// call site.
type preservedToken string

// masked is the only display form. Format delegates to it so there is exactly
// one definition of "safe to show", rather than two that could drift.
func (t preservedToken) masked() string { return agentcred.MaskToken(string(t)) }

// Format renders the masked display form for every verb. Implementing
// fmt.Formatter (rather than fmt.Stringer) is what makes the masking total:
// a %q or %#v on a Stringer would print the underlying string.
func (t preservedToken) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, t.masked())
}

// reveal returns the raw token. Deliberately unexported, deliberately named,
// and deliberately not used on any error path — grep for it before adding a
// caller. It has exactly one production call site: the daemon credential blob
// in RunClaudeAdd.
func (t preservedToken) reveal() string { return string(t) }

// recoveryError wraps cause with the operator's recovery path for a live token
// boss minted but did not store.
//
// It names the token only in masked form, so the advice can identify WHICH
// credential was orphaned (the operator sees the same masked form in the
// console's token list) without putting the secret in a terminal, a log file or
// an error string. cause stays wrapped, so errors.Is/As still reach it.
// The message deliberately carries no trailing period: staticcheck ST1005
// rejects an error string ending in punctuation, and this text is an error
// value rather than a Say line.
func (t preservedToken) recoveryError(cause error, phase string) error {
	return fmt.Errorf("%w\n\n"+
		"A Claude setup token (%s) was already created before this failed %s, and boss did not store it. "+
		"It is live in your Anthropic account: revoke it at https://console.anthropic.com/settings/keys "+
		"so it is not left orphaned, then run `boss account add claude` again to register a replacement. "+
		"If you still hold a valid token, answer No at the walkthrough prompt and paste it rather than minting another",
		cause, t, phase)
}

// credentialNotStored marks a store-phase error that left the daemon without
// the credential, so the caller knows the minted token is now unreachable.
//
// Error() delegates verbatim, so wrapping changes no message and no existing
// caller (the codex flow shares storeAndTest and is unaffected).
type credentialNotStored struct{ err error }

func (e *credentialNotStored) Error() string { return e.err.Error() }
func (e *credentialNotStored) Unwrap() error { return e.err }

// markCredentialNotStored tags err as having left nothing stored.
func markCredentialNotStored(err error) error {
	if err == nil {
		return nil
	}
	return &credentialNotStored{err: err}
}

// isCredentialNotStored reports whether err came from a store-phase path that
// stored nothing.
func isCredentialNotStored(err error) bool {
	var marker *credentialNotStored
	return errors.As(err, &marker)
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

// promptIdentity resolves the label for a new account. A non-empty flagLabel
// (supplied via --label) is used verbatim without prompting. Empty label
// defaults to "<provider>-<n+1>" in non-interactive mode or prompts in
// interactive mode.
func promptIdentity(ctx context.Context, p Prompter, c AccountClient, provider, flagLabel string, nonInteractive bool) (string, error) {
	existing, err := c.ListAccounts(ctx, provider, false)
	if err != nil {
		return "", fmt.Errorf("could not list existing %s accounts: %w", provider, err)
	}

	defaultLabel := nextDefaultLabel(provider, existing)
	return resolveLabel(p, flagLabel, defaultLabel, nonInteractive)
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
// prompting a stdin that the piped token already consumed; the claude flow feeds
// that argument from ClaudeOptions.StdinUnavailable (never from PasteMode, which
// says nothing about whether a prompter can still be asked).
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
func storeAndTest(ctx context.Context, p Prompter, c AccountClient, provider, label string, priority int32, blob []byte, display string) error {
	acct, err := c.AddAccount(ctx, &pb.AddAccountRequest{
		Provider:   provider,
		Label:      label,
		Priority:   priority,
		Credential: blob,
	})
	if err != nil {
		// AddAccount failed, so the daemon holds nothing: mark it so the claude
		// flow can tell the operator their freshly minted token is unreachable.
		// The marker delegates Error() verbatim, so no message changes here and
		// the codex flow (which shares this function) is unaffected.
		if mentionsKeyring(err) {
			return markCredentialNotStored(fmt.Errorf("%w\nhint: bossd could not open the system keyring; see BOSS_KEYRING_BACKEND / --allow-insecure-keyring in the daemon docs", err))
		}
		return markCredentialNotStored(err)
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
	case testUnavailable:
		p.Say("Account %q registered. Verification couldn't run right now (%s); it will run on first use or the next rotation.", label, reason)
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
	// The operator declined to keep it, so the account is being removed and the
	// daemon ends up holding nothing — the same "credential is now unreachable"
	// position as an outright AddAccount failure, and marked the same way.
	if rerr := c.RemoveAccount(ctx, id); rerr != nil {
		p.Say("warning: could not remove unverified account %s: %v", id, rerr)
	}
	if testErr != nil {
		return markCredentialNotStored(testErr)
	}
	return markCredentialNotStored(fmt.Errorf("account verification failed: %s", reason))
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
	// testUnavailable: verification could not run (no agent plugin loaded, or no
	// smoke runner wired). Not a credential failure — keep the account silently.
	testUnavailable
)

// classifyTest maps a TestAccount response (and any transport error) to an
// outcome plus a display reason. A transport error is always a hard failure. A
// non-empty last_test_error is a hard failure so a bad or unverified credential
// is not silently kept.
func classifyTest(resp *pb.TestAccountResponse, err error) (testOutcome, string) {
	if err != nil {
		return testFailed, err.Error()
	}
	// Verification that could not run (no agent plugin loaded, or no smoke runner
	// wired) is reported as live_smoke_ran=false with the sentinel detail. It is
	// not a credential failure — keep the account and let rotation verify later.
	// Note: live_smoke_ran=false alone is NOT enough; a malformed-credential
	// validation failure is also live_smoke_ran=false but carries a different
	// detail and must still route to testFailed.
	if !resp.GetLiveSmokeRan() && resp.GetDetail() == liveSmokeUnavailableDetail {
		return testUnavailable, resp.GetDetail()
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

// MaskLine is maskLine, exported for the TUI's terminal-handoff passthrough.
//
// During a tea.Exec handoff the child's output is relayed straight to the
// operator's terminal, which is the one path where an unmasked line would land
// in their scrollback. That relay must mask with exactly the same rule the flow
// uses internally, so it calls this rather than reimplementing it (BOS-848).
func MaskLine(s string) string { return maskLine(s) }

// RelayDisplayLine prepares one raw child line for display on the operator's
// terminal during a tea.Exec handoff: ANSI stripped, carriage returns removed,
// trimmed, and any setup token masked. ok is false when the line carries
// nothing worth showing.
//
// It exists so the handoff relay normalises EXACTLY as claudeRelayFilter does
// for the TUI progress log. `claude setup-token` is an Ink app that repaints its
// whole frame on every spinner tick, so relaying raw lines verbatim replays the
// banner once per frame — the cursor-control sequences that would have
// overwritten in place are meaningless once each line is re-emitted with its own
// newline. Stripping them, plus the caller's de-duplication, is what turns that
// back into a readable log (BOS-848).
//
// Deliberately NOT applied to the Proc's Lines() channel, which must stay raw:
// ParseClaudeSetupTokenOutput needs the real token to extract it.
func RelayDisplayLine(s string) (string, bool) {
	normalized := claudeANSIEscapeRE.ReplaceAllString(s, "")
	normalized = strings.ReplaceAll(normalized, "\r", "")
	normalized = strings.TrimSpace(normalized)
	if normalized == "" {
		return "", false
	}
	return maskLine(normalized), true
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
	// dir is a directory: 0700 is least-privilege (owner rwx, group/other none)
	// and the owner-execute bit is required to traverse it, so 0600 is unusable.
	// #nosec G302 -- Chmod(dir,0o700) on a private temp dir; least-privilege
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}
