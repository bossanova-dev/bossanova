package server

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// notifyAuthRepoStoreFake is the smallest RepoStore the login path needs: the
// handler only enumerates repo IDs before delegating.
type notifyAuthRepoStoreFake struct {
	db.RepoStore
	repos   []*models.Repo
	listErr error
}

func (f *notifyAuthRepoStoreFake) List(context.Context) ([]*models.Repo, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.repos, nil
}

// notifyAuthNotifierFake records what the handler asked for and returns the
// verdict the test stages.
type notifyAuthNotifierFake struct {
	verdict     LoginVerdict
	err         error
	loginCalls  int
	logoutCalls int
	gotRepoIDs  []string
}

func (f *notifyAuthNotifierFake) NotifyLogin(_ context.Context, repoIDs []string) (LoginVerdict, error) {
	f.loginCalls++
	f.gotRepoIDs = repoIDs
	return f.verdict, f.err
}

func (f *notifyAuthNotifierFake) NotifyLogout() { f.logoutCalls++ }

func notifyAuthServer(notifier AuthNotifier) *Server {
	return &Server{
		repos:        &notifyAuthRepoStoreFake{repos: []*models.Repo{{ID: "repo-1"}, {ID: "repo-2"}}},
		authNotifier: notifier,
		logger:       zerolog.Nop(),
	}
}

// TestNotifyAuthChange_LoginReturnsTheDaemonVerdict is the BOS-945 wire
// contract: whatever the daemon concluded about the credentials it reloaded
// has to reach the caller, because `boss login` is the only place an operator
// will see it.
func TestNotifyAuthChange_LoginReturnsTheDaemonVerdict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		verdict     LoginVerdict
		err         error
		wantOutcome pb.NotifyAuthChangeResponse_Outcome
		wantReason  string
	}{
		{
			name:        "clean login",
			verdict:     LoginVerdict{Outcome: LoginOutcomeOK},
			wantOutcome: pb.NotifyAuthChangeResponse_OUTCOME_OK,
		},
		{
			name:        "flagged credentials carry their reason",
			verdict:     LoginVerdict{Outcome: LoginOutcomeCredentialsFlagged, ReloginReason: "refresh_token_rejected"},
			err:         errors.New("credentials still unusable after login"),
			wantOutcome: pb.NotifyAuthChangeResponse_OUTCOME_CREDENTIALS_FLAGGED,
			wantReason:  "refresh_token_rejected",
		},
		{
			name:        "missing credentials have no reason to carry",
			verdict:     LoginVerdict{Outcome: LoginOutcomeCredentialsMissing},
			err:         errors.New("credentials still unusable after login"),
			wantOutcome: pb.NotifyAuthChangeResponse_OUTCOME_CREDENTIALS_MISSING,
		},
		{
			name:        "register failure is reported, not swallowed",
			verdict:     LoginVerdict{Outcome: LoginOutcomeRegisterFailed},
			err:         errors.New("register boom"),
			wantOutcome: pb.NotifyAuthChangeResponse_OUTCOME_REGISTER_FAILED,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			notifier := &notifyAuthNotifierFake{verdict: tc.verdict, err: tc.err}
			s := notifyAuthServer(notifier)

			resp, err := s.NotifyAuthChange(context.Background(), connect.NewRequest(&pb.NotifyAuthChangeRequest{Action: "login"}))
			// A notifier error must never fail the RPC: the caller is
			// `boss login`, and the login itself succeeded.
			if err != nil {
				t.Fatalf("NotifyAuthChange returned an RPC error: %v", err)
			}
			if got := resp.Msg.GetOutcome(); got != tc.wantOutcome {
				t.Errorf("outcome = %v, want %v", got, tc.wantOutcome)
			}
			if got := resp.Msg.GetReloginReason(); got != tc.wantReason {
				t.Errorf("relogin_reason = %q, want %q", got, tc.wantReason)
			}
			if notifier.loginCalls != 1 {
				t.Errorf("NotifyLogin calls = %d, want 1", notifier.loginCalls)
			}
			if len(notifier.gotRepoIDs) != 2 {
				t.Errorf("repo IDs passed = %v, want both repos", notifier.gotRepoIDs)
			}
		})
	}
}

// A daemon with no orchestrator configured evaluated nothing, so it must report
// OUTCOME_UNSPECIFIED. That is what the CLI renders as silence — reporting OK
// here would invent a verdict nobody reached.
func TestNotifyAuthChange_NoNotifierReportsUnspecified(t *testing.T) {
	t.Parallel()

	s := notifyAuthServer(nil)
	s.authNotifier = nil

	resp, err := s.NotifyAuthChange(context.Background(), connect.NewRequest(&pb.NotifyAuthChangeRequest{Action: "login"}))
	if err != nil {
		t.Fatalf("NotifyAuthChange: %v", err)
	}
	if got := resp.Msg.GetOutcome(); got != pb.NotifyAuthChangeResponse_OUTCOME_UNSPECIFIED {
		t.Errorf("outcome = %v, want OUTCOME_UNSPECIFIED", got)
	}
}

// Logout evaluates no credentials, so it has no verdict — and must not borrow
// the previous login's.
func TestNotifyAuthChange_LogoutReportsUnspecified(t *testing.T) {
	t.Parallel()

	notifier := &notifyAuthNotifierFake{verdict: LoginVerdict{Outcome: LoginOutcomeOK}}
	s := notifyAuthServer(notifier)

	resp, err := s.NotifyAuthChange(context.Background(), connect.NewRequest(&pb.NotifyAuthChangeRequest{Action: "logout"}))
	if err != nil {
		t.Fatalf("NotifyAuthChange: %v", err)
	}
	if got := resp.Msg.GetOutcome(); got != pb.NotifyAuthChangeResponse_OUTCOME_UNSPECIFIED {
		t.Errorf("outcome = %v, want OUTCOME_UNSPECIFIED", got)
	}
	if notifier.logoutCalls != 1 {
		t.Errorf("NotifyLogout calls = %d, want 1", notifier.logoutCalls)
	}
	if notifier.loginCalls != 0 {
		t.Errorf("NotifyLogin calls = %d, want 0 on logout", notifier.loginCalls)
	}
}

func TestNotifyAuthChange_RejectsUnknownAction(t *testing.T) {
	t.Parallel()

	notifier := &notifyAuthNotifierFake{}
	s := notifyAuthServer(notifier)
	_, err := s.NotifyAuthChange(context.Background(), connect.NewRequest(&pb.NotifyAuthChangeRequest{Action: "refresh"}))
	if err == nil {
		t.Fatal("NotifyAuthChange accepted an unknown action")
	}
	// The code, not merely the presence of an error: a caller distinguishes
	// "you asked for something that does not exist" from a transport or
	// internal failure by the code, and only InvalidArgument says do not retry.
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want CodeInvalidArgument", got)
	}
	if notifier.loginCalls != 0 || notifier.logoutCalls != 0 {
		t.Errorf("unknown action reached the notifier: login=%d logout=%d", notifier.loginCalls, notifier.logoutCalls)
	}
}

// TestLoginVerdictToProto_ReloginReasonOnlyRidesTheFlaggedOutcome pins the
// contract daemon.proto publishes for relogin_reason. This mapper is the only
// wire boundary, so enforcing it here is what makes the documented invariant
// true for every producer rather than for the ones that happen to behave.
func TestLoginVerdictToProto_ReloginReasonOnlyRidesTheFlaggedOutcome(t *testing.T) {
	t.Parallel()

	for _, outcome := range []LoginOutcome{
		LoginOutcomeUnspecified,
		LoginOutcomeOK,
		LoginOutcomeCredentialsMissing,
		LoginOutcomeRegisterFailed,
	} {
		got := loginVerdictToProto(LoginVerdict{Outcome: outcome, ReloginReason: "refresh_token_rejected"})
		if got.GetReloginReason() != "" {
			t.Errorf("outcome %v carried relogin_reason %q; the proto documents it as empty here", outcome, got.GetReloginReason())
		}
	}

	got := loginVerdictToProto(LoginVerdict{
		Outcome:       LoginOutcomeCredentialsFlagged,
		ReloginReason: "refresh_token_rejected",
	})
	if got.GetReloginReason() != "refresh_token_rejected" {
		t.Errorf("flagged outcome dropped its relogin_reason: %q", got.GetReloginReason())
	}
}

// TestLoginVerdictToProto_UnknownOutcomeIsUnspecified guards the mapping's
// default arm. A LoginOutcome this build does not recognise must degrade to
// UNSPECIFIED — which renders as silence — never to OK.
func TestLoginVerdictToProto_UnknownOutcomeIsUnspecified(t *testing.T) {
	t.Parallel()

	got := loginVerdictToProto(LoginVerdict{Outcome: LoginOutcome(9999), ReloginReason: "whatever"})
	if got.GetOutcome() != pb.NotifyAuthChangeResponse_OUTCOME_UNSPECIFIED {
		t.Errorf("outcome = %v, want OUTCOME_UNSPECIFIED", got.GetOutcome())
	}
}
