package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"

	"github.com/recurser/bossd/internal/plugin"
)

type cmdFakeAccountResolver struct {
	accountID string
	err       error
	calls     int
	lastProv  string
	lastModel string
}

func (f *cmdFakeAccountResolver) DefaultAgentAccountID(_ context.Context, provider, model string, _ time.Time) (string, error) {
	f.calls++
	f.lastProv = provider
	f.lastModel = model
	return f.accountID, f.err
}

type cmdFakeBareRunner struct {
	plugin.AgentRunner
}

type cmdFakeLoginRunner struct {
	plugin.AgentRunner
	resp  *bossanovav1.CheckLoginResponse
	err   error
	calls int
}

func (r *cmdFakeLoginRunner) CheckLogin(_ context.Context, _ *bossanovav1.CheckLoginRequest) (*bossanovav1.CheckLoginResponse, error) {
	r.calls++
	return r.resp, r.err
}

func TestAgentLoginCheckerResolvesDefaultAgent(t *testing.T) {
	runner := &cmdFakeLoginRunner{
		resp: &bossanovav1.CheckLoginResponse{
			Status: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
			Remedy: "run `codex login`",
		},
	}
	var lookedUp string
	accounts := &cmdFakeAccountResolver{}
	checker := newAgentLoginChecker(
		func() string { return "codex" },
		func(name string) plugin.AgentRunner {
			lookedUp = name
			return runner
		},
		accounts,
		func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	)

	res, err := checker.CheckAgentLogin(context.Background(), "", "gpt-5")
	if err != nil {
		t.Fatalf("CheckAgentLogin: %v", err)
	}
	if lookedUp != "codex" || accounts.lastProv != "codex" || accounts.lastModel != "gpt-5" {
		t.Fatalf("resolved lookup=%q account=(%q,%q), want codex/gpt-5", lookedUp, accounts.lastProv, accounts.lastModel)
	}
	if res.Agent != "codex" || !res.LoggedOut || res.Remedy != "run `codex login`" {
		t.Fatalf("res = %+v, want logged-out codex with remedy", res)
	}

	blank := newAgentLoginChecker(func() string { return "" }, func(string) plugin.AgentRunner {
		t.Fatal("runner lookup should not be called for empty agent")
		return nil
	}, nil, nil)
	res, err = blank.CheckAgentLogin(context.Background(), "", "")
	if err != nil || res.LoggedOut || res.Agent != "" {
		t.Fatalf("blank default res = (%+v, %v), want empty non-blocking result", res, err)
	}
}

func TestAgentLoginCheckerSkipsManagedAccount(t *testing.T) {
	runner := &cmdFakeLoginRunner{
		resp: &bossanovav1.CheckLoginResponse{
			Status: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
			Remedy: "run `claude auth login`",
		},
	}
	checker := newAgentLoginChecker(
		func() string { return "claude" },
		func(string) plugin.AgentRunner { return runner },
		&cmdFakeAccountResolver{accountID: "acct-123"},
		func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	)
	res, err := checker.CheckAgentLogin(context.Background(), "claude", "sonnet")
	if err != nil {
		t.Fatalf("CheckAgentLogin: %v", err)
	}
	if res.Agent != "claude" || res.LoggedOut || res.Remedy != "" {
		t.Fatalf("res = %+v, want not logged out for claude", res)
	}
	if runner.calls != 0 {
		t.Fatalf("runner.CheckLogin called %d times, want 0", runner.calls)
	}
}

func TestAgentLoginCheckerLoggedOut(t *testing.T) {
	const remedy = "codex auth required: run `codex login`"
	runner := &cmdFakeLoginRunner{
		resp: &bossanovav1.CheckLoginResponse{
			Status: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
			Remedy: remedy,
		},
	}
	checker := newAgentLoginChecker(
		func() string { return "claude" },
		func(string) plugin.AgentRunner { return runner },
		&cmdFakeAccountResolver{},
		nil,
	)

	res, err := checker.CheckAgentLogin(context.Background(), "codex", "")
	if err != nil {
		t.Fatalf("CheckAgentLogin: %v", err)
	}
	if res.Agent != "codex" || !res.LoggedOut || res.Remedy != remedy {
		t.Fatalf("res = %+v, want codex logged out with remedy %q", res, remedy)
	}
	if runner.calls != 1 {
		t.Fatalf("runner.CheckLogin calls = %d, want 1", runner.calls)
	}
}

func TestAgentLoginCheckerFailsOpen(t *testing.T) {
	for _, tt := range []struct {
		name   string
		runner plugin.AgentRunner
	}{
		{
			name: "logged in",
			runner: &cmdFakeLoginRunner{
				resp: &bossanovav1.CheckLoginResponse{
					Status: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN,
				},
			},
		},
		{
			name: "unknown status",
			runner: &cmdFakeLoginRunner{
				resp: &bossanovav1.CheckLoginResponse{
					Status: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
				},
			},
		},
		{
			name: "unimplemented",
			runner: &cmdFakeLoginRunner{
				err: grpcstatus.Error(codes.Unimplemented, "no status command"),
			},
		},
		{
			name:   "runner without LoginChecker",
			runner: &cmdFakeBareRunner{},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			checker := newAgentLoginChecker(
				func() string { return "claude" },
				func(string) plugin.AgentRunner { return tt.runner },
				&cmdFakeAccountResolver{},
				nil,
			)
			res, err := checker.CheckAgentLogin(context.Background(), "antigravity", "")
			if err != nil {
				t.Fatalf("CheckAgentLogin error = %v, want nil", err)
			}
			if res.Agent != "antigravity" || res.LoggedOut || res.Remedy != "" {
				t.Fatalf("res = %+v, want non-blocking antigravity result", res)
			}
		})
	}
}

func TestAgentLoginCheckerReturnsErrorOnTransientFailures(t *testing.T) {
	resolverErr := errors.New("sqlite busy")
	for _, tt := range []struct {
		name      string
		resolver  *cmdFakeAccountResolver
		runner    plugin.AgentRunner
		wantCalls int
		checkErr  func(t *testing.T, err error)
	}{
		{
			name:     "account resolver error",
			resolver: &cmdFakeAccountResolver{err: resolverErr},
			runner: &cmdFakeLoginRunner{
				resp: &bossanovav1.CheckLoginResponse{
					Status: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
					Remedy: "run `claude auth login`",
				},
			},
			wantCalls: 0,
			checkErr: func(t *testing.T, err error) {
				if !errors.Is(err, resolverErr) {
					t.Fatalf("err = %v, want %v", err, resolverErr)
				}
			},
		},
		{
			name:     "missing runner",
			resolver: &cmdFakeAccountResolver{},
			runner:   nil,
			checkErr: func(t *testing.T, err error) {
				if err == nil {
					t.Fatal("err = nil, want non-nil for unloaded runner")
				}
			},
		},
		{
			name:     "unavailable",
			resolver: &cmdFakeAccountResolver{},
			runner: &cmdFakeLoginRunner{
				err: grpcstatus.Error(codes.Unavailable, "plugin restarting"),
			},
			wantCalls: 1,
			checkErr: func(t *testing.T, err error) {
				if grpcstatus.Code(err) != codes.Unavailable {
					t.Fatalf("err code = %v, want codes.Unavailable (err=%v)", grpcstatus.Code(err), err)
				}
			},
		},
		{
			name:     "context deadline exceeded",
			resolver: &cmdFakeAccountResolver{},
			runner: &cmdFakeLoginRunner{
				err: context.DeadlineExceeded,
			},
			wantCalls: 1,
			checkErr: func(t *testing.T, err error) {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("err = %v, want context.DeadlineExceeded", err)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			checker := newAgentLoginChecker(
				func() string { return "claude" },
				func(string) plugin.AgentRunner { return tt.runner },
				tt.resolver,
				func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
			)
			res, err := checker.CheckAgentLogin(context.Background(), "claude", "sonnet")
			if err == nil {
				t.Fatalf("CheckAgentLogin error = nil, want non-nil so loginGate skips caching")
			}
			tt.checkErr(t, err)
			if res.LoggedOut {
				t.Fatalf("res = %+v, want LoggedOut=false on transient error", res)
			}
			if loginRunner, ok := tt.runner.(*cmdFakeLoginRunner); ok && loginRunner.calls != tt.wantCalls {
				t.Fatalf("runner.CheckLogin calls = %d, want %d", loginRunner.calls, tt.wantCalls)
			}
		})
	}
}
