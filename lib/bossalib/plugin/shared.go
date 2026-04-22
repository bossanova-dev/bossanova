// Package plugin provides shared plugin constants used by both the daemon
// (host) and plugin binaries. These values must match for the go-plugin
// handshake to succeed.
package plugin

import (
	"os"

	goplugin "github.com/hashicorp/go-plugin"
)

// Handshake constants — both sides must agree on these.
//
// MagicCookieValue is generated fresh per daemon startup (see
// services/bossd/internal/plugin.Host.Start) and propagated to the plugin
// subprocess via the env var named by MagicCookieKey. Plugins pick it up
// through NewHandshakeForPlugin. A per-startup random value means a long-
// lived static secret can't be recovered from a crashed process image or
// stale env dump; it does not by itself authenticate peers (the go-plugin
// handshake's primary defense is still that the subprocess's gRPC address
// is only sent to the parent over the inherited stdout pipe).
const (
	ProtocolVersion = 1
	MagicCookieKey  = "BOSSANOVA_PLUGIN"
)

// Plugin type names used as keys in the go-plugin PluginSet.
const (
	PluginTypeTaskSource  = "task_source"
	PluginTypeEventSource = "event_source"
	PluginTypeScheduler   = "scheduler"
	PluginTypeWorkflow    = "workflow"
)

// NewHandshakeForPlugin returns the HandshakeConfig that a plugin subprocess
// should pass to goplugin.Serve. MagicCookieValue is sourced from the env
// var set by the daemon; if the plugin is run directly (no env var), we
// substitute a non-empty sentinel so go-plugin's server emits its
// "not meant to be executed directly" message instead of "Misconfigured
// ServeConfig" — the latter reads like a plugin bug and sends developers
// down the wrong rabbit hole.
func NewHandshakeForPlugin() goplugin.HandshakeConfig {
	val := os.Getenv(MagicCookieKey)
	if val == "" {
		val = "__unset__"
	}
	return goplugin.HandshakeConfig{
		ProtocolVersion:  ProtocolVersion,
		MagicCookieKey:   MagicCookieKey,
		MagicCookieValue: val,
	}
}
