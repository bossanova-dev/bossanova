package main

import (
	goplugin "github.com/hashicorp/go-plugin"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	sharedplugin "github.com/recurser/bossalib/plugin"
	"github.com/rs/zerolog"
)

func newAgentRunnerPlugin(logger zerolog.Logger) *sharedplugin.AgentRunnerPlugin {
	return &sharedplugin.AgentRunnerPlugin{NewServer: func(broker *goplugin.GRPCBroker) bossanovav1.AgentRunnerServiceServer {
		return newServer(logger)
	}}
}
