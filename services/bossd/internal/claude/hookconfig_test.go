package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readSettings parses worktree/.claude/settings.local.json into a map
// so test assertions can introspect structure rather than comparing
// serialized strings (order is unstable).
func readSettings(t *testing.T, worktree string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(worktree, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return out
}

// findBossdStop returns the bossd-finalize entry from the Stop array,
// or fails the test if it isn't there exactly once.
func findBossdStop(t *testing.T, settings map[string]any) map[string]any {
	t.Helper()
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("settings.hooks missing or wrong type: %T", settings["hooks"])
	}
	stops, ok := hooks["Stop"].([]any)
	if !ok {
		t.Fatalf("settings.hooks.Stop missing or wrong type: %T", hooks["Stop"])
	}
	var found map[string]any
	matches := 0
	for _, raw := range stops {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if m["matcher"] == hookMatcherKey {
			found = m
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("expected exactly 1 bossd-finalize entry in Stop[], got %d", matches)
	}
	return found
}

// assertCommandContains verifies the embedded curl command references
// the expected token, port, and session so FL5-3 and downstream tests
// can trust WriteHookConfig actually plumbed the secrets through.
func assertCommandContains(t *testing.T, entry map[string]any, wants ...string) {
	t.Helper()
	innerHooks, ok := entry["hooks"].([]any)
	if !ok || len(innerHooks) == 0 {
		t.Fatalf("entry.hooks missing or empty: %v", entry["hooks"])
	}
	inner, ok := innerHooks[0].(map[string]any)
	if !ok {
		t.Fatalf("entry.hooks[0] wrong type: %T", innerHooks[0])
	}
	cmd, ok := inner["command"].(string)
	if !ok {
		t.Fatalf("entry.hooks[0].command missing: %v", inner)
	}
	for _, w := range wants {
		if !strings.Contains(cmd, w) {
			t.Errorf("command missing %q: %s", w, cmd)
		}
	}
}

// TestWriteHookConfig_EmptyWorktree — no .claude dir yet, no existing
// settings. Writes a fresh file with our Stop entry.
func TestWriteHookConfig_EmptyWorktree(t *testing.T) {
	worktree := t.TempDir()

	if err := WriteHookConfig(worktree, "sess-1", "tok-abc", 45678); err != nil {
		t.Fatalf("WriteHookConfig: %v", err)
	}

	settings := readSettings(t, worktree)
	entry := findBossdStop(t, settings)
	assertCommandContains(t, entry,
		"Authorization: Bearer tok-abc",
		"http://127.0.0.1:45678/hooks/finalize/sess-1",
	)
}

// TestWriteHookConfig_EmptyFile — .claude/settings.local.json exists
// but is empty. Should be treated as "{}".
func TestWriteHookConfig_EmptyFile(t *testing.T) {
	worktree := t.TempDir()
	claudeDir := filepath.Join(worktree, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	settingsPath := filepath.Join(claudeDir, "settings.local.json")
	if err := os.WriteFile(settingsPath, []byte("   \n  "), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := WriteHookConfig(worktree, "sess-2", "tok-2", 9000); err != nil {
		t.Fatalf("WriteHookConfig: %v", err)
	}

	settings := readSettings(t, worktree)
	findBossdStop(t, settings) // passes if exactly one entry added
}

// TestWriteHookConfig_PreservesOtherKeys — existing settings file has
// unrelated top-level keys and must leave them untouched.
func TestWriteHookConfig_PreservesOtherKeys(t *testing.T) {
	worktree := t.TempDir()
	claudeDir := filepath.Join(worktree, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	existing := map[string]any{
		"permissions": map[string]any{
			"allow": []any{"Bash(ls)"},
		},
		"env": map[string]any{
			"SOME_VAR": "value",
		},
	}
	data, _ := json.Marshal(existing)
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.local.json"), data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := WriteHookConfig(worktree, "sess-3", "tok-3", 1234); err != nil {
		t.Fatalf("WriteHookConfig: %v", err)
	}

	settings := readSettings(t, worktree)
	if _, ok := settings["permissions"]; !ok {
		t.Error("permissions key was dropped")
	}
	if _, ok := settings["env"]; !ok {
		t.Error("env key was dropped")
	}
	findBossdStop(t, settings)
}

// TestWriteHookConfig_PreservesOtherStopHooks — existing Stop array has
// non-bossd hooks. They must all survive alongside ours.
func TestWriteHookConfig_PreservesOtherStopHooks(t *testing.T) {
	worktree := t.TempDir()
	claudeDir := filepath.Join(worktree, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	otherHook := map[string]any{
		"matcher": "user-custom",
		"hooks": []any{
			map[string]any{"type": "command", "command": "echo bye"},
		},
	}
	existing := map[string]any{
		"hooks": map[string]any{
			"Stop": []any{otherHook},
		},
	}
	data, _ := json.Marshal(existing)
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.local.json"), data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := WriteHookConfig(worktree, "sess-4", "tok-4", 5555); err != nil {
		t.Fatalf("WriteHookConfig: %v", err)
	}

	settings := readSettings(t, worktree)
	stops := settings["hooks"].(map[string]any)["Stop"].([]any)
	if len(stops) != 2 {
		t.Fatalf("Stop array length = %d, want 2 (user hook + bossd)", len(stops))
	}

	// user-custom entry must be unchanged.
	var foundUser bool
	for _, raw := range stops {
		m := raw.(map[string]any)
		if m["matcher"] == "user-custom" {
			foundUser = true
			innerHooks := m["hooks"].([]any)
			inner := innerHooks[0].(map[string]any)
			if inner["command"] != "echo bye" {
				t.Errorf("user hook command mutated: %v", inner["command"])
			}
		}
	}
	if !foundUser {
		t.Error("user-custom Stop hook was dropped")
	}
	findBossdStop(t, settings)
}

// TestWriteHookConfig_ReplacesOwnEntry — calling twice must not
// duplicate our entry (idempotency / re-run safety).
func TestWriteHookConfig_ReplacesOwnEntry(t *testing.T) {
	worktree := t.TempDir()

	if err := WriteHookConfig(worktree, "sess-5", "tok-old", 1111); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := WriteHookConfig(worktree, "sess-5", "tok-new", 2222); err != nil {
		t.Fatalf("second write: %v", err)
	}

	settings := readSettings(t, worktree)
	stops := settings["hooks"].(map[string]any)["Stop"].([]any)
	if len(stops) != 1 {
		t.Fatalf("Stop array length = %d, want 1 (dupe on rewrite)", len(stops))
	}
	entry := findBossdStop(t, settings)
	assertCommandContains(t, entry,
		"Authorization: Bearer tok-new",
		"127.0.0.1:2222",
	)
}

// TestWriteHookConfig_MalformedJSON — refuse to clobber a file we
// can't parse. Users get an error so they can investigate.
func TestWriteHookConfig_MalformedJSON(t *testing.T) {
	worktree := t.TempDir()
	claudeDir := filepath.Join(worktree, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.local.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	err := WriteHookConfig(worktree, "sess-6", "tok-6", 3333)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error should mention parse failure: %v", err)
	}

	// Original file untouched (no half-written state).
	raw, _ := os.ReadFile(filepath.Join(claudeDir, "settings.local.json"))
	if string(raw) != "{not json" {
		t.Errorf("malformed file was mutated: %q", raw)
	}
}

// TestWriteHookConfig_TopLevelArray — a JSON array at the top level is
// not a valid settings config and should error rather than silently
// discarding the user's data.
func TestWriteHookConfig_TopLevelArray(t *testing.T) {
	worktree := t.TempDir()
	claudeDir := filepath.Join(worktree, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.local.json"), []byte("[]"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	err := WriteHookConfig(worktree, "sess-7", "tok-7", 4444)
	if err == nil {
		t.Fatal("expected error for top-level array, got nil")
	}
}

// TestWriteHookConfig_FilePermissions — the rendered settings file must
// be user-read/write only (0600) since it contains the hook token.
func TestWriteHookConfig_FilePermissions(t *testing.T) {
	worktree := t.TempDir()
	if err := WriteHookConfig(worktree, "sess-8", "tok-8", 5678); err != nil {
		t.Fatalf("WriteHookConfig: %v", err)
	}
	info, err := os.Stat(filepath.Join(worktree, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("settings file perm = %o, want 0600", info.Mode().Perm())
	}
}

// TestWriteHookConfig_ValidationErrors — empty args fail fast with
// descriptive errors before touching the filesystem.
func TestWriteHookConfig_ValidationErrors(t *testing.T) {
	cases := []struct {
		name      string
		worktree  string
		sessionID string
		token     string
		port      int
		wantMsg   string
	}{
		{"empty worktree", "", "s", "t", 1, "worktreePath"},
		{"empty session", "/tmp/x", "", "t", 1, "sessionID"},
		{"empty token", "/tmp/x", "s", "", 1, "token"},
		{"zero port", "/tmp/x", "s", "t", 0, "port"},
		{"negative port", "/tmp/x", "s", "t", -1, "port"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := WriteHookConfig(c.worktree, c.sessionID, c.token, c.port)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), c.wantMsg) {
				t.Errorf("error %q missing %q", err, c.wantMsg)
			}
		})
	}
}
