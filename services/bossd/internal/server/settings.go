package server

import (
	"context"
	"fmt"
	"os"
	"strings"

	"connectrpc.com/connect"

	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/telemetry"
)

// GetSettings returns the TUI-editable subset of global settings (the fields the
// boss settings form edits) plus one AgentSettings entry per configured agent
// plugin. It resolves values through config.Load() so defaults (e.g.
// default_agent → "claude") are applied exactly as the TUI sees them.
func (s *Server) GetSettings(_ context.Context, _ *connect.Request[pb.GetSettingsRequest]) (*connect.Response[pb.GetSettingsResponse], error) {
	settings, err := config.Load()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("load settings: %w", err))
	}
	return connect.NewResponse(&pb.GetSettingsResponse{Settings: settingsToProto(settings)}), nil
}

// UpdateSettings applies a partial update to global settings and persists it
// atomically. It re-loads from disk (load → apply → save) so it never clobbers
// a concurrently-written file from a stale in-memory snapshot. Only the optional
// scalar fields that are set are applied; per-agent updates upsert config keys
// (an empty value deletes the key) and toggle the enabled flag. Validation
// mirrors the boss TUI: an invalid field returns CodeInvalidArgument and the
// file is left untouched (nothing is written until every field validates).
func (s *Server) UpdateSettings(ctx context.Context, req *connect.Request[pb.UpdateSettingsRequest]) (*connect.Response[pb.UpdateSettingsResponse], error) {
	msg := req.Msg
	settings, err := config.Load()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("load settings: %w", err))
	}

	// Per-agent updates are applied first so default_agent can be validated
	// against the resulting enabled set — a single call may both enable an
	// agent and select it as the default.
	for _, upd := range msg.GetAgents() {
		name := strings.TrimSpace(upd.GetName())
		if name == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("agent name cannot be empty"))
		}
		if upd.Enabled != nil {
			setAgentEnabled(&settings, name, *upd.Enabled)
		}
		if len(upd.GetConfig()) > 0 {
			descriptors := s.agentUserSettings(ctx, name)
			for key, value := range upd.GetConfig() {
				if strings.TrimSpace(key) == "" {
					return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("agent %q config key cannot be empty", name))
				}
				if err := applyAgentConfig(&settings, name, key, value, descriptors[key]); err != nil {
					return nil, connect.NewError(connect.CodeInvalidArgument, err)
				}
			}
		}
	}

	if msg.WorktreeBaseDir != nil {
		dir := strings.TrimSpace(*msg.WorktreeBaseDir)
		if dir == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("worktree_base_dir cannot be empty"))
		}
		// Mirror the settings TUI, which accepts any non-empty path and relies on
		// config.Load() to MkdirAll the configured base — the normal "point the
		// worktree base at a fresh directory" case must work here too, not only
		// when the caller pre-creates the directory. Create it now (same 0o750 as
		// config.Load) so an invalid path — e.g. a component that is a regular
		// file — is rejected, but a merely-absent directory is created.
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("worktree_base_dir %q cannot be created: %w", dir, err))
		}
		settings.WorktreeBaseDir = dir
	}

	if msg.PollIntervalSeconds != nil {
		n := *msg.PollIntervalSeconds
		if n < 0 {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("poll_interval_seconds cannot be negative"))
		}
		settings.PollIntervalSeconds = int(n)
	}

	if msg.DefaultAgent != nil {
		agent := strings.TrimSpace(*msg.DefaultAgent)
		if !agentEnabled(settings, agent) {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("default_agent %q is not an enabled agent", agent))
		}
		// Parity with the TUI default-agent picker, which lists only loaded agent
		// runners (its per-agent AgentInfo set), never every enabled plugin. An
		// enabled task-source/reactor plugin (linear, sentry, …) is enabled but is
		// not a valid default agent; accepting one would persist a default that
		// breaks `boss session new` (validateExplicitAgentName rejects a name that
		// is not a loaded runner). Reuse that same resolver so the rule matches.
		if err := s.validateExplicitAgentName(agent); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("default_agent %q is not a loaded agent runner: %w", agent, err))
		}
		settings.DefaultAgent = agent
	}

	if msg.EventTracingEnabled != nil {
		settings.EventTracingEnabled = *msg.EventTracingEnabled
	}
	if msg.ErrorTrackingEnabled != nil {
		settings.ErrorTrackingEnabled = *msg.ErrorTrackingEnabled
	}
	if msg.PosthogProjectToken != nil {
		settings.PostHogProjectToken = strings.TrimSpace(*msg.PosthogProjectToken)
	}
	if msg.PosthogHost != nil {
		host := strings.TrimSpace(*msg.PosthogHost)
		// Mirror the TUI: an empty host falls back to the first-party default
		// when tracing is enabled (so telemetry has a destination); otherwise
		// leave it empty.
		if host == "" && settings.EventTracingEnabled {
			host = telemetry.DefaultHost
		}
		settings.PostHogHost = host
	}

	if err := config.Save(settings); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("save settings: %w", err))
	}
	if client, ok := s.telemetry.(interface{ Update(config.Settings) }); ok {
		client.Update(settings)
	}
	return connect.NewResponse(&pb.UpdateSettingsResponse{Settings: settingsToProto(settings)}), nil
}

// settingsToProto maps the editable subset of config.Settings plus every
// configured plugin (name/enabled/config) into a GlobalSettings message.
func settingsToProto(s config.Settings) *pb.GlobalSettings {
	out := &pb.GlobalSettings{
		WorktreeBaseDir:      s.WorktreeBaseDir,
		PollIntervalSeconds:  clampInt32(s.PollIntervalSeconds),
		DefaultAgent:         s.DefaultAgent,
		EventTracingEnabled:  s.EventTracingEnabled,
		ErrorTrackingEnabled: s.ErrorTrackingEnabled,
		PosthogProjectToken:  s.PostHogProjectToken,
		PosthogHost:          s.PostHogHost,
	}
	for _, p := range s.Plugins {
		as := &pb.AgentSettings{Name: p.Name, Enabled: p.Enabled}
		if len(p.Config) > 0 {
			as.Config = make(map[string]string, len(p.Config))
			for k, v := range p.Config {
				as.Config[k] = v
			}
		}
		out.Agents = append(out.Agents, as)
	}
	return out
}

// applyAgentConfig upserts a single per-agent config key using the validated
// config accessors, choosing the accessor from the key's descriptor type. An
// empty value always deletes the key (matching the proto contract and
// SetPluginConfigString semantics). A nil descriptor (agent not loaded, or key
// not advertised) is treated as a plain string — no enum validation is possible
// without a descriptor.
func applyAgentConfig(s *config.Settings, name, key, value string, desc *pb.UserSetting) error {
	if value == "" {
		config.SetPluginConfigString(s, name, key, "")
		return nil
	}
	switch desc.GetType() {
	case pb.UserSettingType_USER_SETTING_TYPE_BOOL:
		// A non-"true" value (e.g. "false") deletes the key: absent = false.
		config.SetPluginConfigBool(s, name, key, value == "true")
		return nil
	case pb.UserSettingType_USER_SETTING_TYPE_ENUM:
		return config.SetPluginConfigEnum(s, name, key, value, desc.GetAllowedValues())
	default:
		config.SetPluginConfigString(s, name, key, value)
		return nil
	}
}

// agentEnabled reports whether the named plugin is configured and effectively
// enabled. The entry must exist, but for an experimental plugin the opt-in list
// decides: the daemon's gate overrides plugins[].enabled in both directions and
// never persists the result, so the persisted flag would reject a default the
// daemon has loaded and accept one it forces off.
func agentEnabled(s config.Settings, name string) bool {
	for _, p := range s.Plugins {
		if p.Name == name {
			return config.PluginEnabledForSettings(s, name)
		}
	}
	return false
}

// setAgentEnabled sets the enabled flag for the named plugin, appending a new
// PluginConfig entry (name + enabled only) when the plugin is not yet
// configured — mirroring the TUI's setPluginEnabled so the toggle is not lost
// before `boss config init` fills in Path/Version.
func setAgentEnabled(s *config.Settings, name string, enabled bool) {
	for i := range s.Plugins {
		if s.Plugins[i].Name == name {
			s.Plugins[i].Enabled = enabled
			return
		}
	}
	s.Plugins = append(s.Plugins, config.PluginConfig{Name: name, Enabled: enabled})
}

// agentUserSettings returns the named agent's UserSetting descriptors indexed by
// key, sourced from its live GetInfo. Returns nil when the agent is not loaded
// or GetInfo fails, in which case per-agent config keys fall back to string
// handling (no enum validation).
func (s *Server) agentUserSettings(ctx context.Context, name string) map[string]*pb.UserSetting {
	client, ok := s.agentClients[name]
	if !ok || client == nil {
		return nil
	}
	info, err := client.GetInfo(ctx)
	if err != nil || info == nil {
		return nil
	}
	settings := info.GetUserSettings()
	out := make(map[string]*pb.UserSetting, len(settings))
	for _, us := range settings {
		out[us.GetKey()] = us
	}
	return out
}
