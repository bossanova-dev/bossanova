package main

import (
	goplugin "github.com/hashicorp/go-plugin"
	"github.com/recurser/bossalib/plugin/hostclient"
	"github.com/rs/zerolog"
)

// Type alias so the repair plugin references the shared client interface.
type hostClient = hostclient.Client

// newEagerHostServiceClient creates an eager host service client that dials
// the host service in the background via the go-plugin broker.
func newEagerHostServiceClient(broker *goplugin.GRPCBroker, logger zerolog.Logger) hostClient {
	return hostclient.NewEagerClient(broker, logger)
}
