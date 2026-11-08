package tmux

import (
	"bytes"
	"context"
	"fmt"
	"strings"
)

// CurrentPaths inventories pane working directories. No server means no panes;
// every other failure is unknown liveness and must prohibit cleanup.
func (c *Client) CurrentPaths(ctx context.Context) ([]string, error) {
	cmd := c.cmdFunc(ctx, "tmux", "list-panes", "-a", "-F", "#{pane_current_path}")
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	cmd.WaitDelay = tmuxCommandWaitDelay
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "no server running") || (strings.Contains(msg, "error connecting to") && strings.HasSuffix(msg, "(No such file or directory)")) {
			return nil, nil
		}
		return nil, fmt.Errorf("tmux pane directories: %w (stderr: %s)", err, msg)
	}
	var paths []string
	for _, path := range strings.Split(out.String(), "\n") {
		if path == "" {
			continue
		}
		if !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("invalid tmux pane directory %q", path)
		}
		paths = append(paths, path)
	}
	return paths, nil
}
