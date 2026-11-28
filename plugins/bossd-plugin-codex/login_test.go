package main

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/contract/plugin/agentruntime"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestClassifyCodexLoginStatus(t *testing.T) {
	tests := []struct {
		name string
		out  agentruntime.LoginStatusOutput
		want bossanovav1.AgentLoginStatus
	}{
		{
			name: "logged in using ChatGPT",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte("Logged in using ChatGPT\n"),
				ExitCode: 0,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN,
		},
		{
			name: "logged in using API key",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte("Logged in using an API key - [REDACTED]\n"),
				ExitCode: 0,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN,
		},
		{
			name: "not logged in exits 1",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte("Not logged in\n"),
				ExitCode: 1,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
		},
		{
			name: "not logged in with ANSI escapes exits 1",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte("\x1b[31mNot logged in\x1b[0m\n"),
				ExitCode: 1,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
		},
		{
			name: "not logged in with exit 0 is unknown",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte("Not logged in\n"),
				ExitCode: 0,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "logged in text with non-zero exit is unknown",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte("Logged in using ChatGPT\n"),
				ExitCode: 1,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "exit 0 with unrecognised output is unknown",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte("codex 0.162.1\n"),
				ExitCode: 0,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "non-zero exit with unrecognised error is unknown",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte("error: unrecognized subcommand 'status'\n"),
				ExitCode: 2,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "exit 126 not executable is unknown",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte("Not logged in\n"),
				ExitCode: 126,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "exit 127 command not found is unknown",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte("zsh:1: command not found: codex\n"),
				ExitCode: 127,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "probe timeout error is unknown",
			out: agentruntime.LoginStatusOutput{
				Output: []byte("Not logged in\n"),
				Err:    context.DeadlineExceeded,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "launch error is unknown",
			out: agentruntime.LoginStatusOutput{
				Err: errors.New("exec: codex: not found"),
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyCodexLoginStatus(tt.out); got != tt.want {
				t.Errorf("classifyCodexLoginStatus() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckLoginRemedyMatchesErrAuthRequired(t *testing.T) {
	tests := []struct {
		name       string
		out        agentruntime.LoginStatusOutput
		wantStatus bossanovav1.AgentLoginStatus
		wantRemedy string
	}{
		{
			name:       "logged in carries empty remedy",
			out:        agentruntime.LoginStatusOutput{Output: []byte("Logged in using ChatGPT\n")},
			wantStatus: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN,
			wantRemedy: "",
		},
		{
			name:       "logged out carries ErrAuthRequired text",
			out:        agentruntime.LoginStatusOutput{Output: []byte("Not logged in\n"), ExitCode: 1},
			wantStatus: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
			wantRemedy: ErrAuthRequired.Error(),
		},
		{
			name:       "unknown carries empty remedy",
			out:        agentruntime.LoginStatusOutput{Err: context.DeadlineExceeded},
			wantStatus: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
			wantRemedy: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newServer(nil, zerolog.Nop())
			srv.loginProbe = func(context.Context) agentruntime.LoginStatusOutput { return tt.out }

			resp, err := srv.CheckLogin(context.Background(), &bossanovav1.CheckLoginRequest{})
			if err != nil {
				t.Fatalf("CheckLogin() error = %v", err)
			}
			if resp.GetStatus() != tt.wantStatus {
				t.Errorf("Status = %v, want %v", resp.GetStatus(), tt.wantStatus)
			}
			if resp.GetRemedy() != tt.wantRemedy {
				t.Errorf("Remedy = %q, want %q", resp.GetRemedy(), tt.wantRemedy)
			}
		})
	}
}

func TestCheckLoginDefaultProbeUsesRunner(t *testing.T) {
	var gotName string
	var gotArgs []string
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotName = name
		gotArgs = append([]string(nil), args...)
		return exec.CommandContext(ctx, "sh", "-c", `printf 'Not logged in\n'; exit 1`)
	}

	srv := newServer(nil, zerolog.Nop(), WithCommandFactory(factory))
	resp, err := srv.CheckLogin(context.Background(), &bossanovav1.CheckLoginRequest{})
	if err != nil {
		t.Fatalf("CheckLogin() error = %v", err)
	}
	if gotName != "codex" || !slices.Equal(gotArgs, []string{"login", "status"}) {
		t.Errorf("invoked (%q, %v), want (%q, [login status])", gotName, gotArgs, "codex")
	}
	if resp.GetStatus() != bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT {
		t.Errorf("Status = %v, want LOGGED_OUT", resp.GetStatus())
	}
	if resp.GetRemedy() != ErrAuthRequired.Error() {
		t.Errorf("Remedy = %q, want %q", resp.GetRemedy(), ErrAuthRequired.Error())
	}
}
