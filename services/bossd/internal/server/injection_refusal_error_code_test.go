package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/session"
)

// secretish is shaped like the provider response bodies InjectionError is
// documented as able to wrap: agenterr.Redact masks the bearer token, so its
// survival in an RPC message is the leak these tests exist to catch.
const secretish = `refresh failed: {"error":"invalid_grant"} Authorization: Bearer ya29.SUPERSECRET`

func boundRefusal(outcome account.InjectionOutcome) *account.InjectionError {
	return &account.InjectionError{
		AccountID: "acct-codex-2",
		Provider:  "codex",
		Outcome:   outcome,
		Reason:    "credential injection failed",
		Err:       errors.New(secretish),
	}
}

// assertRefusal is the shared contract for every boundary below: a bound
// account that cannot be injected is the caller's precondition, not a daemon
// fault, and the message an operator reads must not carry the provider's raw
// body.
func assertRefusal(t *testing.T, got error) {
	t.Helper()
	if got == nil {
		t.Fatal("a credential-injection refusal must not be dropped")
	}
	if connect.CodeOf(got) != connect.CodeFailedPrecondition {
		t.Errorf("code = %v, want %v", connect.CodeOf(got), connect.CodeFailedPrecondition)
	}
	if strings.Contains(got.Error(), "ya29.SUPERSECRET") {
		t.Errorf("the provider's raw body reached the RPC message: %q", got.Error())
	}
	if !strings.Contains(got.Error(), "acct-codex-2") {
		t.Errorf("the refusal must still name the account, got %q", got.Error())
	}
}

// Both outcomes map the same way on purpose. Splitting Undetermined off to
// Unavailable would promise a retry contract the rest of this surface does not
// honour, and neither shipped precedent does it.
func TestInjectionRefusal_CreateSessionIsFailedPrecondition(t *testing.T) {
	for _, outcome := range []account.InjectionOutcome{
		account.InjectionOutcomeInvalid,
		account.InjectionOutcomeUndetermined,
	} {
		// Wrapped the way lifecycle actually hands it up.
		err := fmt.Errorf("start session: %w",
			fmt.Errorf("resolve account env for agent %q: %w", "codex", boundRefusal(outcome)))
		assertRefusal(t, createSessionConnectError(err))
	}
}

func TestInjectionRefusal_ResurrectSessionIsFailedPrecondition(t *testing.T) {
	assertRefusal(t, resurrectSessionConnectError(
		fmt.Errorf("resolve account env: %w", boundRefusal(account.InjectionOutcomeInvalid))))
}

func TestInjectionRefusal_WakeChatIsFailedPrecondition(t *testing.T) {
	assertRefusal(t, wakeChatErrorToConnect(
		fmt.Errorf("spawn chat: %w", boundRefusal(account.InjectionOutcomeInvalid))))
}

// The dispatcher leg carries an enum rather than a connect code, and bosso
// turns ERROR_CODE_UNSPECIFIED into Aborted — further from the truth than the
// Internal it replaced, which is why this leg is pinned separately.
func TestInjectionRefusal_WakeChatStreamCodeAndRedaction(t *testing.T) {
	err := fmt.Errorf("spawn chat: %w", boundRefusal(account.InjectionOutcomeInvalid))

	code, mapped, ok := injectionRefusalCommandCode(err, "spawn")
	if !ok {
		t.Fatal("the dispatcher leg did not recognise the refusal")
	}
	if code != pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION {
		t.Errorf("code = %v, want FAILED_PRECONDITION", code)
	}
	if strings.Contains(mapped.Error(), "ya29.SUPERSECRET") {
		t.Errorf("the provider's raw body reached the dispatcher: %q", mapped.Error())
	}

	// The default arm must stay reachable on this leg too.
	if _, _, ok := injectionRefusalCommandCode(errors.New("tmux died"), "spawn"); ok {
		t.Error("an unrelated failure must not be classified as a precondition")
	}
}

// The default arms must still be reachable, or the tests above would pass on a
// mapper that answered FailedPrecondition for everything.
func TestInjectionRefusal_UnrelatedErrorsKeepTheirCodes(t *testing.T) {
	plain := errors.New("fatal: not a git repository")
	if got := createSessionConnectError(plain); connect.CodeOf(got) != connect.CodeInternal {
		t.Errorf("createSession: code = %v, want Internal", connect.CodeOf(got))
	}
	if got := resurrectSessionConnectError(plain); connect.CodeOf(got) != connect.CodeInternal {
		t.Errorf("resurrect: code = %v, want Internal", connect.CodeOf(got))
	}
	if got := wakeChatErrorToConnect(plain); connect.CodeOf(got) != connect.CodeInternal {
		t.Errorf("wakeChat: code = %v, want Internal", connect.CodeOf(got))
	}
}

// TestInjectionRefusal_SwitchAccountIsFailedPrecondition covers the fifth spawn
// edge. Both switch entry points — the SwitchSessionAccount RPC and the
// "/boss switch" chat command — return executeAccountSwitch's error verbatim, so
// its arms are the only thing that decides what a switch reports when the
// respawn refuses to inject the account it just rebound to.
//
// The injected error is shaped the way SwitchAccount actually hands it up:
// StartTmuxChat's "resolve account env for session ..." refusal, re-wrapped once
// by the switch's own "respawn chat after switch" frame.
func TestInjectionRefusal_SwitchAccountIsFailedPrecondition(t *testing.T) {
	for _, outcome := range []account.InjectionOutcome{
		account.InjectionOutcomeInvalid,
		account.InjectionOutcomeUndetermined,
	} {
		s := newInjectionRefusalSwitchServer(fmt.Errorf("respawn chat after switch: %w",
			fmt.Errorf("resolve account env for session %s: %w", "sess-1", boundRefusal(outcome))))
		_, err := s.executeAccountSwitch(context.Background(), "sess-1", "agent-1", "acct-codex-2", false)
		assertRefusal(t, err)
	}
}

// The switch's default arm must stay reachable, or the test above would pass on
// a mapper that answered FailedPrecondition for every switch failure.
func TestInjectionRefusal_SwitchAccountKeepsItsDefaultArm(t *testing.T) {
	s := newInjectionRefusalSwitchServer(errors.New("tmux server died"))
	_, err := s.executeAccountSwitch(context.Background(), "sess-1", "agent-1", "acct-codex-2", false)
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("switch: code = %v, want Internal", connect.CodeOf(err))
	}
}

// newInjectionRefusalSwitchServer is the production shape of the switch route:
// only the switchAccountFn seam is wired, so the flight runs unmodified and the
// error the seam returns is exactly what executeAccountSwitch must classify.
func newInjectionRefusalSwitchServer(err error) *Server {
	return &Server{
		logger: zerolog.Nop(),
		switchAccountFn: func(context.Context, session.SwitchAccountParams) (session.SwitchAccountResult, error) {
			return session.SwitchAccountResult{}, err
		},
	}
}
