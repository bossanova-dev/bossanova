package server

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/agent"
)

// forbiddenSettings holds every key family the web must neither read nor
// write. Each string value carries "SECRET" so a leak into a read is visible.
const forbiddenSettings = `{
  "cloud_control": "full",
  "cloud_control_repos": [{"repo": "SECRET-repo", "mode": "read-only"}],
  "subagent_dispatch_grant": "SECRET-grant",
  "daemon_supervision_mode": "SECRET-supervision",
  "proof_upload": {"account_id": "SECRET-account", "bucket": "SECRET-bucket", "public_base_url": "https://SECRET.example"},
  "managed_accounts": {"max_rotations_per_run": 7, "default_cooldown_minutes": 11},
  "repair": {"cooldown_minutes": 13, "skills": {"repair": "SECRET-skill"}},
  "tmux_delivery": {"send_ready_deadline_seconds": 17},
  "child_env_passthrough": ["SECRET_ENV_*"],
  "posthog_project_token": "SECRET-posthog",
  "daemon_path_extra": ["/SECRET/bin"],
  "a_key_from_a_newer_build": {"SECRET": [1, 2, 3]},
  "default_agent": "claude",
  "plugins": [
    {"name": "claude", "path": "/SECRET/claude-plugin", "enabled": true, "config": {"effort": "max"}},
    {"name": "codex", "path": "/SECRET/codex-plugin", "enabled": true}
  ]
}`

func writeRawSettings(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("BOSS_SETTINGS_PATH", path)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readRawSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func codexDescriptors() *pb.PluginInfo {
	return &pb.PluginInfo{Name: "codex", UserSettings: []*pb.UserSetting{
		{Key: "sandbox", Type: pb.UserSettingType_USER_SETTING_TYPE_ENUM, AllowedValues: []string{"", "read-only", "workspace-write"}},
		{Key: "approval", Type: pb.UserSettingType_USER_SETTING_TYPE_ENUM, AllowedValues: []string{"", "on-request", "never"}},
	}}
}

func daemonSettingsServer() *Server {
	return newSettingsServer(map[string]agent.RunnerClient{
		"claude":   &listAgentsFakeClient{info: &pb.PluginInfo{Name: "claude"}},
		"codex":    &listAgentsFakeClient{info: codexDescriptors()},
		"opencode": &listAgentsFakeClient{info: &pb.PluginInfo{Name: "opencode"}},
	})
}

func everyAllowedFieldUpdate(t *testing.T) *pb.DaemonSettingsUpdate {
	return &pb.DaemonSettingsUpdate{
		DaemonName:               proto.String("studio"),
		WorktreeBaseDir:          proto.String(t.TempDir()),
		PollIntervalSeconds:      proto.Int32(60),
		ShouldDisplayOpenPorts:   proto.Bool(true),
		IsNotificationsEnabled:   proto.Bool(false),
		DefaultAgent:             proto.String("codex"),
		IsAccountRotationEnabled: proto.Bool(false),
		IsProofPagesEnabled:      proto.Bool(true),
		IsErrorTrackingEnabled:   proto.Bool(true),
		IsEventTracingEnabled:    proto.Bool(true),
		Agents: []*pb.DaemonAgentSettingsUpdate{
			{Name: "claude", Model: proto.String("opus"), IsPermissionBypassEnabled: proto.Bool(true)},
			{Name: "codex", Model: proto.String("gpt-5-codex"), Sandbox: proto.String("workspace-write"), Approval: proto.String("never"), IsPermissionBypassEnabled: proto.Bool(true)},
		},
	}
}

func TestDaemonSettings_ReadExposesNoForbiddenKey(t *testing.T) {
	writeRawSettings(t, forbiddenSettings)
	got, err := daemonSettingsServer().GetDaemonSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wire, err := protojson.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, []byte("SECRET")) {
		t.Fatalf("read leaked a forbidden value: %s", wire)
	}
	settings := got.GetSettings()
	if !settings.GetHasOwnProofBucket() || settings.GetCloudControl() != pb.CloudControl_CLOUD_CONTROL_FULL {
		t.Fatalf("read = %v, want the bucket's presence and the cloud control mode", settings)
	}
	var names []string
	for _, a := range settings.GetAgents() {
		names = append(names, a.GetName())
	}
	if !reflect.DeepEqual(names, []string{"claude", "codex", "opencode"}) {
		t.Fatalf("agents = %v, want every loaded runner", names)
	}
}

// TestUpdateDaemonSettings_WritesOnlyAllowlistedKeys sets every writable field
// and checks that every other key, known or not, is exactly as it was.
func TestUpdateDaemonSettings_WritesOnlyAllowlistedKeys(t *testing.T) {
	path := writeRawSettings(t, forbiddenSettings)
	before := readRawSettings(t, path)
	update := everyAllowedFieldUpdate(t)

	got, err := daemonSettingsServer().UpdateDaemonSettings(context.Background(), update)
	if err != nil {
		t.Fatal(err)
	}
	after := readRawSettings(t, path)

	written := map[string]any{
		"daemon_name": "studio", "worktree_base_dir": update.GetWorktreeBaseDir(), "poll_interval_seconds": 60.0,
		"display_open_ports": true, "notifications_enabled": false, "default_agent": "codex",
		"error_tracking_enabled": true, "event_tracing_enabled": true,
	}
	for key, want := range written {
		if after[key] != want {
			t.Errorf("%s = %v, want %v", key, after[key], want)
		}
		delete(after, key)
		delete(before, key)
	}
	managed := after["managed_accounts"].(map[string]any)
	if managed["enabled"] != false || managed["max_rotations_per_run"] != 7.0 {
		t.Errorf("managed_accounts = %v, want enabled false beside the untouched tuning", managed)
	}
	delete(managed, "enabled")
	proof := after["proof_upload"].(map[string]any)
	if proof["pages_enabled"] != true {
		t.Errorf("proof_upload = %v, want pages_enabled", proof)
	}
	delete(proof, "pages_enabled")
	wantPlugins := []any{
		map[string]any{"name": "claude", "path": "/SECRET/claude-plugin", "enabled": true, "config": map[string]any{"effort": "max", "model": "opus", "dangerously_skip_permissions": "true"}},
		map[string]any{"name": "codex", "path": "/SECRET/codex-plugin", "enabled": true, "config": map[string]any{"model": "gpt-5-codex", "sandbox": "workspace-write", "approval": "never", "dangerously_bypass_approvals_and_sandbox": "true"}},
	}
	if !reflect.DeepEqual(after["plugins"], wantPlugins) {
		t.Errorf("plugins = %v, want %v", after["plugins"], wantPlugins)
	}
	delete(after, "plugins")
	delete(before, "plugins")
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("keys outside the allowlist changed:\n got %v\nwant %v", after, before)
	}
	if !got.GetIsRestartNeeded() {
		t.Fatal("is_restart_needed = false after a daemon_name change")
	}
}

func TestUpdateDaemonSettings_PermissionBypassNeedsFullCloudControl(t *testing.T) {
	for _, mode := range []string{"read-only", "private", "typo"} {
		for _, name := range []string{"claude", "codex"} {
			t.Run(mode+"/"+name, func(t *testing.T) {
				path := writeRawSettings(t, `{"cloud_control": "`+mode+`", "plugins": [{"name": "claude"}, {"name": "codex"}]}`)
				seeded, _ := os.ReadFile(path)
				_, err := daemonSettingsServer().UpdateDaemonSettings(context.Background(), &pb.DaemonSettingsUpdate{
					Agents: []*pb.DaemonAgentSettingsUpdate{{Name: name, IsPermissionBypassEnabled: proto.Bool(true)}},
				})
				if connect.CodeOf(err) != connect.CodePermissionDenied {
					t.Fatalf("err = %v, want PermissionDenied", err)
				}
				if now, _ := os.ReadFile(path); !bytes.Equal(now, seeded) {
					t.Fatalf("refused update wrote the file: %s", now)
				}
				// Re-sending the stored value is not a change and is accepted.
				if _, err := daemonSettingsServer().UpdateDaemonSettings(context.Background(), &pb.DaemonSettingsUpdate{
					Agents: []*pb.DaemonAgentSettingsUpdate{{Name: name, IsPermissionBypassEnabled: proto.Bool(false)}},
				}); err != nil {
					t.Fatalf("unchanged bypass refused: %v", err)
				}
			})
		}
	}
	t.Run("full", func(t *testing.T) {
		path := writeRawSettings(t, `{"plugins": [{"name": "claude"}]}`)
		got, err := daemonSettingsServer().UpdateDaemonSettings(context.Background(), &pb.DaemonSettingsUpdate{
			Agents: []*pb.DaemonAgentSettingsUpdate{{Name: "claude", IsPermissionBypassEnabled: proto.Bool(true)}},
		})
		if err != nil {
			t.Fatal(err)
		}
		cfg := readRawSettings(t, path)["plugins"].([]any)[0].(map[string]any)["config"]
		if !reflect.DeepEqual(cfg, map[string]any{"dangerously_skip_permissions": "true"}) || !got.GetSettings().GetAgents()[0].GetIsPermissionBypassEnabled() {
			t.Fatalf("config = %v, reply = %v; want the bypass on", cfg, got.GetSettings().GetAgents()[0])
		}
	})
}

func TestUpdateDaemonSettings_RestartNeeded(t *testing.T) {
	const seed = `{"daemon_name": "old", "managed_accounts": {"enabled": false}, "plugins": [{"name": "claude"}, {"name": "opencode"}]}`
	for _, tc := range []struct {
		name   string
		update *pb.DaemonSettingsUpdate
		want   bool
	}{
		{"daemon name", &pb.DaemonSettingsUpdate{DaemonName: proto.String("new")}, true},
		{"poll interval", &pb.DaemonSettingsUpdate{PollIntervalSeconds: proto.Int32(30)}, true},
		{"error tracking", &pb.DaemonSettingsUpdate{IsErrorTrackingEnabled: proto.Bool(true)}, true},
		{"rotation on", &pb.DaemonSettingsUpdate{IsAccountRotationEnabled: proto.Bool(true)}, true},
		{"model", &pb.DaemonSettingsUpdate{Agents: []*pb.DaemonAgentSettingsUpdate{{Name: "opencode", Model: proto.String("anthropic/claude-sonnet")}}}, true},
		{"open ports", &pb.DaemonSettingsUpdate{ShouldDisplayOpenPorts: proto.Bool(true)}, false},
		{"notifications", &pb.DaemonSettingsUpdate{IsNotificationsEnabled: proto.Bool(false)}, false},
		{"event tracing", &pb.DaemonSettingsUpdate{IsEventTracingEnabled: proto.Bool(true)}, false},
		{"proof pages", &pb.DaemonSettingsUpdate{IsProofPagesEnabled: proto.Bool(true)}, false},
		{"same name", &pb.DaemonSettingsUpdate{DaemonName: proto.String(" old ")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeRawSettings(t, seed)
			got, err := daemonSettingsServer().UpdateDaemonSettings(context.Background(), tc.update)
			if err != nil {
				t.Fatal(err)
			}
			if got.GetIsRestartNeeded() != tc.want {
				t.Fatalf("is_restart_needed = %v, want %v", got.GetIsRestartNeeded(), tc.want)
			}
		})
	}
	t.Run("rotation off applies live", func(t *testing.T) {
		writeRawSettings(t, `{}`)
		got, err := daemonSettingsServer().UpdateDaemonSettings(context.Background(), &pb.DaemonSettingsUpdate{IsAccountRotationEnabled: proto.Bool(false)})
		if err != nil || got.GetIsRestartNeeded() {
			t.Fatalf("restart = %v (%v), want false", got.GetIsRestartNeeded(), err)
		}
	})
}

func TestUpdateDaemonSettings_RejectsInvalidValues(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		update *pb.DaemonSettingsUpdate
	}{
		{"poll too short", &pb.DaemonSettingsUpdate{PollIntervalSeconds: proto.Int32(5)}},
		{"poll too long", &pb.DaemonSettingsUpdate{PollIntervalSeconds: proto.Int32(86401)}},
		{"poll negative", &pb.DaemonSettingsUpdate{PollIntervalSeconds: proto.Int32(-1)}},
		{"relative worktree dir", &pb.DaemonSettingsUpdate{WorktreeBaseDir: proto.String("worktrees")}},
		{"empty worktree dir", &pb.DaemonSettingsUpdate{WorktreeBaseDir: proto.String(" ")}},
		{"uncreatable worktree dir", &pb.DaemonSettingsUpdate{WorktreeBaseDir: proto.String(filepath.Join(blocker, "sub"))}},
		{"daemon name newline", &pb.DaemonSettingsUpdate{DaemonName: proto.String("a\nb")}},
		{"daemon name too long", &pb.DaemonSettingsUpdate{DaemonName: proto.String(strings.Repeat("x", 129))}},
		{"default agent not loaded", &pb.DaemonSettingsUpdate{DefaultAgent: proto.String("gemini")}},
		{"unknown agent", &pb.DaemonSettingsUpdate{Agents: []*pb.DaemonAgentSettingsUpdate{{Name: "gemini", Model: proto.String("x")}}}},
		{"flag as model", &pb.DaemonSettingsUpdate{Agents: []*pb.DaemonAgentSettingsUpdate{{Name: "claude", Model: proto.String("--dangerously-skip-permissions")}}}},
		{"model with space", &pb.DaemonSettingsUpdate{Agents: []*pb.DaemonAgentSettingsUpdate{{Name: "claude", Model: proto.String("opus --x")}}}},
		{"bad sandbox", &pb.DaemonSettingsUpdate{Agents: []*pb.DaemonAgentSettingsUpdate{{Name: "codex", Sandbox: proto.String("danger")}}}},
		{"bad approval", &pb.DaemonSettingsUpdate{Agents: []*pb.DaemonAgentSettingsUpdate{{Name: "codex", Approval: proto.String("always")}}}},
		{"claude sandbox", &pb.DaemonSettingsUpdate{Agents: []*pb.DaemonAgentSettingsUpdate{{Name: "claude", Sandbox: proto.String("read-only")}}}},
		{"opencode bypass", &pb.DaemonSettingsUpdate{Agents: []*pb.DaemonAgentSettingsUpdate{{Name: "opencode", IsPermissionBypassEnabled: proto.Bool(true)}}}},
		{"valid field beside an invalid one", &pb.DaemonSettingsUpdate{DaemonName: proto.String("ok"), PollIntervalSeconds: proto.Int32(1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeRawSettings(t, `{"plugins": [{"name": "claude", "enabled": true}, {"name": "codex", "enabled": true}]}`)
			seeded, _ := os.ReadFile(path)
			_, err := daemonSettingsServer().UpdateDaemonSettings(context.Background(), tc.update)
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("err = %v, want InvalidArgument", err)
			}
			if now, _ := os.ReadFile(path); !bytes.Equal(now, seeded) {
				t.Fatalf("rejected update wrote the file: %s", now)
			}
		})
	}
}

// editingClient writes a local edit to settings.json the first time the
// handler asks the agent for its settings, which happens between the
// handler's read and its write.
type editingClient struct {
	listAgentsFakeClient
	path   string
	edited bool
}

func (c *editingClient) GetInfo(ctx context.Context) (*pb.PluginInfo, error) {
	if !c.edited {
		c.edited = true
		if err := os.WriteFile(c.path, []byte(`{"cloud_control": "full", "local_edit": true, "plugins": [{"name": "codex"}]}`), 0o600); err != nil {
			return nil, err
		}
	}
	return c.listAgentsFakeClient.GetInfo(ctx)
}

func TestUpdateDaemonSettings_KeepsConcurrentLocalEdit(t *testing.T) {
	path := writeRawSettings(t, `{"cloud_control": "full", "plugins": [{"name": "codex"}]}`)
	client := &editingClient{listAgentsFakeClient: listAgentsFakeClient{info: codexDescriptors()}, path: path}
	srv := newSettingsServer(map[string]agent.RunnerClient{"codex": client})

	if _, err := srv.UpdateDaemonSettings(context.Background(), &pb.DaemonSettingsUpdate{
		Agents: []*pb.DaemonAgentSettingsUpdate{{Name: "codex", Sandbox: proto.String("read-only")}},
	}); err != nil {
		t.Fatal(err)
	}
	got := readRawSettings(t, path)
	if got["local_edit"] != true {
		t.Fatalf("settings = %v, lost the local edit made during the update", got)
	}
	if sandbox := got["plugins"].([]any)[0].(map[string]any)["config"].(map[string]any)["sandbox"]; sandbox != "read-only" {
		t.Fatalf("sandbox = %v, want read-only", sandbox)
	}
}
