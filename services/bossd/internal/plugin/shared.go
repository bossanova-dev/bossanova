package plugin

import (
	goplugin "github.com/hashicorp/go-plugin"

	sharedplugin "github.com/recurser/bossalib/plugin"
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
func NewPluginMap(hostService *HostServiceServer) goplugin.PluginSet {
	return goplugin.PluginSet{
		sharedplugin.PluginTypeTaskSource:  &TaskSourceGRPCPlugin{HostService: hostService},
		sharedplugin.PluginTypeEventSource: &EventSourceGRPCPlugin{},
		sharedplugin.PluginTypeScheduler:   &SchedulerGRPCPlugin{},
		sharedplugin.PluginTypeWorkflow:    &WorkflowServiceGRPCPlugin{HostService: hostService},
		sharedplugin.PluginTypeAgentRunner: &AgentRunnerGRPCPlugin{HostService: hostService},
	}
}

// NewVersionedPluginMap wraps NewPluginMap in the go-plugin VersionedPlugins
// shape. Do not add ProtocolVersionV1 here: its managed-MCP request fields use
// protobuf tags that are reserved in v2, so accepting a v1 binary would launch
// an agent without the managed configuration. A mismatched release must fail
// handshake negotiation instead.
func NewVersionedPluginMap(hostService *HostServiceServer) map[int]goplugin.PluginSet {
	return map[int]goplugin.PluginSet{
		sharedplugin.ProtocolVersion: NewPluginMap(hostService),
	}
}
