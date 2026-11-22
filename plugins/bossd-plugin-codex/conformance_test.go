package main

import (
	"testing"

	"github.com/recurser/bossalib/contract/plugin/plugin/agentrunnertest"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

func TestAgentRunnerConformance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	srv := newServer(nil, zerolog.Nop(), WithCommandFactory(fakeCodexShell(t, `printf '%s\n' '{"type":"thread.started","thread_id":"conformance-run"}'; exec sleep 60`)))
	srv.operationRegistry = fixedRegistry(runtimeOperationSurface{Source: codexOperationRegistrySource}, nil)
	srv.modelCatalog = fakeModelListAppServer(t)
	agentrunnertest.Run(t, agentrunnertest.Runner{Name: "codex", Client: agentrunnertest.Serve(t, srv), Unsupported: []string{bossanovav1.AgentRunnerService_PlanQuestionAnswer_FullMethodName, bossanovav1.AgentRunnerService_ReadQuestionMenu_FullMethodName}})
}
