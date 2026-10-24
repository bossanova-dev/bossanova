package main

import (
	"sync/atomic"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	sharedplugin "github.com/recurser/bossalib/contract/plugin/plugin"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

// repairPlugin implements go-plugin's GRPCPlugin interface for the
// plugin (server) side. GRPCServer registers the WorkflowService
// implementation on the gRPC server. GRPCClient is unused on this side.
type repairPlugin struct {
	sharedplugin.WorkflowPlugin

	// monitor is set inside GRPCServer once the repairMonitor is
	// constructed. atomic.Pointer makes the SIGTERM handler in main.go safe
	// to race with plugin start-up.
	monitor atomic.Pointer[repairMonitor]
}

func newRepairPlugin(logger zerolog.Logger) *repairPlugin {
	p := &repairPlugin{}
	p.NewServer = func(broker *goplugin.GRPCBroker) bossanovav1.WorkflowServiceServer {
		server := newRepairMonitor(newEagerHostServiceClient(broker, logger), logger)
		p.monitor.Store(server)
		return server
	}
	return p
}

// Shutdown drains in-flight repair goroutines before the process exits. No-op
// when GRPCServer has not yet been invoked (i.e. SIGTERM during start-up).
func (p *repairPlugin) Shutdown(timeout time.Duration) {
	if m := p.monitor.Load(); m != nil {
		m.Shutdown(timeout)
	}
}

// Ensure repairMonitor implements the shared WorkflowServiceHandler interface.
var _ bossanovav1.WorkflowServiceServer = (*repairMonitor)(nil)
