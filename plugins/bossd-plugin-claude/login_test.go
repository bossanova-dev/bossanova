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

func TestClassifyClaudeAuthStatus(t *testing.T) {
	tests := []struct {
		name string
		out  agentruntime.LoginStatusOutput
		want bossanovav1.AgentLoginStatus
	}{
		{
			name: "logged in JSON exits 0",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte(`{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","subscriptionType":"max"}` + "\n"),
				ExitCode: 0,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN,
		},
		{
			name: "logged out JSON exits 0",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte(`{"loggedIn":false,"authMethod":"none","apiProvider":"firstParty"}` + "\n"),
				ExitCode: 0,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
		},
		{
			name: "logged out JSON exits 1",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte(`{"loggedIn":false}` + "\n"),
				ExitCode: 1,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
		},
		{
			name: "missing loggedIn field is unknown",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte(`{"authMethod":"none","apiProvider":"firstParty"}`),
				ExitCode: 0,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "null loggedIn field is unknown",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte(`{"loggedIn":null}`),
				ExitCode: 0,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "malformed JSON or old CLI usage text is unknown",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte("error: unknown command 'auth'\nUsage: claude [options] [prompt]\n"),
				ExitCode: 1,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "empty output is unknown",
			out: agentruntime.LoginStatusOutput{
				Output:   nil,
				ExitCode: 0,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "exit 126 not executable is unknown",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte(`{"loggedIn":false}`),
				ExitCode: 126,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "exit 127 command not found is unknown",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte("zsh:1: command not found: claude\n"),
				ExitCode: 127,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "logged in JSON with non-zero exit is unknown",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte(`{"loggedIn":true}`),
				ExitCode: 1,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "logged out JSON with unexpected exit 2 is unknown",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte(`{"loggedIn":false}`),
				ExitCode: 2,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "probe error is unknown",
			out: agentruntime.LoginStatusOutput{
				Output: []byte(`{"loggedIn":false}`),
				Err:    context.DeadlineExceeded,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "launch error is unknown",
			out: agentruntime.LoginStatusOutput{
				Err: errors.New("exec: claude: not found"),
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "multi-line logged in JSON surrounded by shell banner and stderr warning",
			out: agentruntime.LoginStatusOutput{
				Output: []byte("zsh: can't change option: zle\nWelcome to zsh\n{\n  \"loggedIn\": true,\n  \"authMethod\": \"claude.ai\"\n}\n(node:1234) ExperimentalWarning: test\n"),
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN,
		},
		{
			name: "logged out JSON with ANSI shell warning containing braces and trailing warning",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte("\x1b[33mzsh: prompt {theme} warning\x1b[0m\n{\"loggedIn\":false,\"authMethod\":\"none\"}\nWarning: update available\n"),
				ExitCode: 1,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
		},
		{
			name: "nested loggedIn without top-level loggedIn is unknown",
			out: agentruntime.LoginStatusOutput{
				Output: []byte(`{"wrapper":{"loggedIn":false}}` + "\n"),
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "conflicting loggedIn objects are unknown",
			out: agentruntime.LoginStatusOutput{
				Output: []byte(`{"loggedIn":true}` + "\n" + `{"loggedIn":false}` + "\n"),
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "non-boolean loggedIn is unknown",
			out: agentruntime.LoginStatusOutput{
				Output: []byte(`{"loggedIn":"false"}` + "\n"),
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyClaudeAuthStatus(tt.out); got != tt.want {
				t.Errorf("classifyClaudeAuthStatus() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckLoginRemedyOnlyOnLoggedOut(t *testing.T) {
	tests := []struct {
		name       string
		out        agentruntime.LoginStatusOutput
		wantStatus bossanovav1.AgentLoginStatus
		wantRemedy string
	}{
		{
			name:       "logged in carries empty remedy",
			out:        agentruntime.LoginStatusOutput{Output: []byte(`{"loggedIn":true}`)},
			wantStatus: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN,
			wantRemedy: "",
		},
		{
			name:       "logged out carries remedy",
			out:        agentruntime.LoginStatusOutput{Output: []byte(`{"loggedIn":false}`), ExitCode: 1},
			wantStatus: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
			wantRemedy: claudeLoginRemedy,
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
		return exec.CommandContext(ctx, "sh", "-c", `printf '{"loggedIn":false}\n'; exit 1`)
	}

	srv := newServer(nil, zerolog.Nop(), WithCommandFactory(factory))
	resp, err := srv.CheckLogin(context.Background(), &bossanovav1.CheckLoginRequest{})
	if err != nil {
		t.Fatalf("CheckLogin() error = %v", err)
	}
	if gotName != "claude" || !slices.Equal(gotArgs, []string{"auth", "status"}) {
		t.Errorf("invoked (%q, %v), want (%q, [auth status])", gotName, gotArgs, "claude")
	}
	if resp.GetStatus() != bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT {
		t.Errorf("Status = %v, want LOGGED_OUT", resp.GetStatus())
	}
	if resp.GetRemedy() != claudeLoginRemedy {
		t.Errorf("Remedy = %q, want %q", resp.GetRemedy(), claudeLoginRemedy)
	}
}

func TestCheckLoginToleratesShellStderrAndBanner(t *testing.T) {
	factory := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", `printf 'zsh: can'\''t change option: zle\n' >&2; printf '{\n  "loggedIn": false,\n  "authMethod": "none"\n}\n'; exit 1`)
	}

	srv := newServer(nil, zerolog.Nop(), WithCommandFactory(factory))
	resp, err := srv.CheckLogin(context.Background(), &bossanovav1.CheckLoginRequest{})
	if err != nil {
		t.Fatalf("CheckLogin() error = %v", err)
	}
	if resp.GetStatus() != bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT {
		t.Errorf("Status = %v, want LOGGED_OUT", resp.GetStatus())
	}
	if resp.GetRemedy() != claudeLoginRemedy {
		t.Errorf("Remedy = %q, want %q", resp.GetRemedy(), claudeLoginRemedy)
	}
}
