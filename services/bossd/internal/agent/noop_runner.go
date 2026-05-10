package agent

import (
	"context"
	"errors"
)

var (
	_ AgentRunner     = NoopRunner{}
	_ AgentDispatcher = NoopRunner{}
)

// errNoAgentPlugin is returned when bossd was started without an
// AgentRunner plugin loaded. Sessions cannot be started until one is
// installed and the daemon restarts.
var errNoAgentPlugin = errors.New("no AgentRunner plugin loaded; install bossd-plugin-claude or bossd-plugin-codex (or another agent runner) and restart")

// NoopRunner is the AgentRunner used when bossd starts with no
// AgentRunner plugin loaded. The daemon stays healthy so existing
// sessions can be inspected, but new session creation fails fast.
type NoopRunner struct{}

func (NoopRunner) Start(_ context.Context, _, _ string, _ *string, _ string) (string, error) {
	return "", errNoAgentPlugin
}

func (NoopRunner) Stop(_ string) error           { return errNoAgentPlugin }
func (NoopRunner) IsRunning(_ string) bool       { return false }
func (NoopRunner) ExitError(_ string) error      { return nil }
func (NoopRunner) History(_ string) []OutputLine { return nil }

func (NoopRunner) Subscribe(_ context.Context, _ string) (<-chan OutputLine, error) {
	return nil, errNoAgentPlugin
}

func (NoopRunner) StartByAgent(_ context.Context, _, _, _ string, _ *string, _ string) (string, error) {
	return "", errNoAgentPlugin
}

func (NoopRunner) StopByAgent(_, _ string) error { return errNoAgentPlugin }

func (NoopRunner) IsRunningByAgent(_, _ string) bool { return false }
