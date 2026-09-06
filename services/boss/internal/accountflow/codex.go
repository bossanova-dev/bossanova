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

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
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
	stored, err := captureCodexCredential(ctx, &o, "boss account add codex")
	if err != nil {
		return err
	}

	// Codex registration always runs against an interactive TTY (the device flow
	// has no --token-stdin path), so identity prompting stays enabled.
	label, err := promptIdentity(ctx, o.Prompter, o.Client, "codex", o.Label, false)
	if err != nil {
		return err
	}
	return storeAndTest(ctx, o.Prompter, o.Client, "codex", label, o.Priority, stored, "")
}

// RunCodexReauth replaces an EXISTING Codex account's stored credential in
// place, by driving the same isolated device login `boss account add codex`
// uses and handing the result to the daemon's RefreshAccount (BOS-1142).
//
// Refresh, not add, is the whole point. Registering a second account would
// leave the failed row in place with its label, its priority and every session
// binding still pointing at the credential the provider just rejected, and an
// operator who believed they had fixed it would now own two rows and no way to
// tell which one their sessions use. Refreshing keeps the identity and swaps
// only the secret, so the bindings that were broken are the bindings that
// recover.
func RunCodexReauth(ctx context.Context, o CodexOptions, accountID string) error {
	// The account id is a positional argument rather than a CodexOptions field
	// so it cannot be forgotten: a reauth with an empty id is a compile-time
	// impossibility here, where a zero-valued struct field would silently
	// become a request the daemon rejects at the far end.
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return errors.New("account id is required to reauthenticate")
	}

	// Confirm the target BEFORE driving the login. The device flow costs the
	// operator a browser round-trip and a real provider session; discovering
	// afterwards that the id was a typo, or that it names a Claude account this
	// flow cannot re-acquire a credential for, wastes all of it. Refusing early
	// is also the only way the error can name the right alternative.
	if err := requireCodexAccount(ctx, o.Client, accountID); err != nil {
		return err
	}

	stored, err := captureCodexCredential(ctx, &o, "boss account reauth "+accountID)
	if err != nil {
		return err
	}

	// TestAfterSave is not optional here. This flow exists to recover an account
	// whose credential was rejected, so "saved" is not the question being asked
	// — "usable" is (BOS-943: a save returning nil does not prove anything is
	// usable). The daemon performs the post-save verification under the same
	// credential lock it wrote under and reports the verdict back; the success
	// message below is gated on that verdict, never on err == nil.
	resp, refreshErr := o.Client.RefreshAccount(ctx, &pb.RefreshAccountRequest{
		Id:            accountID,
		Credential:    stored,
		TestAfterSave: true,
	})
	outcome, reason := classifyRefresh(resp, refreshErr)
	switch outcome {
	case testVerified:
		// The account state RefreshAccount returns is what decides the
		// eligibility half of the line. A verified credential is not the same
		// thing as a bindable account: the daemon replaces the secret and
		// restores health, but it never enables a disabled row and never clears
		// a cooldown, and the TUI offers [R] on every codex row including those.
		o.Prompter.Say("%s", ReauthVerifiedLine(accountID, resp.GetAccount(), time.Now()))
		return nil
	case testUnavailable:
		o.Prompter.Say("%s", ReauthUnverifiedLine(accountID, reason))
		return nil
	default:
		// The new credential is already stored — the old one is gone and there
		// is nothing to roll back to — so this is a report, not a cleanup. The
		// account stays failed until a reauthentication verifies, which is the
		// honest state and is exactly what keeps rotation from selecting it.
		if refreshErr != nil {
			return fmt.Errorf("reauthenticate account %s: %w", accountID, refreshErr)
		}
		return fmt.Errorf("reauthenticate account %s: the new credential was stored but verification failed (%s); the account stays unavailable until a reauthentication verifies", accountID, reason)
	}
}

// reauthVerifiedMarker is the half of the verified verdict that ASSERTS
// verification, and ONLY verification. It is a constant rather than an inline
// format string because the TUI has to tell a verified verdict from an
// unverified one to pick the tier it reports the line at, and the only artefact
// that crosses that boundary is the Say line itself: deriving the tier from a
// phrase re-typed at the call site would let the two drift silently, and the
// drift would fail OPEN — an unverified reauthentication reported in the
// neutral tier.
//
// It used to read "…verified and the account is eligible again", which asserted
// something the flow had not established: RefreshAccount replaces the credential
// and restores health, but it does not enable a disabled account and does not
// clear a cooldown, and [R] is offered on every codex row. The eligibility claim
// is now a separate clause DERIVED from the account state the daemon returns.
const reauthVerifiedMarker = "; the new credential verified."

// reauthEligibleClause is appended when the returned account state says the
// account can be bound right now.
const reauthEligibleClause = " The account is eligible again."

// ReauthVerifiedLine is the closing verdict for a reauthentication whose new
// credential the daemon verified after saving it.
//
// acct is the account row RefreshAccount returned (post-save, with health
// already restored) and now is the instant to judge a cooldown against. A nil
// acct — an older daemon, or a response shape that carried no row — yields the
// verification claim alone: unknown eligibility is not eligibility.
func ReauthVerifiedLine(accountID string, acct *pb.Account, now time.Time) string {
	return "Reauthenticated account " + accountID + reauthVerifiedMarker + reauthEligibilityClause(acct, now)
}

// Account state tokens, mirrored from lib/bossalib/models (AccountStatusActive,
// AccountHealthOK, AccountHealthFailed, AccountStatusDisabled,
// AuthCheckOutcomeAuthInvalid). The boss CLI keeps its own copies of these
// literals rather than importing the daemon's model package — the same mirror
// services/boss/cmd/account.go and internal/views/account_actions.go keep.
const (
	accountStatusActive         = "active"
	accountStatusDisabled       = "disabled"
	accountHealthOK             = "ok"
	accountHealthFailed         = "failed"
	authCheckOutcomeAuthInvalid = "auth_invalid"
)

// reauthEligibilityClause renders what the returned account state says about
// binding, or "" when it says nothing this flow is entitled to turn into a
// claim.
//
// The conditions mirror bossd's rotation.BindableNow — isSelectable (status
// active, health ok, not benched by a durable auth_invalid verdict) plus no
// live cooldown; internal/views.switchAccountDisabledReason mirrors the same
// predicate off the same proto fields for the switch picker. They are
// re-derived rather than shared because rotation lives in bossd's internal
// tree and this is the boss CLI.
//
// Every arm is fail-closed: a token neither side recognises (a newer daemon
// adding a status or health value) reaches the last case and produces NO
// eligibility clause at all, because the failure that matters here is claiming
// eligibility for an account rotation will decline — the operator walks away
// believing the row is back in service.
func reauthEligibilityClause(a *pb.Account, now time.Time) string {
	if a == nil {
		return ""
	}
	ineligible := func(because string) string {
		return " The account is still " + because + ", so rotation will not select it yet."
	}
	switch {
	case a.GetStatus() == accountStatusDisabled:
		return ineligible("disabled")
	case a.GetHealth() == accountHealthFailed:
		return ineligible("marked failed")
	case a.GetAuthCheck().GetOutcome() == authCheckOutcomeAuthInvalid:
		// RecordAuthCheck deliberately leaves Health alone, so this verdict is
		// invisible to a status+health test and has to be read directly.
		return ineligible("benched by its last credential check")
	case a.GetCooldownUntil() != nil && a.GetCooldownUntil().AsTime().After(now):
		return ineligible("cooling down")
	case a.GetStatus() != accountStatusActive || a.GetHealth() != accountHealthOK:
		return ""
	default:
		return reauthEligibleClause
	}
}

// ReauthUnverifiedLine is the closing verdict for a reauthentication whose new
// credential was stored but NOT verified — the check could not run, or reached
// no verdict. The credential may well be fine; nothing has established that.
func ReauthUnverifiedLine(accountID, reason string) string {
	return fmt.Sprintf("Reauthenticated account %s. Verification couldn't run right now (%s); it will run on first use or the next rotation.", accountID, reason)
}

// ReauthLineIsVerified reports whether line is a reauthentication verdict that
// asserts the new credential VERIFIED. Everything else — the unverified
// verdict, an error, an unrelated line, an empty string — is false, so a caller
// choosing how loudly to report the verdict fails closed on anything it does
// not recognise.
//
// It tests for the marker anywhere in the line rather than at the end: the
// eligibility clause now follows it, and only the verification claim decides
// the tier. The unverified verdict does not contain the marker, so the
// fail-closed direction is unchanged.
func ReauthLineIsVerified(line string) bool {
	return strings.Contains(line, reauthVerifiedMarker)
}

// requireCodexAccount resolves accountID in the registry and refuses anything
// this flow cannot re-acquire a credential for.
func requireCodexAccount(ctx context.Context, c AccountClient, accountID string) error {
	// An empty provider filter lists every provider, which is what lets a
	// mistyped id be told apart from a Claude id — the two need different
	// advice.
	accounts, err := c.ListAccounts(ctx, "", false)
	if err != nil {
		return fmt.Errorf("look up account %s: %w", accountID, err)
	}
	for _, a := range accounts {
		if a.GetId() != accountID {
			continue
		}
		if provider := strings.ToLower(a.GetProvider()); provider != "codex" {
			return fmt.Errorf("account %s is a %s account; this flow re-runs the Codex device login. Use `boss account refresh %s` with a replacement credential instead", accountID, provider, accountID)
		}
		return nil
	}
	return fmt.Errorf("no account with id %s (run `boss account ls` to see the ids)", accountID)
}

// classifyRefresh maps a RefreshAccount response to the same three-outcome
// verdict classifyTest produces, so reauthentication does not invent a fourth
// notion of success. The daemon's own post-save read-back is what populates
// these fields; this function only reads its verdict.
func classifyRefresh(resp *pb.RefreshAccountResponse, err error) (testOutcome, string) {
	if err != nil {
		return testFailed, err.Error()
	}
	if resp == nil {
		// No response and no error is not a verdict, and the zero value of
		// testOutcome is "verified" — so say so explicitly rather than letting
		// the fallthrough grant a success nobody reported.
		return testFailed, "the daemon returned no refresh result"
	}
	// live_smoke_ran is the ONLY field that says verification actually happened,
	// so it gates the verified verdict rather than merely selecting between two
	// sentinel details. Verification that could not run, or ran against bytes
	// that were replaced underneath it, is not a credential failure — the daemon
	// reports both with live_smoke_ran=false and a sentinel detail and treats
	// neither as evidence against the credential, so neither may fail the flow
	// here either. But it is not a SUCCESS either: an unrecognised
	// live_smoke_ran=false shape (a detail a newer daemon added, or none at all)
	// used to fall through to testVerified and make this flow announce a
	// credential "verified" when no verification ever ran. It now defaults to
	// unavailable, which is the honest verdict for "the check did not happen"
	// and is the direction that fails closed: the account keeps whatever state
	// the daemon recorded, and the next use or rotation checks it for real.
	if !resp.GetLiveSmokeRan() {
		// A recorded credential fault still outranks "no smoke ran": the daemon
		// wrote a rejection onto the row, and reporting that as merely
		// unverified would drop the one verdict that names a remedy.
		if detail := resp.GetAccount().GetLastTestError(); detail != "" &&
			resp.GetDetail() != liveSmokeUnavailableDetail && resp.GetDetail() != liveSmokeInconclusiveDetail {
			return testFailed, detail
		}
		if detail := resp.GetDetail(); detail != "" {
			return testUnavailable, detail
		}
		return testUnavailable, "the daemon reported no verification result"
	}
	if detail := resp.GetAccount().GetLastTestError(); detail != "" {
		return testFailed, detail
	}
	return testVerified, ""
}

// captureCodexCredential drives the isolated device login and returns the
// canonical account-store credential shape ({access,refresh,id_token}) rather
// than the raw auth.json: both the add and the reauth path verify immediately
// through the daemon, which validates codex credentials in that flat shape.
// The 1.5 materializer owns merge semantics from there.
//
// rerunCmd is the exact command to re-run, quoted back to the operator when the
// device flow is disabled — the add and reauth paths are not the same command.
func captureCodexCredential(ctx context.Context, o *CodexOptions, rerunCmd string) ([]byte, error) {
	if o.CodexBin == "" {
		o.CodexBin = "codex"
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultFlowTimeout
	}
	if o.HomeDir == nil {
		o.HomeDir = func() (string, error) { return tempDir("boss-account-codex-*") }
	}

	dir, cleanup, err := isolatedCodexHome(o)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	blob, err := codexCapture(ctx, *o, dir)
	if err != nil {
		if errors.Is(err, errCodexDeviceAuthDisabled) {
			o.Prompter.Say("Codex device-code login is disabled for this ChatGPT account.")
			o.Prompter.Say("Enable it, then re-run this command:")
			o.Prompter.Say("  1. Open https://chatgpt.com/#settings and go to the Security section.")
			o.Prompter.Say("  2. Turn on \"Enable device code authorization for Codex\".")
			o.Prompter.Say("  3. Re-run: %s", rerunCmd)
		}
		return nil, err
	}

	auth, err := agentcred.ValidateCodexAuthJSON(blob)
	if err != nil {
		if errors.Is(err, agentcred.ErrCodexAuthMissingField) {
			return nil, fmt.Errorf("codex login did not produce a complete credential (id_token is required for token refresh); nothing stored: %w", err)
		}
		return nil, fmt.Errorf("codex auth.json is invalid; nothing stored: %w", err)
	}
	stored, err := agentcred.CodexAccountStoreJSON(auth)
	if err != nil {
		return nil, fmt.Errorf("could not encode codex credential for storage; nothing stored: %w", err)
	}
	return stored, nil
}

// isolatedCodexHome creates the private CODEX_HOME the device login writes
// auth.json into, and returns the cleanup that MUST run on every exit path — a
// leaked temp dir here is a leaked live credential.
func isolatedCodexHome(o *CodexOptions) (string, func(), error) {
	dir, err := o.HomeDir()
	if err != nil {
		return "", nil, err
	}
	// The dir MUST pre-exist and be private before spawn (auth.json holds live
	// tokens).
	// dir is a directory: 0700 is least-privilege and owner-execute is required
	// to traverse the CODEX_HOME dir, so a stricter 0600 would make it unusable.
	// #nosec G302 -- Chmod(dir,0o700) on the private CODEX_HOME cred dir; 0700 is least-privilege
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	if err := os.Chmod(dir, 0o700); err != nil {
		// The dir exists but could not be secured, so it must still go.
		if rerr := os.RemoveAll(dir); rerr != nil {
			o.Prompter.Say("warning: could not remove temp CODEX_HOME %s (delete it manually — it may hold credentials): %v", dir, rerr)
		}
		return "", nil, fmt.Errorf("could not secure temp CODEX_HOME %s: %w", dir, err)
	}
	return dir, func() {
		if rerr := os.RemoveAll(dir); rerr != nil {
			o.Prompter.Say("warning: could not remove temp CODEX_HOME %s (delete it manually — it may hold credentials): %v", dir, rerr)
		}
	}, nil
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
