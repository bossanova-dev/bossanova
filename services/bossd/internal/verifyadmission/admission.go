// Package verifyadmission reads the verify gate's persisted admission facts.
package verifyadmission

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Snapshot is a versioned admission scan across the repository's verify scopes.
// Its zero value has no known exclusions.
type Snapshot struct {
	Version int              `json:"version"`
	Scopes  map[string]scope `json:"scopes"`
}

type scope struct {
	PRs map[string]entry `json:"prs"`
}

type entry struct {
	Head   string `json:"head"`
	State  string `json:"state"`
	Reason string `json:"reason"`
}

// Read returns an empty snapshot on missing, unreadable, or unsupported files.
func Read(path string) (Snapshot, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Snapshot{}, nil
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("read verify admission: %w", err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("parse verify admission: %w", err)
	}
	if snapshot.Version != 1 {
		return Snapshot{}, fmt.Errorf("unsupported verify admission version %d", snapshot.Version)
	}
	return snapshot, nil
}

// Excluded reports a head excluded by every scope that has an answer for it.
// Missing heads and any non-excluded answer preserve the verify hold.
func (s Snapshot) Excluded(prNumber int, headSHA string) (reason string, ok bool) {
	if s.Version != 1 || prNumber <= 0 || headSHA == "" {
		return "", false
	}
	key := strconv.Itoa(prNumber)
	for _, scope := range s.Scopes {
		e, exists := scope.PRs[key]
		if !exists || !strings.EqualFold(e.Head, headSHA) {
			continue
		}
		if e.State != "excluded" {
			return "", false
		}
		// A stable reason makes the answer independent of map iteration order.
		if !ok || e.Reason < reason {
			reason = e.Reason
		}
		ok = true
	}
	return reason, ok
}

// Path resolves the admission file shared by all worktrees of repoPath.
func Path(ctx context.Context, repoPath string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve verify admission git common dir: %w", err)
	}
	commonDir := strings.TrimSpace(string(out))
	if commonDir == "" || !filepath.IsAbs(commonDir) {
		return "", fmt.Errorf("invalid git common dir %q", commonDir)
	}
	return filepath.Join(commonDir, "boss-verify", "verify-admission.json"), nil
}
