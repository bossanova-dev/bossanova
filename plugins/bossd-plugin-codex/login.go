package main

import (
	"context"
	"strings"

	"github.com/recurser/bossalib/contract/plugin/agentruntime"
	sharedplugin "github.com/recurser/bossalib/contract/plugin/plugin"
	"github.com/recurser/bossalib/contract/plugin/statusdetect"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func (s *Server) CheckLogin(ctx context.Context, _ *bossanovav1.CheckLoginRequest) (*bossanovav1.CheckLoginResponse, error) {
	status := classifyCodexLoginStatus(s.runLoginProbe(ctx))
	resp := &bossanovav1.CheckLoginResponse{Status: status}
	if status == bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT {
		resp.Remedy = ErrAuthRequired.Error()
	}
	return resp, nil
}

func (s *Server) runLoginProbe(ctx context.Context) agentruntime.LoginStatusOutput {
	if s.loginProbe != nil {
		return s.loginProbe(ctx)
	}
	var shell string
	var factory agentruntime.CommandFactory
	if s.runner != nil {
		shell = s.runner.loginShell
		factory = s.runner.cmdFactory
	}
	return agentruntime.RunLoginStatus(ctx, agentruntime.LoginStatusProbe{
		Shell:   shell,
		Argv:    []string{"codex", "login", "status"},
		Factory: factory,
		Timeout: sharedplugin.AgentLoginProbeTimeout,
	})
}

func classifyCodexLoginStatus(out agentruntime.LoginStatusOutput) bossanovav1.AgentLoginStatus {
	if out.Err != nil || out.ExitCode == 126 || out.ExitCode == 127 {
		return bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN
	}
	text := strings.TrimSpace(string(statusdetect.StripANSI(out.Output)))
	if out.ExitCode == 0 {
		if strings.Contains(text, "Logged in") {
			return bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN
		}
		return bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN
	}
	if out.ExitCode > 0 && strings.Contains(text, "Not logged in") {
		return bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT
	}
	return bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN
}
