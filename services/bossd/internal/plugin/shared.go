package plugin

import (
	goplugin "github.com/hashicorp/go-plugin"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
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
	host := optionalHostService(hostService)
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
func NewVersionedPluginMap(hostService *HostServiceServer) map[int]goplugin.PluginSet {
	return map[int]goplugin.PluginSet{
		sharedplugin.ProtocolVersion: NewPluginMap(hostService),
	}
}

// optionalHostService preserves the optional callback server when converting a pointer to an interface.
func optionalHostService(host *HostServiceServer) bossanovav1.HostServiceServer {
	if host == nil {
		return nil
	}
	return host
}
