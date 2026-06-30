package tmux

import (
	"context"
	"io"
	"os/exec"
	"path/filepath"
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
		{
			// Env vars are emitted as sorted `-e KEY=VALUE` flags before the
			// command so the launched process (e.g. a cron agent) inherits them.
			name: "session environment",
			opts: NewSessionOpts{
				Name:    "cron-sess",
				WorkDir: "/tmp/wt",
				Command: []string{"claude"},
				Env: map[string]string{
					"BOSS_CRON_NAME": "Nightly triage",
					"BOSS_CRON":      "true",
				},
			},
			expected: []string{
				"tmux", "new-session", "-d", "-s", "cron-sess",
				"-c", "/tmp/wt", "-x", "200", "-y", "50",
				"-e", "BOSS_CRON=true", "-e", "BOSS_CRON_NAME=Nightly triage",
				"claude",
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	c := NewClient(WithCommandFactory(func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "false")
	}))

	if err := c.RefreshClient(context.Background(), "boss-test-sess"); err == nil {
		t.Fatal("expected error when tmux invocation fails, got nil")
	}
}

// TestPipePane verifies the wrapper issues `tmux pipe-pane -o -t <name>
// 'cat >> <quoted-log>'` so pane output is mirrored to disk without
// wrapping the running process in a shell pipe. The interactive launch
// regression that this replaces (bash -c "claude | tee log") made the
// agent's stdout a non-TTY pipe; pipe-pane lives outside the agent
// process so its PTY is unaffected.
func TestPipePane(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(mock.factory))
	ctx := context.Background()

	if err := c.PipePane(ctx, "boss-pp-sess", "/tmp/boss-pp/abc.log"); err != nil {
		t.Fatalf("PipePane failed: %v", err)
	}

	want := []string{"tmux", "pipe-pane", "-o", "-t", "boss-pp-sess", "cat >> '/tmp/boss-pp/abc.log'"}
	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 tmux call, got %d: %v", len(mock.calls), mock.calls)
	}
	if !equalSlices(mock.calls[0], want) {
		t.Errorf("PipePane args: expected %v, got %v", want, mock.calls[0])
	}
}

// TestPipePane_QuotesLogPath asserts that single quotes inside the log
// path are escaped via the '\” idiom rather than being passed through
// raw (which would prematurely close the shell-quoted string and let
// the rest of the path be interpreted as a shell command).
func TestPipePane_QuotesLogPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(mock.factory))
	ctx := context.Background()

	if err := c.PipePane(ctx, "boss-pp-q", "/tmp/foo's bar.log"); err != nil {
		t.Fatalf("PipePane failed: %v", err)
	}
	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 tmux call, got %d", len(mock.calls))
	}
	pipeArg := mock.calls[0][len(mock.calls[0])-1]
	want := `cat >> '/tmp/foo'\''s bar.log'`
	if pipeArg != want {
		t.Errorf("PipePane shell-arg: expected %q, got %q", want, pipeArg)
	}
}

// TestPipePane_EmptySessionName guards the empty-name validation so
// callers can't accidentally invoke `tmux pipe-pane -t` with no target
// (which would apply pipe-pane to the most recently active tmux pane,
// almost certainly not the chat session we meant).
func TestPipePane_EmptySessionName(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(mock.factory))

	if err := c.PipePane(context.Background(), "", "/tmp/x.log"); err == nil {
		t.Fatal("expected error for empty session name, got nil")
	}
	if len(mock.calls) != 0 {
		t.Errorf("expected no tmux calls when session name is empty, got %d", len(mock.calls))
	}
}

// TestPipePane_EmptyLogPath guards against silently arming pipe-pane to
// `cat >> ”` — that would shell-error on every byte the pane wrote.
// Better to fail loud at the call site.
func TestPipePane_EmptyLogPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(mock.factory))

	if err := c.PipePane(context.Background(), "boss-pp", ""); err == nil {
		t.Fatal("expected error for empty log path, got nil")
	}
	if len(mock.calls) != 0 {
		t.Errorf("expected no tmux calls when log path is empty, got %d", len(mock.calls))
	}
}

// TestPipePane_Error verifies a tmux invocation failure surfaces as an
// error rather than being swallowed. Catches mutations like err != nil →
// err == nil that would silently leave the chat with no on-disk log.
func TestPipePane_Error(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	c := NewClient(WithCommandFactory(func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "false")
	}))

	if err := c.PipePane(context.Background(), "boss-pp", "/tmp/x.log"); err == nil {
		t.Fatal("expected error when tmux invocation fails, got nil")
	}
}

func TestHasSession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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

func TestHasSessionStatusDistinguishesTmuxErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()

	missing := NewClient(WithCommandFactory(func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "printf '%s' \"can't find session: missing\" >&2; exit 1")
	}))
	exists, err := missing.HasSessionStatus(ctx, "missing")
	if err != nil {
		t.Fatalf("missing HasSessionStatus error = %v, want nil", err)
	}
	if exists {
		t.Fatal("missing HasSessionStatus exists = true, want false")
	}

	noServer := NewClient(WithCommandFactory(func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "printf '%s' \"no server running on /tmp/tmux-501/default\" >&2; exit 1")
	}))
	exists, err = noServer.HasSessionStatus(ctx, "missing")
	if err != nil {
		t.Fatalf("no-server HasSessionStatus error = %v, want nil", err)
	}
	if exists {
		t.Fatal("no-server HasSessionStatus exists = true, want false")
	}

	unavailable := NewClient(WithCommandFactory(func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "printf '%s' \"error connecting to /tmp/tmux/default\" >&2; exit 1")
	}))
	if _, err := unavailable.HasSessionStatus(ctx, "maybe-live"); err == nil {
		t.Fatal("tmux command error returned nil error")
	}

	silentFailure := NewClient(WithCommandFactory(func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "false")
	}))
	if _, err := silentFailure.HasSessionStatus(ctx, "maybe-live"); err == nil {
		t.Fatal("empty-stderr tmux command failure returned nil error")
	}
}

func TestLineStillAtPromptIgnoresScrollback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	pane := strings.Join([]string{
		"❯ /boss-repair",
		"repair output",
		"❯",
	}, "\n")
	if lineStillAtPrompt(pane, "/boss-repair") {
		t.Fatal("scrollback prompt line reported as current input")
	}

	if !lineStillAtPrompt("repair output\n❯ /boss-repair\n", "/boss-repair") {
		t.Fatal("current prompt input was not detected")
	}
}

// TestLineStillAtPromptFindsInputBeneathChrome covers the real Claude Code
// footer layout: the live input box renders a prompt marker, and BELOW it
// Claude draws chrome (a separator rule, a "Model | cwd" line, a bypass-
// permissions hint) that has no prompt marker. The verifier must skip that
// chrome and read the bottom-most prompt-marker row as the live input, so a
// payload still sitting there reports true and a cleared/submitted input
// (empty "❯") reports false.
func TestLineStillAtPromptFindsInputBeneathChrome(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	const line = "write a haiku about autumn"
	const rule = "────────────────────────────────────────────────────────────────────────────────"

	// Filled input row beneath chrome → still at prompt (true).
	filled := strings.Join([]string{
		"❯ " + line,
		rule,
		"  Opus 4.8 (1M context) | /Users/dave/.bossanova/worktrees/bossanova/x",
		"  ⏵⏵ bypass permissions on",
	}, "\n")
	if !lineStillAtPrompt(filled, line) {
		t.Fatalf("filled input row beneath chrome must report still-at-prompt (true)")
	}

	// Cleared input row (empty "❯") beneath chrome after the response → submitted (false).
	submitted := strings.Join([]string{
		"⏺ here is your haiku response, fully rendered above",
		"❯",
		rule,
		"  Opus 4.8 (1M context) | /Users/dave/.bossanova/worktrees/bossanova/x",
		"  ⏵⏵ bypass permissions on",
	}, "\n")
	if lineStillAtPrompt(submitted, line) {
		t.Fatalf("cleared input row beneath chrome must report submitted (false)")
	}
}

// TestLineStillAtPromptStopsAtPostSubmitOutput covers the window right after a
// line is accepted: the agent echoes the submitted prompt row and starts
// streaming output below it before redrawing an empty input box. The verifier
// must treat that output as proof the payload left the prompt rather than
// scanning past it to the echoed prompt row and reporting it as still pending
// (which would time out waitForLineSubmission on an already-running command).
func TestLineStillAtPromptStopsAtPostSubmitOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	const line = "do the thing"

	// Assistant/tool bullet output below the echoed prompt → submitted (false).
	bulletOutput := strings.Join([]string{
		"❯ " + line,
		"⏺ working on it",
	}, "\n")
	if lineStillAtPrompt(bulletOutput, line) {
		t.Fatalf("prompt row above tool-bullet output must report submitted (false)")
	}

	// Tool-result branch output below the echoed prompt → submitted (false).
	resultOutput := strings.Join([]string{
		"❯ " + line,
		"⏺ Read(main.go)",
		"  ⎿  Read 42 lines",
	}, "\n")
	if lineStillAtPrompt(resultOutput, line) {
		t.Fatalf("prompt row above tool-result output must report submitted (false)")
	}

	// Thinking/working spinner below the echoed prompt — the window after the
	// line is accepted but before the first ⏺ renders — → submitted (false).
	for _, spinner := range []string{"·", "✻", "✽"} {
		spinnerOutput := strings.Join([]string{
			"❯ " + line,
			spinner + " Thinking… (esc to interrupt)",
		}, "\n")
		if lineStillAtPrompt(spinnerOutput, line) {
			t.Fatalf("prompt row above %q spinner must report submitted (false)", spinner)
		}
	}

	// Codex pane: "›" prompt marker with the Codex working bullet below it →
	// submitted (false). Mirrors plugins/bossd-plugin-codex/question.go grammar.
	codexOutput := strings.Join([]string{
		"› " + line,
		"• Working (3s • esc to interrupt)",
	}, "\n")
	if lineStillAtPrompt(codexOutput, line) {
		t.Fatalf("Codex prompt row above the working bullet must report submitted (false)")
	}
}

// TestLineStillAtPromptScansThroughCustomStatusline guards the inverse of the
// post-submit-output case: a custom statusline renders arbitrary rows under the
// live input box (here a truncated "model | cwd" line, a "PR #133" badge, and
// an "◉ xhigh · /effort" tag, mirroring TestSendPlan_CustomStatuslineReady).
// None are conversation output, so a payload still sitting in the input box
// must report still-at-prompt (true) and a cleared box must report submitted
// (false) — otherwise the verifier silently passes an unsubmitted cron prompt.
func TestLineStillAtPromptScansThroughCustomStatusline(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	const line = "implement the next ticket"
	const rule = "────────────────────────────────────────────────────────────────────────────────"
	statusline := []string{
		rule,
		"  Opus 4.7 (1M context) | /Users/dave/.bossanova/worktrees/bossanova/add-a-sc…",
		"  PR #133",
		"                                                             ◉ xhigh · /effort",
	}

	// Payload still in the input box beneath the statusline → still-at-prompt.
	filled := strings.Join(append([]string{"❯ " + line}, statusline...), "\n")
	if !lineStillAtPrompt(filled, line) {
		t.Fatalf("filled input beneath a custom statusline must report still-at-prompt (true)")
	}

	// Cleared input box beneath the statusline → submitted.
	cleared := strings.Join(append([]string{"❯"}, statusline...), "\n")
	if lineStillAtPrompt(cleared, line) {
		t.Fatalf("cleared input beneath a custom statusline must report submitted (false)")
	}
}

func TestKillSession_NotExist(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	tests := []struct {
		name   string
		stderr string
	}{
		{name: "missing session", stderr: "can't find session: test-session"},
		{name: "no tmux server", stderr: "no server running on /tmp/tmux-501/default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockCommandFactory{}
			c := NewClient(WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
				mock.calls = append(mock.calls, append([]string{name}, args...))
				// Simulate tmux error for non-existent session.
				// Both kill-session and has-session should fail.
				return exec.CommandContext(ctx, "sh", "-c", "printf '%s' \""+tt.stderr+"\" >&2; exit 1")
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
		})
	}
}

func TestKillSession_ReturnsLivenessErrorWhenFallbackCannotVerify(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		mock.calls = append(mock.calls, append([]string{name}, args...))
		return exec.CommandContext(ctx, "false")
	}))
	ctx := context.Background()

	err := c.KillSession(ctx, "test-session")
	if err == nil {
		t.Fatal("expected error when kill-session and fallback has-session fail without definite missing evidence")
	}
	if !strings.Contains(err.Error(), "verify tmux session") {
		t.Fatalf("error = %v, want liveness verification context", err)
	}

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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	tests := []struct {
		name           string
		repoID         string
		agentSessionID string
		expected       string
	}{
		{"both short", "abc", "xyz", "boss-abc-xyz"},
		{"both exact 8", "12345678", "abcdefgh", "boss-12345678-abcdefgh"},
		{"both 9 chars truncate", "123456789", "abcdefghi", "boss-12345678-abcdefgh"},
		{"both long truncate to 8", "0123456789abcdef", "fedcba9876543210", "boss-01234567-fedcba98"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ChatSessionName(tt.repoID, tt.agentSessionID)
			if got != tt.expected {
				t.Errorf("ChatSessionName(%q, %q) = %q, want %q",
					tt.repoID, tt.agentSessionID, got, tt.expected)
			}
		})
	}
}

// TestSessionName_NineCharBoundary covers the boundary mutation `len > 8`.
// At exactly 9 characters, the boundary mutates differently than at 8.
func TestSessionName_NineCharBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	fake := &sendPlanRecordingFactory{
		// Marker missing on first poll, present on second — exercises the
		// poll loop without waiting the full deadline.
		capturePaneOutputs: []string{
			"Welcome to Claude\n",
			"Welcome to Claude\n❯\n",
		},
	}
	c := NewClient(WithCommandFactory(fake.factory))

	// Multi-line payload so the bracketed-paste path (load-buffer → paste-buffer
	// → send-keys) is exercised; single-line payloads now deliver via send-keys
	// -l (see TestSendPlan_SingleLineUsesLiteralKeysMultilineUsesPaste).
	if err := c.sendPlan(context.Background(), "boss-test-sess", "do the thing\nand more", sendPlanOpts{
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

func TestSendPlan_CustomReadyMarker(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	fake := &sendPlanRecordingFactory{
		capturePaneOutputs: []string{
			"Welcome to Codex\n",
			"Welcome to Codex\n›\n",
		},
	}
	c := NewClient(WithCommandFactory(fake.factory))

	// Multi-line payload exercises the bracketed-paste path under a custom marker.
	if err := c.SendPlanWithReadyMarker(context.Background(), "boss-test-sess", "fix it\nplease", "›"); err != nil {
		t.Fatalf("SendPlanWithReadyMarker: unexpected error: %v", err)
	}

	calls := fake.callsCopy()
	// Tail is paste-buffer → send-keys Enter, then the submission-verification
	// capture-pane the wrapper now performs — so assert the paste-then-submit
	// order rather than that the final call is send-keys.
	pasteIdx, enterIdx := -1, -1
	for i, call := range calls {
		switch call.subcommand {
		case "paste-buffer":
			pasteIdx = i
		case "send-keys":
			if len(call.args) > 0 && call.args[len(call.args)-1] == "Enter" {
				enterIdx = i
			}
		}
	}
	if pasteIdx == -1 || enterIdx == -1 || pasteIdx > enterIdx {
		t.Fatalf("expected SendPlanWithReadyMarker to paste then submit, calls = %+v", calls)
	}
}

func TestSendLineWithReadyMarker_UsesLiteralKeysAndEnter(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	fake := &sendPlanRecordingFactory{
		capturePaneOutputs: []string{
			"Welcome to Codex\n›\n",
		},
	}
	c := NewClient(WithCommandFactory(fake.factory))

	if err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
		deadline:     time.Second,
		pollInterval: 5 * time.Millisecond,
		readyMarker:  "›",
	}); err != nil {
		t.Fatalf("SendLineWithReadyMarker: unexpected error: %v", err)
	}

	calls := fake.callsCopy()
	if len(calls) < 3 {
		t.Fatalf("expected capture-pane plus two send-keys calls, got %+v", calls)
	}
	for _, call := range calls {
		switch call.subcommand {
		case "load-buffer", "paste-buffer":
			t.Fatalf("SendLineWithReadyMarker must not use bracketed paste, saw %s", call.subcommand)
		}
	}

	textCall := calls[len(calls)-2]
	if textCall.subcommand != "send-keys" ||
		!equalSlices(textCall.args, []string{"-t", "boss-test-sess", "-l", "--", "$boss-repair"}) {
		t.Fatalf("literal send-keys call = %+v", textCall)
	}
	enterCall := calls[len(calls)-1]
	if enterCall.subcommand != "send-keys" ||
		!equalSlices(enterCall.args, []string{"-t", "boss-test-sess", "Enter"}) {
		t.Fatalf("Enter send-keys call = %+v", enterCall)
	}
}

// TestEscapeSendKeysLiteral verifies that only a trailing ";" is escaped for
// the literal send-keys path. tmux drops a single trailing ";" as a command
// terminator but preserves mid-string semicolons, backslashes, and other shell
// metacharacters verbatim, so escaping anything else would corrupt the payload.
func TestEscapeSendKeysLiteral(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no semicolon", "implement the next ticket", "implement the next ticket"},
		{"trailing semicolon", "explain ls;", `explain ls\;`},
		{"only a semicolon", ";", `\;`},
		{"mid-string semicolons untouched", "a;b ; c", "a;b ; c"},
		{"trailing of several escaped once", "a;b;", `a;b\;`},
		{"leading dash preserved", "-foo bar;", `-foo bar\;`},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := escapeSendKeysLiteral(tc.in); got != tc.want {
				t.Fatalf("escapeSendKeysLiteral(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSendLineWithReadyMarker_EscapesTrailingSemicolon confirms the escape is
// actually applied on the literal send-keys argv so a single-line prompt ending
// in ";" is delivered intact instead of being truncated by tmux's command lexer.
func TestSendLineWithReadyMarker_EscapesTrailingSemicolon(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	fake := &sendPlanRecordingFactory{
		capturePaneOutputs: []string{
			"Welcome to Codex\n›\n",
		},
	}
	c := NewClient(WithCommandFactory(fake.factory))

	if err := c.sendLine(context.Background(), "boss-test-sess", "explain ls;", sendPlanOpts{
		deadline:     time.Second,
		pollInterval: 5 * time.Millisecond,
		readyMarker:  "›",
	}); err != nil {
		t.Fatalf("SendLineWithReadyMarker: unexpected error: %v", err)
	}

	calls := fake.callsCopy()
	textCall := calls[len(calls)-2]
	if textCall.subcommand != "send-keys" ||
		!equalSlices(textCall.args, []string{"-t", "boss-test-sess", "-l", "--", `explain ls\;`}) {
		t.Fatalf("literal send-keys call = %+v", textCall)
	}
}

func TestSendLineWithReadyMarker_ReturnsErrorWhenCommandRemainsAtPrompt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	var calls []sendPlanCall
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		subcommand := ""
		if len(args) > 0 {
			subcommand = args[0]
		}
		calls = append(calls, sendPlanCall{subcommand: subcommand, args: append([]string(nil), args[1:]...)})
		switch subcommand {
		case "capture-pane":
			return exec.CommandContext(ctx, "printf", "%s", "ready\n› $boss-repair\n")
		default:
			return exec.CommandContext(ctx, "true")
		}
	}

	c := NewClient(WithCommandFactory(factory))
	err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 5 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	})

	if err == nil {
		t.Fatal("expected command submission verification error")
	}
	if !strings.Contains(err.Error(), "command was not submitted") {
		t.Fatalf("expected command submission error, got %v", err)
	}
	if !strings.Contains(err.Error(), "$boss-repair") {
		t.Fatalf("expected stuck command in error, got %v", err)
	}
	assertTmuxLiteralEnterOrder(t, calls, "$boss-repair")
}

func TestSendLineWithReadyMarker_SucceedsWhenCommandLeavesPrompt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	captureCount := 0
	var calls []sendPlanCall
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		subcommand := ""
		if len(args) > 0 {
			subcommand = args[0]
		}
		calls = append(calls, sendPlanCall{subcommand: subcommand, args: append([]string(nil), args[1:]...)})
		if subcommand == "capture-pane" {
			captureCount++
			if captureCount == 1 {
				return exec.CommandContext(ctx, "printf", "%s", "ready\n› \n")
			}
			return exec.CommandContext(ctx, "printf", "%s", "working\nRunning boss-repair\n")
		}
		return exec.CommandContext(ctx, "true")
	}

	c := NewClient(WithCommandFactory(factory))
	err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 20 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	})

	if err != nil {
		t.Fatalf("SendLineWithReadyMarker: unexpected error: %v", err)
	}
	assertTmuxLiteralEnterOrder(t, calls, "$boss-repair")
}

func assertTmuxLiteralEnterOrder(t *testing.T, calls []sendPlanCall, line string) {
	t.Helper()

	var literalIndex, enterIndex = -1, -1
	for i, call := range calls {
		if call.subcommand != "send-keys" {
			continue
		}
		args := strings.Join(call.args, "\x00")
		if strings.Contains(args, "-l\x00--\x00"+line) {
			literalIndex = i
		}
		if strings.HasSuffix(args, "\x00Enter") {
			enterIndex = i
		}
	}

	if literalIndex == -1 {
		t.Fatalf("missing literal send-keys call for %q in %#v", line, calls)
	}
	if enterIndex == -1 {
		t.Fatalf("missing Enter send-keys call in %#v", calls)
	}
	if literalIndex > enterIndex {
		t.Fatalf("literal send-keys must happen before Enter: literal=%d enter=%d", literalIndex, enterIndex)
	}
}

// A cron session is headless, so a single-line payload that gets typed (via
// send-keys -l) but never submitted must surface as an error rather than a
// silent success. sendPlan verifies the payload left the prompt when
// submitVerifyWait is set, as the public SendPlanWithReadyMarker wrapper does.
func TestSendPlan_ReturnsErrorWhenPayloadRemainsAtPrompt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		subcommand := ""
		if len(args) > 0 {
			subcommand = args[0]
		}
		if subcommand == "capture-pane" {
			return exec.CommandContext(ctx, "printf", "%s", "ready\n❯ /wc-merge-review headless\n")
		}
		return exec.CommandContext(ctx, "true")
	}

	c := NewClient(WithCommandFactory(factory))
	err := c.sendPlan(context.Background(), "boss-test-sess", "/wc-merge-review headless", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 5 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	})

	if err == nil {
		t.Fatal("expected paste submission verification error")
	}
	if !strings.Contains(err.Error(), "command was not submitted") {
		t.Fatalf("expected submission error, got %v", err)
	}
	if !strings.Contains(err.Error(), "/wc-merge-review headless") {
		t.Fatalf("expected stuck payload in error, got %v", err)
	}
}

// When the pasted payload leaves the prompt, verification passes.
func TestSendPlan_SucceedsWhenPayloadLeavesPrompt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	captureCount := 0
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		subcommand := ""
		if len(args) > 0 {
			subcommand = args[0]
		}
		if subcommand == "capture-pane" {
			captureCount++
			if captureCount == 1 {
				return exec.CommandContext(ctx, "printf", "%s", "ready\n❯ \n")
			}
			return exec.CommandContext(ctx, "printf", "%s", "working\nTriaging PRs\n")
		}
		return exec.CommandContext(ctx, "true")
	}

	c := NewClient(WithCommandFactory(factory))
	err := c.sendPlan(context.Background(), "boss-test-sess", "do the thing", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 20 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	})

	if err != nil {
		t.Fatalf("sendPlan: unexpected error: %v", err)
	}
}

// A multi-line plan cannot sit as one matchable prompt row, so submission
// verification is skipped structurally: only the ready-marker poll captures the
// pane, never a post-Enter verification capture. The fake keeps showing input at
// the prompt, so if verification ran it would issue a second capture-pane (and,
// against this still-at-prompt pane, eventually error) — neither happens.
func TestSendPlan_MultilinePayloadSkipsSubmissionVerification(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	captureCount := 0
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		subcommand := ""
		if len(args) > 0 {
			subcommand = args[0]
		}
		if subcommand == "capture-pane" {
			captureCount++
			return exec.CommandContext(ctx, "printf", "%s", "ready\n❯ still showing input\n")
		}
		return exec.CommandContext(ctx, "true")
	}

	c := NewClient(WithCommandFactory(factory))
	err := c.sendPlan(context.Background(), "boss-test-sess", "/cmd\nwith extra notes", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 5 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	})

	if err != nil {
		t.Fatalf("multi-line plan must skip verification, got error: %v", err)
	}
	// Exactly one capture-pane: the ready-marker poll. A second would mean
	// verification ran on a multi-line payload.
	if captureCount != 1 {
		t.Fatalf("capture-pane count = %d, want 1 (verification must be skipped for multi-line)", captureCount)
	}
}

// TestSendPlan_SingleLineUsesLiteralKeysMultilineUsesPaste asserts the
// delivery split: a non-empty single-line payload is typed via send-keys -l
// (literal keystrokes, which Claude's TUI reliably submits on Enter) and never
// through bracketed paste, while a multi-line payload still uses paste-buffer so
// intermediate newlines aren't treated as premature submits.
func TestSendPlan_SingleLineUsesLiteralKeysMultilineUsesPaste(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}

	t.Run("single-line uses send-keys -l not paste-buffer", func(t *testing.T) {
		fake := &sendPlanRecordingFactory{
			capturePaneOutputs: []string{
				"Welcome to Claude\n❯\n", // ready marker present
				"working\nrunning now\n", // verification: payload left the prompt
			},
		}
		c := NewClient(WithCommandFactory(fake.factory))
		if err := c.sendPlan(context.Background(), "boss-test-sess", "do the thing", sendPlanOpts{
			deadline:         time.Second,
			pollInterval:     5 * time.Millisecond,
			submitVerifyWait: 50 * time.Millisecond,
			submitVerifyTick: time.Millisecond,
		}); err != nil {
			t.Fatalf("sendPlan single-line: unexpected error: %v", err)
		}

		calls := fake.callsCopy()
		sawLiteral := false
		for _, call := range calls {
			if call.subcommand == "load-buffer" || call.subcommand == "paste-buffer" {
				t.Fatalf("single-line payload must not use bracketed paste, saw %s", call.subcommand)
			}
			if call.subcommand == "send-keys" &&
				equalSlices(call.args, []string{"-t", "boss-test-sess", "-l", "--", "do the thing"}) {
				sawLiteral = true
			}
		}
		if !sawLiteral {
			t.Fatalf("expected send-keys -l literal delivery, calls = %+v", calls)
		}
	})

	t.Run("multi-line uses paste-buffer not send-keys -l", func(t *testing.T) {
		fake := &sendPlanRecordingFactory{
			capturePaneOutputs: []string{"Welcome to Claude\n❯\n"},
		}
		c := NewClient(WithCommandFactory(fake.factory))
		if err := c.sendPlan(context.Background(), "boss-test-sess", "line one\nline two", sendPlanOpts{
			deadline:     time.Second,
			pollInterval: 5 * time.Millisecond,
		}); err != nil {
			t.Fatalf("sendPlan multi-line: unexpected error: %v", err)
		}

		calls := fake.callsCopy()
		sawPaste := false
		for _, call := range calls {
			if call.subcommand == "paste-buffer" {
				sawPaste = true
			}
			if call.subcommand == "send-keys" {
				for _, a := range call.args {
					if a == "-l" {
						t.Fatalf("multi-line payload must not use send-keys -l, calls = %+v", calls)
					}
				}
			}
		}
		if !sawPaste {
			t.Fatalf("multi-line payload must use paste-buffer, calls = %+v", calls)
		}
	})

	t.Run("single-line with surrounding whitespace uses send-keys -l with trimmed text", func(t *testing.T) {
		fake := &sendPlanRecordingFactory{
			capturePaneOutputs: []string{
				"Welcome to Claude\n❯\n", // ready marker present
				"working\nrunning now\n", // verification: payload left the prompt
			},
		}
		c := NewClient(WithCommandFactory(fake.factory))
		// A single logical line carrying a trailing newline must still take the
		// reliable literal path (typing the trimmed text), not slip into paste —
		// the bracketed-paste failure mode this fix exists to avoid.
		if err := c.sendPlan(context.Background(), "boss-test-sess", "do the thing\n", sendPlanOpts{
			deadline:         time.Second,
			pollInterval:     5 * time.Millisecond,
			submitVerifyWait: 50 * time.Millisecond,
			submitVerifyTick: time.Millisecond,
		}); err != nil {
			t.Fatalf("sendPlan single-line-with-newline: unexpected error: %v", err)
		}

		calls := fake.callsCopy()
		sawLiteral := false
		for _, call := range calls {
			if call.subcommand == "load-buffer" || call.subcommand == "paste-buffer" {
				t.Fatalf("trailing-newline single-line payload must not use bracketed paste, saw %s", call.subcommand)
			}
			if call.subcommand == "send-keys" &&
				equalSlices(call.args, []string{"-t", "boss-test-sess", "-l", "--", "do the thing"}) {
				sawLiteral = true
			}
		}
		if !sawLiteral {
			t.Fatalf("expected send-keys -l with trimmed text, calls = %+v", calls)
		}
	})

	t.Run("leading-dash payload delivered after the -- terminator", func(t *testing.T) {
		fake := &sendPlanRecordingFactory{
			capturePaneOutputs: []string{
				"Welcome to Claude\n❯\n", // ready marker present
				"working\nrunning now\n", // verification: payload left the prompt
			},
		}
		c := NewClient(WithCommandFactory(fake.factory))
		// A payload beginning with "-" must be delivered as a key argument, not
		// parsed by tmux as a send-keys flag. The "--" option terminator must
		// precede it, and the literal text must follow as a single argument.
		const dashPayload = "-n print this without a newline"
		if err := c.sendPlan(context.Background(), "boss-test-sess", dashPayload, sendPlanOpts{
			deadline:         time.Second,
			pollInterval:     5 * time.Millisecond,
			submitVerifyWait: 50 * time.Millisecond,
			submitVerifyTick: time.Millisecond,
		}); err != nil {
			t.Fatalf("sendPlan leading-dash: unexpected error: %v", err)
		}

		calls := fake.callsCopy()
		sawLiteral := false
		for _, call := range calls {
			if call.subcommand == "load-buffer" || call.subcommand == "paste-buffer" {
				t.Fatalf("leading-dash single-line payload must not use bracketed paste, saw %s", call.subcommand)
			}
			if call.subcommand == "send-keys" &&
				equalSlices(call.args, []string{"-t", "boss-test-sess", "-l", "--", dashPayload}) {
				sawLiteral = true
			}
		}
		if !sawLiteral {
			t.Fatalf("expected send-keys -l -- <payload> for a leading-dash prompt, calls = %+v", calls)
		}
	})
}

// TestSendPlan_ReadyMarkerNeverAppears_Errors verifies the deadline path:
// if capture-pane never returns the marker, SendPlan returns an error
// without trying load-buffer / paste-buffer / send-keys.
func TestSendPlan_ReadyMarkerNeverAppears_Errors(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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

	// Multi-line payload so the paste subcommands run; the assertion below
	// confirms the marker poll resolved against the custom statusline.
	if err := c.sendPlan(context.Background(), "boss-test-sess", "the plan\nstep two", sendPlanOpts{
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	fake := &sendPlanRecordingFactory{
		capturePaneOutputs: []string{"Welcome to Claude\n❯\n"},
		failOnSubcommand: map[string]int{
			"paste-buffer": 0, // first paste-buffer call fails
		},
	}
	c := NewClient(WithCommandFactory(fake.factory))

	// Multi-line payload so delivery goes through paste-buffer (single-line now
	// uses send-keys -l), letting the paste-buffer failure injection fire.
	err := c.sendPlan(context.Background(), "boss-test-sess", "plan body\nmore", sendPlanOpts{
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
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
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	tmpFile := filepath.Join(t.TempDir(), "load-buffer-stdin")

	captureCalls := atomic.Int32{}
	c := NewClient(WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if len(args) > 0 && args[0] == "capture-pane" {
			captureCalls.Add(1)
			return exec.CommandContext(ctx, "printf", "%s", "Welcome\n❯\n")
		}
		if len(args) > 0 && args[0] == "load-buffer" {
			// Drain stdin into tmpFile so the test can assert on it.
			return exec.CommandContext(ctx, "sh", "-c", "cat > \"$1\"", "sh", tmpFile)
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

// TestSendMessage_IssuesBracketedPasteSequence verifies SendMessage issues
// load-buffer, paste-buffer, send-keys in order with no capture-pane poll.
func TestSendMessage_IssuesBracketedPasteSequence(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	fake := &sendPlanRecordingFactory{}
	c := NewClient(WithCommandFactory(fake.factory))

	if err := c.SendMessage(context.Background(), "boss-test-sess", "hello world"); err != nil {
		t.Fatalf("SendMessage: unexpected error: %v", err)
	}

	calls := fake.callsCopy()
	subcommands := make([]string, len(calls))
	for i, call := range calls {
		subcommands[i] = call.subcommand
	}

	want := []string{"load-buffer", "paste-buffer", "send-keys"}
	if len(calls) != len(want) {
		t.Fatalf("expected exactly %d calls, got %d: %v", len(want), len(calls), subcommands)
	}
	if !equalSlices(subcommands, want) {
		t.Errorf("subcommands = %v, want %v", subcommands, want)
	}

	if !equalSlices(calls[0].args, []string{"-"}) {
		t.Errorf("load-buffer args = %v, want [-]", calls[0].args)
	}
	if !equalSlices(calls[1].args, []string{"-d", "-p", "-t", "boss-test-sess"}) {
		t.Errorf("paste-buffer args = %v, want [-d -p -t boss-test-sess]", calls[1].args)
	}
	if !equalSlices(calls[2].args, []string{"-t", "boss-test-sess", "Enter"}) {
		t.Errorf("send-keys args = %v, want [-t boss-test-sess Enter]", calls[2].args)
	}
}

func TestSendMessage_EmptySessionName_Error(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	c := NewClient(WithCommandFactory((&mockCommandFactory{}).factory))
	err := c.SendMessage(context.Background(), "", "hello")
	if err == nil {
		t.Fatal("expected error for empty session name")
	}
}
