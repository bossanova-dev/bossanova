// Package plugin provides shared plugin constants used by both the daemon
// (host) and plugin binaries. These values must match for the go-plugin
// handshake to succeed.
package plugin

// Handshake constants — both sides must agree on these.
const (
	ProtocolVersion  = 1
	MagicCookieKey   = "BOSSANOVA_PLUGIN"
	MagicCookieValue = "bossanova"
)

// Plugin type names used as keys in the go-plugin PluginSet.
const (
	PluginTypeTaskSource  = "task_source"
	PluginTypeEventSource = "event_source"
	PluginTypeScheduler   = "scheduler"
)
