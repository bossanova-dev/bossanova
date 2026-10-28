package realstack

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// GHResponse is one explicitly supported invocation of the fake GitHub CLI.
// Any invocation absent from the fixture fails instead of returning empty success.
type GHResponse struct {
	Args     []string `json:"args"`
	Stdout   string   `json:"stdout"`
	Stderr   string   `json:"stderr,omitempty"`
	ExitCode int      `json:"exit_code"`
	Hold     string   `json:"hold,omitempty"`
}

// SetGHResponses replaces this stack's exact-argv GitHub responses.
func (s *Stack) SetGHResponses(t testing.TB, responses []GHResponse) {
	t.Helper()
	data, err := json.Marshal(struct {
		Responses []GHResponse `json:"responses"`
	}{responses})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.home, "gh.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

var holdName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// WaitHoldEntered waits for the fake GitHub invocation to publish its entered marker.
func (s *Stack) WaitHoldEntered(ctx context.Context, name string) error {
	if !holdName.MatchString(name) {
		return fmt.Errorf("invalid hold name %q", name)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(s.home, "holds", name+".entered")); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("read hold %s: %w", name, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// ReleaseHold releases a held invocation, including one that has not entered yet.
func (s *Stack) ReleaseHold(name string) error {
	if !holdName.MatchString(name) {
		return fmt.Errorf("invalid hold name %q", name)
	}
	dir := filepath.Join(s.home, "holds")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create holds: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".release"), nil, 0o600); err != nil {
		return fmt.Errorf("release hold %s: %w", name, err)
	}
	return nil
}
