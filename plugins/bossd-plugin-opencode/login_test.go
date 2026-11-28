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

func TestClassifyOpencodeAuthList(t *testing.T) {
	tests := []struct {
		name string
		out  agentruntime.LoginStatusOutput
		want bossanovav1.AgentLoginStatus
	}{
		{
			name: "stored credentials only",
			out: agentruntime.LoginStatusOutput{
				Output: []byte("┌  Credentials ~/.local/share/opencode/auth.json\n│\n●  Anthropic oauth\n│\n└  1 credential\n"),
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN,
		},
		{
			name: "multiple stored credentials",
			out: agentruntime.LoginStatusOutput{
				Output: []byte("┌  Credentials ~/.local/share/opencode/auth.json\n│\n●  Anthropic oauth\n│\n●  OpenAI api\n│\n└  2 credentials\n"),
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN,
		},
		{
			name: "zero stored credentials with active env provider",
			out: agentruntime.LoginStatusOutput{
				Output: []byte("┌  Credentials ~/.local/share/opencode/auth.json\n│\n└  0 credentials\n\n┌  Environment\n│\n●  Anthropic ANTHROPIC_API_KEY\n│\n└  1 environment variable\n"),
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN,
		},
		{
			name: "zero stored credentials with env provider section without count footer",
			out: agentruntime.LoginStatusOutput{
				Output: []byte("┌  Credentials ~/.local/share/opencode/auth.json\n│\n└  0 credentials\n\n┌  Environment\n│\n●  OpenAI OPENAI_API_KEY\n"),
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN,
		},
		{
			name: "both stored credentials and env providers",
			out: agentruntime.LoginStatusOutput{
				Output: []byte("┌  Credentials ~/.local/share/opencode/auth.json\n│\n●  Anthropic oauth\n│\n└  1 credential\n\n┌  Environment\n│\n●  OpenAI OPENAI_API_KEY\n│\n└  1 environment variable\n"),
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN,
		},
		{
			name: "zero stored credentials and no env section is logged out",
			out: agentruntime.LoginStatusOutput{
				Output: []byte("┌  Credentials ~/.local/share/opencode/auth.json\n│\n└  0 credentials\n"),
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
		},
		{
			name: "zero stored credentials and explicit zero env variables is logged out",
			out: agentruntime.LoginStatusOutput{
				Output: []byte("┌  Credentials ~/.local/share/opencode/auth.json\n│\n└  0 credentials\n\n┌  Environment\n│\n└  0 environment variables\n"),
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
		},
		{
			name: "ANSI colored zero credentials output is logged out",
			out: agentruntime.LoginStatusOutput{
				Output: []byte("\x1b[90m┌\x1b[0m  \x1b[1mCredentials\x1b[0m \x1b[2m~/.local/share/opencode/auth.json\x1b[0m\n\x1b[90m│\x1b[0m\n\x1b[90m└\x1b[0m  \x1b[32m0 credentials\x1b[0m\n"),
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
		},
		{
			name: "ANSI colored env-only output is logged in",
			out: agentruntime.LoginStatusOutput{
				Output: []byte("\x1b[90m┌\x1b[0m  Credentials ~/.local/share/opencode/auth.json\n\x1b[90m└\x1b[0m  0 credentials\n\n\x1b[90m┌\x1b[0m  \x1b[1mEnvironment\x1b[0m\n\x1b[32m●\x1b[0m  Anthropic \x1b[2mANTHROPIC_API_KEY\x1b[0m\n\x1b[90m└\x1b[0m  1 environment variable\n"),
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN,
		},
		{
			name: "empty Environment section without count fails open to unknown",
			out: agentruntime.LoginStatusOutput{
				Output: []byte("┌  Credentials ~/.local/share/opencode/auth.json\n│\n└  0 credentials\n\n┌  Environment\n│\n"),
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "non-zero exit is unknown",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte("┌  Credentials ~/.local/share/opencode/auth.json\n│\n└  0 credentials\n"),
				ExitCode: 1,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "exit 126 not executable is unknown",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte("permission denied\n"),
				ExitCode: 126,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "exit 127 command not found is unknown",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte("zsh:1: command not found: opencode\n"),
				ExitCode: 127,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "timeout error is unknown",
			out: agentruntime.LoginStatusOutput{
				Output: []byte("└  0 credentials\n"),
				Err:    context.DeadlineExceeded,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "launch error is unknown",
			out: agentruntime.LoginStatusOutput{
				Err: errors.New("exec: opencode: not found"),
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
		{
			name: "garbled output is unknown",
			out: agentruntime.LoginStatusOutput{
				Output:   []byte("Usage: opencode [command]\n"),
				ExitCode: 0,
			},
			want: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyOpencodeAuthList(tt.out); got != tt.want {
				t.Errorf("classifyOpencodeAuthList() = %v, want %v", got, tt.want)
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
			out:        agentruntime.LoginStatusOutput{Output: []byte("┌  Credentials ~/.local/share/opencode/auth.json\n│\n●  Anthropic oauth\n│\n└  1 credential\n")},
			wantStatus: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN,
			wantRemedy: "",
		},
		{
			name:       "logged out carries ErrAuthRequired text",
			out:        agentruntime.LoginStatusOutput{Output: []byte("┌  Credentials ~/.local/share/opencode/auth.json\n│\n└  0 credentials\n")},
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
		return exec.CommandContext(ctx, "sh", "-c", `printf '┌  Credentials ~/.local/share/opencode/auth.json\n│\n└  0 credentials\n'`)
	}

	srv := newServer(nil, zerolog.Nop(), WithCommandFactory(factory))
	resp, err := srv.CheckLogin(context.Background(), &bossanovav1.CheckLoginRequest{})
	if err != nil {
		t.Fatalf("CheckLogin() error = %v", err)
	}
	if gotName != "opencode" || !slices.Equal(gotArgs, []string{"auth", "list"}) {
		t.Errorf("invoked (%q, %v), want (%q, [auth list])", gotName, gotArgs, "opencode")
	}
	if resp.GetStatus() != bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT {
		t.Errorf("Status = %v, want LOGGED_OUT", resp.GetStatus())
	}
	if resp.GetRemedy() != ErrAuthRequired.Error() {
		t.Errorf("Remedy = %q, want %q", resp.GetRemedy(), ErrAuthRequired.Error())
	}
}
