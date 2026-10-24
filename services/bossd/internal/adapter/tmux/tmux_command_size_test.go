package tmux

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/bossalib/contract/plugin/loginshell"
)

// A launch whose full tmux command (argv plus -e env entries) would exceed
// tmux's message limit must fail with a named size reason before tmux runs:
// tmux itself only says "command too long" after the fact.
func TestNewSession_RefusesOversizeCommandWithoutCallingTmux(t *testing.T) {
	cases := map[string]NewSessionOpts{
		"inline first input": {
			Name:    "big-argv",
			WorkDir: "/tmp/wt",
			Command: []string{"claude", "--", strings.Repeat("p", 40*1024)},
		},
		"env entries count too": {
			Name:    "big-env",
			WorkDir: "/tmp/wt",
			Command: []string{"claude", "--", strings.Repeat("p", 8*1024)},
			Env:     map[string]string{"BOSS_CONTEXT": strings.Repeat("e", 8*1024)},
		},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			mock := &mockCommandFactory{}
			c := NewClient(WithCommandFactory(mock.factory))
			err := c.NewSession(context.Background(), opts)
			if !errors.Is(err, ErrCommandTooLong) {
				t.Fatalf("NewSession err = %v, want ErrCommandTooLong", err)
			}
			if !strings.Contains(err.Error(), "bytes") {
				t.Fatalf("size reason must name the measured size: %v", err)
			}
			if len(mock.calls) != 0 {
				t.Fatalf("tmux must not be invoked for an oversize command; calls=%v", mock.calls)
			}
		})
	}
}

// A 40 KiB first input launches when it travels through the prompt file: the
// argv tmux receives carries only the path.
func TestNewSession_LargeFirstInputLaunchesThroughFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.initial-input")
	argv, err := loginshell.WrapInitialInput("/bin/zsh", loginshell.Flags("/bin/zsh"),
		[]string{"claude", "--session-id", "abc", "--"}, strings.Repeat("p", 40*1024), path)
	if err != nil {
		t.Fatal(err)
	}
	mock := &mockCommandFactory{}
	c := NewClient(WithCommandFactory(mock.factory))
	if err := c.NewSession(context.Background(), NewSessionOpts{
		Name:    "file-input",
		WorkDir: "/tmp/wt",
		Command: argv,
		Env:     map[string]string{"BOSS_CRON": "true"},
	}); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if len(mock.calls) == 0 || mock.calls[0][1] != "new-session" {
		t.Fatalf("expected tmux new-session to run; calls=%v", mock.calls)
	}
}

// The guard sits below the measured limit, with headroom, but well above any
// ordinary launch.
func TestMaxNewSessionCommandBytes_BelowMeasuredLimit(t *testing.T) {
	const measuredLimit = 16 * 1024 // tmux 3.7c/3.8: 15,000 launches, ~16,300 fails
	if maxNewSessionCommandBytes >= measuredLimit {
		t.Fatalf("guard %d must sit below the measured limit %d", maxNewSessionCommandBytes, measuredLimit)
	}
	if maxNewSessionCommandBytes < 12*1024 {
		t.Fatalf("guard %d is needlessly tight", maxNewSessionCommandBytes)
	}
}
