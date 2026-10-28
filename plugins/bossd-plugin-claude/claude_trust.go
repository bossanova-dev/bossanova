package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

var claudeTrustMu sync.Mutex

// applyClaudeProjectTrust accepts folder trust only for an explicit repo grant.
// Claude Code stores this in projects.<absolute path>.hasTrustDialogAccepted.
// Linked worktrees also inherit trust from their main checkout. Both lexical
// and resolved paths are recorded because macOS renders /var as /private/var.
// Existing homes only are edited: accepting trust must never bypass onboarding.
// Concurrent Claude processes can still overwrite a later save; a missed grant
// is diagnosed by the folder-trust pane recognizer rather than answered by keys.
func (s *Server) applyClaudeProjectTrust(workDir string, configHomeEnv map[string]string, decision bossanovav1.ProjectTrustDecision) {
	if decision != bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT || strings.TrimSpace(workDir) == "" {
		return
	}
	claudeTrustMu.Lock()
	defer claudeTrustMu.Unlock()
	paths, err := claudeProjectTrustPaths(workDir)
	if err != nil {
		s.logger.Warn().Err(err).Msg("could not resolve Claude folder trust")
		return
	}
	ownHome, err := os.UserHomeDir()
	if err != nil {
		s.logger.Warn().Err(err).Msg("could not resolve Claude home")
		return
	}
	candidates := []string{filepath.Join(ownHome, ".claude.json")}
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		candidates = append(candidates, filepath.Join(dir, ".claude.json"))
	}
	if home := configHomeEnv["HOME"]; home != "" {
		if !filepath.IsAbs(home) {
			home = filepath.Join(workDir, home)
		}
		candidates = append(candidates, filepath.Join(home, ".claude.json"))
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		if err := acceptClaudeFolderTrust(candidate, paths); err != nil {
			s.logger.Warn().Err(err).Str("config_path", candidate).Msg("could not accept Claude folder trust; launch will continue")
		}
	}
}

func claudeProjectTrustPaths(workDir string) ([]string, error) {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return nil, err
	}
	paths := []string{abs}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil && resolved != abs {
		paths = append(paths, resolved)
	}
	gitFile, err := os.ReadFile(filepath.Join(abs, ".git"))
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, os.ErrInvalid) {
			return paths, nil
		}
		info, statErr := os.Stat(filepath.Join(abs, ".git"))
		if statErr == nil && info.IsDir() {
			return paths, nil
		}
		return nil, err
	}
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(string(gitFile)), "gitdir:")
	if !ok {
		return paths, nil
	}
	gitDir = strings.TrimSpace(gitDir)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(abs, gitDir)
	}
	common, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if os.IsNotExist(err) {
		return paths, nil
	}
	if err != nil {
		return nil, err
	}
	commonDir := strings.TrimSpace(string(common))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(gitDir, commonDir)
	}
	commonDir = filepath.Clean(commonDir)
	if filepath.Base(commonDir) != ".git" {
		return paths, nil
	}
	root := filepath.Dir(commonDir)
	paths = append(paths, root)
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != root {
		paths = append(paths, resolved)
	}
	return paths, nil
}

func acceptClaudeFolderTrust(path string, keys []string) error {
	for attempt := 0; attempt < 3; attempt++ {
		current, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		next, changed, err := mergeClaudeFolderTrust(current, keys)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if err := replaceClaudeTrustFile(path, next, info.Mode().Perm()); err != nil {
			return err
		}
		saved, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, missing, err := mergeClaudeFolderTrust(saved, keys)
		if err != nil {
			return err
		}
		if !missing {
			return nil
		}
	}
	return fmt.Errorf("claude folder trust was overwritten after three attempts")
}

func replaceClaudeTrustFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".claude-trust-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func trustJSONObject(raw []byte) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, fmt.Errorf("expected JSON object")
	}
	return object, nil
}

func mergeClaudeFolderTrust(raw []byte, keys []string) ([]byte, bool, error) {
	top, err := trustJSONObject(raw)
	if err != nil {
		return nil, false, err
	}
	projects := map[string]json.RawMessage{}
	if existing, ok := top["projects"]; ok {
		projects, err = trustJSONObject(existing)
		if err != nil {
			return nil, false, err
		}
	}
	changed := false
	for _, key := range keys {
		project := map[string]json.RawMessage{}
		if existing, ok := projects[key]; ok {
			project, err = trustJSONObject(existing)
			if err != nil {
				return nil, false, err
			}
		}
		if bytes.Equal(bytes.TrimSpace(project["hasTrustDialogAccepted"]), []byte("true")) {
			continue
		}
		project["hasTrustDialogAccepted"] = json.RawMessage("true")
		projects[key] = marshalTrustObject(project)
		changed = true
	}
	if !changed {
		return raw, false, nil
	}
	top["projects"] = marshalTrustObject(projects)
	return append(marshalTrustObject(top), '\n'), true, nil
}

// json.Marshal compacts RawMessage values. Writing the object envelope ourselves
// preserves every untouched value's exact bytes, including numbers and spaces.
func marshalTrustObject(object map[string]json.RawMessage) []byte {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out bytes.Buffer
	out.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			out.WriteByte(',')
		}
		name, _ := json.Marshal(key)
		out.Write(name)
		out.WriteByte(':')
		out.Write(object[key])
	}
	out.WriteByte('}')
	return out.Bytes()
}
