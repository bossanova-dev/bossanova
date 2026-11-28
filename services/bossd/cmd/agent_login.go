package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"

	cronpkg "github.com/recurser/bossd/internal/cron"
	"github.com/recurser/bossd/internal/plugin"
)

type defaultAgentAccountResolver interface {
	DefaultAgentAccountID(ctx context.Context, provider, model string, now time.Time) (string, error)
}

type agentLoginChecker struct {
	defaultAgent func() string
	runnerByName func(name string) plugin.AgentRunner
	accounts     defaultAgentAccountResolver
	nowFunc      func() time.Time
}

var _ cronpkg.AgentLoginChecker = (*agentLoginChecker)(nil)

func newAgentLoginChecker(
	defaultAgent func() string,
	runnerByName func(name string) plugin.AgentRunner,
	accounts defaultAgentAccountResolver,
	nowFunc func() time.Time,
) *agentLoginChecker {
	if nowFunc == nil {
		nowFunc = time.Now
	}
	return &agentLoginChecker{
		defaultAgent: defaultAgent,
		runnerByName: runnerByName,
		accounts:     accounts,
		nowFunc:      nowFunc,
	}
}

func (c *agentLoginChecker) CheckAgentLogin(ctx context.Context, agentName, model string) (cronpkg.AgentLogin, error) {
	agent := strings.TrimSpace(agentName)
	if agent == "" && c.defaultAgent != nil {
		agent = strings.TrimSpace(c.defaultAgent())
	}
	if agent == "" {
		return cronpkg.AgentLogin{}, nil
	}

	if c.accounts != nil {
		accountID, err := c.accounts.DefaultAgentAccountID(ctx, agent, model, c.nowFunc())
		if err != nil {
			return cronpkg.AgentLogin{}, err
		}
		if accountID != "" {
			return cronpkg.AgentLogin{Agent: agent}, nil
		}
	}

	if c.runnerByName == nil {
		return cronpkg.AgentLogin{Agent: agent}, nil
	}
	runner := c.runnerByName(agent)
	if runner == nil {
		return cronpkg.AgentLogin{}, fmt.Errorf("agent %q runner is not loaded", agent)
	}
	checker, ok := runner.(plugin.LoginChecker)
	if !ok {
		return cronpkg.AgentLogin{Agent: agent}, nil
	}
	resp, err := checker.CheckLogin(ctx, &bossanovav1.CheckLoginRequest{})
	if err != nil {
		if grpcstatus.Code(err) == codes.Unimplemented {
			return cronpkg.AgentLogin{Agent: agent}, nil
		}
		return cronpkg.AgentLogin{}, err
	}
	if resp != nil && resp.GetStatus() == bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT {
		return cronpkg.AgentLogin{
			Agent:     agent,
			LoggedOut: true,
			Remedy:    resp.GetRemedy(),
		}, nil
	}
	return cronpkg.AgentLogin{Agent: agent}, nil
}
