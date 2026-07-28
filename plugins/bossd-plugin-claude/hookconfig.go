package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// hookMatcherKey is the matcher string we stamp on the session-keyed
// Stop-hook group so WriteHookConfig can identify and replace its own
// entry on re-runs without clobbering any Stop hooks a repo's setup
// script may have written first.
const hookMatcherKey = "bossd-finalize"

// runHookMatcherPrefix is the matcher prefix used for run-scoped Stop-hook
// groups. Each run gets its own unique matcher
// ("bossd-agent-run-{agentSessionID}"). A run-keyed write sweeps any
// pre-existing bossd-agent-run-* siblings so the file stays capped at one
// run entry; the session-keyed bossd-finalize entry is left untouched.
const runHookMatcherPrefix = "bossd-agent-run-"

// questionHookURLPrefix is the unique substring of the BOS-485 Notification
// (question) hook's POST URL. bossd keys the Notification array off this — NOT
// the Claude matcher — because that matcher is a real notification-type filter
// (an empty matcher fires on every type), a value the hook necessarily shares
// with any sibling chat's question hook or a user-authored Notification hook.
// The URL carries the agent_session_id, so "/hooks/question/{id}" uniquely
// identifies one run's entry, and the bare prefix identifies "any bossd
// question hook" for the stale-sibling sweep.
const questionHookURLPrefix = "/hooks/question/"

// WriteHookConfig writes (or merges) a Stop-hook entry into
// worktreePath/.claude/settings.local.json. The entry POSTs to the
// bossd loopback hook server with a Bearer token so FinalizeSession (or,
// for run-scoped entries, the per-run completion handler) runs when
// Claude finishes producing output in the worktree.
//
// Note: Claude's Stop hook fires at the end of EVERY main-agent turn, including
// mid-run pauses (e.g. awaiting a background subagent), so the POST is a per-turn
// hint, not a completion signal. The cron path's run-completion gating lives on
// the bossd side in CronCompletionGate (cronRunIsOver); a Stop alone never
// finalizes a still-working cron session.
//
// When agentSessionID == "" (legacy / cron path):
//   - Matcher is "bossd-finalize".
//   - URL is /hooks/finalize/{sessionID}.
//
// When agentSessionID != "" (run-scoped path, e.g. repair chat runs):
//   - Matcher is "bossd-agent-run-{agentSessionID}" — unique per run.
//   - URL is /hooks/agent-run-complete/{agentSessionID}.
//   - Coexists with any existing session-keyed entry; both can fire.
//
// Merge semantics:
//   - Missing file → a new file is created.
//   - Empty file  → treated as "{}".
//   - Top-level JSON must be an object; non-object JSON is an error.
//   - All existing keys are preserved. "hooks" and "hooks.Stop" are
//     created only when absent.
//   - Inside Stop[], the first entry whose matcher matches the one we're
//     installing is replaced in place. On a run-keyed write, all other
//     bossd-agent-run-* entries are pruned first (stale leak recovery).
//     The session-keyed bossd-finalize entry and any user/repo Stop hooks
//     are always left untouched.
//
// Writes are atomic: JSON is serialised to a sibling temp file inside
// the same .claude directory and renamed over the target, so a crash
// mid-write can't leave a half-written file visible to Claude.
func WriteHookConfig(worktreePath, sessionID, agentSessionID, token string, port int) error {
	if worktreePath == "" {
		return errors.New("worktreePath is required")
	}
	if sessionID == "" {
		return errors.New("sessionID is required")
	}
	if token == "" {
		return errors.New("token is required")
	}
	if port <= 0 {
		return fmt.Errorf("port must be positive, got %d", port)
	}

	claudeDir := filepath.Join(worktreePath, ".claude")
	// 0o700: this directory holds settings.local.json, which carries the
	// bossd hook Bearer token (written 0o600). Keep the parent dir owner-only
	// too (G301) rather than the world-readable 0o755.
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		return fmt.Errorf("create .claude dir: %w", err)
	}
	target := filepath.Join(claudeDir, "settings.local.json")

	root, err := loadHookConfig(target)
	if err != nil {
		return err
	}

	hooks := asMap(root, "hooks")
	stops := asSlice(hooks, "Stop")

	// Run-keyed writes sweep stale sibling run entries first so the file can't
	// grow one bossd-agent-run-* entry per run forever. The legacy/finalize
	// path (agentSessionID == "") upserts in place and may legitimately coexist
	// with an active run entry, so it does not sweep.
	if agentSessionID != "" {
		stops = pruneRunHooks(stops)
	}

	matcher, urlPath := hookEntryShape(sessionID, agentSessionID)
	entry := bossdStopEntry(matcher, urlPath, token, port)

	stops = upsertByMatcher(stops, matcher, entry)
	hooks["Stop"] = stops

	// Run-scoped writes also install a Claude Code Notification hook (BOS-485)
	// so a "needs the human" event drives CHAT_STATUS_QUESTION via an explicit
	// signal instead of the pane regex. It POSTs the SAME per-run token to
	// /hooks/question/{agentSessionID}, forwarding the notification payload on
	// stdin so bossd can classify by notification_type. The session-keyed
	// finalize path (agentSessionID == "") has no agent_session_id to key or
	// POST, so it installs no Notification hook.
	//
	// Unlike Stop, Claude's Notification event FILTERS its matcher by
	// notification TYPE (permission_prompt, idle_prompt, …); a synthetic per-run
	// identifier there matches no type, so the hook would never fire (the
	// original BOS-485 inert bug). We therefore install an EMPTY matcher — fire
	// on every type — and let bossd decide which types mean "needs the human".
	// Because the matcher is no longer a unique per-run key, bossd owns this
	// entry by the unique substring of its POST URL (/hooks/question/{id})
	// rather than by matcher — see questionHookURLPrefix. pruneQuestionHooks
	// also sweeps the pre-fix inert entry (same URL, old matcher), so an upgrade
	// heals itself.
	if agentSessionID != "" {
		notifURL := questionHookURLPrefix + agentSessionID
		notifs := pruneQuestionHooks(asSlice(hooks, "Notification"))
		notifEntry := bossdNotificationEntry(notifURL, token, port)
		notifs = upsertByCommandURL(notifs, notifURL, notifEntry)
		hooks["Notification"] = notifs
	}

	root["hooks"] = hooks

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal hook config: %w", err)
	}
	// Trailing newline is a user-friendliness convention for JSON files
	// a human might open; keep the file hygienic on disk.
	out = append(out, '\n')

	return atomicWrite(claudeDir, target, out)
}

// hookEntryShape returns (matcher, urlPath) for the Stop-hook entry
// WriteHookConfig should install. agentSessionID == "" picks the legacy
// session-keyed shape; otherwise the run-keyed shape is used.
func hookEntryShape(sessionID, agentSessionID string) (string, string) {
	if agentSessionID != "" {
		return runHookMatcherPrefix + agentSessionID,
			"/hooks/agent-run-complete/" + agentSessionID
	}
	return hookMatcherKey, "/hooks/finalize/" + sessionID
}

// upsertByMatcher replaces the first entry in stops whose matcher equals
// the supplied key, or appends entry if no match exists. Other entries
// (including run-keyed siblings or unrelated user hooks) are left
// untouched. Returns a fresh slice — never aliases or mutates the input
// backing array (matching pruneRunHooks).
func upsertByMatcher(stops []any, matcher string, entry map[string]any) []any {
	out := make([]any, len(stops), len(stops)+1)
	copy(out, stops)
	for i, raw := range out {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		existing, _ := m["matcher"].(string)
		if existing == matcher {
			out[i] = entry
			return out
		}
	}
	return append(out, entry)
}

// pruneRunHooks drops every Stop entry whose matcher carries the run-scoped
// prefix (runHookMatcherPrefix). The session-keyed bossd-finalize entry and
// any user/repo Stop hooks are preserved. Returns a fresh slice — never
// aliases or mutates the input backing array.
//
// Safe because runs are sequential per worktree (see Global Constraints): any
// pre-existing run entry belongs to a finished run whose RemoveAgentRunHook
// cleanup was missed (e.g. a daemon crash), so removing it can't orphan a live
// run's completion ping.
func pruneRunHooks(stops []any) []any {
	kept := make([]any, 0, len(stops))
	for _, raw := range stops {
		if m, ok := raw.(map[string]any); ok {
			if matcher, _ := m["matcher"].(string); strings.HasPrefix(matcher, runHookMatcherPrefix) {
				continue
			}
		}
		kept = append(kept, raw)
	}
	return kept
}

// entryCommandContains reports whether raw is a Claude hook group whose inner
// {type,command} entries include a command containing substr. This is how the
// Notification array identifies bossd's own question hook — by its POST URL
// rather than by matcher, since the matcher is now a shared type filter.
func entryCommandContains(raw any, substr string) bool {
	m, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	inner, ok := m["hooks"].([]any)
	if !ok {
		return false
	}
	for _, h := range inner {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if cmd, _ := hm["command"].(string); strings.Contains(cmd, substr) {
			return true
		}
	}
	return false
}

// pruneQuestionHooks drops every Notification entry that is one of bossd's
// question hooks (its command POSTs to questionHookURLPrefix), regardless of
// agent_session_id. User/repo Notification hooks are preserved. Returns a fresh
// slice — never aliases or mutates the input backing array. The URL-based
// analogue of pruneRunHooks, and safe for the same reason: runs are sequential
// per worktree, so a pre-existing question hook belongs to a finished run whose
// cleanup was missed. It also sweeps the pre-fix inert entry (same URL, old
// per-run matcher), so an upgrade heals itself.
func pruneQuestionHooks(notifs []any) []any {
	kept := make([]any, 0, len(notifs))
	for _, raw := range notifs {
		if entryCommandContains(raw, questionHookURLPrefix) {
			continue
		}
		kept = append(kept, raw)
	}
	return kept
}

// upsertByCommandURL replaces the first Notification entry whose command
// contains urlPath, or appends entry when none match. Ownership key for the
// Notification array (see questionHookURLPrefix) — the URL-based analogue of
// upsertByMatcher. Returns a fresh slice — never aliases or mutates the input.
func upsertByCommandURL(notifs []any, urlPath string, entry map[string]any) []any {
	out := make([]any, len(notifs), len(notifs)+1)
	copy(out, notifs)
	for i, raw := range out {
		if entryCommandContains(raw, urlPath) {
			out[i] = entry
			return out
		}
	}
	return append(out, entry)
}

// removeCommandURLFromHookArray drops the entry from hooks[key] whose command
// contains urlPath. URL-based analogue of removeMatcherFromHookArray for the
// Notification array. Returns true when an entry was removed and the slice
// rewritten back into hooks. A missing key or wrong-typed value is a no-op
// returning false.
func removeCommandURLFromHookArray(hooks map[string]any, key, urlPath string) bool {
	arr, ok := hooks[key].([]any)
	if !ok {
		return false
	}
	kept := make([]any, 0, len(arr))
	for _, raw := range arr {
		if entryCommandContains(raw, urlPath) {
			continue
		}
		kept = append(kept, raw)
	}
	if len(kept) == len(arr) {
		return false
	}
	hooks[key] = kept
	return true
}

// RemoveRunHookConfig deletes the run-scoped Stop-hook entry
// (matcher runHookMatcherPrefix+agentSessionID) from
// worktreePath/.claude/settings.local.json. Inverse of WriteHookConfig's
// run-keyed path; the daemon calls it when a run completes so a finished
// run's completion ping can't linger. Missing file, missing hooks/Stop, or
// an already-absent entry are all no-ops (no error, no rewrite). The
// bossd-finalize entry and any user hooks are left untouched. Writes are
// atomic, mirroring WriteHookConfig.
func RemoveRunHookConfig(worktreePath, agentSessionID string) error {
	if worktreePath == "" {
		return errors.New("worktreePath is required")
	}
	if agentSessionID == "" {
		return errors.New("agentSessionID is required")
	}
	claudeDir := filepath.Join(worktreePath, ".claude")
	target := filepath.Join(claudeDir, "settings.local.json")

	root, err := loadHookConfig(target)
	if err != nil {
		return err
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return nil
	}

	// Remove the run-keyed entry from BOTH the Stop and Notification arrays
	// (BOS-485 added the Notification run hook). Each removal is independent —
	// a file that only ever had a Stop entry (pre-BOS-485, or a session-keyed
	// write) still cleans up correctly. Only rewrite when something changed.
	//
	// The Stop entry is keyed by its unique per-run matcher, but the Notification
	// entry uses an empty (type-filter) matcher, so it must be removed by the
	// unique substring of its POST URL instead — the same ownership key
	// WriteHookConfig upserts on (see questionHookURLPrefix). Matching the full
	// per-run URL removes only THIS run's entry, never a sibling's.
	matcher := runHookMatcherPrefix + agentSessionID
	changed := false
	changed = removeMatcherFromHookArray(hooks, "Stop", matcher) || changed
	changed = removeCommandURLFromHookArray(hooks, "Notification", questionHookURLPrefix+agentSessionID) || changed
	if !changed {
		return nil // nothing removed — don't rewrite the file
	}

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal hook config: %w", err)
	}
	out = append(out, '\n')
	return atomicWrite(claudeDir, target, out)
}

// removeMatcherFromHookArray drops the entry with the given matcher from
// hooks[key] (a []any of Claude hook groups). Returns true when an entry was
// removed and the slice rewritten back into hooks. A missing key or
// wrong-typed value is a no-op returning false. Other entries (user hooks,
// the session-keyed finalize entry) are preserved.
func removeMatcherFromHookArray(hooks map[string]any, key, matcher string) bool {
	arr, ok := hooks[key].([]any)
	if !ok {
		return false
	}
	kept := make([]any, 0, len(arr))
	for _, raw := range arr {
		if m, ok := raw.(map[string]any); ok {
			if existing, _ := m["matcher"].(string); existing == matcher {
				continue
			}
		}
		kept = append(kept, raw)
	}
	if len(kept) == len(arr) {
		return false
	}
	hooks[key] = kept
	return true
}

// loadHookConfig reads and parses the existing settings.local.json.
// A missing file or empty file both return an empty map so callers can
// start from a clean slate; any other read or parse error is surfaced
// so we don't silently clobber a malformed config.
func loadHookConfig(path string) (map[string]any, error) {
	// Clean last before the read: path is an internally-derived
	// worktree/.claude/settings.local.json location, not caller input (G304).
	cleaned := filepath.Clean(path)
	data, err := os.ReadFile(cleaned)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return map[string]any{}, nil
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, nil
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if root == nil {
		return nil, fmt.Errorf("parse %s: top-level JSON must be an object", path)
	}
	return root, nil
}

// atomicWrite writes data to target via a temp file in the same
// directory, then renames over the target. The temp file is removed on
// any error so we never leave orphans next to the settings file.
// Permissions are 0o600 because the file contains the hook_token.
func atomicWrite(dir, target string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".settings.local.json.tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		cleanup()
		return fmt.Errorf("rename to %s: %w", target, err)
	}
	return nil
}

// bossdStopEntry returns the Stop-hook group we insert into Stop[].
// Shape follows Claude Code's hook schema: a group with a matcher key
// (we use it as an identifier, not a pattern) and an inner hooks array
// of {type, command} pairs.
//
// The curl flags are deliberate:
//   - -s  silent — suppress progress noise in the Claude transcript.
//   - -f  fail on HTTP 4xx/5xx so a rotated token shows up as a hook
//     error instead of silently completing.
//   - --max-time 5 — the server dispatches the completion handler
//     asynchronously and returns 200 in milliseconds, so a real
//     response will never approach this ceiling; the cap exists to
//     keep a wedged daemon from blocking the Stop hook forever.
func bossdStopEntry(matcher, urlPath, token string, port int) map[string]any {
	cmd := fmt.Sprintf(
		`curl -sf --max-time 5 -X POST -H "Authorization: Bearer %s" http://127.0.0.1:%d%s`,
		token, port, urlPath,
	)
	return map[string]any{
		"matcher": matcher,
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": cmd,
			},
		},
	}
}

// bossdNotificationEntry returns the Notification-hook group for BOS-485. It
// mirrors bossdStopEntry's curl flags but differs in two ways that the
// Notification event demands:
//
//   - EMPTY matcher. Claude's Notification event filters its matcher by
//     notification TYPE, so a synthetic per-run identifier would match nothing
//     and the hook would never fire. An empty matcher fires on every type;
//     bossd classifies server-side which types mean "needs the human".
//   - `--data-binary @-` + Content-Type: application/json. The notification
//     payload arrives on the hook's stdin; forwarding it verbatim lets bossd
//     read notification_type and ignore benign types (auth_success, idle_prompt).
func bossdNotificationEntry(urlPath, token string, port int) map[string]any {
	cmd := fmt.Sprintf(
		`curl -sf --max-time 5 -X POST -H "Authorization: Bearer %s" -H "Content-Type: application/json" --data-binary @- http://127.0.0.1:%d%s`,
		token, port, urlPath,
	)
	return map[string]any{
		"matcher": "",
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": cmd,
			},
		},
	}
}

// asMap returns root[key] coerced to a map. If the key is absent or the
// existing value is not a JSON object, a fresh map is installed at
// root[key] and returned — so the caller can safely mutate it and know
// root will reflect the changes.
func asMap(root map[string]any, key string) map[string]any {
	if existing, ok := root[key].(map[string]any); ok {
		return existing
	}
	m := map[string]any{}
	root[key] = m
	return m
}

// asSlice returns root[key] coerced to a []any. Returns an empty slice
// if absent or of the wrong type. The caller is responsible for writing
// the potentially-grown slice back to root.
func asSlice(root map[string]any, key string) []any {
	if existing, ok := root[key].([]any); ok {
		return existing
	}
	return []any{}
}
