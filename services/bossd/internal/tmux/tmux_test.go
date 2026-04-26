package tmux

import (
	"context"
	"os/exec"
	"testing"
)

// mockCommandFactory captures the command arguments for testing.
type mockCommandFactory struct {
	calls [][]string
}

func (m *mockCommandFactory) factory(ctx context.Context, name string, args ...string) *exec.Cmd {
	m.calls = append(m.calls, append([]string{name}, args...))
	// Return a command that immediately succeeds.
	return exec.CommandContext(ctx, "true")
}

func (m *mockCommandFactory) lastCall() []string {
	if len(m.calls) == 0 {
		return nil
	}
	return m.calls[len(m.calls)-1]
}

func TestAvailable(t *testing.T) {
	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(mock.factory))
	ctx := context.Background()

	if !c.Available(ctx) {
		t.Fatalf("expected Available to return true")
	}

	call := mock.lastCall()
	if len(call) != 2 || call[0] != "tmux" || call[1] != "-V" {
		t.Errorf("expected ['tmux', '-V'], got %v", call)
	}
}

func TestNotAvailable(t *testing.T) {
	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		mock.calls = append(mock.calls, append([]string{name}, args...))
		// Return a command that always fails.
		return exec.CommandContext(ctx, "false")
	}))
	ctx := context.Background()

	if c.Available(ctx) {
		t.Fatalf("expected Available to return false when tmux command fails")
	}
}

func TestNewSession_Args(t *testing.T) {
	tests := []struct {
		name     string
		opts     NewSessionOpts
		expected []string
	}{
		{
			name: "basic session",
			opts: NewSessionOpts{
				Name:    "test-session",
				WorkDir: "/tmp/workdir",
				Command: []string{"claude", "--session-id", "abc123"},
			},
			expected: []string{
				"tmux", "new-session", "-d", "-s", "test-session",
				"-c", "/tmp/workdir", "-x", "200", "-y", "50",
				"claude", "--session-id", "abc123",
			},
		},
		{
			name: "custom dimensions",
			opts: NewSessionOpts{
				Name:    "custom-dims",
				WorkDir: "/var/work",
				Command: []string{"sh", "-c", "echo hello"},
				Width:   120,
				Height:  30,
			},
			expected: []string{
				"tmux", "new-session", "-d", "-s", "custom-dims",
				"-c", "/var/work", "-x", "120", "-y", "30",
				"sh", "-c", "echo hello",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockCommandFactory{}
			c := NewClient(WithCommandFactory(mock.factory))
			ctx := context.Background()

			err := c.NewSession(ctx, tt.opts)
			if err != nil {
				t.Fatalf("NewSession failed: %v", err)
			}

			// First call is new-session; subsequent calls bind detach keys
			// and set session options (extended-keys, mouse).
			if len(mock.calls) == 0 {
				t.Fatal("expected at least one call")
			}
			call := mock.calls[0]
			if !equalSlices(call, tt.expected) {
				t.Errorf("expected %v, got %v", tt.expected, call)
			}
		})
	}
}

func TestNewSession_RequiredFields(t *testing.T) {
	tests := []struct {
		name string
		opts NewSessionOpts
		err  string
	}{
		{
			name: "missing name",
			opts: NewSessionOpts{WorkDir: "/tmp", Command: []string{"sh"}},
			err:  "session name is required",
		},
		{
			name: "missing workdir",
			opts: NewSessionOpts{Name: "test", Command: []string{"sh"}},
			err:  "work directory is required",
		},
		{
			name: "missing command",
			opts: NewSessionOpts{Name: "test", WorkDir: "/tmp"},
			err:  "command is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewClient()
			ctx := context.Background()

			err := c.NewSession(ctx, tt.opts)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if err.Error() != tt.err {
				t.Errorf("expected error %q, got %q", tt.err, err.Error())
			}
		})
	}
}

func TestSessionName(t *testing.T) {
	tests := []struct {
		name      string
		repoID    string
		sessionID string
		expected  string
	}{
		{
			name:      "normal IDs",
			repoID:    "abcdef123456",
			sessionID: "xyz789012345",
			expected:  "boss-abcdef12-xyz78901",
		},
		{
			name:      "short IDs",
			repoID:    "abc",
			sessionID: "xyz",
			expected:  "boss-abc-xyz",
		},
		{
			name:      "exact 8 chars",
			repoID:    "12345678",
			sessionID: "abcdefgh",
			expected:  "boss-12345678-abcdefgh",
		},
		{
			name:      "very long IDs",
			repoID:    "0123456789abcdef",
			sessionID: "fedcba9876543210",
			expected:  "boss-01234567-fedcba98",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SessionName(tt.repoID, tt.sessionID)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestNewSession_ExtendedKeysAlways(t *testing.T) {
	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(mock.factory))
	ctx := context.Background()

	err := c.NewSession(ctx, NewSessionOpts{
		Name:    "boss-test-1234",
		WorkDir: "/tmp",
		Command: []string{"claude"},
	})
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}

	// Verify extended-keys is set to "always" (not "on") so that tmux
	// unconditionally forwards modifier+key combos like Shift+Enter.
	expected := []string{"tmux", "set-option", "-t", "boss-test-1234", "extended-keys", "always"}
	found := false
	for _, call := range mock.calls {
		if equalSlices(call, expected) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected set-option extended-keys always call, got calls: %v", mock.calls)
	}
}

func TestNewSession_EnablesMouse(t *testing.T) {
	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(mock.factory))
	ctx := context.Background()

	err := c.NewSession(ctx, NewSessionOpts{
		Name:    "boss-test-1234",
		WorkDir: "/tmp",
		Command: []string{"claude"},
	})
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}

	// Look for the global set-option mouse on call.
	expected := []string{"tmux", "set-option", "-g", "mouse", "on"}
	found := false
	for _, call := range mock.calls {
		if equalSlices(call, expected) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected set-option mouse on call, got calls: %v", mock.calls)
	}
}

func TestNewSession_ExtendedKeysFormatCSIU(t *testing.T) {
	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(mock.factory))
	ctx := context.Background()

	err := c.NewSession(ctx, NewSessionOpts{
		Name:    "boss-test-1234",
		WorkDir: "/tmp",
		Command: []string{"claude"},
	})
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}

	// Verify extended-keys-format is set to csi-u so Claude Code receives
	// CSI-u sequences (e.g. \x1b[13;2u) instead of xterm format.
	expected := []string{"tmux", "set-option", "-g", "extended-keys-format", "csi-u"}
	found := false
	for _, call := range mock.calls {
		if equalSlices(call, expected) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected set-option extended-keys-format csi-u call, got calls: %v", mock.calls)
	}
}

func TestNewSession_PreservesTermProgram(t *testing.T) {
	// Set TERM_PROGRAM to simulate running under a real terminal.
	t.Setenv("TERM_PROGRAM", "ghostty")

	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(mock.factory))
	ctx := context.Background()

	err := c.NewSession(ctx, NewSessionOpts{
		Name:    "boss-test-1234",
		WorkDir: "/tmp",
		Command: []string{"claude"},
	})
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}

	// Verify TERM_PROGRAM is forwarded into the tmux session environment.
	expected := []string{"tmux", "set-environment", "-t", "boss-test-1234", "TERM_PROGRAM", "ghostty"}
	found := false
	for _, call := range mock.calls {
		if equalSlices(call, expected) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected set-environment TERM_PROGRAM ghostty call, got calls: %v", mock.calls)
	}
}

func TestNewSession_SkipsTermProgramWhenTmux(t *testing.T) {
	// When TERM_PROGRAM is already "tmux", we should NOT set it
	// (that's the value we're trying to override).
	t.Setenv("TERM_PROGRAM", "tmux")

	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(mock.factory))
	ctx := context.Background()

	err := c.NewSession(ctx, NewSessionOpts{
		Name:    "boss-test-1234",
		WorkDir: "/tmp",
		Command: []string{"claude"},
	})
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}

	for _, call := range mock.calls {
		if len(call) >= 2 && call[1] == "set-environment" {
			t.Errorf("should not set-environment when TERM_PROGRAM=tmux, got call: %v", call)
		}
	}
}

func TestHasSession(t *testing.T) {
	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(mock.factory))
	ctx := context.Background()

	if !c.HasSession(ctx, "test-session") {
		t.Fatalf("expected HasSession to return true")
	}

	call := mock.lastCall()
	expected := []string{"tmux", "has-session", "-t", "test-session"}
	if !equalSlices(call, expected) {
		t.Errorf("expected %v, got %v", expected, call)
	}
}

func TestKillSession_NotExist(t *testing.T) {
	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		mock.calls = append(mock.calls, append([]string{name}, args...))
		// Simulate tmux error for non-existent session.
		// Both kill-session and has-session should fail.
		cmd := exec.CommandContext(ctx, "sh", "-c", "exit 1")
		return cmd
	}))
	ctx := context.Background()

	// Should not return an error for non-existent session (idempotent).
	err := c.KillSession(ctx, "test-session")
	if err != nil {
		t.Fatalf("expected no error for non-existent session, got: %v", err)
	}

	// Should have called both kill-session and has-session.
	if len(mock.calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(mock.calls))
	}

	expectedKill := []string{"tmux", "kill-session", "-t", "test-session"}
	if !equalSlices(mock.calls[0], expectedKill) {
		t.Errorf("expected first call to be %v, got %v", expectedKill, mock.calls[0])
	}

	expectedHas := []string{"tmux", "has-session", "-t", "test-session"}
	if !equalSlices(mock.calls[1], expectedHas) {
		t.Errorf("expected second call to be %v, got %v", expectedHas, mock.calls[1])
	}
}

func TestCapturePane_ScrollbackFlag(t *testing.T) {
	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(mock.factory))
	ctx := context.Background()

	// CapturePane returns empty since mock "true" produces no output, but
	// we only care about verifying the command arguments.
	_, _ = c.CapturePane(ctx, "boss-test-sess")

	call := mock.lastCall()
	expected := []string{"tmux", "capture-pane", "-p", "-S", "-1000", "-t", "boss-test-sess"}
	if !equalSlices(call, expected) {
		t.Errorf("CapturePane args: expected %v, got %v", expected, call)
	}
}

func TestPasteText_Args(t *testing.T) {
	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(mock.factory))
	ctx := context.Background()

	if err := c.PasteText(ctx, "boss-test-sess", "hello\nworld"); err != nil {
		t.Fatalf("PasteText failed: %v", err)
	}

	if len(mock.calls) != 2 {
		t.Fatalf("expected 2 tmux calls (load-buffer, paste-buffer), got %d: %v", len(mock.calls), mock.calls)
	}

	wantLoad := []string{"tmux", "load-buffer", "-b", "bossanova-prefill-boss-test-sess", "-"}
	if !equalSlices(mock.calls[0], wantLoad) {
		t.Errorf("load-buffer args: expected %v, got %v", wantLoad, mock.calls[0])
	}

	wantPaste := []string{"tmux", "paste-buffer", "-d", "-p", "-b", "bossanova-prefill-boss-test-sess", "-t", "boss-test-sess"}
	if !equalSlices(mock.calls[1], wantPaste) {
		t.Errorf("paste-buffer args: expected %v, got %v", wantPaste, mock.calls[1])
	}
}

func TestPasteText_EmptySessionName(t *testing.T) {
	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(mock.factory))
	ctx := context.Background()

	if err := c.PasteText(ctx, "", "hi"); err == nil {
		t.Fatal("expected error for empty session name, got nil")
	}
	if len(mock.calls) != 0 {
		t.Errorf("expected no tmux calls when session name is empty, got %d", len(mock.calls))
	}
}

// TestChatSessionName covers the same > 8 truncation logic as SessionName,
// applied to the chat-id path.
func TestChatSessionName(t *testing.T) {
	tests := []struct {
		name     string
		repoID   string
		claudeID string
		expected string
	}{
		{"both short", "abc", "xyz", "boss-abc-xyz"},
		{"both exact 8", "12345678", "abcdefgh", "boss-12345678-abcdefgh"},
		{"both 9 chars truncate", "123456789", "abcdefghi", "boss-12345678-abcdefgh"},
		{"both long truncate to 8", "0123456789abcdef", "fedcba9876543210", "boss-01234567-fedcba98"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ChatSessionName(tt.repoID, tt.claudeID)
			if got != tt.expected {
				t.Errorf("ChatSessionName(%q, %q) = %q, want %q",
					tt.repoID, tt.claudeID, got, tt.expected)
			}
		})
	}
}

// TestSessionName_NineCharBoundary covers the boundary mutation `len > 8`.
// At exactly 9 characters, the boundary mutates differently than at 8.
func TestSessionName_NineCharBoundary(t *testing.T) {
	// 9-char IDs must be truncated to 8.
	got := SessionName("123456789", "abcdefghi")
	want := "boss-12345678-abcdefgh"
	if got != want {
		t.Errorf("SessionName(9-char): got %q, want %q", got, want)
	}
}

// TestCapturePane_Success verifies the success path: a working tmux command
// returns the captured pane content with no error.
// Catches mutation: err != nil → err == nil (would return error on success).
func TestCapturePane_Success(t *testing.T) {
	mock := &captureOutputFactory{output: "pane content line 1\npane content line 2\n"}
	c := NewClient(WithCommandFactory(mock.factory))

	out, err := c.CapturePane(context.Background(), "boss-test-sess")
	if err != nil {
		t.Fatalf("CapturePane: unexpected error %v", err)
	}
	if out != "pane content line 1\npane content line 2\n" {
		t.Errorf("CapturePane content = %q", out)
	}
}

// TestCapturePane_Error verifies that a tmux command failure surfaces as an
// error. Catches mutation: err != nil → err == nil (would swallow error).
func TestCapturePane_Error(t *testing.T) {
	c := NewClient(WithCommandFactory(func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		// Simulate command failure.
		return exec.CommandContext(ctx, "false")
	}))

	out, err := c.CapturePane(context.Background(), "missing-session")
	if err == nil {
		t.Fatal("CapturePane: expected error on command failure, got nil")
	}
	if out != "" {
		t.Errorf("CapturePane: expected empty output on error, got %q", out)
	}
}

// captureOutputFactory provides a CommandFactory that emits fixed stdout.
type captureOutputFactory struct {
	output string
	calls  [][]string
}

func (f *captureOutputFactory) factory(ctx context.Context, name string, args ...string) *exec.Cmd {
	f.calls = append(f.calls, append([]string{name}, args...))
	// Use printf so output has no trailing newline beyond what we specify.
	return exec.CommandContext(ctx, "printf", "%s", f.output)
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
