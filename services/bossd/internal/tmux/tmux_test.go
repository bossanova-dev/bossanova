package tmux

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
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
				"-e", "TERM=xterm-256color",
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
				"-e", "TERM=xterm-256color",
				"sh", "-c", "echo hello",
			},
		},
		{
			// Env vars are emitted as sorted `-e KEY=VALUE` flags before the
			// command so the launched process (e.g. a cron agent) inherits them.
			// A normalized TERM is appended last (termnorm falls back to
			// xterm-256color when the env carries no TERM) so tmux always has a
			// resolvable terminal.
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
				"-e", "TERM=xterm-256color",
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

// TestNewSession_DaemonIDStamp covers the ownership stamp the orphaned-tmux
// reaper reads back with `show-environment` (BOS-846). It lives on the client
// rather than at each call site so every pane bossd creates carries it — a
// single unstamped spawn path would produce panes the reaper cannot attribute.
func TestNewSession_DaemonIDStamp(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	tests := []struct {
		name     string
		daemonID string
		env      map[string]string
		expected []string
	}{
		{
			name:     "stamped alongside no other env",
			daemonID: "daemon-abc",
			expected: []string{
				"tmux", "new-session", "-d", "-s", "stamp-sess",
				"-c", "/tmp/wt", "-x", "200", "-y", "50",
				"-e", "BOSS_DAEMON_ID=daemon-abc",
				"-e", "TERM=xterm-256color",
				"claude",
			},
		},
		{
			// Sorted with the rest, not appended: the argv must stay
			// deterministic whatever the caller's map iteration order.
			name:     "sorted in with caller env",
			daemonID: "daemon-abc",
			env:      map[string]string{"BOSS_CRON": "true", "AAA_FIRST": "1"},
			expected: []string{
				"tmux", "new-session", "-d", "-s", "stamp-sess",
				"-c", "/tmp/wt", "-x", "200", "-y", "50",
				"-e", "AAA_FIRST=1", "-e", "BOSS_CRON=true",
				"-e", "BOSS_DAEMON_ID=daemon-abc",
				"-e", "TERM=xterm-256color",
				"claude",
			},
		},
		{
			// The client's identity is authoritative. A caller that passes its
			// own value must not be able to make a pane look like it belongs to
			// a different daemon, which would exempt it from reaping forever.
			name:     "client value wins over a caller-supplied key",
			daemonID: "daemon-abc",
			env:      map[string]string{"BOSS_DAEMON_ID": "someone-else"},
			expected: []string{
				"tmux", "new-session", "-d", "-s", "stamp-sess",
				"-c", "/tmp/wt", "-x", "200", "-y", "50",
				"-e", "BOSS_DAEMON_ID=daemon-abc",
				"-e", "TERM=xterm-256color",
				"claude",
			},
		},
		{
			// No identity resolved: emit nothing rather than an empty stamp,
			// which would read as "owned by the daemon whose id is the empty
			// string" instead of as unstamped.
			name:     "empty daemon id stamps nothing",
			daemonID: "",
			expected: []string{
				"tmux", "new-session", "-d", "-s", "stamp-sess",
				"-c", "/tmp/wt", "-x", "200", "-y", "50",
				"-e", "TERM=xterm-256color",
				"claude",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockCommandFactory{}
			c := NewClient(WithCommandFactory(mock.factory), WithDaemonID(tt.daemonID))

			opts := NewSessionOpts{
				Name:    "stamp-sess",
				WorkDir: "/tmp/wt",
				Command: []string{"claude"},
				Env:     tt.env,
			}
			if err := c.NewSession(context.Background(), opts); err != nil {
				t.Fatalf("NewSession() error = %v", err)
			}
			if got := mock.calls[0]; !slices.Equal(got, tt.expected) {
				t.Fatalf("argv = %v, want %v", got, tt.expected)
			}
			// The caller's map is shared state; mutating it would leak the
			// stamp into whatever else the caller reuses it for.
			if tt.env != nil {
				if _, ok := tt.env["BOSS_DAEMON_ID"]; ok != (tt.name == "client value wins over a caller-supplied key") {
					t.Errorf("caller Env map was mutated: %v", tt.env)
				}
			}
		})
	}
}

// TestNewSession_NoDaemonIDOptionIsUnstamped pins that a client built without
// the option behaves exactly as before, so every existing caller and test keeps
// its argv.
func TestNewSession_NoDaemonIDOptionIsUnstamped(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(mock.factory))

	if err := c.NewSession(context.Background(), NewSessionOpts{
		Name:    "plain",
		WorkDir: "/tmp/wt",
		Command: []string{"claude"},
	}); err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	for _, arg := range mock.calls[0] {
		if strings.Contains(arg, "BOSS_DAEMON_ID") {
			t.Fatalf("unstamped client emitted %q", arg)
		}
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

// TestNewSession_RemainOnExit verifies that RemainOnExit arms
// `set-option -t <name> remain-on-exit on` after session creation (BOS-477),
// and that the option is omitted when the flag is unset.
func TestNewSession_RemainOnExit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	remainOn := []string{"tmux", "set-option", "-t", "boss-test-1234", "remain-on-exit", "on"}

	t.Run("armed when set", func(t *testing.T) {
		mock := &mockCommandFactory{}
		c := NewClient(WithCommandFactory(mock.factory))
		if err := c.NewSession(context.Background(), NewSessionOpts{
			Name:         "boss-test-1234",
			WorkDir:      "/tmp",
			Command:      []string{"claude"},
			RemainOnExit: true,
		}); err != nil {
			t.Fatalf("NewSession failed: %v", err)
		}
		found := false
		for _, call := range mock.calls {
			if equalSlices(call, remainOn) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected remain-on-exit on call, got calls: %v", mock.calls)
		}
	})

	t.Run("omitted when unset", func(t *testing.T) {
		mock := &mockCommandFactory{}
		c := NewClient(WithCommandFactory(mock.factory))
		if err := c.NewSession(context.Background(), NewSessionOpts{
			Name:    "boss-test-1234",
			WorkDir: "/tmp",
			Command: []string{"claude"},
		}); err != nil {
			t.Fatalf("NewSession failed: %v", err)
		}
		for _, call := range mock.calls {
			if len(call) >= 5 && call[1] == "set-option" && call[4] == "remain-on-exit" {
				t.Errorf("did not expect remain-on-exit call when flag unset, got: %v", call)
			}
		}
	})
}

// TestPaneDead covers the pane_dead probe: "1" → dead, "0" → alive, and a tmux
// command failure surfaces as (false, err) rather than a false "dead" verdict.
func TestPaneDead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()

	t.Run("dead pane reports true", func(t *testing.T) {
		mock := &captureOutputFactory{output: "1\n"}
		c := NewClient(WithCommandFactory(mock.factory))
		dead, err := c.PaneDead(ctx, "boss-dead")
		if err != nil {
			t.Fatalf("PaneDead: %v", err)
		}
		if !dead {
			t.Fatal("expected dead=true for pane_dead=1")
		}
		want := []string{"tmux", "display-message", "-p", "-t", "boss-dead", "#{pane_dead}"}
		if last := mock.calls[len(mock.calls)-1]; !equalSlices(last, want) {
			t.Errorf("PaneDead argv = %v, want %v", last, want)
		}
	})

	t.Run("alive pane reports false", func(t *testing.T) {
		mock := &captureOutputFactory{output: "0\n"}
		c := NewClient(WithCommandFactory(mock.factory))
		dead, err := c.PaneDead(ctx, "boss-alive")
		if err != nil {
			t.Fatalf("PaneDead: %v", err)
		}
		if dead {
			t.Fatal("expected dead=false for pane_dead=0")
		}
	})

	t.Run("command failure returns error", func(t *testing.T) {
		c := NewClient(WithCommandFactory(func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "false")
		}))
		dead, err := c.PaneDead(ctx, "missing")
		if err == nil {
			t.Fatal("expected error on command failure, got nil")
		}
		if dead {
			t.Fatal("expected dead=false on command failure")
		}
	})
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

// clientListFactory is a CommandFactory that answers `tmux list-clients` with
// the given client names on stdout and succeeds at everything else, recording
// every argv. failRefreshFor names clients whose refresh-client must fail, so a
// test can prove one dead client does not skip the clients behind it.
type clientListFactory struct {
	clients        []string
	failRefreshFor map[string]bool
	calls          [][]string
}

func (f *clientListFactory) factory(ctx context.Context, name string, args ...string) *exec.Cmd {
	f.calls = append(f.calls, append([]string{name}, args...))
	if len(args) > 0 && args[0] == "list-clients" {
		return exec.CommandContext(ctx, "printf", "%s", strings.Join(f.clients, "\n"))
	}
	if len(args) > 2 && args[0] == "refresh-client" && f.failRefreshFor[args[2]] {
		return exec.CommandContext(ctx, "false")
	}
	return exec.CommandContext(ctx, "true")
}

// refreshTargets returns the -t argument of every refresh-client call recorded.
func (f *clientListFactory) refreshTargets() []string {
	var got []string
	for _, call := range f.calls {
		if len(call) > 3 && call[1] == "refresh-client" {
			got = append(got, call[3])
		}
	}
	return got
}

// TestRefreshClient verifies the wrapper resolves the session's CLIENTS and
// refreshes each one by client name. `refresh-client -t` takes a client, not a
// session: the previous implementation passed the session name straight to it,
// which always failed with "can't find client", so the web-tmux-attach RESYNC
// repaint never actually fired. This test is the regression gate for that — it
// fails if the target reverts to the session name.
func TestRefreshClient(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	mock := &clientListFactory{clients: []string{"/dev/ttys008", "/dev/ttys011"}}
	c := NewClient(WithCommandFactory(mock.factory))

	if err := c.RefreshClient(context.Background(), "boss-test-sess"); err != nil {
		t.Fatalf("RefreshClient failed: %v", err)
	}

	wantList := []string{"tmux", "list-clients", "-t", "boss-test-sess", "-F", "#{client_name}"}
	if len(mock.calls) == 0 || !equalSlices(mock.calls[0], wantList) {
		t.Fatalf("expected first call %v, got %v", wantList, mock.calls)
	}
	got := mock.refreshTargets()
	want := []string{"/dev/ttys008", "/dev/ttys011"}
	if !equalSlices(got, want) {
		t.Errorf("refresh-client targets: expected %v, got %v", want, got)
	}
	if slices.Contains(got, "boss-test-sess") {
		t.Error("refresh-client was targeted at the SESSION name; it takes a client name")
	}
}

// TestRefreshClient_NoClientsIsNoOp verifies a session with nothing attached
// refreshes nothing and reports success. That is the normal state of a headless
// or cron session — there is no viewer to repaint — so it must not surface as a
// failure on a fire-and-forget call.
func TestRefreshClient_NoClientsIsNoOp(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	mock := &clientListFactory{}
	c := NewClient(WithCommandFactory(mock.factory))

	if err := c.RefreshClient(context.Background(), "boss-test-sess"); err != nil {
		t.Fatalf("expected nil error for a session with no clients, got %v", err)
	}
	if targets := mock.refreshTargets(); len(targets) != 0 {
		t.Errorf("expected no refresh-client calls, got %v", targets)
	}
}

// TestRefreshClient_RefreshesEveryClientDespiteFailure verifies a client that
// fails to refresh — the benign race where it detached between the list and the
// refresh — does not skip the clients behind it, and that the failure is still
// reported rather than swallowed.
func TestRefreshClient_RefreshesEveryClientDespiteFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	mock := &clientListFactory{
		clients:        []string{"/dev/ttys008", "/dev/ttys011", "/dev/ttys013"},
		failRefreshFor: map[string]bool{"/dev/ttys011": true},
	}
	c := NewClient(WithCommandFactory(mock.factory))

	err := c.RefreshClient(context.Background(), "boss-test-sess")
	if err == nil {
		t.Fatal("expected the failing client to surface as an error, got nil")
	}
	got := mock.refreshTargets()
	want := []string{"/dev/ttys008", "/dev/ttys011", "/dev/ttys013"}
	if !equalSlices(got, want) {
		t.Errorf("every client must still be refreshed: expected %v, got %v", want, got)
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

// TestRefreshClient_Error verifies a failure of the client LOOKUP surfaces as
// an error rather than being swallowed — the `false` factory here fails the
// list-clients call, before any refresh is attempted. Catches mutations like
// err != nil → err == nil that would silently break the resync repaint flow.
// The refresh half of that contract is covered by
// TestRefreshClient_RefreshesEveryClientDespiteFailure.
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

func TestShowEnv(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()

	t.Run("returns value when the key is set", func(t *testing.T) {
		c := NewClient(WithCommandFactory(func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c",
				"printf '%s\\n' 'ANTHROPIC_BASE_URL=http://127.0.0.1:44127/s/tok'")
		}))
		got, ok := c.ShowEnv(ctx, "boss-sess", "ANTHROPIC_BASE_URL")
		if !ok {
			t.Fatal("ShowEnv ok = false, want true for a set key")
		}
		if got != "http://127.0.0.1:44127/s/tok" {
			t.Fatalf("ShowEnv value = %q, want the baked URL", got)
		}
	})

	t.Run("passes the right argv", func(t *testing.T) {
		mock := &mockCommandFactory{}
		c := NewClient(WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
			mock.calls = append(mock.calls, append([]string{name}, args...))
			return exec.CommandContext(ctx, "sh", "-c", "printf '%s\\n' 'ANTHROPIC_BASE_URL=v'")
		}))
		_, _ = c.ShowEnv(ctx, "boss-sess", "ANTHROPIC_BASE_URL")
		want := []string{"tmux", "show-environment", "-t", "boss-sess", "ANTHROPIC_BASE_URL"}
		if len(mock.calls) != 1 || !equalSlices(mock.calls[0], want) {
			t.Fatalf("ShowEnv argv = %v, want %v", mock.calls, want)
		}
	})

	t.Run("absent key returns false", func(t *testing.T) {
		// tmux errors (exit 1, stderr) on an unknown variable.
		c := NewClient(WithCommandFactory(func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c",
				"printf '%s' 'unknown variable: ANTHROPIC_BASE_URL' >&2; exit 1")
		}))
		if got, ok := c.ShowEnv(ctx, "boss-sess", "ANTHROPIC_BASE_URL"); ok || got != "" {
			t.Fatalf("ShowEnv(absent) = %q,%v, want \"\",false", got, ok)
		}
	})

	t.Run("removal marker returns false", func(t *testing.T) {
		// A var flagged for removal in the session env prints "-KEY", no value.
		c := NewClient(WithCommandFactory(func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c", "printf '%s\\n' '-ANTHROPIC_BASE_URL'")
		}))
		if got, ok := c.ShowEnv(ctx, "boss-sess", "ANTHROPIC_BASE_URL"); ok || got != "" {
			t.Fatalf("ShowEnv(removal) = %q,%v, want \"\",false", got, ok)
		}
	})

	t.Run("empty value returns empty string ok", func(t *testing.T) {
		c := NewClient(WithCommandFactory(func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c", "printf '%s\\n' 'ANTHROPIC_BASE_URL='")
		}))
		got, ok := c.ShowEnv(ctx, "boss-sess", "ANTHROPIC_BASE_URL")
		if !ok || got != "" {
			t.Fatalf("ShowEnv(empty) = %q,%v, want \"\",true", got, ok)
		}
	})
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
// (which would time out waitForSubmission on an already-running command).
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

// TestLineStillAtPromptWrappedSingleLine is the BOS-489 regression guard: a
// single-line payload wider than the pane wraps across rows, so the live input
// box never sits as one matchable row. The historical text-match
// (strings.Contains(oneRow, fullLine)) could therefore never match a wrapped
// payload, reported it as "left the prompt", and suppressed the swallowed-Enter
// retry — a false delivered:true. The verifier must instead read the wrapping-
// robust composer signal: a still-visible (non-empty) input box is still-at-prompt,
// while a cleared composer / agent activity below the marker is submitted.
func TestLineStillAtPromptWrappedSingleLine(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}

	// A payload comfortably wider than a pane row, so the input box wraps.
	longLine := strings.Repeat("verify the wide single-line payload wraps and still resolves ", 3)
	const paneWidth = 78
	markerRow := "❯ " + longLine[:paneWidth]
	continuation := longLine[paneWidth:]
	const rule = "────────────────────────────────────────────────────────────────────────────────"

	// Fixture sanity: the marker row genuinely cannot contain the whole payload —
	// the exact condition that produced the BOS-489 false positive.
	if strings.Contains(strings.TrimSpace(markerRow), longLine) {
		t.Fatal("invalid fixture: marker row unexpectedly contains the full wrapped line")
	}

	// Wrapped payload still sitting in the composer (input box non-empty, no agent
	// activity), footer chrome below → still-at-prompt (true). The old text-match
	// wrongly reported this submitted.
	pending := strings.Join([]string{
		markerRow,
		continuation,
		rule,
		"  Opus 4.8 (1M context) | /Users/dave/.bossanova/worktrees/bossanova/x",
	}, "\n")
	if !lineStillAtPrompt(pending, longLine) {
		t.Fatalf("wrapped single line still in the composer must report still-at-prompt (true)")
	}

	// Same wrapped payload, now actually submitted: composer cleared to a bare
	// glyph → submitted (false).
	cleared := strings.Join([]string{
		"⏺ working on the wide payload above",
		"❯",
		rule,
		"  Opus 4.8 (1M context) | /Users/dave/.bossanova/worktrees/bossanova/x",
	}, "\n")
	if lineStillAtPrompt(cleared, longLine) {
		t.Fatalf("cleared composer after a wide single line must report submitted (false)")
	}

	// Submitted with the wrapped prompt still echoed but agent activity rendered
	// below it → submitted (false).
	working := strings.Join([]string{
		markerRow,
		continuation,
		"⏺ working",
	}, "\n")
	if lineStillAtPrompt(working, longLine) {
		t.Fatalf("agent activity below a wrapped echoed prompt must report submitted (false)")
	}

	// Agent working full-screen with no input box drawn at all → submitted (false):
	// the single-line no-marker early return must survive the wrap-robust rewrite.
	noMarker := "⏺ working on the wide payload\n  ⎿  still going\n"
	if lineStillAtPrompt(noMarker, longLine) {
		t.Fatalf("agent working with no input box must report submitted (false)")
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

func TestPanePID_ReturnsFirstPanePID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	mock := &captureOutputFactory{output: "97664\n99277\n"}
	c := NewClient(WithCommandFactory(mock.factory))

	pid, err := c.PanePID(context.Background(), "boss-3db0b79b-3e22ae16")
	if err != nil {
		t.Fatalf("PanePID: %v", err)
	}
	if pid != 97664 {
		t.Errorf("PanePID = %d, want first pane pid 97664", pid)
	}
	last := mock.calls[len(mock.calls)-1]
	want := []string{"tmux", "list-panes", "-t", "boss-3db0b79b-3e22ae16", "-F", "#{pane_pid}"}
	if !equalSlices(last, want) {
		t.Errorf("PanePID argv = %v, want %v", last, want)
	}
}

func TestPanePID_ErrorOnCommandFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	c := NewClient(WithCommandFactory(func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "false")
	}))
	if _, err := c.PanePID(context.Background(), "missing"); err == nil {
		t.Fatal("PanePID: expected error on command failure, got nil")
	}
}

func TestPanePID_ErrorOnEmptyOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	mock := &captureOutputFactory{output: ""}
	c := NewClient(WithCommandFactory(mock.factory))
	if _, err := c.PanePID(context.Background(), "boss-empty"); err == nil {
		t.Fatal("PanePID: expected error on empty output, got nil")
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

	// failCapturePaneFrom, when non-nil, makes EVERY capture-pane call at or
	// after that 0-based occurrence exit non-zero — 0 fails every occurrence,
	// 1 lets the first succeed and fails all the rest. It is consulted before
	// the indexed capturePaneOutputs lookup below, so a failing occurrence
	// consumes its index without producing stdout.
	//
	// Unlike failOnSubcommand (which returns `false`, whose stderr is empty),
	// the failing command writes captureFailStderr to STDERR. That matters:
	// CapturePane runs cmd.Output() with no Stderr set, so os/exec parks the
	// child's stderr on (*exec.ExitError).Stderr, and an empty stderr cannot
	// exercise the errors.As recovery in captureErrText.
	failCapturePaneFrom *int

	// captureFailStderr is the text a failed capture-pane writes to stderr.
	// Empty → defaultCaptureFailStderr, a real tmux message.
	captureFailStderr string

	// failWithStderr maps a subcommand to the stderr text EVERY invocation of
	// it writes before exiting non-zero. Unlike failOnSubcommand (one indexed
	// occurrence, empty stderr) this is unconditional and carries tmux's own
	// words, which is what a caller reading stderr — HasSessionStatus — needs
	// in order to tell "no such session" from "something else went wrong".
	// An empty string is a MEANINGFUL value here: it models the failure that
	// says nothing, which HasSessionStatus must treat as indeterminate.
	failWithStderr map[string]string

	// capturePaneRules, when non-empty, selects capture-pane stdout by ELAPSED
	// TIME since this factory's first capture-pane call rather than by call
	// index, and takes precedence over capturePaneOutputs. Rules must be in
	// ascending `after` order; the last one whose `after` has elapsed wins, and
	// output is empty until the first rule is due.
	//
	// Position cannot express what the retry tests need. "The marker appears
	// after the first attempt's budget expires" is a statement about the clock,
	// and how many capture-pane calls an attempt fits into its budget is a
	// property of the machine, not of the test.
	capturePaneRules []capturePaneRule
	firstCaptureAt   time.Time
}

// capturePaneRule is one entry in capturePaneRules: from `after` onwards (until
// a later rule is due) capture-pane emits `output`.
type capturePaneRule struct {
	after  time.Duration
	output string
}

// defaultCaptureFailStderr is what tmux itself prints when the target session
// has gone — the production shape of the failure this models.
const defaultCaptureFailStderr = "can't find session: boss-test-sess"

// failingCaptureCmd returns a command that writes stderr and exits non-zero,
// mirroring how tmux reports a missing session. The message is passed as $0
// rather than interpolated into the script so no test string can be read as
// shell syntax.
func failingCaptureCmd(ctx context.Context, stderr string) *exec.Cmd {
	if stderr == "" {
		stderr = defaultCaptureFailStderr
	}
	return failingCmdWithStderr(ctx, stderr)
}

// failingCmdWithStderr is failingCaptureCmd without the defaulting, so a caller
// can model a failure whose stderr really is empty.
func failingCmdWithStderr(ctx context.Context, stderr string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", `printf '%s\n' "$0" >&2; exit 1`, stderr)
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
	if stderr, ok := f.failWithStderr[subcommand]; ok {
		return failingCmdWithStderr(ctx, stderr)
	}

	switch subcommand {
	case "capture-pane":
		idx := int(f.captureCallIdx.Add(1)) - 1
		if f.failCapturePaneFrom != nil && idx >= *f.failCapturePaneFrom {
			return failingCaptureCmd(ctx, f.captureFailStderr)
		}
		if len(f.capturePaneRules) > 0 {
			if f.firstCaptureAt.IsZero() {
				f.firstCaptureAt = time.Now()
			}
			elapsed := time.Since(f.firstCaptureAt)
			ruled := ""
			for _, r := range f.capturePaneRules {
				if elapsed >= r.after {
					ruled = r.output
				}
			}
			return exec.CommandContext(ctx, "printf", "%s", ruled)
		}
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
	if strings.Contains(err.Error(), "$boss-repair") {
		t.Fatalf("submission error leaked command payload: %v", err)
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
	if strings.Contains(err.Error(), "/wc-merge-review headless") {
		t.Fatalf("submission error leaked payload: %v", err)
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

// A multi-line plan that loads into the composer but never executes (the paste
// swallowed the Enter) must surface as an error in a headless cron session — the
// BOS-228 fix. A multi-line payload cannot be matched as one prompt row, so the
// verifier reads the shape-aware multiLineSubmitted signal instead; a pane that
// keeps showing the payload at the composer (no agent activity, non-empty input
// box) means "still pending", so after an Enter retry against the same pane the
// verifier errors loudly rather than reporting a silent success.
func TestSendPlan_MultilinePayloadNeverSubmittedErrors(t *testing.T) {
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
			return exec.CommandContext(ctx, "printf", "%s", "ready\n❯ /cmd\nwith extra notes\n")
		}
		return exec.CommandContext(ctx, "true")
	}

	c := NewClient(WithCommandFactory(factory))
	err := c.sendPlan(context.Background(), "boss-test-sess", "/cmd\nwith extra notes", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 5 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	})

	if err == nil {
		t.Fatal("expected multi-line submission verification error, got nil")
	}
	if !strings.Contains(err.Error(), "not submitted") {
		t.Fatalf("expected not-submitted error, got %v", err)
	}
	if !strings.Contains(err.Error(), "retry") {
		t.Fatalf("expected error to mention the Enter retry, got %v", err)
	}
	if !strings.Contains(err.Error(), "boss-test-sess") {
		t.Fatalf("expected error to name the session, got %v", err)
	}
}

// TestMultiLineSubmittedIgnoresPastedGlyphLines guards the false-positive the
// BOS-228 verifier must avoid: a swallowed multi-line paste whose own lines lead
// with an agent-activity glyph (a "•" or "·" bullet, common in plans) must NOT
// be read as the agent's activity. Reading it as activity would report the paste
// as submitted while it sits idle in the composer — the exact silent no-op this
// change eliminates. A genuine agent row (not part of the payload) still counts.
func TestMultiLineSubmittedIgnoresPastedGlyphLines(t *testing.T) {
	// Swallowed paste: both payload lines still in the composer, the 2nd leading
	// with a "•" bullet. No non-payload agent row → still pending.
	payload := "line one\n• bullet two"
	pending := "ready\n❯ line one\n• bullet two\n"
	if multiLineSubmitted(pending, payload) {
		t.Fatalf("pasted glyph-leading continuation line must report still pending, pane=%q", pending)
	}

	// "·" (U+00B7) middle-dot bullet — same class, same expectation.
	payloadDot := "do the audit\n· step two"
	pendingDot := "ready\n❯ do the audit\n· step two\n"
	if multiLineSubmitted(pendingDot, payloadDot) {
		t.Fatalf("pasted middle-dot continuation line must report still pending, pane=%q", pendingDot)
	}

	// Genuine agent activity (a ⏺ row the agent rendered, not in the payload)
	// below the echoed prompt → submitted, even though a payload bullet is
	// present.
	submitted := "ready\n❯ line one\n• bullet two\n⏺ working\n"
	if !multiLineSubmitted(submitted, payload) {
		t.Fatalf("genuine agent activity below the marker must report submitted, pane=%q", submitted)
	}

	// Cleared composer is submitted regardless of payload content.
	if !multiLineSubmitted("ready\n❯\n", payload) {
		t.Fatal("cleared composer must report submitted")
	}
}

// TestMultiLineSubmittedRejectsGlyphAbovePastedLines guards the cleared-composer
// false-positive: a composer that renders the prompt glyph on its OWN row above
// the pasted continuation lines ("❯\nline one\nline two", or a payload with a
// blank leading line) is an UNSUBMITTED composer. Trusting the empty glyph there
// would report a swallowed paste as submitted and resurrect the silent no-op.
func TestMultiLineSubmittedRejectsGlyphAbovePastedLines(t *testing.T) {
	// Prompt glyph on its own row, payload still visible below it → still pending.
	payload := "line one\nline two"
	pending := "ready\n❯\nline one\nline two\n"
	if multiLineSubmitted(pending, payload) {
		t.Fatalf("empty glyph above still-visible payload must report still pending, pane=%q", pending)
	}

	// Payload with a blank leading line renders an empty marker row with content
	// below it → still pending, not a cleared composer.
	blankLead := "\nreal work here\nand more"
	pendingBlank := "ready\n❯\n\nreal work here\nand more\n"
	if multiLineSubmitted(pendingBlank, blankLead) {
		t.Fatalf("blank-leading payload below an empty glyph must report still pending, pane=%q", pendingBlank)
	}

	// Genuinely cleared composer: empty glyph at the bottom with only footer
	// chrome below it (no payload lines) → submitted.
	cleared := "ready\n❯ line one\nline two\n───────\n❯\n  model | cwd\n"
	if !multiLineSubmitted(cleared, payload) {
		t.Fatalf("empty glyph with only footer below (echoed prompt above) must report submitted, pane=%q", cleared)
	}
}

// TestSendPlan_MultiLineSubmitVerifiedFirstTry covers the happy path for the
// BOS-228 multi-line verification: after the paste + Enter the pane shows a
// positive submission signal (agent activity below the marker, or a cleared
// composer), so the verifier passes without a retry. It also asserts a
// verification capture-pane poll ran AFTER the send-keys Enter.
func TestSendPlan_MultiLineSubmitVerifiedFirstTry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}

	cases := []struct {
		name      string
		submitted string
	}{
		{"agent activity below marker", "ready\n❯\n⏺ working\n"},
		{"cleared composer", "ready\n❯\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &sendPlanRecordingFactory{
				capturePaneOutputs: []string{
					"ready\n❯\n", // ready-marker poll
					tc.submitted, // post-Enter verification poll
				},
			}
			c := NewClient(WithCommandFactory(fake.factory))
			if err := c.sendPlan(context.Background(), "boss-test-sess", "line one\nline two", sendPlanOpts{
				readyMarker:      "ready",
				deadline:         time.Second,
				pollInterval:     5 * time.Millisecond,
				submitVerifyWait: 50 * time.Millisecond,
				submitVerifyTick: time.Millisecond,
			}); err != nil {
				t.Fatalf("sendPlan multi-line: unexpected error: %v", err)
			}

			calls := fake.callsCopy()
			lastEnterIdx := -1
			captureAfterEnter := false
			for i, call := range calls {
				switch call.subcommand {
				case "send-keys":
					if len(call.args) > 0 && call.args[len(call.args)-1] == "Enter" {
						lastEnterIdx = i
					}
				case "capture-pane":
					if lastEnterIdx != -1 && i > lastEnterIdx {
						captureAfterEnter = true
					}
				}
			}
			if lastEnterIdx == -1 {
				t.Fatalf("expected a send-keys Enter, calls = %+v", calls)
			}
			if !captureAfterEnter {
				t.Fatalf("expected a verification capture-pane after the send-keys Enter, calls = %+v", calls)
			}
		})
	}
}

// TestSendPlan_MultiLineRetrySucceedsAfterSwallowedEnter proves the retry-once
// behaviour: the composer keeps showing the pasted payload through the first
// verify window (the TUI swallowed the first Enter), then after exactly one
// additional Enter the pane shows agent activity and the verifier passes.
func TestSendPlan_MultiLineRetrySucceedsAfterSwallowedEnter(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	var mu sync.Mutex
	enterCount := 0
	var calls []sendPlanCall
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		mu.Lock()
		defer mu.Unlock()
		subcommand := ""
		if len(args) > 0 {
			subcommand = args[0]
		}
		calls = append(calls, sendPlanCall{subcommand: subcommand, args: append([]string(nil), args[1:]...)})
		switch subcommand {
		case "capture-pane":
			// Still pending until the retry Enter (2nd Enter) lands, then the
			// agent starts working below the marker.
			if enterCount < 2 {
				return exec.CommandContext(ctx, "printf", "%s", "ready\n❯ line one\nline two\n")
			}
			return exec.CommandContext(ctx, "printf", "%s", "ready\n❯\n⏺ working\n")
		case "send-keys":
			if len(args) > 1 && args[len(args)-1] == "Enter" {
				enterCount++
			}
			return exec.CommandContext(ctx, "true")
		default:
			return exec.CommandContext(ctx, "true")
		}
	}

	c := NewClient(WithCommandFactory(factory))
	if err := c.sendPlan(context.Background(), "boss-test-sess", "line one\nline two", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 20 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	}); err != nil {
		t.Fatalf("sendPlan multi-line retry: unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	enters := 0
	for _, call := range calls {
		if call.subcommand == "send-keys" && len(call.args) > 0 && call.args[len(call.args)-1] == "Enter" {
			enters++
		}
	}
	if enters != 2 {
		t.Fatalf("expected exactly two send-keys Enter (initial + one retry), got %d", enters)
	}
}

// TestSendLine_RetrySucceedsAfterSwallowedEnter is the single-line analogue:
// the sendLine submit path must also retry the Enter once before succeeding.
func TestSendLine_RetrySucceedsAfterSwallowedEnter(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	var mu sync.Mutex
	enterCount := 0
	var calls []sendPlanCall
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		mu.Lock()
		defer mu.Unlock()
		subcommand := ""
		if len(args) > 0 {
			subcommand = args[0]
		}
		calls = append(calls, sendPlanCall{subcommand: subcommand, args: append([]string(nil), args[1:]...)})
		switch subcommand {
		case "capture-pane":
			if enterCount < 2 {
				return exec.CommandContext(ctx, "printf", "%s", "ready\n› $boss-repair\n")
			}
			return exec.CommandContext(ctx, "printf", "%s", "ready\n› \n")
		case "send-keys":
			if len(args) > 1 && args[len(args)-1] == "Enter" {
				enterCount++
			}
			return exec.CommandContext(ctx, "true")
		default:
			return exec.CommandContext(ctx, "true")
		}
	}

	c := NewClient(WithCommandFactory(factory))
	if err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 20 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	}); err != nil {
		t.Fatalf("sendLine retry: unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	enters := 0
	for _, call := range calls {
		if call.subcommand == "send-keys" && len(call.args) > 0 && call.args[len(call.args)-1] == "Enter" {
			enters++
		}
	}
	if enters != 2 {
		t.Fatalf("expected exactly two send-keys Enter (initial + one retry), got %d", enters)
	}
}

// TestSendLine_LongSingleLineRetrySucceedsAfterSwallowedEnter is the BOS-489
// end-to-end guard: a single-line payload wider than the pane, whose first Enter
// is swallowed (the delivery landed before the input box settled), must be caught
// as still-pending and retried — not falsely reported submitted. Before the fix,
// the wrapped payload could never text-match one captured row, so the verifier
// passed on the first try and no retry fired (a silent false delivered:true). The
// composer keeps showing the wrapped payload through the first verify window, then
// after exactly one additional Enter the pane clears with agent activity.
func TestSendLine_LongSingleLineRetrySucceedsAfterSwallowedEnter(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	// Wider than the simulated pane, so the input box wraps across rows.
	longLine := strings.Repeat("deliver this wide single-line instruction to the agent ", 3)
	const paneWidth = 70
	// Still-pending pane: the wrapped payload sits in a non-empty input box.
	pendingPane := "ready\n❯ " + longLine[:paneWidth] + "\n" + longLine[paneWidth:] + "\n"
	// Submitted pane after the retry Enter: cleared composer + agent activity.
	submittedPane := "ready\n❯\n⏺ working\n"

	var mu sync.Mutex
	enterCount := 0
	var calls []sendPlanCall
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		mu.Lock()
		defer mu.Unlock()
		subcommand := ""
		if len(args) > 0 {
			subcommand = args[0]
		}
		calls = append(calls, sendPlanCall{subcommand: subcommand, args: append([]string(nil), args[1:]...)})
		switch subcommand {
		case "capture-pane":
			// Still pending until the retry Enter (2nd Enter) lands, then the
			// agent starts working below a cleared marker.
			if enterCount < 2 {
				return exec.CommandContext(ctx, "printf", "%s", pendingPane)
			}
			return exec.CommandContext(ctx, "printf", "%s", submittedPane)
		case "send-keys":
			if len(args) > 1 && args[len(args)-1] == "Enter" {
				enterCount++
			}
			return exec.CommandContext(ctx, "true")
		default:
			return exec.CommandContext(ctx, "true")
		}
	}

	c := NewClient(WithCommandFactory(factory))
	if err := c.sendLine(context.Background(), "boss-test-sess", longLine, sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 20 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	}); err != nil {
		t.Fatalf("sendLine long single-line retry: unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	enters := 0
	for _, call := range calls {
		if call.subcommand == "send-keys" && len(call.args) > 0 && call.args[len(call.args)-1] == "Enter" {
			enters++
		}
	}
	if enters != 2 {
		t.Fatalf("expected exactly two send-keys Enter (initial + one retry), got %d", enters)
	}
}

// TestPrefillPlanWithReadyMarker_MultiLineNoEnterNoVerify asserts prefill-only
// multi-line delivery: the payload is pasted into the composer, but NO Enter is
// ever sent and NO post-delivery verification capture-pane runs.
func TestPrefillPlanWithReadyMarker_MultiLineNoEnterNoVerify(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	fake := &sendPlanRecordingFactory{
		capturePaneOutputs: []string{"Welcome to Claude\n❯\n"},
	}
	c := NewClient(WithCommandFactory(fake.factory))
	if err := c.PrefillPlanWithReadyMarker(context.Background(), "boss-test-sess", "line one\nline two", "❯"); err != nil {
		t.Fatalf("PrefillPlanWithReadyMarker: unexpected error: %v", err)
	}
	assertPrefilledNoEnterNoVerify(t, fake.callsCopy())
}

// TestPrefillLineWithReadyMarker_SingleLineNoEnterNoVerify asserts prefill-only
// single-line delivery: the line is typed via send-keys -l, but NO Enter is
// sent and NO verification capture-pane runs after it.
func TestPrefillLineWithReadyMarker_SingleLineNoEnterNoVerify(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	fake := &sendPlanRecordingFactory{
		capturePaneOutputs: []string{"Welcome to Codex\n›\n"},
	}
	c := NewClient(WithCommandFactory(fake.factory))
	if err := c.PrefillLineWithReadyMarker(context.Background(), "boss-test-sess", "$boss-repair", "›"); err != nil {
		t.Fatalf("PrefillLineWithReadyMarker: unexpected error: %v", err)
	}

	calls := fake.callsCopy()
	sawLiteral := false
	for _, call := range calls {
		if call.subcommand == "send-keys" &&
			equalSlices(call.args, []string{"-t", "boss-test-sess", "-l", "--", "$boss-repair"}) {
			sawLiteral = true
		}
	}
	if !sawLiteral {
		t.Fatalf("expected literal send-keys delivery, calls = %+v", calls)
	}
	assertPrefilledNoEnterNoVerify(t, calls)
}

// assertPrefilledNoEnterNoVerify checks the prefill contract: no send-keys Enter
// was ever issued, and no capture-pane poll ran after the last delivery
// subcommand (i.e. no post-delivery verification).
func assertPrefilledNoEnterNoVerify(t *testing.T, calls []sendPlanCall) {
	t.Helper()

	lastDeliveryIdx := -1
	for i, call := range calls {
		switch call.subcommand {
		case "send-keys":
			if len(call.args) > 0 && call.args[len(call.args)-1] == "Enter" {
				t.Fatalf("prefill must not send Enter, calls = %+v", calls)
			}
			lastDeliveryIdx = i
		case "load-buffer", "paste-buffer":
			lastDeliveryIdx = i
		}
	}
	for i, call := range calls {
		if call.subcommand == "capture-pane" && i > lastDeliveryIdx {
			t.Fatalf("prefill must not run a verification capture-pane after delivery, calls = %+v", calls)
		}
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

// countEnterSendKeys returns how many recorded send-keys calls submit Enter
// (i.e. the last arg is "Enter"). Used to assert the submit-vs-prefill split:
// a prefill path must issue zero Enter send-keys.
func countEnterSendKeys(calls []sendPlanCall) int {
	n := 0
	for _, c := range calls {
		if c.subcommand == "send-keys" && len(c.args) > 0 && c.args[len(c.args)-1] == "Enter" {
			n++
		}
	}
	return n
}

// TestSendMessage_SubmitSingleLine_DeliversEntersAndVerifies verifies that a
// single-line submit waits for the ready marker, delivers the text via literal
// keystrokes, presses Enter, then runs the verifier (a capture-pane poll after
// the Enter). This is the BOS-242 Gap 1 reliable-submit path.
func TestSendMessage_SubmitSingleLine_DeliversEntersAndVerifies(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	fake := &sendPlanRecordingFactory{
		// Ready marker present on the first poll; a cleared (submitted) prompt on
		// the verify poll so verification passes without waiting the full budget.
		capturePaneOutputs: []string{"❯\n", "❯\n"},
	}
	c := NewClient(WithCommandFactory(fake.factory))

	if err := c.SendMessage(context.Background(), "boss-test-sess", "/boss-repair watch", true, "❯"); err != nil {
		t.Fatalf("SendMessage: unexpected error: %v", err)
	}

	calls := fake.callsCopy()
	subcommands := make([]string, len(calls))
	for i, call := range calls {
		subcommands[i] = call.subcommand
	}

	// A literal send-keys delivery (send-keys -l -- <text>) followed by an Enter.
	var sawLiteral, sawEnter, sawCaptureAfterEnter bool
	enterIdx := -1
	for i, call := range calls {
		if call.subcommand == "send-keys" && len(call.args) >= 3 && call.args[2] == "-l" {
			sawLiteral = true
			// The delivered payload must be the literal message, no command prefix.
			if got := call.args[len(call.args)-1]; got != "/boss-repair watch" {
				t.Errorf("literal send-keys payload = %q, want %q", got, "/boss-repair watch")
			}
		}
		if call.subcommand == "send-keys" && len(call.args) > 0 && call.args[len(call.args)-1] == "Enter" {
			sawEnter = true
			enterIdx = i
		}
	}
	for i := enterIdx + 1; i >= 0 && i < len(calls); i++ {
		if calls[i].subcommand == "capture-pane" {
			sawCaptureAfterEnter = true
			break
		}
	}
	if !sawLiteral {
		t.Errorf("expected a literal send-keys delivery, got %v", subcommands)
	}
	if !sawEnter {
		t.Errorf("expected an Enter send-keys, got %v", subcommands)
	}
	if !sawCaptureAfterEnter {
		t.Errorf("expected a verifier capture-pane after Enter, got %v", subcommands)
	}
}

// TestSendMessage_CodexSubmit_WaitsForReadyMarkerBeforeEnter is the BOS-285
// sub-issue B regression guard: a codex single-line submit
// (send_chat_message{submit:true}) must poll capture-pane for codex's "›"
// composer-ready marker BEFORE it presses Enter, and — when the marker never
// appears — must fail loud rather than silently no-op (the cold-boot race where
// a premature Enter was absorbed as a newline). This locks the ordering that
// closes the boot race so a refactor cannot reintroduce a blind Enter.
func TestSendMessage_CodexSubmit_WaitsForReadyMarkerBeforeEnter(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	const codexMarker = "›" // chatReadyMarker("codex"), spawn_chat_tmux.go

	t.Run("gates on ready marker before enter", func(t *testing.T) {
		fake := &sendPlanRecordingFactory{
			// Marker absent on the first poll (cold boot), present on the second,
			// then a cleared prompt for the post-Enter submit verifier.
			capturePaneOutputs: []string{
				"codex booting…\n",
				"codex ready\n›\n",
				"codex ready\n›\n",
			},
		}
		c := NewClient(WithCommandFactory(fake.factory))

		if err := c.SendMessage(context.Background(), "boss-test-sess", "run the thing", true, codexMarker); err != nil {
			t.Fatalf("SendMessage: unexpected error: %v", err)
		}

		calls := fake.callsCopy()
		firstCaptureIdx, literalIdx, enterIdx := -1, -1, -1
		for i, call := range calls {
			switch {
			case call.subcommand == "capture-pane" && firstCaptureIdx == -1:
				firstCaptureIdx = i
			case call.subcommand == "send-keys" && len(call.args) >= 3 && call.args[2] == "-l":
				literalIdx = i
			case call.subcommand == "send-keys" && len(call.args) > 0 && call.args[len(call.args)-1] == "Enter":
				if enterIdx == -1 {
					enterIdx = i
				}
			}
		}
		if firstCaptureIdx == -1 {
			t.Fatalf("expected a capture-pane ready poll, calls = %+v", calls)
		}
		if enterIdx == -1 {
			t.Fatalf("expected an Enter send-keys (submit), calls = %+v", calls)
		}
		if literalIdx == -1 {
			t.Fatalf("expected a literal send-keys delivery, calls = %+v", calls)
		}
		// The ready-marker poll and the literal delivery must both precede Enter.
		if firstCaptureIdx >= enterIdx {
			t.Errorf("ready-marker capture-pane (idx %d) must precede Enter (idx %d)", firstCaptureIdx, enterIdx)
		}
		if literalIdx >= enterIdx {
			t.Errorf("literal delivery (idx %d) must precede Enter (idx %d)", literalIdx, enterIdx)
		}
		// At least two capture-pane polls happened before Enter (cold boot then
		// ready), proving Enter did not fire until the marker was observed.
		polls := 0
		for i := 0; i < enterIdx; i++ {
			if calls[i].subcommand == "capture-pane" {
				polls++
			}
		}
		if polls < 2 {
			t.Errorf("expected ≥2 ready-marker polls before Enter, got %d (calls %+v)", polls, calls)
		}
	})

	t.Run("cannot silently no-op when never ready", func(t *testing.T) {
		fake := &sendPlanRecordingFactory{
			// Marker never appears — the composer never became ready.
			capturePaneOutputs: []string{"codex booting…\n"},
		}
		c := NewClient(WithCommandFactory(fake.factory))

		// sendLine is the path SendMessage routes a single-line submit through;
		// call it directly with a short deadline so the never-ready case fails
		// fast instead of waiting the full production budget.
		err := c.sendLine(context.Background(), "boss-test-sess", "run the thing", sendPlanOpts{
			deadline:     20 * time.Millisecond,
			pollInterval: 2 * time.Millisecond,
			readyMarker:  codexMarker,
		})
		if err == nil {
			t.Fatal("expected an error when the codex ready marker never appears, got nil")
		}
		if !strings.Contains(err.Error(), "ready marker") {
			t.Fatalf("expected a ready-marker error, got %v", err)
		}
		// The gate must not have delivered or submitted anything — no literal
		// send-keys and no Enter — so a never-ready composer is a loud failure,
		// never a silent no-op that drops the message.
		for _, call := range fake.callsCopy() {
			if call.subcommand == "send-keys" {
				t.Fatalf("no send-keys must occur before the marker is ready, saw %+v", call)
			}
			if call.subcommand == "load-buffer" || call.subcommand == "paste-buffer" {
				t.Fatalf("no delivery must occur before the marker is ready, saw %+v", call)
			}
		}
	})
}

// TestSendMessage_OpenCodeSubmit verifies the OpenCode rail glyph reaches both
// the readiness gate and the post-Enter submit verifier. The rail is also a
// possible box-border rune, so a constant-only host test would miss a send path
// that times out before typing anything.
func TestSendMessage_OpenCodeSubmit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}

	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{
		"OpenCode ready\n┃ Type a message\n",
		"OpenCode ready\n┃\n",
	}}
	c := NewClient(WithCommandFactory(fake.factory))
	if err := c.SendMessage(context.Background(), "boss-test-sess", "run the thing", true, "┃"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
}

// TestSendMessage_Submit_DeliversEntersAndVerifies verifies that submit=true
// delivers, presses Enter once, and runs the verifier for BOTH payload shapes
// (BOS-488): a single-line message is typed via literal keystrokes, a multi-line
// message is bracketed-pasted, and each is auto-submitted and verified one layer
// down. This locks the collapsed two-branch routing — submit no longer forks on
// payload shape, so a multi-line submit is no longer silently downgraded to a
// paste-only prefill.
func TestSendMessage_Submit_DeliversEntersAndVerifies(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	cases := []struct {
		name     string
		text     string
		wantType bool // literal send-keys -l delivery (single-line); else paste
	}{
		{"single-line types", "run the thing", true},
		{"multi-line pastes", "line one\nline two", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Ready marker on the first poll; a cleared (submitted) prompt on the
			// verify poll so verification passes without waiting the full budget —
			// "❯\n" clears the single-line literal row AND reads as a submitted
			// multi-line composer (empty marker row, no payload lines below).
			fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{"❯\n", "❯\n"}}
			c := NewClient(WithCommandFactory(fake.factory))

			if err := c.SendMessage(context.Background(), "boss-test-sess", tc.text, true, "❯"); err != nil {
				t.Fatalf("SendMessage: unexpected error: %v", err)
			}

			calls := fake.callsCopy()

			// Exactly one Enter submits the payload.
			if n := countEnterSendKeys(calls); n != 1 {
				t.Errorf("submit pressed Enter %d times, want 1", n)
			}

			// Delivery shape: single-line types (send-keys -l), multi-line pastes
			// (load-buffer + paste-buffer).
			var sawLiteral, sawLoad, sawPaste bool
			enterIdx := -1
			for i, call := range calls {
				switch call.subcommand {
				case "send-keys":
					if len(call.args) >= 3 && call.args[2] == "-l" {
						sawLiteral = true
					}
					if len(call.args) > 0 && call.args[len(call.args)-1] == "Enter" {
						enterIdx = i
					}
				case "load-buffer":
					sawLoad = true
				case "paste-buffer":
					sawPaste = true
				}
			}
			if tc.wantType {
				if !sawLiteral {
					t.Errorf("expected literal send-keys -l delivery for single-line, calls = %+v", calls)
				}
			} else if !sawLoad || !sawPaste {
				t.Errorf("expected load-buffer+paste-buffer for multi-line, got load=%v paste=%v", sawLoad, sawPaste)
			}

			// A verifier capture-pane poll ran AFTER the Enter, proving submission
			// was verified rather than fired blind.
			if enterIdx == -1 {
				t.Fatalf("expected a send-keys Enter, calls = %+v", calls)
			}
			sawCaptureAfterEnter := false
			for i := enterIdx + 1; i < len(calls); i++ {
				if calls[i].subcommand == "capture-pane" {
					sawCaptureAfterEnter = true
					break
				}
			}
			if !sawCaptureAfterEnter {
				t.Errorf("expected a verifier capture-pane after Enter, calls = %+v", calls)
			}
		})
	}
}

// TestSendMessage_PrefillMultiLine_PastesNoEnter verifies the default
// (submit=false) path prefills a multi-line payload into the composer with NO
// Enter — the prefill contract is unchanged by the BOS-488 collapse.
func TestSendMessage_PrefillMultiLine_PastesNoEnter(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{"❯\n"}}
	c := NewClient(WithCommandFactory(fake.factory))

	if err := c.SendMessage(context.Background(), "boss-test-sess", "line one\nline two", false, "❯"); err != nil {
		t.Fatalf("SendMessage: unexpected error: %v", err)
	}

	calls := fake.callsCopy()
	if n := countEnterSendKeys(calls); n != 0 {
		t.Errorf("multi-line prefill pressed Enter %d times, want 0", n)
	}
	// Must paste (load-buffer + paste-buffer), not type.
	var sawLoad, sawPaste bool
	for _, call := range calls {
		switch call.subcommand {
		case "load-buffer":
			sawLoad = true
		case "paste-buffer":
			sawPaste = true
		}
	}
	if !sawLoad || !sawPaste {
		t.Errorf("expected load-buffer+paste-buffer for multi-line prefill, got load=%v paste=%v", sawLoad, sawPaste)
	}
}

// TestSendMessage_PrefillSingleLine_TypesNoEnter verifies the default
// (submit=false) path prefills a single line into the composer with NO Enter.
func TestSendMessage_PrefillSingleLine_TypesNoEnter(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{"❯\n"}}
	c := NewClient(WithCommandFactory(fake.factory))

	if err := c.SendMessage(context.Background(), "boss-test-sess", "just prefill me", false, "❯"); err != nil {
		t.Fatalf("SendMessage: unexpected error: %v", err)
	}

	calls := fake.callsCopy()
	if n := countEnterSendKeys(calls); n != 0 {
		t.Errorf("prefill pressed Enter %d times, want 0", n)
	}
}

func TestSendMessage_EmptySessionName_Error(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	c := NewClient(WithCommandFactory((&mockCommandFactory{}).factory))
	err := c.SendMessage(context.Background(), "", "hello", true, "❯")
	if err == nil {
		t.Fatal("expected error for empty session name")
	}
}

// TestWillSubmit pins the routing predicate SendMessage dispatches on, which is
// also what the SendChatMessage RPC asks to decide whether the delivery it just
// made was a submit (and so whether delivery_state may claim SUBMITTED). Both
// callers must agree: a whitespace-only body with submit=true is delivered as a
// PREFILL — no Enter, no verification — so reporting it as submitted would be a
// false success of exactly the kind BOS-598 exists to remove.
func TestWillSubmit(t *testing.T) {
	tests := []struct {
		name   string
		submit bool
		text   string
		want   bool
	}{
		{name: "submit with text", submit: true, text: "hello", want: true},
		{name: "submit with surrounding whitespace", submit: true, text: "  hello\n", want: true},
		{name: "submit with multi-line text", submit: true, text: "line one\nline two", want: true},
		{name: "submit with empty text", submit: true, text: "", want: false},
		{name: "submit with whitespace-only text", submit: true, text: " \n\t ", want: false},
		{name: "prefill with text", submit: false, text: "hello", want: false},
		{name: "prefill with empty text", submit: false, text: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WillSubmit(tt.submit, tt.text); got != tt.want {
				t.Errorf("WillSubmit(%v, %q) = %v, want %v", tt.submit, tt.text, got, tt.want)
			}
		})
	}
}

// sendKeysKind classifies a recorded `tmux send-keys` argv into the three
// shapes the submit path emits, so a test can assert their ORDER without
// re-encoding the full argv of each one.
func sendKeysKind(args []string) string {
	if len(args) == 0 {
		return ""
	}
	switch args[len(args)-1] {
	case "Enter":
		return "enter"
	case "C-u":
		return "clear"
	}
	for _, a := range args {
		if a == "-l" {
			return "literal"
		}
	}
	return "other"
}

// composerPane is a stateful tmux fake that MODELS the composer the submit path
// manipulates, rather than replaying a canned capture sequence keyed on an Enter
// counter. The retry now reads the pane back after its clear (C-u is
// kill-to-start-of-line, not a guaranteed whole-composer empty, so the
// postcondition is checked), which a counter-keyed fake cannot represent: it
// would report the composer as still holding the payload no matter what
// keystroke was sent, and no argv ordering asserted against it would be earned.
//
// Delivery (send-keys -l, or paste-buffer) fills the composer, C-u empties it,
// and Enter submits whatever it holds — except the first Enter when
// swallowFirstEnter is set, which models the TUI dropping a keystroke that
// arrived before the input box settled. That is the exact condition the retry
// exists for.
type composerPane struct {
	mu sync.Mutex
	// pasted is what the paste path (load-buffer/paste-buffer) puts in the
	// composer; the literal path uses the argv it was given.
	pasted string
	// deliverKeepsBytes models a DELIVERY that lands only the first N bytes, so
	// the composer holds a truncated payload before the verifier has pressed a
	// single key. The retry's per-press comparisons can only establish that
	// nothing CHANGED, so a composer that starts out wrong is the one shape they
	// cannot speak to on their own.
	deliverKeepsBytes int
	// swallowFirstEnter drops the first Enter, forcing the retry path.
	swallowFirstEnter bool
	// clearIsNoop models a composer that survives C-u untouched — the payload is
	// still intact, so the bare Enter that follows is the recovery the retry
	// exists for.
	clearIsNoop bool
	// clearKeepsBytes models C-u as the line-scoped kill it actually is: it leaves
	// the first N bytes of the payload behind. What survives is MUTILATED user
	// text, so it must never be submitted — but a second press finishes the job,
	// which is the convergence the retry depends on.
	clearKeepsBytes int
	// clearDropsOneByte models a composer that NEVER converges: every press takes
	// one more byte and always leaves a fragment behind.
	clearDropsOneByte bool
	// markerGoneAfterClear models the input box disappearing after the clear (the
	// agent accepted the payload and went full-screen), which must NOT be read as
	// a cleared composer to type into.
	markerGoneAfterClear bool
	// activityAfterClear renders an agent-activity row BELOW the input box from
	// the clear onwards while the composer keeps holding the payload. C-u cannot
	// submit anything, so activity there is never evidence the clear worked.
	activityAfterClear bool
	// staleCaptures models a composer that has already changed but has not
	// repainted: the next N capture-pane calls after each clear keep replaying the
	// PRE-clear screen, whatever the composer now actually holds. Counting
	// captures rather than milliseconds is what makes the tests that depend on it
	// deterministic — the submit path's poll windows take a fixed number of
	// captures each (their budget is well under one tick), so which capture first
	// sees the truth is a property of the code under test, not of the load on the
	// machine running it.
	staleCaptures int
	// markerlessCaptureNo makes ONE capture, counted from the first capture-pane
	// call of the run, render no input box at all. A repainting composer really
	// does produce such a frame: the box is redrawn rather than moved, so a
	// capture can land between the erase and the draw. It says nothing about what
	// the composer holds.
	markerlessCaptureNo int
	// markerlessFromCaptureNo makes EVERY capture from the Nth onwards render no
	// input box, modelling a pane that stays unreadable rather than one frame that
	// is — a full-screen overlay, a redraw storm, an agent that repaints the box
	// away. No amount of looking again resolves it, so nothing the composer holds
	// can be established from it.
	markerlessFromCaptureNo int
	// paneWidth models a pane NARROWER than the payload: a held line longer than
	// this many columns is rendered across as many physical rows as it takes,
	// which is what a terminal actually does and what capture-pane therefore
	// returns. Unset (0) means "infinitely wide" — one row per held line — which
	// is what every other test here models.
	//
	// This exists because a one-row composer cannot express the shape the submit
	// verifier's baseline logic exists to defend. A payload line wider than the
	// pane occupies several rows, so a C-u that kills only the tail leaves the
	// MARKER row byte-identical and changes only the rows below it. A fake that
	// never wraps renders that mutilation as a changed marker row, i.e. as the
	// easy case that every predicate here already catches — so the hard case
	// silently cannot fail a test, and a wrong fix passes the suite.
	paneWidth int

	held          string
	markerGone    bool
	activityShown bool
	stalePane     string
	staleLeft     int
	captures      int
	enters        int
	submitted     bool
	calls         []sendPlanCall
}

// deliver is what a delivery actually lands in the composer: the whole text,
// or — with deliverKeepsBytes set — only its first N bytes.
func (p *composerPane) deliver(text string) string {
	if p.deliverKeepsBytes > 0 && len(text) > p.deliverKeepsBytes {
		return text[:p.deliverKeepsBytes]
	}
	return text
}

// composerRows renders the physical rows the live input box occupies: the
// marker row, then one row per wrapped continuation. The glyph sits on the first
// row only; the terminal indents the continuations under it, which is why the
// submit verifier trims a row before judging it.
func (p *composerPane) composerRows() []string {
	var rows []string
	for _, line := range strings.Split(p.held, "\n") {
		rows = append(rows, wrapComposerLine(line, p.paneWidth)...)
	}
	rows[0] = "› " + rows[0]
	for i := 1; i < len(rows); i++ {
		rows[i] = "  " + rows[i]
	}
	return rows
}

// wrapComposerLine splits one composer line the way a terminal of the given
// width does: hard-wrapped at the column boundary, never at a word boundary. A
// width of zero or less models a pane wide enough to never wrap.
func wrapComposerLine(line string, width int) []string {
	runes := []rune(line)
	if width <= 0 || len(runes) <= width {
		return []string{line}
	}
	var rows []string
	for len(runes) > width {
		rows = append(rows, string(runes[:width]))
		runes = runes[width:]
	}
	if len(runes) > 0 {
		rows = append(rows, string(runes))
	}
	return rows
}

// paneNow renders the composer as it currently IS.
func (p *composerPane) paneNow() string {
	pane := "ready\n"
	if !p.markerGone {
		pane += strings.Join(p.composerRows(), "\n") + "\n"
	}
	if p.activityShown {
		pane += "· thinking\n"
	}
	if p.submitted {
		pane += "⏺ working\n"
	}
	return pane
}

// capture renders what capture-pane sees: the pre-clear screen while a repaint
// is still owed, otherwise the live pane.
func (p *composerPane) capture() string {
	p.captures++
	if p.captures == p.markerlessCaptureNo {
		return "ready\n"
	}
	if p.markerlessFromCaptureNo > 0 && p.captures >= p.markerlessFromCaptureNo {
		return "ready\n"
	}
	if p.staleLeft > 0 {
		p.staleLeft--
		return p.stalePane
	}
	return p.paneNow()
}

func (p *composerPane) factory(ctx context.Context, _ string, args ...string) *exec.Cmd {
	p.mu.Lock()
	defer p.mu.Unlock()
	subcommand := ""
	if len(args) > 0 {
		subcommand = args[0]
	}
	p.calls = append(p.calls, sendPlanCall{subcommand: subcommand, args: append([]string(nil), args[1:]...)})
	switch subcommand {
	case "capture-pane":
		return exec.CommandContext(ctx, "printf", "%s", p.capture())
	case "paste-buffer":
		p.held = p.deliver(p.pasted)
	case "send-keys":
		switch sendKeysKind(args[1:]) {
		case "literal":
			p.held = p.deliver(args[len(args)-1])
		case "clear":
			if p.staleCaptures > 0 {
				p.stalePane, p.staleLeft = p.paneNow(), p.staleCaptures
			}
			switch {
			case p.clearDropsOneByte && len(p.held) > 1:
				p.held = p.held[:len(p.held)-1]
			case p.clearKeepsBytes > 0 && len(p.held) > p.clearKeepsBytes:
				p.held = p.held[:p.clearKeepsBytes]
			case p.clearIsNoop:
			default:
				p.held = ""
			}
			if p.markerGoneAfterClear {
				p.markerGone = true
			}
			if p.activityAfterClear {
				p.activityShown = true
			}
		case "enter":
			p.enters++
			if p.enters == 1 && p.swallowFirstEnter {
				break
			}
			if p.held != "" {
				p.submitted = true
			}
			p.held = ""
		}
	}
	return exec.CommandContext(ctx, "true")
}

// submitSteps renders the recorded calls as the ordered delivery/keystroke steps
// the submit path emitted, so a test can assert their ORDER across both delivery
// shapes (literal typing and bracketed paste) without re-encoding each argv.
func (p *composerPane) submitSteps() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var steps []string
	for _, call := range p.calls {
		switch call.subcommand {
		case "paste-buffer":
			steps = append(steps, "paste")
		case "send-keys":
			if kind := sendKeysKind(call.args); kind != "other" {
				steps = append(steps, kind)
			}
		}
	}
	return steps
}

// stepsFromFirstClear is submitSteps with the capture-pane calls left IN, from
// the first clear onwards. The captures are the only place a test can see HOW
// the submit path decided what the composer held — how many times it looked, and
// at what point in the keystroke sequence — which is what separates a decision
// taken on one reading from one taken on a second, later reading of the same
// composer.
func (p *composerPane) stepsFromFirstClear() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var steps []string
	for _, call := range p.calls {
		switch call.subcommand {
		case "capture-pane":
			steps = append(steps, "capture")
		case "paste-buffer":
			steps = append(steps, "paste")
		case "send-keys":
			if kind := sendKeysKind(call.args); kind != "other" {
				steps = append(steps, kind)
			}
		}
	}
	if i := slices.Index(steps, "clear"); i >= 0 {
		return steps[i:]
	}
	return nil
}

// didSubmit reports whether the composer ever handed a payload to the agent —
// the fake's own record of what actually left the input box, independent of the
// argv the submit path emitted.
func (p *composerPane) didSubmit() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.submitted
}

func (p *composerPane) argvOfKind(kind string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, call := range p.calls {
		if call.subcommand == "send-keys" && sendKeysKind(call.args) == kind {
			return call.args
		}
	}
	return nil
}

// TestSendLine_RetryClearsComposerBeforeSecondEnter is the BOS-598 idempotency
// guard (acceptance criterion 4). Before this change the retry fired a bare
// second Enter: if the FIRST Enter had in fact been accepted and the verifier
// simply could not see it yet, that stray Enter landed in a freshly-emptied
// composer — and if the composer still held the payload, a re-delivery without
// clearing would double-type it. The retry must therefore clear the composer
// (send-keys C-u) and re-deliver the payload before its second Enter.
//
// The assertion is against the recorded tmux argv, which is the only place the
// ordering is observable: send-keys -l (deliver) → Enter → C-u (clear) →
// send-keys -l (re-deliver) → Enter.
func TestSendLine_RetryClearsComposerBeforeSecondEnter(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	pane := &composerPane{swallowFirstEnter: true}
	c := NewClient(WithCommandFactory(pane.factory))
	if err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 20 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	}); err != nil {
		t.Fatalf("sendLine retry: unexpected error: %v", err)
	}

	want := []string{"literal", "enter", "clear", "literal", "enter"}
	if got := pane.submitSteps(); !slices.Equal(got, want) {
		t.Fatalf("submit sequence = %v, want %v", got, want)
	}

	// The clear must be targeted at the session under test, not a bare global
	// keystroke that could land in whatever pane tmux considers current.
	clearArgs := pane.argvOfKind("clear")
	if len(clearArgs) < 3 || clearArgs[0] != "-t" || clearArgs[1] != "boss-test-sess" {
		t.Fatalf("clear argv = %v, want it targeted at -t boss-test-sess", clearArgs)
	}
}

// TestSendPlan_MultiLineRetryClearsComposerBeforeSecondEnter is the multi-line
// analogue, and covers the riskier of the two delivery shapes: a swallowed Enter
// on a pasted plan used to be retried with a bare Enter, and re-delivering
// without a clear would concatenate a second copy of the WHOLE plan. The paste
// path must show the same clear-then-re-deliver ordering as the literal one.
func TestSendPlan_MultiLineRetryClearsComposerBeforeSecondEnter(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	const plan = "line one\nline two"
	pane := &composerPane{pasted: plan, swallowFirstEnter: true}
	c := NewClient(WithCommandFactory(pane.factory))
	if err := c.sendPlan(context.Background(), "boss-test-sess", plan, sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 20 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	}); err != nil {
		t.Fatalf("sendPlan multi-line retry: unexpected error: %v", err)
	}

	want := []string{"paste", "enter", "clear", "paste", "enter"}
	if got := pane.submitSteps(); !slices.Equal(got, want) {
		t.Fatalf("submit sequence = %v, want %v", got, want)
	}
}

// TestSendLine_RetryNoOpClearSubmitsTheIntactPayloadOnce pins the benign half of
// the clear postcondition. C-u is kill-to-start-of-LINE and nothing in this repo
// demonstrates that either the Claude or the Codex composer empties on one press,
// so the composer surviving is expected, not exceptional. When it survives
// UNCHANGED the keystroke was a no-op and the held payload is intact, so the
// retry must NOT re-deliver — typing a second copy on top of the first would
// submit both concatenated. It presses Enter on what is already there, exactly
// once, and succeeds.
func TestSendLine_RetryNoOpClearSubmitsTheIntactPayloadOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	pane := &composerPane{swallowFirstEnter: true, clearIsNoop: true}
	c := NewClient(WithCommandFactory(pane.factory))
	if err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 20 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	}); err != nil {
		t.Fatalf("sendLine retry: unexpected error for a no-op clear: %v", err)
	}

	// One delivery only: the second Enter lands on the copy already held.
	want := []string{"literal", "enter", "clear", "enter"}
	if got := pane.submitSteps(); !slices.Equal(got, want) {
		t.Fatalf("submit sequence = %v, want %v (no second copy typed onto an intact payload)", got, want)
	}
}

// TestSendLine_RetryTreatsAnUnreadableSnapshotAsNoEvidence pins the cost of
// getting the UNREADABLE capture wrong. composerFootprint returns "" for a pane
// that drew no input box, and a repainting composer really does produce such a
// frame — the box is erased and redrawn, so a capture can land in between. That
// frame says nothing about what the composer holds, in EITHER direction: reading
// it as a cut latches mutilation, a one-way door that forbids every later Enter
// and fails a healthy delivery loudly as NOT_SUBMITTED with the payload sitting
// intact in the box.
//
// So the baseline is polled rather than sampled: the retry looks again until the
// box is readable, and only what it reads there licenses anything. The composer
// here survives C-u untouched, exactly as in the no-op case above, and only the
// first look is blank — the retry must still recover: one Enter onto the intact
// payload, no second copy typed.
func TestSendLine_RetryTreatsAnUnreadableSnapshotAsNoEvidence(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	// Capture 3 is the baseline poll's FIRST look: the ready-marker wait takes
	// capture 1 and the first submission verify takes capture 2, each of those
	// windows taking exactly one capture because its budget is well under the
	// pollers' default tick. Any other capture landing on it changes the outcome,
	// so a mis-count fails this test rather than passing it vacuously.
	pane := &composerPane{swallowFirstEnter: true, clearIsNoop: true, markerlessCaptureNo: 3}
	c := NewClient(WithCommandFactory(pane.factory))
	if err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 30 * time.Millisecond,
	}); err != nil {
		t.Fatalf("sendLine retry: unexpected error when only the pre-clear snapshot was unreadable: %v", err)
	}

	want := []string{"literal", "enter", "clear", "enter"}
	if got := pane.submitSteps(); !slices.Equal(got, want) {
		t.Fatalf("submit sequence = %v, want %v (a blank snapshot is not proof the clear cut into the payload)", got, want)
	}
}

// TestSendLine_RetryUnreadableSnapshotIsNotALicenceToEnterATruncatedPayload is
// the twin of the test above, and the reason the two must be pinned together:
// the SAME unreadable frame that must not be read as a cut must equally not be
// read as proof of an intact payload. Whichever way an absent footprint is
// resolved by default, one of these two deliveries is wrong — so neither may be
// decided from it, and the composer must be read again until it is legible.
//
// Here the pane is unreadable exactly when the clear truncates the payload, and
// then holds the fragment. Reading "no evidence of a cut" as "safe to Enter"
// submits the fragment, which leaves the composer, which the verifier reads as
// submitted — delivered=true for corrupted user text, silently, with every test
// green. Nothing is ever Entered onto a payload that could not be shown intact.
func TestSendLine_RetryUnreadableSnapshotIsNotALicenceToEnterATruncatedPayload(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	// Capture 3 is the retry's first look at the composer (see the capture count
	// above), and the press that follows it takes all but the first 9 bytes of the
	// payload; clearIsNoop then holds the fragment there, so the run can only end
	// loudly or by submitting mutilated text.
	pane := &composerPane{
		swallowFirstEnter:   true,
		markerlessCaptureNo: 3,
		clearKeepsBytes:     len("$boss-rep"),
		clearIsNoop:         true,
	}
	c := NewClient(WithCommandFactory(pane.factory))
	err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 30 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("sendLine retry: want a loud error when the clear truncated the payload behind an unreadable frame, got nil (mutilated text was submitted and reported as success)")
	}
	if got := OutcomeOf(err); got != OutcomeNotSubmitted {
		t.Fatalf("OutcomeOf(err) = %v, want %v (the payload provably never left the composer)", got, OutcomeNotSubmitted)
	}
	if !errors.Is(err, errSubmissionPending) {
		t.Fatalf("err = %v, want it to carry errSubmissionPending", err)
	}
	if steps := pane.stepsFromFirstClear(); slices.Contains(steps, "enter") {
		t.Fatalf("steps from the clear = %v, want no enter (the composer held a truncated payload)", steps)
	}
}

// TestSendLine_RetryPressesNoKeyWhileTheComposerStaysUnreadable pins the floor
// under the two tests above. They both rest on there being a readable reading of
// the composer to judge later ones against; this is the case where there is
// none — the box never renders inside the retry's budget.
//
// Every key available here is unsafe without one. Enter would submit whatever
// the invisible box holds, which may already be a fragment, and C-u would keep
// cutting into a payload no one can see. The only correct move is to press
// nothing and say so loudly: the payload is confirmed still pending (the caller
// proved that before the retry began, and no key has been sent since), so the
// operator gets an accurate NOT_SUBMITTED they can retry rather than a silent
// corruption or a "may already have been submitted" they cannot act on.
func TestSendLine_RetryPressesNoKeyWhileTheComposerStaysUnreadable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	// Captures 1 and 2 are the ready-marker wait and the first submission verify,
	// which must see the payload sitting in the composer for the retry to run at
	// all; the box is gone from capture 3 on, so every look the retry takes is
	// unreadable.
	pane := &composerPane{swallowFirstEnter: true, markerlessFromCaptureNo: 3}
	c := NewClient(WithCommandFactory(pane.factory))
	err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 30 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("sendLine retry: want a loud error when the composer never became readable, got nil")
	}
	if got := OutcomeOf(err); got != OutcomeNotSubmitted {
		t.Fatalf("OutcomeOf(err) = %v, want %v (nothing was pressed, so nothing was submitted)", got, OutcomeNotSubmitted)
	}
	if !errors.Is(err, errSubmissionPending) {
		t.Fatalf("err = %v, want it to carry errSubmissionPending", err)
	}
	// The literal and the swallowed Enter are the original delivery; the retry
	// itself adds nothing — not even the C-u it would otherwise open with.
	want := []string{"literal", "enter"}
	if got := pane.submitSteps(); !slices.Equal(got, want) {
		t.Fatalf("submit sequence = %v, want %v (no key may be pressed into a composer that cannot be read)", got, want)
	}
}

// TestSendLine_RetryConvergesWhenTheClearOnlyKillsPartOfThePayload pins the
// dominant hostile case. C-u is a line-scoped kill, so against a composer whose
// payload spans more than one rendered row it leaves a TRUNCATED copy behind,
// and the pane predicates are prefix-based (wrapping immunity, BOS-489) so they
// read that fragment as "still holds the payload" — indistinguishable from the
// intact case on content alone. Pressing Enter there would submit mangled user
// text and report success. What separates the two is whether a press CHANGES the
// composer: a shrinking footprint proves the kill is landing, so the retry keeps
// pressing (bounded) until the composer is provably empty, and only then
// re-delivers and submits.
func TestSendLine_RetryConvergesWhenTheClearOnlyKillsPartOfThePayload(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	pane := &composerPane{swallowFirstEnter: true, clearKeepsBytes: len("$boss-rep")}
	c := NewClient(WithCommandFactory(pane.factory))
	if err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 60 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	}); err != nil {
		t.Fatalf("sendLine retry: want the second clear to converge and submit, got %v", err)
	}
	// Two clears: the first truncates, the second empties. Only once the
	// composer is provably empty is the payload re-typed and submitted — the
	// mutilated fragment is never what gets Entered.
	want := []string{"literal", "enter", "clear", "clear", "literal", "enter"}
	if got := pane.submitSteps(); !slices.Equal(got, want) {
		t.Fatalf("submit sequence = %v, want %v (clear until empty, then re-deliver)", got, want)
	}
}

// TestSendLine_RetryFailsLoudlyWhenTheClearNeverConverges pins the surrender
// branch. A composer that gives up one byte per press is still CHANGING, so the
// no-op shortcut never fires and the payload is never intact — but it also never
// empties. The retry must not press forever, and it must never Enter onto the
// surviving fragment: after a bounded number of presses it fails loudly as
// NOT_SUBMITTED, which is precisely true.
func TestSendLine_RetryFailsLoudlyWhenTheClearNeverConverges(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	pane := &composerPane{swallowFirstEnter: true, clearDropsOneByte: true}
	c := NewClient(WithCommandFactory(pane.factory))
	err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 60 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	})
	if err == nil {
		t.Fatal("sendLine retry: want a loud error when the clear never converges, got nil")
	}
	if got := OutcomeOf(err); got != OutcomeNotSubmitted {
		t.Fatalf("OutcomeOf(err) = %v, want %v (the payload provably never left the composer)", got, OutcomeNotSubmitted)
	}
	if !errors.Is(err, errSubmissionPending) {
		t.Fatalf("err = %v, want it to carry errSubmissionPending", err)
	}
	// Bounded presses, and no Enter after any of them: nothing is typed into,
	// and nothing is submitted from, a composer whose contents cannot be
	// trusted. The absent trailing Enter is what proves the fragment never
	// submitted.
	want := []string{"literal", "enter", "clear", "clear", "clear"}
	if got := pane.submitSteps(); !slices.Equal(got, want) {
		t.Fatalf("submit sequence = %v, want %v (no Enter onto a mutilated payload)", got, want)
	}
}

// TestSendLine_RetryFailsLoudlyWhenAPartialClearThenStalls pins the interaction
// between the two clear outcomes, which is where the unchanged-footprint
// shortcut is unsound unless the mutilation is LATCHED. C-u is line-scoped, so
// press 1 can kill only part of the payload (footprint changes, so the loop
// correctly presses again) and press 2 can then be a genuine no-op (footprint
// unchanged). Reading that second press as "the press did nothing, so the held
// payload is intact" is false — the payload was already cut — and Entering the
// remainder would submit mangled user text AND report success, because the
// mutilated text leaving the composer is exactly what the verifier reads as
// submitted. Once anything has been killed, an unchanged footprint means STUCK,
// not intact: the same loud not-submitted failure as the press cap, with no
// Enter.
//
// clearKeepsBytes plus clearIsNoop models that composer directly: the first
// press truncates to the kept prefix, and every later press finds nothing more
// to kill on that line.
func TestSendLine_RetryFailsLoudlyWhenAPartialClearThenStalls(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	pane := &composerPane{swallowFirstEnter: true, clearKeepsBytes: len("$boss-rep"), clearIsNoop: true}
	c := NewClient(WithCommandFactory(pane.factory))
	err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 60 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	})
	if err == nil {
		t.Fatal("sendLine retry: want a loud error when a partial clear stalls, got nil (a mutilated payload was submitted and reported as success)")
	}
	if got := OutcomeOf(err); got != OutcomeNotSubmitted {
		t.Fatalf("OutcomeOf(err) = %v, want %v (the payload provably never left the composer)", got, OutcomeNotSubmitted)
	}
	if !errors.Is(err, errSubmissionPending) {
		t.Fatalf("err = %v, want it to carry errSubmissionPending", err)
	}
	// The absent trailing Enter is the assertion that matters: the truncated
	// payload is never submitted.
	want := []string{"literal", "enter", "clear", "clear"}
	if got := pane.submitSteps(); !slices.Equal(got, want) {
		t.Fatalf("submit sequence = %v, want %v (no Enter onto a mutilated payload)", got, want)
	}
}

// TestSendLine_RetryNeverSubmitsAWrappedPayloadTheClearTruncated is the
// wrapped-geometry twin of the stall test above, and the regression guard for
// the defect that survived three fix rounds of BOS-598 with a green suite each
// time.
//
// A payload line WIDER than the pane occupies several physical rows. C-u is
// line-scoped, so it can kill the tail of that line and leave the MARKER row
// byte-identical — every visible difference lands on the continuation rows
// below it. A footprint that reads only the marker row, or that admits a
// below-marker row only when it exactly equals a whole payload line, therefore
// sees NOTHING change across the clear. That reads as "the press did nothing,
// so the box still holds the payload intact", the bare Enter fires on the
// fragment, and the RPC answers delivered=true / DELIVERY_STATE_SUBMITTED for
// silently truncated user text — falsifying acceptance criterion 3 on the exact
// path the retry exists to serve. Over-wide lines are the normal case for the
// multi-line payloads that path carries, not a corner.
//
// The composer must instead see the continuation rows change, latch mutilated,
// and fail loudly with no Enter at all.
func TestSendLine_RetryNeverSubmitsAWrappedPayloadTheClearTruncated(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	// Deliberately free of the prompt-marker glyphs, so no wrapped continuation
	// row can be mistaken for the marker row itself.
	const wide = "$boss-repair fix the failing check on pull request 1754 and push the result"
	const paneWidth = 24
	const keepBytes = 40

	// The geometry IS the test: unless the kept prefix outruns the marker row,
	// the truncation shows up on the marker row and this degenerates into the
	// easy case the existing tests already cover.
	if keepBytes <= paneWidth || keepBytes >= len(wide) {
		t.Fatalf("test geometry is wrong: need paneWidth(%d) < keepBytes(%d) < len(payload)(%d), so the clear leaves the marker row identical and cuts only the rows below it", paneWidth, keepBytes, len(wide))
	}

	pane := &composerPane{
		swallowFirstEnter: true,
		paneWidth:         paneWidth,
		clearKeepsBytes:   keepBytes,
		clearIsNoop:       true,
	}
	c := NewClient(WithCommandFactory(pane.factory))
	err := c.sendLine(context.Background(), "boss-test-sess", wide, sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 60 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	})
	if err == nil {
		t.Fatal("sendLine retry: want a loud error when the clear truncates a WRAPPED payload, got nil (a truncated payload was submitted and reported as delivered)")
	}
	if got := OutcomeOf(err); got != OutcomeNotSubmitted {
		t.Fatalf("OutcomeOf(err) = %v, want %v (the payload provably never left the composer)", got, OutcomeNotSubmitted)
	}
	if !errors.Is(err, errSubmissionPending) {
		t.Fatalf("err = %v, want it to carry errSubmissionPending", err)
	}
	// The absent trailing Enter is the assertion that matters: the truncated
	// payload is never submitted.
	want := []string{"literal", "enter", "clear", "clear"}
	if got := pane.submitSteps(); !slices.Equal(got, want) {
		t.Fatalf("submit sequence = %v, want %v (no Enter onto a truncated wrapped payload)", got, want)
	}
	// And the composer itself agrees: nothing was ever handed to the agent after
	// the swallowed first Enter.
	if pane.didSubmit() {
		t.Fatal("the composer submitted a payload: the truncated fragment reached the agent")
	}
}

// TestSendLine_RetrySubmitsAnIntactWrappedPayloadInPlace is the counterweight
// to the truncation guard above, and the test that isolates the FOOTPRINT fix
// from the baseline one.
//
// The two guards can mask each other. Refusing to Enter a wrapped payload is
// trivially achievable by refusing to Enter EVERY wrapped payload — and since
// an over-wide line is the normal case for the payloads this retry serves, that
// would turn the dominant healthy send into a permanent loud failure while the
// truncation test still passed. So this pins the other side: an over-wide
// payload that survives its C-u INTACT must still be recovered in place by the
// bare Enter, with no re-delivery (a second copy would concatenate).
//
// It fails unless composerFootprint reads the wrapped continuation rows: with a
// footprint that stops at the marker row, the baseline never accounts for the
// whole payload, the mutilation guard latches before a single key is pressed,
// and this healthy send is rejected as not-submitted.
func TestSendLine_RetrySubmitsAnIntactWrappedPayloadInPlace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	const wide = "$boss-repair fix the failing check on pull request 1754 and push the result"
	pane := &composerPane{
		swallowFirstEnter: true,
		paneWidth:         24,
		clearIsNoop:       true,
	}
	c := NewClient(WithCommandFactory(pane.factory))
	if err := c.sendLine(context.Background(), "boss-test-sess", wide, sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 60 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	}); err != nil {
		t.Fatalf("sendLine retry: want an INTACT wrapped payload to be recovered by the bare Enter, got %v", err)
	}
	// No second delivery: the box still holds the payload, so it is submitted
	// where it sits.
	want := []string{"literal", "enter", "clear", "enter"}
	if got := pane.submitSteps(); !slices.Equal(got, want) {
		t.Fatalf("submit sequence = %v, want %v (submit the intact wrapped payload in place)", got, want)
	}
	if !pane.didSubmit() {
		t.Fatal("the composer never submitted: the intact wrapped payload was not delivered")
	}
}

// TestSendLine_RetryNeverSubmitsAPayloadThatArrivedTruncated pins the OTHER
// direction the baseline can be wrong, and isolates the baseline fix from the
// footprint one by staying on a single unwrapped row throughout.
//
// Every mutilation decision in the retry is taken by comparing a post-clear
// reading against the baseline, which can only ever establish that nothing
// CHANGED. A composer that was ALREADY holding a truncated payload when the
// retry first looked at it is therefore the one shape those comparisons cannot
// speak to: the reading is perfectly stable across a no-op C-u, no press is at
// fault, and the bare Enter fires on the fragment — reporting delivered=true for
// user text the agent never fully received. The content predicates cannot catch
// it either, being prefix-based for wrapping immunity (BOS-489).
//
// So the baseline must be checked against the payload itself, not merely
// re-read: a reading that does not account for the whole payload is not
// evidence the box holds it.
func TestSendLine_RetryNeverSubmitsAPayloadThatArrivedTruncated(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	const payload = "$boss-repair"
	pane := &composerPane{
		swallowFirstEnter: true,
		deliverKeepsBytes: len("$boss-rep"),
		clearIsNoop:       true,
	}
	c := NewClient(WithCommandFactory(pane.factory))
	err := c.sendLine(context.Background(), "boss-test-sess", payload, sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 60 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	})
	if err == nil {
		t.Fatal("sendLine retry: want a loud error when the composer already held a truncated payload, got nil (the fragment was submitted and reported as delivered)")
	}
	if got := OutcomeOf(err); got != OutcomeNotSubmitted {
		t.Fatalf("OutcomeOf(err) = %v, want %v (the payload provably never left the composer)", got, OutcomeNotSubmitted)
	}
	if !errors.Is(err, errSubmissionPending) {
		t.Fatalf("err = %v, want it to carry errSubmissionPending", err)
	}
	if pane.didSubmit() {
		t.Fatal("the composer submitted a payload: the truncated fragment reached the agent")
	}
}

// TestSendPlan_MultiLineRetryConvergesWhenTheClearIsPartial is the multi-line
// twin of the convergence test. A multi-line payload is exactly where a
// line-scoped kill leaves a fragment, so the paste path must reach the same
// place: clear until empty, then re-PASTE (not re-type) and submit once.
func TestSendPlan_MultiLineRetryConvergesWhenTheClearIsPartial(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	const plan = "line one\nline two"
	pane := &composerPane{pasted: plan, swallowFirstEnter: true, clearKeepsBytes: len("line one")}
	c := NewClient(WithCommandFactory(pane.factory))
	if err := c.sendPlan(context.Background(), "boss-test-sess", plan, sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 60 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	}); err != nil {
		t.Fatalf("sendPlan retry: want the second clear to converge and submit, got %v", err)
	}
	want := []string{"paste", "enter", "clear", "clear", "paste", "enter"}
	if got := pane.submitSteps(); !slices.Equal(got, want) {
		t.Fatalf("submit sequence = %v, want %v (clear until empty, then re-paste)", got, want)
	}
}

// TestSendLine_RetryAgentActivityIsNotEvidenceTheClearWorked pins the boundary
// between the two questions the retry asks. "Was this submitted?" is satisfied
// by agent activity below the input box; "is the composer empty?" is not, because
// C-u cannot make an agent respond. Answering the second with the first meant a
// pane that demonstrably STILL held the payload, but happened to render an
// activity glyph below the marker, classified as a cleared composer — the one
// state that licenses the retry to re-type and press Enter, submitting two
// concatenated copies of the user's message.
//
// The composer here survives C-u intact and shows activity from the clear
// onwards, so the recorded argv must show exactly one delivery: the retry
// presses Enter on the copy already held and never types a second.
func TestSendLine_RetryAgentActivityIsNotEvidenceTheClearWorked(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	pane := &composerPane{swallowFirstEnter: true, clearIsNoop: true, activityAfterClear: true}
	c := NewClient(WithCommandFactory(pane.factory))
	if err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 20 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	}); err != nil {
		t.Fatalf("sendLine retry: unexpected error: %v", err)
	}

	want := []string{"literal", "enter", "clear", "enter"}
	if got := pane.submitSteps(); !slices.Equal(got, want) {
		t.Fatalf("submit sequence = %v, want %v (activity below the marker is not a cleared composer, so nothing is re-typed)", got, want)
	}
}

// TestSendLine_RetryReReadsThePaneBeforeABareEnter pins the loss mode hiding
// inside the unchanged-footprint shortcut. "The two captures agree" is what a
// no-op C-u looks like — and equally what a composer that DID empty looks like
// before it repaints, since both captures are taken inside one clear slice. On
// the second reading a bare Enter lands in an empty box, and waitForSubmission
// then reads that cleared composer as "submitted": delivered=true for a payload
// that was silently discarded.
//
// So an unchanged footprint may not be acted on blind. A second, later reading
// separates the two: if the box is in fact empty the retry must re-deliver
// before its Enter, exactly as it does when the clear is observed to take.
//
// The pane owes exactly ONE repaint (staleCaptures: 1), and the sequence rather
// than the clock is what makes this deterministic: submitVerifyTick is left zero
// so each poll window's budget (submitVerifyWait/3, ~10ms) is far under the
// pollers' 100ms default tick, and every window therefore takes exactly one
// capture. Capture 1 after the clear is the stale one the clear-verify window
// sees; capture 2 is the re-read, and the first sight of the truth.
func TestSendLine_RetryReReadsThePaneBeforeABareEnter(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	pane := &composerPane{swallowFirstEnter: true, staleCaptures: 1}
	c := NewClient(WithCommandFactory(pane.factory))
	if err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 30 * time.Millisecond,
	}); err != nil {
		t.Fatalf("sendLine retry: unexpected error for a slow-repainting composer: %v", err)
	}

	// The re-delivery between the clear and the final Enter is half the
	// assertion: a bare Enter there would submit an empty composer and the
	// verifier would report that as success. The other half is the SECOND capture
	// before it — the re-read this test exists for. A retry that decided on the
	// clear-verify window's reading alone reaches the same keystrokes as the
	// ordinary observed-clear path (TestSendLine_RetryClearsComposerBeforeSecondEnter),
	// so the keystrokes alone cannot tell the two apart; the extra look can.
	want := []string{"clear", "capture", "capture", "literal", "enter", "capture"}
	if got := pane.stepsFromFirstClear(); !slices.Equal(got, want) {
		t.Fatalf("steps from the clear = %v, want %v (look again before acting on an unchanged composer, then re-deliver into it)", got, want)
	}
}

// TestSendLine_RetryLooksAgainBeforeEnteringAMutilatedRemainder pins the loss
// mode the re-read itself can introduce. The re-read exists because an unchanged
// footprint may be a stale screen rather than a no-op C-u — but "the pane
// repainted" and "the payload is intact" are different claims. C-u is
// line-scoped, so the repaint can reveal a TRUNCATED payload, and the content
// predicates are prefix-based (BOS-489) so the fragment still reads as "the
// composer holds the payload". Acting on that reading alone sends a bare Enter
// onto mutilated user text; the fragment then leaves the composer, which
// waitForSubmission reads as submitted, and the RPC reports delivered=true for
// corrupted text.
//
// The second reading must therefore be compared against the baseline exactly as
// the first one is: a footprint that is anything OTHER than what the box held
// before a key was pressed is a cut, so mutilation latches and the loop presses
// again instead of Entering the remainder. Here the composer truncates on press
// 1 and then survives every later press, so the run ends where a stuck partial
// clear must — loudly, with no Enter.
func TestSendLine_RetryLooksAgainBeforeEnteringAMutilatedRemainder(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	pane := &composerPane{
		swallowFirstEnter: true,
		clearKeepsBytes:   len("$boss-rep"),
		clearIsNoop:       true,
		staleCaptures:     1,
	}
	c := NewClient(WithCommandFactory(pane.factory))
	err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 30 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("sendLine retry: want a loud error when the repaint reveals a truncated payload, got nil (mutilated text was submitted and reported as success)")
	}
	if got := OutcomeOf(err); got != OutcomeNotSubmitted {
		t.Fatalf("OutcomeOf(err) = %v, want %v (the payload provably never left the composer)", got, OutcomeNotSubmitted)
	}
	if !errors.Is(err, errSubmissionPending) {
		t.Fatalf("err = %v, want it to carry errSubmissionPending", err)
	}
	// No Enter after any clear: the truncated payload is never submitted. Each
	// press costs its post-clear window plus the re-read; the reading they are
	// judged against is the ONE baseline taken before the first clear, so no press
	// pays for a pre-clear capture of its own.
	want := []string{"clear", "capture", "capture", "clear", "capture", "capture"}
	if got := pane.stepsFromFirstClear(); !slices.Equal(got, want) {
		t.Fatalf("steps from the clear = %v, want %v (no Enter onto a mutilated payload)", got, want)
	}
}

// TestSendLine_RetryComposerVanishingOnTheReReadIsUnconfirmed is the vanishing
// twin of the re-read. The box can disappear on the repaint the re-read is there
// to wait for — the agent accepted the payload after all and went full-screen —
// and the second reading must classify that exactly as the first one does. A
// pane with no input box is unknowable, not cleared: re-delivering into it
// submits the message twice, and Entering into it is the same double-submit.
func TestSendLine_RetryComposerVanishingOnTheReReadIsUnconfirmed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	pane := &composerPane{swallowFirstEnter: true, markerGoneAfterClear: true, staleCaptures: 1}
	c := NewClient(WithCommandFactory(pane.factory))
	err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 30 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("sendLine retry: want an error when the composer vanishes on the re-read, got nil")
	}
	if got := OutcomeOf(err); got != OutcomeUnconfirmed {
		t.Fatalf("OutcomeOf(err) = %v, want %v (the payload may have been accepted)", got, OutcomeUnconfirmed)
	}
	if errors.Is(err, errSubmissionPending) {
		t.Fatalf("err = %v, must NOT claim the payload is confirmed pending", err)
	}
	// The stale capture hides the vanishing from the clear-verify window, so the
	// re-read is the capture that sees it: two looks, then a stop.
	want := []string{"clear", "capture", "capture"}
	if got := pane.stepsFromFirstClear(); !slices.Equal(got, want) {
		t.Fatalf("steps from the clear = %v, want %v (no keystroke into a pane with no input box)", got, want)
	}
}

// TestSendLine_RetryComposerVanishingAfterClearIsUnconfirmed pins the other way
// "the composer is not holding the payload" can arise. classifyComposerAfterClear
// must not read a pane with NO input box as a cleared composer: the box can
// disappear because the agent accepted the payload after all and went
// full-screen, and re-delivering into that submits the message twice — the exact
// double-submit this change exists to prevent. It is unknowable, not cleared.
func TestSendLine_RetryComposerVanishingAfterClearIsUnconfirmed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	pane := &composerPane{swallowFirstEnter: true, markerGoneAfterClear: true}
	c := NewClient(WithCommandFactory(pane.factory))
	err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 20 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	})
	if err == nil {
		t.Fatal("sendLine retry: want an error when the composer vanishes after the clear, got nil")
	}
	if got := OutcomeOf(err); got != OutcomeUnconfirmed {
		t.Fatalf("OutcomeOf(err) = %v, want %v (the payload may have been accepted)", got, OutcomeUnconfirmed)
	}
	if errors.Is(err, errSubmissionPending) {
		t.Fatalf("err = %v, must NOT claim the payload is confirmed pending", err)
	}

	want := []string{"literal", "enter", "clear"}
	if got := pane.submitSteps(); !slices.Equal(got, want) {
		t.Fatalf("submit sequence = %v, want %v (no re-delivery into a pane with no input box)", got, want)
	}
}

// TestSendPlan_MultiLineNoComposerDrawnIsUnconfirmed pins the classification
// asymmetry the clear is gated on. The two submission predicates disagree about
// a pane with NO prompt marker: lineStillAtPrompt reads it as submitted (the
// agent is working full-screen), while multiLineSubmitted fails toward "still
// pending". A multi-line plan pasted into a booting agent or behind a
// full-screen overlay therefore reaches the verify deadline as "pending" without
// a single positive observation — and calling that a confirmed not-submitted
// would license C-u plus a full re-paste into a pane the verifier cannot see.
//
// It must classify as UNCONFIRMED and send no recovery keystrokes at all.
func TestSendPlan_MultiLineNoComposerDrawnIsUnconfirmed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	var mu sync.Mutex
	var calls []sendPlanCall
	factory := func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		mu.Lock()
		defer mu.Unlock()
		subcommand := ""
		if len(args) > 0 {
			subcommand = args[0]
		}
		calls = append(calls, sendPlanCall{subcommand: subcommand, args: append([]string(nil), args[1:]...)})
		if subcommand == "capture-pane" {
			// Satisfies the ready-marker wait, but never draws an input box:
			// no prompt-marker row appears at any point.
			return exec.CommandContext(ctx, "printf", "%s", "ready\nbooting the agent\n")
		}
		return exec.CommandContext(ctx, "true")
	}

	c := NewClient(WithCommandFactory(factory))
	err := c.sendPlan(context.Background(), "boss-test-sess", "line one\nline two", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 20 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected an error when no composer is ever drawn, got nil")
	}
	if got := OutcomeOf(err); got != OutcomeUnconfirmed {
		t.Fatalf("OutcomeOf(err) = %v, want %v", got, OutcomeUnconfirmed)
	}
	if errors.Is(err, errSubmissionPending) {
		t.Fatal("an unconfirmed result must not carry errSubmissionPending: that sentinel is what gates the composer clear")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, call := range calls {
		if call.subcommand == "send-keys" && sendKeysKind(call.args) == "clear" {
			t.Fatalf("sent C-u into a pane with no composer drawn, calls = %+v", calls)
		}
	}
}

// TestUnconfirmedErrorsStateTheObservationNotTheVerdict pins what an UNCONFIRMED
// error is for. The verdict already travels beside it — OutcomeUnconfirmed here,
// delivery_state on the wire — and every surface that renders one labels the
// state itself before printing this text ("delivery unconfirmed: …" in the CLI).
// An error that also opens with its own verdict therefore makes the operator
// read the same fact twice in one line, pushing the part only this text knows
// (which pane, and what was actually seen) past where it is skimmed.
//
// So each of these must name its pane and describe what was observed, and must
// not re-state that submission could not be verified.
func TestUnconfirmedErrorsStateTheObservationNotTheVerdict(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	tests := []struct {
		name string
		// send provokes one of the UNCONFIRMED exits against session "boss-test-sess".
		send func() error
		// observed is the thing only this error knows.
		observed string
	}{
		{
			name: "the composer vanished after the clear",
			send: func() error {
				pane := &composerPane{swallowFirstEnter: true, markerGoneAfterClear: true}
				return NewClient(WithCommandFactory(pane.factory)).sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
					readyMarker:      "ready",
					submitVerifyWait: 20 * time.Millisecond,
					submitVerifyTick: time.Millisecond,
				})
			},
			observed: "disappeared after the composer clear",
		},
		{
			name: "no composer was ever drawn",
			send: func() error {
				factory := func(ctx context.Context, _ string, args ...string) *exec.Cmd {
					if len(args) > 0 && args[0] == "capture-pane" {
						// Satisfies the ready-marker wait, never draws an input box.
						return exec.CommandContext(ctx, "printf", "%s", "ready\nbooting the agent\n")
					}
					return exec.CommandContext(ctx, "true")
				}
				return NewClient(WithCommandFactory(factory)).sendPlan(context.Background(), "boss-test-sess", "line one\nline two", sendPlanOpts{
					readyMarker:      "ready",
					submitVerifyWait: 20 * time.Millisecond,
					submitVerifyTick: time.Millisecond,
				})
			},
			observed: "no live composer was drawn",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.send()
			if err == nil {
				t.Fatal("want an unconfirmed error, got nil")
			}
			if got := OutcomeOf(err); got != OutcomeUnconfirmed {
				t.Fatalf("OutcomeOf(err) = %v, want %v", got, OutcomeUnconfirmed)
			}
			text := err.Error()
			if !strings.Contains(text, "boss-test-sess") {
				t.Fatalf("err = %q, want it to name the pane the operator has to go and look at", text)
			}
			if !strings.Contains(text, tc.observed) {
				t.Fatalf("err = %q, want it to carry the observation %q", text, tc.observed)
			}
			if !strings.Contains(text, "the payload's state is unknown") {
				t.Fatalf("err = %q, want it to say what the observation means for the payload", text)
			}
			for _, verdict := range []string{"could not be verified", "unconfirmed"} {
				if strings.Contains(text, verdict) {
					t.Fatalf("err = %q re-states the verdict %q that every surface prefixes for itself", text, verdict)
				}
			}
		})
	}
}

// TestSendLine_UnconfirmedCaptureFailureSkipsRetry proves the composer clear is
// gated on a CONFIRMED-pending verdict. A capture failure leaves the pane state
// unknown, so firing C-u (and an Enter) into it could clobber whatever the user
// is actually looking at. The verifier must classify that as unconfirmed and
// send no recovery keystrokes at all.
func TestSendLine_UnconfirmedCaptureFailureSkipsRetry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Parallel()

	var mu sync.Mutex
	captureCount := 0
	var calls []sendPlanCall
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		mu.Lock()
		defer mu.Unlock()
		subcommand := ""
		if len(args) > 0 {
			subcommand = args[0]
		}
		calls = append(calls, sendPlanCall{subcommand: subcommand, args: append([]string(nil), args[1:]...)})
		if subcommand == "capture-pane" {
			captureCount++
			// The first capture satisfies the ready-marker wait; every capture
			// after it (i.e. the verification polls) fails.
			if captureCount == 1 {
				return exec.CommandContext(ctx, "printf", "%s", "ready\n› \n")
			}
			return exec.CommandContext(ctx, "false")
		}
		return exec.CommandContext(ctx, "true")
	}

	c := NewClient(WithCommandFactory(factory))
	err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
		readyMarker:      "ready",
		submitVerifyWait: 20 * time.Millisecond,
		submitVerifyTick: time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected an error when verification cannot resolve, got nil")
	}
	if got := OutcomeOf(err); got != OutcomeUnconfirmed {
		t.Fatalf("OutcomeOf(err) = %v, want %v", got, OutcomeUnconfirmed)
	}

	mu.Lock()
	defer mu.Unlock()
	var kinds []string
	for _, call := range calls {
		if call.subcommand == "send-keys" {
			kinds = append(kinds, sendKeysKind(call.args))
		}
	}
	want := []string{"literal", "enter"}
	if len(kinds) != len(want) || kinds[0] != want[0] || kinds[1] != want[1] {
		t.Fatalf("send-keys sequence = %v, want %v (no clear, no retry Enter)", kinds, want)
	}
}
