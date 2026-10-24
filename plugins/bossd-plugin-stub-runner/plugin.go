package main

import (
	goplugin "github.com/hashicorp/go-plugin"
	sharedplugin "github.com/recurser/bossalib/contract/plugin/plugin"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

func newAgentRunnerPlugin(logger zerolog.Logger) *sharedplugin.AgentRunnerPlugin {
	return &sharedplugin.AgentRunnerPlugin{NewServer: func(broker *goplugin.GRPCBroker) bossanovav1.AgentRunnerServiceServer {
		return newServer(logger)
	}}
}
