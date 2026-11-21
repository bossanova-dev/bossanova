package tmux

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"
)

// ServeSessionName names a session's dedicated repository serve process.
func ServeSessionName(repoID, sessionID string) string {
	return SessionName(repoID, sessionID) + "-serve"
}

// KillPaneProcessGroup terminates the pane's process group, including children
// that ignore the SIGHUP tmux sends when its session is killed.
func (c *Client) KillPaneProcessGroup(ctx context.Context, name string) error {
	pid, err := c.PanePID(ctx, name)
	if err != nil {
		alive, probeErr := c.HasSessionStatus(ctx, name)
		if probeErr == nil && !alive {
			return nil
		}
		return err
	}
	group, err := syscall.Getpgid(pid)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	own, err := syscall.Getpgid(0)
	if err != nil {
		return err
	}
	if group <= 1 || group == own {
		return fmt.Errorf("refuse to kill unsafe pane process group %d", group)
	}
	err = syscall.Kill(-group, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (c *Client) requireExitOnCompletion(ctx context.Context, name string) error {
	cmd := c.cmdFunc(ctx, "tmux", "set-option", "-t", name, "remain-on-exit", "off")
	if err := cmd.Run(); err != nil {
		return c.cleanFailedServeStart(ctx, name, err)
	}
	// Disabling retention does not reap a pane that exited before the option
	// changed. Remove that dead pane without signalling its potentially reused PID.
	dead, err := c.PaneDead(ctx, name)
	if err != nil {
		return c.cleanFailedServeStart(ctx, name, err)
	}
	if dead {
		return c.KillSession(ctx, name)
	}
	return nil
}

func (c *Client) cleanFailedServeStart(ctx context.Context, name string, cause error) error {
	exists, probeErr := c.HasSessionStatus(ctx, name)
	if probeErr == nil && !exists {
		return nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	groupErr := c.KillPaneProcessGroup(cleanup, name)
	killErr := c.KillSession(cleanup, name)
	return errors.Join(fmt.Errorf("ensure serve pane exits on completion: %w", cause), groupErr, killErr)
}
