package plugin

import "time"

// This file exposes unexported host internals to the external plugin_test
// package (host_restart_integration_test.go) so integration tests can drive the
// restart path deterministically. Compiled only under `go test`.

// SetHealthCheckInterval overrides the health/restart tick and returns a
// restore func. Callers MUST set it before Host.Start (the interval is read
// once when the health loop's ticker is constructed) and restore it after the
// host is Stopped, mirroring the watchdogPollInterval test seam.
func SetHealthCheckInterval(d time.Duration) func() {
	prev := healthCheckInterval
	healthCheckInterval = d
	return func() { healthCheckInterval = prev }
}

// PluginPIDs returns a snapshot of loaded-plugin name -> subprocess pid, so a
// test can SIGTERM a specific plugin's process and then assert it was
// relaunched under a new pid.
func (h *Host) PluginPIDs() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]int, len(h.plugins))
	for i := range h.plugins {
		out[h.plugins[i].cfg.Name] = pluginPID(h.plugins[i].client)
	}
	return out
}
