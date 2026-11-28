package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	sharedplugin "github.com/recurser/bossalib/contract/plugin/plugin"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/safego"

	"github.com/recurser/bossd/internal/plugin"
)

const agentLoginCheckTimeout = sharedplugin.AgentLoginProbeTimeout + 2*time.Second

type agentLoginState int

const (
	agentLoginUnknown agentLoginState = iota
	agentLoginLoggedIn
	agentLoginLoggedOut
	agentLoginManaged
)

type agentLoginResult struct {
	Agent  string
	State  agentLoginState
	Remedy string
	Reason string
}

func (s *Server) agentLogins(ctx context.Context) []agentLoginResult {
	names := s.loadedAgentNames()
	if len(names) == 0 {
		return nil
	}
	results := make([]agentLoginResult, len(names))
	done := make([]<-chan struct{}, len(names))
	for i, name := range names {
		results[i] = agentLoginResult{
			Agent:  name,
			State:  agentLoginUnknown,
			Reason: "probe did not finish",
		}
		client := s.agentLoginClient(name)
		done[i] = safego.Go(s.logger, func() {
			results[i] = s.agentLogin(ctx, name, client)
		})
	}
	for _, ch := range done {
		<-ch
	}
	slices.SortFunc(results, func(a, b agentLoginResult) int {
		return strings.Compare(a.Agent, b.Agent)
	})
	return results
}

func (s *Server) agentLoginClient(name string) any {
	if len(s.agentClients) > 0 {
		return s.agentClients[name]
	}
	if s.pluginHost != nil {
		return s.pluginHost.AgentRunnerByName(name)
	}
	return nil
}

func (s *Server) agentLogin(ctx context.Context, name string, client any) agentLoginResult {
	if s.resolver != nil {
		id, err := s.resolver.DefaultAgentAccountID(ctx, accountAgentName(name), "", time.Now())
		if err == nil && id != "" {
			return agentLoginResult{Agent: name, State: agentLoginManaged}
		}
	}

	checker, ok := client.(plugin.LoginChecker)
	if !ok {
		return agentLoginResult{
			Agent:  name,
			State:  agentLoginUnknown,
			Reason: fmt.Sprintf("the %s plugin has no login status command", name),
		}
	}

	probeCtx, cancel := context.WithTimeout(ctx, agentLoginCheckTimeout)
	defer cancel()
	resp, err := checker.CheckLogin(probeCtx, &bossanovav1.CheckLoginRequest{})
	if err != nil {
		code := grpcstatus.Code(err)
		switch {
		case code == codes.Unimplemented:
			return agentLoginResult{
				Agent:  name,
				State:  agentLoginUnknown,
				Reason: "no login status command",
			}
		case code == codes.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded):
			return agentLoginResult{
				Agent:  name,
				State:  agentLoginUnknown,
				Reason: "timed out",
			}
		default:
			return agentLoginResult{
				Agent:  name,
				State:  agentLoginUnknown,
				Reason: fmt.Sprintf("plugin unavailable (%s)", code),
			}
		}
	}

	switch resp.GetStatus() {
	case bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN:
		return agentLoginResult{
			Agent: name,
			State: agentLoginLoggedIn,
		}
	case bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT:
		return agentLoginResult{
			Agent:  name,
			State:  agentLoginLoggedOut,
			Remedy: strings.TrimSpace(resp.GetRemedy()),
		}
	default:
		return agentLoginResult{
			Agent:  name,
			State:  agentLoginUnknown,
			Reason: fmt.Sprintf("the %s CLI's status output was not recognised", name),
		}
	}
}

func agentLoginRemedy(r agentLoginResult) string {
	if remedy := strings.TrimSpace(r.Remedy); remedy != "" {
		return remedy
	}
	return fmt.Sprintf("log in to the %s CLI", r.Agent)
}

func agentLoginDoctorCheck(r agentLoginResult) *bossanovav1.RepairDoctorCheck {
	name := fmt.Sprintf("agent logged in: %s", r.Agent)
	switch r.State {
	case agentLoginLoggedIn:
		return &bossanovav1.RepairDoctorCheck{
			Name:   name,
			Ok:     true,
			Detail: fmt.Sprintf("%s CLI is logged in", r.Agent),
		}
	case agentLoginLoggedOut:
		return &bossanovav1.RepairDoctorCheck{
			Name:   name,
			Ok:     false,
			Detail: fmt.Sprintf("%s CLI is logged out — %s", r.Agent, agentLoginRemedy(r)),
		}
	case agentLoginManaged:
		return &bossanovav1.RepairDoctorCheck{
			Name:   name,
			Ok:     true,
			Detail: "managed account bound; the CLI's ambient login was not checked",
		}
	default:
		return &bossanovav1.RepairDoctorCheck{
			Name:   name,
			Ok:     true,
			Detail: fmt.Sprintf("could not determine the %s CLI's login state: %s", r.Agent, r.Reason),
		}
	}
}

func (s *Server) startAgentLoginChecks(ctx context.Context) func() []*bossanovav1.RepairDoctorCheck {
	var results []agentLoginResult
	done := safego.Go(s.logger, func() {
		results = s.agentLogins(ctx)
	})
	return func() []*bossanovav1.RepairDoctorCheck {
		<-done
		if len(results) == 0 {
			return []*bossanovav1.RepairDoctorCheck{{
				Name:   "agent logged in",
				Ok:     true,
				Detail: "skipped — no agent runner loaded",
			}}
		}
		checks := make([]*bossanovav1.RepairDoctorCheck, len(results))
		for i, r := range results {
			checks[i] = agentLoginDoctorCheck(r)
		}
		return checks
	}
}
