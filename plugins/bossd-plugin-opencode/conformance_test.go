package main

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/contract/plugin/agentruntime"
	"github.com/recurser/bossalib/contract/plugin/plugin/agentrunnertest"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestAgentRunnerConformance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("OPENCODE_DB", t.TempDir()+"/absent.db")
	srv := newServer(nil, zerolog.Nop(), WithCommandFactory(fakeOpencodeShell(t, `printf '%s\n' '{"sessionID":"conformance-run"}'; exec sleep 60`)))
	// Do not resolve the developer's installed CLI or its database path.
	srv.store.resolvePathFn = func() string { return "" }
	srv.loginProbe = func(context.Context) agentruntime.LoginStatusOutput {
		return agentruntime.LoginStatusOutput{
			Output: []byte("┌  Credentials ~/.local/share/opencode/auth.json\n│\n└  0 credentials\n"),
		}
	}
	agentrunnertest.Run(t, agentrunnertest.Runner{Name: "opencode", Client: agentrunnertest.Serve(t, srv), Unsupported: []string{bossanovav1.AgentRunnerService_PreflightHeadlessRun_FullMethodName, bossanovav1.AgentRunnerService_DescribeMCPSurface_FullMethodName, bossanovav1.AgentRunnerService_ProbeProgressLiveness_FullMethodName, bossanovav1.AgentRunnerService_ListModels_FullMethodName, bossanovav1.AgentRunnerService_PlanQuestionAnswer_FullMethodName, bossanovav1.AgentRunnerService_ReadQuestionMenu_FullMethodName}})
}
