package cron

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/domain"
)

const loginCacheTTL = 5 * time.Minute

type AgentLoginChecker interface {
	CheckAgentLogin(ctx context.Context, agentName, model string) (AgentLogin, error)
}

type AgentLogin struct {
	Agent     string
	LoggedOut bool
	Remedy    string
}

type loginCacheEntry struct {
	result    AgentLogin
	checkedAt time.Time
}

type loginGate struct {
	checker AgentLoginChecker
	nowFunc func() time.Time

	mu             sync.Mutex
	entries        map[string]loginCacheEntry
	defaultAgent   string
	defaultChecked time.Time
}

func newLoginGate(checker AgentLoginChecker, nowFunc func() time.Time) *loginGate {
	return &loginGate{
		checker: checker,
		nowFunc: nowFunc,
		entries: make(map[string]loginCacheEntry),
	}
}

func (g *loginGate) lookup(agentName string, now time.Time) (AgentLogin, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	key := agentName
	if key == "" {
		if g.defaultAgent == "" || now.Sub(g.defaultChecked) >= loginCacheTTL {
			return AgentLogin{}, false
		}
		key = g.defaultAgent
	}
	entry, ok := g.entries[key]
	if !ok || now.Sub(entry.checkedAt) >= loginCacheTTL {
		return AgentLogin{}, false
	}
	return entry.result, true
}

func (g *loginGate) store(agentName string, res AgentLogin, checkedAt time.Time) {
	key := res.Agent
	if key == "" {
		key = agentName
	}
	if key == "" {
		return
	}
	g.mu.Lock()
	g.entries[key] = loginCacheEntry{result: res, checkedAt: checkedAt}
	if agentName == "" {
		g.defaultAgent = key
		g.defaultChecked = checkedAt
	}
	g.mu.Unlock()
}

func (g *loginGate) check(ctx context.Context, agentName, model string, useCache bool) (AgentLogin, error) {
	if useCache {
		if res, ok := g.lookup(agentName, g.nowFunc()); ok {
			return res, nil
		}
	}
	res, err := g.checker.CheckAgentLogin(ctx, agentName, model)
	if err != nil {
		return AgentLogin{}, err
	}
	g.store(agentName, res, g.nowFunc())
	return res, nil
}

func (s *Scheduler) checkAgentLogin(ctx context.Context, job *domain.CronJob, scheduled bool, logger zerolog.Logger) error {
	if s.loginGate == nil {
		return nil
	}
	res, err := s.loginGate.check(ctx, job.AgentName, job.Model, scheduled)
	if err != nil {
		logger.Debug().Err(err).Str("agent", job.AgentName).Msg("fire: agent login check failed; proceeding")
		return nil
	}
	if !res.LoggedOut {
		return nil
	}
	agent := res.Agent
	if agent == "" {
		agent = job.AgentName
	}
	if markErr := s.markFireFailed(ctx, job, nil); markErr != nil {
		logger.Warn().Err(markErr).Msg("fire: also failed to mark outcome=fire_failed")
	}
	logger.Warn().
		Str("agent", agent).
		Str("remedy", res.Remedy).
		Msg("fire: agent CLI is not logged in; blocking fire")
	return fmt.Errorf("agent %s is not logged in: %s", agent, res.Remedy)
}
