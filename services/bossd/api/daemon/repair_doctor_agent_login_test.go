package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/accountwiring"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/plugin"
)

type loginProbeRunnerClient struct {
	agent.RunnerClient
	resp  *bossanovav1.CheckLoginResponse
	err   error
	fn    func(context.Context, *bossanovav1.CheckLoginRequest) (*bossanovav1.CheckLoginResponse, error)
	calls atomic.Int32
}

var _ plugin.LoginChecker = (*loginProbeRunnerClient)(nil)

func (c *loginProbeRunnerClient) CheckLogin(ctx context.Context, req *bossanovav1.CheckLoginRequest) (*bossanovav1.CheckLoginResponse, error) {
	c.calls.Add(1)
	if c.fn != nil {
		return c.fn(ctx, req)
	}
	return c.resp, c.err
}

type loginNoProbeRunnerClient struct {
	agent.RunnerClient
}

type failingLoginAccountRegistry struct {
	err error
}

func (r *failingLoginAccountRegistry) List(context.Context) ([]account.Meta, error) {
	return nil, r.err
}

func (r *failingLoginAccountRegistry) Get(context.Context, string) (account.Meta, bool, error) {
	return account.Meta{}, false, r.err
}

func (r *failingLoginAccountRegistry) TouchLastUsed(context.Context, string, time.Time) error {
	return r.err
}

func TestAgentLoginDoctorCheckMapsEachStatus_RepairDoctor_AC274602(t *testing.T) {
	t.Parallel()

	const leakedErrToken = "secret-backend-diagnostic-xyz"

	tests := []struct {
		name       string
		agent      string
		client     agent.RunnerClient
		wantState  agentLoginState
		wantOk     bool
		wantDetail string
	}{
		{
			name:  "logged in",
			agent: "codex",
			client: &loginProbeRunnerClient{
				resp: &bossanovav1.CheckLoginResponse{
					Status: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN,
				},
			},
			wantState:  agentLoginLoggedIn,
			wantOk:     true,
			wantDetail: "codex CLI is logged in",
		},
		{
			name:  "logged out with remedy",
			agent: "codex",
			client: &loginProbeRunnerClient{
				resp: &bossanovav1.CheckLoginResponse{
					Status: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
					Remedy: "run `codex login`",
				},
			},
			wantState:  agentLoginLoggedOut,
			wantOk:     false,
			wantDetail: "codex CLI is logged out — run `codex login`",
		},
		{
			name:  "logged out with empty remedy falls back",
			agent: "codex",
			client: &loginProbeRunnerClient{
				resp: &bossanovav1.CheckLoginResponse{
					Status: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
					Remedy: "",
				},
			},
			wantState:  agentLoginLoggedOut,
			wantOk:     false,
			wantDetail: "codex CLI is logged out — log in to the codex CLI",
		},
		{
			name:  "logged out with whitespace remedy falls back",
			agent: "codex",
			client: &loginProbeRunnerClient{
				resp: &bossanovav1.CheckLoginResponse{
					Status: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
					Remedy: "   ",
				},
			},
			wantState:  agentLoginLoggedOut,
			wantOk:     false,
			wantDetail: "codex CLI is logged out — log in to the codex CLI",
		},
		{
			name:  "unknown status",
			agent: "codex",
			client: &loginProbeRunnerClient{
				resp: &bossanovav1.CheckLoginResponse{
					Status: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
				},
			},
			wantState:  agentLoginUnknown,
			wantOk:     true,
			wantDetail: "could not determine the codex CLI's login state: the codex CLI's status output was not recognised",
		},
		{
			name:  "unspecified status",
			agent: "codex",
			client: &loginProbeRunnerClient{
				resp: &bossanovav1.CheckLoginResponse{
					Status: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNSPECIFIED,
				},
			},
			wantState:  agentLoginUnknown,
			wantOk:     true,
			wantDetail: "could not determine the codex CLI's login state: the codex CLI's status output was not recognised",
		},
		{
			name:       "nil response",
			agent:      "codex",
			client:     &loginProbeRunnerClient{resp: nil},
			wantState:  agentLoginUnknown,
			wantOk:     true,
			wantDetail: "could not determine the codex CLI's login state: the codex CLI's status output was not recognised",
		},
		{
			name:  "unimplemented rpc",
			agent: "codex",
			client: &loginProbeRunnerClient{
				err: grpcstatus.Error(codes.Unimplemented, "not implemented"),
			},
			wantState:  agentLoginUnknown,
			wantOk:     true,
			wantDetail: "could not determine the codex CLI's login state: no login status command",
		},
		{
			name:       "client without LoginChecker capability",
			agent:      "codex",
			client:     &loginNoProbeRunnerClient{},
			wantState:  agentLoginUnknown,
			wantOk:     true,
			wantDetail: "could not determine the codex CLI's login state: the codex plugin has no login status command",
		},
		{
			name:       "nil client",
			agent:      "codex",
			client:     nil,
			wantState:  agentLoginUnknown,
			wantOk:     true,
			wantDetail: "could not determine the codex CLI's login state: the codex plugin has no login status command",
		},
		{
			name:  "unavailable rpc never leaks raw error text",
			agent: "codex",
			client: &loginProbeRunnerClient{
				err: grpcstatus.Error(codes.Unavailable, leakedErrToken),
			},
			wantState:  agentLoginUnknown,
			wantOk:     true,
			wantDetail: "could not determine the codex CLI's login state: plugin unavailable (Unavailable)",
		},
		{
			name:  "grpc deadline exceeded",
			agent: "codex",
			client: &loginProbeRunnerClient{
				err: grpcstatus.Error(codes.DeadlineExceeded, leakedErrToken),
			},
			wantState:  agentLoginUnknown,
			wantOk:     true,
			wantDetail: "could not determine the codex CLI's login state: timed out",
		},
		{
			name:  "context deadline exceeded",
			agent: "codex",
			client: &loginProbeRunnerClient{
				err: context.DeadlineExceeded,
			},
			wantState:  agentLoginUnknown,
			wantOk:     true,
			wantDetail: "could not determine the codex CLI's login state: timed out",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := &Server{logger: zerolog.Nop()}
			res := srv.agentLogin(context.Background(), tt.agent, tt.client)
			if res.Agent != tt.agent {
				t.Fatalf("Agent = %q, want %q", res.Agent, tt.agent)
			}
			if res.State != tt.wantState {
				t.Fatalf("State = %v, want %v (reason=%q)", res.State, tt.wantState, res.Reason)
			}
			check := agentLoginDoctorCheck(res)
			wantName := "agent logged in: " + tt.agent
			if check.GetName() != wantName {
				t.Errorf("Name = %q, want %q", check.GetName(), wantName)
			}
			if check.GetOk() != tt.wantOk {
				t.Errorf("Ok = %v, want %v (detail=%q)", check.GetOk(), tt.wantOk, check.GetDetail())
			}
			if check.GetDetail() != tt.wantDetail {
				t.Errorf("Detail = %q, want %q", check.GetDetail(), tt.wantDetail)
			}
			if strings.Contains(check.GetDetail(), leakedErrToken) {
				t.Errorf("Detail %q leaked raw error text %q", check.GetDetail(), leakedErrToken)
			}
			if tt.wantState != agentLoginLoggedIn && strings.Contains(check.GetDetail(), "CLI is logged in") {
				t.Errorf("non-LOGGED_IN Detail %q must not contain %q", check.GetDetail(), "CLI is logged in")
			}
		})
	}
}

func TestAgentLoginsSkipManagedAccount_RepairDoctor_AC274602(t *testing.T) {
	srv, accts := newAccountServer(t, newFakeCredStore(), nil)
	mustAddClaude(t, srv, "work", []byte("blob"))
	srv.resolver = account.NewResolver(accountwiring.NewRegistry(accts), nil, zerolog.Nop())

	probe := &loginProbeRunnerClient{
		resp: &bossanovav1.CheckLoginResponse{
			Status: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
			Remedy: "should not be probed",
		},
	}
	srv.agentClients = map[string]agent.RunnerClient{"claude": probe}

	results := srv.agentLogins(context.Background())
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Agent != "claude" || results[0].State != agentLoginManaged {
		t.Fatalf("result = %+v, want Agent=claude State=agentLoginManaged", results[0])
	}
	if got := probe.calls.Load(); got != 0 {
		t.Fatalf("CheckLogin calls = %d, want 0 when a managed account is bound", got)
	}

	check := agentLoginDoctorCheck(results[0])
	if check.GetName() != "agent logged in: claude" {
		t.Errorf("Name = %q, want %q", check.GetName(), "agent logged in: claude")
	}
	if !check.GetOk() {
		t.Errorf("Ok = false, want true")
	}
	wantDetail := "managed account bound; the CLI's ambient login was not checked"
	if check.GetDetail() != wantDetail {
		t.Errorf("Detail = %q, want %q", check.GetDetail(), wantDetail)
	}
	if strings.Contains(check.GetDetail(), "CLI is logged in") {
		t.Errorf("Managed Detail %q must not contain %q", check.GetDetail(), "CLI is logged in")
	}
}

func TestAgentLoginsProbeWhenResolverFails_RepairDoctor_AC274602(t *testing.T) {
	t.Parallel()

	probe := &loginProbeRunnerClient{
		resp: &bossanovav1.CheckLoginResponse{
			Status: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
			Remedy: "run codex login",
		},
	}
	srv := &Server{
		logger:       zerolog.Nop(),
		resolver:     account.NewResolver(&failingLoginAccountRegistry{err: errors.New("registry read failed")}, nil, zerolog.Nop()),
		agentClients: map[string]agent.RunnerClient{"codex": probe},
	}

	results := srv.agentLogins(context.Background())
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if got := probe.calls.Load(); got != 1 {
		t.Fatalf("CheckLogin calls = %d, want 1 when resolver errors", got)
	}
	if results[0].State != agentLoginLoggedOut {
		t.Fatalf("State = %v, want agentLoginLoggedOut", results[0].State)
	}
	check := agentLoginDoctorCheck(results[0])
	if check.GetOk() {
		t.Errorf("Ok = true, want false")
	}
	wantDetail := "codex CLI is logged out — run codex login"
	if check.GetDetail() != wantDetail {
		t.Errorf("Detail = %q, want %q", check.GetDetail(), wantDetail)
	}
}

func TestAgentLoginsNoAgentsLoaded(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		srv  *Server
	}{
		{name: "nil plugin host", srv: &Server{logger: zerolog.Nop()}},
		{name: "empty plugin host", srv: &Server{logger: zerolog.Nop(), pluginHost: &plugin.Host{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if results := tc.srv.agentLogins(context.Background()); len(results) != 0 {
				t.Fatalf("agentLogins = %+v, want empty", results)
			}
			checks := tc.srv.startAgentLoginChecks(context.Background())()
			if len(checks) != 1 {
				t.Fatalf("len(checks) = %d, want 1", len(checks))
			}
			if checks[0].GetName() != "agent logged in" {
				t.Errorf("Name = %q, want %q", checks[0].GetName(), "agent logged in")
			}
			if !checks[0].GetOk() {
				t.Errorf("Ok = false, want true")
			}
			wantDetail := "skipped — no agent runner loaded"
			if checks[0].GetDetail() != wantDetail {
				t.Errorf("Detail = %q, want %q", checks[0].GetDetail(), wantDetail)
			}
		})
	}
}

func TestAgentLoginsProbesConcurrently(t *testing.T) {
	t.Parallel()

	const total = 3
	var arrived atomic.Int32
	allEntered := make(chan struct{})

	makeClient := func() agent.RunnerClient {
		return &loginProbeRunnerClient{
			fn: func(ctx context.Context, _ *bossanovav1.CheckLoginRequest) (*bossanovav1.CheckLoginResponse, error) {
				if arrived.Add(1) == total {
					close(allEntered)
				}
				select {
				case <-allEntered:
					return &bossanovav1.CheckLoginResponse{
						Status: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN,
					}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			},
		}
	}

	srv := &Server{
		logger: zerolog.Nop(),
		agentClients: map[string]agent.RunnerClient{
			"zebra":  makeClient(),
			"claude": makeClient(),
			"codex":  makeClient(),
		},
	}

	results := srv.agentLogins(t.Context())
	if len(results) != total {
		t.Fatalf("len(results) = %d, want %d", len(results), total)
	}
	wantOrder := []string{"claude", "codex", "zebra"}
	for i, wantAgent := range wantOrder {
		if results[i].Agent != wantAgent {
			t.Errorf("results[%d].Agent = %q, want %q (sorted)", i, results[i].Agent, wantAgent)
		}
		if results[i].State != agentLoginLoggedIn {
			t.Errorf("results[%d].State = %v, want agentLoginLoggedIn (reason=%q)", i, results[i].State, results[i].Reason)
		}
	}
}

func TestRepairDoctorIncludesAgentLoginChecks_RepairDoctor_AC274601(t *testing.T) {
	useProtectedRootsTestHome(t, t.TempDir(), "")
	stubBin := t.TempDir()
	for _, name := range []string{"claude", "gh"} {
		if err := os.WriteFile(filepath.Join(stubBin, name), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", stubBin)

	srv := New(Config{PluginHost: &plugin.Host{}})
	srv.agentClients = map[string]agent.RunnerClient{
		"claude": &loginProbeRunnerClient{
			resp: &bossanovav1.CheckLoginResponse{
				Status: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN,
			},
		},
		"codex": &loginProbeRunnerClient{
			resp: &bossanovav1.CheckLoginResponse{
				Status: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
				Remedy: "run codex login",
			},
		},
	}

	resp, err := srv.RepairDoctor(context.Background(), connect.NewRequest(&bossanovav1.RepairDoctorRequest{}))
	if err != nil {
		t.Fatalf("RepairDoctor: %v", err)
	}

	fixedIdx := -1
	claudeIdx := -1
	codexIdx := -1
	var claudeCheck, codexCheck *bossanovav1.RepairDoctorCheck
	for i, check := range resp.Msg.GetChecks() {
		switch check.GetName() {
		case "failover proxy fixed port":
			fixedIdx = i
		case "agent logged in: claude":
			claudeIdx = i
			claudeCheck = check
		case "agent logged in: codex":
			codexIdx = i
			codexCheck = check
		}
	}

	if fixedIdx < 0 {
		t.Fatal("missing failover proxy fixed port check")
	}
	if claudeCheck == nil || codexCheck == nil {
		t.Fatalf("missing agent login checks: claude=%v codex=%v", claudeCheck, codexCheck)
	}
	if claudeIdx <= fixedIdx || codexIdx <= fixedIdx {
		t.Errorf("agent login checks (claude=%d, codex=%d) must appear after fixed-port check (%d)", claudeIdx, codexIdx, fixedIdx)
	}
	if claudeIdx >= codexIdx {
		t.Errorf("agent login checks must be sorted by agent name: claude=%d codex=%d", claudeIdx, codexIdx)
	}
	if !claudeCheck.GetOk() || claudeCheck.GetDetail() != "claude CLI is logged in" {
		t.Errorf("claude check = (Ok=%v, Detail=%q), want (true, %q)", claudeCheck.GetOk(), claudeCheck.GetDetail(), "claude CLI is logged in")
	}
	if codexCheck.GetOk() || codexCheck.GetDetail() != "codex CLI is logged out — run codex login" {
		t.Errorf("codex check = (Ok=%v, Detail=%q), want (false, %q)", codexCheck.GetOk(), codexCheck.GetDetail(), "codex CLI is logged out — run codex login")
	}
}
