package plugin

import (
	"context"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	sharedplugin "github.com/recurser/bossalib/contract/plugin/plugin"

	"github.com/recurser/bossd/internal/ghusage"
)

// NewHandshake builds the HandshakeConfig the daemon uses to spawn plugin
// subprocesses. cookieValue is generated fresh by Host.Start on each daemon
// startup and is propagated to the subprocess by go-plugin as the env var
// sharedplugin.MagicCookieKey.
func NewHandshake(cookieValue string) goplugin.HandshakeConfig {
	return goplugin.HandshakeConfig{
		ProtocolVersion:  sharedplugin.ProtocolVersion,
		MagicCookieKey:   sharedplugin.MagicCookieKey,
		MagicCookieValue: cookieValue,
	}
}

// NewPluginMap builds a plugin set with the given HostServiceServer injected
// into plugin types that need host callbacks (TaskSource, WorkflowService,
// AgentRunner).
// This allows the plugin subprocess to call back to the host via the
// go-plugin broker.
func NewPluginMap(hostService sharedplugin.HostServiceRegistrar) goplugin.PluginSet {
	host := hostService
	return goplugin.PluginSet{
		sharedplugin.PluginTypeTaskSource:  &sharedplugin.TaskSourcePlugin{HostService: host},
		sharedplugin.PluginTypeWorkflow:    &sharedplugin.WorkflowPlugin{HostService: host},
		sharedplugin.PluginTypeAgentRunner: &sharedplugin.AgentRunnerPlugin{HostService: host},
	}
}

// NewVersionedPluginMap wraps NewPluginMap in the go-plugin VersionedPlugins
// shape. Only the current version may negotiate: v1 lacks managed-MCP fields,
// v2 lacks repo-trust fields, v3 uses the retired runner start RPC, and v4 uses
// MaterializeAccount instead of MaterializeAgentAccount (BOS-1784).
// A mismatched release must fail handshake negotiation.
//
// A non-nil hostService is wrapped so every unary host call the plugin named
// pluginName makes runs under its plugin:<name> GitHub usage label; a nil one
// stays nil, so a host with no callback door still serves none.
func NewVersionedPluginMap(hostService sharedplugin.HostServiceRegistrar, pluginName string) map[int]goplugin.PluginSet {
	if hostService != nil {
		hostService = usageLabelledHostService{HostServiceRegistrar: hostService, label: ghusage.PluginLabel(pluginName)}
	}
	return map[int]goplugin.PluginSet{
		sharedplugin.ProtocolVersion: NewPluginMap(hostService),
	}
}

// usageLabelledHostService is the callback door one plugin dials. It labels
// every unary host call with plugin:<name>, so the GitHub calls the door makes
// on that plugin's behalf are attributed to it in the usage ledger.
type usageLabelledHostService struct {
	sharedplugin.HostServiceRegistrar
	label string
}

// HostServerOptions implements sharedplugin.HostServerOptioner.
func (h usageLabelledHostService) HostServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{grpc.ChainUnaryInterceptor(h.unaryLabeler())}
}

func (h usageLabelledHostService) unaryLabeler() grpc.UnaryServerInterceptor {
	label := h.label
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		return handler(ghusage.WithLabel(ctx, label), req)
	}
}
