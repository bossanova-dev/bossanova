package main

import (
	goplugin "github.com/hashicorp/go-plugin"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	sharedplugin "github.com/recurser/bossalib/plugin"
	"github.com/recurser/bossalib/plugin/hostclient"
	"github.com/rs/zerolog"
)

func newAgentRunnerPlugin(logger zerolog.Logger, runnerOpts ...Option) *sharedplugin.AgentRunnerPlugin {
	return &sharedplugin.AgentRunnerPlugin{NewServer: func(broker *goplugin.GRPCBroker) bossanovav1.AgentRunnerServiceServer {
		return newServer(hostclient.NewEagerClientWithBrokerID(broker, logger, sharedplugin.BrokerIDAgentRunnerHostService), logger, runnerOpts...)
	}}
}
