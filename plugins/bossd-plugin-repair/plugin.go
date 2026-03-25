package main

import (
	"context"

	goplugin "github.com/hashicorp/go-plugin"
	sharedplugin "github.com/recurser/bossalib/plugin"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
)

// repairPlugin implements go-plugin's GRPCPlugin interface for the
// plugin (server) side. GRPCServer registers the WorkflowService
// implementation on the gRPC server. GRPCClient is unused on this side.
type repairPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	logger zerolog.Logger
}

func (p *repairPlugin) GRPCServer(broker *goplugin.GRPCBroker, srv *grpc.Server) error { //nolint:unparam // interface implementation
	// Create a host client that eagerly starts broker.Dial(1) in a background
	// goroutine. The go-plugin broker cleans up pending connection info after
	// 5 seconds, so we must start the Dial immediately rather than deferring
	// to the first RPC call. The goroutine blocks until the host calls
	// AcceptAndServe on broker ID 1 (which happens during Dispense).
	hostClient := newEagerHostServiceClient(broker, p.logger)
	server := newRepairMonitor(hostClient, p.logger)

	srv.RegisterService(&sharedplugin.WorkflowServiceDesc, server)
	return nil
}

func (p *repairPlugin) GRPCClient(context.Context, *goplugin.GRPCBroker, *grpc.ClientConn) (any, error) {
	// Plugin side does not use GRPCClient.
	return nil, nil
}

// Ensure repairMonitor implements the shared WorkflowServiceHandler interface.
var _ sharedplugin.WorkflowServiceHandler = (*repairMonitor)(nil)
