package log

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var services = []string{"bossd", "boss", "bosso"}

// Services returns known service log names in merge tie-break order.
func Services() []string {
	out := make([]string, len(services))
	copy(out, services)
	return out
}

// KnownService reports whether name writes one of the supported service logs.
func KnownService(name string) bool {
	for _, service := range services {
		if name == service {
			return true
		}
	}
	return false
}

// RotatedPaths returns existing lumberjack log paths oldest-first, followed by
// the current file. A missing directory is an empty, non-error result.
func RotatedPaths(service string) ([]string, error) {
	current := LogPath(service)
	if current == "" {
		return nil, nil
	}
	backups, err := filepath.Glob(filepath.Join(filepath.Dir(current), service+"-*.log"))
	if err != nil {
		return nil, fmt.Errorf("glob %s log backups: %w", service, err)
	}
	sort.Strings(backups)
	paths := backups
	switch _, err := os.Stat(current); {
	case err == nil:
		paths = append(paths, current)
	case os.IsNotExist(err):
	default:
		return nil, fmt.Errorf("stat %s: %w", current, err)
	}
	return paths, nil
}

// BackupInfos returns the identities of lumberjack backups that exist now.
// Callers that need a point-in-time tail can pass these to
// TailRotatingSnapshot to exclude backups created after the snapshot.
func BackupInfos(service string) ([]os.FileInfo, error) {
	paths, err := RotatedPaths(service)
	if err != nil {
		return nil, err
	}
	current := LogPath(service)
	infos := make([]os.FileInfo, 0, len(paths))
	for _, path := range paths {
		if path == current {
			continue
		}
		info, err := os.Stat(path)
		if err == nil {
			infos = append(infos, info)
			continue
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
	}
	return infos, nil
}

// TailRotating returns the final maxLines physical lines, spilling back into
// lumberjack backups when the current file is too short.
func TailRotating(service string, maxLines int) (string, error) {
	return tailRotating(service, maxLines, nil, nil, nil, false, false)
}

// TailRotatingAt returns the final maxLines physical lines while reading no
// bytes beyond currentEnd from the file that was current at the snapshot. A
// negative currentEnd excludes the current log, which is useful when it did
// not exist at the snapshot. currentInfo identifies that file if it has since
// been rotated to a backup path.
func TailRotatingAt(service string, maxLines int, currentEnd int64, currentInfo os.FileInfo) (string, error) {
	return tailRotating(service, maxLines, &currentEnd, currentInfo, nil, false, false)
}

// TailRotatingSnapshot returns the final maxLines physical lines that existed
// when the caller captured currentEnd, currentInfo, and backupInfos. It
// excludes files that became backups after that snapshot because a follower
// will read those replacement files from their beginning.
func TailRotatingSnapshot(service string, maxLines int, currentEnd int64, currentInfo os.FileInfo, backupInfos []os.FileInfo) (string, error) {
	return tailRotating(service, maxLines, &currentEnd, currentInfo, backupInfos, true, false)
}

// TailRotatingHandoff returns the final maxLines physical lines that a paused
// follower will not read after it resumes. It reads the snapshot file only
// through currentEnd and preserves every replacement that rotated before the
// follower could reopen it, while leaving the current replacement for the
// follower to read. When the snapshot ended inside a partial line, it omits
// that partial segment because the follower already retains it.
func TailRotatingHandoff(service string, maxLines int, currentEnd int64, currentInfo os.FileInfo, partialLine bool) (string, error) {
	return tailRotating(service, maxLines, &currentEnd, currentInfo, nil, false, partialLine)
}

func tailRotating(service string, maxLines int, currentEnd *int64, currentInfo os.FileInfo, backupInfos []os.FileInfo, boundBackups, dropSnapshotPartial bool) (string, error) {
	if maxLines <= 0 {
		return "", nil
	}
	paths, err := RotatedPaths(service)
	if err != nil {
		return "", err
	}
	var lines []string
	current := LogPath(service)
	for i := len(paths) - 1; i >= 0 && len(lines) < maxLines; i-- {
		var chunk string
		var err error
		var info os.FileInfo
		isSnapshotFile := false
		if currentEnd != nil && (currentInfo != nil || (boundBackups && paths[i] != current)) {
			var statErr error
			info, statErr = os.Stat(paths[i])
			if statErr != nil {
				return "", fmt.Errorf("stat %s: %w", paths[i], statErr)
			}
			if currentInfo != nil {
				isSnapshotFile = os.SameFile(info, currentInfo)
			}
		}
		if currentEnd != nil && boundBackups && paths[i] != current && !isSnapshotFile && !matchesFileInfo(info, backupInfos) {
			// This backup was created after the follower snapshot. The follower
			// will read the replacement file from its beginning, so including it
			// here would duplicate those records.
			continue
		}
		if currentEnd != nil && isSnapshotFile {
			if *currentEnd < 0 {
				continue
			}
			lineLimit := maxLines - len(lines)
			if dropSnapshotPartial {
				// The snapshot can end in an unterminated line retained by the
				// follower. Read one extra physical line so dropping that partial
				// does not shrink the requested complete-record backlog.
				lineLimit++
			}
			chunk, err = TailAt(paths[i], lineLimit, *currentEnd)
		} else if currentEnd != nil && paths[i] == current {
			// The current pathname now identifies a replacement file that the
			// follower will read from its beginning. It did not exist at the
			// snapshot, so including it in the backlog would duplicate records.
			if currentInfo != nil {
				continue
			}
			if *currentEnd < 0 {
				continue
			}
			chunk, err = TailAt(paths[i], maxLines-len(lines), *currentEnd)
		} else {
			chunk, err = Tail(paths[i], maxLines-len(lines))
		}
		if err != nil {
			return "", err
		}
		if dropSnapshotPartial && isSnapshotFile {
			if lastNewline := strings.LastIndex(chunk, "\n"); lastNewline >= 0 {
				chunk = chunk[:lastNewline]
			} else {
				chunk = ""
			}
		}
		chunk = strings.TrimSuffix(chunk, "\n")
		if chunk != "" {
			lines = append(strings.Split(chunk, "\n"), lines...)
		}
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n"), nil
}

func matchesFileInfo(info os.FileInfo, candidates []os.FileInfo) bool {
	for _, candidate := range candidates {
		if os.SameFile(info, candidate) {
			return true
		}
	}
	return false
}
