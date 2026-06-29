package main

import (
	"strings"
	"testing"
)

func TestResolveEnvReport_EnvFirst(t *testing.T) {
	env := map[string]string{
		"BOSS_SESSION_ID":       "sess-123",
		"BOSS_AGENT_SESSION_ID": "chat-456",
		"BOSS_REPO_ID":          "repo-789",
		"BOSS_AGENT":            "codex",
		"BOSS_WORKTREE":         "/tmp/wt",
		"BOSS_SETTINGS_PATH":    "/cfg/settings.json",
		"BOSS_SOCKET":           "/cfg/bossd.sock",
		"BOSS_BIN":              "/usr/local/bin/boss",
		"BOSS_MCP_BIN":          "/usr/local/bin/mcp",
		"BOSS_CRON":             "true",
		"BOSS_CRON_JOB_ID":      "cron-1",
		"BOSS_CRON_NAME":        "bs-technical-debt",
	}
	got := resolveEnvReport(func(k string) string { return env[k] })

	if got.Session.SessionID != "sess-123" {
		t.Errorf("SessionID = %q, want sess-123", got.Session.SessionID)
	}
	if got.Session.Agent != "codex" {
		t.Errorf("Agent = %q, want codex", got.Session.Agent)
	}
	if got.Mode != "cron" {
		t.Errorf("Mode = %q, want cron", got.Mode)
	}
	if got.Cron == nil || got.Cron.Name != "bs-technical-debt" {
		t.Errorf("Cron not populated from env: %+v", got.Cron)
	}
	if got.Binaries.Boss != "/usr/local/bin/boss" {
		t.Errorf("Binaries.Boss = %q", got.Binaries.Boss)
	}
	if got.Binaries.SettingsPath != "/cfg/settings.json" {
		t.Errorf("SettingsPath = %q, want /cfg/settings.json (env-first)", got.Binaries.SettingsPath)
	}
	if got.Daemon.Socket != "/cfg/bossd.sock" {
		t.Errorf("Daemon.Socket = %q, want /cfg/bossd.sock (env-first)", got.Daemon.Socket)
	}
}

func TestResolveEnvReport_ManagedNonCron(t *testing.T) {
	env := map[string]string{
		"BOSS_SESSION_ID": "sess-123",
		"BOSS_AGENT":      "claude",
	}
	got := resolveEnvReport(func(k string) string { return env[k] })
	if got.Mode != "managed" {
		t.Errorf("Mode = %q, want managed", got.Mode)
	}
	if got.Cron != nil {
		t.Errorf("Cron should be nil for non-cron session, got %+v", got.Cron)
	}
}

func TestResolveEnvReport_StandaloneFallsBackToConfig(t *testing.T) {
	// No BOSS_* vars: not a managed session. SettingsPath/Socket fall back to
	// fresh config resolution; session identifiers are empty.
	got := resolveEnvReport(func(string) string { return "" })
	if got.Mode != "standalone" {
		t.Errorf("Mode = %q, want standalone", got.Mode)
	}
	if got.Session.SessionID != "" {
		t.Errorf("SessionID = %q, want empty in standalone mode", got.Session.SessionID)
	}
	// Fresh resolution should at least produce a settings path.
	if got.Binaries.SettingsPath == "" {
		t.Errorf("SettingsPath should be resolved from config in standalone mode")
	}
}

func TestResolveEnvReport_CapabilitiesPopulated(t *testing.T) {
	got := resolveEnvReport(func(string) string { return "" })
	if len(got.Capabilities.MCP) == 0 {
		t.Error("Capabilities.MCP should be non-empty")
	}
	foundLS := false
	for _, c := range got.Capabilities.CLI {
		if c == "boss ls" {
			foundLS = true
		}
	}
	if !foundLS {
		t.Errorf("Capabilities.CLI should include 'boss ls'; got %v", got.Capabilities.CLI)
	}
	foundListSessions := false
	for _, m := range got.Capabilities.MCP {
		if m == "list_sessions" {
			foundListSessions = true
		}
	}
	if !foundListSessions {
		t.Errorf("Capabilities.MCP should include 'list_sessions'; got %v", got.Capabilities.MCP)
	}
}

func TestRenderEnvHuman_ContainsKeySections(t *testing.T) {
	rep := resolveEnvReport(func(string) string { return "" })
	out := renderEnvHuman(rep)
	for _, want := range []string{"Mode:", "Capabilities", "CLI commands", "MCP tools"} {
		if !strings.Contains(out, want) {
			t.Errorf("human output missing %q\n---\n%s", want, out)
		}
	}
}
