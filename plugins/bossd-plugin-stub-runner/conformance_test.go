package main

import (
	"testing"

	"github.com/recurser/bossalib/contract/plugin/plugin/agentrunnertest"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

func TestAgentRunnerConformance(t *testing.T) {
	srv := newServer(zerolog.Nop())
	agentrunnertest.Run(t, agentrunnertest.Runner{Name: "stub-runner", Client: agentrunnertest.Serve(t, srv), Unsupported: []string{bossanovav1.AgentRunnerService_PreflightHeadlessRun_FullMethodName, bossanovav1.AgentRunnerService_DescribeMCPSurface_FullMethodName, bossanovav1.AgentRunnerService_MaterializeAgentAccount_FullMethodName, bossanovav1.AgentRunnerService_ReadTranscript_FullMethodName, bossanovav1.AgentRunnerService_RotationCapability_FullMethodName}})
}
