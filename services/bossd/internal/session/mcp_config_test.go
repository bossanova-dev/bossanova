package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// hermeticAppDataDir points config.Load() at a temp settings.json whose
// app_data_dir is a temp dir, so WriteSessionMcpConfig resolves a deterministic
// location on every platform (macOS does not honor XDG_CONFIG_HOME).
func hermeticAppDataDir(t *testing.T) string {
	t.Helper()
	appData := t.TempDir()
	settings := filepath.Join(t.TempDir(), "settings.json")
	body := `{"app_data_dir":` + strconv.Quote(appData) + `}`
	if err := os.WriteFile(settings, []byte(body), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", settings)
	return appData
}

func TestWriteSessionMcpConfig_EmptyMcpBinReturnsEmpty(t *testing.T) {
	path, err := WriteSessionMcpConfig(SessionFacts{AgentSessionID: "abc", McpBin: "", Socket: "/s.sock"})
	if err != nil {
		t.Fatalf("WriteSessionMcpConfig: %v", err)
	}
	if path != "" {
		t.Fatalf("want empty path when McpBin empty, got %q", path)
	}
}

func TestMcpConfigJSON_ShapeAndServerKey(t *testing.T) {
	raw, err := mcpConfigJSON("/trusted/mcp", "/run/bossd.sock", false)
	if err != nil {
		t.Fatalf("mcpConfigJSON: %v", err)
	}
	var doc struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	boss, ok := doc.MCPServers["boss"]
	if !ok {
		t.Fatal(`want a "boss" server key (drives the mcp__boss__* namespace)`)
	}
	if boss.Command != "/trusted/mcp" {
		t.Fatalf("command = %q, want /trusted/mcp", boss.Command)
	}
	if strings.Join(boss.Args, " ") != "--socket /run/bossd.sock" {
		t.Fatalf("args = %v, want [--socket /run/bossd.sock]", boss.Args)
	}
}

func TestMcpConfigJSON_NoSocketOmitsArgs(t *testing.T) {
	raw, err := mcpConfigJSON("/trusted/mcp", "", false)
	if err != nil {
		t.Fatalf("mcpConfigJSON: %v", err)
	}
	var doc struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := doc.MCPServers["boss"].Args; len(got) != 0 {
		t.Fatalf("want no args when socket empty, got %v", got)
	}
}

// TestWriteSessionMcpConfig_CuratedForCron pins the cron/fleet curation: a cron
// SessionFacts (IsCron true) must yield BOTH the stdio "boss" server AND the
// HTTP "bossanova-linear" server, so a strict-mode agent still reaches Linear.
// The Authorization header references ${LINEAR_API_KEY} by NAME — the literal is
// written verbatim and the real key is never inlined (AC: no secrets on disk).
func TestWriteSessionMcpConfig_CuratedForCron(t *testing.T) {
	hermeticAppDataDir(t)
	path, err := WriteSessionMcpConfig(SessionFacts{
		AgentSessionID: "cron-sess",
		McpBin:         "/trusted/mcp",
		Socket:         "/run/bossd.sock",
		IsCron:         true,
	})
	if err != nil {
		t.Fatalf("WriteSessionMcpConfig: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var doc struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	boss, ok := doc.MCPServers["boss"]
	if !ok || boss.Command != "/trusted/mcp" || strings.Join(boss.Args, " ") != "--socket /run/bossd.sock" {
		t.Fatalf("boss server malformed: %+v", boss)
	}
	linear, ok := doc.MCPServers["bossanova-linear"]
	if !ok {
		t.Fatal(`cron config must include "bossanova-linear" so strict mode keeps Linear access`)
	}
	if linear.Type != "http" || linear.URL != "https://mcp.linear.app/mcp" {
		t.Fatalf("linear server = %+v, want http https://mcp.linear.app/mcp", linear)
	}
	if got := linear.Headers["Authorization"]; got != "Bearer ${LINEAR_API_KEY}" {
		t.Fatalf("Authorization = %q, want literal Bearer ${LINEAR_API_KEY}", got)
	}
	if !strings.Contains(string(raw), "${LINEAR_API_KEY}") {
		t.Fatalf("raw config must contain the literal ${LINEAR_API_KEY} env reference: %s", raw)
	}
	// No inlined key: the header value must be the env-var reference, never a
	// resolved 40+/64-hex-like token.
	if hexToken := regexp.MustCompile(`[0-9a-fA-F]{40,}`); hexToken.Match(raw) {
		t.Fatalf("raw config appears to contain an inlined secret token: %s", raw)
	}
}

// TestWriteSessionMcpConfig_NonCronBossOnly pins that interactive (non-cron)
// spawns are unchanged: ONLY the "boss" server, no Linear entry, preserving the
// pre-curation shape byte-for-byte.
func TestWriteSessionMcpConfig_NonCronBossOnly(t *testing.T) {
	hermeticAppDataDir(t)
	path, err := WriteSessionMcpConfig(SessionFacts{
		AgentSessionID: "interactive-sess",
		McpBin:         "/trusted/mcp",
		Socket:         "/run/bossd.sock",
		IsCron:         false,
	})
	if err != nil {
		t.Fatalf("WriteSessionMcpConfig: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := doc.MCPServers["boss"]; !ok {
		t.Fatal(`non-cron config must still include the "boss" server`)
	}
	if len(doc.MCPServers) != 1 {
		t.Fatalf("non-cron config must contain ONLY boss, got keys %v", doc.MCPServers)
	}
	// Byte-identical to the pre-curation renderer for the same inputs.
	want, err := mcpConfigJSON("/trusted/mcp", "/run/bossd.sock", false)
	if err != nil {
		t.Fatalf("mcpConfigJSON: %v", err)
	}
	if string(raw) != string(want) {
		t.Fatalf("non-cron output drifted from pre-curation shape:\n got %s\nwant %s", raw, want)
	}
}

func TestWriteSessionMcpConfig_WritesUnderAppDataNotWorktree(t *testing.T) {
	appData := hermeticAppDataDir(t)
	path, err := WriteSessionMcpConfig(SessionFacts{
		AgentSessionID: "sess-1",
		McpBin:         "/trusted/mcp",
		Socket:         "/run/bossd.sock",
	})
	if err != nil {
		t.Fatalf("WriteSessionMcpConfig: %v", err)
	}
	if path == "" {
		t.Fatal("want a non-empty path")
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("want absolute path, got %q", path)
	}
	if strings.Contains(path, "worktree") || filepath.Base(path) == ".mcp.json" {
		t.Fatalf("config must not be in a worktree or be .mcp.json: %q", path)
	}
	wantDir := filepath.Join(appData, "mcp-configs")
	if got := filepath.Dir(path); got != wantDir {
		t.Fatalf("config dir = %q, want %q (must live under app-data)", got, wantDir)
	}
	if filepath.Base(path) != "sess-1.json" {
		t.Fatalf("config file = %q, want sess-1.json (keyed by agent-session id)", filepath.Base(path))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config not written: %v", err)
	}
}

func TestWriteSessionMcpConfig_SanitizesHostileID(t *testing.T) {
	appData := hermeticAppDataDir(t)
	wantDir := filepath.Join(appData, "mcp-configs")
	// Each hostile id must stay inside mcp-configs and must not resolve to a
	// sibling app-data file like settings.json.
	for _, id := range []string{"../settings", "..", ".", "a/b", `..\settings`, "x/../../settings"} {
		path, err := WriteSessionMcpConfig(SessionFacts{
			AgentSessionID: id,
			McpBin:         "/trusted/mcp",
			Socket:         "/run/bossd.sock",
		})
		if err != nil {
			t.Fatalf("WriteSessionMcpConfig(%q): %v", id, err)
		}
		if got := filepath.Dir(path); got != wantDir {
			t.Fatalf("id %q escaped: dir = %q, want %q", id, got, wantDir)
		}
		if base := filepath.Base(path); base == "settings.json" || strings.ContainsAny(base, `/\`) {
			t.Fatalf("id %q produced unsafe filename %q", id, base)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("id %q config not written: %v", id, err)
		}
	}
}

func TestWriteSessionMcpConfig_OverwritesPerSpawn(t *testing.T) {
	hermeticAppDataDir(t)
	facts := SessionFacts{AgentSessionID: "sess-2", McpBin: "/trusted/mcp", Socket: "/a.sock"}
	first, err := WriteSessionMcpConfig(facts)
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	facts.Socket = "/b.sock"
	second, err := WriteSessionMcpConfig(facts)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if first != second {
		t.Fatalf("per-spawn path must be stable (keyed by id): %q vs %q", first, second)
	}
	raw, err := os.ReadFile(second)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(raw), "/b.sock") || strings.Contains(string(raw), "/a.sock") {
		t.Fatalf("config not overwritten with latest socket: %s", raw)
	}
}
