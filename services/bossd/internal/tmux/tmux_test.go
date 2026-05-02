package tmux

import (
	"context"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// TestSetAttachOptions verifies that SetAttachOptions issues the two
// session-level tmux set-option commands in the expected order with the
// expected arguments.
func TestSetAttachOptions(t *testing.T) {
	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(mock.factory))
	ctx := context.Background()

	if err := c.SetAttachOptions(ctx, "boss-test-sess"); err != nil {
		t.Fatalf("SetAttachOptions failed: %v", err)
	}

	wantCalls := [][]string{
		{"tmux", "set-option", "-t", "boss-test-sess", "aggressive-resize", "on"},
		{"tmux", "set-option", "-t", "boss-test-sess", "window-size", "smallest"},
	}
	if len(mock.calls) != len(wantCalls) {
		t.Fatalf("expected %d tmux calls, got %d: %v", len(wantCalls), len(mock.calls), mock.calls)
	}
	for i, want := range wantCalls {
		if !equalSlices(mock.calls[i], want) {
			t.Errorf("call %d: expected %v, got %v", i, want, mock.calls[i])
		}
	}
}

// TestSetAttachOptions_Idempotent verifies that calling SetAttachOptions
// twice issues the same set of commands the second time — tmux's set-option
// is naturally idempotent, so the wrapper just needs to not get clever.
func TestSetAttachOptions_Idempotent(t *testing.T) {
	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(mock.factory))
	ctx := context.Background()

	if err := c.SetAttachOptions(ctx, "boss-test-sess"); err != nil {
		t.Fatalf("first SetAttachOptions failed: %v", err)
	}
	firstRun := append([][]string(nil), mock.calls...)

	mock.calls = nil
	if err := c.SetAttachOptions(ctx, "boss-test-sess"); err != nil {
		t.Fatalf("second SetAttachOptions failed: %v", err)
	}
	secondRun := mock.calls

	if len(firstRun) != len(secondRun) {
		t.Fatalf("idempotent calls should produce same number of invocations: first=%d second=%d",
			len(firstRun), len(secondRun))
	}
	for i := range firstRun {
		if !equalSlices(firstRun[i], secondRun[i]) {
			t.Errorf("call %d differs between runs: first=%v second=%v",
				i, firstRun[i], secondRun[i])
		}
	}
}

// TestSetAttachOptions_Error verifies that a tmux invocation failure surfaces
// as an error (not swallowed). Catches mutations like err != nil → err == nil.
func TestSetAttachOptions_Error(t *testing.T) {
	c := NewClient(WithCommandFactory(func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		// Simulate tmux failure.
		return exec.CommandContext(ctx, "false")
	}))

	err := c.SetAttachOptions(context.Background(), "boss-test-sess")
	if err == nil {
		t.Fatal("expected error when tmux invocation fails, got nil")
	}
}

// TestRefreshClient verifies the wrapper issues `tmux refresh-client -t <name>`
// with the configured session name. Used by the web-tmux-attach client after
// a ring-buffer overflow to force tmux to repaint all attached viewers.
func TestRefreshClient(t *testing.T) {
	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(mock.factory))
	ctx := context.Background()

	if err := c.RefreshClient(ctx, "boss-test-sess"); err != nil {
		t.Fatalf("RefreshClient failed: %v", err)
	}

	want := []string{"tmux", "refresh-client", "-t", "boss-test-sess"}
	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 tmux call, got %d: %v", len(mock.calls), mock.calls)
	}
	if !equalSlices(mock.calls[0], want) {
		t.Errorf("RefreshClient args: expected %v, got %v", want, mock.calls[0])
	}
}

// TestRefreshClient_EmptySessionName guards the empty-name validation so
// callers can't accidentally invoke `tmux refresh-client -t` with no target.
func TestRefreshClient_EmptySessionName(t *testing.T) {
	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(mock.factory))
	ctx := context.Background()

	if err := c.RefreshClient(ctx, ""); err == nil {
		t.Fatal("expected error for empty session name, got nil")
	}
	if len(mock.calls) != 0 {
		t.Errorf("expected no tmux calls when session name is empty, got %d", len(mock.calls))
	}
}

// TestRefreshClient_Error verifies a tmux invocation failure surfaces as an
// error rather than being swallowed. Catches mutations like err != nil →
// err == nil that would silently break the resync repaint flow.
func TestRefreshClient_Error(t *testing.T) {
	c := NewClient(WithCommandFactory(func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "false")
	}))

	if err := c.RefreshClient(context.Background(), "boss-test-sess"); err == nil {
		t.Fatal("expected error when tmux invocation fails, got nil")
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

// sendPlanRecordingFactory is a CommandFactory that records each tmux
// invocation in order, lets callers stub per-subcommand outputs and exit
// status, captures the stdin handed to load-buffer, and varies
// capture-pane output across calls so tests can model "marker appears on
// poll N" scenarios.
type sendPlanRecordingFactory struct {
	mu    sync.Mutex
	calls []sendPlanCall

	// capturePaneOutputs is consumed in order; once exhausted, the last
	// value is reused. Empty slice → empty stdout for every call.
	capturePaneOutputs []string
	captureCallIdx     atomic.Int32

	// failOnSubcommand maps a subcommand (e.g. "send-keys") to the call
	// index at which it should exit non-zero. Default: never fail.
	failOnSubcommand map[string]int
}

type sendPlanCall struct {
	subcommand string
	args       []string
}

func (f *sendPlanRecordingFactory) factory(ctx context.Context, name string, args ...string) *exec.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()

	subcommand := ""
	if len(args) > 0 {
		subcommand = args[0]
	}
	f.calls = append(f.calls, sendPlanCall{subcommand: subcommand, args: append([]string(nil), args[1:]...)})

	if failIdx, ok := f.failOnSubcommand[subcommand]; ok && failIdx == subcommandSeenIndex(f.calls, subcommand)-1 {
		return exec.CommandContext(ctx, "false")
	}

	switch subcommand {
	case "capture-pane":
		idx := int(f.captureCallIdx.Add(1)) - 1
		out := ""
		if len(f.capturePaneOutputs) > 0 {
			if idx >= len(f.capturePaneOutputs) {
				out = f.capturePaneOutputs[len(f.capturePaneOutputs)-1]
			} else {
				out = f.capturePaneOutputs[idx]
			}
		}
		return exec.CommandContext(ctx, "printf", "%s", out)
	case "load-buffer":
		// `cat` drains whatever stdin SendPlan assigns and exits 0.
		// Stdin contents aren't asserted by this factory's tests — see
		// TestSendPlan_LoadBufferReceivesPlanStdin for that coverage.
		cmd := exec.CommandContext(ctx, "cat")
		cmd.Stdout = io.Discard
		return cmd
	default:
		return exec.CommandContext(ctx, "true")
	}
}

// subcommandSeenIndex returns the 1-based occurrence count of subcommand
// in calls so far (i.e. "this is the Nth time we've seen this subcommand").
func subcommandSeenIndex(calls []sendPlanCall, subcommand string) int {
	n := 0
	for _, c := range calls {
		if c.subcommand == subcommand {
			n++
		}
	}
	return n
}

// callsCopy returns a copy of the recorded call log.
func (f *sendPlanRecordingFactory) callsCopy() []sendPlanCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sendPlanCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// TestSendPlan_HappyPath_Order verifies SendPlan issues capture-pane (poll
// loop), load-buffer, paste-buffer, send-keys in order with the right args.
func TestSendPlan_HappyPath_Order(t *testing.T) {
	fake := &sendPlanRecordingFactory{
		// Marker missing on first poll, present on second — exercises the
		// poll loop without waiting the full deadline.
		capturePaneOutputs: []string{
			"Welcome to Claude\n",
			"Welcome to Claude\n❯\n",
		},
	}
	c := NewClient(WithCommandFactory(fake.factory))

	if err := c.sendPlan(context.Background(), "boss-test-sess", "do the thing", sendPlanOpts{
		deadline:     2 * time.Second,
		pollInterval: 5 * time.Millisecond,
	}); err != nil {
		t.Fatalf("SendPlan: unexpected error: %v", err)
	}

	calls := fake.callsCopy()
	// Must have at least 2 capture-pane calls (poll loop) followed by
	// load-buffer, paste-buffer, send-keys.
	subcommands := make([]string, len(calls))
	for i, c := range calls {
		subcommands[i] = c.subcommand
	}

	// Verify the tail order.
	wantTail := []string{"load-buffer", "paste-buffer", "send-keys"}
	if len(calls) < len(wantTail) {
		t.Fatalf("expected at least %d calls, got %d: %v", len(wantTail), len(calls), subcommands)
	}
	gotTail := subcommands[len(calls)-len(wantTail):]
	if !equalSlices(gotTail, wantTail) {
		t.Errorf("tail subcommands = %v, want %v (full sequence: %v)", gotTail, wantTail, subcommands)
	}

	// Verify ≥ 2 capture-pane calls preceded the tail.
	captureCount := 0
	for _, s := range subcommands[:len(calls)-len(wantTail)] {
		if s == "capture-pane" {
			captureCount++
		}
	}
	if captureCount < 2 {
		t.Errorf("expected ≥ 2 capture-pane polls before paste, got %d (subcommands: %v)", captureCount, subcommands)
	}

	// Verify args on the trailing commands.
	loadCall := calls[len(calls)-3]
	if !equalSlices(loadCall.args, []string{"-"}) {
		t.Errorf("load-buffer args = %v, want [-]", loadCall.args)
	}
	pasteCall := calls[len(calls)-2]
	if !equalSlices(pasteCall.args, []string{"-d", "-p", "-t", "boss-test-sess"}) {
		t.Errorf("paste-buffer args = %v, want [-d -p -t boss-test-sess]", pasteCall.args)
	}
	enterCall := calls[len(calls)-1]
	if !equalSlices(enterCall.args, []string{"-t", "boss-test-sess", "Enter"}) {
		t.Errorf("send-keys args = %v, want [-t boss-test-sess Enter]", enterCall.args)
	}
}

// TestSendPlan_ReadyMarkerNeverAppears_Errors verifies the deadline path:
// if capture-pane never returns the marker, SendPlan returns an error
// without trying load-buffer / paste-buffer / send-keys.
func TestSendPlan_ReadyMarkerNeverAppears_Errors(t *testing.T) {
	fake := &sendPlanRecordingFactory{
		capturePaneOutputs: []string{"Welcome to Claude — still loading\n"},
	}
	c := NewClient(WithCommandFactory(fake.factory))

	// Use a tight deadline so the test runs quickly.
	err := c.sendPlan(context.Background(), "boss-test-sess", "plan body", sendPlanOpts{
		deadline:     50 * time.Millisecond,
		pollInterval: 5 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected error when ready marker never appears, got nil")
	}
	if !strings.Contains(err.Error(), "ready marker") {
		t.Errorf("expected error to mention ready marker, got: %v", err)
	}

	// Should have polled capture-pane multiple times but never invoked
	// any of the paste-related subcommands.
	calls := fake.callsCopy()
	for _, c := range calls {
		switch c.subcommand {
		case "load-buffer", "paste-buffer", "send-keys":
			t.Errorf("expected no %s call when marker never appears, got: %v", c.subcommand, c.args)
		}
	}
}

// TestSendPlan_CustomStatuslineReady_Succeeds reproduces the failure mode
// from a real cron run against a Claude Code instance with a customised
// statusline. The default footer hint ("? for shortcuts") is replaced by
// the user's statusline (PR badge, /effort tag, model summary), so the
// only stable readiness signal in the captured pane is the input-box
// prompt indicator (❯). SendPlan must detect that and proceed.
func TestSendPlan_CustomStatuslineReady_Succeeds(t *testing.T) {
	const customStatuslinePane = ` ▐▛███▜▌   Claude Code v2.1.126
▝▜█████▛▘  Opus 4.7 (1M context) · Claude Max
  ▘▘ ▝▝    ~/.bossanova/worktrees/bossanova/add-a-scheduling-feature

────────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────────
  Opus 4.7 (1M context) | /Users/dave/.bossanova/worktrees/bossanova/add-a-sc…
  PR #133
                                                             ◉ xhigh · /effort
`
	fake := &sendPlanRecordingFactory{
		capturePaneOutputs: []string{customStatuslinePane},
	}
	c := NewClient(WithCommandFactory(fake.factory))

	if err := c.sendPlan(context.Background(), "boss-test-sess", "the plan", sendPlanOpts{
		deadline:     500 * time.Millisecond,
		pollInterval: 5 * time.Millisecond,
	}); err != nil {
		t.Fatalf("SendPlan against custom-statusline pane: unexpected error: %v", err)
	}

	// All three paste subcommands must have run — confirms the marker
	// poll resolved and SendPlan didn't abort early.
	calls := fake.callsCopy()
	wantTail := []string{"load-buffer", "paste-buffer", "send-keys"}
	if len(calls) < len(wantTail) {
		t.Fatalf("expected at least %d tail calls, got %d", len(wantTail), len(calls))
	}
	gotTail := make([]string, len(wantTail))
	for i, c := range calls[len(calls)-len(wantTail):] {
		gotTail[i] = c.subcommand
	}
	if !equalSlices(gotTail, wantTail) {
		t.Errorf("tail subcommands = %v, want %v", gotTail, wantTail)
	}
}

// TestSendPlan_TimeoutErrorIncludesPaneContents verifies that when the
// ready marker never appears, the resulting error embeds the last captured
// pane so operators can diagnose what Claude was actually showing without
// having to re-run with extra instrumentation. Without this, the cron
// failure path was opaque ("ready marker not seen") because the daemon
// also kills the tmux session as cleanup, leaving nothing to attach to.
func TestSendPlan_TimeoutErrorIncludesPaneContents(t *testing.T) {
	const fingerprint = "AUTH-PROMPT-VISIBLE-IN-PANE"
	fake := &sendPlanRecordingFactory{
		capturePaneOutputs: []string{"Welcome to Claude\n" + fingerprint + "\nplease re-authenticate"},
	}
	c := NewClient(WithCommandFactory(fake.factory))

	err := c.sendPlan(context.Background(), "boss-test-sess", "plan body", sendPlanOpts{
		deadline:     50 * time.Millisecond,
		pollInterval: 5 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), fingerprint) {
		t.Errorf("expected timeout error to include captured pane content (looking for %q), got: %v",
			fingerprint, err)
	}
}

// TestSendPlan_PasteBufferFails_Errors verifies the failure mode where one
// of the tmux subcommands (paste-buffer) returns non-zero. SendPlan must
// surface that as an error.
func TestSendPlan_PasteBufferFails_Errors(t *testing.T) {
	fake := &sendPlanRecordingFactory{
		capturePaneOutputs: []string{"Welcome to Claude\n❯\n"},
		failOnSubcommand: map[string]int{
			"paste-buffer": 0, // first paste-buffer call fails
		},
	}
	c := NewClient(WithCommandFactory(fake.factory))

	err := c.sendPlan(context.Background(), "boss-test-sess", "plan body", sendPlanOpts{
		deadline:     time.Second,
		pollInterval: 5 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected error when paste-buffer fails, got nil")
	}
	if !strings.Contains(err.Error(), "paste-buffer") {
		t.Errorf("expected paste-buffer error, got: %v", err)
	}
}

// TestSendPlan_EmptySessionName_Errors guards the input validation so a
// caller can't accidentally send a plan to no target.
func TestSendPlan_EmptySessionName_Errors(t *testing.T) {
	c := NewClient()
	err := c.SendPlan(context.Background(), "", "plan body")
	if err == nil {
		t.Fatal("expected error for empty session name, got nil")
	}
}

// TestSendPlan_LoadBufferReceivesPlanStdin verifies that SendPlan pipes
// the plan through tmux load-buffer's stdin (rather than as an argv).
// We use a real shell command (`cat > tmpfile`) to capture stdin.
func TestSendPlan_LoadBufferReceivesPlanStdin(t *testing.T) {
	tmpFile := t.TempDir() + "/load-buffer-stdin"

	captureCalls := atomic.Int32{}
	c := NewClient(WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if len(args) > 0 && args[0] == "capture-pane" {
			captureCalls.Add(1)
			return exec.CommandContext(ctx, "printf", "%s", "Welcome\n❯\n")
		}
		if len(args) > 0 && args[0] == "load-buffer" {
			// Drain stdin into tmpFile so the test can assert on it.
			return exec.CommandContext(ctx, "sh", "-c", "cat > "+tmpFile)
		}
		return exec.CommandContext(ctx, "true")
	}))

	plan := "the plan body\nwith multiple lines"
	if err := c.sendPlan(context.Background(), "boss-test", plan, sendPlanOpts{
		deadline:     time.Second,
		pollInterval: 5 * time.Millisecond,
	}); err != nil {
		t.Fatalf("SendPlan: %v", err)
	}

	got, err := readFile(tmpFile)
	if err != nil {
		t.Fatalf("read stdin capture: %v", err)
	}
	if got != plan {
		t.Errorf("load-buffer stdin = %q, want %q", got, plan)
	}
}

// readFile is a tiny helper that returns a file's contents as a string.
// Inlined here to avoid pulling os into the test imports just for this.
func readFile(path string) (string, error) {
	f, err := exec.Command("cat", path).Output()
	if err != nil {
		return "", err
	}
	return string(f), nil
}
