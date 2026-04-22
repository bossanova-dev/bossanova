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
// into plugin types that need host callbacks (TaskSource, WorkflowService).
// This allows the plugin subprocess to call back to the host via the
// go-plugin broker.
func NewPluginMap(hostService *HostServiceServer) goplugin.PluginSet {
	return goplugin.PluginSet{
		sharedplugin.PluginTypeTaskSource:  &TaskSourceGRPCPlugin{HostService: hostService},
		sharedplugin.PluginTypeEventSource: &EventSourceGRPCPlugin{},
		sharedplugin.PluginTypeScheduler:   &SchedulerGRPCPlugin{},
		sharedplugin.PluginTypeWorkflow:    &WorkflowServiceGRPCPlugin{HostService: hostService},
	}
}

// NewVersionedPluginMap wraps NewPluginMap in the go-plugin VersionedPlugins
// shape. Using a versioned map from day one lets us introduce breaking
// plugin-protocol changes later by adding new versions without having to
// special-case every client config site.
func NewVersionedPluginMap(hostService *HostServiceServer) map[int]goplugin.PluginSet {
	return map[int]goplugin.PluginSet{
		sharedplugin.ProtocolVersion: NewPluginMap(hostService),
	}
}
