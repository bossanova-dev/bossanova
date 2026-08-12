package tmux

import (
	"context"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"
)

// scriptedTmux is a CommandFactory that replays canned stdout/stderr and a
// canned exit code, recording every invocation. It exists because
// mockCommandFactory always runs `true`, which cannot express the stdout a
// parsing primitive needs, nor the stderr the "no server running" branch
// keys on.
//
// The script is a compile-time constant and every dynamic value travels as a
// positional argument, so nothing the test supplies is ever interpreted by
// the shell.
type scriptedTmux struct {
	mu     sync.Mutex
	calls  [][]string
	stdout string
	stderr string
	exit   int
}

const scriptedTmuxShell = `printf '%s' "$1"; printf '%s' "$2" >&2; exit "$3"`

func (s *scriptedTmux) factory(ctx context.Context, name string, args ...string) *exec.Cmd {
	s.mu.Lock()
	s.calls = append(s.calls, append([]string{name}, args...))
	stdout, stderr, exit := s.stdout, s.stderr, s.exit
	s.mu.Unlock()
	// #nosec G204 -- test-only tmux double; constant script, payloads passed as
	// positional parameters so they are never interpreted by the shell.
	return exec.CommandContext(ctx, "sh", "-c", scriptedTmuxShell, "sh",
		stdout, stderr, strconv.Itoa(exit))
}

func (s *scriptedTmux) lastCall() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		return nil
	}
	return s.calls[len(s.calls)-1]
}

func TestListSessions_ParsesNameAndCreationTime(t *testing.T) {
	fake := &scriptedTmux{
		stdout: "boss-abcdef12-34567890\t1754870000\n" +
			"work\t1754860000\n",
	}
	c := NewClient(WithCommandFactory(fake.factory))

	got, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions() error = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListSessions() returned %d sessions, want 2: %+v", len(got), got)
	}
	if got[0].Name != "boss-abcdef12-34567890" {
		t.Errorf("got[0].Name = %q, want %q", got[0].Name, "boss-abcdef12-34567890")
	}
	if !got[0].Created.Equal(time.Unix(1754870000, 0)) {
		t.Errorf("got[0].Created = %v, want %v", got[0].Created, time.Unix(1754870000, 0))
	}
	if got[1].Name != "work" {
		t.Errorf("got[1].Name = %q, want %q", got[1].Name, "work")
	}
	if !got[1].Created.Equal(time.Unix(1754860000, 0)) {
		t.Errorf("got[1].Created = %v, want %v", got[1].Created, time.Unix(1754860000, 0))
	}
}

// TestListSessions_Argv pins the exact command line. The format string is the
// contract the test fakes in internal/status and internal/testharness emit
// against, so a silent change here would desynchronise them from production.
func TestListSessions_Argv(t *testing.T) {
	fake := &scriptedTmux{}
	c := NewClient(WithCommandFactory(fake.factory))

	if _, err := c.ListSessions(context.Background()); err != nil {
		t.Fatalf("ListSessions() error = %v, want nil", err)
	}

	want := []string{"tmux", "list-sessions", "-F", "#{session_name}\t#{session_created}"}
	got := fake.lastCall()
	if len(got) != len(want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv = %q, want %q", got, want)
		}
	}
}

// TestListSessions_NoServerRunning is the load-bearing fail-open case: an idle
// host with no tmux server has genuinely zero live sessions, which is an
// affirmative absent signal, not a read failure.
func TestListSessions_NoServerRunning(t *testing.T) {
	fake := &scriptedTmux{stderr: "no server running on /tmp/tmux-501/default\n", exit: 1}
	c := NewClient(WithCommandFactory(fake.factory))

	got, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions() error = %v, want nil for 'no server running'", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListSessions() = %+v, want empty", got)
	}
}

// TestListSessions_OtherFailureIsAnError is the fail-closed half of D6: any
// failure that is not the affirmative "no server" signal must surface, so a
// caller never mistakes an unreadable tmux for an empty one.
func TestListSessions_OtherFailureIsAnError(t *testing.T) {
	for _, tt := range []struct {
		name   string
		stderr string
	}{
		{name: "permission denied", stderr: "error connecting to /tmp/tmux-501/default (Permission denied)\n"},
		{name: "silent failure", stderr: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := &scriptedTmux{stderr: tt.stderr, exit: 1}
			c := NewClient(WithCommandFactory(fake.factory))

			got, err := c.ListSessions(context.Background())
			if err == nil {
				t.Fatalf("ListSessions() error = nil, want an error")
			}
			if len(got) != 0 {
				t.Fatalf("ListSessions() = %+v, want empty on error", got)
			}
		})
	}
}

func TestListSessions_SkipsMalformedLines(t *testing.T) {
	fake := &scriptedTmux{
		stdout: "no-tab-here\n" +
			"bad-time\tnot-a-number\n" +
			"\tleading-empty-name\n" +
			"\n" +
			"boss-abcdef12-34567890\t1754870000\n",
	}
	c := NewClient(WithCommandFactory(fake.factory))

	got, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions() error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListSessions() returned %d sessions, want 1: %+v", len(got), got)
	}
	if got[0].Name != "boss-abcdef12-34567890" {
		t.Errorf("got[0].Name = %q, want the one well-formed line", got[0].Name)
	}
}

func TestListSessions_EmptyStdout(t *testing.T) {
	fake := &scriptedTmux{}
	c := NewClient(WithCommandFactory(fake.factory))

	got, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions() error = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListSessions() = %+v, want empty", got)
	}
}
