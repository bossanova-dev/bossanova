package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const codexTrustedProjectLevel = "trusted"

func trustCodexWorktree(worktreePath string) error {
	if strings.TrimSpace(worktreePath) == "" {
		return nil
	}

	absPath, err := filepath.Abs(worktreePath)
	if err != nil {
		return fmt.Errorf("resolve worktree path: %w", err)
	}

	configDir, err := codexConfigDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("create codex config dir: %w", err)
	}

	lockFile, err := lockCodexConfig(configDir)
	if err != nil {
		return err
	}
	defer unlockCodexConfig(lockFile)

	configPath := filepath.Join(configDir, "config.toml")
	var mode os.FileMode = 0o600
	current, err := os.ReadFile(configPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("read codex config: %w", err)
		}
	} else if info, statErr := os.Stat(configPath); statErr == nil {
		mode = info.Mode().Perm()
	}

	next := setCodexProjectTrust(string(current), absPath)
	if next == string(current) {
		return nil
	}
	if err := os.WriteFile(configPath, []byte(next), mode); err != nil {
		return fmt.Errorf("write codex config: %w", err)
	}
	return nil
}

func lockCodexConfig(configDir string) (*os.File, error) {
	lockPath := filepath.Join(configDir, ".config.toml.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open codex config lock: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("lock codex config: %w", err)
	}
	return lockFile, nil
}

func unlockCodexConfig(lockFile *os.File) {
	_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	_ = lockFile.Close()
}

func codexConfigDir() (string, error) {
	if codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME")); codexHome != "" {
		return codexHome, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}

func setCodexProjectTrust(config, worktreePath string) string {
	header := `[projects."` + tomlBasicStringEscape(worktreePath) + `"]`
	trustLine := `trust_level = "` + codexTrustedProjectLevel + `"`
	lines := strings.Split(config, "\n")

	for i, line := range lines {
		if strings.TrimSpace(line) != header {
			continue
		}

		sectionEnd := len(lines)
		for j := i + 1; j < len(lines); j++ {
			trimmed := strings.TrimSpace(lines[j])
			if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
				sectionEnd = j
				break
			}
		}

		for j := i + 1; j < sectionEnd; j++ {
			if strings.HasPrefix(strings.TrimSpace(lines[j]), "trust_level") {
				indent := lines[j][:len(lines[j])-len(strings.TrimLeft(lines[j], " \t"))]
				lines[j] = indent + trustLine
				return strings.Join(lines, "\n")
			}
		}

		lines = append(lines[:i+1], append([]string{trustLine}, lines[i+1:]...)...)
		return strings.Join(lines, "\n")
	}

	if strings.TrimSpace(config) == "" {
		return header + "\n" + trustLine + "\n"
	}
	if !strings.HasSuffix(config, "\n") {
		config += "\n"
	}
	return config + "\n" + header + "\n" + trustLine + "\n"
}

func tomlBasicStringEscape(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 {
				_, _ = fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}
