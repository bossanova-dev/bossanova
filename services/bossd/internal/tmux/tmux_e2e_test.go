package tmux

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestE2E_Lifecycle(t *testing.T) {
	// Skip if tmux not installed.
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed, skipping E2E tests")
	}

	c := NewClient()
	ctx := context.Background()

	// Verify tmux is available.
	if !c.Available(ctx) {
		t.Fatal("tmux should be available")
	}

	// Create a unique session name.
	name := "boss-e2e-" + time.Now().Format("20060102150405")
	workDir := t.TempDir()

	// Clean up on exit.
	defer func() {
		_ = c.KillSession(ctx, name)
	}()

	// Create a session running sleep.
	err := c.NewSession(ctx, NewSessionOpts{
		Name:    name,
		WorkDir: workDir,
		Command: []string{"sleep", "60"},
		Width:   120,
		Height:  30,
	})
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	// Verify session exists.
	if !c.HasSession(ctx, name) {
		t.Fatal("session should exist after creation")
	}

	// Kill the session.
	err = c.KillSession(ctx, name)
	if err != nil {
		t.Fatalf("failed to kill session: %v", err)
	}

	// Verify session no longer exists.
	if c.HasSession(ctx, name) {
		t.Fatal("session should not exist after kill")
	}

	// Killing again should be idempotent (no error).
	err = c.KillSession(ctx, name)
	if err != nil {
		t.Fatalf("killing already-dead session should be idempotent: %v", err)
	}
}
