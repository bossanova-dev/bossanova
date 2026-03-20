package plugin

import (
	goplugin "github.com/hashicorp/go-plugin"
)

// Handshake is the magic cookie configuration that plugin clients and
// servers must agree on. It prevents accidental execution of non-plugin
// binaries.
var Handshake = goplugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "BOSSANOVA_PLUGIN",
	MagicCookieValue: "bossanova",
}

// PluginMap is the set of plugin types the host knows how to dispense.
// Each key maps to a GRPCPlugin implementation that bridges go-plugin
// to the generated proto service clients.
var PluginMap = goplugin.PluginSet{
	"task_source":  &TaskSourceGRPCPlugin{},
	"event_source": &EventSourceGRPCPlugin{},
	"scheduler":    &SchedulerGRPCPlugin{},
}
