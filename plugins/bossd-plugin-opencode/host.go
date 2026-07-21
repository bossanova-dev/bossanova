package main

import (
	goplugin "github.com/hashicorp/go-plugin"
	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/plugin/hostclient"
)

// Type alias so the plugin references the shared client interface.
type hostClient = hostclient.Client

// newEagerHostServiceClient creates an eager host service client that dials
// the host via the go-plugin broker, on the AgentRunner-reserved broker ID.
func newEagerHostServiceClient(broker *goplugin.GRPCBroker, logger zerolog.Logger) hostClient {
	// AgentRunner uses broker ID 2 (BrokerIDAgentRunnerHostService).
	// Hardcoded here rather than imported from bossalib/plugin to avoid a
	// dependency cycle on host-side code.
	const brokerID = uint32(2)
	return hostclient.NewEagerClientWithBrokerID(broker, logger, brokerID)
}
