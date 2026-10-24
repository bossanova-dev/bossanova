package git

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const preparedWorktreeMarkerName = "boss-prepared.json"

// PreparedWorktreeMarker records a prepared linked worktree kept after a failed start.
type PreparedWorktreeMarker struct {
	Claimed         bool      `json:"claimed,omitempty"` // Reserved by a create; refreshed on another transient failure.
	Version         int       `json:"version"`
	SessionID       string    `json:"sessionId"`
	RepoID          string    `json:"repoId"`
	Branch          string    `json:"branch"`
	IsBranchCreated bool      `json:"isBranchCreated"`
	HeadSHA         string    `json:"headSha"`
	BaseBranch      string    `json:"baseBranch"`
	KeptAt          time.Time `json:"keptAt"`
	KeptUntil       time.Time `json:"keptUntil"`
	Reason          string    `json:"reason"`
}

func (m PreparedWorktreeMarker) validate() error {
	if m.Version != 1 || strings.TrimSpace(m.SessionID) == "" || strings.TrimSpace(m.RepoID) == "" || strings.TrimSpace(m.Branch) == "" {
		return fmt.Errorf("invalid prepared worktree marker version or identity")
	}
	if _, err := hex.DecodeString(m.HeadSHA); err != nil || (len(m.HeadSHA) != 40 && len(m.HeadSHA) != 64) {
		return fmt.Errorf("invalid prepared worktree marker HEAD")
	}
	if m.KeptAt.IsZero() || !m.KeptUntil.After(m.KeptAt) {
		return fmt.Errorf("invalid prepared worktree marker retention interval")
	}
	if strings.TrimSpace(m.Reason) == "" || strings.ContainsAny(m.Reason, "\r\n") {
		return fmt.Errorf("invalid prepared worktree marker reason")
	}
	return nil
}

func preparedGitOutput(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", path}, args...)...)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %v in %s: %w", args, path, err)
	}
	return output, nil
}

func preparedWorktreeMarkerPath(ctx context.Context, path string) (string, error) {
	output, err := preparedGitOutput(ctx, path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	privateDir := strings.TrimSuffix(string(output), "\n")
	output, err = preparedGitOutput(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	commonDir := strings.TrimSuffix(string(output), "\n")
	// Only a linked worktree's private registration directory may hold the marker.
	if filepath.Dir(filepath.Dir(privateDir)) != filepath.Clean(commonDir) || filepath.Base(filepath.Dir(privateDir)) != "worktrees" {
		return "", fmt.Errorf("prepared worktree marker requires a linked worktree: %s", path)
	}
	return filepath.Join(privateDir, preparedWorktreeMarkerName), nil
}

// WritePreparedWorktreeMarker atomically replaces the marker in the private git directory.
func WritePreparedWorktreeMarker(ctx context.Context, path string, marker PreparedWorktreeMarker) error {
	if err := marker.validate(); err != nil {
		return err
	}
	markerPath, err := preparedWorktreeMarkerPath(ctx, path)
	if err != nil {
		return err
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("encode prepared worktree marker: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(markerPath), ".boss-prepared-*")
	if err != nil {
		return fmt.Errorf("create prepared worktree marker: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("write prepared worktree marker: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close prepared worktree marker: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), markerPath); err != nil {
		return fmt.Errorf("replace prepared worktree marker: %w", err)
	}
	return nil
}

// ReadPreparedWorktreeMarker reads and validates a linked worktree's marker.
func ReadPreparedWorktreeMarker(ctx context.Context, path string) (*PreparedWorktreeMarker, error) {
	markerPath, err := preparedWorktreeMarkerPath(ctx, path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(markerPath)
	if err != nil {
		return nil, fmt.Errorf("read prepared worktree marker: %w", err)
	}
	var marker PreparedWorktreeMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return nil, fmt.Errorf("decode prepared worktree marker: %w", err)
	}
	if err := marker.validate(); err != nil {
		return nil, err
	}
	return &marker, nil
}

// PreparedWorktreeHead returns the current worktree HEAD for retention safety checks.
func PreparedWorktreeHead(ctx context.Context, path string) (string, error) {
	output, err := preparedGitOutput(ctx, path, "rev-parse", "--verify", "HEAD")
	return strings.TrimSpace(string(output)), err
}

// ListWorktreePaths returns registered worktree paths without porcelain quoting.
func ListWorktreePaths(ctx context.Context, repoPath string) ([]string, error) {
	output, err := preparedGitOutput(ctx, repoPath, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, field := range strings.Split(string(output), "\x00") {
		if path, ok := strings.CutPrefix(field, "worktree "); ok {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// ConsumePreparedWorktreeMarker removes a prepared marker after the agent starts.
func ConsumePreparedWorktreeMarker(ctx context.Context, path string) error {
	markerPath, err := preparedWorktreeMarkerPath(ctx, path)
	if err != nil {
		return err
	}
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("consume prepared worktree marker: %w", err)
	}
	return nil
}
