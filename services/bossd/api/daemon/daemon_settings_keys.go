package server

import (
	"slices"

	"github.com/recurser/bossalib/kit/config"
)

// restartRule says when a changed settings.json key reaches the running daemon.
type restartRule int

const (
	appliesLive restartRule = iota
	needsRestart
	// needsRestartToEnable: the switch is read live, but a loop it gates
	// starts only when it was on at startup.
	needsRestartToEnable
)

// daemonSettingKey is one settings.json key the web may write. value reads it
// from typed settings; nil means absent, as config.Save's omitempty leaves it.
type daemonSettingKey struct {
	path    []string
	value   func(config.Settings) any
	restart restartRule
}

// daemonSettingKeys is the write allowlist for DaemonSettingsUpdate's
// top-level fields. Nothing outside it, or outside agentSettingKeys, is ever
// written: cloud_control*, subagent_dispatch_grant, daemon_supervision_mode,
// the proof bucket and every operator tuning block keep whatever the file has.
var daemonSettingKeys = []daemonSettingKey{
	// The hostname bossd registers and snapshots with is fixed at startup.
	{[]string{"daemon_name"}, func(s config.Settings) any { return nonZero(s.DaemonName) }, needsRestart},
	// New repositories read it live; the agent log directory under it is fixed at startup.
	{[]string{"worktree_base_dir"}, func(s config.Settings) any { return nonZero(s.WorktreeBaseDir) }, needsRestart},
	// The display poller's ticker.
	{[]string{"poll_interval_seconds"}, func(s config.Settings) any { return nonZero(s.PollIntervalSeconds) }, needsRestart},
	{[]string{"display_open_ports"}, func(s config.Settings) any { return nonZero(s.DisplayOpenPorts) }, appliesLive},
	{[]string{"notifications_enabled"}, func(s config.Settings) any { return deref(s.NotificationsEnabled) }, appliesLive},
	// New sessions read it per creation.
	{[]string{"default_agent"}, func(s config.Settings) any { return nonZero(s.DefaultAgent) }, appliesLive},
	// The account usage refresh loop and failover proxy start only when rotation was on at startup.
	{[]string{"managed_accounts", "enabled"}, func(s config.Settings) any { return deref(s.ManagedAccounts.Enabled) }, needsRestartToEnable},
	{[]string{"proof_upload", "pages_enabled"}, func(s config.Settings) any { return nonZero(s.ProofUpload.PagesEnabled) }, appliesLive},
	// Sentry is initialised once and its DSN handed to plugins at launch.
	{[]string{"error_tracking_enabled"}, func(s config.Settings) any { return nonZero(s.ErrorTrackingEnabled) }, needsRestart},
	// The telemetry client takes the new value through Update.
	{[]string{"event_tracing_enabled"}, func(s config.Settings) any { return nonZero(s.EventTracingEnabled) }, appliesLive},
}

// agentSettingKeys names the plugin config keys the web may edit for one
// agent. model applies to every agent runner; an empty key means the agent
// has no such setting. Every plugin key reaches the plugin as environment at
// launch, so a change needs a restart.
type agentSettingKeys struct {
	bypass, sandbox, approval string
}

const agentModelKey = "model"

var agentSettingKeysByName = map[string]agentSettingKeys{
	"claude": {bypass: "dangerously_skip_permissions"},
	"codex":  {bypass: "dangerously_bypass_approvals_and_sandbox", sandbox: "sandbox", approval: "approval"},
}

func (k agentSettingKeys) all() []string {
	return slices.DeleteFunc([]string{agentModelKey, k.bypass, k.sandbox, k.approval}, func(key string) bool { return key == "" })
}

// writeDaemonSettingChanges copies every allowlisted key whose value differs
// between before and after into doc, and reports whether any did and whether
// one of them needs a restart.
func writeDaemonSettingChanges(doc *config.Document, before, after config.Settings, agents []string) (changed, restart bool) {
	for _, key := range daemonSettingKeys {
		was, now := key.value(before), key.value(after)
		if was == now {
			continue
		}
		doc.Set(now, key.path...)
		changed = true
		switch key.restart {
		case needsRestart:
			restart = true
		case needsRestartToEnable:
			restart = restart || (!before.ManagedAccounts.ManagedAgentAccountsEnabled() && after.ManagedAccounts.ManagedAgentAccountsEnabled())
		case appliesLive:
		}
	}
	for _, name := range agents {
		for _, key := range agentSettingKeysByName[name].all() {
			now := config.PluginConfigString(&after, name, key)
			if config.PluginConfigString(&before, name, key) == now {
				continue
			}
			doc.SetPluginConfig(name, key, now)
			changed, restart = true, true
		}
	}
	return changed, restart
}

func nonZero[T comparable](v T) any {
	var zero T
	if v == zero {
		return nil
	}
	return v
}

func deref(p *bool) any {
	if p == nil {
		return nil
	}
	return *p
}
