package main

import (
	"testing"

	"github.com/recurser/bossalib/contract/plugin/plugin/agentrunnertest"
	"github.com/rs/zerolog"
)

func TestAgentRunnerConformance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	srv := newServer(nil, zerolog.Nop(), WithCommandFactory(fakeCodexShell(t, `printf '%s\n' '{"type":"thread.started","thread_id":"conformance-run"}'; exec sleep 60`)))
	srv.operationRegistry = fixedRegistry(runtimeOperationSurface{Source: codexOperationRegistrySource}, nil)
	agentrunnertest.Run(t, agentrunnertest.Runner{Name: "codex", Client: agentrunnertest.Serve(t, srv)})
}
