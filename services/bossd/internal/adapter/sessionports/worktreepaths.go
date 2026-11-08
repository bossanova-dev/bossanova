package sessionports

import (
	"context"
	"fmt"
	"strings"
)

// WorktreeProcessPaths inventories process working directories for safe cleanup.
type WorktreeProcessPaths struct{}

// CurrentPaths fails closed even when lsof emitted partial output with an error.
func (WorktreeProcessPaths) CurrentPaths(ctx context.Context) ([]string, error) {
	out, err := runCommand(ctx, defaultProcessTimeout, "lsof", "-nP", "-d", "cwd", "-Fn")
	if err != nil {
		return nil, fmt.Errorf("list process working directories: %w", err)
	}
	return parseCWDPaths(string(out))
}

func parseCWDPaths(out string) ([]string, error) {
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p', 'f':
			continue
		case 'n':
			// Linux lsof can report /proc/<pid>/cwd with a readlink-error
			// annotation and still exit successfully. Treat procfs paths as
			// unknown rather than allowing cleanup against a partial inventory.
			if !strings.HasPrefix(line, "n/") || strings.HasPrefix(line, "n/proc/") {
				return nil, fmt.Errorf("invalid lsof cwd record %q", line)
			}
			paths = append(paths, line[1:])
		default:
			return nil, fmt.Errorf("unknown lsof cwd record %q", line)
		}
	}
	return paths, nil
}
