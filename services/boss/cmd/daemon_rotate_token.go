package main

import (
	"fmt"
	"os"

	"github.com/recurser/bossalib/socketauth"
	"github.com/spf13/cobra"
)

// rotateToken removes the daemon socket token so the next daemon start
// generates a fresh one (via socketauth.LoadOrCreateToken at Listen). Removing a
// non-existent file is treated as success — rotation is idempotent.
func rotateToken(socketPath string) error {
	if err := os.Remove(socketauth.TokenPath(socketPath)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove token: %w", err)
	}
	return nil
}

// runDaemonRotateToken resolves the daemon socket, rotates its token, and prints
// the restart instruction. The running daemon reads its token once at Listen, so
// the new token only takes effect after a restart.
func runDaemonRotateToken(cmd *cobra.Command) error {
	socketPath, err := defaultSocketPath()
	if err != nil {
		return fmt.Errorf("resolve socket path: %w", err)
	}
	if err := rotateToken(socketPath); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(),
		"Socket token rotated. Restart the daemon to apply: 'boss daemon restart' (or restart the launchd service).")
	return nil
}
