package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// hookMatcherKey is the matcher string we stamp on our Stop-hook group so
// WriteHookConfig can identify and replace its own entry on re-runs
// without clobbering any Stop hooks a repo's setup script may have
// written first.
const hookMatcherKey = "bossd-finalize"

// WriteHookConfig writes (or merges) a Stop-hook entry into
// worktreePath/.claude/settings.local.json. The entry POSTs to the
// bossd loopback hook server with a Bearer token so FinalizeSession
// runs when Claude finishes producing output in the worktree.
//
// Merge semantics:
//   - Missing file → a new file is created.
//   - Empty file  → treated as "{}".
//   - Top-level JSON must be an object; non-object JSON is an error.
//   - All existing keys are preserved. "hooks" and "hooks.Stop" are
//     created only when absent.
//   - Inside Stop[], the first entry whose matcher == "bossd-finalize"
//     is replaced in place. Any other Stop hooks (including any the
//     repo setup script added) are left untouched.
//
// Writes are atomic: JSON is serialised to a sibling temp file inside
// the same .claude directory and renamed over the target, so a crash
// mid-write can't leave a half-written file visible to Claude.
func WriteHookConfig(worktreePath, sessionID, token string, port int) error {
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
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		return fmt.Errorf("create .claude dir: %w", err)
	}
	target := filepath.Join(claudeDir, "settings.local.json")

	root, err := loadHookConfig(target)
	if err != nil {
		return err
	}

	hooks := asMap(root, "hooks")
	stops := asSlice(hooks, "Stop")

	entry := bossdStopEntry(sessionID, token, port)

	replaced := false
	for i, raw := range stops {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		matcher, _ := m["matcher"].(string)
		if matcher == hookMatcherKey {
			stops[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		stops = append(stops, entry)
	}
	hooks["Stop"] = stops
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

// loadHookConfig reads and parses the existing settings.local.json.
// A missing file or empty file both return an empty map so callers can
// start from a clean slate; any other read or parse error is surfaced
// so we don't silently clobber a malformed config.
func loadHookConfig(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
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
//   - --max-time 5 — the server dispatches FinalizeSession
//     asynchronously and returns 200 in milliseconds, so a real
//     response will never approach this ceiling; the cap exists to
//     keep a wedged daemon from blocking the Stop hook forever.
func bossdStopEntry(sessionID, token string, port int) map[string]any {
	cmd := fmt.Sprintf(
		`curl -sf --max-time 5 -X POST -H "Authorization: Bearer %s" http://127.0.0.1:%d/hooks/finalize/%s`,
		token, port, sessionID,
	)
	return map[string]any{
		"matcher": hookMatcherKey,
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
