package accountwiring

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	maxSeedRollouts = 8
	// seedTailBytes must stay >= transcriptTailSize in plugins/bossd-plugin-codex/transcript.go.
	seedTailBytes = 64 * 1024
)

type seedFileCreator func(path string, perm os.FileMode) (io.WriteCloser, error)
type probeSeedRollout struct {
	path     string
	size     int64
	modified time.Time
}

func seedUsageProbeHome(homeKey, dstHome string) error {
	if homeKey != "CODEX_HOME" {
		return nil
	}
	srcHome := strings.TrimSpace(os.Getenv(homeKey))
	if srcHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		srcHome = filepath.Join(home, ".codex")
	}
	return seedCodexProbeSessions(srcHome, dstHome, func(path string, perm os.FileMode) (io.WriteCloser, error) {
		// #nosec G304 -- writes probe-seed copy to filepath.Join(dst, rel) where rel is from filepath.Rel of a walked file; internal, not attacker-named (secret-adjacent)
		// owner=@recurser review-by=2027-01-18 issue=BOS-28
		return os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	})
}

func seedCodexProbeSessions(srcHome, dstHome string, create seedFileCreator) error {
	src := filepath.Join(srcHome, "sessions")
	if resolved, err := filepath.EvalSymlinks(src); err == nil {
		src = resolved
	}
	dst := filepath.Join(dstHome, "sessions")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return fmt.Errorf("create usage probe sessions: %w", err)
	}
	selected := make([]probeSeedRollout, 0, maxSeedRollouts+1)
	// Selection reads metadata only, including rollouts resumed in old date directories.
	_ = filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		matches, _ := filepath.Match("rollout-*.jsonl", d.Name())
		if !matches {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		selected = append(selected, probeSeedRollout{path, info.Size(), info.ModTime()})
		sort.Slice(selected, func(i, j int) bool {
			if selected[i].modified.Equal(selected[j].modified) {
				return selected[i].path < selected[j].path
			}
			return selected[i].modified.After(selected[j].modified)
		})
		if len(selected) > maxSeedRollouts {
			selected = selected[:maxSeedRollouts]
		}
		return nil
	})
	for _, file := range selected {
		rel, err := filepath.Rel(src, file.path)
		if err != nil {
			continue
		}
		if err := copyProbeSeedTail(file, filepath.Join(dst, rel), create); err != nil {
			return err
		}
	}
	return nil
}

func copyProbeSeedTail(file probeSeedRollout, dst string, create seedFileCreator) error {
	// #nosec G304 -- reads a daemon-controlled usage-probe seed; src is under its own WalkDir root, not attacker-named (secret-adjacent)
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	in, err := os.Open(file.path)
	if err != nil {
		return nil
	}
	defer func() { _ = in.Close() }()
	start := max(int64(0), file.size-seedTailBytes)
	if _, err := in.Seek(start, io.SeekStart); err != nil {
		return nil
	}
	// Read before creating the destination so unreadable/truncated sources leave no seed.
	var tail bytes.Buffer
	_, err = io.CopyN(&tail, in, min(file.size, int64(seedTailBytes)))
	if err != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("create usage probe seed dir: %w", err)
	}
	out, err := create(dst, 0o600)
	if err != nil {
		return fmt.Errorf("create usage probe seed file: %w", err)
	}
	_, copyErr := io.CopyN(out, &tail, int64(tail.Len()))
	closeErr := out.Close()
	if copyErr != nil {
		return fmt.Errorf("copy usage probe seed tail: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close usage probe seed file: %w", closeErr)
	}
	if err := os.Chtimes(dst, file.modified, file.modified); err != nil {
		return fmt.Errorf("preserve usage probe seed mtime: %w", err)
	}
	return nil
}
