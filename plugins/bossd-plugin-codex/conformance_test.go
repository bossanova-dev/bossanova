package main

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/contract/plugin/agentruntime"
	"github.com/recurser/bossalib/contract/plugin/plugin/agentrunnertest"
)

func TestAgentRunnerConformance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	srv := newServer(nil, zerolog.Nop(), WithCommandFactory(fakeCodexShell(t, `printf '%s\n' '{"type":"thread.started","thread_id":"conformance-run"}'; exec sleep 60`)))
	srv.operationRegistry = fixedRegistry(runtimeOperationSurface{Source: codexOperationRegistrySource}, nil)
	srv.modelCatalog = fakeModelListAppServer(t)
	srv.loginProbe = func(context.Context) agentruntime.LoginStatusOutput {
		return agentruntime.LoginStatusOutput{Output: []byte("Not logged in\n"), ExitCode: 1}
	}
	agentrunnertest.Run(t, agentrunnertest.Runner{Name: "codex", Client: agentrunnertest.Serve(t, srv)})
}
