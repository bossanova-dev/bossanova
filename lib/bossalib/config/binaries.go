package config

import (
	"os"
	"os/exec"
	"path/filepath"
)

// ResolveTrustedExecutable returns the absolute path to a trusted executable
// named name (e.g. "boss", "mcp"), or "" if none is found at a trusted path.
//
// It searches only the directory of the running executable (bossd) and the
// process PATH — never the current working directory or a worktree — and
// validates each candidate with isTrustedPath (owned by the current user or
// root, not group/world-writable). This prevents a binary planted in an
// untrusted repo worktree from being selected and later exec'd by an agent.
func ResolveTrustedExecutable(name string) string {
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), name))
	}
	if p, err := exec.LookPath(name); err == nil {
		candidates = append(candidates, p)
	}
	for _, c := range candidates {
		if abs, err := filepath.Abs(c); err == nil {
			c = abs
		}
		if ok, _ := isTrustedPath(c); ok {
			return c
		}
	}
	return ""
}
