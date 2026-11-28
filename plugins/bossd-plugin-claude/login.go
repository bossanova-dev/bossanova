package main

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/recurser/bossalib/contract/plugin/agentruntime"
	sharedplugin "github.com/recurser/bossalib/contract/plugin/plugin"
	"github.com/recurser/bossalib/contract/plugin/statusdetect"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

const claudeLoginRemedy = "run `claude auth login`"

func (s *Server) CheckLogin(ctx context.Context, _ *bossanovav1.CheckLoginRequest) (*bossanovav1.CheckLoginResponse, error) {
	status := classifyClaudeAuthStatus(s.runLoginProbe(ctx))
	resp := &bossanovav1.CheckLoginResponse{Status: status}
	if status == bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT {
		resp.Remedy = claudeLoginRemedy
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
		Argv:    []string{"claude", "auth", "status"},
		Factory: factory,
		Timeout: sharedplugin.AgentLoginProbeTimeout,
	})
}

func classifyClaudeAuthStatus(out agentruntime.LoginStatusOutput) bossanovav1.AgentLoginStatus {
	if out.Err != nil || out.ExitCode == 126 || out.ExitCode == 127 {
		return bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN
	}
	loggedIn, ok := extractClaudeLoggedIn(out.Output)
	if !ok {
		return bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN
	}
	if loggedIn {
		if out.ExitCode == 0 {
			return bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN
		}
		return bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN
	}
	if out.ExitCode == 0 || out.ExitCode == 1 {
		return bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT
	}
	return bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN
}

func extractClaudeLoggedIn(raw []byte) (bool, bool) {
	cleaned := statusdetect.StripANSI(raw)
	var (
		found    bool
		loggedIn bool
	)
	for offset := 0; offset < len(cleaned); {
		rel := bytes.IndexByte(cleaned[offset:], '{')
		if rel < 0 {
			break
		}
		offset += rel
		var payload struct {
			LoggedIn *bool `json:"loggedIn"`
		}
		dec := json.NewDecoder(bytes.NewReader(cleaned[offset:]))
		if err := dec.Decode(&payload); err != nil {
			offset++
			continue
		}
		advance := int(dec.InputOffset())
		if advance <= 0 {
			advance = 1
		}
		offset += advance
		if payload.LoggedIn == nil {
			continue
		}
		if found && loggedIn != *payload.LoggedIn {
			return false, false
		}
		found = true
		loggedIn = *payload.LoggedIn
	}
	return loggedIn, found
}
