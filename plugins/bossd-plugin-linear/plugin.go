package main

import (
	goplugin "github.com/hashicorp/go-plugin"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	sharedplugin "github.com/recurser/bossalib/plugin"
	"github.com/recurser/bossalib/plugin/hostclient"
	"github.com/rs/zerolog"
)

func newTaskSourcePlugin(logger zerolog.Logger) *sharedplugin.TaskSourcePlugin {
	return &sharedplugin.TaskSourcePlugin{NewServer: func(broker *goplugin.GRPCBroker) bossanovav1.TaskSourceServiceServer {
		return newServer(hostclient.NewEagerClient(broker, logger), logger)
	}}
}
