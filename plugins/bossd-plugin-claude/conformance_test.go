package main

import (
	"context"
	"testing"

	"github.com/recurser/bossalib/contract/plugin/plugin/agentrunnertest"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

func TestAgentRunnerConformance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := newServer(nil, zerolog.Nop(), WithCommandFactory(fakeClaude(t, "exec sleep 60")))
	srv.oneShot = func(context.Context, string, string, map[string]string, bool) (string, error) {
		return "Conformance title", nil
	}
	srv.initProber = fixtureInitProber{}
	agentrunnertest.Run(t, agentrunnertest.Runner{Name: "claude", Client: agentrunnertest.Serve(t, srv), Unsupported: []string{bossanovav1.AgentRunnerService_PreflightHeadlessRun_FullMethodName}})
}
