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
// shape. Do not add ProtocolVersionV1 or ProtocolVersionV2 here: v1's
// managed-MCP request fields use protobuf tags that are reserved since v2, and
// a v2 binary ignores the repo-trust fields v3 added (BOS-1562), so accepting
// either would launch an agent without the managed configuration or without
// its trust decision. A mismatched release must fail handshake negotiation
// instead.
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
