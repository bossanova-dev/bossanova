package plugin

import (
	goplugin "github.com/hashicorp/go-plugin"

	sharedplugin "github.com/recurser/bossalib/plugin"
)

// Handshake is the magic cookie configuration that plugin clients and
// servers must agree on. Values are sourced from the shared library.
var Handshake = goplugin.HandshakeConfig{
	ProtocolVersion:  sharedplugin.ProtocolVersion,
	MagicCookieKey:   sharedplugin.MagicCookieKey,
	MagicCookieValue: sharedplugin.MagicCookieValue,
}

// NewPluginMap builds a plugin set with the given HostServiceServer injected
// into plugins that need it (TaskSource). This allows the plugin subprocess
// to call back to the host via the go-plugin broker.
func NewPluginMap(hostService *HostServiceServer) goplugin.PluginSet {
	return goplugin.PluginSet{
		sharedplugin.PluginTypeTaskSource:  &TaskSourceGRPCPlugin{HostService: hostService},
		sharedplugin.PluginTypeEventSource: &EventSourceGRPCPlugin{},
		sharedplugin.PluginTypeScheduler:   &SchedulerGRPCPlugin{},
		sharedplugin.PluginTypeWorkflow:    &WorkflowServiceGRPCPlugin{HostService: hostService},
	}
}
