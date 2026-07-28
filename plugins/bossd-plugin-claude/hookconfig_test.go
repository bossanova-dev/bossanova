package main

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

	if err := WriteHookConfig(worktree, "sess-1", "", "tok-abc", 45678); err != nil {
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

	if err := WriteHookConfig(worktree, "sess-2", "", "tok-2", 9000); err != nil {
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

	if err := WriteHookConfig(worktree, "sess-3", "", "tok-3", 1234); err != nil {
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

	if err := WriteHookConfig(worktree, "sess-4", "", "tok-4", 5555); err != nil {
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

	if err := WriteHookConfig(worktree, "sess-5", "", "tok-old", 1111); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := WriteHookConfig(worktree, "sess-5", "", "tok-new", 2222); err != nil {
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

	err := WriteHookConfig(worktree, "sess-6", "", "tok-6", 3333)
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

	err := WriteHookConfig(worktree, "sess-7", "", "tok-7", 4444)
	if err == nil {
		t.Fatal("expected error for top-level array, got nil")
	}
}

// TestWriteHookConfig_FilePermissions — the rendered settings file must
// be user-read/write only (0600) since it contains the hook token.
func TestWriteHookConfig_FilePermissions(t *testing.T) {
	worktree := t.TempDir()
	if err := WriteHookConfig(worktree, "sess-8", "", "tok-8", 5678); err != nil {
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

// TestWriteHookConfig_DirPermissions locks in the owner-only 0o700 mode on
// the created .claude directory (G301) — it holds the token-bearing
// settings.local.json, so it must not be world/group readable.
func TestWriteHookConfig_DirPermissions(t *testing.T) {
	worktree := t.TempDir()
	if err := WriteHookConfig(worktree, "sess-dir", "", "tok-dir", 5678); err != nil {
		t.Fatalf("WriteHookConfig: %v", err)
	}
	info, err := os.Stat(filepath.Join(worktree, ".claude"))
	if err != nil {
		t.Fatalf("stat .claude dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf(".claude is not a directory")
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf(".claude dir perm = %o, want 0700", info.Mode().Perm())
	}
}

// TestWriteHookConfig_ValidationErrors — empty args fail fast with
// descriptive errors before touching the filesystem.
func TestWriteHookConfig_ValidationErrors(t *testing.T) {
	cases := []struct {
		name           string
		worktree       string
		sessionID      string
		agentSessionID string
		token          string
		port           int
		wantMsg        string
	}{
		{"empty worktree", "", "s", "", "t", 1, "worktreePath"},
		{"empty session", "/tmp/x", "", "", "t", 1, "sessionID"},
		{"empty token", "/tmp/x", "s", "", "", 1, "token"},
		{"zero port", "/tmp/x", "s", "", "t", 0, "port"},
		{"negative port", "/tmp/x", "s", "", "t", -1, "port"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := WriteHookConfig(c.worktree, c.sessionID, c.agentSessionID, c.token, c.port)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), c.wantMsg) {
				t.Errorf("error %q missing %q", err, c.wantMsg)
			}
		})
	}
}

// findRunStop returns the run-keyed entry for the given agentSessionID
// from the Stop array, or fails the test if it isn't there exactly once.
func findRunStop(t *testing.T, settings map[string]any, agentSessionID string) map[string]any {
	t.Helper()
	wantMatcher := runHookMatcherPrefix + agentSessionID
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
		if m["matcher"] == wantMatcher {
			found = m
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("expected exactly 1 %s entry in Stop[], got %d", wantMatcher, matches)
	}
	return found
}

// notifCommand returns the first inner command string of a hook group, or ""
// if the shape doesn't match. Lets tests key the Notification entry off its
// POST URL, the same ownership key WriteHookConfig uses.
func notifCommand(entry map[string]any) string {
	inner, ok := entry["hooks"].([]any)
	if !ok || len(inner) == 0 {
		return ""
	}
	h, ok := inner[0].(map[string]any)
	if !ok {
		return ""
	}
	cmd, _ := h["command"].(string)
	return cmd
}

// findRunNotification returns bossd's question-hook entry for the given
// agentSessionID from the Notification array, or fails the test if it isn't
// there exactly once. The entry is keyed by the unique substring of its POST
// URL (/hooks/question/{id}), NOT by matcher: bossd installs it with an EMPTY
// matcher because Claude's Notification event filters the matcher by
// notification type, so a synthetic matcher would make the hook inert (the
// original BOS-485 bug). This helper matching on URL is what keeps the test
// honest about the fired-hook contract rather than a dead identifier.
func findRunNotification(t *testing.T, settings map[string]any, agentSessionID string) map[string]any {
	t.Helper()
	wantURL := "/hooks/question/" + agentSessionID
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("settings.hooks missing or wrong type: %T", settings["hooks"])
	}
	notifs, ok := hooks["Notification"].([]any)
	if !ok {
		t.Fatalf("settings.hooks.Notification missing or wrong type: %T", hooks["Notification"])
	}
	var found map[string]any
	matches := 0
	for _, raw := range notifs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if strings.Contains(notifCommand(m), wantURL) {
			found = m
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("expected exactly 1 entry POSTing to %s in Notification[], got %d", wantURL, matches)
	}
	return found
}

// TestWriteHookConfig_RunKeyed_InstallsNotificationHook — a run-keyed write
// installs a Notification hook alongside the Stop hook, POSTing to
// /hooks/question/{id} with the same token. The Notification entry MUST use an
// empty matcher (so Claude, which filters the Notification matcher by type,
// actually fires it) and MUST forward the payload body so bossd can classify by
// notification_type. These two assertions are the guard against regressing to
// the original inert-hook / false-green bug.
func TestWriteHookConfig_RunKeyed_InstallsNotificationHook(t *testing.T) {
	worktree := t.TempDir()
	const agentSession = "agent-q-1"

	if err := WriteHookConfig(worktree, "sess-q1", agentSession, "tok-q", 45678); err != nil {
		t.Fatalf("WriteHookConfig: %v", err)
	}

	settings := readSettings(t, worktree)

	// Stop hook still present and correct.
	stopEntry := findRunStop(t, settings, agentSession)
	assertCommandContains(t, stopEntry,
		"Authorization: Bearer tok-q",
		"http://127.0.0.1:45678/hooks/agent-run-complete/"+agentSession,
	)

	// Notification hook present, POSTs to question with the same token, and
	// forwards the JSON payload on stdin for server-side classification.
	notifEntry := findRunNotification(t, settings, agentSession)
	assertCommandContains(t, notifEntry,
		"Authorization: Bearer tok-q",
		"http://127.0.0.1:45678/hooks/question/"+agentSession,
		"--data-binary @-",
		"Content-Type: application/json",
	)

	// Empty matcher: Claude filters the Notification matcher by notification
	// type, so anything non-empty (e.g. a synthetic per-run id) matches no type
	// and the hook never fires. This is the crux of the BOS-485 fix.
	if m, ok := notifEntry["matcher"].(string); !ok || m != "" {
		t.Errorf("Notification matcher = %q (ok=%v), want empty string so the hook fires on every type", notifEntry["matcher"], ok)
	}
}

// TestWriteHookConfig_SessionKeyed_NoNotificationHook — a session-keyed
// (cron finalize) write has no agent_session_id, so no Notification hook is
// installed (there's no id to key or POST).
func TestWriteHookConfig_SessionKeyed_NoNotificationHook(t *testing.T) {
	worktree := t.TempDir()
	if err := WriteHookConfig(worktree, "sess-nonotif", "", "tok", 1234); err != nil {
		t.Fatalf("WriteHookConfig: %v", err)
	}
	settings := readSettings(t, worktree)
	hooks, _ := settings["hooks"].(map[string]any)
	if _, ok := hooks["Notification"]; ok {
		t.Errorf("session-keyed write installed a Notification hook; want none")
	}
}

// TestWriteHookConfig_Notification_PreservesUserHooks — an existing
// user/repo Notification hook survives a run-keyed write alongside ours.
func TestWriteHookConfig_Notification_PreservesUserHooks(t *testing.T) {
	worktree := t.TempDir()
	claudeDir := filepath.Join(worktree, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	userNotif := map[string]any{
		"matcher": "user-notif",
		"hooks": []any{
			map[string]any{"type": "command", "command": "echo hi"},
		},
	}
	userStop := map[string]any{
		"matcher": "user-stop",
		"hooks": []any{
			map[string]any{"type": "command", "command": "echo stop"},
		},
	}
	existing := map[string]any{
		"hooks": map[string]any{
			"Notification": []any{userNotif},
			"Stop":         []any{userStop},
		},
	}
	data, _ := json.Marshal(existing)
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.local.json"), data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := WriteHookConfig(worktree, "sess-u", "agent-u", "tok-u", 5555); err != nil {
		t.Fatalf("WriteHookConfig: %v", err)
	}

	settings := readSettings(t, worktree)
	notifs := settings["hooks"].(map[string]any)["Notification"].([]any)
	if len(notifs) != 2 {
		t.Fatalf("Notification length = %d, want 2 (user + bossd)", len(notifs))
	}
	var foundUser bool
	for _, raw := range notifs {
		m := raw.(map[string]any)
		if m["matcher"] == "user-notif" {
			foundUser = true
			if m["hooks"].([]any)[0].(map[string]any)["command"] != "echo hi" {
				t.Error("user Notification hook mutated")
			}
		}
	}
	if !foundUser {
		t.Error("user Notification hook was dropped")
	}
	findRunNotification(t, settings, "agent-u")
	// User Stop hook preserved too.
	stops := settings["hooks"].(map[string]any)["Stop"].([]any)
	var foundUserStop bool
	for _, raw := range stops {
		if raw.(map[string]any)["matcher"] == "user-stop" {
			foundUserStop = true
		}
	}
	if !foundUserStop {
		t.Error("user Stop hook was dropped")
	}
}

// TestWriteHookConfig_Notification_PreservesEmptyMatcherUserHook — the exact
// collision URL-keying exists to prevent: a user's OWN Notification hook that
// also uses an empty matcher (fire on every type) must survive a run-keyed
// write. Under the old matcher-keyed upsert this would have been silently
// overwritten by ours (same "" matcher); keying on the POST URL keeps them
// distinct.
func TestWriteHookConfig_Notification_PreservesEmptyMatcherUserHook(t *testing.T) {
	worktree := t.TempDir()
	claudeDir := filepath.Join(worktree, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	userNotif := map[string]any{
		"matcher": "", // user's catch-all Notification hook
		"hooks": []any{
			map[string]any{"type": "command", "command": "echo user-catch-all"},
		},
	}
	existing := map[string]any{
		"hooks": map[string]any{"Notification": []any{userNotif}},
	}
	data, _ := json.Marshal(existing)
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.local.json"), data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := WriteHookConfig(worktree, "sess-e", "agent-e", "tok-e", 5555); err != nil {
		t.Fatalf("WriteHookConfig: %v", err)
	}

	settings := readSettings(t, worktree)
	notifs := settings["hooks"].(map[string]any)["Notification"].([]any)
	if len(notifs) != 2 {
		t.Fatalf("Notification length = %d, want 2 (user catch-all + bossd)", len(notifs))
	}
	// User's empty-matcher hook untouched.
	var foundUser bool
	for _, raw := range notifs {
		m := raw.(map[string]any)
		if notifCommand(m) == "echo user-catch-all" {
			foundUser = true
		}
	}
	if !foundUser {
		t.Error("user's empty-matcher Notification hook was clobbered")
	}
	// Ours present, keyed by URL.
	findRunNotification(t, settings, "agent-e")
}

// TestWriteHookConfig_Notification_LatestRunWins — a second run-keyed write
// for a different agent_session_id sweeps the prior run's Notification entry
// (runs are sequential per worktree), mirroring the Stop-hook prune.
func TestWriteHookConfig_Notification_LatestRunWins(t *testing.T) {
	worktree := t.TempDir()
	if err := WriteHookConfig(worktree, "sess-n", "agent-N1", "tok-1", 5555); err != nil {
		t.Fatalf("run N1 write: %v", err)
	}
	if err := WriteHookConfig(worktree, "sess-n", "agent-N2", "tok-2", 6666); err != nil {
		t.Fatalf("run N2 write: %v", err)
	}
	settings := readSettings(t, worktree)
	notifs := settings["hooks"].(map[string]any)["Notification"].([]any)
	if len(notifs) != 1 {
		t.Fatalf("Notification length = %d, want 1 (only latest run)", len(notifs))
	}
	findRunNotification(t, settings, "agent-N2")
}

// TestWriteHookConfig_RunKeyed_ReplacesSameNotification — calling the
// run-keyed path twice with the same agent_session_id replaces the
// Notification entry in place rather than duplicating it.
func TestWriteHookConfig_RunKeyed_ReplacesSameNotification(t *testing.T) {
	worktree := t.TempDir()
	const agentSession = "agent-nsame"
	if err := WriteHookConfig(worktree, "sess-ns", agentSession, "tok-old", 7777); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := WriteHookConfig(worktree, "sess-ns", agentSession, "tok-new", 8888); err != nil {
		t.Fatalf("second write: %v", err)
	}
	settings := readSettings(t, worktree)
	notifs := settings["hooks"].(map[string]any)["Notification"].([]any)
	if len(notifs) != 1 {
		t.Fatalf("Notification length = %d, want 1 (no dupe on rewrite)", len(notifs))
	}
	entry := findRunNotification(t, settings, agentSession)
	assertCommandContains(t, entry,
		"Authorization: Bearer tok-new",
		"http://127.0.0.1:8888/hooks/question/"+agentSession,
	)
}

// TestRemoveRunHookConfig_RemovesNotificationToo — RemoveRunHookConfig
// deletes both the Stop and Notification run entries for the agent_session_id,
// leaving the session-keyed finalize entry and user hooks untouched.
func TestRemoveRunHookConfig_RemovesNotificationToo(t *testing.T) {
	worktree := t.TempDir()
	if err := WriteHookConfig(worktree, "sess-1", "", "tok-final", 4000); err != nil {
		t.Fatalf("finalize write: %v", err)
	}
	if err := WriteHookConfig(worktree, "sess-1", "run-both", "tok-1", 4000); err != nil {
		t.Fatalf("run write: %v", err)
	}
	// Sanity: both entries exist before removal.
	pre := readSettings(t, worktree)
	findRunStop(t, pre, "run-both")
	findRunNotification(t, pre, "run-both")

	if err := RemoveRunHookConfig(worktree, "run-both"); err != nil {
		t.Fatalf("RemoveRunHookConfig: %v", err)
	}

	settings := readSettings(t, worktree)
	// Stop run entry gone.
	for _, raw := range settings["hooks"].(map[string]any)["Stop"].([]any) {
		if raw.(map[string]any)["matcher"] == runHookMatcherPrefix+"run-both" {
			t.Fatal("Stop run entry should have been removed")
		}
	}
	// Notification entry gone: no remaining entry POSTs to this run's question
	// URL. Matching on URL (not matcher) is what actually proves removal — the
	// entry carries an empty matcher, so a matcher check would pass vacuously.
	if notifsRaw, ok := settings["hooks"].(map[string]any)["Notification"]; ok {
		for _, raw := range notifsRaw.([]any) {
			if m, ok := raw.(map[string]any); ok && strings.Contains(notifCommand(m), "/hooks/question/run-both") {
				t.Fatal("Notification run entry should have been removed")
			}
		}
	}
	// finalize entry survives.
	findBossdStop(t, settings)
}

// TestWriteHookConfig_RunKeyed_Empty — fresh worktree, no existing
// settings. Run-keyed write installs a "bossd-agent-run-{uuid}" entry
// that POSTs to /hooks/agent-run-complete/{uuid}.
func TestWriteHookConfig_RunKeyed_Empty(t *testing.T) {
	worktree := t.TempDir()
	const agentSession = "agent-run-uuid-1"

	if err := WriteHookConfig(worktree, "sess-r1", agentSession, "tok-run", 45678); err != nil {
		t.Fatalf("WriteHookConfig: %v", err)
	}

	settings := readSettings(t, worktree)
	entry := findRunStop(t, settings, agentSession)
	assertCommandContains(t, entry,
		"Authorization: Bearer tok-run",
		"http://127.0.0.1:45678/hooks/agent-run-complete/"+agentSession,
	)

	// session-keyed entry must NOT have been installed by a run-keyed write.
	stops := settings["hooks"].(map[string]any)["Stop"].([]any)
	for _, raw := range stops {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if m["matcher"] == hookMatcherKey {
			t.Errorf("run-keyed write unexpectedly installed session-keyed %q entry", hookMatcherKey)
		}
	}
}

// TestWriteHookConfig_RunKeyed_CoexistsWithSessionKeyed — file already
// contains a session-keyed entry; a subsequent run-keyed write must add
// a sibling entry without modifying the session-keyed one.
func TestWriteHookConfig_RunKeyed_CoexistsWithSessionKeyed(t *testing.T) {
	worktree := t.TempDir()

	if err := WriteHookConfig(worktree, "sess-coexist", "", "tok-sess", 1111); err != nil {
		t.Fatalf("session-keyed write: %v", err)
	}
	if err := WriteHookConfig(worktree, "sess-coexist", "agent-run-coexist", "tok-run", 2222); err != nil {
		t.Fatalf("run-keyed write: %v", err)
	}

	settings := readSettings(t, worktree)
	stops := settings["hooks"].(map[string]any)["Stop"].([]any)
	if len(stops) != 2 {
		t.Fatalf("Stop array length = %d, want 2 (session + run)", len(stops))
	}

	sessEntry := findBossdStop(t, settings)
	assertCommandContains(t, sessEntry,
		"Authorization: Bearer tok-sess",
		"http://127.0.0.1:1111/hooks/finalize/sess-coexist",
	)

	runEntry := findRunStop(t, settings, "agent-run-coexist")
	assertCommandContains(t, runEntry,
		"Authorization: Bearer tok-run",
		"http://127.0.0.1:2222/hooks/agent-run-complete/agent-run-coexist",
	)
}

// TestWriteHookConfig_SessionKeyed_PreservesRunKeyed — file already
// contains a run-keyed entry; a session-keyed write must insert/update
// its own entry without disturbing the run-keyed one.
func TestWriteHookConfig_SessionKeyed_PreservesRunKeyed(t *testing.T) {
	worktree := t.TempDir()

	if err := WriteHookConfig(worktree, "sess-after-run", "agent-run-first", "tok-run", 3333); err != nil {
		t.Fatalf("run-keyed write: %v", err)
	}
	if err := WriteHookConfig(worktree, "sess-after-run", "", "tok-sess", 4444); err != nil {
		t.Fatalf("session-keyed write: %v", err)
	}

	settings := readSettings(t, worktree)
	stops := settings["hooks"].(map[string]any)["Stop"].([]any)
	if len(stops) != 2 {
		t.Fatalf("Stop array length = %d, want 2 (run + session)", len(stops))
	}

	runEntry := findRunStop(t, settings, "agent-run-first")
	assertCommandContains(t, runEntry,
		"Authorization: Bearer tok-run",
		"http://127.0.0.1:3333/hooks/agent-run-complete/agent-run-first",
	)

	sessEntry := findBossdStop(t, settings)
	assertCommandContains(t, sessEntry,
		"Authorization: Bearer tok-sess",
		"http://127.0.0.1:4444/hooks/finalize/sess-after-run",
	)
}

// TestWriteHookConfig_RunKeyed_LatestRunWins — a second run-keyed write
// for a different agent_session_id sweeps the first. Runs are sequential
// per worktree; the previous run's entry is stale and must not accumulate.
func TestWriteHookConfig_RunKeyed_LatestRunWins(t *testing.T) {
	worktree := t.TempDir()

	if err := WriteHookConfig(worktree, "sess-multi", "agent-A", "tok-A", 5555); err != nil {
		t.Fatalf("run A write: %v", err)
	}
	if err := WriteHookConfig(worktree, "sess-multi", "agent-B", "tok-B", 6666); err != nil {
		t.Fatalf("run B write: %v", err)
	}

	settings := readSettings(t, worktree)
	stops := settings["hooks"].(map[string]any)["Stop"].([]any)
	// Only agent-B survives; agent-A was pruned as a stale sibling.
	if len(stops) != 1 {
		t.Fatalf("Stop array length = %d, want 1 (only latest run)", len(stops))
	}

	// The single surviving entry is agent-B; the len==1 check above already
	// proves the stale agent-A sibling was swept.
	entryB := findRunStop(t, settings, "agent-B")
	assertCommandContains(t, entryB,
		"Authorization: Bearer tok-B",
		"http://127.0.0.1:6666/hooks/agent-run-complete/agent-B",
	)
}

// TestWriteHookConfig_RunWritePrunesStaleRunHooks — a run-keyed write must
// sweep any pre-existing bossd-agent-run-* entries while keeping the
// session-keyed finalize entry and any user hooks untouched.
func TestWriteHookConfig_RunWritePrunesStaleRunHooks(t *testing.T) {
	worktree := t.TempDir()
	// Pre-existing: a session-keyed finalize entry and an OLD run entry.
	if err := WriteHookConfig(worktree, "sess-1", "", "tok-final", 4000); err != nil {
		t.Fatalf("finalize write: %v", err)
	}
	if err := WriteHookConfig(worktree, "sess-1", "run-old", "tok-old", 4000); err != nil {
		t.Fatalf("old run write: %v", err)
	}
	// Writing a NEW run entry must prune the old run entry but keep finalize.
	if err := WriteHookConfig(worktree, "sess-1", "run-new", "tok-new", 4000); err != nil {
		t.Fatalf("new run write: %v", err)
	}

	settings := readSettings(t, worktree)
	stop := settings["hooks"].(map[string]any)["Stop"].([]any)
	got := map[string]bool{}
	for _, raw := range stop {
		got[raw.(map[string]any)["matcher"].(string)] = true
	}
	want := map[string]bool{"bossd-finalize": true, runHookMatcherPrefix + "run-new": true}
	if len(got) != len(want) {
		t.Fatalf("matchers = %v, want exactly %v", got, want)
	}
	for m := range want {
		if !got[m] {
			t.Errorf("missing matcher %q", m)
		}
	}
	if got[runHookMatcherPrefix+"run-old"] {
		t.Error("stale run entry run-old was not pruned")
	}
}

func TestRemoveRunHookConfig_RemovesOnlyTargetEntry(t *testing.T) {
	worktree := t.TempDir()
	if err := WriteHookConfig(worktree, "sess-1", "", "tok-final", 4000); err != nil {
		t.Fatalf("finalize write: %v", err)
	}
	if err := WriteHookConfig(worktree, "sess-1", "run-1", "tok-1", 4000); err != nil {
		t.Fatalf("run write: %v", err)
	}

	if err := RemoveRunHookConfig(worktree, "run-1"); err != nil {
		t.Fatalf("RemoveRunHookConfig: %v", err)
	}

	settings := readSettings(t, worktree)
	stop := settings["hooks"].(map[string]any)["Stop"].([]any)
	for _, raw := range stop {
		if raw.(map[string]any)["matcher"] == runHookMatcherPrefix+"run-1" {
			t.Fatal("run-1 entry should have been removed")
		}
	}
	// finalize entry survives
	var foundFinalize bool
	for _, raw := range stop {
		if raw.(map[string]any)["matcher"] == "bossd-finalize" {
			foundFinalize = true
		}
	}
	if !foundFinalize {
		t.Error("bossd-finalize entry must be preserved")
	}
}

func TestRemoveRunHookConfig_MissingFileIsNoOp(t *testing.T) {
	if err := RemoveRunHookConfig(t.TempDir(), "run-x"); err != nil {
		t.Fatalf("missing file should be a no-op, got %v", err)
	}
}

// TestWriteHookConfig_RunKeyed_ReplacesSameRun — calling the run-keyed
// path twice with the same agent_session_id must replace the entry in
// place rather than duplicating it.
func TestWriteHookConfig_RunKeyed_ReplacesSameRun(t *testing.T) {
	worktree := t.TempDir()
	const agentSession = "agent-same"

	if err := WriteHookConfig(worktree, "sess-rsame", agentSession, "tok-old", 7777); err != nil {
		t.Fatalf("first run-keyed write: %v", err)
	}
	if err := WriteHookConfig(worktree, "sess-rsame", agentSession, "tok-new", 8888); err != nil {
		t.Fatalf("second run-keyed write: %v", err)
	}

	settings := readSettings(t, worktree)
	stops := settings["hooks"].(map[string]any)["Stop"].([]any)
	if len(stops) != 1 {
		t.Fatalf("Stop array length = %d, want 1 (no dupe on rewrite)", len(stops))
	}
	entry := findRunStop(t, settings, agentSession)
	assertCommandContains(t, entry,
		"Authorization: Bearer tok-new",
		"http://127.0.0.1:8888/hooks/agent-run-complete/"+agentSession,
	)
}
