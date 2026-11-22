package main

import (
	"testing"

	"github.com/recurser/bossalib/contract/plugin/plugin/agentrunnertest"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestAgentRunnerConformance(t *testing.T) {
	s, _ := newTestServer(t, WithCommandFactory(fakeAgy(`echo '`+initLine(conversationA)+`'; exec sleep 60`)))
	agentrunnertest.Run(t, agentrunnertest.Runner{
		Name:   pluginName,
		Client: agentrunnertest.Serve(t, s),
		Unsupported: []string{
			bossanovav1.AgentRunnerService_PreflightHeadlessRun_FullMethodName,
			bossanovav1.AgentRunnerService_DescribeMCPSurface_FullMethodName,
			bossanovav1.AgentRunnerService_ListModels_FullMethodName,
			bossanovav1.AgentRunnerService_PlanQuestionAnswer_FullMethodName,
			bossanovav1.AgentRunnerService_ReadQuestionMenu_FullMethodName,
		},
	})
}
