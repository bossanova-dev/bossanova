package tmux

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Pane identifies a pane's root process and owning tmux session.
type Pane struct {
	PID         int
	SessionName string
}

// ListPanes reads all pane roots in one invocation. An absent tmux server
// returns no panes; other failures and malformed records return an error so
// caller attribution cannot mistake an incomplete inventory for an empty one.
func (c *Client) ListPanes(ctx context.Context) ([]Pane, error) {
	cmd := c.cmdFunc(ctx, "tmux", "list-panes", "-a", "-F", "#{pane_pid} #{session_name}")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = tmuxCommandWaitDelay
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "no server running") ||
			(strings.Contains(msg, "error connecting to") && strings.HasSuffix(msg, "(No such file or directory)")) {
			return nil, nil
		}
		return nil, fmt.Errorf("tmux list-panes: %w (stderr: %s)", err, msg)
	}

	var panes []Pane
	for _, line := range strings.Split(stdout.String(), "\n") {
		if line == "" {
			continue
		}
		pidText, sessionName, ok := strings.Cut(line, " ")
		if !ok || strings.TrimSpace(sessionName) == "" {
			return nil, fmt.Errorf("tmux list-panes: malformed pane record %q", line)
		}
		// Retained dead panes have no process that can occur in a caller's ancestry.
		if pidText == "" {
			continue
		}
		pid, err := strconv.Atoi(pidText)
		if err != nil || pid < 0 {
			return nil, fmt.Errorf("tmux list-panes: malformed pane record %q", line)
		}
		if pid == 0 {
			continue
		}
		panes = append(panes, Pane{PID: pid, SessionName: sessionName})
	}
	return panes, nil
}
