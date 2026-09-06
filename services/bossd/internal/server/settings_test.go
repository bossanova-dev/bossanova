package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/telemetry"
	"github.com/recurser/bossd/internal/agent"
)

// seedSettings writes seed to a temp settings.json and points BOSS_SETTINGS_PATH
// at it so config.Load()/config.Save() (used by the handlers) resolve there and
// the write guard permits writes. It returns the settings path.
func seedSettings(t *testing.T, seed config.Settings) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	t.Setenv("BOSS_SETTINGS_PATH", path)
	if err := config.SaveTo(path, seed); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	return path
}

// newSettingsServer builds a bare Server with the given agent clients, matching
// the direct-construction pattern used by the ListAgents tests.
func newSettingsServer(clients map[string]agent.AgentRunnerClient) *Server {
	return &Server{agentClients: clients, logger: zerolog.Nop()}
}

type settingsTelemetryClient struct {
	updates  int
	settings config.Settings
}

func (c *settingsTelemetryClient) Update(settings config.Settings) {
	c.updates++
	c.settings = settings
}
func (*settingsTelemetryClient) Capture(context.Context, telemetry.Event, string, map[string]any) {}
func (*settingsTelemetryClient) Identify(context.Context, string, map[string]any)                 {}
func (*settingsTelemetryClient) Alias(context.Context, string, string)                            {}
func (*settingsTelemetryClient) Close()                                                           {}

func mustGetSettings(t *testing.T, srv *Server) *pb.GlobalSettings {
	t.Helper()
	resp, err := srv.GetSettings(context.Background(), connect.NewRequest(&pb.GetSettingsRequest{}))
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	return resp.Msg.GetSettings()
}

func mustUpdateSettings(t *testing.T, srv *Server, req *pb.UpdateSettingsRequest) *pb.GlobalSettings {
	t.Helper()
	resp, err := srv.UpdateSettings(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	return resp.Msg.GetSettings()
}

func i32ptr(v int32) *int32 { return &v }
func boolptr(b bool) *bool  { return &b }

// TestGetSettings_ReturnsEditableSurface proves GetSettings surfaces the
// top-level editable fields plus every configured agent's enabled flag and
// dynamic config map.
func TestGetSettings_ReturnsEditableSurface(t *testing.T) {
	worktree := t.TempDir()
	seedSettings(t, config.Settings{
		WorktreeBaseDir:      worktree,
		PollIntervalSeconds:  45,
		DefaultAgent:         "claude",
		EventTracingEnabled:  true,
		ErrorTrackingEnabled: true,
		PostHogProjectToken:  "phc_tok",
		PostHogHost:          "https://ph.example",
		Plugins: []config.PluginConfig{
			{Name: "claude", Enabled: true, Config: map[string]string{"dangerously_skip_permissions": "true"}},
			{Name: "codex", Enabled: false, Config: map[string]string{"sandbox": "read-only"}},
		},
	})
	srv := newSettingsServer(nil)

	got := mustGetSettings(t, srv)
	if got.GetWorktreeBaseDir() != worktree {
		t.Errorf("worktree = %q, want %q", got.GetWorktreeBaseDir(), worktree)
	}
	if got.GetPollIntervalSeconds() != 45 {
		t.Errorf("poll = %d, want 45", got.GetPollIntervalSeconds())
	}
	if got.GetDefaultAgent() != "claude" {
		t.Errorf("default_agent = %q, want claude", got.GetDefaultAgent())
	}
	if !got.GetEventTracingEnabled() || !got.GetErrorTrackingEnabled() {
		t.Errorf("tracing flags not surfaced: %+v", got)
	}
	if got.GetPosthogProjectToken() != "phc_tok" || got.GetPosthogHost() != "https://ph.example" {
		t.Errorf("posthog fields not surfaced: %+v", got)
	}
	if len(got.GetAgents()) != 2 {
		t.Fatalf("agents = %d, want 2", len(got.GetAgents()))
	}
	byName := map[string]*pb.AgentSettings{}
	for _, a := range got.GetAgents() {
		byName[a.GetName()] = a
	}
	if a := byName["claude"]; a == nil || !a.GetEnabled() || a.GetConfig()["dangerously_skip_permissions"] != "true" {
		t.Errorf("claude agent not surfaced correctly: %+v", byName["claude"])
	}
	if a := byName["codex"]; a == nil || a.GetEnabled() || a.GetConfig()["sandbox"] != "read-only" {
		t.Errorf("codex agent not surfaced correctly: %+v", byName["codex"])
	}
}

// TestUpdateSettings_PartialAppliesOnlySetFields proves an unset optional field
// is left untouched and only the set field changes.
func TestUpdateSettings_PartialAppliesOnlySetFields(t *testing.T) {
	worktree := t.TempDir()
	seedSettings(t, config.Settings{
		WorktreeBaseDir:     worktree,
		PollIntervalSeconds: 30,
		DefaultAgent:        "claude",
		PostHogProjectToken: "keep-me",
		Plugins:             []config.PluginConfig{{Name: "claude", Enabled: true}},
	})
	srv := newSettingsServer(nil)

	got := mustUpdateSettings(t, srv, &pb.UpdateSettingsRequest{
		PollIntervalSeconds: i32ptr(90),
	})
	if got.GetPollIntervalSeconds() != 90 {
		t.Errorf("poll = %d, want 90", got.GetPollIntervalSeconds())
	}
	if got.GetWorktreeBaseDir() != worktree {
		t.Errorf("worktree changed unexpectedly: %q", got.GetWorktreeBaseDir())
	}
	if got.GetPosthogProjectToken() != "keep-me" {
		t.Errorf("posthog token changed unexpectedly: %q", got.GetPosthogProjectToken())
	}
}

func TestUpdateSettings_RefreshesTelemetryClient(t *testing.T) {
	worktree := t.TempDir()
	seedSettings(t, config.Settings{
		WorktreeBaseDir: worktree,
		DefaultAgent:    "claude",
		Plugins:         []config.PluginConfig{{Name: "claude", Enabled: true}},
	})
	telemetryClient := &settingsTelemetryClient{}
	srv := newSettingsServer(nil)
	srv.telemetry = telemetryClient

	mustUpdateSettings(t, srv, &pb.UpdateSettingsRequest{
		EventTracingEnabled: boolptr(true),
		PosthogProjectToken: strptr("phc-updated"),
		PosthogHost:         strptr("https://telemetry.example"),
	})

	if telemetryClient.updates != 1 {
		t.Fatalf("telemetry updates = %d, want 1", telemetryClient.updates)
	}
	if !telemetryClient.settings.EventTracingEnabled || telemetryClient.settings.PostHogProjectToken != "phc-updated" || telemetryClient.settings.PostHogHost != "https://telemetry.example" {
		t.Fatalf("telemetry settings = %#v", telemetryClient.settings)
	}
}

func TestUpdateSettings_Validation(t *testing.T) {
	existingDir := t.TempDir()
	base := func() config.Settings {
		return config.Settings{
			WorktreeBaseDir:     existingDir,
			PollIntervalSeconds: 30,
			DefaultAgent:        "claude",
			Plugins:             []config.PluginConfig{{Name: "claude", Enabled: true}},
		}
	}

	// A regular file so a worktree base *under* it is uncreatable (MkdirAll can
	// not turn a file into a directory) — the residual invalid-path case now that
	// a merely-absent directory is created rather than rejected.
	regularFile := filepath.Join(existingDir, "a-file")
	if err := os.WriteFile(regularFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed regular file: %v", err)
	}

	cases := []struct {
		name string
		req  *pb.UpdateSettingsRequest
	}{
		{"empty worktree", &pb.UpdateSettingsRequest{WorktreeBaseDir: strptr("")}},
		{"uncreatable worktree", &pb.UpdateSettingsRequest{WorktreeBaseDir: strptr(filepath.Join(regularFile, "sub"))}},
		{"negative poll", &pb.UpdateSettingsRequest{PollIntervalSeconds: i32ptr(-5)}},
		{"default_agent not enabled", &pb.UpdateSettingsRequest{DefaultAgent: strptr("codex")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := seedSettings(t, base())
			before, _ := os.ReadFile(path)
			srv := newSettingsServer(nil)

			_, err := srv.UpdateSettings(context.Background(), connect.NewRequest(tc.req))
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Errorf("file was modified on a rejected update:\nbefore=%s\nafter=%s", before, after)
			}
		})
	}
}

// TestUpdateSettings_CreatesFreshWorktreeBaseDir proves the RPC mirrors the
// settings TUI: pointing worktree_base_dir at a not-yet-existing directory is
// accepted and the directory is created (the TUI relies on config.Load() to
// MkdirAll it), rather than being rejected for not existing.
func TestUpdateSettings_CreatesFreshWorktreeBaseDir(t *testing.T) {
	seedSettings(t, config.Settings{
		WorktreeBaseDir: t.TempDir(),
		DefaultAgent:    "claude",
		Plugins:         []config.PluginConfig{{Name: "claude", Enabled: true}},
	})
	srv := newSettingsServer(nil)

	fresh := filepath.Join(t.TempDir(), "new", "worktrees")
	got := mustUpdateSettings(t, srv, &pb.UpdateSettingsRequest{
		WorktreeBaseDir: strptr(fresh),
	})
	if got.GetWorktreeBaseDir() != fresh {
		t.Errorf("worktree = %q, want %q", got.GetWorktreeBaseDir(), fresh)
	}
	if info, err := os.Stat(fresh); err != nil || !info.IsDir() {
		t.Errorf("worktree_base_dir %q was not created as a directory (err=%v)", fresh, err)
	}
}

// TestUpdateSettings_DefaultAgentMustBeLoadedRunner proves default_agent parity
// with the TUI picker, which lists only loaded agent runners: an enabled plugin
// that is not a loaded runner (e.g. a task source) is rejected, while an enabled
// loaded runner is accepted.
func TestUpdateSettings_DefaultAgentMustBeLoadedRunner(t *testing.T) {
	seedSettings(t, config.Settings{
		WorktreeBaseDir: t.TempDir(),
		DefaultAgent:    "claude",
		Plugins: []config.PluginConfig{
			{Name: "claude", Enabled: true},
			{Name: "sentry", Enabled: true}, // enabled, but NOT an agent runner
		},
	})
	clients := map[string]agent.AgentRunnerClient{
		"claude": &listAgentsFakeClient{info: &pb.PluginInfo{Name: "claude"}},
	}
	srv := newSettingsServer(clients)

	// Rejected: an enabled non-runner plugin is not a valid default agent.
	_, err := srv.UpdateSettings(context.Background(), connect.NewRequest(&pb.UpdateSettingsRequest{
		DefaultAgent: strptr("sentry"),
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("expected InvalidArgument for non-runner default_agent, got %v", err)
	}

	// Accepted: an enabled, loaded agent runner.
	got := mustUpdateSettings(t, srv, &pb.UpdateSettingsRequest{DefaultAgent: strptr("claude")})
	if got.GetDefaultAgent() != "claude" {
		t.Errorf("default_agent = %q, want claude", got.GetDefaultAgent())
	}
}

// TestUpdateSettings_EnumValidatedAgainstDescriptor proves per-agent enum values
// are validated against the agent's advertised AllowedValues.
func TestUpdateSettings_EnumValidatedAgainstDescriptor(t *testing.T) {
	seedSettings(t, config.Settings{
		WorktreeBaseDir: t.TempDir(),
		DefaultAgent:    "claude",
		Plugins:         []config.PluginConfig{{Name: "codex", Enabled: true}},
	})
	clients := map[string]agent.AgentRunnerClient{
		"codex": &listAgentsFakeClient{info: &pb.PluginInfo{
			Name: "codex",
			UserSettings: []*pb.UserSetting{{
				Key:           "sandbox",
				Type:          pb.UserSettingType_USER_SETTING_TYPE_ENUM,
				AllowedValues: []string{"", "read-only", "workspace-write"},
			}},
		}},
	}
	srv := newSettingsServer(clients)

	// Rejected: value outside AllowedValues.
	_, err := srv.UpdateSettings(context.Background(), connect.NewRequest(&pb.UpdateSettingsRequest{
		Agents: []*pb.AgentSettingsUpdate{{Name: "codex", Config: map[string]string{"sandbox": "bogus"}}},
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("expected InvalidArgument for bad enum, got %v", err)
	}

	// Accepted: value in AllowedValues.
	got := mustUpdateSettings(t, srv, &pb.UpdateSettingsRequest{
		Agents: []*pb.AgentSettingsUpdate{{Name: "codex", Config: map[string]string{"sandbox": "workspace-write"}}},
	})
	if v := agentConfigValue(got, "codex", "sandbox"); v != "workspace-write" {
		t.Errorf("sandbox = %q, want workspace-write", v)
	}
}

// TestUpdateSettings_PerAgentConfigRoundTrip covers bool set/delete-on-false,
// string upsert + delete-on-empty, the enabled toggle, and creating a
// PluginConfig entry for a not-yet-present plugin.
func TestUpdateSettings_PerAgentConfigRoundTrip(t *testing.T) {
	seedSettings(t, config.Settings{
		WorktreeBaseDir: t.TempDir(),
		DefaultAgent:    "claude",
		Plugins:         []config.PluginConfig{{Name: "claude", Enabled: true}},
	})
	clients := map[string]agent.AgentRunnerClient{
		"claude": &listAgentsFakeClient{info: &pb.PluginInfo{
			Name: "claude",
			UserSettings: []*pb.UserSetting{
				{Key: "dangerously_skip_permissions", Type: pb.UserSettingType_USER_SETTING_TYPE_BOOL},
				{Key: "extra", Type: pb.UserSettingType_USER_SETTING_TYPE_STRING},
			},
		}},
	}
	srv := newSettingsServer(clients)

	// bool true is stored as "true".
	got := mustUpdateSettings(t, srv, &pb.UpdateSettingsRequest{
		Agents: []*pb.AgentSettingsUpdate{{Name: "claude", Config: map[string]string{"dangerously_skip_permissions": "true"}}},
	})
	if v := agentConfigValue(got, "claude", "dangerously_skip_permissions"); v != "true" {
		t.Fatalf("bool true not stored: %q", v)
	}

	// bool false deletes the key.
	got = mustUpdateSettings(t, srv, &pb.UpdateSettingsRequest{
		Agents: []*pb.AgentSettingsUpdate{{Name: "claude", Config: map[string]string{"dangerously_skip_permissions": "false"}}},
	})
	if _, ok := agentConfig(got, "claude")["dangerously_skip_permissions"]; ok {
		t.Fatalf("bool false should delete the key: %+v", agentConfig(got, "claude"))
	}

	// string upsert then delete-on-empty.
	got = mustUpdateSettings(t, srv, &pb.UpdateSettingsRequest{
		Agents: []*pb.AgentSettingsUpdate{{Name: "claude", Config: map[string]string{"extra": "hello"}}},
	})
	if v := agentConfigValue(got, "claude", "extra"); v != "hello" {
		t.Fatalf("string upsert failed: %q", v)
	}
	got = mustUpdateSettings(t, srv, &pb.UpdateSettingsRequest{
		Agents: []*pb.AgentSettingsUpdate{{Name: "claude", Config: map[string]string{"extra": ""}}},
	})
	if _, ok := agentConfig(got, "claude")["extra"]; ok {
		t.Fatalf("empty value should delete the key: %+v", agentConfig(got, "claude"))
	}

	// enabled toggle for the existing plugin.
	got = mustUpdateSettings(t, srv, &pb.UpdateSettingsRequest{
		Agents: []*pb.AgentSettingsUpdate{{Name: "claude", Enabled: boolptr(false)}},
	})
	if a := agentByName(got, "claude"); a == nil || a.GetEnabled() {
		t.Fatalf("claude should be disabled: %+v", a)
	}

	// creating a PluginConfig entry for a not-yet-present plugin.
	got = mustUpdateSettings(t, srv, &pb.UpdateSettingsRequest{
		Agents: []*pb.AgentSettingsUpdate{{Name: "opencode", Enabled: boolptr(true)}},
	})
	if a := agentByName(got, "opencode"); a == nil || !a.GetEnabled() {
		t.Fatalf("opencode plugin entry should be created + enabled: %+v", a)
	}
}

// TestUpdateSettings_RoundTripPersistsAndMatchesSaveTo proves an update is
// readable by a subsequent GetSettings and that the on-disk JSON is byte-for-byte
// what config.SaveTo produces for the resulting settings.
func TestUpdateSettings_RoundTripPersistsAndMatchesSaveTo(t *testing.T) {
	worktree := t.TempDir()
	path := seedSettings(t, config.Settings{
		WorktreeBaseDir:     worktree,
		PollIntervalSeconds: 30,
		DefaultAgent:        "claude",
		Plugins:             []config.PluginConfig{{Name: "claude", Enabled: true}},
	})
	srv := newSettingsServer(nil)

	mustUpdateSettings(t, srv, &pb.UpdateSettingsRequest{PollIntervalSeconds: i32ptr(120)})

	got := mustGetSettings(t, srv)
	if got.GetPollIntervalSeconds() != 120 {
		t.Fatalf("round-trip poll = %d, want 120", got.GetPollIntervalSeconds())
	}

	// On-disk JSON equals config.SaveTo output for the loaded settings.
	loaded, err := config.LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	golden := filepath.Join(t.TempDir(), "golden.json")
	if err := config.SaveTo(golden, loaded); err != nil {
		t.Fatalf("SaveTo golden: %v", err)
	}
	onDisk, _ := os.ReadFile(path)
	want, _ := os.ReadFile(golden)
	if string(onDisk) != string(want) {
		t.Errorf("on-disk JSON does not match config.SaveTo output:\ngot=%s\nwant=%s", onDisk, want)
	}
}

// TestUpdateSettings_PostHogHostDefaultsWhenTracing proves an empty posthog_host
// falls back to the first-party default only when tracing is enabled.
func TestUpdateSettings_PostHogHostDefaultsWhenTracing(t *testing.T) {
	seedSettings(t, config.Settings{
		WorktreeBaseDir: t.TempDir(),
		DefaultAgent:    "claude",
		Plugins:         []config.PluginConfig{{Name: "claude", Enabled: true}},
	})
	srv := newSettingsServer(nil)

	got := mustUpdateSettings(t, srv, &pb.UpdateSettingsRequest{
		EventTracingEnabled: boolptr(true),
		PosthogHost:         strptr("   "),
	})
	if got.GetPosthogHost() != telemetry.DefaultHost {
		t.Errorf("posthog_host = %q, want default %q", got.GetPosthogHost(), telemetry.DefaultHost)
	}
}

// --- helpers ---

func agentByName(g *pb.GlobalSettings, name string) *pb.AgentSettings {
	for _, a := range g.GetAgents() {
		if a.GetName() == name {
			return a
		}
	}
	return nil
}

func agentConfig(g *pb.GlobalSettings, name string) map[string]string {
	if a := agentByName(g, name); a != nil {
		return a.GetConfig()
	}
	return nil
}

func agentConfigValue(g *pb.GlobalSettings, name, key string) string {
	return agentConfig(g, name)[key]
}

// TestUpdateSettings_OptedInExperimentalDefaultAgent covers the opt-in path from
// BOS-1145 at the RPC layer. The persisted entry stays "enabled": false for an
// experimental plugin — config init writes it that way and the daemon's gate
// never persists its override — so validating the default agent against the
// persisted flag rejected a runner the daemon had actually loaded.
func TestUpdateSettings_OptedInExperimentalDefaultAgent(t *testing.T) {
	dir := t.TempDir()
	seedSettings(t, config.Settings{
		WorktreeBaseDir:     dir,
		PollIntervalSeconds: 30,
		DefaultAgent:        "claude",
		ExperimentalPlugins: []string{"opencode"},
		Plugins: []config.PluginConfig{
			{Name: "claude", Enabled: true},
			{Name: "opencode", Enabled: false},
		},
	})
	clients := map[string]agent.AgentRunnerClient{
		"claude":   &listAgentsFakeClient{info: &pb.PluginInfo{Name: "claude"}},
		"opencode": &listAgentsFakeClient{info: &pb.PluginInfo{Name: "opencode"}},
	}
	srv := newSettingsServer(clients)

	got := mustUpdateSettings(t, srv, &pb.UpdateSettingsRequest{DefaultAgent: strptr("opencode")})
	if got.GetDefaultAgent() != "opencode" {
		t.Errorf("default_agent = %q, want opencode; the opt-in makes it effectively enabled", got.GetDefaultAgent())
	}
}

// TestUpdateSettings_LegacyEnabledExperimentalDefaultAgentRejected is the other
// direction: a pre-gate "enabled": true entry with no opt-in is forced off at
// load, so accepting it as a default would persist an agent that never loads.
func TestUpdateSettings_LegacyEnabledExperimentalDefaultAgentRejected(t *testing.T) {
	dir := t.TempDir()
	seedSettings(t, config.Settings{
		WorktreeBaseDir:     dir,
		PollIntervalSeconds: 30,
		DefaultAgent:        "claude",
		Plugins: []config.PluginConfig{
			{Name: "claude", Enabled: true},
			{Name: "opencode", Enabled: true},
		},
	})
	clients := map[string]agent.AgentRunnerClient{
		"claude":   &listAgentsFakeClient{info: &pb.PluginInfo{Name: "claude"}},
		"opencode": &listAgentsFakeClient{info: &pb.PluginInfo{Name: "opencode"}},
	}
	srv := newSettingsServer(clients)

	_, err := srv.UpdateSettings(context.Background(), connect.NewRequest(&pb.UpdateSettingsRequest{
		DefaultAgent: strptr("opencode"),
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("expected InvalidArgument for a legacy-enabled experimental default_agent, got %v", err)
	}
}
