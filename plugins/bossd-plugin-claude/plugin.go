package main

import (
	goplugin "github.com/hashicorp/go-plugin"
	sharedplugin "github.com/recurser/bossalib/contract/plugin/plugin"
	"github.com/recurser/bossalib/contract/plugin/plugin/hostclient"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

func newAgentRunnerPlugin(logger zerolog.Logger, runnerOpts ...RunnerOption) *sharedplugin.AgentRunnerPlugin {
	return &sharedplugin.AgentRunnerPlugin{NewServer: func(broker *goplugin.GRPCBroker) bossanovav1.AgentRunnerServiceServer {
		return newServer(hostclient.NewEagerClientWithBrokerID(broker, logger, sharedplugin.BrokerIDAgentRunnerHostService), logger, runnerOpts...)
	}}
}
