package main

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/recurser/bossalib/contract/plugin/agentruntime"
	sharedplugin "github.com/recurser/bossalib/contract/plugin/plugin"
	"github.com/recurser/bossalib/contract/plugin/statusdetect"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

var (
	opencodeCredCountRe = regexp.MustCompile(`(?i)^(\d+)\s+credentials?$`)
	opencodeEnvCountRe  = regexp.MustCompile(`(?i)^(\d+)\s+environment\s+variables?$`)
)

func (s *Server) CheckLogin(ctx context.Context, _ *bossanovav1.CheckLoginRequest) (*bossanovav1.CheckLoginResponse, error) {
	status := classifyOpencodeAuthList(s.runLoginProbe(ctx))
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
		Argv:    []string{"opencode", "auth", "list"},
		Factory: factory,
		Timeout: sharedplugin.AgentLoginProbeTimeout,
	})
}

func classifyOpencodeAuthList(out agentruntime.LoginStatusOutput) bossanovav1.AgentLoginStatus {
	if out.Err != nil || out.ExitCode != 0 {
		return bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN
	}
	clean := string(statusdetect.StripANSI(out.Output))
	var (
		hasCredCount    bool
		credCount       int
		hasEnvCount     bool
		envCount        int
		inEnvSection    bool
		hasEnvSection   bool
		envSectionItems int
	)
	for _, rawLine := range strings.Split(clean, "\n") {
		line := trimClackPrefix(rawLine)
		if line == "" {
			continue
		}
		if m := opencodeCredCountRe.FindStringSubmatch(line); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				hasCredCount = true
				credCount = n
			}
			inEnvSection = false
			continue
		}
		if strings.EqualFold(line, "environment") {
			hasEnvSection = true
			inEnvSection = true
			continue
		}
		if m := opencodeEnvCountRe.FindStringSubmatch(line); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				hasEnvCount = true
				envCount = n
			}
			inEnvSection = false
			continue
		}
		if inEnvSection {
			envSectionItems++
		}
	}
	if !hasCredCount {
		return bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN
	}
	if credCount > 0 {
		return bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN
	}
	if hasEnvCount {
		if envCount > 0 {
			return bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN
		}
		return bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT
	}
	if hasEnvSection {
		if envSectionItems > 0 {
			return bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN
		}
		return bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN
	}
	return bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT
}

func trimClackPrefix(line string) string {
	return strings.TrimSpace(strings.TrimLeftFunc(line, func(r rune) bool {
		return unicode.IsSpace(r) ||
			(r >= 0x2500 && r <= 0x25ff) ||
			r == '•' || r == '·'
	}))
}
