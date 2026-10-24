// Package realstack boots isolated real bossd processes for end-to-end consumers.
package realstack

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// BaseEnv removes ambient credentials and process routing from a child environment.
func BaseEnv(base []string) []string {
	env := make([]string, 0, len(base))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if managedEnvKey(key) {
			continue
		}
		env = append(env, entry)
	}
	return env
}

func managedEnvKey(key string) bool {
	for _, prefix := range []string{"BOSS", "ANTHROPIC_", "CLAUDE_", "OPENAI_", "CODEX_", "GH_", "GITHUB_", "XDG_", "GIT_CONFIG"} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return key == "HOME" || key == "PROOF_ANTHROPIC_API_KEY"
}

func settings(home, socket, stub string, port int) map[string]any {
	return map[string]any{
		"worktree_base_dir": filepath.Join(home, "worktrees"), "app_data_dir": filepath.Join(home, "data"), "socket_path": socket,
		"providers_acknowledged": true,
		"plugins":                []map[string]any{{"name": "stub", "path": stub, "enabled": true}},
		"managed_accounts":       map[string]any{"failover_proxy_port": port},
	}
}

func writeSettings(path string, cfg map[string]any) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

type tmuxSession struct{ name, path string }

func sessionsToSweep(current []tmuxSession, before map[string]bool, roots []string) []string {
	var result []string
	for _, session := range current {
		if !strings.HasPrefix(session.name, "boss-") || before[session.name] {
			continue
		}
		for _, root := range roots {
			if session.path == root || strings.HasPrefix(session.path, filepath.Clean(root)+string(filepath.Separator)) {
				result = append(result, session.name)
				break
			}
		}
	}
	return result
}
