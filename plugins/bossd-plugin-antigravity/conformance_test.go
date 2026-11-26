package main

import (
	"testing"

	"github.com/recurser/bossalib/contract/plugin/plugin/agentrunnertest"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestAgentRunnerConformance(t *testing.T) {
	script := `if [ "$1" = "mcp" ] && [ "$2" = "list" ]; then echo "No MCP servers configured."; exit 0; fi; echo '` + initLine(conversationA) + `'; exec sleep 60`
	s, _ := newTestServer(t, WithCommandFactory(fakeAgy(script)))
	agentrunnertest.Run(t, agentrunnertest.Runner{
		Name:   pluginName,
		Client: agentrunnertest.Serve(t, s),
		Unsupported: []string{
			bossanovav1.AgentRunnerService_PreflightHeadlessRun_FullMethodName,
			bossanovav1.AgentRunnerService_ListModels_FullMethodName,
			bossanovav1.AgentRunnerService_PlanQuestionAnswer_FullMethodName,
		},
	})
}
